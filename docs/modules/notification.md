# notification 模块

实时通知：接住 PHP 侧发来的消息，通过 WebSocket 扇出给浏览器。它替换的是 Phorge 自带的 Node.js 版 Aphlict 服务（`support/aphlict/server/`），线协议逐字兼容。

**这是第一个自己占一个二进制、并且自己占两个端口的域**，所以承载关系要比 render/diff 那两行讲得细一点。

> **这份文档同时是本域的技术报告**，所以它比 [`render.md`](render.md) 与 [`diff.md`](diff.md) 长出一倍多。前六节仍然是 [`../README.md`](../README.md) 规定的那个骨架，第 7 节之后是骨架之外的东西：迁入改了什么、怎么排查、四层测试各守住什么。本域迁入前是一个独立仓库，那份仓库里的 `TECHNICAL_REPORT.md` 描述的是迁入**之前**的布局（`internal/config/`、`internal/hub/`、`cmd/server/main.go`、裸 `net/http` + `ServeMux`），那些路径现在一个都不存在——它只可作为历史材料，**不要拿它当当前实现依据**。本文与当前仓库代码才是维护入口。
>
> 三处内容**不在**这份文档里，因为它们由别的文件持有，抄过来只会漂移：与 Phorge 之间的兼容约束以 [`compat/phorge/README.md`](../../compat/phorge/README.md) 第五节为准；已知缺口与改进建议在 [`../findings.md`](../findings.md) 的 notification 一节；平台层设施的实现在 [`../platform.md`](../platform.md)，构建与发布在 [`../delivery.md`](../delivery.md)。

| | |
|---|---|
| 二进制 | `gorge-notification`（**不是** `gorge-render`） |
| 端口 | client `:22280`（WebSocket，**必须浏览器可达**）+ admin `:22281`（HTTP，只给 PHP） |
| 包 | `go/internal/notification/`、`go/internal/notification/hub/`、`go/internal/notification/peer/` |
| 入口 | `go/cmd/gorge-notification/main.go` |
| 契约 | [`api/openapi/notification.yaml`](../../api/openapi/notification.yaml)、[`go/internal/contracts/notification.go`](../../go/internal/contracts/notification.go) |
| 依赖 | Fiber v3 + `gofiber/contrib/v3/websocket`（底层 `fasthttp/websocket`）；版本以 [`go/go.mod`](../../go/go.mod) 为准 |
| 固件 | `tests/contract/notification/admin/` + `client/`，**分两个目录因为它们是两个端口** |
| e2e | `tests/e2e/notification.sh`（5 条场景，用 `ADMIN_URL` + `CLIENT_URL` 两个变量，不是 `BASE_URL`） |
| 兼容约束 | [`compat/phorge/README.md`](../../compat/phorge/README.md) 第五节 ← **改动前必读** |

### 为什么不并进 `gorge-render`

diff 并进 render 的理由是「两个域都是无外部依赖的纯计算，拆进程换不来隔离收益」（见 [`diff.md`](diff.md) 第 1 节）。这句话在本域三处都不成立：

- **它有进程状态。** `hub.Hub` 里的连接表与 history 是进程内存，重启即清空。纯计算服务可以随便滚动重启，本域的每次重启都会断开所有浏览器连接。
- **它有长连接。** 一个 WebSocket 的生命周期就是一个 handler 的生命周期，可以是几小时。把它和高亮请求塞进同一个进程，`ShutdownTimeout` 的排空语义会同时服务两种寿命差三个数量级的请求（这件事的实际结局见第 4.6 节，比看起来的更微妙）。
- **它不能鉴权。** 严格 Aphlict 兼容意味着两个端口都不挂 `auth.Token`（理由见第 5 节）。而 `gorge-render` 的 token 认证的是「调用方对这个**进程**的身份」；把一个必须裸奔的域并进去，同一个进程里就会一半路由要 token、一半路由必须不要，这个进程的安全边界就没法一句话说清了。

### 为什么两个端口在一个进程里

因为 `hub.Hub` 是两个端口共用的那一份状态：admin 端口往里 `Publish`，client 端口的 listener 从里面读。拆成两个进程就得在它们之间再造一条通信通道，而那条通道要解决的问题正是 hub 已经解决了的。

`main.go` 因此为 `cfg.Servers` 里每条 spec 建一个 `httpx.Server`，最后交给 `httpx.RunAll` 一起跑——这是平台层为本域新增的两项设施之一，见 [`../platform.md`](../platform.md) 第 1.4 节。

### 两个地址的不对称，是最容易配错的一处

| 端口 | 谁来连 | 地址填什么 |
|---|---|---|
| admin `:22281` | phorge 容器里的 PHP | compose 内网服务名（`gorge-notification`），生产部署里**不要**映射到宿主 |
| client `:22280` | 用户浏览器里的 `JX.Aphlict` | **浏览器可达的外部地址**，必须映射到宿主 |

client 那一条填错时的表现值得单独记一下：`PhabricatorNotificationServerRef::getWebsocketURI()` 是把地址**发给浏览器**的，所以填了 `gorge-notification` 这种只在 compose 内网解析得开的名字，服务端一切健康、Config → Cluster → Notification 页面两台服务器都显示正常，只有每个真实用户的浏览器连不上。**服务端没有任何一处会察觉到这件事。**

镜像的 `HEALTHCHECK` 指向 admin 口（`deploy/compose/docker-compose.yml` 里传 `PORT: 22281`，`.github/workflows/release.yml` 的 matrix 里也带同一个值）。它可以指向任意一个口——两个口都注册了 `/healthz`——选 admin 是因为它是纯 HTTP，探针不必关心 WebSocket。这个构建参数只喂 `ENV GORGE_HEALTHCHECK_PORT`，服务本身不读它；漏传的话它会停在 render 的 8140 默认值上，探针打向一个没人监听的端口，容器起来之后反复重启。

## 1. 职责边界

### 1.1 它替换掉的是一整条运行时依赖

render 域替换的是一次 `fork` + Python 解释器初始化，省下的是每次请求的进程开销。本域替换的是一个**常驻的外部 Node.js 服务**，所以省下的东西不一样：

