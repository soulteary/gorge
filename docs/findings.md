# 偏差与改进建议

代码与文档比对时发现的实际问题。按模块分节，新模块迁入后在下面新开一节，不要混进别的模块。基线 `da522f9`。

修掉一条就把它从这里删掉，别标记成「已完成」留着——这个文件的价值在于短。

---

## 平台层

### 1. `GORGE_RENDER_TIMEOUT_SEC` 并非请求超时（文档不符）

**影响**：高。服务当前实际上没有请求级超时。

`deploy/compose/.env.example` 写的是 "Per-request timeout in seconds"，根 `README.md` 写的是「请求超时（秒）」。但 `cfg.TimeoutSec` 在代码里的唯一用途是：

```go
srv := httpx.New(httpx.Config{
	ListenAddr:      cfg.ListenAddr,
	ShutdownTimeout: time.Duration(cfg.TimeoutSec) * time.Second,
```

它控制的是**优雅关闭的排空时长**。全仓库没有 `middleware.Timeout`，也没有给 `http.Server` 设 `ReadTimeout` / `WriteTimeout`，即单个请求没有超时上限，一个慢客户端可以长期占住连接。

**建议二选一**：把变量按实际语义改名为 `GORGE_SHUTDOWN_TIMEOUT_SEC` 并修正两处文档（改动小）；或者补上请求超时中间件，让文档描述成真（才真正补上防护）。

这条对将来的模块影响更大：render 是纯计算且有 1MiB 输入上限，最坏情况可控；conduit、search 这类要打下游的模块没有超时会直接堆积连接。

### 2. 传输层 BodyLimit 无法通过环境变量配置

**影响**：中。是个运维陷阱。

`httpx.Config.BodyLimit` 在代码里可设，但 `main.go` 没传，于是恒为硬编码的 `2M`。把 `GORGE_RENDER_MAX_BYTES` 调到 2M 以上时，实际生效的上限仍是 2M——**配置看起来生效了但没有**。

根 README 已经描述了这个行为，属于「已知且已记录」，但记录不等于不会踩。建议给平台层加一个 `GORGE_BODY_LIMIT`，默认仍是 `2M`。

---

## render 模块

### 3. `Languages()` 每次请求重建列表

**影响**：低。该端点调用频率低。

```go
func (h *Highlighter) Languages() []string {
	names := lexers.Names(false)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, strings.ToLower(name))
	}
	return result
}
```

Chroma 的 lexer 注册表在进程生命周期内不变，这个列表（数百项）却在每次 `GET /api/highlight/languages` 时重新分配与转小写。用 `sync.Once` 缓存是一行改动。

---

## diff 模块

### 4. `config.Base` 由 render 域持有，diff 域读不到进程级配置

**影响**：中。是个结构问题，不是 bug。

`config.Base`（`GORGE_LISTEN_ADDR`、`GORGE_SERVICE_TOKEN`、`GORGE_CONFIG_FILE`）目前嵌在 `render.Config` 里。diff 域是同一个进程里的第二个域，但它的 `diff.Config` 只有一个域级字段，进程级配置得由 `main.go` 从 `render.Load()` 的结果里取出来再传给它：

```go
cfg, err := render.Load()          // 这里面有 config.Base
diffCfg := diff.LoadFromEnv()      // 这里只有 MaxBytes

diff.RegisterRoutes(srv.Echo(), &diff.Deps{
	Token:    cfg.ServiceToken,    // 从 render 的配置里借
	MaxBytes: diffCfg.MaxBytes,
})
```

这样接是对的——两个域各自声称拥有 `GORGE_LISTEN_ADDR` 会让「谁说了算」变得含混——但「进程级配置住在某个域的包里」这件事本身摆错了位置。**第三个域进来时这条会开始咬人**：它同样得从 `render.Load()` 借 token，而它和 render 之间并没有任何关系。

