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

