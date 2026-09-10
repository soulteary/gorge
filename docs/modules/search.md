# search 模块

替 Phorge 做全文索引与检索：收下一份文档写进配置好的存储，收下一个查询把命中的 PHID 列表还回去。独占 `gorge-search` 这个二进制与 `:8120` 这个端口。

| | |
|---|---|
| 二进制 | `gorge-search` |
| 端口 | `:8120` |
| 包 | `go/internal/search/` |
| 契约 | [`api/openapi/search.yaml`](../../api/openapi/search.yaml) |
| 固件 | `tests/contract/search/` + `tests/contract/search/unavailable/` |
| 兼容约束 | [`compat/phorge/README.md`](../../compat/phorge/README.md) 第七节 ← **改动前必读** |

## 1. 职责边界

**负责**：把一份 `PhabricatorSearchAbstractDocument` 交给存储，把一个 `PhabricatorSavedQuery` 翻译成存储的查询语法，以及索引本身的生命周期（建、查在不在、查配置有没有过期、报统计）。两个后端：Elasticsearch 与 Meilisearch，外加一个内存 `test` 后端。

**不负责**：**它不持有索引**。索引在 Elasticsearch 或 Meilisearch 里，那是一份有卷、有内存配额、有自己升级路径的存储，本服务只是它前面的一层协议翻译。`deploy/compose/docker-compose.yml` 因此**没有**声明 ES/Meili 容器——把存储埋进服务层的编排文件，会让 `docker compose down -v` 变成一种丢索引的方式。

**它也不返回文档内容**，只返回 PHID。这不是省流量，是**策略检查的位置**：Phorge 拿到 PHID 之后用自己的 Query 类去加载对象，那一步才会套上可见性策略。一个会返回正文的检索后端等于绕过了策略层，把用户看不到的对象直接发出去。

**有外部依赖**，这是它与 render / diff 的结构性差异，也是它单独占一个进程的原因：它持有进程内状态（Elasticsearch 扇出的主机健康表）、要连出去打 ES/Meili、并且有一个真实的就绪条件可报。按 [`../architecture.md`](../architecture.md) 的分界，它与 notification、mailer 归在同一侧。

## 2. 路由与依赖

```go
func RegisterRoutes(app fiber.Router, deps *Deps) {
	g := app.Group("/api/search")
	g.Use(auth.Token(deps.Token))

	g.Post("/index", indexDocument(deps))
	g.Post("/query", searchQuery(deps))
	g.Post("/init", initIndex(deps))
	g.Get("/exists", indexExists(deps))
	g.Get("/stats", indexStats(deps))
	g.Post("/sane", indexIsSane(deps))
	g.Get("/backends", listBackends(deps))
}
```

| 方法 | 路径 | 对应 PHP 方法 | 鉴权 |
|---|---|---|---|
| POST | `/api/search/index` | `reindexAbstractDocument()` | 需要 |
| POST | `/api/search/query` | `executeSearch()` | 需要 |
| POST | `/api/search/init` | `initIndex()` | 需要 |
| GET | `/api/search/exists` | `indexExists()` | 需要 |
| GET | `/api/search/stats` | `getIndexStats()` | 需要 |
| POST | `/api/search/sane` | `indexIsSane()` | 需要 |
| GET | `/api/search/backends` | `getBackends()`，**目前没有调用者** | 需要 |
| GET | `/`、`/healthz`、`/readyz` | setup check | 不需要（平台层注册） |

`/backends` 那一行要说明白：`PhabricatorGorgeSearchClient::getBackends()` 定义了，但 PHP 侧没有任何地方调它。集群面板那一页确实会打本服务，但打的是 `/stats`；后端**那几列**来自 `PhabricatorGorgeSearchHost::getStatusViewColumns()`，而那个方法只读本地的 `cluster.search` 配置，一个 HTTP 请求都不发。所以这是一条**诊断端点**——它回答的是「跑着的服务自己认为它有哪些后端」，与「配置文件里写了什么」是两个问题，而这是唯一能把两者分开的办法。凭据不出现在它的响应里这条约束照样要守，理由换成诊断输出会进工单、日志与支持邮件，而不是「会被打印在一个网页上」。

`Deps` 两个字段：`Engine` / `Token`。`TestRoutePathsAreStable` 断言这七条路径仍注册着——PHP 侧 `PhabricatorGorgeFulltextStorageEngine` 按字面调它们。

