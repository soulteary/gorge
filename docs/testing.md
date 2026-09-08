# 测试体系

四层，各守一类东西。新模块迁入时四层都要补，其中契约固件是唯一跨语言共享的一层。

| 层 | 位置 | 守什么 | 谁跑 |
|---|---|---|---|
| 单元测试 | `go/internal/**/*_test.go` | 各包行为 | CI |
| 分层测试 | `internal/platform/layering_test.go` | 平台层不反向依赖域包 | CI |
| 契约固件 | `tests/contract/<域>/*.json` + 域内 runner | 线上契约 | CI（Go runner）+ 将来的 PHP runner |
| e2e 冒烟 | `tests/e2e/<域>.sh` | 对着真实运行实例的端到端行为 | 手动 / `make e2e` |

## 1. 分层测试

用 `go/parser` 解析 `platform/` 下所有文件的 import，断言不出现域包与契约包。55 行把一条通常只写在 README 里的架构规则变成 CI 失败。详见 [`architecture.md`](architecture.md) 第 3.1 节——那里也写了**新增域包时要往 `forbiddenPrefixes` 加一行**，这是本层唯一需要人工维护的地方。

## 2. 契约固件：一份描述，两个 runner

固件放在仓库根的 `tests/` 而不是 `go/` 下，因为它们是给 Go 服务与将来的 PHP 适配层**共同**运行的：一份线上契约的描述，两个执行器。

每个子目录归属一个域：

| 目录 | 服务 | Go runner |
|---|---|---|
| `render/` | `gorge-render` | `go/internal/render/contract_test.go` |
| `diff/` | `gorge-render`（同进程） | `go/internal/diff/contract_test.go` |
| `notification/admin/` | `gorge-notification` 的 admin 口 | `go/internal/notification/contract_admin_test.go` |
| `notification/client/` | 同上，client 口 | `go/internal/notification/contract_client_test.go` |
| `mailer/` | `gorge-mailer` | `go/internal/mailer/contract_test.go` |
| `search/` | `gorge-search` | `go/internal/search/contract_test.go` |
| `search/unavailable/` | 同上，但服务配的是一个必然失败的后端 | 同一个文件里的第二个 `Run` |
| `file-storage/` | `gorge-file-storage` | `go/internal/filestorage/contract_test.go` |
| `webhook/` | `gorge-webhook` | `go/internal/webhook/contract_test.go` |
| `webhook/unavailable/` | 同上，但服务配的是一个必然失败的 store | 同一个文件里的第二个 `Run` |
| `taskqueue/` | `gorge-taskqueue` | `go/internal/taskqueue/contract_test.go` |
| `taskqueue/unavailable/` | 同上，但服务配的是一个必然失败的 store | 同一个文件里的第二个 `Run` |
| `conduit/` | `gorge-conduit` | `go/internal/conduit/contract_test.go` |
| `dbapi/` | `gorge-db-api` | `go/internal/dbapi/contract_test.go` |
| `dbapi/unavailable/` | 同上，但服务对着一个连不上的库 | 同一个文件里的第二个 `Run` |

**db-api 域的固件与 e2e 已落地。** db-api 的 Go 服务迁入的同时，契约固件与 e2e 脚本也一并补齐：`go/internal/dbapi/contract_test.go` 按 webhook / taskqueue 的形状写好，指向 `tests/contract/dbapi/`（七条只读路由 + 401 + 查询参数认证，共 10 份 JSON）与 `tests/contract/dbapi/unavailable/`（3 份：库连不上时 message 保持通用、body 不泄漏 SQL/库名/主机/端口）。`go test ./...` 现已整套变绿，`internal/dbapi` 覆盖率 86.9%。下面第 2.3 节的固件总数与第 4 节的 e2e 脚本数均已把 db-api 计入。

**notification 一个域两个固件目录**，因为它是一个域两个端口，而同一条请求在两个端口上的正确答案不一样（`GET /` 在 admin 口是 200 探针、在 client 口必须是 501）。合成一个目录就没法表达这件事。

**search 也是两个目录，但理由完全不同：它一个域一个端口，分开的是被测服务的配置。** mailer 的固件能在请求体里用 `mailerKeys` 指向一个会失败的适配器，所以健康与故障两种情况共存于一个目录；而 search 的引擎**只按角色选后端**，请求体里没有任何选择后端的手段。于是「一个正常的存储」与「一个坏掉的存储」是两份服务配置而不是两种请求，只能由两个 `contracttest.Run` 各起一个服务来跑。`unavailable/` 那六份是五个域级错误码唯一的到达路径（`ERR_CHECK_FAILED` 有 `/exists` 与 `/sane` 两条路进去，所以是六份而不是五份），而它们能被写出来的前提是 `engine.TestBackend` 支持可注入失败——那也是它是生产代码而不是 `_test.go` 辅助函数的原因。

