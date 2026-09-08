# Gorge 技术文档

按模块组织。跨模块的地基（分层、平台层、测试、交付）各一份，**每个业务模块一份独立文档**，后续模块迁入时只新增文件，不改动既有文档。

基线：`da522f9`，Go 1.27 / Chroma v2.27.0 / Echo v4.15.4 / gorilla-websocket v1.5.3。

文中的覆盖率与行数都是**快照**，按上面这个基线读，不要当成会被 CI 守住的数字。

## 目录

### 跨模块

| 文档 | 内容 |
|---|---|
| [`architecture.md`](architecture.md) | 项目定位、仓库结构、三层划分与被测试守住的分层约束、契约层、加一个模块的成本 |
| [`platform.md`](platform.md) | `internal/platform` 四个包：httpx（引导与错误信封）、auth、health、config |
| [`testing.md`](testing.md) | 四层测试体系、契约固件格式、为什么断言 contains 而非 golden、覆盖率现状 |
| [`delivery.md`](delivery.md) | Dockerfile、Compose 编排、CI 与 Release 流水线 |
| [`findings.md`](findings.md) | 已发现的代码/文档偏差与改进建议，按模块分节 |

### 模块

| 模块 | 二进制 | 端口 | 文档 | 状态 |
|---|---|---|---|---|
| render | `gorge-render` | `:8140` | [`modules/render.md`](modules/render.md) | 已迁入 |
| diff | `gorge-render`（同进程） | `:8140` | [`modules/diff.md`](modules/diff.md) | 已迁入 |
| notification | `gorge-notification` | `:22280`（client/WS）+ `:22281`（admin） | [`modules/notification.md`](modules/notification.md) | 已迁入 |
| mailer | `gorge-mailer` | `:8110` | [`modules/mailer.md`](modules/mailer.md) | 已迁入 |
| search | `gorge-search` | `:8120` | [`modules/search.md`](modules/search.md) | 已迁入 |
| file-storage | `gorge-file-storage` | `:8100` | [`modules/file-storage.md`](modules/file-storage.md) | 已迁入 |
| webhook | `gorge-webhook` | `:8160` | [`modules/webhook.md`](modules/webhook.md) | 已迁入 |
| conduit | `gorge-conduit` | `:8150` | [`modules/conduit.md`](modules/conduit.md) | 已迁入 |

render 与 diff 共用一个二进制与一个端口：都是无外部依赖的纯计算，拆进程换不来隔离收益。路径按域命名（`/api/highlight/*`、`/api/diff/*`）正是为了让这种合并不需要改动任何一侧。

notification 是第一个反例，两个方向上都是：它自己占一个二进制，因为它有进程内状态、有小时级的长连接、并且**必须不鉴权**（严格 Aphlict 兼容）；它还自己占两个端口，因为 Phorge 校验 `notification.servers` 时要求 admin 与 client 两类记录同时存在且 host:port 不重复。两个端口留在同一个进程里，是因为它们共用同一份内存里的连接表。平台层为此新增了 `httpx.RunAll` 与 `httpx.Config.SkipRootProbe`，见 [`platform.md`](platform.md) 第 1.4 与 3.1 节。

mailer 同样自己占一个二进制，理由与 notification 不同：它是「有外部依赖」的第一个模块——持有适配器状态、要连出去打 SMTP 与各家 provider，于是它也是第一个 `/readyz` 真的比 `/healthz` 多说了点什么的模块（就绪 = 至少配了一个后端）。但它只占一个端口，也照常鉴权，所以没有给平台层带来任何新设施。

search 是「有外部依赖」的第二个模块，也因此走的是与 mailer 完全相同的路：一个二进制、一个端口、照常鉴权、`/readyz` 报「至少配了一个可读后端」、healthcheck 打 `/readyz` 而不是 `/healthz`。它没有给平台层带来新设施，这本身是个结论——**「有外部依赖」这一类到 file-storage 为止有三个成员，三次都没需要新东西**，所以下一个这类域可以直接照 mailer 或 search 的骨架来。两处差异值得知道：它的外部依赖是**存储**而不是投递通道，所以 `deploy/compose/docker-compose.yml` 刻意不声明 Elasticsearch 容器（把存储埋进服务层的编排文件会让 `docker compose down -v` 变成一种丢索引的方式）；而它的就绪判据与 mailer 一样刻意**不拨测**下游，理由见 [`modules/mailer.md`](modules/mailer.md) 与 [`modules/search.md`](modules/search.md) 各自的第 2 节。

