# 测试体系

不同测试验证不同边界。包内测试和共享 JSON 固件不能证明实际数据库、provider 或配对部署已经工作。

| 层 | 位置 | 验证范围 |
|---|---|---|
| 包内与分层测试 | `go/**/*_test.go` | 算法、handler、依赖方向与生命周期 |
| 文档与契约漂移检查 | `go/internal/doccheck/` | 索引、默认端口、文件链接、清理登记与配对 manifest |
| 共享 JSON 固件 | `tests/contract/<domain>/` | 请求、响应和兼容线格式 |
| Runtime / 真实依赖测试 | Go integration tests 与 PHP runtime contracts | 数据库事务、恢复和跨语言实际调用 |
| e2e 冒烟 | `tests/e2e/` | 运行实例的监听、基础协议与部分往返 |

## 1. 分层测试与文档检查

平台 import 检查使用人工登记的 `forbiddenPrefixes`，新增域必须更新清单，见 [architecture](architecture.md) 第 3.1 节。`make docs-check` 检查文档索引与本地文件链接等；设置 `PHORGE_FORK_DIR` 后才执行配对 Phorge 的文档及 manifest 检查。没有配对目录时的 SKIP 不表示跨仓库通过。

本地默认布局是同一父目录下的 `gorge/` 与 `phorge-fork/`，目录变量须指向实际 checkout。
Phorge CI 把 Gorge 放在自身 `.test-gorge/`，因此显式传 `--gorge-dir .test-gorge`，Go
跨仓库检查使用 `PHORGE_FORK_DIR=$GITHUB_WORKSPACE`。不要把本地兄弟目录路径直接
套到 CI；容器入口会在自己的隔离执行目录保留文档所需的兄弟布局。

## 2. 契约固件

域目录与 runner 的完整登记见 [契约索引](../tests/contract/README.md)，逐条固件作用见对应目录 README。固件描述 HTTP 应答；Webhook 出站 payload、后台执行和事务恢复需要额外测试，不能靠只读固件证明。

### 2.1 格式与断言

```json
{
  "name": "highlight a source fragment",
  "request": {
    "method": "POST",
    "path": "/api/highlight/render",
    "headers": {"X-Service-Token": "contract-token"},
    "body": "{\"source\":\"x = 1\",\"language\":\"python\"}"
  },
  "expect": {
    "status": 200,
    "jsonHas": ["data.html"],
    "jsonAbsent": ["error"],
    "htmlContainsClasses": ["k", "mi"]
  }
}
```

`request.body` 是字符串，以便表达非法 JSON 或原始字节。`json*` 断言解码响应，`html*` 断言 `data.html`，`body*` 断言原始响应；头部使用 `headerEquals`。检查 markup 优先用 `html*`，避免不同 JSON 编码器对 `<` 的转义差异。

JSON 路径支持数组下标；查找时先尝试完整字面量键，再按点分段，因此 Aphlict 的 `clients.active` 不会被误当成嵌套对象。只有状态码/原始字节断言的固件无需 JSON 解码。

### 2.2 Runner 配置

需鉴权的共享固件使用 `contract-token`；notification 按 Aphlict 协议不挂 service token，admin/client 分开运行。其他环境与 seed 以各域 runner 为准，不一概假设使用默认配置。

正常与 unavailable 固件需要不同服务状态。search、webhook、taskqueue 和 db-api 的故障配置由 runner 注入；mailer 可用请求 `mailerKeys` 选择 test adapter。PHP runtime contracts 可能使用自己的配置及测试 token，不能把共享 JSON runner 的前提套到全部 PHP 测试。

### 2.3 状态与测试边界

共享固件应避免依赖前一文件制造的数据；精确往返放在独立测试中。taskqueue 的写固件会改变 active 计数，统计断言应使用稳定 seed 字段或形状；Webhook 的 seed 要让易混淆的计数不同。

