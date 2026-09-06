# 架构

跨模块的地基：项目定位、仓库结构、三层划分及其强制手段、加一个模块的成本。平台层各包的实现细节见 [`platform.md`](platform.md)，具体模块见 [`modules/`](modules/)。

## 1. 项目定位

Gorge 是 Phorge（Phabricator 社区维护分支）的 Go 服务层单仓库。Phorge 里若干原本靠子进程、PHP 内联实现或外部依赖完成的能力，在这里以常驻 Go 服务重写，通过 HTTP 与 PHP 侧对接。仓库同时容纳 Go 代码、共享契约（OpenAPI + 契约固件）、容器编排，以及将来 PHP 侧的适配层。

当前产出三个二进制、承载**四个域**：`gorge-render` 里住着 render 与 diff，`gorge-notification` 独占一个进程与两个端口，`gorge-mailer` 独占一个进程与一个端口。代码规模：生产代码 5174 行，测试代码 6374 行（约为生产代码的 1.23 倍），外加 47 份语言中立的契约固件（render 12 + diff 14 + notification 11 + mailer 10）与四份 e2e 冒烟脚本。

### 1.1 为什么要替换掉进程内实现

以已迁入的 render 域为例。Phorge 默认通过 `PhutilPygmentsSyntaxHighlighter` 调用 Pygments，每次高亮都要 fork 一个 Python 进程：

- **进程开销**：进程创建与 Python 解释器初始化的开销远大于实际的词法分析计算；
- **环境依赖**：PHP 服务器上必须装 Python 与 Pygments，扩大了运维面与攻击面；
- **无法独立伸缩**：计算与 PHP 应用绑死在同一台机器上。

改成打向常驻 Go 进程的一次 HTTP 调用之后，三条都消失了。代价是引入了一条**兼容边界**：Go 侧的输出必须与被替换掉的实现一致，而这类不一致的典型表现是静默降级——代码块照常渲染，只是没有颜色，不会有任何报错。这条约束的具体形态见各模块文档的「兼容契约」一节。

已迁入的 diff 域动机相同，但兼容边界的形状不同、也更硬：它替换的是 `diff -U65535` 这个子进程调用，而输出被 PHP 侧**解析**而非渲染，所以要求逐字节一致（见 [`modules/diff.md`](modules/diff.md) 第 3.1 节）。

notification 域的动机又不一样：它替换的不是子进程或内联实现，而是一个**外部 Node.js 常驻服务**（Aphlict），所以省下的是一整条运行时依赖，而不是进程创建开销。它的兼容边界也是三个里最不容易发现的——PHP 侧不读本服务的响应体、还把异常整个吞掉，而最坏的一条破坏之后连错误都不产生：请求答 200、集群面板双绿，只有消息内容被静默揉碎（见 [`modules/notification.md`](modules/notification.md) 第 5 节）。

后续的 conduit、search、mailer 等各有各的形状，但「失效时不报错」这个特征是共通的。

## 2. 仓库结构

```
.
├── go/                       单一 Go module（github.com/soulteary/gorge/go）
│   ├── cmd/gorge-render/     二进制入口，一个 cmd 一个服务
│   ├── cmd/gorge-notification/ 同上；两个 httpx.Server 交给 httpx.RunAll
│   ├── cmd/gorge-mailer/     同上；唯一传了非 nil Ready 的入口
│   ├── internal/contracts/   线上数据结构，PHP / Go / OpenAPI / 固件的唯一真源
│   ├── internal/contracttest/ 契约固件 runner，两个域各写一个 wrapper 指向自己的目录
│   ├── internal/platform/    httpx / auth / health / config，不依赖任何业务域
│   ├── internal/render/      render 域：highlight 引擎 + HTTP 路由 + 配置
│   ├── internal/diff/        diff 域：unified / prose 引擎 + HTTP 路由 + 配置
│   ├── internal/notification/ notification 域：admin / client 两组路由 + hub/ + peer/
│   ├── internal/mailer/      mailer 域：七个投递适配器 + Dispatcher + HTTP 路由 + 配置
│   └── Dockerfile            一份 Dockerfile 服务所有二进制（ARG SERVICE 选择）
├── api/openapi/render.yaml   render 域的 HTTP 契约
├── api/openapi/diff.yaml     diff 域的 HTTP 契约
├── api/openapi/notification.yaml  notification 域的 HTTP 契约（两个端口）
├── api/openapi/mailer.yaml   mailer 域的 HTTP 契约
├── compat/phorge/README.md   与 Phorge 的兼容约束，改动前必读
├── deploy/compose/           本地与单机部署编排
├── deploy/kubernetes/        （占位）
├── php/{extensions,adapters}/（占位）PHP 侧接入代码
├── docs/                     本文档目录
└── tests/
    ├── contract/render/      语言中立的契约固件，Go 与 PHP runner 共读
    ├── contract/diff/        同上；unified 部分做字节精确断言
    ├── contract/notification/ 分 admin/ 与 client/ 两组，因为它们是两个端口
    ├── contract/mailer/      同上；失败路径靠 test 适配器的可控失败注入
    ├── e2e/render.sh         对着运行中实例做的冒烟测试
    ├── e2e/diff.sh           同上，打同一个端口的 /api/diff/*
    ├── e2e/notification.sh   同上，但要 ADMIN_URL 与 CLIENT_URL 两个变量
    └── e2e/mailer.sh         同上；被测实例必须配了至少一个后端，否则 /readyz 那条按设计失败
```

