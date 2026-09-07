# file-storage 模块

替 Phorge 存放文件的**字节**：收一个原始请求体，写进配置好的后端中第一个接受它的那个，再按 handle 原样交回去。独占 `gorge-file-storage` 这个二进制与 `:8100` 这个端口。

| | |
|---|---|
| 二进制 | `gorge-file-storage` |
| 端口 | `:8100` |
| 包 | `go/internal/filestorage/` |
| 契约 | [`api/openapi/file-storage.yaml`](../../api/openapi/file-storage.yaml) |
| 固件 | `tests/contract/file-storage/`（14 份） |
| 兼容约束 | [`compat/phorge/README.md`](../../compat/phorge/README.md) 第八节 ← **改动前必读** |

## 1. 职责边界

**负责**：把一个文件的字节收下来、放进某个后端，并如实报告「放进了哪个后端、拿什么 handle 取回来」；以及按这对 `(engine, handle)` 把字节交回去、删掉。三个后端——MySQL blob、本地磁盘、S3——按优先级排成写入候选链。

**不负责**：文件的**元数据**。名字、大小、MIME 类型，以及那对 `(engine, handle)` 本身，全都存在 **Phorge 自己的数据库**里。本服务不认识「文件」这个概念，只认识「一段字节」和「一个不透明的 handle」——handle 只对铸出它的那个引擎有意义，所以读和删都必须同时给出引擎名，服务端不做推断。

也不负责去重、垃圾回收策略与分块。去重与 GC 是 Phorge 的 `PhabricatorFile` 与它的 GC daemon 的事，本服务连「哪些 handle 还有人引用」都不知道；**分块则是 Phorge 的 chunked storage engine 在上游就做完了**——超过 8 MB 的文件在到达这里之前已经被切成 4 MB 的块，每一块走一次独立的 `POST`。这也是第 2 节那个 `16M` 传输上限的由来：它不是文件大小上限，是「一次合法请求最大能有多大」的两倍余量。

**有外部依赖**，与 mailer 同属一类，也是它单独占一个进程的原因：本地磁盘要挂卷、blob 要连数据库、S3 要连对象存储，并且有一个真实的就绪条件可报。它比 mailer 更进一步——**这是仓库里第一个打开数据库连接的二进制**。

## 2. 路由与依赖

```go
func RegisterRoutes(e *echo.Echo, deps *Deps) {
	g := e.Group("/api/file")
	g.Use(auth.Token(deps.Token))

	g.POST("/blob", writeBlob(deps))
	g.GET("/blob", readBlob(deps))
	g.DELETE("/blob", deleteBlob(deps))
	g.GET("/engines", listEngines(deps))
}
```

| 方法 | 路径 | 鉴权 | 成功响应 |
|---|---|---|---|
| POST | `/api/file/blob` | 需要 | 信封，`{handle, engine, size}` |
| GET | `/api/file/blob` | 需要 | **原始 `application/octet-stream` 字节，不套信封** |
| DELETE | `/api/file/blob` | 需要 | 信封，`{status: "deleted"}` |
| GET | `/api/file/engines` | 需要 | 信封，按写入顺序排列的后端数组 |
| GET | `/`、`/healthz`、`/readyz` | 不需要（平台层注册） | 裸 `{"status":"ok"}` |

**三个方法压在同一条路径上，而 handle 走查询参数而不是路径段**，这两件事是同一个原因：本地磁盘的 handle 是 `ab/cd/{28 hex}`、S3 的 handle 是 `phabricator/ab/cd/{16 hex}`，**handle 里带斜杠**。做成路径段就要求每一个客户端都记得转义它，而漏转义的表现是 404 而不是报错。`TestHandlesWithSlashesSurviveTheQueryString` 钉住这一条，两种斜杠写法各跑一遍——`PhutilURI` 会把它转义成 `%2F`，而 curl 与 e2e 脚本原样发送，两种都合法且必须解出同一个 handle；`TestRoutePathsAreStable` 钉住四条路径本身——`PhabricatorGorgeFileStorageClient` 已经在按字面调它们。