内存 search backend 不运行 Elasticsearch 分析器。中文文档能写入或被子串查询找到，不能证明 `cjk` mapping 正确。file-storage 必须同时验证成功的裸字节、失败信封、零字节文件与历史 handle；只测新写入再读回看不到历史可达性漂移。

worker 没有共享 JSON 固件目录；静态 meta、stats、通知计数、就绪与执行协议由包内测试和配对 PHP runtime contracts 验证。原生邮件、上传、投影、scheduler、cleanup、integrations 的持久路径同样不能由基础同步固件代替。

## 3. 按输出稳定性选择断言

render 的 HTML 会随 Chroma 升级变化，固定的是 PHP 依赖的 CSS 类与别名语义，采用 contains/不变量断言。unified diff 被 `ArcanistDiffParser` 解析，需精确验证 hunk 和无尾换行标记；系统 diff 交叉测试检验格式，重复行歧义对齐不保证相同。prose diff 同时验证分段和旧/新文本无损还原。

二进制与签名协议需检查响应头、原始字节和摘要；持久任务需检查事务、重放、过期持有者、提交后崩溃与未知结果。替换预期文件前先判断变化是否真的符合兼容边界。

## 4. e2e 与真实依赖

冒烟脚本访问已经运行的实例，不负责启动或停机。它们会执行自身场景所需的写入/清理，不能假定只读：search init 销毁索引，taskqueue 留下测试任务归档，file-storage 创建/删除字节。仅对隔离测试部署运行有写入的脚本。

```sh
BASE_URL=http://127.0.0.1:8140 TOKEN=dev bash tests/e2e/render.sh
BASE_URL=http://127.0.0.1:8140 TOKEN=dev bash tests/e2e/diff.sh
ADMIN_URL=http://127.0.0.1:22281 CLIENT_URL=http://127.0.0.1:22280 bash tests/e2e/notification.sh
BASE_URL=http://127.0.0.1:8110 TOKEN=dev bash tests/e2e/mailer.sh
BASE_URL=http://127.0.0.1:8120 TOKEN=dev bash tests/e2e/search.sh
BASE_URL=http://127.0.0.1:8100 TOKEN=dev bash tests/e2e/file-storage.sh
BASE_URL=http://127.0.0.1:8100 TOKEN=dev ENGINES="blob local-disk s3" bash tests/e2e/file-storage-matrix.sh
BASE_URL=http://127.0.0.1:8160 TOKEN=dev bash tests/e2e/webhook.sh
BASE_URL=http://127.0.0.1:8090 TOKEN=dev bash tests/e2e/taskqueue.sh
BASE_URL=http://127.0.0.1:8080 TOKEN=dev bash tests/e2e/dbapi.sh
```

render/diff 共用端口；notification 用两个端口且不传 TOKEN。其他脚本在 TOKEN 为空时会跳过未授权断言，不能把 SKIP 当成鉴权验证通过。mailer 需配置后端，search 需可读后端，文件矩阵需实际配置列出的每个引擎，queue 与 webhook 需可达的队列存储。配对服务的 schema/能力还须通过对应 readiness 检查。

search 在 test backend 上跳过 CJK 分析器场景；要验证它们必须使用真实 Elasticsearch。Webhook 冒烟只覆盖状态和接口，不能证明后台外部投递。队列冒烟走基础 enqueue/lease/complete，不替代 worker 的 fenced 生命周期验收。

`TOKEN=dev-token make e2e` 运行 [Makefile](../Makefile) 明确登记的脚本及 URL；其他模块的 runtime suites 不由此自动启动。真实 MySQL、Redis、图片后端与配对 PHP 验证见各模块的“验证”节和 [CI](../.github/workflows/ci.yml)。配对 DB 诊断还见 [db-api-cross-repo](../.github/workflows/db-api-cross-repo.yml)。