**webhook 也是两个目录，理由与 search 结构上相同**：它的两个端点都只做一件事——数行——所以唯一的失败模式就是数据库，而「store 不答话时端点答什么」不是一个请求能提出的问题，只能换一个服务配置来制造。所以 `unavailable/` 那一份由第二个 `Run` 起一个 store 每次调用都失败的服务来跑。

但 webhook 在这一层还有一件与前六个域都不同的事，读它的固件之前必须知道：**这批固件描述的是这个域较小的那一半。**`gorge-webhook` 真正在做的是排空一个队列并向第三方 POST，而那件事**没有任何请求能启动**——固件的形式是「一个请求加它的期望应答」，所以它只能描述本服务**答**的东西，描述不了本服务**发**的东西。而后者恰恰是这个域最硬的契约（投出去那份文档是逐字节钉住的，签名对它算），它由 `go/internal/webhook/dispatcher_test.go` 守着，那里可以把时钟按住并读出确切的字节。**别因为固件目录只有 5 份就以为这个域的契约面小。**

webhook 的 runner 还有一条别的域都没有的前提：它必须**注入一个 store 而不是连 MySQL**，并且 seed 一份确切的状态（2 个 hook 其中 1 个禁用，queued / sent / failed = 3 / 2 / 1）。**那四个数字刻意互不相等**——只要有两个相等，`stats.json` 就能被一个答错字段的实现通过，而最容易混的那一对（`activeWebhooks` 与 `hooks.total`）正是那一个禁用 hook 分开的。这也是 `go/internal/webhook/store.go` 把 store 抽成 interface 的原因，而不是抽出来之后顺便能这么测：file-storage 能把固件指向一个本地目录、mailer 能指向一个 `test` 适配器，本域没有对应物，因为**两个端点都读库**。逐条对应与 seed 的完整要求见 [`../tests/contract/webhook/README.md`](../tests/contract/webhook/README.md)。

**taskqueue 与 webhook 结构上完全同形**：一个 `unavailable/` 目录、runner 注入内存 `Store`（`memstore_test.go`）而不连 MySQL/Redis、seed 一份确定状态（3 个活跃任务、2 个归档任务）。它比 webhook 多一层要小心的东西：**固件里有会改状态的写操作**（`enqueue.json`、`lease.json`），而 `stats.json` 又要断言计数，所以固件按字母序执行时写操作会先跑。处理方式是让 `stats.json` 只精确断言不受写操作影响的 `archivedCount`（没有固件 complete 任务，所以归档数稳定在 2），其余三个计数只用 `jsonHas` 断言存在——这样固件对执行顺序稳健。这也是 taskqueue 一个域三份 store 实现（MySQL / Redis / 内存）的收益兑现处：三者满足同一个接口，contract 与 handler 测都注入最轻的那份。

这些 runner 都只是三行 wrapper，真正的重放逻辑在 `go/internal/contracttest/`。它是 diff 迁入时从 render 的固件测试里抽出来的，抽出的理由不是省代码，而是**断言词汇必须在两个域之间保持一致**——各写一份 runner，两个域很快会开始用不同的方式描述自己的契约。它是普通包而非 `_test.go`，因为要被两个域的测试 import。

### 2.1 格式

一个文件一个 JSON 对象：

```json
{
  "name": "short human-readable name",
  "description": "why this case matters",
  "request": {
    "method": "POST",
    "path": "/api/highlight/render",
    "headers": { "X-Service-Token": "contract-token" },
    "body": "{\"source\":\"x = 1\",\"language\":\"python\"}"
  },
  "expect": {
    "status": 200,
    "jsonHas": ["data.html"],
    "jsonAbsent": ["error"],
    "jsonEquals": { "data.language": "python" },
    "htmlContainsClasses": ["k", "mi"],
    "htmlContains": ["<span"],
    "htmlNotContains": ["<pre>"]
  }
}
```

`request.body` 是**字符串**而非嵌套对象。这是刻意的：嵌套对象无法表达一个格式错误的 payload，而 `render-malformed-body.json` 正需要它。

`expect` 的字段除 `status` 外全部可选，分三组作用域：`json*` 作用于解码后的响应、`html*` 作用于解码出的 `data.html`、`body*` 作用于原始响应体。**检查渲染出的标记时优先用 `html*`**——JSON 编码器对 `<` 的转义方式不同，对原始 body 做子串匹配在 Go 与 PHP runner 之间不可移植。

`json*` 的路径支持数组下标，数字段落即下标：`data.parts.0.type` 能钉住 prose diff 返回的片段序列。`jsonAbsent` 对越界下标返回「不存在」，所以 `data.parts.4` 可以用来断言片段数量。