- PHP 服务器上不必再装 Node.js 与 `ws` 这类 npm 包，一整条与主体应用无关的运行时与它的供应链从部署里消失；
- 交付形态与仓库里其他服务对齐——同一份参数化 `go/Dockerfile`、同一个 `httpx` 引导、同一批探针路径（见 [`../delivery.md`](../delivery.md)）；
- Aphlict 的 `bin/aphlict start` 那套 pidfile + 日志文件的进程管理不再需要，日志直接进 stdout 交给容器运行时收（这也是 Aphlict 配置里 `logs` 与 `pidfile` 两个键被丢掉的原因，见第 4.5 节）。

代价是引入了一条兼容边界，而它的形状是各域里最难受的：Aphlict 的线协议既被 PHP 侧调用、又被浏览器里的 `JX.Aphlict` 调用，两侧都不读本服务的错误信息。

### 1.2 负责与不负责

**负责**：接收 Aphlict 消息并按 `subscribers` 过滤后扇出给订阅了对应 PHID 的浏览器；维护一小段可重放的 history；把连接与消息计数报给 Phorge 的集群面板；把消息中继给配置了的 cluster peer。

**不负责**：决定谁该收到什么。`subscribers` 是 PHP 侧算好放进消息里的，本域只做集合求交。也不负责持久化——消息不落盘，见下。

**不负责渲染通知内容**。消息体对本域是透明的：`hub.Message` 是 `map[string]any`，原样进 history、原样发给浏览器，只有 `subscribers` 与 `touched` 两个键对本域有意义。`tryToPostMessage()` 还会往消息里塞一个 32 字符的 `uniqueID`，本域同样不读它。Phorge 将来往消息里加字段不需要改这里。

### 1.3 「可降级」是好几处设计的前提

状态全在内存、失败只告警不阻塞、peer 中继失败只 `slog.Warn` 不重试——这些选择的共同前提是 `PhabricatorNotificationClient::tryToPostMessage()` 在 PHP 侧的这段代码：

```php
foreach ($servers as $server) {
  try {
    $server->postMessage($data);
    return;
  } catch (Exception $ex) {
    // Just ignore any issues here.
  }
}
```

通知丢了没人会收到报错——这既是它可以被简单实现的原因，也是它出问题特别难发现的原因。

**但「PHP 侧全吞」这句话有一个例外，值得单独记住**：`/status/` 的失败是可见的。`PhabricatorAphlictSetupCheck` 调 `tryAnyConnection()`，它对第一台 admin 服务器打 `loadServerStatus()`，抛异常就在 Config 里挂出一条 "Unable to Connect to Notification Server" 的 setup issue。所以两个 admin 端点的可观测性是不对称的：**`GET /status/` 坏了会报，`POST /` 坏了不会**。第 5 节四条约束的危险程度排序就是从这条不对称来的。

### 1.4 PHP 侧给的时间预算是 2 秒

`PhabricatorNotificationServerRef::newFuture()` 对三个调用（`postMessage`、`loadServerStatus`、`testClient`）一律 `setTimeout(2)`。这个数字约束了 admin handler 的设计，并且它比本域内部的 peer 超时**更短**：

| 环节 | 超时 |
|---|---|
| PHP 等本服务应答 | 2 秒（`HTTPSFuture::setTimeout`） |
| 本服务中继给一个 peer | 5 秒（`peer.broadcastTimeout`） |

所以 `Peers.BroadcastMessage` 里那句 `go p.BroadcastMessage(...)` 不是「顺手并发一下」：**同步做的话，一个不可达的 peer 就会让 PHP 侧超时**，而超时被上面那个 catch 吞掉，表现是通知偶发丢失、且只在某台 peer 挂掉时发生。goroutine 把 5 秒挪到了请求响应之外。

`setFollowLocation(false)` 也在那个方法里，注释写着 Aphlict 从不发 `Location:`，收到就说明有事不对——本域同样从不发重定向，改动路由时别引入一个。

## 2. 路由与依赖

两组路由分别注册在两个 Fiber app 上，互不可见。

```go
// client 口
func RegisterClientRoutes(app fiber.Router, deps *ClientDeps) {
	app.Get("/", serveClient(deps))
	app.Get("/*", serveClient(deps))
}

// admin 口
func RegisterAdminRoutes(app fiber.Router, deps *AdminDeps) {
	app.Post("/", postMessage(deps))
	app.Get("/status/", serverStatus(deps))
}
```

| 端口 | 方法 | 路径 | 谁在调 | 鉴权 |
|---|---|---|---|---|
| client | GET | `/`、`/*` | 浏览器（WS 升级）／Phorge 的 `testClient()`（纯 HTTP，期望 501） | 无 |
| client | GET | `/healthz`、`/readyz` | 容器探针（平台层注册） | 无 |
| admin | POST | `/` | `PhabricatorNotificationServerRef::postMessage()`、以及 peer 的中继 | 无 |
| admin | GET | `/status/` | `loadServerStatus()`，集群面板与 `PhabricatorAphlictSetupCheck` | 无 |
| admin | GET | `/`、`/healthz`、`/readyz` | 容器探针（平台层注册） | 无 |

**没有路由组，这是本域与 render/diff 的一处结构差异。** 那两个域的 `/api/**` 挂在带 `auth.Token` 的分组下，本域两个端口则是裸路由。错误状态由 Fiber 的路由器与平台错误处理器统一映射；具体状态属于契约，由 handler / contract 测试锁住，不应从框架名称推导。

### 2.1 三处路由细节都是有理由的

- **client 口的 `GET /` 不是平台层那个探针。** `httpx.Config.SkipRootProbe` 为 true 时 `health.Register` 跳过 `app.Get("/", Live())`，把根路径让给域包。这是平台层为本域新增的第二项设施（[`../platform.md`](../platform.md) 第 3.1 节），存在的唯一理由是兼容约束二（第 5.2 节）。
- **`GET /*` 不会吃掉 `/healthz`。** Fiber 的静态路由优先于通配符，所以两个容器探针在通配符下面照样应答。`TestClientProbesOutrankTheWildcard` 钉住了这条假设——它是本端口 `HEALTHCHECK` 能工作的前提，而通配符若真吃掉了探针，表现是容器反复重启，不是 404。
- **`/status/` 的尾斜杠属于契约。** PHP 侧写死 `getURI('/status/')`，`/status` 返回 404。`TestStatusWithoutTrailingSlashIsNotFound` 明写了这一点，免得有人为「整齐」把它改成无斜杠版。