file-storage 带进来两件仓库里此前没有的东西，而它同样**没有**给平台层新增任何设施——这三件事凑在一起才是这个模块值得单说一句的地方。第一件是**第一个数据库驱动**：`go-sql-driver/mysql` 由 `internal/filestorage/db.go` 自己 import，连接池也住在域包里，`platform/` 至今没有、也刻意不长任何数据库设施（一个域要连接池不构成共享关切，理由记在 [`findings.md`](findings.md) 第 24 条）。第二件是**第一个非 JSON 的 `/api/**` 成功响应**：读文件成功答的是原始 `application/octet-stream` 字节，失败才是信封。而这一条恰恰不需要平台层配合——`httpx` 从不强迫 handler 用 JSON 应答，handler 直接调 `c.Stream` 就行，失败路径上 `errorHandler` 照旧产出信封，见 [`platform.md`](platform.md) 第 1.1 节。

webhook 是「有外部依赖」这一类的第四个成员，但它在另一个维度上是第一个，而那个维度比上面几段讨论过的都更根本：**它的工作不由入站请求驱动。**前六个域都是「有人来问、答一句」，所以「服务在正常工作」与「服务答得出请求」是同一件事；webhook 的两个 HTTP 端点都只是报数，没有任何一条路径能启动一次投递——真正的工作是一个轮询 `{namespace}_herald.herald_webhookrequest` 的循环。三个后果值得在读它的模块文档之前就知道。第一，**每一层测试的分辨能力都要重新评估**：契约固件只覆盖那两个只读端点、e2e 脚本压根碰不到投递，而它最硬的那条契约（投出去的字节）只有单元测试守得住，理由记在 [`../tests/contract/webhook/README.md`](../tests/contract/webhook/README.md)。第二，**`/readyz` 与 `/healthz` 的差距在这里比在任何别的域都大**：一个连不上库的实例照旧在监听、`/healthz` 照旧 200、投递量为零，而且任何地方都不出现失败——Phorge 继续入队，那些行就静静躺着。这也是为什么本域的可观测性问题（该有的信号被日志默认级别吞掉、不该有的刷屏）单独登记成了 [`findings.md`](findings.md) 第 44 条。第三，它是唯一一个**必须替换而不能与 PHP 侧并存**的域：队列在数据库里，两边谁都能取，所以「服务在跑但 PHP 侧的接管开关没写进去」的表现不是「配了不生效」，而是每个 webhook 发两次，登记在 [`findings.md`](findings.md) 第 38 条。它同样**没有**给平台层新增任何设施——那个循环整个住在域包里，`main.go` 只是多起一个 goroutine 并在关连接池之前等它收尾。

conduit 是第八个域，而它在一个所有前七个域都共享的维度上是第一个反例：**它的消费方是其它 Go 服务而非 Phorge PHP。**前七个域都在 Phorge 前面被 Phorge 调用，conduit 挡在 Phorge 前面、被别的 Go 服务调用。它是一个 Conduit API 的反向代理网关，替换的既不是子进程、也不是运行时依赖、也不是外部存储/投递通道，而是一种**调用拓扑**——把「各 Go 服务直连 Phorge `/api/*`」的多对一直连改成「多对一经网关」，在网关层统一鉴权、按 IP 限流与请求审计。它无持久状态（限流表在内存、每实例独立），所以 healthcheck 打 `/healthz` 而非 `/readyz`，并**刻意不**把上游可达性纳入就绪判据——那报告的是 Phorge 的健康而非网关自己的。它的兼容边界形状也是独一份的：错误信封是 Conduit 专用的 `{result,error_code,error_info}`（不是其它域的 `{data,error}`）、成功路径必须纯透传，理由见 [`modules/conduit.md`](modules/conduit.md) 第 5 节。它同样**没有**给平台层新增任何设施。

**[`modules/notification.md`](modules/notification.md) 明显长于其余模块文档，这是刻意的**：本域迁入前带着一份独立的技术报告，那份报告写的是迁入前的包布局、现在每条路径都不存在了，所以它没有被搬进来，而是由模块文档同时充当本域的技术报告。它的前六节仍然是下面那个骨架，第 7 节之后（迁入前后的差异、排查、四层测试各守什么）是骨架之外的补充。**不要照着它把其余几份也扩写**——过期技术报告的问题登记在 [`findings.md`](findings.md) 第 6 条，解决方式是删掉过期报告并改指模块文档，不是加长。

## 阅读顺序

第一次接触这个仓库：[`architecture.md`](architecture.md) → [`platform.md`](platform.md) → 你要改的那个模块文档。

