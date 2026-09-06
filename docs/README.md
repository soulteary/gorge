# Gorge 技术文档

按模块组织。跨模块的地基（分层、平台层、测试、交付）各一份，**每个业务模块一份独立文档**，后续模块迁入时只新增文件，不改动既有文档。

基线：`da522f9`，Go 1.26 / Chroma v2.27.0 / Echo v4.15.4。

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
| diff | `gorge-render`（并入） | `:8140` | — | 计划中，见 [`modules/render.md`](modules/render.md) |

## 阅读顺序

第一次接触这个仓库：[`architecture.md`](architecture.md) → [`platform.md`](platform.md) → 你要改的那个模块文档。

**准备改高亮输出、语言别名表、端口或路由**：先读 [`../compat/phorge/README.md`](../compat/phorge/README.md)。那里记录了三件破坏后不会报错、只会静默失效的事，[`modules/render.md`](modules/render.md) 第 5 节是它的概述，但以 compat 文件为准。

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