`contracttest` 是 diff 迁入时从 render 的固件测试里抽出来的。抽出的理由不是省代码，而是**断言词汇必须在两个域之间保持一致**——各写一份 runner，两个域很快会开始用不同的方式描述自己的契约。

单个 Go module 承载所有二进制，是这个仓库的基本选择：依赖版本只有一份、`go test ./...` 一次跑完、平台层被所有域直接引用而不经过版本协商。代价是所有服务被绑在同一条依赖升级节奏上，这在服务数量还是个位数时不构成问题。

## 3. 三层划分

| 层 | 包 | 职责 | 允许依赖 |
|---|---|---|---|
| 平台层 | `internal/platform/{httpx,auth,health,config}` | HTTP 引导、鉴权、探针、配置读取 | 只依赖标准库与三方库 |
| 契约层 | `internal/contracts` | 线上数据结构，无行为 | 无 |
| 域层 | `internal/render`、`internal/diff`、`internal/notification` | 业务逻辑与路由 | 平台层 + 契约层 |

平台层不含任何业务知识：`httpx.Config` 里没有一个字段提到高亮，`auth.Token` 不知道自己保护的是哪些路由，`config.Base` 只有 `ListenAddr` 与 `ServiceToken` 两个「每个服务都有」的字段。

### 3.1 分层约束是被测试守住的

`go/internal/platform/layering_test.go` 用 `go/parser` 走遍 `platform/` 下所有 `.go` 文件，解析 import 列表，断言其中不出现域包与契约包：

```go
var forbiddenPrefixes = []string{
	"github.com/soulteary/gorge/go/internal/render",
	"github.com/soulteary/gorge/go/internal/diff",
	"github.com/soulteary/gorge/go/internal/notification",
	"github.com/soulteary/gorge/go/internal/contracts",
}
```

这是整个仓库最值得注意的一处设计。分层规则通常只写在 README 里靠 code review 维持，几个月后就会被一次「顺手复用」破坏。这里把它变成了 `go test ./...` 会失败的硬约束，代价只有 55 行代码。它的直接目的是保证将来某个平台子包能被单独拆成 module，而服务拆分本身就是这个单仓库的既定演进方向。

注意 `contracts` 也在禁止列表里。平台层如果引用了某个域的线上结构，`httpx` 就会带上一份业务相关的 JSON 定义，拆分时同样会拽出依赖。

**新增域包时要同步往 `forbiddenPrefixes` 里加一行**，否则这个测试对新域是失效的——它照样通过，只是不再检查任何新东西。上面 `internal/diff` 与 `internal/notification` 两行都是各自迁入时手工补的，这是目前唯一需要人工维护的地方，改进建议见 [`findings.md`](findings.md) 第 8 条。

### 3.2 契约层：四个消费方的唯一真源

`internal/contracts` 一共 99 行（`render.go` 20 + `diff.go` 42 + `notification.go` 37），装的是过网络的数据结构，包注释写明了它的地位：

```go
// It is the single source of truth for what goes over the network: nothing in
// here may carry behaviour, and every field change is a compatibility change.
```

四个消费方围着它转：

```
        internal/contracts/{render,diff,notification}.go
                                │
        ┌───────────────┬───────┴───────┬────────────────┐
        ▼               ▼               ▼                ▼
   Go handler    api/openapi/*.yaml   PHP 适配层   tests/contract/*.json
   （直接引用）    （手工镜像，注释声明）  （手工镜像）    （固件断言）
```

`notification.go` 在这里是个例外，值得单独知道：它承载的结构**刻意不套 `{data,error}` 信封**，因为 PHP 侧直接 `phutil_json_decode()` 取键。所以这个文件的包注释是全仓库唯一一处「这里的形状就是线上形状，不要统一」的声明，理由见 [`../compat/phorge/README.md`](../compat/phorge/README.md) 第 5.3 节。