**准备改高亮输出、语言别名表、端口或路由**：先读 [`../compat/phorge/README.md`](../compat/phorge/README.md)。那里记录了几件破坏后不会报错、只会静默失效的事，[`modules/render.md`](modules/render.md) 第 5 节是它的概述，但以 compat 文件为准。

**准备改 unified diff 的输出格式**：同样先读 [`../compat/phorge/README.md`](../compat/phorge/README.md)，第 4 节。那份输出是被 `ArcanistDiffParser` **解析**的，一个错误的 hunk 头不会报错，只会让它之后的每一行都放错位置。

**准备改邮件服务的错误码映射**：先读 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第六节。`ERR_PERMANENT_FAILURE` 是全仓库唯一一个改变 Phorge **行为**而不只是改变它报告内容的码——它决定 worker 队列要不要重投这封信。两个方向的误判都不报错：一边是写错的收件人地址被永久重投，另一边是本可以发出去的信被静默丢掉。

**准备改通知服务的端口、路由或响应形状**：先读 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第 5 节。这个域的失败模式是三个里最难发现的——PHP 侧不读本服务的响应体、还把异常整个吞掉，而其中最坏的一条（5.4）破坏之后**连错误都不产生**：请求答 200、计数照常增长、集群面板双绿，只有消息内容被静默揉碎。

**准备改搜索服务的字段名、四字符常量或分析器链**：先读 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第七节。那一节五条**全部**是「写得进去、答 200、就是查不到」型：写入侧与查询侧是两条独立的路径，各自都能独立地完全正常，而没有任何一层会去比对「写进去的键」与「查出来的键」。唯一的例外是分析器链——改它会让所有既有索引明确报 not sane 并强制一次全量重建，那一条**会**报错。

**准备改 webhook 投出去的那份 payload、签名头名或回写字段**：先读 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第九节。那一节分成性质相反的两半：出站字节那一半是全仓库少见的「**会**报错」的兼容约束，只是错误发生在别人的服务器上——签名是对 payload 的**整个字符串（含 2 空格缩进与末尾换行）**算的，所以改缩进就是改签名；回写字段那一半则完全静默，其中 `status` 的取值范围还是整个抢占机制的地基，多一个值会同时弄坏 Phorge 的界面和 PHP 的回退路径。

## 新增一个模块时

1. 在 `modules/` 下加一份 `<域名>.md`，按下面的骨架写；
2. 在本文件的模块表里加一行；
3. 若该模块引入了新的跨模块设施（比如平台层新增一个包），补 [`platform.md`](platform.md)；
4. 该模块自己的偏差与待办，在 [`findings.md`](findings.md) 里新开一节，不要混进别的模块。

**另外，下面这几处跨模块文档带着会过期的计数，每次迁入都要重数一遍**——它们在前几次迁入里连着漏了几回，所以单列出来：

| 位置 | 会过期的东西 |
|---|---|
| [`architecture.md`](architecture.md) 第 1 节 | 二进制数、域数、生产/测试代码行数、契约固件数、e2e 脚本数 |
| [`architecture.md`](architecture.md) 第 2、3.1、3.2 节 | 仓库结构树、`forbiddenPrefixes` 列表、`internal/contracts` 的行数 |
| [`testing.md`](testing.md) 第 4、5 节 | 各包覆盖率与固件数 |
| [`../compat/phorge/README.md`](../compat/phorge/README.md) 附录 | 域级错误码总数、「本附录讲哪几个域」那句 |

行数与固件数**用 `find` / `wc` 数，不要估**。根 [`../README.md`](../README.md) 与 [`delivery.md`](delivery.md) 曾经也在这张表里，现在已经改成不点名数量的写法，所以不必再随迁入维护。

模块文档骨架：

```markdown
# <域名> 模块

一句话定位 + 承载它的二进制与端口。

## 1. 职责边界      这个域负责什么，不负责什么
## 2. 路由与依赖     注册了哪些路径，Deps 里有什么
## 3. 核心实现       handler 的处理链路、域内引擎
## 4. 配置           域级环境变量与默认值
## 5. 兼容契约       与 Phorge 之间不能随意改动的约定，指向 compat/
## 6. 域级错误码     本域独有的码，以及它为什么不收敛进平台码
```

第 5 节是最容易被略过、也最要紧的一节：Gorge 的各个模块都在替换 Phorge 里既有的能力，替换出错时的典型表现是**静默降级而非报错**，只有文档能提醒下一个人。