### 2.2 Deps 与那一份共享的 hub

`ClientDeps` 只有 `Hub`，`AdminDeps` 是 `Hub` + `Peers`。两者共用 `main.go` 里建的**同一个** `*hub.Hub`——这是整个双端口结构的支点，把它拆成两个 hub，两个端口就变成两个互不相干的服务了。

`peer.List` 只给 admin 口，因为 fingerprint 与中继都只发生在消息进来的那一侧。

### 2.3 启动装配：`main.go` 与它拒绝掉的两种写法

```go
cfg, err := notification.Load()          // 一道校验，两类 server 必须都在
messages := hub.New()                    // 一个 hub
peers := peer.NewList()                  // 本进程的 fingerprint 在这里生成
for _, spec := range cfg.Cluster { peers.AddPeer(peer.NewPeer(...)) }
for _, spec := range cfg.Servers { /* 按 type 建 httpx.Server 并注册路由 */ }
httpx.RunAll(servers...)
```

两个 `httpx.New` 的参数不同，差异就是本域全部的端口级配置：

| | client | admin |
|---|---|---|
| `SkipRootProbe` | **true** | false |
| `Ready` | nil | nil |

`Ready: nil` 两处都是显式写的并带注释：hub 全在内存，就绪等于存活，没有第三方依赖可探。

拒掉的第一种写法是**每个端口各起一个 goroutine 跑 `Server.Run()`**。那样每个端口各注册一次信号处理，得到几个互不知情的关闭流程；更糟的是绑不上端口的那一个静静退出，进程还活着、另一个端口的健康探针还绿。`RunAll` 共用一个 `signal.NotifyContext`，并且**任一 listener 出错就拖着整组停**——只有 client 口活着的通知服务会接住浏览器连接然后永远没有东西可以告诉它们。理由与实现都在 [`../platform.md`](../platform.md) 第 1.4 节。

拒掉的第二种写法是**在一个 Fiber app 上挂两组路由再起两个 listener**。两个端口的 `GET /` 语义相反（admin 是 200 探针、client 必须是 501），同一个路由表表达不了这件事。两个 app 是这条约束的直接结果，也是契约固件必须分两个目录的原因。

## 3. 核心实现

### 3.1 admin `POST /`：四步，其中两步是为了不出事

```
自己解 body → 查/盖 fingerprint → Publish 进 hub → 并发中继给 peer → 裸 receipt
```

**第一步刻意不用 `c.Bind()`。** Phorge 用 `HTTPSFuture` 发这个 POST，payload 是 `phutil_json_encode()` 出来的裸 JSON，但 curl 给它贴的是默认的 `application/x-www-form-urlencoded`。Fiber 的 binder 会按 Content-Type 把这段字节当表单，而不是当 JSON。于是有两种结局，都不是「被拒绝」这么干净：

| 消息里含什么 | `c.Bind()` 的结局 |
|---|---|
| 一个非法的百分号转义（`100% done` 这种） | `url.ParseQuery` 报错 → 400 `invalid URL escape "% d"` |
| 其他任何东西 | 整段 JSON 变成一个没有 `=` 的垃圾键，`msg["type"]` 是 nil，**照常答 200 加真 fingerprint** |

第二行才是真正的失败模式。它**不产生任何错误**：PHP 侧的 `postMessage()` 成功返回，`messages.in` 照常增长，集群面板全绿，日志里连一条 4xx 都没有，只有消息内容被静默揉碎。

**415 不是这条兼容约束的判据。** `application/x-www-form-urlencoded` 是 binder 认识的类型，所以真实流量可能被按错误格式成功解析。当前 handler 直接对 `c.Body()` 调 `json.Unmarshal`，让 JSON 语义独立于 Content-Type；测试必须检查进入 hub 的字段，而不能只看 200。

这一条是迁入过程中真踩到的，完整推理在 [`compat/phorge/README.md`](../../compat/phorge/README.md) 第 5.4 节。**挡住它的是那个带 `100%` 的 payload，不是「断言了 200」这件事**——把这几处断言各自守住多少讲清楚很要紧，因为把它们记强了比不记更坏，见第 9.3 节。

顺带一个后果：请求体为空时 `json.Decode` 返回 `io.EOF`，落到 400 `ERR_BAD_REQUEST`，这也是本 handler 唯一走信封的路径。

**第二步是集群防环的全部。** `peers.AddFingerprint(msg)` 往消息的 `touched` 列表里盖本进程的 fingerprint，返回「这条消息在这里是新的吗」。已经有本进程指纹的消息说明它绕了一圈回来了，此时**照样回 receipt 但不 republish**——不然一条消息会重复投递，并在 peer 之间永远转下去。fingerprint 是每进程随机生成而不是配置的，这样两台服务器不可能因为配置失误撞上同一个。

第三步与第四步：`Hub.Publish` 同步做本地扇出，`Peers.BroadcastMessage` 给每个未在 `touched` 里的 peer 起一个 goroutine（理由见第 1.4 节）。响应用 `c.JSON()` 而非 `httpx.OK()`——这是刻意的信封豁免，见第 5.3 节。

### 3.2 admin `GET /status/`：一行 handler，两处不能动

```go
return c.Status(http.StatusOK).JSON(deps.Hub.Status(instanceOf(c)))
```

`Hub.Status()` 直接返回 `*contracts.AphlictStatus`，handler 不做任何加工。两处约束：**键里的点是字面量**（PHP 用 `idx($details, 'clients.active')` 读），以及**不套信封**。都在第 5.3 节。

`instanceOf(c)` 读 `?instance=`，空则 `default`。这个查询参数是 `getURI()` 在 `cluster.instance` 有值时统一追加的，所以 admin 与 client 两侧都会带上它；client 侧另外还把实例编进路径（第 3.5 节），于是那一侧的实例名在同一个 URL 里出现两次。本域按 Aphlict 的做法**只读路径那一份**。

### 3.3 client 口的升级链：先判 501，再交给 upgrader

```
非 Upgrade 请求 ──► 501 + "HTTP/501 Use Websockets\n"      ← 兼容约束二，逐字节
Upgrade 请求 ──► contrib websocket upgrader ──► 入 hub ──► readLoop
```

