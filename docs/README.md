# Gorge 技术文档

按模块组织。跨模块的地基（分层、平台层、测试、交付）各一份，**每个业务模块一份独立文档**，后续模块迁入时只新增文件，不改动既有文档。

基线：`da522f9`，Go 1.26 / Chroma v2.27.0 / Echo v4.15.4 / gorilla-websocket v1.5.3。

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

render 与 diff 共用一个二进制与一个端口：都是无外部依赖的纯计算，拆进程换不来隔离收益。路径按域命名（`/api/highlight/*`、`/api/diff/*`）正是为了让这种合并不需要改动任何一侧。

notification 是第一个反例，两个方向上都是：它自己占一个二进制，因为它有进程内状态、有小时级的长连接、并且**必须不鉴权**（严格 Aphlict 兼容）；它还自己占两个端口，因为 Phorge 校验 `notification.servers` 时要求 admin 与 client 两类记录同时存在且 host:port 不重复。两个端口留在同一个进程里，是因为它们共用同一份内存里的连接表。平台层为此新增了 `httpx.RunAll` 与 `httpx.Config.SkipRootProbe`，见 [`platform.md`](platform.md) 第 1.4 与 3.1 节。

mailer 同样自己占一个二进制，理由与 notification 不同：它是「有外部依赖」的第一个模块——持有适配器状态、要连出去打 SMTP 与各家 provider，于是它也是第一个 `/readyz` 真的比 `/healthz` 多说了点什么的模块（就绪 = 至少配了一个后端）。但它只占一个端口，也照常鉴权，所以没有给平台层带来任何新设施。

**[`modules/notification.md`](modules/notification.md) 明显长于另外两份模块文档，这是刻意的**：本域迁入前带着一份独立的技术报告，那份报告写的是迁入前的包布局、现在每条路径都不存在了，所以它没有被搬进来，而是由模块文档同时充当本域的技术报告。它的前六节仍然是下面那个骨架，第 7 节之后（迁入前后的差异、排查、四层测试各守什么）是骨架之外的补充。**不要照着它把另外两份也扩写**——那两个域的技术报告问题登记在 [`findings.md`](findings.md) 第 6 条，解决方式是删掉过期报告并改指模块文档，不是加长。

## 阅读顺序

第一次接触这个仓库：[`architecture.md`](architecture.md) → [`platform.md`](platform.md) → 你要改的那个模块文档。

**准备改高亮输出、语言别名表、端口或路由**：先读 [`../compat/phorge/README.md`](../compat/phorge/README.md)。那里记录了几件破坏后不会报错、只会静默失效的事，[`modules/render.md`](modules/render.md) 第 5 节是它的概述，但以 compat 文件为准。

**准备改 unified diff 的输出格式**：同样先读 [`../compat/phorge/README.md`](../compat/phorge/README.md)，第 4 节。那份输出是被 `ArcanistDiffParser` **解析**的，一个错误的 hunk 头不会报错，只会让它之后的每一行都放错位置。

**准备改邮件服务的错误码映射**：先读 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第六节。`ERR_PERMANENT_FAILURE` 是全仓库唯一一个改变 Phorge **行为**而不只是改变它报告内容的码——它决定 worker 队列要不要重投这封信。两个方向的误判都不报错：一边是写错的收件人地址被永久重投，另一边是本可以发出去的信被静默丢掉。

**准备改通知服务的端口、路由或响应形状**：先读 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第 5 节。这个域的失败模式是三个里最难发现的——PHP 侧不读本服务的响应体、还把异常整个吞掉，而其中最坏的一条（5.4）破坏之后**连错误都不产生**：请求答 200、计数照常增长、集群面板双绿，只有消息内容被静默揉碎。

## 新增一个模块时

1. 在 `modules/` 下加一份 `<域名>.md`，按下面的骨架写；
2. 在本文件的模块表里加一行；
3. 若该模块引入了新的跨模块设施（比如平台层新增一个包），补 [`platform.md`](platform.md)；
4. 该模块自己的偏差与待办，在 [`findings.md`](findings.md) 里新开一节，不要混进别的模块。

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