**建议**：把 `config.Base` 的加载上提到一个进程级的 `platform/config.LoadProcess()` 或 `cmd/gorge-render` 自己的私有类型里，各域的 `Config` 只留域级字段（diff 已经是这个形状）。这次刻意没做，因为它会动到 render 的配置加载路径与那套「新名优先、旧名兜底」的查找逻辑，不该和迁入混在一个改动里。

### 5. LCS 不是 Myers：一处技术债的两个症状

**影响**：中。两个症状都不是 bug，但都有代价。

`unified.lcs()` 是经典 LCS 全表动态规划，而 GNU diff 跑的是 Myers 的 `O(ND)` 算法。由此来的两件事：

**症状一：靠常量护栏兜内存。** 全表要 `(n+1)×(m+1)`，所以 `maxCells = 4_000_000` 这道护栏必须存在（见 [`modules/diff.md`](modules/diff.md) 第 5 节）。代价是 2001 行对 2001 行这种完全正常的文件比较会被拒成 413，而 GNU 毫无压力——Myers 的内存与**差异量**成正比，不与两侧行数之积成正比。

**症状二：歧义对齐的选择与 GNU 不同。** 一行重复出现时可能有多个同样最小的对齐，GNU 的选择来自 Myers 加它的边界平移启发式。实测约 2900 组生成输入：91.4% 逐字节一致，8.6% 分歧且全部含重复行，但 **hunk 头 0 次不同、编辑数 0 次不同**——即行号从不错位、diff 从不更差。边界写在 `compat/phorge/README.md` 第 4.6 节，由 `unified/systemdiff_test.go` 守着。

**建议**：换成 Myers（或 `diff-match-patch` 那类实现）一并解决两个症状。护栏是权宜，不是可调参数——**不要简单地把 `maxCells` 往上调**，那只是把 OOM 的门槛挪高。

换的时候注意：`systemdiff_test.go` 里 `TestAlignmentChoiceMayDifferFromSystemDiff` 目前只断言 hunk 头与编辑数。真换到 Myers 之后应当先看这组歧义输入能不能全等，能的话就把它收紧成整值比对，这个测试就从「记录偏差」变成「锁住全等」。

---

## 文档

### 6. 存在两份过期的技术报告

**影响**：中。会把下一个人引到不存在的路径上。

`go/internal/render/highlight/TECHNICAL_REPORT.md` 与仓库外的 `gorge-highlight/TECHNICAL_REPORT.md` 描述的是**单仓库改造之前**的布局（`internal/config/`、`internal/httpapi/`、`cmd/server/main.go`），这些路径现在都不存在；文中的依赖版本（Chroma v2.14.0、Echo v4.12.0）与运行时基础镜像（alpine 3.20）也已过期。

更要紧的是 `highlight.go` 的包注释仍指向它：

```go
// Package highlight renders source code to Pygments-compatible HTML using
// Chroma. See TECHNICAL_REPORT.md in this directory for the design rationale
```

**建议**：删除 `go/internal/render/highlight/TECHNICAL_REPORT.md`，把包注释改指 `docs/modules/render.md` 与 `compat/phorge/README.md`。

### 7. 兼容测试没有反向指回 `compat/`

**影响**：低，但错过成本高。

`compat/phorge/README.md` 是三条兼容约束的权威描述，守住它们的测试却分散在 `highlight/compat_test.go`、`highlight/highlight_test.go`、`render/http_test.go` 与 12 份固件里。目前只有单向引用（文档提到测试）。

在这些测试文件顶部加一行指回 `compat/phorge/README.md` 的注释，能让「为什么这个断言存在」在改动现场就可见。这正是这批测试最容易被当成冗余删掉的地方——它们断言的都是些看起来无关紧要的 CSS 类名。

### 8. `layering_test.go` 的禁止列表需要人工维护

**影响**：中。diff 迁入时已经踩到一次——这一行是手工补的。