`Deps` 只有两个字段：`Router` 与 `Token`。没有 `BodyLimit`——mailer 需要它是因为正文截断发生在 handler 里，而这里的大小判据来自各引擎自己的 `MaxFileSize()`，传输上限则整个交给平台中间件。

`main.go` 与另外三个二进制的差别是那两行 `httpx.Config`，外加一次显式关闭：

```go
srv := httpx.New(httpx.Config{
	ListenAddr: cfg.ListenAddr,
	BodyLimit:  filestorage.TransportBodyLimit, // "16M"，文件是裸请求体，平台默认 2M 会把每一次上传都封在 2 MB
	Ready:      router.Ready,
})
```

```go
runErr := srv.Run()
if closeErr := router.Close(); closeErr != nil { … }
```

`Close` 是**显式调用而不是 `defer`** 的：下面那条失败分支要 `os.Exit`，而 `os.Exit` 不跑 `defer`——这里唯一值得在退出路径上释放的就是那个连接池。

## 3. 核心实现

### 3.1 写：一道前置检查，两条分支

```
POST /api/file/blob
  ├─ Content-Length < 0            → 400 ERR_BAD_REQUEST
  ├─ ?engine= 指名了引擎（WriteTo）
  │    ├─ 引擎不存在               → 400 ERR_BAD_REQUEST
  │    ├─ 超过该引擎的 MaxFileSize → 413 ERR_TOO_LARGE
  │    └─ 写失败                   → 500 ERR_INTERNAL（不回退到别的引擎）
  └─ 未指名（Write）
       ├─ 没有候选引擎             → 503 ERR_NO_ENGINE
       └─ 所有候选都失败           → 500 ERR_INTERNAL
```

**必须有 `Content-Length`，没有就 400。** 两个理由都是硬的：S3 要用长度签请求，不给长度 SDK 就得把整个 body 缓冲起来算长度；而所有后端的大小限额都得在读第一个字节**之前**判掉，否则「限额」只是「读完 20 MB 再拒绝」的另一种写法。真实调用方都会带——Phorge 的 `HTTPSFuture` 与 curl 对内存里或磁盘上的 body 都设这个头——所以拒绝一个不带长度的请求，比替它悄悄缓冲要好。`TestWriteBlobRequiresAContentLength` 用一个 `httptest` 认不出长度的 reader 走这条路。

**指名引擎时不回退。** Phorge 在「这个文件的引擎已经记录在案、现在要重写它」时才指名，把字节悄悄落到别处会让那条记录指向不存在的东西。`TestRouterWriteToDoesNotFallThrough` 守住它。

**超限在 handler 里判成 413 而不是留给引擎。** 引擎自己也会拒（blob 引擎的两道大小检查见 3.2），但那是一个普通 error，会被平台错误处理器变成 500——一个调用方无从下手的状态码。在 handler 里判掉，「文件对这个后端太大」就和仓库里其它每一处大小拒绝一样答 413 `ERR_TOO_LARGE`。

**`ERR_NO_ENGINE` 与 500 是两句不同的话**，这是本域唯一的域级错误码存在的全部理由：503 说的是**一次都没试**（没配后端，或者这个大小没有任何后端收），500 说的是**试了并且坏了**。前者运维去改配置，后者运维去看日志。`TestRouterEveryEngineFailingIsNotErrNoEngine` 专门守着这两者不许混。

### 3.2 三个后端与那个接口

```go
type StorageEngine interface {
	Identifier() string   // 存进 Phorge 数据库的字符串，不能改
	Priority() int        // 写入候选顺序，小的先试
	CanWrite() bool
	HasSizeLimit() bool
	MaxFileSize() int64
	WriteFile(ctx, src io.Reader, size int64, params WriteParams) (handle string, err error)
	ReadFile(ctx, handle string) (rc io.ReadCloser, size int64, err error)
	DeleteFile(ctx, handle string) error
}
```

