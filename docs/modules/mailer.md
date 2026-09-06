# mailer 模块

替 Phorge 发出站邮件：收一封信，交给配置好的后端中第一个接受它的那个。独占 `gorge-mailer` 这个二进制与 `:8110` 这个端口。

| | |
|---|---|
| 二进制 | `gorge-mailer` |
| 端口 | `:8110` |
| 包 | `go/internal/mailer/` |
| 契约 | [`api/openapi/mailer.yaml`](../../api/openapi/mailer.yaml) |
| 固件 | `tests/contract/mailer/`（10 份） |
| 兼容约束 | [`compat/phorge/README.md`](../../compat/phorge/README.md) 第六节 ← **改动前必读** |

## 1. 职责边界

**负责**：把一封信交出去，并如实报告「交出去了没有」以及「这次失败还值不值得再试」。七个后端（SMTP / sendmail / SES / SendGrid / Mailgun / Postmark / test）按优先级串成 failover 链。

**不负责**：队列、去重、退避重投、投递回执。**Phorge 的 worker 队列是重试的唯一权威**，本服务同步应答、不持久化任何东西——进程重启不会丢下待发邮件，因为它从来就没有持有过。第 4 节的重试是这条边界内的例外，且刻意被压到秒级。

**有外部依赖**，这是它与 render / diff 的结构性差异，也是它单独占一个进程的原因：它持有适配器状态、要连出去打 SMTP 与各家 provider、并且有一个真实的就绪条件可报。按 [`../architecture.md`](../architecture.md) 的分界，它归到 notification 那一侧。

## 2. 路由与依赖

```go
func RegisterRoutes(e *echo.Echo, deps *Deps) {
	g := e.Group("/api/mailer")
	g.Use(auth.Token(deps.Token))

	g.POST("/send", sendMail(deps))
	g.GET("/mailers", listMailers(deps))
}
```

| 方法 | 路径 | 鉴权 |
|---|---|---|
| POST | `/api/mailer/send` | 需要 |
| GET | `/api/mailer/mailers` | 需要 |
| GET | `/`、`/healthz`、`/readyz` | 不需要（平台层注册） |

`Deps` 三个字段：`Dispatcher` / `Token` / `BodyLimit`。`TestRoutePathsAreStable` 断言这两条路径仍注册着——Phorge 侧 `PhabricatorGorgeMailerClient` 已经在调它们。

`main.go` 与另外两个二进制的唯一差别是那两行 `httpx.Config`：

```go
srv := httpx.New(httpx.Config{
	ListenAddr: cfg.ListenAddr,
	BodyLimit:  mailer.TransportBodyLimit, // "10M"，平台默认 2M 装不下带附件的信
	Ready:      dispatcher.Ready,
})
```

**`/readyz` 是本域唯一一处比 `/healthz` 多说了点什么的地方。** 就绪判据只有一条：**至少有一个适配器配置成功**。这精确对应「服务活着、每一封信都失败」这个状态——迁入前 `/healthz` 在零后端时照样 200，compose 报 healthy，而没有任何一封信发得出去。

刻意**不**在 `Ready` 里拨测 SMTP 或 provider：那会让就绪状态随第三方抖动而翻转，而多后端 failover 本来就是为此存在的。所以 200 的含义是「本服务能发起一次投递」，不是「下一封信会到」。

## 3. 核心实现

### 3.1 handler 的四道处理

`c.Bind` 的错误分支与 render 域同形：非 400 的 `*echo.HTTPError`（流式上传时冒出来的 413）原样交回平台错误处理器，只有真正的 JSON 语法错误留在本地当 400。理由见 [`render.md`](render.md) 第 3.1 节。

剩下三道：**必填校验**（from / to / subject 缺一即 400）、**正文截断**（`textBody` / `htmlBody` 超 `BodyLimit` 时按 UTF-8 边界切断，静默截断而非拒绝）、**交给 Dispatcher**。

必填校验返回 400 而不是 422，这个区分是给 Phorge 看的：422 的含义是「某个后端判定这封信投不出去」，而这三种情况根本没走到后端。答 422 会让 Phorge 为它自己的序列化 bug 记一笔永久投递失败。