```go
var forbiddenPrefixes = []string{
	"github.com/soulteary/gorge/go/internal/render",
	"github.com/soulteary/gorge/go/internal/diff",
	"github.com/soulteary/gorge/go/internal/contracts",
}
```

新增域包时忘了加一行，这个测试对新域就是静默失效的：它照样通过，只是不再检查任何新东西。可以改成扫描 `internal/` 下除 `platform`、`contracts`、`contracttest` 外的所有目录自动生成列表，这样新域自动被纳入。第二个域进来后这已经不是假想问题了。

（mailer 迁入时同样是手工补的这一行，这是它第三次被手工维护。）

### 16. 根 `README.md` 与 `delivery.md` 停在「只有一个二进制」

**影响**：中。是新人接触这个仓库时读到的第一段话。

两处都还在断言只有 `gorge-render` 一个二进制：

- 根 [`README.md`](../README.md)：「当前只有一个二进制 `gorge-render`，承载 render 域」，目录结构里也只列了 `cmd/gorge-render/`、`internal/render/`、`api/openapi/render.yaml`、`tests/contract/render/`、`tests/e2e/render.sh`。
- [`delivery.md`](delivery.md)：「当前仓库只产出 `gorge-render` 一个二进制」，以及「覆盖 `SERVICE` 要等到真有第二个 `cmd/` 才有意义」。

实际是**三个二进制、四个域**。这两处在 diff、notification、mailer 三次迁入里都没有被更新，说明「模块文档只新增不改动既有文档」这条规则被套用到了不该套用的地方——[`docs/README.md`](README.md) 的「新增一个模块时」清单里确实没有它们。

**建议**：把这两处改成不点名数量的写法（「产出若干二进制，见 [`docs/README.md`](README.md) 的模块表」），让它们不再需要随每次迁入维护；同时在「新增一个模块时」清单里补一条，指明哪些跨模块文档带有会过期的计数（`architecture.md` 第 1 节的行数与固件数、`testing.md` 第 4、5 节）。

---

## notification 模块

### 9. 覆盖率的三处真实缺口

**影响**：中。

现状：`internal/notification` 97.1%、`hub` 83.7%、`peer` 96.2%。`hub` 那个数字里有相当一部分是**度量假象**——`Listener` 的 `WriteJSON`/`ReadMessage`/`Close` 等方法要一条真 WebSocket 才调得到，而那些连接建在 `internal/notification` 的测试里，`go test` 默认只把一个包自己的测试计入该包覆盖率。用 `-coverpkg` 合并度量后它们都是 100%。

合并度量之后剩下三处真缺口：

1. **`Hub.Publish` 摘除写失败 listener 的分支**（`hub.go` 174-179）。这是 `clients.active` 唯一的自愈路径。它坏掉的表现是集群面板上的活跃连接数只增不减，看起来像用户在涨。
2. **`replay` 的写失败路径**（`client.go` 160-162，以及 `readLoop` 里对它的 `return`）。它保证一个已经走掉的客户端不会被剩下的历史消息逐条重试。
3. **history 的按时长清理**（`hub.go` 210-215）。4096 条那道上限有 `TestHistoryPurgeHonoursTheSizeLimit` 压着，但真正约束线上重放窗口的是 60 秒那道，它没有任何测试。

**建议**：三条都不需要起进程。第三条优先，因为 `hub_test.go` 与被测代码同包，往 `h.history` 里塞一条时间戳提前的记录就能覆盖；也因为它是唯一一条「上限没生效了也看不出来」的——history 无限增长在测试里表现为一切正常。

### 10. Aphlict 配置文件里的多数键被静默丢弃

**影响**：中。是个运维陷阱。