| 引擎 | `Identifier()` | 优先级 | 大小限额 | handle 形态 | 是否流式 |
|---|---|---|---|---|---|
| MySQL blob | `blob` | 1 | `MySQLBlobMaxSize`（默认 1 MB） | 自增行 id | **否**，整行缓冲 |
| 本地磁盘 | `local-disk` | 5 | 无 | `ab/cd/{28 hex}` | 是 |
| S3 | `amazon-s3` | 100 | 无 | `phabricator[/{instance}]/ab/cd/{16 hex}` | 是 |

优先级的含义是「小文件优先落进数据库（Phorge 自己的备份已经覆盖它），其余落磁盘，有对象存储时也只在它是唯一后端时才用到 S3」。**三个 identifier 与三种 handle 形态都是兼容契约**，见第 5 节。

三处实现细节值得单独知道：

- **blob 引擎的大小限额检查两遍。** 一遍打在调用方声明的 `Content-Length` 上（这样超大文件一个字节都不读），一遍打在实际读到的字节数上（`io.LimitReader(src, maxSize+1)`）——**声明的长度是一个说法，不是一个承诺**。`TestMySQLBlobRefusesABodyLongerThanItsDeclaredLength` 守后一遍。
- **本地磁盘的 handle 格式在读和删的时候都要校验，这是安全边界不是整洁癖。** handle 从查询参数进来，而 `filepath.Join(root, handle)` 会老老实实把 `../../..` 解析掉。`localHandlePattern` 是唯一挡住「读走这个进程能打开的任意文件」的东西，`TestLocalDiskRejectsBadHandle` 把 `../../../etc/passwd` 这一类逐个试过。写失败时会把半截文件删掉——它的 handle 从没交给任何人，留着就是永远没人读也永远没人删。
- **S3 的写路径声明 payload 未签名。** SigV4 默认要读一遍 body 算 SHA256 再 seek 回开头，而 HTTP 请求体不能 seek，于是明文 HTTP 端点（自建 MinIO / Ceph 的常态）上流式上传会直接失败在 `request stream is not seekable`。SDK 在 HTTPS 上本来就走「声明未签名」这条路，`s3.go` 的 `unsignedPayload` 把同一个选择延伸到明文 HTTP，且只加在 `PutObject` 上。另一条路是把每次上传缓冲下来——那正是改成流式要消掉的东西。记在 [`../findings.md`](../findings.md) 第 25 条。

### 3.3 Router：优先级顺序、写入回退，与那个 rewind 预算

`Write` 先筛候选（`CanWrite()` 为真、且文件不超过该引擎的限额），再按优先级升序逐个试，**某个引擎失败时换下一个**：

```
按 priority 升序遍历候选
  ├─ 成功        → 返回 {handle, engine, size}
  └─ 失败        → 记 FILE_WRITE_FAILED，Rewind 后试下一个
       └─ Rewind 失败 → 记 FILE_WRITE_NOT_RETRYABLE，整体失败
```

**「失败也回退」而不是「只按大小回退」，是迁入时补上的**，也是 Phorge 自己的 `buildFromFileData` 一直以来的行为。它要接住的场景很具体：`bin/storage upgrade` 还没跑过时 `file_storageblob` 这张表不存在，于是每一次 blob 写入都失败——这时上传必须落到本地磁盘，而不是整个失败掉。`TestRouterFallsThroughOnWriteFailure` 用的正是 `Table 'phorge_file.file_storageblob' doesn't exist` 这个错误。

**但字节只存在一次，所以回退有预算。** `rewindReader` 记录已读过的字节以便第二个引擎重放，一旦读过的量超过预算就把记录整个丢掉并标记为不可回退——**留一个前缀比不留更坏**，那会让重放存下一个截断的文件。

预算是**推导出来的，不是可调参数**：它等于各引擎里最大的那个大小限额（默认就是 blob 引擎的 1 MB），也就是「那个不肯流式、无论如何都要整读的引擎最多会读多少」。`TestRouterBudgetFollowsTheSizeLimitedEngines` 断言这条推导：有 blob 就是 blob 的限额，没有带限额的引擎就是 0，此时 `Write` 退化成单次尝试——那也正是迁入前的全部行为。