迁入时从旧 `internal/httpapi/handlers.go` **删掉了四样被平台层取代的东西**：本地的 `apiResponse` / `apiError` 信封、`tokenAuth()`、`healthPing()`，以及 `GET /`、`/healthz`、`/readyz` 三条注册。最后这一条是硬性的：`httpx.New()` 已经注册过它们，域包不能再次注册同一组平台路由。

`main.go` 与 mailer 的骨架只差一行：

```go
srv := httpx.New(httpx.Config{
	ListenAddr: cfg.ListenAddr,
	Ready:      se.Ready,
})
```

**`/readyz` 是本域唯一一处比 `/healthz` 多说了点什么的地方。** 就绪判据只有一条：**至少有一个后端带 `read` 角色**。这精确对应「服务活着、每一次检索都失败」这个状态——迁入前 `/readyz` 与 `/healthz` 是同一个 `healthPing()`，零后端时照样 200，compose 报 healthy。

刻意**不**在 `Ready` 里拨测 ES/Meili，理由与 mailer 第 6.5 条相同：拨测会让就绪状态随第三方抖动翻转，而主机健康表与 failover 链本来就是为吸收这种抖动存在的。所以 200 的含义是「本服务能发起一次检索」，不是「下一次检索会成功」。反过来说，**这里的 503 永远意味着「配错了」，不意味着「Elasticsearch 抽了一下」**，PHP 侧 setup check 可以据此直接给出结论。

## 3. 核心实现

### 3.1 四个子包

```
internal/search/
├── config.go                 配置装配 + NewEngine
├── http.go                   七条 handler
├── esquery/                  bool query 构造器 + Phorge 的 4 字符常量
└── engine/
    ├── backend.go            SearchBackend 接口 + BackendDef
    ├── engine.go             SearchEngine：按角色扇出
    ├── test.go               内存后端（可注入失败）
    ├── elasticsearch/
    └── meilisearch/
```

`BackendDef` 放在 `engine/` 而不是配置层，是为了断开一个循环：`engine` 需要它来描述后端，配置层需要它来解析 JSON，放在配置层则 `engine` 要反向依赖配置。

`engine/test.go` 是**生产代码而不是 `_test.go` 辅助函数**，这不是放错了地方：它的可注入失败是五个域级错误码在契约固件里唯一的到达路径（一个真后端没法被要求「按需出故障」），而固件在 `tests/contract/search/` 下、供不同实现共读，所以那个后端必须能被非测试代码构造出来。它同时也是 compose 默认值与「配置写好之前先验通链路」这个工作流的实现。

`engine/meilisearch/` 已有独立的 `backend_test.go`，覆盖配置、请求形状、响应解析和过滤属性之间的约束。它仍不能代替对真实 Meilisearch 的兼容验证：HTTP fake 只能证明本域认为对端会如何响应，无法证明目标版本确实接受这些请求。第一次真实联调暴露的问题及补上的跨函数约束测试登记在 [`../findings.md`](../findings.md) #17；升级 Meilisearch 或调整查询、索引设置时，应当继续跑真实后端联调。

### 3.2 SearchEngine：读写扇出方式不同，这个不对称是有意的

- **写**打到**每一个**带 `write` 角色的后端，任何一个失败都会被报告。两个写后端意味着两份索引同步推进，这是在不停机的前提下另起一份索引的唯一办法。
- **读**打到**第一个**带 `read` 角色且答得出来的后端，其余的是 failover 链。
- **`IndexIsSane` 只问第一个**：任何一个后端答 false 都指向同一个结论——重建索引，多问几个只会把一个明确的答案变成一个没人知道该怎么处理的部分答案。
- **`IndexStats` 跳过失败的后端**而不是报告失败，因为它喂的是状态面板，那里第二意见比错误有用。

**「一个可写后端都没有」是错误，不是空操作。** 这是本域最容易犯的那种错：对一个什么都没存下的请求答 `"indexed"`，会让一次大库全量重建索引跑到结束、每一份文档都报成功、而索引是空的。

**这也是为什么 Phorge 的 `cluster.search` 里应当只配一个 gorge 条目。** Phorge 自己也有 read/write 角色与多 ref 的 failover，两层都配等于把扇出规则写了一半在这边一半在那边，下一个人只会读到其中一半。扇出与 failover 全部交给 Go 侧。