`ServerSpec` 只读 `type`/`port`/`listen`，`Config` 只读 `servers`/`cluster`。而 Phorge 自带的 `conf/aphlict/aphlict.default.json` 还有 per-server 的 `ssl.key`/`ssl.cert`/`ssl.chain` 与顶层的 `logs`、`pidfile`——`encoding/json` 把它们静默忽略。**「能读 Aphlict 的配置文件」目前只兑现了一半，而多出来的那一半不报错。**

两个具体后果：

- 配了 `ssl.cert` 的文件递过来，服务照常起，但只讲明文 HTTP。PHP 侧 `notification.servers` 里若相应填了 `protocol: https`，`testClient()` 失败、面板报连接错误——**报了错，但报的地方离原因很远**。
- 默认文件里 admin 的 `listen` 是 `127.0.0.1`，而 `LoadFromFile` 只在该字段为**空**时才填默认值，所以这个值会被原样保留。容器里这等于 admin 口对 phorge 容器不可达，而 PHP 侧 `PhabricatorNotificationClient::tryToPostMessage()` 是 `catch (Exception $ex) {}` 全吞——通知完全不工作，且没有任何一处报错。

**建议**：`LoadFromFile` 把没识别的键列一条 `slog.Warn`。`config.go` 的注释已经写明 `ssl.*` 不生效，但注释拦不住一个把现成文件直接挂进容器的运维。

### 11. admin 口的 `GET /` 从 405 变成 200（已知偏离）

**影响**：低。

Aphlict 的 admin server 对非 POST 的 `/` 回 405（`support/aphlict/server/lib/AphlictAdminServer.js:114`）。Gorge 的 admin 口保留了平台层的根探针，`POST /` 与 `GET /` 方法不同、可以共存，于是 `GET /` 回 200 与裸 `{"status":"ok"}`。PHP 侧只打 `POST /` 与 `GET /status/`，观察不到这个差别，`TestAdminProbesAnswer` 把现状钉住了。

登记而不修，是因为「探针形状全仓库一致」比「与 Aphlict 逐位全等」更有价值。**唯一要留神的是别把 client 口也顺手这么处理**——那个端口的 `GET /` 必须是 501，见 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第五节。

### 12. 多实例部署要求配 `cluster`，而 `cluster` 只能从文件给

**影响**：高，但只在多实例时。

状态全在进程内存里。跑两个实例时，`tryToPostMessage()` 对 admin 服务器列表 `shuffle()` 之后只投给其中一个成功的，所以一条消息只进那一个实例的 hub；要让它到达连在另一个实例上的浏览器，只能靠 `cluster` 里的 peer 中继。而 `LoadFromEnv` **完全没有表达 `cluster` 的手段**，只有 `GORGE_NOTIFICATION_CONFIG_FILE` 那条路。

漏配的表现：每条通知只到一部分用户，看起来像偶发丢失；两个实例的 `/status/` 都正常，日志里什么都没有。

同源的第二件事：`replay` 读的是本实例的 history，而 history 随进程启动清空。刚重启过的实例上重放窗口是空的，重连的客户端拿不到断线期间的消息，也不会收到任何提示。

**建议**：给 `cluster` 补一个环境变量形式（`GORGE_NOTIFICATION_CLUSTER`，`host:port` 逗号分隔），让「多实例」不必绑定挂载文件这条更重的路。`deploy/compose/.env.example` 已经写明了这个前提，但那是文档层的补救，不是代码层的。

---

## mailer 模块

### 13. 重试默认值从 250 次 / 15 秒改成 2 次 / 2 秒（**已改，登记原因**）

**影响**：高，但是正向的。这条不是待办，是一次**行为变更的记录**——下一个看到这两个数字变小的人需要知道为什么。

迁入前的 `MaxRetries=250` / `RetryWait=15` 是**死配置**：`config.go` 读它们，而 `Dispatcher.Send` 从不使用，所以七个后端各只试一次。迁入时把它们真正接进了单适配器重试循环，于是那组值第一次有了含义——而它的含义是 `250 × 15s ≈ 62 分钟`，一次 HTTP 请求最坏阻塞一小时以上。