这个折中要说清楚，因为它**不覆盖**的那一类同样真实：**本地磁盘或 S3 写到一半失败是任何预算都救不回来的**。它们是流式的，失败时字节早已流走；而且它们后面本来也没有第四个引擎可以接。`TestRouterStopsWhenTheFailedWriteConsumedTheBody` 就是把这个边界钉死的用例——它断言的不只是「整体失败」，还有「下一个引擎一个字节都没收到」。

### 3.4 读答原始字节，删除幂等

**成功的 `GET /api/file/blob` 是全仓库 `/api/**` 里唯一不套 `{data, error}` 信封的成功响应**：`c.Stream(200, "application/octet-stream", rc)`。失败仍然是信封。所以客户端的判据只能是**状态码**：200 就把 body 当文件，其余就交给信封解析器。**不能拿「body 是不是空的」当判据**——0 字节文件是一个合法的 200 加空 body。平台层为此**没有**改任何代码，理由见 [`../platform.md`](../platform.md) 第 1.1 节。

引擎知道长度时会带上 `Content-Length`（`size >= 0`），这是客户端区分「完整文件」与「被截断的文件」的唯一依据；引擎不知道长度时（`ReadFile` 返回 `-1`）就干脆不带，让响应走 chunked——**猜一个长度比不给更坏**。`TestReadBlobAnswersRawBytes`、`TestReadBlobAnswersAnEmptyFile` 与 `TestReadBlobOmitsContentLengthWhenTheSizeIsUnknown` 分别压这三种情形。

**所有读失败一律 404 `ERR_NOT_FOUND`**，包括 handle 格式非法与后端连不上。这不是偷懒：调用方手里对每个文件只有一对 `(engine, handle)`，没有第二个选择可试，区分出来它也做不了别的；具体原因在服务日志里。

**删除是幂等的**：删一个已经不在的对象返回 200 `{"status":"deleted"}`，三个引擎都是这个语义。这是**一次刻意的行为变更**——迁入前的 mysqlblob 引擎在受影响行数为 0 时返回错误。Phorge 是「删字节」和「删那条指向字节的记录」一气呵成的，对已经消失的字节报错，会留下一条永远退不掉的记录。`TestDeleteBlobIsIdempotent` 与 `TestLocalDiskDeleteIsIdempotent` 守着它，登记在 [`../findings.md`](../findings.md) 第 23 条。

**同一个非法 handle，读答 404，删答 400 —— 这处不对称是幂等换来的。** 读区分不出「格式非法」和「已经没了」，也不需要区分，所以两者都塌进 404（上一段）。删把「已经没了」算作成功，于是非法 handle 成了**唯一一个不是后端故障的失败**：不把它拎出来，它就落进平台的 500，等于在调用方乱传参数时告诉运维「服务坏了」。引擎因此用 `ErrBadHandle`（`engine.go`）标记这一类错误，`deleteBlob` 认这个哨兵并答 400 `ERR_BAD_REQUEST`。

反过来说，**删除返回 500 就只有一个含义**：后端真的没删掉，字节可能还在，Phorge 绝不能退掉指向它的那条记录。`TestDeleteBlobBadHandleIs400`、`TestDeleteBlobFailureIs500`、`TestReadBlobBadHandleStays404` 与 `TestEnginesReportABadHandle` 把这四个点一起钉住。

### 3.5 `/readyz` 的死锁陷阱：**不要去检查 `file_storageblob` 存不存在**

就绪判据只有两条，一条不多：**至少注册了一个引擎**，以及**持有连接的引擎能连上**（当前只有 blob 引擎实现了 `readyChecker`，它做的就是一次 `db.PingContext`）。整个探测受 5 秒超时约束。

看起来「顺手」该加的那第三条——查一下 `file_storageblob` 这张表在不在——**会让整个栈在第一次启动时死锁**：

- 这张表是 Phorge 的 `bin/storage upgrade` 建的；
- 那条命令跑在 Phorge 应用容器里；
- 而那个容器**要等本服务 healthy 之后才启动**。