`html*` 那组只有 render 域在用，但保留在共享词汇里——两个域要用同一套名字描述自己的契约。

### 2.2 runner 的前提

runner 必须用 token `contract-token` 启动服务：固件靠这个值认证，且其中一份固件断言不带 token 的请求被拒。其余一律用服务默认值。

**notification 是这一条的例外**：那个域按设计不挂鉴权中间件（Phorge 的通知客户端不发凭据，配了 token 会让它发的每条消息都被拒），所以它的两个 runner 忽略 `contracttest.Token`，固件目录里也没有 `unauthorized.json`。`contracttest.go` 里那个常量的注释写明了这一点——**别看到少一份未授权固件就去补一份**。

Go runner 用 `httptest` 起一个内存中的 `httpx.New(...)` + `RegisterRoutes(...)`，不监听真实端口。

### 2.3 现有固件

**render 域 12 份**：正常渲染（python / go）、别名解析、大小写敏感的两条（`.R` / `.r`）、未知语言、空 source、CRLF、格式错误的请求体、未授权、查询参数认证、语言列表。

**diff 域 14 份**：hunk 头的三种计数形态、无尾换行的三种组合、identical 分支、normalize、prose 的三条、未授权、查询参数认证、格式错误的请求体。逐条对应见 [`../tests/contract/diff/README.md`](../tests/contract/diff/README.md)。

**notification 域 11 份**：admin 7 份（发消息、form-urlencoded 的 Content-Type、空 body、格式错误的 body、`/status/` 的扁平点号键、带 instance 的 `/status/`、根探针），client 4 份（`GET /` 与实例路径各一条 501、带 Upgrade 头但不是 WebSocket 的一条 501、`/healthz` 不被通配符吃掉）。逐条对应见 [`../tests/contract/notification/README.md`](../tests/contract/notification/README.md)。

**search 域 23 份**：主目录 17 份（写入成功、写入一份中文文档、缺 `phid`、缺 `type`、格式错误的请求体、检索、无筛选列表、三个所有者状态、`/init`、`/init` 缺 `docTypes`、`/sane`、`/sane` 缺 `docTypes`、`/exists`、`/stats`、`/backends`、未授权、查询参数认证），`unavailable/` 6 份（五个域级错误码，`ERR_CHECK_FAILED` 占两份）。

**file-storage 域 14 份**：写入（默认按优先级、指名引擎、零字节文件、未知引擎答 400 而不是替换成别的）、读取（裸字节、读不到时仍是信封、缺 engine 参数、任何引擎都签发不出的 handle 仍是 404）、删除（对已经不在的字节成功、缺 handle、非法 handle 答 400）、引擎列表、未授权、查询参数认证。

这一组里有两对是**成对**的，拆掉任何一半都只剩一半防线：`read-blob.json` 与 `read-blob-missing.json` 分别钉住「成功答裸字节」与「失败仍答信封」，它们合起来才表达了全仓唯一那个非信封成功响应的完整形状；`read-blob-bad-handle.json`（404）与 `delete-blob-bad-handle.json`（400）钉住的是同一个非法 handle 在读与删两条路上**刻意**答两个不同的码，理由见 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第 8.6 节与 [`modules/file-storage.md`](modules/file-storage.md) 第 3.4 节。`write-blob-empty.json` 也不是凑数的：零字节文件是一个合法的 200 加一个空 body，而按「body 是不是空的」判断成败的客户端会把它报成错误。

**webhook 域 5 份**：主目录 4 份（`stats` 的四个计数、`hooks` 数的是**每一个** hook 而不只是启用的、未授权、查询参数认证），`unavailable/` 1 份（数据库不可达时答 500 `ERR_INTERNAL`，且 body 里不出现库名、主机、端口或 SQL）。最后那一份是本域**没有域级错误码**这个决定的反面守卫：既然没有码承载细节，message 就必须保持通用。

**taskqueue 域 8 份**：主目录 6 份（`enqueue` 入队并回显 id 与默认优先级、`lease` 用 `X-Lease-Owner` 头租走任务并回显 owner、`stats` 的四个计数、`tasks` 活跃任务列表、`tasks/:id` 单任务带 Phorge 列名、未授权），`unavailable/` 2 份（`stats` 与 `tasks` 在后端不可达时答 500 `ERR_INTERNAL`，且 body 不出现库名、主机、端口、SQL 或 `connection refused`）。worker 域**没有固件**：它唯一的端点读进程内计数器、永不失败，一个「请求加期望应答」的固件对它无可断言，那条路径由 `go/internal/worker/http_test.go` 覆盖。

**conduit 域 4 份**：未授权、限流、缺 Conduit 方法，以及成功请求的透明代理；runner 用 `httptest` 启动假上游，避免依赖真实 Phorge。

