# Gorge 技术文档

按模块组织。跨模块的地基（分层、平台层、测试、交付）各一份，**每个业务模块一份独立文档**，新增模块时同步更新索引及受影响的跨模块说明。

文档描述当前源码 checkout。依赖版本以 [`go/go.mod`](../go/go.mod) 为准，镜像版本以 [`go/Dockerfile`](../go/Dockerfile) 为准；不要在说明文字里复制一份容易失效的版本清单。

文中的覆盖率与行数若未明确标注生成日期，只用于解释测试边界，不作为当前数值的权威来源。精确结果以 `make cover` 或 Release / 手动触发的 Go Test Report artifact 为准。

## 目录

历史版本准备记录：[2026.10.07-r2](releases/2026.10.07-r2.md)。该版本双语发布正文见
[RELEASE_NOTES.md](../RELEASE_NOTES.md)；当前发布步骤与候选回执以
[delivery](delivery.md)和[发布工具](../deploy/release/README.md)为准。

### 跨模块

接管与持久状态运维参见 [`operations.md`](operations.md)：统一报告、保留边界、
容量观测、一致备份与跨仓库故障验收。

| 文档 | 内容 |
|---|---|
| [`architecture.md`](architecture.md) | 项目定位、仓库结构、三层划分与被测试守住的分层约束、契约层、加一个模块的成本 |
| [`platform.md`](platform.md) | `internal/platform`：HTTP 引导、鉴权、探针、配置、Conduit client 与只读观测 |
| [`testing.md`](testing.md) | 四层测试体系、契约固件格式、为什么断言 contains 而非 golden、覆盖率现状 |
| [`delivery.md`](delivery.md) | Dockerfile、Compose 编排、CI 与 Release 流水线 |
| [`operations.md`](operations.md) | 接管报告、持久状态、容量、备份与配对验收 |
| [`findings.md`](findings.md) | 当前限制、已落实的兼容决策与后续验证边界，按模块分节 |

### 模块

| 模块 | 二进制 | 端口 | 文档 | 状态 |
|---|---|---|---|---|
| render | `gorge-render` | `:8140` | [`modules/render.md`](modules/render.md) | 已迁入 |
| diff | `gorge-render`（同进程） | `:8140` | [`modules/diff.md`](modules/diff.md) | 已迁入 |
| notification | `gorge-notification` | `:22280`（client/WS）+ `:22281`（admin） | [`modules/notification.md`](modules/notification.md) | 已迁入 |
| mailer | `gorge-mailer` | `:8110` | [`modules/mailer.md`](modules/mailer.md) | 已迁入 |
| search | `gorge-search` | `:8120` | [`modules/search.md`](modules/search.md) | 已迁入 |
| image | `gorge-image` | `:8190` | [`modules/image.md`](modules/image.md) | 已实现，按部署模式接管 |
| file-storage | `gorge-file-storage` | `:8100` | [`modules/file-storage.md`](modules/file-storage.md) | 已迁入 |
| webhook | `gorge-webhook` | `:8160` | [`modules/webhook.md`](modules/webhook.md) | 已迁入 |
| taskqueue | `gorge-taskqueue` | `:8090` | [`modules/taskqueue.md`](modules/taskqueue.md) | 已迁入 |
| worker | `gorge-worker` | `:8170` | [`modules/worker.md`](modules/worker.md) | 已迁入 |
| maintenance | `gorge-maintenance` | `:8200` | [`modules/maintenance.md`](modules/maintenance.md) | 已实现，执行权由 owner/epoch 控制 |
| db-api | `gorge-db-api` | `:8080` | [`modules/dbapi.md`](modules/dbapi.md) | 已迁入 |
| conduit | `gorge-conduit` | `:8150` | [`modules/conduit.md`](modules/conduit.md) | 已迁入 |
| gitea | `gorge-gitea` | `:8180` | [`modules/gitea.md`](modules/gitea.md) | 已迁入 |
| integrations | `gorge-integrations` | `:8210` | [`modules/integrations.md`](modules/integrations.md) | 按配置启用：入站/SMS/连接器/Fact |

render 与 diff 共用进程与端口。notification 在同一进程内分别监听 client 与 admin；其余二进制按模块独立部署。部署启用状态与表中的实现状态不是同一件事，profile、配置、数据库 owner 与运行握手共同决定是否执行。

图片的基础配置保留 legacy 模式，配对 Phorge 的生产 overlay 强制使用 `gorge`；maintenance 初始 owner 与后续接管须按 [maintenance](modules/maintenance.md)和[operations](operations.md)检查，不能用本表代替实际配置或数据库状态。