于是本服务等一张只有 Phorge 能建的表，Phorge 等本服务健康，两边都不会先动。**ping 数据库则是安全的**，因为数据库服务器是一个独立容器，两边谁都不依赖。这也正是 3.3 那个「失败也回退」存在的另一面：`bin/storage upgrade` 跑完之前 blob 写入必然失败，服务不该因此不可用，上传该落到本地磁盘去。

`Router.Ready` 与 `MySQLBlobEngine.Ready` 的注释里都写着这一条，`TestMySQLBlobReadyReportsAnUnreachableDatabase` 与 `TestReadyzReportsUnconfiguredBackends` 是它的两半：后者断言零后端时 `/healthz` 仍 200 而 `/readyz` 是 503——**「进程活着」与「能存下任何东西」必须分得开**，否则编排会把一个每传必失败的实例留在轮转里。

另外，`OpenDB` **刻意不 ping**。`sql.Open` 是惰性的，所以数据库还没起来时服务照样启动、照样答 `/healthz`，由 `/readyz` 去报告数据库连不上——这正是编排区分「正在启动」与「坏了」所需要的。在这里 ping 只会让一个「慢」的依赖把容器打进重启循环。

## 4. 配置

**每个后端由它自己的配置开关决定是否注册，没有一张「要启用哪些后端」的清单。**一个后端都没配是一个可以正常启动的合法状态，`/readyz` 会把它报成不可用。

| 变量 | 兜底旧名 | 默认值 | 说明 |
|---|---|---|---|
| `GORGE_LISTEN_ADDR` | `LISTEN_ADDR` | `:8100` | 监听地址 |
| `GORGE_SERVICE_TOKEN` | `SERVICE_TOKEN` | 空 | 服务间认证 token，为空则不鉴权 |
| `GORGE_FILE_MYSQL_HOST` | `MYSQL_HOST` | **空** | **blob 后端的开关**，见下 |
| `GORGE_FILE_MYSQL_PORT` | `MYSQL_PORT` | `3306` | |
| `GORGE_FILE_MYSQL_USER` | `MYSQL_USER` | `phorge` | |
| `GORGE_FILE_MYSQL_PASS` | `MYSQL_PASS` | 空 | |
| `GORGE_FILE_NAMESPACE` | `STORAGE_NAMESPACE` | `phorge` | Phorge 的存储命名空间；DSN 里的库名由它拼成 `{namespace}_file` |
| `GORGE_FILE_MYSQL_BLOB_MAX_SIZE` | `MYSQL_BLOB_MAX_SIZE` | `1000000` | 单行 blob 上限（字节）。设成 `0` 是关掉 blob 后端的另一种写法 |
| `GORGE_FILE_LOCAL_DISK_PATH` | `LOCAL_DISK_PATH` | **空** | **本地磁盘后端的开关**；必须是绝对路径，不存在时会创建 |
| `GORGE_FILE_S3_BUCKET` | `S3_BUCKET` | 空 | S3 五件套之一 |
| `GORGE_FILE_S3_ACCESS_KEY` | `S3_ACCESS_KEY` | 空 | |
| `GORGE_FILE_S3_SECRET_KEY` | `S3_SECRET_KEY` | 空 | |
| `GORGE_FILE_S3_REGION` | `S3_REGION` | 空 | |
| `GORGE_FILE_S3_ENDPOINT` | `S3_ENDPOINT` | 空 | 显式端点，且客户端固定走 path-style——MinIO / Ceph 不提供 virtual-hosted 形式 |
| `GORGE_FILE_INSTANCE_NAME` | `INSTANCE_NAME` | 空 | 多个 Phorge 实例共用一个桶时的 key 前缀段 |

命名规则与「新名优先、旧名兜底」的查找机制见 [`../platform.md`](../platform.md) 第 4 节。

三个后端的启用条件：

| 后端 | 启用条件 |
|---|---|
| `blob` | `MySQLHost != ""` **且** `MySQLBlobMaxSize > 0` |
| `local-disk` | `LocalDiskPath != ""` |
| `amazon-s3` | 五个 S3 变量**全部**非空 |

S3 要求五个齐全，是因为半套配置会造出一个每次请求都失败的客户端——那比不注册这个后端更坏。`TestS3RequiresEverySetting` 守着。

