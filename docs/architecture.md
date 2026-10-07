# 架构

项目定位、仓库结构、依赖方向与模块扩展方式。共享设施见 [platform](platform.md)，完整服务和默认端口见 [模块索引](README.md)。

## 1. 项目定位

Gorge 是配对 Phorge 的 Go 服务层。它把高亮、diff、通知协议、存储与外部协议执行，以及队列、调度和部分持久任务移到常驻服务。PHP 继续拥有业务对象、权限、事务与尚未迁移的执行路径。服务配置开启、依赖就绪、数据库执行权与历史任务排空是不同条件，不能用容器存活代替接管验收。

### 1.1 迁移边界

- render/diff 替代 Pygments 和系统 diff 子进程。render 必须保留 PHP 别名与 CSS 类；diff 保留解析器要求的格式，但重复行产生的歧义对齐不保证与 GNU 相同。
- notification 实现 Aphlict 的 admin/client 协议，成功应答不保证浏览器收到消息。
- mailer、search、file-storage 和 integrations 承接第三方协议，并按配置提供持久执行能力。同步接口与持久接口的去重、重试和恢复保证不同。
- webhook 消费 PHP 生产的 Herald 请求表；taskqueue 拥有任务租约与归档，worker 通过 HTTP 执行并提交租约保护的结果。PHP taskmaster 和旧 webhook HTTP 投递已退役。
- scheduler 和 maintenance 使用数据库 owner/epoch 控制执行权；启动 Go 服务不会自动夺取 PHP owner。
- db-api 只读集群诊断；Conduit 网关转发内部调用，业务方法还须遵守自己的身份与协议校验。

跨语言兼容边界见 [compat/phorge](../compat/phorge/README.md)。持久状态、恢复与退役验收见 [operations](operations.md)。

## 2. 仓库结构

```text
go/
  cmd/gorge-*/              二进制入口
  internal/contracts/       共享线上结构
  internal/contracttest/    JSON 契约固件重放器
  internal/doccheck/        文档与配对契约检查
  internal/platform/       共享设施
  internal/<domain>/       域实现与包内测试
  Dockerfile               参数化构建入口
api/openapi/               已登记接口的规范；范围见各文件说明
compat/phorge/             跨语言兼容边界
deploy/compose/            本地编排、示例与 demo
deploy/release/            固定源码对、候选镜像与发布回执
docs/modules/              各域配置、协议与限制
tests/contract/            共享 JSON 固件及 PHP runtime contracts
tests/e2e/                 对运行实例执行的冒烟脚本
```

render 与 diff 共用 `gorge-render`；notification 在一个进程中提供两个监听端口。scheduler 运行在 taskqueue 内，文件生命周期运行在 file-storage 内，搜索投影运行在 search 内，不另建二进制。worker 除通过 HTTP 消费队列外，还可直接连接 Feed/邮件 outbox 数据库。具体部署 profile 和配置以 Compose 与模块文档为准。

所有服务属于一个 Go module，依赖版本以 `go/go.mod` 为准。`go test ./...` 运行包内测试，但需要真实依赖的用例可能因环境未配置而跳过；见 [testing](testing.md)。

## 3. 三层划分

| 层 | 位置 | 职责与依赖 |
|---|---|---|
| 平台层 | `internal/platform/` | HTTP、鉴权、探针、配置、Conduit transport、只读观测；不引入域或契约包 |
| 契约层 | `internal/contracts/` | 线上结构；字段改变需要兼容性评审 |
| 域层 | `internal/<domain>/` | 域逻辑与路由；使用平台与契约，拥有各自连接、写入和执行策略 |

`platform/operations` 接收域提供的连接做只读观测，不是共享连接池或业务存储层。平台全部共享包见 [platform](platform.md) 的索引。

### 3.1 分层检查

[分层测试](../go/internal/platform/layering_test.go) 解析平台源码 import，拒绝 `forbiddenPrefixes` 登记的域和契约依赖。该清单由人工维护，不能将通过测试解释为未登记的新域也已受保护；新增域必须更新登记清单。文档索引、端口与配对契约另由 [doccheck](../go/internal/doccheck/doccheck_test.go) 检查。

### 3.2 契约的消费方

Go handler 直接引用 `internal/contracts`；OpenAPI、PHP 客户端和共享固件维护相应线格式。OpenAPI 是手工维护的，部分文件仅覆盖基础接口；持久扩展及部署协议还须读模块文档和 runtime contracts。配对版本和 manifest 检查不能替代真实系统验收。

普通 JSON API 使用 `{data,error}`。探针返回裸状态；notification 使用 Aphlict 形状；Conduit 使用 `{result,error_code,error_info}`；文件、上传范围读取、图片与 metrics 各有二进制或文本协议。不能为统一形状而改变这些边界。Webhook 出站 payload 的序列化字节参与 HMAC，除了字段，还要保持缩进、键顺序和末尾换行。

## 4. 加一个模块

### 4.1 新增一个二进制

1. 增加 `go/cmd/<name>/main.go`，装配依赖与生命周期。
2. 增加 Compose service，并设置 `SERVICE` 与正确的健康检查端口。
3. 更新 Release matrix、候选 manifest 与配对验收的服务登记、服务文档索引，以及需要的 PHP 部署和握手配置；登记位置见 [delivery](delivery.md)。
4. 按真实依赖增加测试和验收；需要额外运行依赖的服务也须更新 Dockerfile。

参数化 Dockerfile 可复用，不能据此假定任何新服务都无需调整镜像。`PORT` 默认 8140；错误端口会使容器 unhealthy，是否重启由编排决定。构建上下文是 `go/`，详见 [delivery](delivery.md)。

### 4.2 新增域或并入现有进程

增加域包、规范 `GORGE_<DOMAIN>_*` 配置、路由、契约、测试和模块说明，并更新分层登记清单。域错误码留在域包；仅在调用方需要不同处置时增加新码。合入现有进程时需明确共享监听/token、资源限额、就绪与关闭策略；没有新二进制时无需新增 Release matrix 项。

render/diff 当前从 render 配置借用进程级设置。notification 的绑定主机与两个端口不能直接套用单个 `ListenAddr`，因此使用自己的配置结构。扩展模式以实际入口装配为准，不照抄迁入顺序。

### 4.3 路由与进程

路由是客户端契约，不能随二进制名称改变。render 仍使用 `/api/highlight/*`，diff 使用 `/api/diff/*`，两者都在默认 `:8140`；旧 `gorge-diff` 和 `:8130` 已退役。新增域应保留现有消费者的路径，或提供明确版本迁移。