`websocket.New` 只在 `websocket.IsWebSocketUpgrade(c)` 成立后调用。干净升级由 contrib handler 接管连接，读循环结束后返回 `nil`；握手被拒时返回 `*fiber.Error`，交给平台错误处理器。`TestUpgradedResponseIsNotEnvelopedByTheErrorHandler` 锁住成功升级后不会再追加 JSON 信封。

配置使用 `AllowEmptyOrigin: true`，并且不设置 Origin 白名单。**这不是放松了 Aphlict 的姿态，而是复现它**：本端口按设计就在另一个 host:port 上，浏览器带来的 Origin 是 Phorge 的源，永远不等于本服务自己的源。这里既不读 cookie 也不读任何凭据，所以同源检查挡不住任何本来挡得住的东西。

### 3.4 线协议：四条命令，三种沉默

`readLoop` 认 `subscribe`、`unsubscribe`、`replay`、`ping` 四条命令，与 `AphlictClientServer.js` 的 `switch` 逐条对应。三处「不报错」是刻意的：

- **畸形帧与未知命令一律跳过。** Aphlict 也只写一行日志，而浏览器拿到一句协议抱怨也无从处置；报错的代价是 JS 客户端与本服务版本不齐时连接会被关掉。`TestUnknownCommandsAreIgnored` 连发一个假命令与一段非 JSON，然后用一次 ping/pong 证明会话还活着。
- **`ping` 的写失败被丢掉。** 写不动说明对端走了，读循环下一轮自会发现。
- **`replay` 的写失败会结束会话。** 这是唯一一处「失败就走」，而它与 Aphlict 有一处小差别：Aphlict 是 `break` 出重放循环、连接留着，本域是让 `readLoop` 返回、连接关掉。理由写在 `replay` 的注释里——客户端已经走了，剩下的消息会以同样的方式一条条失败。

`replay` 的 `age` 缺省 60000 毫秒，与 Aphlict 的 `message.data.age || 60000` 同值。`data` 整个缺失时 `json.Unmarshal` 报错，同样落到这个缺省值上，所以 `{"command":"replay"}` 这种不带 data 的帧是合法的（`TestReplayHonoursSubscriptions` 就这么发）。

**重放要过两道过滤**：先按时间（`Hub.GetHistory`），再按订阅（`IsSubscribedToAny`）。第二道不能省，不然一次重连就会把别人的通知漏给这个浏览器，这也是 `TestReplayHonoursSubscriptions` 存在的理由——它把「不该收到的那条」先发出去，所以过滤坏掉时读到的是错的那条，而不是靠等一个超时来证明「没收到」。

### 3.5 实例名从路径里切

`~{instance}/` 从路径里切出来：`getWebsocketURI()` 在 `cluster.instance` 有值时会把它追加进 URI。裸 `~` 与切不出内容的情况都归 `default` 实例，`TestParseInstance` 用六种路径压住这张表。

与 Aphlict 的实现有一处**只在 Phorge 不会生成的路径上**才看得出的差别：Aphlict 是 `path.split('~')[1]` 再删掉所有 `/`，本域是取第一个 `~` 之后的全部再 `strings.Trim` 掉两端的 `/`。所以 `/~a/b/c` 在 Aphlict 是实例 `abc`、在这里是 `a/b/c`。`getWebsocketURI()` 只会生成 `~{instance}/` 这一种形状，所以这条差别到不了真实流量；记在这里是因为它看起来像 bug，而它不是。

### 3.6 hub：进程内存就是全部状态

`Hub` 持有「实例 → 连接表」与一段共享 history。

**`getList` 是双重检查锁定。** 先读锁查一次，没有再升写锁查一次然后建。每条发布的消息与每次状态查询都要过这里，而实例创建每个实例只发生一次，所以读多写少的形状值得这个额外分支。

**每个实例的连接表自带一把锁**（`listenerList.mu`），所以一个实例的扇出不会阻塞另一个实例。扇出前先 `snapshot()` 把 listener 拷出来再写，否则一个慢客户端会卡住同实例上的每一次投递——而且遍历中修改 map 会直接 panic。

**写失败当场摘除。** `Publish` 里写不动的 listener 直接从表里删掉并 `Close()`，不等它自己的读循环发现。这是 `clients.active` 唯一的自愈路径；不摘的话这个数只会单向增长，而集群面板上看不出这是死连接还是真用户。这条分支恰好也是覆盖率的一处真实缺口（[`../findings.md`](../findings.md) 第 9 条第 1 项）。

**history 有两道上限**：4096 条与 60 秒，跟 Aphlict 一致。清理是惰性的——每次 `Publish` 顺手调一次 `purgeHistory()`，不另起定时器 goroutine，于是清理频率自然跟着流量走。两道上限取更严的那个结果：`keep` 从条数约束算出的起点开始，再往前扫到第一条没过期的。起作用的主要是时间那道——请求重放超过一分钟的客户端拿到的是「剩下的」，不是错误。

写锁只覆盖 `append` 与 `purgeHistory`，**不覆盖扇出**。这是热路径上唯一值得说的一处：网络 I/O 在锁外。

history **不按实例分区**：`Publish` 不记录消息属于哪个实例，`GetHistory` 也不筛。所以一个实例上的客户端重放时，理论上能看到另一个实例的消息（`replay` 只按 `subscribers` 过滤，而无 `subscribers` 的广播消息不被这道过滤挡下）。**这不是移植引入的**——Aphlict 的 `_messageHistory` 挂在 admin server 上，client server 的 `getHistory()` 是去问所有 admin server 再拼起来，同样不分实例。照抄了，没有「顺手修正」，因为改动会让重放行为与被替换的实现不一致。

`Status()` 直接返回 `*contracts.AphlictStatus` 而不是先造一个域内结构再转换，理由与 render 域 `Highlight()` 的选择相同：第二个形状要带自己的 json tag，两者迟早会漂移，而这个形状的键名是 PHP 直接索引的（第 5.3 节）。计数的口径也照抄 Aphlict：`clients.*` 按实例统计（来自 `listenerList`），`messages.*` 是进程全局（`atomic` 计数器），`history.size` 同样是全局。`messages.out` 数的是**投递次数**而不是消息数，一条消息扇给两个订阅者就 +2，所以它通常大于 `messages.in`。

