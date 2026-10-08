# 构建与交付

一份 Dockerfile、一条 compose 编排、多条工作流，服务任意数量的二进制。新增服务还须核对运行依赖、端口、部署和验收配置。

## 1. 一份 Dockerfile 服务所有二进制

`go/Dockerfile` 用 `ARG SERVICE` 选择编译哪个 `./cmd/<name>`，**构建上下文是 `go/` 而非仓库根**：

```bash
docker build -t gorge-render go/
docker build -t gorge-search --build-arg SERVICE=gorge-search --build-arg PORT=8120 go/
docker build -t gorge-conduit --build-arg SERVICE=gorge-conduit --build-arg PORT=8150 go/
```

仓库产出若干个二进制，当前有哪些、各占什么端口见 [`README.md`](README.md) 的模块表（**这里刻意不重复那个数字**，它在前几次迁入里过期过好几轮）。`SERVICE` 这个参数是给**新增二进制**用的，不是给新增域用的：域并入既有进程时不碰这里，见 [`architecture.md`](architecture.md) 第 4.2 节。

`PORT` 只在服务不监听 8140 时需要显式给，理由见下一段。

多阶段构建：

- **构建阶段** `golang:${GO_VERSION}-alpine${BUILDER_ALPINE_VERSION}`。默认值与可用镜像标签直接看 [`go/Dockerfile`](../go/Dockerfile) 顶部的 `ARG`；builder 与 runtime 的 Alpine 版本刻意独立。先 `COPY go.mod go.sum` 再 `go mod download`，然后才 `COPY .`，让依赖层在源码变动时仍能命中缓存。编译参数 `CGO_ENABLED=0 -trimpath -ldflags="-s -w"`：静态链接、抹掉构建路径、去符号表。
- **运行阶段** `alpine:${ALPINE_VERSION}`。带二进制与 CA 证书；image 服务额外安装 ImageMagick 与 codec，以 uid 10001 的非 root 用户 `gorge` 运行。healthcheck 用的 wget 由 busybox 自带，不额外装包。

两处针对 Docker 语义的处理，都在注释里写了原因：

- **`ENTRYPOINT` 不能展开构建参数**，所以用 `ln -s` 把二进制挂到一个固定名字 `gorge-service` 上，`ENTRYPOINT ["gorge-service"]` 才写得出来。
- **`HEALTHCHECK` 经由 `/bin/sh` 在容器运行时求值**，所以端口通过 `ENV GORGE_HEALTHCHECK_PORT=${PORT}` 传递。这个变量**只被 HEALTHCHECK 指令用，服务本身不读**——改 `GORGE_LISTEN_ADDR` 的端口时必须同步改它（构建参数 `PORT`），否则探针一直打旧端口，容器会被判成 unhealthy；是否重启由外部编排决定。

## 2. Compose 编排

`deploy/compose/docker-compose.yml`。用 YAML 锚点 `x-service-defaults` 抽出每个服务都要的 `restart`、网络、日志轮转（10MB × 3），新服务 `<<: *service-defaults` 一行继承。

```bash
cd deploy/compose && cp .env.example .env && docker compose up -d --build
# 或从仓库根
make compose-up
```

`build.context` 指向 `../../go`，`build.args.SERVICE` 选二进制。这个本地服务层 Compose
保留 `<service>-${GORGE_IMAGE_TAG}` 镜像名，默认 `latest`；它与配对发布门禁是不同入口。
当前 Release 不更新这些服务标签。部署已验收版本时，从 `release-manifest.json` 取各服务
的完整 `ghcr.io/<owner>/<repo>@sha256:…` 引用，用部署 overlay 固定镜像；不能用
`GORGE_IMAGE_TAG=latest` 或独立服务的历史标签推断一套匹配版本。

`.env.example` 里每个变量都带注释说明取值含义，其中共享 token 留空会让普通域的中间件跳过鉴权；image、maintenance、原生邮件和持久投影等能力要求非空 token，启用前须按对应模块配置。

`make compose-config` 在不启动的前提下校验 compose 文件。

## 3. CI

基础静态检查与单元测试在 [ci.yml](../.github/workflows/ci.yml)：