### 3.2 Dispatcher：优先级 failover + 单适配器重试

```
按 priority 降序遍历适配器
  └─ 单个适配器内重试 MaxRetries 次，间隔 RetryWait
       ├─ 成功        → 返回 SendResult{mailerKey, messageId}
       ├─ PermanentError → 立即返回，不重试、也不换后端
       └─ 临时失败     → 重试；重试用尽后换下一个适配器
```

`mailerKeys` 只**收窄**候选集，不改变顺序——请求里写 `["b","a"]` 仍按服务端的优先级试。

**遇 `PermanentError` 不换后端**，因为永久失败描述的是这封信而不是这个后端，换一个只会更慢地拿到同样的拒绝。

### 3.3 永久失败分类

这是迁入时补上的真实缺口：老代码定义了 `PermanentError` 但七个适配器**从不返回它**，后果是收件人地址写错会被 Phorge 的 worker 无限重投。

| 后端 | 永久 | 临时 |
|---|---|---|
| SMTP | 5xx 应答码 | 4xx 应答码；无应答码的连接/TLS 失败 |
| SES / SendGrid / Mailgun / Postmark | HTTP 4xx | **429**、5xx、网络错误 |
| Postmark | 额外认 `ErrorCode` 300/406/409/422 | 其余 `ErrorCode` |
| Mailgun | 附件不是合法 base64 | — |
| sendmail | 退出码 64/65/66/67/68/77/78 | 75（`EX_TEMPFAIL`）、71、74、**以及任何未登记的码** |

两条贯穿全表的规则：

- **429 是 4xx 里唯一的例外**。限流说的是「现在不行」而不是「永远不行」，跟着其余 4xx 判永久会让每一次 provider 限流都丢信。
- **不认识的信号一律判临时**。两个方向的代价不对称：误判临时只是白费几次 worker 循环，误判永久是**静默丢信**，而且落在 Phorge 里的状态看起来就像地址写错了。

handler 把 `*PermanentError` 映射到 422 + `ERR_PERMANENT_FAILURE`，其余失败到 502 + `ERR_SEND_FAILED`。

### 3.4 MIME 构建

`mime.go`，被 SMTP / sendmail / SES 三个后端共用——它们要一封完整的 RFC 5322 报文（SES 走 `SendRawEmail`，正是为了保住附件与自定义头）。四家 provider API 不用，它们收字段、自己组装。

三处值得知道的细节：附件 `data` **原样透传不重新编码**（进来就是 base64，出去也是）；正文一律 base64 编码而非 8bit（Phorge 的信带 UTF-8 主题与正文，base64 是路径上没有中继能弄坏的那一种）；自定义头的 CR/LF 会被剥掉，`TestBuildMIMEStripsHeaderInjection` 守着这一条。

## 4. 配置

| 变量 | 兜底旧名 | 默认值 | 说明 |
|---|---|---|---|
| `GORGE_LISTEN_ADDR` | `LISTEN_ADDR` | `:8110` | 监听地址 |
| `GORGE_SERVICE_TOKEN` | `SERVICE_TOKEN` | 空 | 服务间认证 token，为空则不鉴权 |
| `GORGE_MAILER_CONFIG` | `MAILER_CONFIG` | 无 | 后端列表，JSON 数组 |
| `GORGE_MAILER_CONFIG_FILE` | `MAILER_CONFIG_FILE` | 无 | JSON 配置文件路径 |
| `GORGE_MAILER_MAX_RETRIES` | `MAX_RETRIES` | `2` | 单个适配器内的重试次数 |
| `GORGE_MAILER_RETRY_WAIT` | `RETRY_WAIT` | `2` | 重试间隔（秒） |
| `GORGE_MAILER_BODY_LIMIT` | `BODY_LIMIT` | `524288` | 单封信正文截断上限（字节） |
| `GORGE_MAILER_TYPE` | `MAILER_TYPE` | 无 | 单后端速配：设了它才会从下面那批扁平变量拼出一个后端 |
| `GORGE_MAILER_KEY` | `MAILER_KEY` | `default` | 同上，该后端的 key |