**「配了但建不起来」直接让启动失败，「一个都没配」不算错误。**前者（比如本地磁盘给了相对路径）如果放过去，会留下一个看起来配好了、实际什么都不存的服务；后者是配置还没写完时的正常状态，由 `/readyz` 表达。`TestNewRouterFromConfig` 的三个子用例分别是这两条加上「只配本地磁盘」。

### blob 后端的开关变了

`MySQLBlobEnabled()` 现在**要求显式给出 host**，这是相对独立服务时期的一次刻意变更：那边 `MYSQL_HOST` 默认 `127.0.0.1`、`MYSQL_BLOB_MAX_SIZE` 默认 1 MB，于是一个只配了本地磁盘的部署**照样会注册一个指向不存在的数据库的 blob 后端**——它优先级 1，接走每一个小文件上传并让它失败，而 `/readyz`（它会 ping）把整个服务报成不可用。要求 host 之后，「什么都没配」与「配了 blob」才区分得开，与 mailer 的 `MAILER_TYPE` 是同一个形状。`TestNoBackendIsConfiguredByDefault` 钉住它，记在 [`../findings.md`](../findings.md) 第 22 条。

## 5. 兼容契约

权威描述在 [`compat/phorge/README.md`](../../compat/phorge/README.md) 第八节，这里是概述。前六条**破坏之后都不报错，只是既有文件从此读不出来**——而且是对全部存量文件同时发生，新写入的一切照常，所以「写一个读回来」这个最自然的验证动作完全看不见它；最后一条性质不同，它让整个栈在首次启动时死锁。

- **三个 engine identifier 字符串**——`blob` / `local-disk` / `amazon-s3`——Phorge 对每一个写在这里的文件都记着其中一个，且它们和 Phorge 自己的引擎 identifier 是同一批字符串。
- **复合 handle `engine/handle`，按第一个斜杠切**：Phorge 的 `file` 表只有一个 handle 列，所以引擎名编在里面。这个复合串是 PHP 侧独有的——本服务答的是 `engine` 与 `handle` 两个字段，拼接与拆解都在 `PhabricatorGorgeFileStorageEngine`，**Go 侧没有任何测试守得住它**。
- **三种 handle 形态**：本地磁盘的 `ab/cd/{28 hex}`（与 Phorge 自己的本地磁盘布局一致，**同时还是一道安全边界**，见 3.2），blob 的自增行 id，S3 的对象 key。
- **S3 的 key 前缀 `phabricator[/{instance}]/`**：这是 Phorge 自己的前缀，不是装饰。改掉它，桶里每一个对象都原地不动地变成不可达。
- **blob 后端与 Phorge 原生 `PhabricatorMySQLFileStorageEngine` 写同一张表、用同一套自增 id handle 方案**——`{namespace}_file.file_storageblob`。这是一个需要知道的隐患而不是一个特性，`bin/storage` 的维护与 GC 也碰这些行。
- **二进制传输约定**：读成功是原始字节，读失败是 JSON 信封，PHP 客户端必须按**状态码**分支。
- **`/readyz` 不能检查 `file_storageblob` 是否存在**，见 3.5。

## 6. 域级错误码

一个：

| 码 | 状态 | 含义 |
|---|---|---|
| `ERR_NO_ENGINE` | 503 | 没有任何已配置的后端会接下这个文件——**一次都没试** |

它定义在 `internal/filestorage/http.go`，不会被全局错误处理器改写成 `ERR_INTERNAL`——`httpx.Fail` 一写响应就 committed（见 [`../platform.md`](../platform.md) 第 1.2 节）。

**它是本域唯一一个「配置问题」而不是「存储问题」的写失败**，也是唯一一个 Phorge 侧的 setup check 能据以行动的码：503 意味着去补一个后端配置，而其余任何失败都收敛进平台码——请求本身有问题落 400，超过某个具名引擎的限额落 413，读不到落 404，后端试了并且坏了落 500。

**新增域级错误码时加在自己的域包里，不要塞进 `platform/httpx`。**