### 3.3 CJK：迁入时补上的能力，也是唯一一处「删掉不报错」的改动

旧实现的三条分析器 `english_exact` / `letter_stop` / `english_stem` **全是英文链**，其中 `letter` tokenizer 对中文基本失效。迁入时加了一条 CJK 链，只用 Elasticsearch **内置**的 filter，不引入 `analysis-icu` 或 `smartcn` 插件：

```go
filterCJKBigram: map[string]any{"type": "cjk_bigram", "output_unigrams": true},

analyzerCJKText: map[string]any{
	"tokenizer": "standard",
	"filter":    []string{"cjk_width", "lowercase", filterCJKBigram},
},
```

`titl` / `body` / `cmnt` 三个语料字段各多一个 `cjk` 子字段（与既有的 `raw` / `keywords` / `stems` 并列）。`output_unigrams: true` 让单字查询仍能命中，否则「登录」这样的二元切分会把单字查询整个漏掉。

查询侧**另起一条 should 子句**而不是往既有那条上加字段，因为**分析器是子句的属性而不是字段的属性**：`english_exact` 把中文切成单字，用它去打一个按二元组建索引的字段，评分基本等于随机。

三件配套的事：

1. **改 mapping 会让所有既有索引报 not sane**，必须 `bin/search init` + `bin/search index --all --force` 重建。这一条会**明确报 false**而不是静默失效，属于可接受的迁移代价，但必须写进 DOCKER.md。登记在 [`../findings.md`](../findings.md) #18——登记的不是这一次，是**下一次**：任何一处动 `buildIndexConfig()` 的改动都有同样的后果。
2. **`bin/search ngrams` 与本引擎无关**。那是 Ferret（MySQL）专属路径，`PhabricatorSearchNgrams` 与 `PhabricatorFerretEngine` 全在 MySQL 侧。旧 `phorge/DOCKER.md` 里那套「跑 ngrams 启用中文搜索」的说法在这个引擎下是误导，不要照抄。
3. **`cjk` 子字段的存在本身是兼容契约的一部分**，见 [`compat/phorge/README.md`](../../compat/phorge/README.md) 第 7.5 条。删掉它之后每一条路径都照常答 200，只有中文检索静默退化。

### 3.4 文档在索引里是平铺的

`buildDocSpec()` 把一份文档摊平：每个 field 与 relationship 变成一个以其**四字符常量**命名的顶层键，同名的累积成列表，带时间戳的 relationship 额外写一个 `<name>_ts`。所以 `titl`、`cmnt`、`auth`、`ownr` 这些名字不是缩写风格，**它们就是索引里的键**，与 `PhabricatorSearchDocumentFieldType` / `PhabricatorSearchRelationship` 的常量值逐字符相等。用一种拼法写进去、用另一种拼法查，两边都不报错，只是永远查不到。

常量集中在 `esquery/builder.go` 一处，`TestNamesMatchThePHPConstants` 逐条钉住。

### 3.5 Elasticsearch 后端的主机健康表

`health map[string]bool`，每主机一个标记而不是整体一个 up 标记，读请求在健康主机里随机挑一个。两条规则值得知道：

- **主机初始为健康**。标记成未知会让一个新进程的第一个请求直接失败在「no healthy hosts」上，而那时集群完全正常。
- **只有传输层失败与 5xx 会把主机标记为不健康。** 4xx 是请求的问题，不是主机的问题；跟着 4xx 摘主机，一个写错的查询能在几次请求里把整个集群摘空。

`version` 默认 5，它决定四件事：时间戳字段名（`< 2` 用另一个名字）、文本字段类型（`>= 5` 用 `text`，否则 `string`）、`IndexExists` 走 `_stats` 还是 `_status`，以及下一节那件比前三件都重要的事。

### 3.6 `version` 决定文档类型放在哪儿，而这不是一个可以填错的值

前三个版本差异都是"某个名字换一种拼法"。这一个不是：它决定**文档类型是索引结构的一部分，还是文档上的一个字段**。

Elasticsearch 6 把一个索引收紧到只能有一个 mapping type，7 起把 type 整个移除了。而 Phorge 要索引七种文档类型，所以从 6 开始它们不可能各占一个 mapping type。三种形状：