**db-api 域 13 份**：主目录 10 份（`servers`、`server-health`、`server-health-unknown`、`schema-diff`、`schema-issues`、`setup-issues`、`charset-info`、`migrations-status` 八条只读路由，加未授权与查询参数认证），`unavailable/` 3 份（`schema-diff`、`charset-info` 在库连不上时答 503 `ERR_DB_UNREACHABLE` 且 body 不泄漏 SQL/库名、主机、端口，`servers` 则以 in-band `fail` 记录不可达节点、`refKey` 作为契约字段合法保留）。连同上面各域，契约固件现共 **114 份**。

`index-cjk-document.json` 值得说一句它**验不到**什么：它断言一份中文文档写得进去、答 200 并回显 PHID，这是真的；但固件跑的是内存 `test` 后端，那个后端做子串匹配、不过分析器，所以**它对 `cjk` 子字段一无所知**。中文检索真正能不能工作只有 `tests/e2e/search.sh` 的第 11、12 条对着真 Elasticsearch 才验得到（见 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第 7.5 条末尾）。别把这份固件当成 CJK 的覆盖。

这批固件让共享 runner 长了两处：`lookupJSONPath` 现在会**先把整段路径当字面量键查一次**再按 `.` 切分，否则 `clients.active` 这类键寻址不到（那些点是键名的一部分，不是嵌套）；`check` 现在对「只断言状态码与原始字节」的固件跳过 JSON 解码，否则 client 口那句纯文本 501 会在解码那一步就失败。两处都是共享词汇的扩展而非 notification 专用分支。

file-storage 又让它长了第三处，形状相同：`expect.headerEquals` 断言响应头，头名按 canonical 形式匹配。逼出它的是那个非信封的读路径——`Content-Type` 正是 PHP 客户端用来分辨「一份文件」与「一个信封」的依据，而在此之前固件没有任何办法断言它。

**webhook 一处都没让它长**，这本身值得记一句：它的两个端点答的都是最普通的信封加几个整数，共享词汇原样够用。**taskqueue 同样一处都没让它长**——它十条路由答的要么是信封包着 `Task` / 计数、要么是 `{"status":"ok"}`，共享词汇照旧够用。所以「一个新域会不会给固件 runner 加东西」取决于它的**应答形状**有多特别，而不取决于这个域本身有多特别——而按后者算，webhook 与 taskqueue 都在最特别的那几个里，却都没给 runner 添一行。

diff 域**刻意没有**超限固件：两道尺寸护栏都随部署可配，一份断言 413 的固件会随被测服务的启动参数时过时不过，而这正是契约固件不能有的性质。那些路径在 `go/internal/diff/http_test.go` 里覆盖，那里可以设限。

## 3. 断言的精度要按输出的稳定性来定

这一层最容易走极端：要么全做字节精确、要么全做包含。**两个域给出了相反的答案，而两个都是对的。**

### 3.1 render：contains 而非 golden

渲染出的 HTML 会随 Chroma 每次升级而变，而且变化方式**不构成回归**——一个 token 裂成两个、空白在 span 之间挪位。字节精确的 golden 文件会在每次 Chroma 升级时失败，然后被**不加阅读地重新录制**，那比没有测试更糟。

真正必须固定不动的是 Pygments CSS 类名集合，因为 Phorge 的样式表是按它写的。`k`、`nf`、`nb`、`s2`、`mi`、`c1` 消失意味着全站代码块失去样式，这才是值得捕捉的回归，`htmlContainsClasses` 捕捉的就是它。

### 3.2 diff：整值比对

unified diff 的输出不是被渲染的，是被 `ArcanistDiffParser` **解析**的——它读 hunk 头来决定之后每一行的归属。写成 `-1,1` 而 GNU 写 `-1`，仍然是一份语法合法的 unified diff，所以没有任何东西会拒绝它；只是它之后的行全部被归到错误的位置，错误在评审页面上表现为渲染错位，离出错点已经很远。

**这里不存在「可以安全断言的子集」**，所以 `data.diff` 做整值比对。期望值不是手写的，是从真实 `diff -U65535` 抓的（命令写在固件 README 里）。

不止抓一次：`go/internal/diff/unified/systemdiff_test.go` 每次 `go test` 都真的调系统 `diff` 交叉验证，所以格式漂移不依赖谁记得重跑命令。**这一层测出了手写用例没覆盖的一处偏差**——对齐存在多个同样最小的方案时（仅发生于含重复行的输入），引擎选的那个与 GNU 不同。保证因此是分层的：格式规则（hunk 头计数、`\ No newline` 位置）与 GNU 逐字节等同、编辑脚本始终最小，但歧义时的对齐选择不保证相同。实测 hunk 头 0 次不同，所以行号从不错位。完整数据见 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第 4.6 节。