Phorge 的 `deploy/acceptance/accept.py` 是完整 Docker 验收入口，默认读取兄弟 Gorge
目录，也可显式 `--gorge-dir <实际 checkout>`；`--check` 只验证锁定配置，不启动容器。
完整运行创建独立 Compose 项目和临时持久资源，启动真实 MySQL、Redis、S3、image、
render、Elasticsearch 和 Meilisearch，再执行 Go 全包 race 与 PHP 配对、恢复契约。
发布候选模式另检查十四个候选 digest 的包装，并绑定实际启动的 image/render 容器。
详细发布范围见 [delivery](delivery.md)；其他候选服务并非全部以该镜像启动业务测试。

完整入口保存 Go `-json` 事件：只有带 `Test` 名称的 `Action=skip` 会加入
`skippedTests` 并使必测阶段失败；没有用例名的包级 skip 通常表示无测试文件，不算
跳过用例。普通本地测试允许依赖未配置时跳过，完整发布验收不允许用这些跳过取得
通过回执。还须分别报告失败阶段、实际依赖和 `notCovered`。

测试环境变量按具体测试/工作流设置。未配置真实依赖时，有些 Go 测试会 SKIP。数据库测试可能创建/删除测试库或表，只能使用其要求的隔离数据库；不能把生产 DSN 交给测试。Phorge 完整交付、恢复及退役验收见 [operations](operations.md)。

## 5. 覆盖率与检查结果

```sh
make check
make lint
make cover
```

`make check` 执行格式、vet 与 Go tests，不包含所有真实后端、lint 或 govulncheck。`make cover` 生成 `go/coverage.out` 和 `go/coverage.html`。当前数据以本次运行和日志中的 SKIP 为准，不在手写文档维护包百分比、测试数量或代码行数。

仓库已有 `make lint`，它自动进入 `go/` 运行 golangci-lint。直接运行工具也须从该
模块目录执行。记录 `golangci-lint --version`，因为 CI 当前使用 `latest`。errcheck
检查未处理错误，Staticcheck 提供额外规则；它们不由 `go vet` 或 race 替代。

`cmd/*` 的覆盖率不能表达完整启动与停机验收；真实 listener/信号可由包内测试验证，容器依赖与后台收尾还需 runtime 测试。默认覆盖只记各包自己的测试，需要跨包路径时使用 `-coverpkg`。内存 fake 验证请求与协议形状，不能证明 MySQL/Redis/ES/provider 的实际语义。

[Go Test Report](../.github/workflows/test-report.yml) 在手动触发或发布标签触发时保存报告 artifact，设置 `commit: false`，不会将报告写回仓库。普通 CI 与文档检查是否通过、真实依赖是否运行、部署是否验收必须分别报告。

## 6. 资源与恢复证据的限制

通知测试验证慢浏览器/peer 隔离、单 writer、有界队列、历史和实例预算，以及超过
队列容量的健康历史重放。Worker 回归验证长短任务持续补槽、heartbeat fencing、有限
排空和到期任务归档；无法确认归档时不能宣称未知执行已持久隔离。diff 回归覆盖
2001/10000 行中的单行改动、共同前后缀裁剪和文本/换行无损不变量，仍保留未解决
中段的比较单元预算。对应包的 benchmark/fuzz 可用于对比同一环境下的变化。

这些测试不等于生产最大连接数、吞吐、内存上限或尾延迟承诺。上线前仍须用实际
消息大小、连接与任务分布、图像/搜索负载，在目标 CPU/内存和容器限制下测量。
完整验收也明确未覆盖登录后的浏览器业务流程和真实邮件供应商投递。

Phorge `tests/operations` 的隔离恢复测试与 ES 原生快照回归证明各自 fixture 的恢复
路径。生产备份 bundle 的 SQL/卷回执保留 `businessIntegrity=not_verified`；含外部
搜索快照时还保留 `externalSearchRestore=not_verified`。具体备份的业务引用、unknown
核对、原生搜索恢复和切流必须另行验收，见 [operations](operations.md)。