| `version` | mapping | 写入 URL | 类型如何收窄检索 |
|---|---|---|---|
| `< 6` | 每个文档类型一份，内容完全相同 | `/{index}/{TYPE}/{phid}` | URL 里列出类型 |
| `6` | 一份，嵌在 `_doc` 下 | `/{index}/_doc/{phid}` | `docType` 过滤 |
| `>= 7` | 一份，`properties` 直接在根上 | `/{index}/_doc/{phid}` | `docType` 过滤 |

`< 6` 那一列刻意与 Phorge 自带引擎逐字段相同——那份冗余不是疏忽，是为了让同一个索引同时满足两边的 sanity check。从 6 开始类型变成文档上的 `docType` keyword 字段（名字与 Meilisearch 后端用的属性一致），URL 里的类型段换成 `_doc`，而 URL 里那道"只搜请求的类型"的限制**移进了查询**，成为一个 `docType` 的 terms 过滤。

**这一项填错的代价不对称，而且两个方向都不温和：**

- 对着 ES 7 集群填 5，多 type mapping 会被 `mapper_parsing_exception` 直接拒掉。索引根本没建出来，之后每一次检索都失败——`/init` 就报错，所以至少它是响的。
- 反过来（对着 ES 5 集群填 7）更糟：`_doc` 在 ES 5 上是一个合法的 type 名，索引建得出来、文档写得进去，但类型收窄会依赖一个 Phorge 自带引擎不认识的字段。

`ES_VERSION` 因此**必须与集群的真实主版本一致**。`backend_test.go` 里 `TestVersion5KeepsAMappingPerDocumentType`、`TestVersion7HasNoMappingTypes`、`TestVersion6NestsASingleMappingType` 三条各钉一种形状。

还有两处同源的版本差异值得一起记住，它们都属于"旧写法在新集群上不是被忽略而是被拒绝"：

- **`include_in_all`**：`_all` 在 6.0 随之移除，mapping 里再提它会被拒。所以它只在 `< 6` 写出来——而在 5 上必须写，否则对着 Phorge 自带引擎的 sanity check 会报不一致。
- **`not` 查询**：`exclude` 曾经用 `{"not": {"ids": ...}}`，而 `not` 在 2.0 弃用、**5.0 移除**。也就是说它在本后端支持的每一个版本上都只会换回一个解析错误，从来没有真的排除过任何东西。现在用 `bool.must_not`，它从 1.x 起语义未变，所以这一处不需要版本分支。

## 4. 配置

| 变量 | 默认值 | 说明 |
|---|---|---|
| `GORGE_LISTEN_ADDR` | `:8120` | 监听地址 |
| `GORGE_SERVICE_TOKEN` | 空 | 服务间认证 token，为空则不鉴权 |
| `GORGE_SEARCH_BACKENDS` | 无 | 后端列表，JSON 数组 |
| `GORGE_SEARCH_CONFIG_FILE` | 无 | JSON 配置文件路径 |
| `GORGE_SEARCH_ENGINE` | `elasticsearch` | 单后端速配时用哪个引擎 |

规范变量命名规则见 [`../platform.md`](../platform.md) 第 4 节。

各后端选项另有一批扁平变量，**原样保留不加前缀**（与 mailer 保留 `SMTP_HOST` 同理：它们命名的是后端的设置，不是本服务的设置）：

| 变量 | 默认值 | |
|---|---|---|
| `ES_HOST` | 无 | 逗号分隔，**设了它才会拼出后端** |
| `ES_INDEX` | `phabricator` | |
| `ES_VERSION` | `5` | 必须与集群真实主版本一致，它决定 mapping 形状——见 3.6 节 |
| `ES_TIMEOUT` | `15` | 秒 |
| `ES_PROTOCOL` | `http` | |
| `MEILI_HOST` / `MEILI_INDEX` / `MEILI_MASTER_KEY` / `MEILI_TIMEOUT` / `MEILI_PROTOCOL` | 同上 | |

`Load()` 的取值顺序与另外两个域一致：指了配置文件就读文件，否则读环境变量；走文件时**仍然从环境变量取 `GORGE_SERVICE_TOKEN`**。

### 「没配后端」是一个受支持的状态

服务照常启动、`/healthz` 答 200、`/readyz` 答 503。这是刻意的：一个部署在配置写好之前必须能起得来，否则编排会陷在启动循环里。