三个数字彼此矛盾：

| 谁 | 等多久 |
|---|---|
| 原默认值下的一个适配器 | 最坏 62 分钟 |
| PHP 客户端 | 30 秒 |
| Phorge worker 队列的下一轮 | 分钟级，且是外层重试的**权威** |

所以默认值改为 `2` / `2`（单适配器最坏 4 秒），并让整个重试循环受 `c.Request().Context()` 约束，客户端断开即止。Go 侧只吸收秒级抖动，重投这件事仍然归 worker 队列管。

`TestMailerRetryDefaultsStaySmall` 断言的不只是这两个数，还有它们的乘积低于 10 秒——**它守的是那个不等式，不是那两个字面值**。要调大，先想清楚哪一侧的等待更长。

### 14. 七个适配器里有四个的发送路径没有测试

**影响**：中。

`internal/mailer` 覆盖率 79.1%，缺口集中且可指名：SMTP 的两条发送路径（明文与 implicit TLS）、SendGrid / Mailgun / Postmark 的 HTTP 往返。

分类逻辑本身是测到的——`classifyProviderStatus` 与 `classifySMTPError` 有直接的表驱动用例，sendmail 用一个 stub 脚本走完了真实的退出码路径，SES 因为 `endpoint` 可配而用 `httptest` 打了完整的一圈（连带覆盖了 SigV4 签名）。缺的是另外三家 provider 的那一圈，原因很具体：**它们的端点是编译期常量**。

**建议**：把三个端点改成适配器字段，构造时从 `options["endpoint"]` 取、默认值保持现状。这样它们能照 SES 那样用 `httptest` 测，顺带让「provider 有区域性端点或企业私有部署」这件事变得可配——Mailgun 的 `api-hostname` 已经是这个形状了，另外两家只是没做。

### 15. `MAILER_CONFIG` 解析失败只有一条日志

**影响**：中。是个运维陷阱，但已经比迁入前好。

老代码是 `_ = json.Unmarshal(...)`：JSON 写错了就静默得到零个后端，而 `/healthz` 照样 200。现在会 `slog.Error` 一条，并且 `/readyz` 会因为「零后端」而报 503，所以这个状态不再伪装成健康。

但它仍然不是启动失败。「配置写错」与「还没配」在退出码上无法区分，而这两件事的处置完全不同。

**建议**：给一个显式的严格模式（比如 `GORGE_MAILER_STRICT=1` 时解析失败即 `os.Exit(1)`），让编排能在部署阶段就把配置错误拦下来，而不是等到第一封信。没有直接改成硬失败，是因为「先起服务、再补配置」是这个域的一个合理工作流——`/readyz` 已经把它表达清楚了。

---

## file-storage 模块

### 17. blob 后端的启用开关从「默认开」改成「必须显式给 host」（**已改，登记原因**）

**影响**：高，但是正向的。这条不是待办，是一次**行为变更的记录**——它改变的是「什么都没配」这个状态下服务的行为。

独立服务时期 `MYSQL_HOST` 默认 `127.0.0.1`、`MYSQL_BLOB_MAX_SIZE` 默认 `1000000`，而启用判据只看后者。两个默认值凑在一起的后果是：一个**只配了本地磁盘**的部署，照样注册一个指向根本不存在的数据库的 blob 后端。而这个后端优先级是 1：

- 它接走每一个 1 MB 以内的上传——也就是绝大多数上传——然后失败；
- `/readyz` 会 ping 它，于是**整个服务**被报成不可用，尽管本地磁盘好端端地配着；
- 运维看到的是「我明明配了本地磁盘，服务却说自己没就绪」，而配置文件里没有任何一处提到 MySQL。