只有 Go handler 是编译期绑定的，另外三个靠约定同步。为此每份 OpenAPI 文档都在 `info.description` 里显式写了它镜像的是哪个 contracts 文件，而契约固件被设计成 Go 与 PHP **两个 runner 共读同一批文件**（见 [`testing.md`](testing.md)），这样至少 Go/PHP 之间的漂移会被测试捕捉。

一个由此而来的实现选择：`Highlighter.Highlight()` 直接返回 `*contracts.HighlightResult`，而不是先返回一个域内结构体再转换。包注释说明了理由——第二个结构体要带自己的 json tag，两者迟早会漂移。

## 4. 加一个模块的成本

### 4.1 新增一个二进制

1. 写 `go/cmd/<name>/main.go`，复用 `internal/platform` 引导；
2. `deploy/compose/docker-compose.yml` 加一个 service，`build.args.SERVICE` 填 `<name>`；
3. `.github/workflows/release.yml` 的 `matrix.service` 加一项。

`go/Dockerfile` 与 CI 不需要改。这是「一个 cmd 一个服务 + 一份参数化 Dockerfile + 平台层统一引导」三件事对齐之后的直接收益，细节见 [`delivery.md`](delivery.md)。

notification 是走完这条路的第一个例子，也暴露了一处这三步没覆盖的东西：Dockerfile 的 `ARG PORT` 只决定镜像内置 `HEALTHCHECK` 用的端口，而它默认是 render 的 8140。一个监听别的端口的新服务必须显式传 `PORT`，否则探针打向一个没人监听的端口，容器起来之后反复重启。见 [`modules/notification.md`](modules/notification.md) 开头那一节。

### 4.2 新增一个域（可能并入既有二进制）

diff 就是走完这条路的例子：它没有新起进程，而是并入 `gorge-render`，加一组 `/api/diff/*` 路由。要做的是：

1. 建 `go/internal/<domain>/`，写 `RegisterRoutes(e, deps)` 与域级 `Config`；
2. 域级环境变量加 `GORGE_<DOMAIN>_` 前缀（理由见 [`platform.md`](platform.md) 的 config 一节）；
3. 在 `layering_test.go` 的 `forbiddenPrefixes` 加一行；
4. 域级错误码定义在自己的域包里，**不要塞进 `platform/httpx`**；若本域没有真实的失败模式就不要造码（diff 域即如此）；
5. 补 `api/openapi/<domain>.yaml` 与 `tests/contract/<domain>/` 固件，用 `internal/contracttest` 写一个 wrapper 指向自己的固件目录；
6. 补 `tests/e2e/<domain>.sh` 并挂进 Makefile 的 `e2e` 目标；
7. 按 [`README.md`](README.md) 的骨架加一份 `docs/modules/<domain>.md`。

`main.go` 里多一次 `RegisterRoutes` 调用即可，平台层不动。`.github/workflows/release.yml` 也不动——没有新二进制。

**并入既有二进制时唯一需要现场判断的是共享配置。** 进程级配置（监听地址、service token）当前住在 `render.Config` 里，新域得从 `render.Load()` 的结果里借；这样接是对的，但位置摆错了，见 [`findings.md`](findings.md) 第 4 条。

第三个域（notification）进来时那一条**没有**兑现，原因值得记一下：它是独立二进制，且它既不用 `ServiceToken`（严格 Aphlict 兼容 = 不鉴权），`ListenAddr` 的 `host:port` 语义也套不进「一个绑定主机 + 两个端口」。所以它定义自己的 `Config` 且不嵌 `config.Base`，第 4 条继续留在 findings 里等下一个**并入既有进程**的域。

### 4.3 路由按域命名，不按二进制命名

`gorge-render` 这一个进程承载 render 域，但路由保持 `/api/highlight/*` 而**不是** `/api/render/*`。两个理由：这些路径是 Phorge 侧客户端已经在调的，改了要同步改 PHP；按域命名之后，diff 并进同一进程时直接加 `/api/diff/*`，两边都不用动。

这条设计已经兑现了一次：diff 迁入时两个域的路由都没有改动，PHP 侧的 `gorge.render.uri` 与 `gorge.render.token` 也不需要改。

同样的道理适用于端口：`gorge-diff` 原先的 `:8130` **已废弃**，diff 域走 `:8140`。编排里也没有新增 service，只在既有的 `render` 上多了一个 `GORGE_DIFF_MAX_BYTES` 环境变量。