这件事本身是这一层价值的例证：**手写用例只能覆盖想到的形状。**

prose diff 落在两者之间：它的输出没有外部基准，所以固件钉住几个小输入的分段，而真正要紧的**无损不变量**（`=`+`-` 还原旧文本、`=`+`+` 还原新文本）交给单元测试用 17 组输入压。

### 3.3 判据

**问输出会不会在无回归的情况下变化。** 会（第三方渲染器的 markup）就断言不变量；不会（被下游解析的格式）就整值比对。搞反任何一边都有代价：前者会训练出「失败了就重新录制」的习惯，后者会让静默的格式漂移一路走到生产。

## 4. e2e 冒烟

九份脚本，都对着**已经在跑**的实例执行，自己不启动也不清理任何东西（db-api 的 `dbapi.sh` 已落地，镜像 `webhook.sh` 的形状）：

```bash
BASE_URL=http://127.0.0.1:8140 TOKEN=dev bash tests/e2e/render.sh
BASE_URL=http://127.0.0.1:8140 TOKEN=dev bash tests/e2e/diff.sh
ADMIN_URL=http://127.0.0.1:22281 CLIENT_URL=http://127.0.0.1:22280 \
  bash tests/e2e/notification.sh
BASE_URL=http://127.0.0.1:8110 TOKEN=dev bash tests/e2e/mailer.sh
BASE_URL=http://127.0.0.1:8120 TOKEN=dev bash tests/e2e/search.sh   # ⚠ 会销毁索引
BASE_URL=http://127.0.0.1:8100 TOKEN=dev bash tests/e2e/file-storage.sh
BASE_URL=http://127.0.0.1:8160 TOKEN=dev bash tests/e2e/webhook.sh
BASE_URL=http://127.0.0.1:8090 TOKEN=dev bash tests/e2e/taskqueue.sh
BASE_URL=http://127.0.0.1:8080 TOKEN=dev bash tests/e2e/dbapi.sh
# 或
TOKEN=dev-token make e2e     # 九份都跑
```

render 与 diff 共用一个端口，所以那两份是「两个脚本打同一个 `BASE_URL`」，不是两套部署。notification 是另一个进程，而且**要两个变量**：`ADMIN_URL` 与 `CLIENT_URL` 不可互换，同一个请求在两个端口上的正确答案不一样，这正是它第 4、5 条场景要验证的东西。它也没有 `TOKEN`——那个域按设计不鉴权。

`render.sh` 六条场景：存活探针（并断言响应里**不出现** `"data"`，即探针没被套上信封）、就绪探针、无 token 得 401、渲染成功且 HTML 带 `k`/`nf`/`nb`/`mi` 类名、语言列表含 python。

`diff.sh` 九条场景：无 token 得 401、四种 hunk 头/标记形态做整值比对、identical 分支、normalize、prose 分段、格式错误的请求体得 400。

`notification.sh` 五条场景：admin 存活探针、用 curl 的**默认** `Content-Type`（即 form-urlencoded 贴在一段真 JSON 上，Phorge 的实际形状）发消息并拿到 fingerprint、`/status/` 的扁平点号键、client 口纯 HTTP 得 501、client 口真握手得 101。第 2 条是这份脚本存在的主要理由，而它的**payload 才是断言的关键**：里面那个 `100% done` 对表单解析器是非法的百分号转义，所以只有「不看头、直接按 JSON 解」的 handler 才会答 200。把那个百分号「清理」掉，这条场景就退化成一个永远通过的检查——理由见 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第 5.4 节。

`mailer.sh` 八条场景：存活探针、就绪探针、无 token 得 401、后端列表、发一封并断言 `data.mailerKey`、带 base64 附件的一封、缺收件人得 400、`mailerKeys` 指向不存在的后端得 502。**它对被测实例有一个额外前提**：必须配了至少一个后端，否则第 2 条按设计就该失败——`/readyz` 报的正是「一个后端都没配」。用 `test` 后端起服务就能满足，`make e2e` 与 compose 的默认值都是它。

`search.sh` 十八条场景，两件事与其余四份不同：

- **它会销毁索引。**第 5 条打 `POST /api/search/init`，而那条路径按设计先删索引再重建。所以它只能对着一次性部署跑，脚本头部与运行时都印了警告。这是唯一一份有破坏性的 e2e。
- **两条场景在内存后端上是 SKIP 而不是 PASS。**第 11、12 条（两字中文查询命中、`登录跳转` 只命中中文文档）是关于 Elasticsearch **分析器链**的断言，而 `test` 后端做的是子串匹配、根本不过分析器——它会把两条都答对而什么都没证明。脚本因此先看 `/backends` 的应答里有没有 `"type":"test"`，是就 skip。**别把那个 skip 改成 pass**：整份脚本存在的主要理由就是这两条，一个看起来像覆盖的假 PASS 比 SKIP 坏得多。