现在 `MySQLBlobEnabled()` 要求 `MySQLHost != "" && MySQLBlobMaxSize > 0`。加上 host 这一条之后，「什么都没配」与「配了 blob」才成为两个可区分的状态——这正是 mailer 的 `MAILER_TYPE` 在那个域里做的事：**一个后端的存在必须是被声明出来的，不能是被默认值凑出来的。**

`TestNoBackendIsConfiguredByDefault` 钉住它：清空全部环境变量之后，三个 `*Enabled()` 必须全是 false。**它守的是「零配置等于零后端」这个不变量，不是那几个默认值本身**——`MYSQL_BLOB_MAX_SIZE` 的默认值 `1000000` 至今没变，也不该因为这条改动而变。

### 18. DELETE 改成幂等（**已改，登记原因**）

**影响**：中，正向。同样是行为变更记录。

独立服务时期的 mysqlblob 引擎在 `RowsAffected() == 0` 时返回错误——一个看起来很合理的「你删的东西不存在」。但它和调用方的实际用法冲突：**Phorge 是「删掉字节」和「删掉那条指向字节的记录」一气呵成的**。对已经消失的字节报错，会让它删不掉那条记录，于是数据库里留下一行永远退不掉、指向虚空的记录，而下一次 GC 会再试一次、再失败一次。

现在三个引擎一致地把「对象不在」当成功：本地磁盘忽略 `os.IsNotExist`，blob 引擎不看 `RowsAffected`，S3 本来就是这个语义。接口注释里写明了这是**接口的要求**而不是各引擎的巧合。

代价要说清楚：**调用方无法再区分「删掉了」与「本来就不在」**。这是有意放弃的信息——没有任何一个调用方会因为这个区别做不同的事，而它换来的是 GC 能推进。`TestDeleteBlobIsIdempotent` 与 `TestLocalDiskDeleteIsIdempotent` 各守一层，契约固件 `delete-blob.json` 也是照着一个没人 seed 过的 handle 写的。

### 19. `platform/` 没有数据库设施，并且**刻意不加**

**影响**：低（现在），但它是一个会被下一个人误判的结构决定，所以登记。

`go-sql-driver/mysql` 由 `internal/filestorage/db.go` 自己 import，连接池的三个参数（`maxOpenConns=25` / `maxIdleConns=5` / `connMaxLifetime=5m`）也定在那里。看到「仓库里第一次出现数据库」而顺手在 `platform/` 下开一个 `db` 包，是很自然的动作，**但现在做是错的**：一个域需要连接池不构成共享关切，抽出去只会得到一个只有一个调用方的包，而它的默认值必须替一个不存在的第二方猜测。

那三个参数是**按本域的用法定的**，不是通用值：这个服务只在一次 INSERT 或一次 SELECT 的时间里持有连接，行大小还被 `MySQLBlobMaxSize` 封着；而它跟 Phorge 共用同一个数据库实例，真正需要留出余量的是 Phorge 自己的连接池。换一个域来，这三个数字大概率都不合适。

**什么时候重新考虑**：第二个域需要连接池的时候，而且判据是那时两个域的 DSN 拼法与池参数是否真的能共用——不是「都用了 database/sql」。在那之前，`platform/` 保持没有数据库设施这件事本身就是文档：它说明「域包可以持有自己的外部依赖」。

### 20. S3 写路径必须声明 payload 未签名（迁入时发现的真实 bug）

**影响**：高。修之前，自建对象存储上的每一次上传都是失败的。

把 S3 的写路径从「缓冲整个文件再上传」改成「流式上传」之后，明文 HTTP 端点上的 `PutObject` **直接失败**，报 `request stream is not seekable`。原因不在本仓库：SigV4 默认要用 payload 的 SHA256 参与签名，而 SDK 算这个 hash 的办法是把 body 读一遍再 seek 回开头——**HTTP 请求体不能 seek**。