`version` 报 8。它描述的是线协议版本而不是本服务的版本，所以只在协议变化时才动。**注意 `phorge-fork` 里那份 Aphlict 报的是 7**，本域比它高一位；PHP 侧只把这个值当集群面板上的一个展示标签（`pht('Version %s', …)`），没有任何一处做比较，所以差异的全部后果就是那一行字。

### 3.7 Listener：两把锁不是过度设计

```go
type Listener struct {
	id            uint64
	conn          *websocket.Conn
	subscriptions map[string]struct{}
	mu            sync.RWMutex   // 保护 subscriptions
	writeMu       sync.Mutex     // 串行化写
}
```

`writeMu` 必须与 `mu` 分开：底层 WebSocket 连接只允许一个 writer，而本域真的有两个写入方——`Publish` 的扇出，与客户端自己那条读循环发出的回复（pong、重放）。如果复用 `mu`，一次卡住的网络写会同时挡住 `IsSubscribedToAny`，于是整条扇出流水线停在一个慢客户端上。

`subscriptions` 用 `map[string]struct{}` 而不是切片：订阅匹配在每条消息的每个 listener 上都要做一次，O(1) 查找加短路返回，而典型场景是「消息带少量 PHID、listener 只订了自己那一个」。

**Listener 不记自己属于哪个实例。** 迁入前它有这个字段；现在没有了，注释写明了理由——Hub 已经把它挂在某个实例下面，第二份副本只可能与第一份不一致。实例名现在由调用方（`serveClient` 的 defer）持有。

### 3.8 peer：fingerprint 网格

`cluster` 配了几个 peer，就往几个 peer 的 admin 口转发。防环靠 `touched` 列表，两层：

1. **消息级**：`AddFingerprint` 见到自己的指纹就判定「来过了」，不 republish。
2. **peer 级**：广播前跳过指纹已在 `touched` 里的 peer，省掉一次注定被对端丢掉的请求。

peer 的 fingerprint 是从它自己的 POST receipt 里**学**来的，所以第一次中继之前它是空的——空 fingerprint 的 peer 一律尝试，因为它不可能出现在 `touched` 里。`TestPeerBroadcastLearnsTheFingerprint` 与 `TestListBroadcastSkipsPeersThatHaveSeenTheMessage` 分别压这两半。

中继复用 admin 口本身：`POST {protocol}://{host}:{port}/?instance={instance}`，`Content-Type: application/json`，body 就是原消息。**所以集群通信没有第二套协议，也不需要 admin handler 为它开分支**——这一点与 `AphlictPeer.js` 完全一致，连 URL 形状都一样。

与 Aphlict 的两处差别都是收紧：

- `NewList()` 用 `crypto/rand` 生成指纹，Aphlict 用 `Math.random()`。字母表（55 个字符，去掉了 `0`/`O`、`1`/`l`/`I` 这类易混字符）与长度（16）逐字照抄，空间 55¹⁶ ≈ 7×10²⁷。
- peer 的 `http.Client` 带 5 秒超时，Aphlict 那边是裸 `http.request` 没有超时。理由见第 1.4 节。

中继失败只 `slog.Warn`，不重试。通知是可降级功能，一条偶发丢失的代价远小于重试逻辑的复杂度——`TestPeerBroadcastSurvivesAnUnreachablePeer` 用一个永不监听的端口压住「不 panic、不阻塞」。

### 3.9 并发模型

锁的层级是从外到内的三层，不存在反向获取，所以没有死锁可能：

```
Hub.mu            → instances map 与 history
  listenerList.mu → 某个实例的 listeners map
    Listener.mu / Listener.writeMu → 订阅集合 / 写
```

`nextID`、`messagesIn`、`messagesOut` 走 `atomic`，热路径上完全无锁。

**每条连接一个 goroutine，不是两个。** 读由 handler 自己那条 goroutine 做（`readLoop` 就是 handler 的主体），写由 `Publish` 的调用方直接做——也就是处理 admin POST 的那条 goroutine。底层连接支持这种「一读一写」的形态，所以不必为每条连接再起一条写 goroutine，几千并发连接时这是一半的 goroutine 与对应的栈内存。代价是 `writeMu` 必须存在（第 3.7 节），以及一个慢客户端会占住 admin 请求的一小段时间——这是 `snapshot()` 之外还要靠「写失败当场摘除」兜底的原因。

## 4. 配置

### 4.1 刻意不嵌 `config.Base`

`notification.Config` **不嵌 `config.Base`**，而这是全仓库唯一一处「不嵌是对的」而非结构问题（对比 [`../findings.md`](../findings.md) 第 4 条）。两个理由：

- `Base` 带 `ServiceToken`，而严格 Aphlict 兼容要求不鉴权——配上 token 会让 Phorge 发来的每条消息静默失败；
- `Base.ListenAddr` 的语义是 `host:port`，而本域是「一个绑定主机 + 两个端口」，套不进去。

于是 [`../findings.md`](../findings.md) 第 4 条预告的「第三个域进来会咬人」在这里没有兑现，那一条继续留在 findings 里等下一个**并入既有进程**的域。

### 4.2 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `GORGE_NOTIFICATION_CLIENT_PORT` | `22280` | client 口，浏览器连的那个 |
| `GORGE_NOTIFICATION_ADMIN_PORT` | `22281` | admin 口，PHP 打的那个 |
| `GORGE_NOTIFICATION_LISTEN_ADDR` | `0.0.0.0` | **绑定主机，不是 `host:port`**，两个端口共用 |
| `GORGE_NOTIFICATION_CONFIG_FILE` | 无 | Aphlict 格式 JSON |

规范变量命名规则见 [`../platform.md`](../platform.md) 第 4 节。Aphlict 时代的裸变量已移除，`TestRetiredAliasesAreIgnored` 防止它们意外恢复。

两个默认端口沿用 Aphlict 的值，不是随手挑的：Phorge 的 `notification.servers` 里已经写着它们，而 client 那个值还会到达浏览器。

`ServerSpec.Addr()` 用 `net.JoinHostPort` 而不是 `fmt.Sprintf("%s:%d")`，这样 IPv6 字面量能带上方括号（`::1` → `[::1]:22280`），空 listen 得到 `:22280` 即绑全部接口。`TestServerSpecAddr` 四行覆盖这四种。

### 4.3 文件模式是替换，不是逐字段覆盖

