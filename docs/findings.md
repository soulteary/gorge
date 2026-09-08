# 偏差与改进建议

代码与文档比对时发现的实际问题。按模块分节，新模块迁入后在下面新开一节，不要混进别的模块。内容跟随当前 `main`；已修项按下面的规则删除。

修掉一条就把它从这里删掉，别标记成「已完成」留着——这个文件的价值在于短。**编号是稳定的 ID，不重排**：别处按号引用它们，所以删掉一条会留下一个空号（#16 已修，就是这么来的），新增一条一律接在最大号之后，即使它归到中间某一节。

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

diff.RegisterRoutes(srv.App(), &diff.Deps{
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

（上面那段代码是 diff 迁入时的样子；列表此后每次迁入都手工长一行，到 webhook 为止已经是**第六次**手工维护，而六次里没有一次是被这个测试提醒的——它对漏掉的新域照样通过。）

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

## search 模块

### 17. Meilisearch 后端的测试补上了，但补测试并没有发现它的第一个真 bug（**已改，登记教训**）

**影响**：中→低。覆盖率已从 `[no test files]` 到 91.5%，但这条留下来，因为它记的是一件比覆盖率更有用的事。

原始条目说这个包零覆盖、说 Elasticsearch 的 `backend_test.go` 是可以照抄的模板。测试后来照着补了，覆盖率也上去了，**然后第一次把服务对着真 Meilisearch 跑，第一个查询就 400 了**：

```
Attribute `id` is not filterable.
```

`buildFilters()` 把 `SearchQuery.Exclude` 渲染成 `id != {phid}`，而 `filterableAttributes()` 只声明了 `docType` 与那批 relationship。Meilisearch 不给主键破例，所以**每一个带 `exclude` 的查询都失败**，而不是少排除一条。

值得记的是**两条测试为什么都是绿的**：

- `TestBuildFiltersExclude` 只断言渲染出的字符串是 `id != PHID-TASK-9`——它是对的，问题不在这一侧。
- `TestIndexIsSane` 拿 `filterableAttributes()` 同时当实际值和期望值，所以它比对的是那个函数**和它自己**。

也就是说两侧都来自同一份误解，于是它们一起错、一起通过。这与第 42 条 `MySQLStore` 的形状完全相同，只是那里的边界是 SQL 驱动、这里是 Meilisearch 的过滤器声明规则。

修法是给 `filterableAttributes()` 加上 `id`。**新增的测试刻意不是"期望列表里应该有 id"**——那还是在重述——而是 `TestEveryFilterableAttributeIsDeclared`：把一个触达每条过滤分支的查询交给 `buildFilters()`，取出每个表达式的属性名，断言它都在 `filterableAttributes()` 里。它把两个函数绑在了一起，所以抓的是这一类而不是这一个。它也自带一条空转防护（渲染出零个过滤器就 fail），否则一个什么都不产出的 `buildFilters` 会让它假通过。

**教训**：一个后端的"HTTP 面更小、照抄一份不需要新设施"是真的，但照抄出来的 fake 只能证明本域**以为**对端会怎么答。这个包 91.5% 的覆盖率是在那次 400 之前就达到的。

### 18. CJK mapping 变更强制全量重建索引（**已改，登记代价**）

**影响**：高，但是一次性的，且**会明确报错**。

这条不是待办，是一次**迁移代价的记录**。迁入时给 `buildIndexConfig()` 加了 `cjk_text` 分析器与三个 `cjk` 子字段，而 `IndexIsSane()` 是拿 `configDeepMatch(actual, buildIndexConfig(docTypes))` 比对线上索引的，所以**所有既有索引立刻报 not sane**，必须：

```
bin/search init
bin/search index --all --force
```

大库上第二条是小时级操作，且期间检索结果不完整。

**它与 search 域其余的兼容约束性质相反：这一条明确报 false，不静默失效**（见 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第 7.5 条）。`indexIsSane()` 的存在正是为了让这类改动有一个可报告的信号，所以这是设计按预期工作，不是缺陷。

留在这里的理由只有一个：**它会再发生。**任何一次动 `buildIndexConfig()` 的改动——加一条分析器、调一个 filter 的顺序、给某个字段换类型——都有同样的后果，而这一点在代码里只有一句函数注释提到。写进 `DOCKER.md` 与 [`modules/search.md`](modules/search.md) 第 3.3 节了；这一条是给下一个改 mapping 的人留的备份。

顺带记一件容易照抄错的事：`bin/search ngrams` 在这个引擎下**不适用**，它是 Ferret（MySQL）专属路径。旧 `phorge/DOCKER.md` 里那套「跑 ngrams 启用中文搜索」的说法在 gorge 引擎下是误导。

### 19. `GORGE_SEARCH_BACKENDS` 解析失败只有一条日志

**影响**：中。是个运维陷阱，形状与第 15 条完全相同。

JSON 写错了落在与「还没配」同一个地方——零个后端、`/readyz` 答 503——外加一条 `slog.Error`。所以它至少不再伪装成健康（`/healthz` 200 而 `/readyz` 503，compose 会报 unhealthy），但**「配错了」与「还没配」在退出码与探针上无法区分**，只有日志能分开它们，而这两件事的处置完全不同。

这个域里的后果比 mailer 那边重一档：一封发不出去的信有人在一天内会注意到，而一个不能检索的 Phorge 只是「搜不到东西」——那是搜索功能的一个正常输出。

**建议**：与第 15 条一起做，用同一个机制（比如统一的 `GORGE_STRICT_CONFIG=1`）而不是两个域各造一个开关。没有直接改成硬失败的理由也与第 15 条相同：「先起服务、再补配置」是这个域受支持的工作流，`/readyz` 已经把它表达清楚了。

### 20. `/api/search/sane` 现在拒绝空 `docTypes`（**已改，登记原因**）

**影响**：中，是一次行为收紧的记录。

迁入前空的 `docTypes` 会被接受。它在 `/init` 上的后果是建出一个没有任何文档类型 mapping 的索引；在 `/sane` 上的后果更坏，而且方向相反：sanity check 拿「本服务今天会为**这批类型**建出的配置」去比对线上索引，**空类型列表建出的是一份空期望，任何索引都满足它**——包括一份没有 mapping、没有 `cjk` 子字段的索引。答案会是一个自信的 `sane: true`。

所以两条路径现在都答 400 `ERR_BAD_REQUEST`。这是本域唯一一处刻意偏离迁入前行为的地方，登记而不是只写在注释里，因为它是**放宽方向上的一次单向门**：将来谁为了兼容某个老调用方把它改回接受空列表，坏掉的不是这个端点，是「索引配置有没有过期」这个问题从此永远答 true。

`tests/contract/search/` 与 `tests/e2e/search.sh` 第 18 条各有一份成对断言（`/init` 与 `/sane` 各一条），别只留一条。

### 21. 五个域级错误码在 Phorge 侧没有消费者

**影响**：低。这条**记录事实，不提议改 Go 侧**——当前行为是用户明确决定保留的。

`ERR_INDEX_FAILED` / `ERR_SEARCH_FAILED` / `ERR_INIT_FAILED` / `ERR_CHECK_FAILED` / `ERR_STATS_FAILED` 五个码在 Go 侧分得很细，理由是充分的（见 [`modules/search.md`](modules/search.md) 第 6 节）。但 PHP 侧接不住这个区分：`PhabricatorGorgeSearchClient` **没有覆盖** `newServiceErrorException()`，而 `PhabricatorGorgeMailerClient` 覆盖了——它必须覆盖，因为 `ERR_PERMANENT_FAILURE` 决定 worker 要不要重投（`compat/phorge/README.md` 第 6.2 条）。于是走 search 客户端的**十一个码**（六个平台码加五个域级码）全部塌成同一个通用异常，码本身只作为文本活在异常消息里。

具体后果：Phorge 侧无法按码分支，`ERR_CHECK_FAILED`（问不到）与一个正常的 `sane: false`（该重建了）在 PHP 代码里的区别只剩「一个抛异常、一个返回 false」，而**这个区别恰好是够用的**——这也是保留现状的理由。

**不要据此收敛 Go 侧的五个码。**它们的价值在另外两个消费方上，都真实存在：`bin/search` 的输出，以及运维读 502 响应体时看到的那个字符串。收敛成一个码，那两处拿到的就只有「搜索服务返回了 502」。

要记的只有一件事：**别在 Go 侧新增一个「PHP 必须按码分支」的搜索域错误码而不同时覆盖 `newServiceErrorException()`。**那种码写出来会看起来生效，实际上没有读者——形状与 `compat/phorge/README.md` 第 5.3 节末尾那条「不要指望用响应体给 PHP 侧传递失败原因」相同。

### 46. Elasticsearch 后端只在 ES 5 上能建索引，而文档声称支持 7/8（**已改，登记原因**）

**影响**：高。配了 ES 7/8 的部署检索完全不可用，且不是退化而是从建索引就失败。

`buildIndexConfig()` 原本无条件按文档类型给 mapping 分 key（`mappings[docType]`），那是 Phorge 自带引擎的布局，也是 ES 5 及更早的模型。但 **ES 6 把一个索引收紧到只能有一个 mapping type，ES 7 起把 type 整个移除了**，而本域要索引七种类型。于是对着 ES 7 集群调 `POST /api/search/init` 直接拿到：

```
mapper_parsing_exception: Root mapping definition has unsupported parameters: [PSTE : ...] [TASK : ...] ...
```

代码里唯一的版本分支是 `version >= 5`，它只切文本字段类型（`text`/`string`）与 relationship 的写法，**从来没有处理过 type 被移除这件事**。而 `.env.example` 与 `docker-compose.gorge.yml` 的注释都写着「ES 7 / 8 都走 >= 5 的那条分支，填准确的值只是为了日志好读」——那句话是错的，`version` 恰恰是本文件里最不能填错的一项。

顺着这条查出的另外两处同源缺陷，都属于**旧写法在新集群上不是被忽略而是被拒绝**：

- **`include_in_all`**：`_all` 在 6.0 随之移除，mapping 里再提它就会被拒。原先无条件写出。
- **`not` 查询**：`exclude` 用的是 `{"not": {"ids": ...}}`，而 `not` 在 2.0 弃用、**5.0 移除**。也就是说它在本后端支持的每一个版本上都只换回一个解析错误，**从来没有真的排除过任何东西**——包括默认的 ES 5。这一处没有任何测试覆盖，而 `esquery` 里 `AddMustNot` 一直存在、一直没被用上。

三处的修法与形状见 [`modules/search.md`](modules/search.md) 第 3.6 节。`must_not` 从 1.x 起语义未变，所以那一处不需要版本分支，对每个版本都是修复。

**登记的代价与第 18 条同源**：`version >= 6` 的 mapping 形状与 `< 6` 不同，所以从 ES 5 迁到 6/7 不是改一个数字，**必须重建索引**。`IndexIsSane()` 会明确报 false，所以这件事是响的，不是静默的。

**这条也是"真后端才验得到"的又一例**，与第 17 条成对：ES 后端 83.5% 的覆盖率、`httptest` 假集群里断言的路径与 spec 形状，全都通过了——因为假集群对什么请求都答 200，而 `mapper_parsing_exception` 只有真 ES 会说。`tests/e2e/search.sh` 第 5 条是唯一能问出这个问题的地方，而它要一个真集群才有意义。现在 `deploy/compose/demo/` 提供了一个（ES 7.17 + Meilisearch 同时挂上），18 条场景在两个后端上各自全过。

---

## file-storage 模块

### 22. blob 后端的启用开关从「默认开」改成「必须显式给 host」（**已改，登记原因**）

**影响**：高，但是正向的。这条不是待办，是一次**行为变更的记录**——它改变的是「什么都没配」这个状态下服务的行为。

独立服务时期 `MYSQL_HOST` 默认 `127.0.0.1`、`MYSQL_BLOB_MAX_SIZE` 默认 `1000000`，而启用判据只看后者。两个默认值凑在一起的后果是：一个**只配了本地磁盘**的部署，照样注册一个指向根本不存在的数据库的 blob 后端。而这个后端优先级是 1：

- 它接走每一个 1 MB 以内的上传——也就是绝大多数上传——然后失败；
- `/readyz` 会 ping 它，于是**整个服务**被报成不可用，尽管本地磁盘好端端地配着；
- 运维看到的是「我明明配了本地磁盘，服务却说自己没就绪」，而配置文件里没有任何一处提到 MySQL。

现在 `MySQLBlobEnabled()` 要求 `MySQLHost != "" && MySQLBlobMaxSize > 0`。加上 host 这一条之后，「什么都没配」与「配了 blob」才成为两个可区分的状态——这正是 mailer 的 `MAILER_TYPE` 在那个域里做的事：**一个后端的存在必须是被声明出来的，不能是被默认值凑出来的。**

`TestNoBackendIsConfiguredByDefault` 钉住它：清空全部环境变量之后，三个 `*Enabled()` 必须全是 false。**它守的是「零配置等于零后端」这个不变量，不是那几个默认值本身**——`MYSQL_BLOB_MAX_SIZE` 的默认值 `1000000` 至今没变，也不该因为这条改动而变。

### 23. DELETE 改成幂等（**已改，登记原因**）

**影响**：中，正向。同样是行为变更记录。

独立服务时期的 mysqlblob 引擎在 `RowsAffected() == 0` 时返回错误——一个看起来很合理的「你删的东西不存在」。但它和调用方的实际用法冲突：**Phorge 是「删掉字节」和「删掉那条指向字节的记录」一气呵成的**。对已经消失的字节报错，会让它删不掉那条记录，于是数据库里留下一行永远退不掉、指向虚空的记录，而下一次 GC 会再试一次、再失败一次。

现在三个引擎一致地把「对象不在」当成功：本地磁盘忽略 `os.IsNotExist`，blob 引擎不看 `RowsAffected`，S3 本来就是这个语义。接口注释里写明了这是**接口的要求**而不是各引擎的巧合。

代价要说清楚：**调用方无法再区分「删掉了」与「本来就不在」**。这是有意放弃的信息——没有任何一个调用方会因为这个区别做不同的事，而它换来的是 GC 能推进。`TestDeleteBlobIsIdempotent` 与 `TestLocalDiskDeleteIsIdempotent` 各守一层，契约固件 `delete-blob.json` 也是照着一个没人 seed 过的 handle 写的。

### 24. `platform/` 没有数据库设施，并且**刻意不加**

**影响**：低（现在），但它是一个会被下一个人误判的结构决定，所以登记。

`go-sql-driver/mysql` 由 `internal/filestorage/db.go` 自己 import，连接池的三个参数（`maxOpenConns=25` / `maxIdleConns=5` / `connMaxLifetime=5m`）也定在那里。看到「仓库里第一次出现数据库」而顺手在 `platform/` 下开一个 `db` 包，是很自然的动作，**但现在做是错的**：一个域需要连接池不构成共享关切，抽出去只会得到一个只有一个调用方的包，而它的默认值必须替一个不存在的第二方猜测。

那三个参数是**按本域的用法定的**，不是通用值：这个服务只在一次 INSERT 或一次 SELECT 的时间里持有连接，行大小还被 `MySQLBlobMaxSize` 封着；而它跟 Phorge 共用同一个数据库实例，真正需要留出余量的是 Phorge 自己的连接池。换一个域来，这三个数字大概率都不合适。

**什么时候重新考虑**：第二个域需要连接池的时候，而且判据是那时两个域的 DSN 拼法与池参数是否真的能共用——不是「都用了 database/sql」。在那之前，`platform/` 保持没有数据库设施这件事本身就是文档：它说明「域包可以持有自己的外部依赖」。

### 25. S3 写路径必须声明 payload 未签名（迁入时发现的真实 bug）

**影响**：高。修之前，自建对象存储上的每一次上传都是失败的。

把 S3 的写路径从「缓冲整个文件再上传」改成「流式上传」之后，明文 HTTP 端点上的 `PutObject` **直接失败**，报 `request stream is not seekable`。原因不在本仓库：SigV4 默认要用 payload 的 SHA256 参与签名，而 SDK 算这个 hash 的办法是把 body 读一遍再 seek 回开头——**HTTP 请求体不能 seek**。

关键是这条路只在**明文 HTTP** 上走得到：SDK 在 HTTPS 上本来就改用「声明 payload 未签名」（传输层已经保护了完整性）。而明文 HTTP 端点恰恰是自建 MinIO / Ceph 的常态，也就是这个后端的主要使用场景。`s3.go` 的 `unsignedPayload` 把 SDK 在 HTTPS 上的那个选择延伸到明文 HTTP，且**只加在 `PutObject` 上**——这里只有它带 body。

另一条路是把每一次上传缓冲下来算 hash，但那正是改成流式要消掉的东西：16M 的传输上限乘上并发数就是这个进程的内存底线。

`TestS3RoundTrip` 钉住它，靠的是**用一个不可 seek 的 reader**（`oneByteReader` 包着 `strings.Reader`）而不是 `strings.Reader` 本身——后者是可以 seek 的，SDK 会走另一条路，测试照样通过而防线消失。**改这个测试时别把那层包装「简化」掉。**

### 26. 写入回退的范围由 rewind 预算限定，而它不覆盖一整类失败

**影响**：中。不是 bug，是一个必须被知道的边界。

`Router.Write` 在某个引擎失败时会换下一个试，这是迁入时补上的能力（它要接住的是 `bin/storage upgrade` 跑之前 `file_storageblob` 不存在的那段时间）。但**字节只存在一次**：`rewindReader` 只记录预算以内的字节，超出之后把记录整个丢弃并拒绝重放——留一个前缀会让下一个引擎存下一个截断的文件，那比失败坏得多。

预算是**推导出来的**：各引擎里最大的那个大小限额，实践中就是 blob 引擎的 1 MB。它**不是可调参数**，也不该做成可调参数——它的含义是「那个不肯流式的引擎最多会读多少」，而不是「我们愿意为重试花多少内存」。`TestRouterBudgetFollowsTheSizeLimitedEngines` 断言的正是这条推导关系。

**它不覆盖的那一类同样真实**：本地磁盘或 S3 写到一半失败，任何预算都救不回来——它们是流式的，字节早已流走。这一类目前是可以接受的，因为这两个引擎后面本来也没有第三个可以接；但**如果将来在 S3 之后再加一个后端，这条就会开始咬人**，而它的表现是「失败」而不是「静默错误」，这一点值得庆幸。`TestRouterStopsWhenTheFailedWriteConsumedTheBody` 把这个边界钉死，它断言的不只是整体失败，还有「下一个引擎一个字节都没收到」。

### 27. 被 recover 的 panic 会藏在一片绿色的状态码断言背后

**影响**：高。这是一条**测试方法学**的发现，不限于本域——七个域里现在只有两个有这道防线（file-storage 与 webhook），另外五个都没有。

迁入过程中真实发生过一次：一个 handler 拿着 nil 引擎调了 `ReadFile`，panic 了，而**每一条状态码断言都是绿的**。链条是这样的：

1. 一个 helper「顺手」把 `httpx.Fail(...)` 的返回值当成「出错了没有」往上传——但 `Fail` 在响应写成功时返回 **nil**，也就是说「答了一个 400」这件事在调用点读起来是「没问题」；
2. handler 于是继续往下走，拿着 nil 引擎 panic；
3. 平台层的 `Recover` 中间件把 panic 变成 500；
4. `errorHandler` 看到响应**已经 committed**（第 1 步那个 400 已经写出去了），于是不再落笔；
5. 于是测试看到的还是那个 400，断言通过。

**唯一的证据是日志里一行没人看的堆栈。**所以 `router_test.go` 的 `quietLogs` 现在做两件事：把有意为之的失败路径日志静音，以及在 `t.Cleanup` 里检查捕获到的日志——**含有 `PANIC_RECOVERED` 就让这个测试失败**。契约固件的 runner（`contract_test.go`）也调它，因为一份只断言状态码的固件比单元测试更没有分辨能力。

顺带记下 `resolveTarget` 现在的形状是这条发现的产物：它**返回一个拒绝原因的字符串而不是自己应答**，这样调用点没有「把 nil 当成没问题」的机会。函数注释里写明了理由。

**建议**：render / diff / notification / mailer / search 五个域各自的测试 helper 都加同一道检查。成本是十行，而它挡的是一整类「测试全绿、handler 在崩」的情况。

webhook 迁入时把这道检查照抄了过去（`memstore_test.go` 的 `quietLogs`），所以这条建议已经被兑现了一次，而那一次值得记：**在那个域里它比在这里更要紧，因为那个域的多数代码跑在投递 goroutine 里。**一个在 goroutine 里被 recover 掉的 panic 连「一个请求答错了码」这种痕迹都不会留——测试主体早已返回，唯一的证据就是那行没人看的堆栈。所以这道检查该跟着「有后台 goroutine」这个特征走，而不只是跟着「有 handler」走。

### 28. 仓库里现在有两套 AWS 签名实现

**影响**：低，但它会把下一个想「统一一下」的人引向错误的方向。

`aws-sdk-go-v2` 随 file-storage 首次进入单仓（`aws-sdk-go-v2` / `credentials` / `service/s3`，连同十几行传递依赖），`s3.go` 用它签请求。而 `internal/mailer/ses.go` 里躺着约四十行手写 SigV4——`deriveSigningKey` 那条 `AWS4` → date → region → service 的派生链，以及 `AWS4-HMAC-SHA256` 头的拼装。**同一个仓库、同一个签名算法、两份实现。**

看起来该二选一，但两个方向现在都是错的：

- 让 mailer 改用 SDK，是为一次 `POST` 表单请求引入 `service/sesv2`。手写签名换来的是端点可配、可以用 `httptest` 打完整的一圈——第 14 条里 SES 是七个适配器中唯一被测全的那个，靠的正是这一点。
- 让 file-storage 改成手写，则要自己实现 path-style 端点、重试、以及 `UNSIGNED-PAYLOAD` 那条（第 25 条）——这些恰好是 SDK 已经做对的部分。

**登记而不修**，同时把判据留下：值得抽出去的时机是**第三个域也需要 AWS 签名**，而那时该抽的是「签一个请求」这件事本身，不是「都换成 SDK」。这条与第 24 条是同一个形状——两个调用方不构成共享关切——只是那边说的是连接池。

### 29. 删除路径上的非法 handle 曾经答 500（**已修**）

**影响**：中，正向。这条既是行为变更记录，也是第 23 条的直接副产品。

第 23 条把 DELETE 改成幂等之后，`deleteBlob` 手里就只剩一种失败可以往上抛了，而**它把两件性质完全不同的事抛成了同一个 500**：

- 后端真的没删掉（磁盘只读、数据库连不上）——字节可能还在，Phorge 绝不能退掉指向它的记录。500 在这里是诚实的。
- 调用方传了一个任何引擎都不可能签发的 handle——**服务什么问题都没有**，可它答的是「服务内部错误」。

第二种在 500 里意味着：日志里多一条 `REQUEST_FAILED`，运维按 500 的排查路径去翻服务日志，而真正该看的是调用方传了什么。Phorge 自己不会传出这种 handle，所以触发它的现实场景是**Phorge 数据库里那条记录本身坏了**——这恰恰是最需要错误信息指对方向的时候。

修法是把「格式非法」做成一个可辨认的错误而不是一句文案：引擎的 handle 校验统一 wrap `ErrBadHandle`（`engine.go`），`deleteBlob` 用 `errors.Is` 认出来答 400 `ERR_BAD_REQUEST`。**没有新增域级错误码**，收敛进平台码。

**读路径刻意不跟着改**：`readBlob` 对同一个非法 handle 继续答 404，理由见 [`modules/file-storage.md`](modules/file-storage.md) 第 3.4 节——读区分不出也不需要区分。于是同一个非法 handle 在读和删上答两个码，这处不对称是刻意的，`TestReadBlobBadHandleStays404` 与 `TestDeleteBlobBadHandleIs400` 成对钉住它，免得下一个人把其中一个「顺手统一」掉。`TestEnginesReportABadHandle` 则守着引擎那一层真的在报这个哨兵——两个 handler 测试用的是 stub 引擎，绕过了真实校验。

**一般化的那条**：一个 endpoint 越是把失败算作成功，剩下那些真的失败就越需要被分开。幂等是拿「区分能力」换「调用方好写」，而换掉的那部分要在别处补回来。

### 43. `/readyz` 让整个栈在首次启动时永久死锁（**存量 bug，已修在编排侧**）

**影响**：高，而且是那种「新装一次必然踩、踩了不会自愈」的形状。它**不是** webhook 迁入引入的，是 webhook 的端到端验证顺手发现的；也**不是**推断，下面这三行是真实二进制对着真实 MySQL、在一个新数据卷上打出来的：

```
GET /healthz → 200
GET /readyz  → 503
  reason: engine blob: ping database: Error 1049 (42000): Unknown database 'phabricator_file'
```

链条上每一环都是各自正确的决定，合起来才成环：

1. blob 后端在 `phorge-fork` 的编排里**无条件启用**。第 22 条把开关改成「必须显式给 host」，而 `MySQLBlobEnabled()` 的判据是 host 非空且 size > 0——compose 把 host 写成字面量 `mysql`，size 取默认 1MiB，两个条件都满足。
2. `FileDSN()` 里带着**库名** `{namespace}_file`。
3. go-sql-driver 在**握手阶段**就把库名发过去，所以库不存在时 1049 报在连接上，而不是报在某条查询上。ping 因此失败。
4. `/readyz` 恒 503。
5. `phorge` 用 `service_healthy` 等它，于是卡住。
6. 而建那个库的 `bin/storage upgrade` **就在 phorge 容器里**，永远不会跑。

`db-init` 帮不上忙：它那份 `db-grant.sql` 只有一条 GRANT，一个库都不建。

**修法在编排侧**：`phorge-fork/docker-compose.gorge.yml` 里 `phorge` 对 `gorge-file-storage` 的依赖改成 `service_started`，与 `gorge-webhook` 一致（第 41 条）。

**Go 侧的 `/readyz` 语义刻意没有动**，两个理由，第二个比第一个重要：

- [`compat/phorge/README.md`](../compat/phorge/README.md) 第 8.7 节把「不查表存在性」定成约定，改 readiness 会正面踩到它；
- **而且那个 503 是对的。**库建出来之前 blob 后端确实一个字节都写不进去，而那个中间状态早有兜底——写入按 priority 下沉到本地磁盘（第 26 条讲的是这条兜底的边界）。所以 Phorge 根本不需要等本服务就绪：它等的东西从来就不是它需要的东西。**把一个诚实的探针改哑来解开死锁，是拿可观测性换启动顺序**，而这里有一个更便宜的换法。

**这条同时修正了 8.7 里一句既有的推理。**原文写「ping 数据库是安全的，因为数据库服务器是一个独立容器，谁都不依赖」——独立的是**服务器**，DSN 里带的是**库名**。「不查表」这条约定本身没错，只是**不足以**让 ping 脱离那个闭环。8.7 已按实测改写。

**同一句推理逐字适用于 `webhook.HeraldDSN()`**（`{namespace}_herald`，同样由 `bin/storage upgrade` 建）。webhook 躲过去只是因为它的依赖本来就是 `service_started`，**不是因为它的 readiness 有本质区别**——两个探针在这件事上是同一个形状。这也是第 41 条末尾那段「区别在 Phorge 建的是表还是库」需要作废的原因：两个域的**库**都是 Phorge 建的，真正的区别只在依赖那一行的强度。

**一般化的那条**：一个就绪探针的依赖不是「那台服务器」，是**它 DSN 里写下的全部东西**。判断一个探针有没有把自己接回等待者，要逐段读连接串，而不是数它调了几个方法。

---

## webhook 模块

本节前四条是**行为变更的记录**，不是待办：它们描述的三个缺陷在独立服务里是真实存在并且正在发生的，迁入时一并修掉。之后四条是**未修的已知遗留**，再之后是三条结构性判断与两条 PHP 侧的坑。最后两条（第 44、45 条）来自迁入后的端到端验证，性质与前面都不同：**那次验证没有在迁入的代码里找到任何缺陷**——抢占、退避、熔断、HMAC、payload 字节格式对着真实 MySQL 的行为与设计完全一致——这两条讲的是**观测这些机制的能力**，以及一个配置项实际生效的方式和它的文案不符。

这一整节有一个共同的形状，值得先说：**本域没有调用方**。它的工作是一个后台循环，没有任何请求能启动一次投递，也没有任何端点能报告一次投递。所以下面每一条的「谁会发现」这个问题，答案通常都是「没有人，除非有人去读那张表」——比 search 域那些「答 200 但查不到」还要再安静一档，因为那边至少还有一个请求可以观察。

### 30. 消除重复投递，单实例也会发生（**已改，登记原因**）

**影响**：高，但是正向的。这是本次迁入修掉的最要紧的一件事。

独立服务的 `FetchQueuedRequests` 就是 `SELECT ... WHERE status = 'queued'`，没有任何抢占动作。投递期间那一行的 status 仍然是 `queued`——**而它必须仍然是**，见下面第一条设计理由——所以下一个 tick 会再取到同一批行。默认轮询间隔 1 秒、投递超时上限 15 秒，于是**一个单实例部署也会把同一个事件 POST 出去十几次**。这不是「多实例时的竞态」，是每一次比一秒慢的投递都会发生的事。

改成了基于 `dateModified` 的乐观 CAS claim（`UPDATE ... SET dateModified = GREATEST(dateModified + 1, UNIX_TIMESTAMP()) WHERE id = ? AND status = 'queued' AND dateModified = ?`，只有 `RowsAffected() == 1` 算抢到）。三点设计理由都是硬的，改动这套机制之前每一条都要重新过一遍：

- **不能改 `status` 的取值范围。**Phorge 的 UI 按它渲染图标，`HeraldWebhookWorker::doWork()` 的前置检查要求 `status === queued`。在这里发明一个 `claimed` 值会同时弄坏界面和 PHP 的回退路径——这正是抢占必须发生在别的列上的原因，也是「投递中」与「等待中」从查询侧看起来一模一样的原因。
- **候选查询里 `dateModified = dateCreated` 那一半不是冗余。**它让新插入的行不必先等一个它从未进入过的 lease：Lisk 的 `willSaveObject` 在插入时把 `dateCreated` 与 `dateModified` 写成同一秒，而本服务之后的每一次写入都让后者严格变大，所以「两者相等」就等于「没人碰过」。这个前提哪天不成立，代价也只是这个条件不再命中——延迟，不是正确性。
- **`GREATEST(dateModified + 1, UNIX_TIMESTAMP())` 里的 `+ 1` 是必需的。**这一列只有秒级精度，所以同一秒内插入并抢占的那一行会被写回它已经持有的值，于是 `WHERE` 对第二个抢占者**仍然成立**——那正是这条语句要挡的全部失败。

`TestTheSameRequestIsNotDeliveredTwice`、`TestOnlyOneClaimWinsTheSameVersion` 与 `TestAClaimInTheSameSecondStillChangesTheVersion` 分别守这三层。另有 `TestClaimStatementIsAnOptimisticCompareAndSet` 直接断言那条 SQL 的形状——它看起来是在测字符串，实际测的是那三个 WHERE 条件都还在。

### 31. 补上失败重试退避（**已改，登记原因**）

**影响**：高，正向。它和第 30 条是同一个循环上的两个不同缺陷，但后果的方向不同。

独立服务的 `handleFailure` 把 `retry = forever` 的 request 直接写回 `queued`，下一秒立刻重投。于是那个「累积 10 次失败触发 300 秒熔断」的护栏在**十秒**内就烧完了——而在它生效之前，一个已经在失败的接收端已经被打了十次。

新增 `RetryBackoffSec`，默认 **60**，对齐 Phorge：一个抛异常的 worker 由框架重新 lease，等待时长来自 `PhabricatorWorker::getWaitBeforeRetry()` 返回 null 时落到的 `PhabricatorWorkerLeaseQuery::getDefaultWaitBeforeRetry()`。

**要记住的是它与 lease 是两个不同的时长，分别用 `dateModified` 与 `lastRequestEpoch` 判断，刻意不合并成一个条件。**合并之后两个方向都错：一个被崩溃进程遗弃的行要等满 60 秒才有人接，而一个刚失败的 request 会在 lease 到期（30 秒）后就被重投。lease 回答的是「是不是已经有另一次尝试在进行中」，那是一个关于投递超时的问题；退避回答的是「一个坏掉的接收端该被多快重试」。`TestFetchClaimableStatementKeepsItsConditionsSeparate` 与 `TestAFailedRequestWaitsTheRetryBackoff` 成对守着这条区分。

### 32. 补上 `/readyz`（**已改，登记原因**）

**影响**：高，正向。**这个缺口对本域比对任何其他域都严重**，所以它值得单独一条而不是并进第 30、31 条。

独立服务只有 `/` 与 `/healthz` 两个无条件 200，而且 `NewStore` 在 ping 失败时直接 `os.Exit(1)`。两件事凑在一起的后果是：一个连不上库的实例要么根本起不来（于是被编排打进重启循环，尽管它的依赖只是慢了一拍），要么——如果库是在启动之后才不可达的——**在监听、健康检查全绿、投递量为零，而且任何地方都不出现失败**。Phorge 继续入队，那些行就静静躺着，Herald 界面上它们停在蓝色的 Queued 图标上，和「服务慢了一拍」长得一模一样。

为什么这一条在本域比在 mailer / search / file-storage 都重：那三个域的失败都伴随着一个**有人在等的请求**——一封发不出去的信、一次空的检索、一个打不开的附件。本域的失败没有任何请求参与，队列只是不再变短，而「队列里有东西」是这张表的正常状态。

现在 `OpenDB` 不再 ping（`sql.Open` 是惰性的），ping 挂到 `httpx.Config.Ready` 上，容器 healthcheck 打 `/readyz`。**`/readyz` 只 ping 不查表**：`herald_webhookrequest` 由 Phorge 的 `bin/storage upgrade` 建，跑那条命令的容器可能后启动，所以要求表存在等于把一个健康的部署在它第一次迁移期间报成坏的。这与 file-storage 那条同源（第 24 条附近、`modules/file-storage.md` 第 3.5 节），但在这里还多一层：编排侧的依赖方向也因此必须是 `service_started`，见第 41 条。

### 33. 500 响应体不再回显 `err.Error()`（**已改，登记原因**）

**影响**：中，正向。

独立服务的两个端点在出错时走 `respondErr(..., err.Error())`，也就是把 `database/sql` 驱动的错误原文放进 500 的响应体——那里面有主机、端口，失败发生在语句上时还有 SQL。

现在 handler 把 store 的错误**原样往上抛**，由平台错误处理器答 500 `ERR_INTERNAL` 加一句通用文案，原因只进 `slog`。这与全仓库的口径一致（`compat/phorge/README.md` 附录：排查 500 看服务日志，不要指望响应体），但在本域它还承担了第二个作用：**本域没有域级错误码**（`modules/webhook.md` 第 6 节），所以没有任何东西承载细节，message 就必须保持通用。`tests/contract/webhook/unavailable/stats-database-unreachable.json` 从反面钉住这一点——它断言的是 body 里**不出现**库名、主机、端口与 SQL。`TestAStoreFailureIsAnOpaque500` 在单测层守同一件事。

### 34. 传输失败的 `errorCode` 是 Go 的原始错误字符串

**影响**：低到中。**登记而不修**，当前行为是刻意保留的。

一次连接失败之后，`classifyTransportError` 把 `err.Error()` 整个当作 `errorCode` 写进 `properties`，于是 `dial tcp 10.0.0.7:9000: connect: connection refused` 这样的串会连同它解析出的 IP 与端口一起被渲染到 Phorge 的 webhook 请求详情页上。

保留的理由是与旧服务及 Phorge UI 既有观感一致：那一栏本来就在显示「HTTP Status Code / 502」这类原文，一个短码反而需要 Phorge 侧知道怎么翻译它。但要记清一件事——**Phorge 自己在这个位置放的是短码**（`ERROR_*` 一族），所以这里是本服务在扩大那一栏的值域，而不是在填一个已有的形状。

后果是可见的、也是有限的：那个页面的读者是能管理 Herald 规则的用户，而内部主机名与端口对他们并不算秘密。真要改，改法是给传输错误一组自己的短码（`connection-refused` / `dns-failure` / …），并同时决定「认不出的传输错误」落到哪一个——而那个兜底码会把今天可以直接读出来的原因藏起来，这是不修的第二个理由。

### 35. 属性不可解析时 `properties` 列会丢掉 Phorge 写入的全部内容

**影响**：低。**登记而不修**，源仓库行为相同。

`UpdateResult` 整列重写 `properties`（这是必须的，见 `modules/webhook.md` 第 5 节：`RequestProperties` 原样带回本服务不读的键正是为此）。但在**属性本身解不开**这条路上，`json.Unmarshal` 失败之后那个结构体是零值，于是回写下去的只有本次设的 `errorType` 与 `errorCode` 两个键——Phorge 写进去的 `transactionPHIDs`、`triggerPHIDs`、`retry` 全部消失。

它的影响之所以低，是因为原列**本已是非法 JSON**，里面没有可保留的内容：既然解不开，就没有办法只替换其中两个键。但这是本服务唯一一条会丢弃 Phorge 写入内容的写路径，所以值得登记——将来若有人把 `properties` 的解析改成宽容一些（比如先按 `map[string]any` 解一遍再取已知键），这条路就会变成「部分可保留」，那时它就不再是无害的了。

`TestUnparseablePropertiesFailTheRequestPermanently` 覆盖这条路，但它断言的是终态与 errorCode，**不是**那一列剩下什么。

### 36. 优雅退出时在途投递的结果写入会丢失

**影响**：中。**登记而不修**，修它需要一处结构改动。

`Dispatcher.Run` 在 context 取消时会等在途投递结束（`inFlight.Wait()`），而 `main.go` 也在关连接池之前等这个循环收尾。但那些投递**共用同一个 context**，所以取消它同时也取消了它们的结果写入：一个 POST 已经发出去、对方已经答了 200 的 request，它的终态写不进去。

后果是那一行留在 `queued`（或者仍带着旧的 claim），lease 到期后**会被再投一次**。所以第 30 条那条「同一个事件不会被投两次」的保证，在「进程正好在投递中被停掉」这个窗口上是不成立的。窗口的大小就是一次部署或重启。

修法是给结果写入一个**独立于投递的 context**（比如一个带自己超时的 `context.WithoutCancel` 派生），这样取消只中断还在飞的 POST，不中断已经拿到答案的那些行的收尾。没有在本次做，是因为它牵动 `Dispatcher.record` 的每一个调用点，而那些调用点正好是三条不同的失败分类路径——和一次迁入混在一个改动里不合适。`dispatcher.go` 的 `Run` 注释里写明了这是已知限制而非疏漏。

### 37. 全局静默 `phabricator.silent` 本服务读不到

**影响**：中，但只在开了全局静默的部署上。**登记而不修**，PHP 侧已经用一个守卫把它挡住了。

`phabricator.silent` 是 Phorge **服务器**的配置项，而本服务从不读 Phorge 的配置——它只能看到 request 行 `properties` 里那个 per-request 的 `silent` 属性，而那个属性描述的是一次事务，不是整个装置。

所以一个开了全局静默、又把投递交给本服务的部署，webhook 会照常发出去——静默模式存在的全部目的就此失效。**这正是 `phorge-fork` 侧那个守卫要用合取条件的原因**：`PhabricatorGorgeWebhookClient::isDeliveryDelegated()` 要求 `gorge.webhook.uri` 非空 **且** 静默未开。静默的装置继续走 PHP 路径，由 worker 的 `failRequest(..., ERROR_SILENT)` 把 request 标成 `failed`，而本服务只取 `queued`，于是自然碰不到它们。

**要记住的是这条修法的方向**：它不是「让 Go 侧学会读 Phorge 的配置」，而是「让静默这一类流量根本不进入 Go 侧的视野」。前者需要本服务去解析 `conf/local/local.json` 或者新增一个必须与 Phorge 保持同步的环境变量，两者都是把一个配置项变成两处真源。

### 38. 本服务必须**替换** PHP 侧投递，不能并存

**影响**：高。这条既不是缺陷也不是待办，是一条**部署约束**，登记在这里是因为它是这个域最容易被误判的一件事。

其余五个域的形状都是「PHP 调 Go」，所以「服务在跑但 PHP 侧没配」的表现是 PHP 侧继续用它自己的实现——一个安全的、可见的失配。本域的队列在数据库里，两边谁都能取，所以同一个失配的表现是**两个消费者同时排空一个队列**，也就是给别人的 endpoint 发重复 POST。

**两个消费者共用一个队列不是扩容方案。**要横向扩容就多起几个 `gorge-webhook`——claim 机制正是为此存在的——而不是让 phd 的 `HeraldWebhookWorker` 和它一起跑。而且接收方**无法把这种重复与一次真正的重复事件区分开**：payload 是逐字节相同的（同一个 `dateCreated`、同一批 transaction PHID），签名也相同，所以任何按内容去重的接收端都会把它当成一次事件的合法重传，而按事件去重的接收端根本看不出来收了两次。

顺带记一件相关的历史：老仓库 `phorge` 里的 `HeraldWebhookWorker.php` 与上游**完全没有 diff**，而 `PhabricatorGoWebhookClient` 是死代码（零调用点）——也就是说那个部署一直带着这个双投缺陷在跑。`phorge-fork` 侧的两处守卫（`queueCall()` 与 `doWork()`）就是补这一条的。

### 39. `platform/` 仍然不加 db 包，尽管本域是「第二个需要 DB 的域」

**影响**：低。这条是第 24 条那个判据**实际到来时的答复**，所以它必须被写下来——否则下一个人只会看到「等第二个域再评估」而不知道评估已经做过了。

第 24 条留的判据是「第二个域需要连接池的时候，而且判据是那时两个域的 DSN 拼法与池参数是否真的能共用——不是『都用了 database/sql』」。webhook 正是第二个，按这条判据逐项看下来，答案是**继续不加**：

| | file-storage | webhook |
|---|---|---|
| DSN 库名 | `{ns}_file` | `{ns}_herald` |
| 连接池 | 20 / 5 / 5m（按「一次 INSERT 或一次 SELECT」定，行大小被 blob 上限封着） | 20 / 5 / 5m（按「`MaxConcurrent` 次并发投递 × 每次碰库三到四回」定） |
| 查询语义 | 单行读写，无并发控制 | 乐观 CAS claim、lease、滑动窗口计数 |
| `/readyz` | 「配了引擎」+「持有连接的引擎连得上」 | 只有「连得上」 |

`maxOpenConns` 两边碰巧都是 20 这件事**不构成共性**——它们是从两条完全不同的推导得出的，而共享一个默认值意味着下一次任何一侧调整它都得先说服另一侧。真正能共享的只有「调三个 `Set*` 方法」这几行，而那不值得一个包。

所以两个域各自 import `go-sql-driver/mysql`、各自在域包的 `db.go` 里管自己的池。**这两个域共享的是一个驱动，不是一个关切。**`webhook/db.go` 的 import 注释里写明了这句话。

**什么时候重新考虑**：不是「第三个域」——那只是把同一个数字往上加——而是**某两个域真的需要连同一个库、并且需要看到彼此的连接预算**的时候。在那之前，`platform/` 保持没有数据库设施这件事本身就是文档，说明「域包可以持有自己的外部依赖」。这条与第 28 条（两套 AWS 签名实现）是同一个形状。

### 40. PHP 侧：新索引必须同时声明进 `CONFIG_KEY_SCHEMA`

**影响**：高，而且它的失败模式在时间上是错位的。这条讲的是 `phorge-fork`，记在这里是因为 Go 侧的候选查询是按吃到这个索引的形状写的。

`key_status (status, id)` 由 autopatch（`20260907.herald.01.requeststatuskey.sql`）加上，这一步是显而易见的。不显而易见的是第二步：**它必须同时出现在 `HeraldWebhookRequest::getConfiguration()` 的 `CONFIG_KEY_SCHEMA` 里**，否则 `PhabricatorStorageManagementWorkflow::findAdjustments()` 会把它判成 `ISSUE_SURPLUSKEY`——一个「数据库里有、而模型没声明」的多余索引——而 `bin/storage adjust` 会把多余索引删掉。

于是漏掉这一步的后果是：索引建好了、查询用上了、一切正常，然后在**某个与本次改动毫无关联的时间点**（下一次有人跑 `bin/storage adjust`，可能是几个月后为了别的事）它被删掉，队列轮询悄悄退回全表扫。没有任何一处会把这次删除与 webhook 联系起来。

`HeraldWebhookRequest.php` 里那段声明的注释写明了这个理由。**加任何一个 autopatch 索引都适用同一条**，这不是本次改动特有的。

### 41. PHP 侧：`phorge` 对 `gorge-webhook` 的 compose 依赖只能是 `service_started`

**影响**：中，但它会让首次启动完全卡住，所以必须知道。同样讲的是 `phorge-fork`。

`gorge-webhook` 的 healthcheck 探 `/readyz`，而 `/readyz` ping 的是 `{ns}_herald` 这个库——**那个库正是 phorge 容器里的 `bin/storage upgrade` 建的**（`db-init` 只发 GRANT，不建库）。所以让 `phorge` 用 `service_healthy` 等它，得到的是「phorge 等服务、服务等只有 phorge 能建的库」这个死锁，两个容器一起停在启动阶段。

依赖因此是 `service_started`——它是 `phorge` 那六条 Gorge 依赖里唯一一条这样的。代价是首次启动时 `gorge-webhook` 会有一段 unhealthy 的窗口，直到 `bin/storage upgrade` 跑完。**那段窗口是预期的，不是故障**，`docker-compose.gorge.yml` 的 healthcheck 注释里写明了它。

**这里此前写的「file-storage 那边库不是 Phorge 建的、所以它可以照常被 `service_healthy` 等」是错的，已由第 43 条证伪。**`{ns}_file` 与 `{ns}_herald` 都是 `bin/storage upgrade` 建的，两个域的 `/readyz` 因此是同一个形状、同一个闭环；file-storage 只是当时用了 `service_healthy`，于是同一条链在那边真的死锁了。**两个域现在都用 `service_started`，而正确的读法是：这条不是本域特有的权衡，是每一个 ping 带库名 DSN 的域都必须接受的依赖强度。**

### 42. `MySQLStore` 零覆盖，而这一次不该用补测试来抬

**影响**：中。它是仓库里第二个「整块零覆盖」的登记项（第 17 条是第一个），但性质与那一条相反，所以处置方式也相反。

`internal/webhook` 70.4% 是覆盖率表上最低的真实数字，缺口整块落在一个类型上：`MySQLStore` 的十个方法与 `OpenDB` 全部 0.0%，包里其余每一处都在 83% 到 100% 之间。原因是结构性的——域逻辑全部由内存 fake 驱动测到，而那些 `QueryContext` / `ExecContext` 的包装层没有任何东西碰得到：仓库里没有 MySQL，也刻意不用 sqlmock（惯例是手写 fake）。

**关键是分清哪一半已经守住了。**那几条 SQL 的**形状**是测到的——`TestClaimStatementIsAnOptimisticCompareAndSet` 与 `TestFetchClaimableStatementKeepsItsConditionsSeparate` 直接断言常量字符串里那些 WHERE 条件都还在，而那正是最容易被「顺手简化」掉的东西（第 30、31 条那三条设计理由每一条都对应其中一个条件）。没守住的是另一半：**驱动照这些字符串跑出来的结果是不是那个意思。**具体是两个问题——`GREATEST(dateModified + 1, UNIX_TIMESTAMP())` 在秒级精度下真的每次都让版本前进吗；两个并发 UPDATE 打同一行时 `RowsAffected()` 真的只有一个是 1 吗。

**这两个问题一个 fake 永远答不了**，因为 fake 实现的是本域**以为**那条 SQL 会做的事——它和被测代码来自同一份理解，所以它们会一起错。这是本条与第 17 条的分水岭：Meilisearch 后端的缺口是「没写的测试」，补是可行的、而且有 Elasticsearch 那份可以照抄；这里的缺口是「测不到的边界」，用 fake 把它覆盖到 100% 只会得到一个看起来更厚、实际什么都没多守住的防线。

**所以不要为了这个数字补测试。**抬它的唯一诚实办法是一次对着真 MySQL 的集成测试，而且值得写的只有两条，就是上面那两个问题——一个建表、插一行、从两个 goroutine 同时 claim、断言恰好一个成功的用例，加一个同秒插入并 claim 的用例。它需要 CI 里有一个 MySQL 服务容器，那是本仓库至今没有的东西，也是这条留在这里而不是直接做掉的原因。在那之前，`tests/e2e/webhook.sh` 对着真库跑的那几条是唯一碰到这个类型的东西，但它只读不写，所以 claim 那条路它也走不到。

### 44. 这个域最需要的那个信号打不出来，不需要的那个刷屏

**影响**：中。两条日志，一条 Debug 一条 Warn，方向正好相反，所以合成一条登记——分开写会让人以为它们是两个独立的措辞问题，而它们是同一个问题：**这个域的日志级别没有按「排查时要看什么」分配过。**

**打不出来的那条是 `WEBHOOK_CLAIM_LOST`。**它用 `slog.Debug`，而全仓库 grep 不到任何日志级别配置——没有 `LOG_LEVEL`、没有 `SetLogLoggerLevel`、没有 `HandlerOptions`——`log/slog` 的默认级别是 Info。所以它在**任何**部署里都不会出现。

而它承载的正好是这个域唯一一个直接信号：**抢占到底有没有在挡重复投递。**第 30 条那套机制是本次迁入最要紧的一处改动，它的正确性表现为「什么都没发生」——没有重复 POST——而「什么都没发生」和「机制根本没生效但今天恰好没并发」在外部完全同形。端到端验证因此只能靠**「POST 总数恰好等于队列行数」反推**（三实例 / 单 hook / 40 行加压下零重复），而那需要一个受控的接收端，不是运维手里有的东西。

**刷屏的那条是 `WEBHOOK_HOOK_IN_ERROR_BACKOFF`**，它是 Warn，而且是**每个候选行每个 tick** 一条。实测 60 秒打了 1140 条（poll 300ms、6 行）。按出厂默认（poll 1s、`MaxConcurrent` 8）折算约 8 条/秒，一个坏掉的 hook 带着积压跑一天约 69 万条 Warn——而熔断窗口本身只有 300 秒，也就是说这些 Warn 里绝大多数在重复陈述一个已经稳定了的状态。它会把同一时间段内真正要紧的东西（`WEBHOOK_DELIVERY_FAILED` 的原因、`WEBHOOK_CLAIM_FAILED`）冲掉。

**修法的方向不是调级别了事。**`CLAIM_LOST` 抬到 Info 是一行改动，但那样它会在正常的多实例部署里稳定产出噪声——抢输是预期行为。真正对的形状是**计数**而不是逐条日志：抢占的成/败、熔断跳过的行数，都属于「一个周期性汇总一行」或者「一个指标」，而不属于每行一条。`IN_ERROR_BACKOFF` 同理，它该是「进入熔断时一条、退出时一条」，中间的每 tick 一条没有信息量。仓库至今没有指标设施，所以这条留在这里而不是直接做掉；在那之前，**排查抢占问题的唯一办法是数 POST，请知道这一点**。

### 45. 失败重投的实际间隔是 `max(ClaimLease(), RetryBackoffSec)`，配置项因此有一半是哑的

**影响**：中。默认值下**没有任何问题**——30 < 60，退避那道门晚，生效的就是 60（实测两次为 60.01 秒与 59.98 秒），设计是对的。这条讲的是往下调的那个方向。

第 30 条的候选查询里，lease 与失败退避是**两个独立的 AND 条件**（`TestFetchClaimableStatementKeepsItsConditionsSeparate` 就是为了钉住它们没被合并），而 `UpdateResult` 在失败时**同时**刷新 `dateModified` 与 `lastRequestEpoch`。于是那一行要重新成为候选，必须**同时**越过两道门，而先到的那道不算数：

| `RETRY_BACKOFF_SEC` | `CLAIM_LEASE_SEC` | 实际间隔 |
|---|---|---|
| 60（默认） | 30（默认） | 60，实测吻合 |
| 2 | 20 | **20**，实测 20.1 秒 |
| 0 | 30 | **30** |

**所以把 `GORGE_WEBHOOK_RETRY_BACKOFF_SEC` 调到 lease 以下不会有任何效果，它被静默抬回 lease。**这与 `Config.ClaimLease()` 那个「低于投递超时就抬上去」的抬升是两件不同的事：后者发生在配置里、有测试、有注释；这一个发生在 SQL 的合取条件里，配置结构体里读不出来，日志里也不出现。

**根治方向是启动时校验并告警**——`RetryBackoffSec < ClaimLease()` 时打一条 Warn 说明实际生效值——而不是靠文档提醒，因为会去调这个值的人正是那种不会先读文档的人。那是代码改动，本次不做。已改的只有文档：[`modules/webhook.md`](modules/webhook.md) 第 3.2 节与 `deploy/compose/.env.example` 里那句「设成 0 恢复下一个 tick 重投」（它是错的，设成 0 得到的是 lease 时长）。

**一般化的那条**：两个刻意不合并的条件，只要它们判据的那两列会被同一次写入一起刷新，对外就仍然表现为一个条件——取两者中更严的那个。分开是对的（合并会让两个方向都错，见第 3.2 节），但**分开不等于独立**，而文档和配置注释很容易把「分开」写成「独立」。