其余十六条覆盖两个探针（含断言探针**不出现** `"data"`）、401、`/backends` 不含凭据、`/init`、`/exists`、刚建完的索引必须 `sane: true`、写入英文与中文文档各一份、英文检索往返、无筛选列表、`authorPHIDs` 筛选、`exclude`、`/stats`（并断言响应里**不出现** `storageBytes`）、缺 `phid` 得 400，以及 `/init` 与 `/sane` 空 `docTypes` 各得 400。它对被测实例的前提与 mailer 相同：至少配一个后端。

内容断言全部**轮询**而不是断言一次，因为 Elasticsearch 的 refresh 是近实时的：写入之后立刻查会漏掉那份文档，一个只断言一次的脚本会大约一半的时候失败。

`file-storage.sh` 十一条场景：两个探针、无 token 得 401、引擎列表、写一个文件并读回来、删除、零字节文件、未知引擎得 400、缺参数、以及一个任何引擎都签发不出的 handle。它对被测实例的前提与 mailer、search 相同：至少配一个后端，本地磁盘最省事。

`webhook.sh` 十一条场景，而它与其余六份最大的不同是**它碰不到本域的主要工作**：投递不由任何请求启动，也没有端点报告一次投递，所以这份脚本能验的只有那两个只读端点、两个探针，加上一条跨端点的不变量。真实的投递链路在 `go/internal/webhook/dispatcher_test.go`——那里能把时钟按住并读出确切的字节，而一个 e2e 脚本两样都做不到。

即便如此它仍有三条别处拿不到的断言：**`activeWebhooks` 永不大于 `hooks.total`**（第 7 条，两次独立查询、两张不同的表，一个把某个 COUNT 写在错误表上的实现在忙碌队列上会产出一组看起来很合理的数字）、**`/api/webhook/hooks` 的响应里不出现 hook 的 URI 或 HMAC key**（第 8 条，那个 key 就是「一次投递可信」的全部依据）、以及 **POST / PUT / DELETE 都不许答 200**（第 10 条，队列的内容归 Phorge，一个能推行进去的端点是给这张表开第二个入口）。它对被测实例的前提比其余六份都硬：**必须有一个可达的 `{namespace}_herald` 库**，没有可退回的本地后端——两个端点都在数行。空队列、零 hook 都没问题，每一条断言都是关于形状与不变量的，不关于具体数字。

`taskqueue.sh` 十五条场景，而它与 webhook 那份最大的不同是**它能跑通本域的主要工作**：队列可写（`enqueue`），所以脚本能入一个任务、把它租回来、complete 掉，再读计数往前走——`archivedCount` 至少 +1（用「单调增长」而非精确算术断言，因为可能有并发 worker 在归档）。这也是它会留痕的原因：那个测试任务落在归档表里，这是对的——队列是一份日志。它用的 task class 是 Phorge 自己的 `PhabricatorTestWorker`，免得游荡的 worker 把它当真活干。前提与 webhook 同样硬：**必须有一个可达的后端（`{namespace}_worker` 库或 Redis）**，没有本地回退——每个端点都碰存储；`/readyz` 不通时脚本直接退出，因为后面每条都会失败。**worker（`gorge-worker`，`/api/worker/stats`）不在这八份里**：它的唯一端点读进程内计数器，一个 e2e 脚本对它无非再验一次探针与鉴权，而那已被 `go/internal/worker/http_test.go` 覆盖。

render、diff、mailer、search、file-storage、webhook 与 taskqueue 七份都在 `TOKEN` 为空时跳过 401 那条并明确打印 SKIP，而不是静默略过。

它填补的是单元测试与契约固件都够不着的地方：真实的 `main()`、真实的监听端口、真实的容器编排。十个 `cmd` 包的覆盖率缺口就靠它兜。（`httpx.Run()` 一度也在这个名单上，现在不在了——见第 5 节。）**但 `cmd/gorge-webhook` 与 `cmd/gorge-worker` 是这句话第一次不完全成立的地方**：那两个 `main()` 里的一半是「起后台 goroutine（webhook 投递 / worker 消费）、收到信号后先排空再关连接池」这段编排，而 e2e 脚本只打 HTTP 端口，看不见它。

`diff.sh` 还有一个单元测试拿不到的作用：`\ No newline at end of file` 这个标记里含反斜杠，是整个 payload 里唯一会被 JSON 转义错误悄悄改坏的部分，而它只有过一趟真实的 HTTP 编解码才验证得到。

## 5. 覆盖率现状