命名规则与「新名优先、旧名兜底」的查找机制见 [`../platform.md`](../platform.md) 第 4 节。

各后端选项另有一批扁平变量（`SMTP_HOST` / `SMTP_PORT` / `SMTP_USER` / `SMTP_PASSWORD` / `SMTP_PROTOCOL`、`MAILER_ACCESS_KEY` / `MAILER_SECRET_KEY` / `MAILER_REGION` / `MAILER_ENDPOINT`、`MAILER_API_KEY` / `MAILER_DOMAIN` / `MAILER_API_HOSTNAME`、`MAILER_ACCESS_TOKEN`），原样保留。**迁入时删掉了 `MAILER_FROM_NUMBER` / `MAILER_ACCOUNT_SID` / `MAILER_AUTH_TOKEN` 三条**：它们没有任何对应适配器，是某个 Twilio 实现的残留，SMS 在 Phorge 侧由原生 `PhabricatorMailTwilioAdapter` 负责。`TestMailerConfigDropsTwilioOptions` 钉住这次删除。

`Load()` 的取值顺序与 render 域一致：指了配置文件就读文件，否则读环境变量；走文件时**仍然从环境变量取 `SERVICE_TOKEN`**。这一条在本域比在别处更要紧——mailer 的配置文件里装着这个部署的全部后端凭据。

### 重试默认值是一次刻意的变更

老默认值是 `MaxRetries=250` / `RetryWait=15`，但老代码**从不读它们**。迁入时把它们真正接进发送循环，同一组值会让一次 HTTP 请求最坏阻塞一小时以上，而 PHP 客户端只等 30 秒。所以默认值改为 **2 次 / 2 秒**，并让整个重试循环受 `c.Request().Context()` 约束——客户端断开即止。

这不是把重试变弱了：**Phorge 的 worker 队列仍然是外层重试的唯一权威**，Go 侧只吸收秒级抖动。记在 [`../findings.md`](../findings.md)。

## 5. 兼容契约

权威描述在 [`compat/phorge/README.md`](../../compat/phorge/README.md) 第六节，这里是概述。四条约定：`/api/mailer/*` 两条路径、wire 字段名一律 camelCase、`ERR_PERMANENT_FAILURE` 的语义、附件的 base64 编码位置。

**最要紧的是 `ERR_PERMANENT_FAILURE`**，因为它是本域唯一一个改变 Phorge **行为**而不只是改变它**报告内容**的码：PHP 客户端把它转成 `PhabricatorMetaMTAPermanentFailureException`，worker 队列见到这个异常才停止重投，其余任何失败都会被重新入队。

把它收敛进平台码，或者把某个临时失败误报成它，都不会有任何一处报错：前者让写错的收件人地址被永久重投，后者让本可以发出去的信被记成 `FAIL`。两个方向都只在几天后的邮件统计里看得出来。

配置形态上还有一条约定不在 Go 侧但影响这里：端点与 token 全部由 Phorge 的 `cluster.mailers` 条目的 `options` 承载，**不新增任何全局 config key**。这消掉了老实现里「设了 URL 却依然不发信」的两层配置坑。

## 6. 域级错误码

两个，都是迁入前就有、Phorge 侧已经在用的码，定义在 `internal/mailer/http.go`：

| 码 | 状态 | 含义 |
|---|---|---|
| `ERR_PERMANENT_FAILURE` | 422 | 某个后端判定这封信投不出去，重试无用 |
| `ERR_SEND_FAILED` | 502 | 所有候选后端都临时失败；`mailerKeys` 一个都没匹配上也是它 |

它们不会被全局错误处理器改写成 `ERR_INTERNAL`——`httpx.Fail` 一写响应就 committed（见 [`../platform.md`](../platform.md) 第 1.2 节）。

**后端失败不是 500。** 500 在本域只意味着「服务自己出了问题」，投递失败一律落在 422 或 502。

**新增域级错误码时加在自己的域包里，不要塞进 `platform/httpx`。**