这是本域与 render 域取值语义的唯一差异：`config.LoadJSONFile` 的通常用法是「文件只覆盖它提到的东西」，但文件里的 `servers` 列表就是监听器的**完整集合**，跟默认那一对合并会起一堆没人要的端口。所以 `Load()` 指了文件就整份换掉，再统一校验。`TestLoadPrefersTheFileOverTheEnvironment` 同时设了文件与 `GORGE_NOTIFICATION_CLIENT_PORT`，断言拿到的是文件里那个值。

`LoadFromFile` 只补一个默认：条目没写 `listen` 时填 `0.0.0.0`（Aphlict 与 Phorge 自己的配置生成器都会省略它）。**只在字段为空时才填**，所以文件里写着的值一律保留——包括 Aphlict 默认配置里 admin 那个 `127.0.0.1`，那在容器里等于 admin 口对 phorge 容器不可达，见 [`../findings.md`](../findings.md) 第 10 条。

### 4.4 校验只有一条

admin 与 client 两类必须都在，缺任一类启动即失败；顺带拒掉未知的 `type` 与 ≤0 的端口。这跟 PHP 侧 `PhabricatorNotificationServersConfigType` 的校验是同一条——与其让服务安静地少监听一个端口，不如在启动时报出来。同一类有多条是允许的（`main.go` 按 spec 逐个起 listener），`TestLoadAcceptsExtraServersOfEachKind` 留着这条路。

`Load()` 是唯一的校验入口，所以 `main.go` 之后可以直接 `switch spec.Type` 而不必再有 `default` 分支。

### 4.5 Aphlict 配置里被丢掉的键

`ssl.key` / `ssl.cert` / `ssl.chain` / `logs` / `pidfile` 五组键**不进结构体**——`Config` 只有 `Servers` 与 `Cluster`，`ServerSpec` 只有 `type`/`port`/`listen`。迁入前它们是「解析了但不用」的字段，现在是「根本不解析」，原因写在 `config.go` 的类型注释里：日志走 stdout 交给容器运行时，TLS 在上游终止。

`encoding/json` 对多出来的键静默忽略，所以**把 Aphlict 的现成配置文件直接递过来不会报错，只会有一半不生效**。这是个运维陷阱，两个具体后果与建议在 [`../findings.md`](../findings.md) 第 10 条——挂现成文件之前先看那条。

`cluster` 只能从文件给，没有环境变量形式，这对多实例部署是个硬前提，见 [`../findings.md`](../findings.md) 第 12 条。

### 4.6 两个从平台层继承来的值，和它们在本域的实际含义

这两个都是 `httpx` 的默认值，`main.go` 一个都没显式传，但它们在本域的行为与在 render 域不同，值得单独说：

**`BodyLimit` = `2M`。** 迁入前 admin handler 自己用 `http.MaxBytesReader` 卡 1 MiB；那段代码没有了，现在卡住超大请求体的是平台层的 `BodyLimit` 中间件，超限答 413 `ERR_TOO_LARGE` 而不是从前的 400。这个值目前不能通过环境变量调（[`../findings.md`](../findings.md) 第 2 条）。两个值对通知消息都远远够用——一条消息通常几百字节——所以这次替换是纯收益：少一段手写代码，多一个与其他域一致的错误码。

**`ShutdownTimeout` = 10 秒。** 这里有一处反直觉：**它管不到 client 口的 WebSocket。** Go 的 `http.Server.Shutdown` 明确既不接管也不等待被 hijack 的连接，而一条升级完成的 WebSocket 正是这一类。所以收到 SIGTERM 之后，10 秒的排空只作用于 admin 口那些普通 HTTP 请求，client 口上那些长连接不会拖满这 10 秒，而是随进程退出被操作系统断掉，浏览器侧表现为一次重连。

**这也正是文档开头「长连接与短请求不该共处一个进程」那条理由的实际结局**：不是「排空语义被拉长」，而是「排空对其中一类根本不适用」。真要优雅地送走浏览器（发一个 close 帧、等客户端确认），需要 hub 主动遍历连接去关，本域刻意没做——通知是可降级功能，一次重连的代价小于维护一套关闭协商的代价。写在这里是因为下一个人可能会去查「为什么 `ShutdownTimeout` 对 WebSocket 没效果」。

## 5. 兼容契约

权威描述在 [`compat/phorge/README.md`](../../compat/phorge/README.md) 第五节，这里是概述。四条约束加一处已知偏离，共同特征仍然是**破坏后不报错**，而本域比 render/diff 更彻底：PHP 侧不读本服务的响应体，`tryToPostMessage()` 还把异常整个吞掉。其中 5.4 更进一步——破坏之后**连异常都没有**，因为那个 POST 成功了。

按「破坏之后还有谁能发现」排序：

| 约束 | 谁会发现 |
|---|---|
| 5.1 双端口不可合并 | PHP 侧存配置时就抛异常，当场可见 |
| 5.2 client 口 501 | `testClient()` 抛异常，集群面板报 Connection Error |
| 5.3 admin 不套信封 | 没人报错；面板的 Uptime/Clients/Messages 列变空白或 0 |
| 5.4 不能用 binder | **没有任何一处发现** |

### 5.1 两个端口不可合并

`PhabricatorNotificationServersConfigType::validateStoredValue()` 要求 `notification.servers` 里同时有 `admin` 与 `client` 两类，且 `"{$host}:{$port}"` 不得重复。合并成一个端口的话，PHP 侧根本存不下这份配置——这一条反而是四条里唯一会**当场报错**的。它的推论就是本文开头那张地址不对称表。

### 5.2 client 口的 `GET /` 必须回 501

`PhabricatorNotificationServerRef::testClient()` 把 501 当健康信号，拿到 200 抛 `Got HTTP 200, but expected HTTP 501`。这与平台层无条件注册的 `GET / → 200` 正面冲突，`httpx.Config.SkipRootProbe` 就是为这一条存在的，整个仓库只有这一个端口设它。响应体也是逐字节的 `HTTP/501 Use Websockets\n`（Aphlict 的 `AphlictClientServer.js` 原文，末尾换行在内）。

### 5.3 admin 的成功响应不套信封

`POST /` 回裸 `{"fingerprint":"..."}`，`GET /status/` 回带点号键的扁平 map。PHP 用 `phutil_json_decode($body)` 之后直接 `idx($details, 'clients.active')`，套上 `{data,error}` 会让每个字段都取不到而**不产生任何错误**。这两个 handler 因此用 `c.JSON()`。键里的点是字面量，不是嵌套约定；`contracts/notification.go` 的 json tag 就是这些带点的字面串。