| Job | 内容 |
|---|---|
| `fmt` | `gofmt -s -l`，非空即失败并打印 diff |
| `vet` | `go vet ./...` |
| `build` | `go build ./...` |
| `test` | `go test -v ./...` |
| `lint` | golangci-lint（`--timeout=5m`） |
| `security-scan` | govulncheck |

两处配置细节：

- **`paths` 过滤**限定在 `Makefile`、`go/**`、`tests/**`、`.github/workflows/**`，纯 PHP 改动不触发 Go 流水线。将来 `php/` 下有代码时应当另开一条工作流，而不是放宽这里的过滤。
- **Go 命令在 `go/` 执行**，因为 module 在子目录。镜像构建从仓库根显式传 `go/` 上下文。`golangci-lint-action` 自己解析 module，所以走 `with.working-directory: go` 输入而不是 shell 级目录。

覆盖率报告不跟随普通 push / pull request 生成。`.github/workflows/test-report.yml` 只在手动触发或推送 `YYYY.MM.DD-rN` 标签时运行 `soulteary/go-test-report-action`，报告、徽章、JSON 与原始结果上传为 Actions 制品；`commit: false` 保证报告不会由机器人写回仓库并产生新的提交。

本地对齐 CI：

```bash
make check    # fmt-check + vet + test，即 CI 的主要内容（不含 lint 与 govulncheck）
make lint     # 需要本地装 golangci-lint
```

`go vet` 不覆盖 errcheck 和 Staticcheck 的全部规则，`-race` 也只检查实际运行中的
数据竞争。不能用 vet/race 通过代替 lint 通过。CI 的 lint action 当前选择 `version: latest`；
复现本地结果时记录 `golangci-lint --version`，完整质量检查须单独执行 `make lint`。

## 4. Release

[release.yml](../.github/workflows/release.yml) 由 `YYYY.MM.DD-rN` tag 或手动 dispatch
触发。两种方式都要求已存在的版本 tag，并检出该 tag 的 Gorge commit；配对 Phorge
通过 `PHORGE_CONTRACT_REF` 仓库变量或 `phorge_ref` 输入固定到完整 40 位 commit SHA，
不回退到 main。发布前还验证 tag 的真实日历日期。

流程依次固定源码对、检查契约、构建十四个双架构候选镜像、执行完整配对验收、生成
`release-manifest.json`，最后发布 GitHub Release。候选标签使用
`candidate-<run ID>-<attempt>-<service>`，最终清单绑定每个镜像的 digest 与两仓库 commit。
`linux/amd64,linux/arm64` 编译和按 service 分 scope 的 Buildx cache 保留；服务清单、
健康端口和源码标签由矩阵及 [清单门禁](../deploy/release/manifest.py) 核对。

十四个候选镜像都检查非 root 用户、二进制可执行性、源码 revision 和健康端口。
image/render 还必须以对应候选 digest 启动实际容器，记录各自 container/image ID，
再执行配对契约。其他服务的业务路径由配对源码的 Go/PHP 测试验证；这不表示十四个
候选容器都完成了运行时业务验收，也不表示另一架构已执行同样的运行时测试。

所有版本的最终 publish job 共用仓库级锁。锁内读取 GitHub Release `latest`，按日期
和整数修订号比较版本；先将草稿以 `--latest=false` 发布，仅严格更新的版本提升
`latest`。旧版本晚完成可发布自身版本，但不会让入口倒退。只有明确 HTTP 404 才视为
无既有 release；权限、限流、超时、坏响应或未知旧 tag 格式均停止后续操作。
每个 GitHub CLI 调用限时 30 秒，失败后保留实际状态供核对，不自动覆写已有版本。

构建或配对验收失败时不进入发布。常规 CI 的 lint 与安全扫描是独立 job；Release 自身
执行契约、vet 和配对测试，不应把它描述为已重复所有 CI 质量规则。发布细节及模拟
GitHub 回归见 [发布门禁说明](../deploy/release/README.md)。历史独立 package/tag
保持已有状态，当前流程不更新任何 `*-latest` 服务标签。