关键是这条路只在**明文 HTTP** 上走得到：SDK 在 HTTPS 上本来就改用「声明 payload 未签名」（传输层已经保护了完整性）。而明文 HTTP 端点恰恰是自建 MinIO / Ceph 的常态，也就是这个后端的主要使用场景。`s3.go` 的 `unsignedPayload` 把 SDK 在 HTTPS 上的那个选择延伸到明文 HTTP，且**只加在 `PutObject` 上**——这里只有它带 body。

另一条路是把每一次上传缓冲下来算 hash，但那正是改成流式要消掉的东西：16M 的传输上限乘上并发数就是这个进程的内存底线。

`TestS3RoundTrip` 钉住它，靠的是**用一个不可 seek 的 reader**（`oneByteReader` 包着 `strings.Reader`）而不是 `strings.Reader` 本身——后者是可以 seek 的，SDK 会走另一条路，测试照样通过而防线消失。**改这个测试时别把那层包装「简化」掉。**

### 21. 写入回退的范围由 rewind 预算限定，而它不覆盖一整类失败

**影响**：中。不是 bug，是一个必须被知道的边界。

`Router.Write` 在某个引擎失败时会换下一个试，这是迁入时补上的能力（它要接住的是 `bin/storage upgrade` 跑之前 `file_storageblob` 不存在的那段时间）。但**字节只存在一次**：`rewindReader` 只记录预算以内的字节，超出之后把记录整个丢弃并拒绝重放——留一个前缀会让下一个引擎存下一个截断的文件，那比失败坏得多。

预算是**推导出来的**：各引擎里最大的那个大小限额，实践中就是 blob 引擎的 1 MB。它**不是可调参数**，也不该做成可调参数——它的含义是「那个不肯流式的引擎最多会读多少」，而不是「我们愿意为重试花多少内存」。`TestRouterBudgetFollowsTheSizeLimitedEngines` 断言的正是这条推导关系。

**它不覆盖的那一类同样真实**：本地磁盘或 S3 写到一半失败，任何预算都救不回来——它们是流式的，字节早已流走。这一类目前是可以接受的，因为这两个引擎后面本来也没有第三个可以接；但**如果将来在 S3 之后再加一个后端，这条就会开始咬人**，而它的表现是「失败」而不是「静默错误」，这一点值得庆幸。`TestRouterStopsWhenTheFailedWriteConsumedTheBody` 把这个边界钉死，它断言的不只是整体失败，还有「下一个引擎一个字节都没收到」。

### 22. 被 recover 的 panic 会藏在一片绿色的状态码断言背后

**影响**：高。这是一条**测试方法学**的发现，不限于本域——另外三个域现在都还没有这道防线。

迁入过程中真实发生过一次：一个 handler 拿着 nil 引擎调了 `ReadFile`，panic 了，而**每一条状态码断言都是绿的**。链条是这样的：

1. 一个 helper「顺手」把 `httpx.Fail(...)` 的返回值当成「出错了没有」往上传——但 `Fail` 在响应写成功时返回 **nil**，也就是说「答了一个 400」这件事在调用点读起来是「没问题」；
2. handler 于是继续往下走，拿着 nil 引擎 panic；
3. 平台层的 `Recover` 中间件把 panic 变成 500；
4. `errorHandler` 看到响应**已经 committed**（第 1 步那个 400 已经写出去了），于是不再落笔；
5. 于是测试看到的还是那个 400，断言通过。

**唯一的证据是日志里一行没人看的堆栈。**所以 `router_test.go` 的 `quietLogs` 现在做两件事：把有意为之的失败路径日志静音，以及在 `t.Cleanup` 里检查捕获到的日志——**含有 `PANIC_RECOVERED` 就让这个测试失败**。契约固件的 runner（`contract_test.go`）也调它，因为一份只断言状态码的固件比单元测试更没有分辨能力。

顺带记下 `resolveTarget` 现在的形状是这条发现的产物：它**返回一个拒绝原因的字符串而不是自己应答**，这样调用点没有「把 nil 当成没问题」的机会。函数注释里写明了理由。