`history.age` 在 history 为空时必须是 `null` 而不是 0，所以 `AphlictStatus.HistoryAge` 是指针。

**错误路径仍然走信封，这不是不一致**：PHP 用 `resolvex()`，非 2xx 直接抛异常且从不解析响应体。反过来也成立——**别指望用响应体给 PHP 侧传递失败原因**，那个字段没有读者。

### 5.4 admin handler 必须按原始 JSON 解码

Phorge 用 `HTTPSFuture` 发裸 JSON，Content-Type 是 curl 的默认 `application/x-www-form-urlencoded`。`c.Bind()` 对它**不报错**，而是照字面做表单解析，把整段 JSON 揉成一个垃圾键，然后照常答 200。**这是本节四条里唯一一条破坏之后连错误状态码都不产生的**，机制与 415 为什么不在这条路上见第 3.1 节。

### 5.5 已知偏离

**admin 口的 `GET /` 回 200 而 Aphlict 回 405。** `POST /` 与 `GET /` 方法不同、可以共存，所以平台层的根探针在这个端口上留着了。PHP 侧只打 `/status/` 与 `POST /`，观察不到这个差别，登记在 [`../findings.md`](../findings.md) 第 11 条。**别把这个处理方式套到 client 口上**——那边的 `GET /` 必须是 501。

**第二处偏离方向相反，没有单独登记**：Aphlict 的 client server 对**任何**非升级 HTTP 请求都写 501（`_onrequest` 不看方法），而本域的 501 只挂在 `GET` 上，`POST /` 会得到 405 错误信封（第 2 节）。Phorge 只用 `GET` 探这个端口，浏览器也只发升级请求，所以真实流量里到不了；提一句是因为拿 curl 试探时容易撞见，而它看起来像「端口坏了」。

## 6. 域级错误码

**本域没有域级错误码，而且比 diff 域「没有」得更彻底**：diff 域是没有可报告的失败模式，本域是连成功响应都不在信封里（第 5.3 节）。真造一个码出来，Phorge 也没有地方读它——`tryToPostMessage()` 只看 HTTP 状态码，`loadServerStatus()` 非 2xx 直接抛。

实际会返回的都是平台码，五个，全部走标准信封：

| 码 | 状态 | 什么时候 |
|---|---|---|
| `ERR_BAD_REQUEST` | 400 | admin 的 body 解不开（空 body 的 `io.EOF` 也在内） |
| `ERR_NOT_FOUND` | 404 | admin 口路径不匹配（`/status` 少了尾斜杠就是这个） |
| `ERR_METHOD_NOT_ALLOWED` | 405 | 路径存在但方法不对，**两个端口都真的会返回它** |
| `ERR_TOO_LARGE` | 413 | 请求体超过平台层的 `2M`（第 4.6 节） |
| `ERR_INTERNAL` | 500 | panic 被 `Recover` 兜住 |

405 那一行与 render/diff 相反，原因在第 2 节：那两个域的鉴权分组匹配了所有方法，把 405 吃成了 404；本域没有分组。

真需要新增时加在 `go/internal/notification/` 自己的包里，**不要塞进 `platform/httpx`**。

## 7. 迁入改变了什么

迁入前本域是一个独立仓库的独立 Go module，`net/http` + `ServeMux` 自己搭 HTTP。下面这张表是给「读过那份旧技术报告」的人看的，也是这份文档取代它的理由。

| | 迁入前 | 现在 |
|---|---|---|
| 包布局 | `internal/config/`、`internal/hub/`、`internal/peer/`、`cmd/server/` | `internal/notification/{admin,client,config}.go` + `hub/` + `peer/`、`cmd/gorge-notification/` |
| HTTP | `net/http` + `http.ServeMux`，路径靠 `r.URL.Path` 手工分派 | Fiber v3 路由，`httpx.New` 统一引导（中间件栈、错误处理器、探针） |
| 启动 | 每个 spec 一个 goroutine 跑 `http.Serve`，`sync.WaitGroup` 等 | 每个 spec 一个 `httpx.Server`，`httpx.RunAll` 统一跑与关（第 2.3 节） |
| 探针 | handler 里手写 `/healthz` | `health.Register` 注册 `/healthz` `/readyz`（client 口另设 `SkipRootProbe`） |
| 错误响应 | `http.Error(w, "bad request", 400)` 裸文本 | `{data,error}` 信封 + 平台码，**成功响应刻意豁免**（第 5.3 节） |
| 状态响应类型 | `hub.Status()` 返回 `map[string]any` | 返回 `*contracts.AphlictStatus`，契约层持有形状 |
| 请求体上限 | handler 里 `http.MaxBytesReader` 卡 1 MiB → 400 | 平台层 `BodyLimit("2M")` → 413（第 4.6 节） |
| 配置校验 | 没有；配歪了就少监听一个端口 | `Load()` 强制两类 server 都在，缺则启动失败（第 4.4 节） |
| Aphlict 的 `ssl.*`/`logs`/`pidfile` | 解析进结构体但不用 | 根本不解析（第 4.5 节） |
| 日志 | `log.Printf` | `log/slog`，与进程其余日志汇合 |
| `Listener.instance` | 有这个字段 | 去掉了，实例名只由 Hub 与调用方持有（第 3.7 节） |
| 501 的位置 | handler 第一行 `websocket.IsWebSocketUpgrade` 判断 | 同样；通过后交给 contrib upgrader（第 3.3 节） |
| 契约与测试 | 四组 `_test.go` | 加上 11 份跨语言契约固件、一份 e2e 脚本、一份 OpenAPI |

**没变的是设计判断本身**：一个 hub 接住两个端口、fingerprint 网格防环、history 双上限、每连接一条 goroutine、`writeMu` 与 `mu` 分开、`CheckOrigin` 放行。这些在旧报告里的推理仍然成立，本文第 3 节是把它们对着当前代码重新讲了一遍。

旧报告里另外几处不要照抄：它说指纹空间是 5.6×10²⁷（实际 55¹⁶ ≈ 7×10²⁷）、说 `version: 8` 与 Aphlict 一致（`phorge-fork` 里那份报 7，见第 3.6 节）、并且列举了一批当时同仓库的兄弟服务（`gorge-db-api` 之类），本仓库当前实际有哪些域与二进制见 [`../README.md`](../README.md) 的模块表。