| 包 | 覆盖率 |
|---|---|
| `platform/auth` | 100.0% |
| `platform/config` | 100.0% |
| `platform/health` | 100.0% |
| `platform/httpx` | 99.1% |
| `render` | 91.7% |
| `render/highlight` | 92.9% |
| `diff` | 92.1% |
| `diff/unified` | 100.0% |
| `diff/prose` | 100.0% |
| `notification` | 97.6% |
| `notification/hub` | 83.7% |
| `notification/peer` | 96.2% |
| `mailer` | 79.9%（见下） |
| `search` | 97.5% |
| `search/engine` | 99.5% |
| `search/esquery` | 100.0% |
| `search/engine/elasticsearch` | 83.5% |
| `search/engine/meilisearch` | 91.5%（见下） |
| `filestorage` | 85.2% |
| `webhook` | 70.4%（见下） |
| `conduit` | 85.4% |
| `taskqueue` | 13.4% |
| `worker` | 78.7% |
| `worker/handlers` | 56.8% |
| `dbapi` | 86.9% |
| `contracttest` | 19.0%（见下） |
| `cmd/gorge-render` | 0.0% |
| `cmd/gorge-notification` | 0.0% |
| `cmd/gorge-mailer` | 0.0% |
| `cmd/gorge-search` | 0.0% |
| `cmd/gorge-file-storage` | 0.0% |
| `cmd/gorge-webhook` | 0.0% |
| `cmd/gorge-taskqueue` | 0.0% |
| `cmd/gorge-worker` | 0.0% |
| `cmd/gorge-conduit` | 0.0% |
| `cmd/gorge-db-api` | 3.0% |
| **总计** | **72.4%** |

上表是基线快照。**db-api 迁入后已把它计入**：`internal/dbapi` 一次干净的 `go test ./...` 覆盖率为 **86.9%**（整套现已全绿，`tests/contract/dbapi/` 与 `tests/contract/dbapi/unavailable/` 固件均已落地）；`cmd/gorge-db-api` 只有密码选择辅助函数被单测覆盖，其余入口逻辑与另外九个 `cmd` 同理由 e2e 在集成层兜（`tests/e2e/dbapi.sh` 已落地）。

`httpx` 从 74.1% 升到 97.1%，是 notification 迁入时给 `RunAll` 补的那批测试带来的：原先被认为「要起真进程才测得到」的信号循环与 `Shutdown` 路径，用 `:0` 端口起真 listener 加真 `SIGTERM` 就覆盖到了。剩下的缺口与两个 `cmd` 的 0.0% 都是刻意的：`main()` 起真进程的成本高于收益，由 e2e 在集成层面兜；`httpx` 剩的三处写在 [`platform.md`](platform.md) 第 5 节。

`contracttest` 的 19.0% 仍然主要是**度量假象，不是未测代码**：它自己的 `_test.go` 只直接测两个纯函数（`lookupJSONPath` 与 `assertsStructure`，都是 notification 固件逼出来的），而重放逻辑本身被各域的固件测试每次完整跑过——`go test` 默认只把一个包自己的测试计入该包覆盖率。**不要为了让这个数字变好看而给它补测试**；那两个纯函数值得直接测，是因为它们的寻址与分派规则本身有分支，不是因为数字。真要度量就用 `-coverpkg`。

`webhook` 的 70.4% 缺口是**整块的、而且刚好等于一个文件里的一个类型**：`MySQLStore` 的十个方法与 `OpenDB` 全部 0.0%，包里其余每一处都在 83% 到 100% 之间。taskqueue 的 13.4% 同样主要来自没有真实 MySQL/Redis 的存储边界，不应以 fake 冒充集成覆盖。

这不是「新代码测得差」，是**把 store 抽成 interface 这个决定的直接账单**。域逻辑——claim、两个 cutoff、三个时间窗、payload 字节、签名、失败分类——全部由内存 fake 驱动测到，而那些 `db.QueryContext` / `db.ExecContext` 的包装层没有任何东西碰得到：仓库里没有 MySQL、也刻意不用 sqlmock（惯例是手写 fake，见 2.2）。

**要看清它守住了什么、又没守住什么，得分成两半读**：那几条 SQL 的**形状**是测到的——`TestClaimStatementIsAnOptimisticCompareAndSet` 与 `TestFetchClaimableStatementKeepsItsConditionsSeparate` 直接断言常量字符串里那些 WHERE 条件都还在，而那正是最容易被「顺手简化」掉的东西。没测到的是「驱动照这些字符串跑出来的结果是不是那个意思」——`GREATEST(dateModified + 1, UNIX_TIMESTAMP())` 在秒级精度下真的每次都让版本前进吗，`RowsAffected()` 在两个并发 UPDATE 打同一行时真的只有一个是 1 吗。**这两个问题一个 fake 永远答不了**，因为 fake 实现的是本域**以为**那条 SQL 会做的事。所以这个数字不该用补测试的办法抬——抬它的唯一诚实办法是一次对着真 MySQL 的集成测试，登记在 [`findings.md`](findings.md) 第 42 条。