**建议**：render / diff / notification / mailer 四个域各自的测试 helper 都加同一道检查。成本是十行，而它挡的是一整类「测试全绿、handler 在崩」的情况。

### 23. 仓库里现在有两套 AWS 签名实现

**影响**：低，但它会把下一个想「统一一下」的人引向错误的方向。

`aws-sdk-go-v2` 随 file-storage 首次进入单仓（`aws-sdk-go-v2` / `credentials` / `service/s3`，连同十几行传递依赖），`s3.go` 用它签请求。而 `internal/mailer/ses.go` 里躺着约四十行手写 SigV4——`deriveSigningKey` 那条 `AWS4` → date → region → service 的派生链，以及 `AWS4-HMAC-SHA256` 头的拼装。**同一个仓库、同一个签名算法、两份实现。**

看起来该二选一，但两个方向现在都是错的：

- 让 mailer 改用 SDK，是为一次 `POST` 表单请求引入 `service/sesv2`。手写签名换来的是端点可配、可以用 `httptest` 打完整的一圈——第 14 条里 SES 是七个适配器中唯一被测全的那个，靠的正是这一点。
- 让 file-storage 改成手写，则要自己实现 path-style 端点、重试、以及 `UNSIGNED-PAYLOAD` 那条（第 20 条）——这些恰好是 SDK 已经做对的部分。

**登记而不修**，同时把判据留下：值得抽出去的时机是**第三个域也需要 AWS 签名**，而那时该抽的是「签一个请求」这件事本身，不是「都换成 SDK」。这条与第 19 条是同一个形状——两个调用方不构成共享关切——只是那边说的是连接池。

### 24. 删除路径上的非法 handle 曾经答 500（**已修**）

**影响**：中，正向。这条既是行为变更记录，也是第 18 条的直接副产品。

第 18 条把 DELETE 改成幂等之后，`deleteBlob` 手里就只剩一种失败可以往上抛了，而**它把两件性质完全不同的事抛成了同一个 500**：

- 后端真的没删掉（磁盘只读、数据库连不上）——字节可能还在，Phorge 绝不能退掉指向它的记录。500 在这里是诚实的。
- 调用方传了一个任何引擎都不可能签发的 handle——**服务什么问题都没有**，可它答的是「服务内部错误」。

第二种在 500 里意味着：日志里多一条 `REQUEST_FAILED`，运维按 500 的排查路径去翻服务日志，而真正该看的是调用方传了什么。Phorge 自己不会传出这种 handle，所以触发它的现实场景是**Phorge 数据库里那条记录本身坏了**——这恰恰是最需要错误信息指对方向的时候。

修法是把「格式非法」做成一个可辨认的错误而不是一句文案：引擎的 handle 校验统一 wrap `ErrBadHandle`（`engine.go`），`deleteBlob` 用 `errors.Is` 认出来答 400 `ERR_BAD_REQUEST`。**没有新增域级错误码**，收敛进平台码。

**读路径刻意不跟着改**：`readBlob` 对同一个非法 handle 继续答 404，理由见 [`modules/file-storage.md`](modules/file-storage.md) 第 3.4 节——读区分不出也不需要区分。于是同一个非法 handle 在读和删上答两个码，这处不对称是刻意的，`TestReadBlobBadHandleStays404` 与 `TestDeleteBlobBadHandleIs400` 成对钉住它，免得下一个人把其中一个「顺手统一」掉。`TestEnginesReportABadHandle` 则守着引擎那一层真的在报这个哨兵——两个 handler 测试用的是 stub 引擎，绕过了真实校验。

**一般化的那条**：一个 endpoint 越是把失败算作成功，剩下那些真的失败就越需要被分开。幂等是拿「区分能力」换「调用方好写」，而换掉的那部分要在别处补回来。