## 8. 排查时的三处反直觉

- **client 口的 501 是正常的。** 用 curl 打它拿到 501 说明它是健康的；拿到 200 说明有人把 `SkipRootProbe` 摘了，而 Phorge 会因此报连接错误。
- **admin 口不鉴权。** 它没有 token、没有 origin 检查，任何能连上它的东西都能给任意用户推任意通知。所以生产部署里它**不应该映射到宿主**——`deploy/compose/docker-compose.yml` 现在把两个端口都映射出来是本地开发的便利，`deploy/compose/.env.example` 里 `GORGE_NOTIFICATION_ADMIN_PORT` 的注释写明了这一点并要求真实部署删掉那条映射。
- **服务端全绿不代表通知能用。** 服务健康、集群面板双绿、`/status/` 计数正常，与「浏览器收到了通知」之间还隔着两件服务端观察不到的事：client 地址是否浏览器可达，以及消息内容有没有被揉碎（第 3.1 节）。真正的验收是两个浏览器窗口登录不同账号互相触发通知。

对着跑起来的实例做最短验证的命令在 [`compat/phorge/README.md`](../../compat/phorge/README.md) 第 5.5 节末尾，`tests/e2e/notification.sh` 跑的就是那几条。

## 9. 测试

四层与仓库其余部分一致（[`../testing.md`](../testing.md)），本域的分工是这样落的：

### 9.1 哪一层守什么

| 层 | 本域的内容 | 只有它能覆盖的东西 |
|---|---|---|
| 单元测试 | `admin_test.go` 10 个、`client_test.go` 14 个、`config_test.go` 12 个、`hub/hub_test.go` 10 个、`peer/peer_test.go` 9 个 | 用 `httptest.NewServer` 起真 listener 之后的真 WebSocket 会话 |
| 分层测试 | `layering_test.go` 的 `forbiddenPrefixes` 里那行 `internal/notification` | 平台层不反向依赖本域 |
| 契约固件 | `admin/` 7 份 + `client/` 4 份 | 跨语言：将来的 PHP runner 读同一批文件 |
| e2e | `tests/e2e/notification.sh` 5 条场景 | **101 握手**——`httptest.NewRecorder` 不实现 `http.Hijacker`，升级在内存里完不成 |

分层测试那一行是迁入时**手工**补的。这是全仓库唯一需要人工维护的测试，漏补的话它对新域静默失效——照样通过，只是不再检查任何新东西（[`../findings.md`](../findings.md) 第 8 条）。

固件为什么分两个目录、每份各钉住什么，见 [`tests/contract/notification/README.md`](../../tests/contract/notification/README.md)。这批固件让共享 runner 长了两处（整段路径先当字面量键查一次；只断言状态码与原始字节的固件跳过 JSON 解码），细节在 [`../testing.md`](../testing.md) 第 2.3 节。

WebSocket 测试的两处手法值得抄：`syncCommands` 发一次 ping 并等 pong，用「命令按序处理」这个事实把 `subscribe` 已生效这件事变成确定的，而不是拿 sleep 去赛跑；`expectFirstMessage` 把「不该收到的那条」先发出去，于是过滤失效时读到的是错的那条——比等一个超时来证明「没收到」既更强也更稳，况且底层连接在读超时之后就不可用了，测试也没法接着往下走。

### 9.2 覆盖率

覆盖率与代码行数不在模块文档里维护快照；当前结果用 `make cover` 生成，原因见 [`../testing.md`](../testing.md) 第 5 节。

`hub` 的包内结果有一部分是**度量假象**：`Listener` 的 `WriteJSON`/`ReadMessage`/`Close` 等方法要一条真 WebSocket 才调得到，而这些连接建在 `internal/notification` 的测试里，`go test` 默认只把一个包自己的测试计入该包覆盖率。观察整条调用链时应使用 `-coverpkg`。覆盖率的真实缺口登记在 [`../findings.md`](../findings.md) 第 9 条。

### 9.3 三处断言的 teeth 在哪，别改坏

这几处的共同点是：**看起来可以简化，简化之后测试照样通过，但守卫无声消失。**

- **`tests/contract/notification/admin/post-form-content-type.json` 的 payload 里那个 `%`。** 断言只有「200 + 有 fingerprint + 没有信封」，而这三条在消息被揉碎的情况下**全部成立**（第 3.1 节那张表的第二行）。真正挡住 `c.Bind()` 的是 `build 100% done` 这段字节——它对表单解析器是个非法转义，于是请求在进 handler 之前就被拒成 400。把那个百分号「清理」掉，这份固件就退化成一个永远通过的检查。
- **`TestContentTypeIsIgnored` 挡得住，但挡住它的是 `history[0]["type"]` 那一行。** 它跑 `application/json`、form-urlencoded、空头三种。换成 `c.Bind()` 之后空头那个子测试撞 415，而镜像 Phorge 的 form-urlencoded 那个撞内容断言（`type` 变成 nil）——上一条固件看不见的「被揉碎」那条路，是这里挡住的。它自己的注释写明了理由（"inspected rather than counted"）：被揉碎的 body 一样会在 history 里留下一条，只有内容分得出两者。**别把它改回只数条数**，那样就只剩空头子测试的 415 会红，而空头是 Phorge 不会发的形状。它覆盖不到的是「被拒成 400」那条路（payload 里没有百分号），而且它只验到 hub。
- **`TestMessagePostedToAdminReachesASubscribedBrowser` 是最强的那一处。** 它的 POST 走 `postAsPhorge`（贴 Phorge 真发的那个头，不是 `postTo` 写死的 `application/json`，而 `c.Bind` 处理 JSON 是正确的、拿它测等于不设防），payload 带 `build 100% done`，消息从真 WebSocket 上**逐字段读回来**。于是 binder 坏掉这条约束的**两条**路都落在它手上：非法转义被直接拒成 400，或者答 200 但字段被揉碎。上面那两处各只覆盖其中一条。把 `postAsPhorge` 换回 `postTo`、把百分号去掉、或者把字段断言简化成 `expectFirstMessage` 那样只看第一条是谁——任何一步都会让它继续通过。