发布正文读取 tag 源码中的 [RELEASE_NOTES.md](../RELEASE_NOTES.md)，再附加清单部署
提示。准备 PR 更新正文和版本范围；准备状态及待完成检查写入 `docs/releases/`，
正文应可直接用于正式 Release。工作流已创建 Release 时，不要手动创建同名版本。

多架构发布锁使用 image index digest。发布前先检查基础和配对验收镜像的平台，
防止把本机 ARM64 子 manifest 用于 AMD64 runner。[Release Build](../.github/workflows/release-build.yml)
在相关 PR 中构建 render/image 的 AMD64 与 ARM64 镜像，不发布产物；AMD64
runner 通过 QEMU 执行 ARM64 构建步骤。平台与恢复规则见
[发布门禁说明](../deploy/release/README.md)。

## 5. 加一个服务的登记

1. `go/cmd/<name>/main.go`
2. `deploy/compose/docker-compose.yml` 加一个 service，`build.args.SERVICE` 填 `<name>`
3. `.github/workflows/release.yml` 的 `matrix.service` 加一项

这三处是基础登记。发布清单与候选验收另有完整服务集，新增二进制还须同步
`deploy/release/manifest.py`、配对 Phorge 的 `deploy/acceptance/accept.py`，并补对应端口、
包装和运行时验收。新运行依赖需要更新 Dockerfile 与 CI，不能只添加矩阵项。

若新服务不是新二进制而是并入既有进程的新域，则这三处一处都不用改，见 [`architecture.md`](architecture.md) 第 4.2 节。

## 6. Makefile

`make help` 列出全部目标。分三组：

| 组 | 目标 |
|---|---|
| Go | `build` `run` `test` `cover` `fmt` `fmt-check` `vet` `lint` `tidy` `check` |
| Docker | `docker-build` `compose-config` `compose-up` `compose-down` `compose-logs` |
| 测试 | `docs-check` `e2e` |

`SERVICE`、`BASE_URL` 是可覆盖变量（`SERVICE=<二进制名> make build`），默认分别是 `gorge-render` 与 `http://127.0.0.1:8140`。`make build` 与 `make run` 一次只作用于一个二进制，别的二进制靠覆盖 `SERVICE` 来选。

`e2e` 跑 Makefile 明确登记的冒烟脚本。它们**不共用一个 URL**，因为一个域一个端口的划分不是一比一的：render 与 diff 由一个进程在一个端口上服务（都打 `BASE_URL`），notification 一个进程两个端口（`NOTIFY_ADMIN_URL` 与 `NOTIFY_CLIENT_URL`），mailer 与 search 各自一个进程一个端口（`MAILER_URL`、`SEARCH_URL`）。所以 Makefile 里是一组变量而不是一个。

各脚本对被测实例有前提，不满足时它们是**按设计失败**而不是环境问题：`mailer.sh` 要求配了至少一个后端（否则 `/readyz` 那条红），`search.sh` 同理，且它**会销毁索引**——只对着一次性部署跑。

## 7. 真实后端与跨仓库验收

CI 还包含 queue-execution（MySQL/Redis）、worker-lifecycle、image-runtime、search-projection-runtime、cleanup-runtime 和 integrations-runtime。服务镜像、测试环境变量与筛选用例以工作流为准，`make check` 不会自动启动这些后端。

[contract-drift.yml](../.github/workflows/contract-drift.yml) 对照配对 PHP checkout 的协议和迁移；本地设置 `PHORGE_FORK_DIR` 后执行 `make docs-check`。没有该变量时跨仓库检查会跳过，不能把跳过当成配对通过。

完整容器交付、恢复故障和版本配对见 [operations.md](operations.md) 及 Phorge 的 `deploy/acceptance/accept.py`。本地单元测试通过不等于生产接管或退役验收通过。

完整 Docker 验收使用真实 MySQL、Redis、S3、image/render、Elasticsearch 和 Meilisearch。
Go JSON 事件中带 `Test` 名称的 skip 会使必测阶段失败；没有 `Test` 字段的包级
`[no test files]` 事件不当成用例跳过。验收记录浏览器业务流程、真实 provider 投递和
生产性能尚未覆盖，不能用成功回执替代这些上线前检查。