扩展能力另见 [scheduler](modules/scheduler.md)（taskqueue 内的持久触发器调度）、[file-lifecycle](modules/file-lifecycle.md)（上传、迁移与删除恢复）和 [search-projection](modules/search-projection.md)（持久投影、影子 generation 与重建）。这些能力不是额外的二进制。

## 阅读顺序

第一次接触这个仓库：[`architecture.md`](architecture.md) → [`platform.md`](platform.md) → 你要改的那个模块文档。

**准备改高亮输出、语言别名表、端口或路由**：先读 [`../compat/phorge/README.md`](../compat/phorge/README.md)。那里记录了几件破坏后不会报错、只会静默失效的事，[`modules/render.md`](modules/render.md) 第 5 节是它的概述，但以 compat 文件为准。

**准备改 unified diff 的输出格式**：同样先读 [`../compat/phorge/README.md`](../compat/phorge/README.md)，第 4 节。那份输出由 `ArcanistDiffParser` 解析；部分格式偏离会报错，部分可能被接受却造成位置错误。修改时须验证完整 hunk 与解析结果，不能只检查 HTTP 成功。

**准备改邮件服务的错误码映射**：先读 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第六节。同步发送的 `ERR_PERMANENT_FAILURE` 决定 PHP worker 是否停止重试；原生持久投递还须对照 [mailer](modules/mailer.md) 的账本状态与不确定结果恢复规则。

同步发送的 `ERR_OUTCOME_UNKNOWN` 表示无法确认供应商是否已接受邮件，必须保留 PHP 的不确定结果围栏；只有明确未接受的失败才可自动重试或切换后端。调整请求期限、回执解析或错误分类时一起验收这条边界。

**准备改通知服务的端口、路由或响应形状**：先读 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第五节。PHP 对部分通知失败会吞掉异常，HTTP 成功也不保证浏览器收到消息；需要验收实际消息内容与收件路径。

**准备改搜索服务的字段名、四字符常量或分析器链**：先读 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第七节。写入与查询必须使用同一字段约定；分析器变化可能使已有索引不再 sane，须安排重建。持久投影另见 [search-projection](modules/search-projection.md)。

**准备改 webhook payload、签名头名或回写字段**：先读 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第九节。签名覆盖整个 payload 字符串（含缩进与末尾换行）；回写状态同时影响 Go claim 与 PHP 兼容任务的终态处理。PHP HTTP 投递已退役。

**准备改 taskqueue 的任务字段或租约/失败计数语义**：先读 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第十节。`taskClass`、`dataID`、`leaseOwner`、`leaseExpires`、`failureCount` 对应 Phorge worker 表字段。队列没有 `status` 列，持有权由租约字段决定；新消费者还须遵守 finalize/resolve 的 fencing 与持久回执协议，不能回退到已退役的 PHP taskmaster。

**准备改 db-api 的字段名、错误码或库/表名约定**：先读 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第十一节。`refKey`、`isFatal`、`connectionStatus` 由 PHP 直接按键读取；`isFatal` 决定 setup issue 是阻断还是告警。库名 `{namespace}_meta_data` 与表名 `patch_status`/`hoststate` 必须与 Phorge 保持一致。

## 新增一个模块时

1. 在 `modules/` 下加一份 `<域名>.md`，按下面的骨架写；
2. 在本文件的模块表里加一行；
3. 若该模块引入了新的跨模块设施（比如平台层新增一个包），补 [`platform.md`](platform.md)；
4. 该模块自己的偏差与待办，在 [`findings.md`](findings.md) 里新开一节，不要混进别的模块。

`make docs-check` 会验证 `go/cmd/` 的每个二进制都在模块表中、`tests/contract/` 的每个域都在契约索引中，检查模块默认监听端口、本地 Markdown 文件链接、静态 API group 路由的文档引用及清理器登记清单，并阻止已经退役的 HTTP API 写法重新混进当前文档。设置 `PHORGE_FORK_DIR` 后还会检查配对 Phorge 的文档链接与生产清理清单。静态路由检查不覆盖动态路径或仅接收已挂载 router 的 helper，仍须做 runtime 验收。新增入口或固件目录时先更新索引；行数、覆盖率与依赖版本引用各自的单一事实来源，不在手写说明里复制快照。

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

第 5 节应说明实际消费者与破坏后的表现，并链接对应契约或 runtime 验收。部分兼容错误会静默降级，文档说明与行为测试须一起维护。