`mailer` 的 79.9% 缺口同样是**可指名的**：SMTP 的两条发送路径与 SendGrid / Mailgun / Postmark 的 HTTP 往返。永久失败分类本身测到了（`classifyProviderStatus` / `classifySMTPError` 有表驱动用例，sendmail 用 stub 脚本走了真实退出码路径，SES 因为端点可配而用 `httptest` 打了完整一圈），缺的是另外三家 provider 那一圈——它们的端点是编译期常量，测不了。修法与理由写在 [`findings.md`](findings.md) 第 14 条。

`notification/hub` 的 83.7% 有一部分是同一个假象：`Listener` 那几个要真 WebSocket 才调得到的方法，连接建在 `internal/notification` 的测试里，不计入 `hub`。`-coverpkg` 合并度量后它们都是 100%，覆盖率的真实缺口只剩三处，都登记在 [`findings.md`](findings.md) 第 9 条。

**总计现在是 72.4%。**后续迁入的 webhook、taskqueue、worker、conduit 与 db-api 同时带来了大量数据库、Redis、后台循环和进程入口边界；其中 taskqueue 的生产存储实现是当前最主要的降幅来源，性质与 webhook 的 MySQLStore 缺口相同。

**两次要分开读，因为处置方式相反。**search 那次拉低总数的是两个包，缺口在「没写的测试」上，补是可行的（第 17 条给了照抄的模板）；webhook 那次的缺口在「测不到的边界」上——`MySQLStore` 那十个方法的 0.0% 不是有人偷懒，而是仓库里没有 MySQL，而 fake 恰好证明不了那几条 SQL 真的按它们写的那样跑。前者该补，后者补了反而更坏：一个用 fake 覆盖到 100% 的 store 层看起来防线更厚，实际什么都没多守住。

search 那次拉低总数的两个包：

- `search/engine/meilisearch` 当时 **0.0%**，全仓库唯一一个零覆盖的非 `cmd` 包。测试后来照着 Elasticsearch 那份补上了，现在 91.5%。
- `search/engine/elasticsearch` 81.4%（现在 83.5%），缺口是**真实的 HTTP 往返分支**：`httptest` 假集群覆盖了路径拼接、spec 形状与 `configDeepMatch` 的判定，没覆盖的是主机健康表在多主机 failover 下的那几条状态迁移。

**这两个数字不该被「顺手补测试」抹平。** 一个假集群能验证的是「本服务发出了正确的请求」，验证不了「Elasticsearch 会怎么回答」——后者是 `tests/e2e/search.sh` 的活，而它只在有真集群时才有意义。

**而这句话后来以最直接的方式得到了印证，值得当成本节的结论读。**两个后端的覆盖率都补上去之后，第一次把服务对着真后端跑，**两边各暴露一个从建索引/第一个查询就失败的缺陷**，而两边的单元测试都是绿的：

- Meilisearch：`exclude` 用 `id` 过滤，而 `id` 没被声明为 filterable，每个带 `exclude` 的查询 400。两条相关测试一条只看渲染出的字符串、一条拿同一个函数当实际值与期望值比对——**同一份误解的两侧**（[`findings.md`](findings.md) 第 17 条）。
- Elasticsearch：mapping 按文档类型分 key，这在 ES 6 起就被拒绝，ES 7 上 `POST /api/search/init` 直接 `mapper_parsing_exception`。假集群对什么请求都答 200，所以它验不到（[`findings.md`](findings.md) 第 46 条）。同时查出 `exclude` 用的 `not` 查询在 ES 5.0 就已移除——**它在本后端支持的每个版本上都从未真的排除过任何东西**，且完全没有测试覆盖。

所以第 4 节那九份 e2e 脚本不是「单元测试的补充」，在这两个后端上它们是**唯一**能问出问题的地方。`deploy/compose/demo/` 存在的理由正是这个：它把生产编排刻意留给使用者自建的后端一起拉起来，让 `search.sh` 那 18 条能真的对着 ES 7.17 与 Meilisearch 各跑一遍。

生成报告：

```bash
make cover     # 写出 go/coverage.html 并打印 func 级明细
```

普通 CI 只执行测试，不生成或写回覆盖率报告。`.github/workflows/test-report.yml` 在手动触发或推送 `YYYY.MM.DD-rN` 发布标签时调用 `soulteary/go-test-report-action`，将 Markdown、SVG、JSON 和原始测试结果作为 Actions 制品保留；工作流显式设置 `commit: false`，不会产生机器人报告提交。