`GORGE_SEARCH_BACKENDS` **解析失败落在同一个地方**——没有后端、not ready——外加一条 `slog.Error`。这里没有报错退出，代价是「还没配」与「配错了」在 `/readyz` 上长得一样，只有日志能分开它们。登记在 [`../findings.md`](../findings.md) #19。

## 5. 兼容契约

权威描述在 [`compat/phorge/README.md`](../../compat/phorge/README.md) 第七节，这里是概述。五条约定：

1. **七条路径**按域命名，`PhabricatorGorgeFulltextStorageEngine` 按字面调用。
2. **wire 字段名一律 camelCase**（`dateCreated`、`relatedPHID`、`authorPHIDs`……），声明在 [`go/internal/contracts/search.go`](../../go/internal/contracts/search.go)。唯一的例外是统计里的 `storage_bytes`，它早于 monorepo、PHP 侧按这个拼法读，改成 `storageBytes` 会让集群面板的存储列变空且不报错。
3. **四字符字段与关系名**与 PHP 常量逐条对齐，见 3.4。
4. **默认索引名 `phabricator`**，与 Phorge 自带的 Elasticsearch 引擎一致。
5. **`cjk` 子字段的存在本身**，见 3.3。

**本域最要紧的一条与 mailer 不同**：mailer 那条会改变 Phorge 的行为，本域这五条**全部是「破坏后不报错」型**。第 3、5 两条尤其安静——索引写得进去、查询答得出来、状态码全是 200，只是查不到东西。

## 6. 域级错误码

五个，都是迁入前就有、Phorge 侧已经在用的码，定义在 `internal/search/http.go`：

| 码 | 状态 | 含义 |
|---|---|---|
| `ERR_INDEX_FAILED` | 502 | 写入失败 |
| `ERR_SEARCH_FAILED` | 502 | 所有可读后端都答不出来 |
| `ERR_INIT_FAILED` | 502 | 建索引失败 |
| `ERR_CHECK_FAILED` | 502 | `exists` 或 `sane` 问不到 |
| `ERR_STATS_FAILED` | 502 | 没有后端能给出统计 |

**全部 502 而不是 500。** 500 在本域只意味着「服务自己出了问题」，后端出问题一律 502——答 500 会把排查的人送去看错误的日志。

**五个而不是一个**，因为调用方对它们的反应不同：`bin/search` 处理「建索引失败」和「查询失败」的位置完全不同，收敛成一个只会让运维拿到一句「搜索服务返回了 502」。

**但这个区分目前在 PHP 的异常分支上没有兑现。**`PhabricatorGorgeSearchClient` 没有覆盖 `newServiceErrorException()`（`PhabricatorGorgeMailerClient` 覆盖了，因为 `ERR_PERMANENT_FAILURE` 必须与 `ERR_SEND_FAILED` 分道），所以走这个客户端的十一个码全部塌成同一个通用异常，码只作为文本活在异常消息里。这是**刻意保留**的行为，登记在 [`../findings.md`](../findings.md) #21。五个码的价值兑现在另外两个消费方上——`bin/search` 的输出，与运维读 502 响应体时看到的那个字符串——所以**不要据此把它们收敛成一个**，也不要在这里新增一个「PHP 必须按码分支」的码而不同时覆盖 `newServiceErrorException()`。

三个码的存在本身是在防一种更坏的答案：

- `ERR_SEARCH_FAILED` 防的是「200 + 空列表」。那与「什么都没匹配上」无法区分，Phorge 的搜索页会为一次故障渲染空状态——没人会去查的那一种结果。
- `ERR_CHECK_FAILED` 防的是把「问不到」答成 `exists: false` 或 `sane: false`。这两个读数都会让运维去重建索引，而重建是本域代价最大、也最具破坏性的操作，绝不该由一次连接失败触发。
- `ERR_STATS_FAILED` 防的是状态面板把缺失的数字渲染成 0 文档。

**新增域级错误码时加在自己的域包里，不要塞进 `platform/httpx`。**

### 一处刻意收紧：`/sane` 现在要求 `docTypes`

迁入前空的 `docTypes` 会被接受。它的后果比 `/init` 上同样的疏漏更坏：sanity check 拿「本服务今天会为这批类型建出的配置」去比对线上索引，空类型列表建出的是一份空期望，**任何索引都满足它**——包括一份没有 mapping、没有 `cjk` 子字段的索引。答案会是一个自信的 `sane: true`。现在两条路径都答 400，记在 [`../findings.md`](../findings.md) #20。
