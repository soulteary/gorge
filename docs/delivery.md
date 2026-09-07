# 构建与交付

一份 Dockerfile、一条 compose 编排、两条流水线，服务任意数量的二进制。新增模块时这里最多改一行。

## 1. 一份 Dockerfile 服务所有二进制

`go/Dockerfile` 用 `ARG SERVICE` 选择编译哪个 `./cmd/<name>`，**构建上下文是 `go/` 而非仓库根**：

```bash
docker build -t gorge-render go/
docker build -t gorge-search --build-arg SERVICE=gorge-search --build-arg PORT=8120 go/
docker build -t gorge-conduit --build-arg SERVICE=gorge-conduit go/   # 将来的二进制
```

仓库产出若干个二进制，当前有哪些、各占什么端口见 [`README.md`](README.md) 的模块表（**这里刻意不重复那个数字**，它在前几次迁入里过期过好几轮）。`SERVICE` 这个参数是给**新增二进制**用的，不是给新增域用的：域并入既有进程时不碰这里，见 [`architecture.md`](architecture.md) 第 4.2 节。

`PORT` 只在服务不监听 8140 时需要显式给，理由见下一段。

多阶段构建：

- **构建阶段** `golang:1.26-alpine3.22`。先 `COPY go.mod go.sum` 再 `go mod download`，然后才 `COPY .`，让依赖层在源码变动时仍能命中缓存。编译参数 `CGO_ENABLED=0 -trimpath -ldflags="-s -w"`：静态链接、抹掉构建路径、去符号表。
- **运行阶段** `alpine:3.22`。只带二进制与 CA 证书，以 uid 10001 的非 root 用户 `gorge` 运行。healthcheck 用的 wget 由 busybox 自带，不额外装包。

两处针对 Docker 语义的处理，都在注释里写了原因：

- **`ENTRYPOINT` 不能展开构建参数**，所以用 `ln -s` 把二进制挂到一个固定名字 `gorge-service` 上，`ENTRYPOINT ["gorge-service"]` 才写得出来。
- **`HEALTHCHECK` 经由 `/bin/sh` 在容器运行时求值**，所以端口通过 `ENV GORGE_HEALTHCHECK_PORT=${PORT}` 传递。这个变量**只被 HEALTHCHECK 指令用，服务本身不读**——改 `GORGE_LISTEN_ADDR` 的端口时必须同步改它（构建参数 `PORT`），否则探针一直打旧端口，容器会被判成 unhealthy 而反复重启。

## 2. Compose 编排

`deploy/compose/docker-compose.yml`。用 YAML 锚点 `x-service-defaults` 抽出每个服务都要的 `restart`、网络、日志轮转（10MB × 3），新服务 `<<: *service-defaults` 一行继承。

```bash
cd deploy/compose && cp .env.example .env && docker compose up -d --build
# 或从仓库根
make compose-up
```

`build.context` 指向 `../../go`，`build.args.SERVICE` 选二进制，`image` 同时写着 ghcr 地址——这样本地能构建、CI 产物也能直接拉。

`.env.example` 里每个变量都带注释说明取值含义，其中 `GORGE_SERVICE_TOKEN` 留空即关闭鉴权，注释明确写了「只在私网可接受」。

`make compose-config` 在不启动的前提下校验 compose 文件。

## 3. CI

`.github/workflows/ci.yml`，六个并行 job：

| Job | 内容 |
|---|---|
| `fmt` | `gofmt -s -l`，非空即失败并打印 diff |
| `vet` | `go vet ./...` |
| `build` | `go build ./...` |
| `test` | `go test -v ./...` |
| `lint` | golangci-lint（`--timeout=5m`） |
| `security-scan` | govulncheck |

两处配置细节：

- **`paths` 过滤**限定在 `go/**`、`tests/**`、`.github/workflows/**`，纯 PHP 改动不触发 Go 流水线。将来 `php/` 下有代码时应当另开一条工作流，而不是放宽这里的过滤。
- **所有 job 带 `working-directory: go`**，因为 module 在子目录。`golangci-lint-action` 是例外，它自己解析 module，所以走 `with.working-directory` 输入而不是 shell 级的那个——这一点在 workflow 里有注释，容易踩。

覆盖率报告不跟随普通 push / pull request 生成。`.github/workflows/test-report.yml` 只在手动触发或推送 `YYYY.MM.DD-rN` 标签时运行 `soulteary/go-test-report-action`，报告、徽章、JSON 与原始结果上传为 Actions 制品；`commit: false` 保证报告不会由机器人写回仓库并产生新的提交。

本地对齐 CI：

```bash
make check    # fmt-check + vet + test，即 CI 的主要内容（不含 lint 与 govulncheck）
make lint     # 需要本地装 golangci-lint
```

## 4. Release

`.github/workflows/release.yml`，`YYYY.MM.DD-rN` CalVer tag 触发，也可手动 dispatch 指定镜像 tag。手动触发固定检出 `main`，不受 Actions 页面当时所选 ref 影响。

按 `matrix.service` 逐个构建并推到 ghcr.io，`fail-fast: false` 让一个服务失败不拖垮其余。双架构 `linux/amd64,linux/arm64`，GitHub Actions cache 按 service 分 scope（`scope=${{ matrix.service }}`），避免不同二进制互相冲掉缓存。

tag 策略由 `docker/metadata-action` 生成：CalVer 发布同时推送原始版本标签与 `latest`；手动触发从 `main` 构建，并使用输入的镜像标签（默认 `latest`）。

## 5. 加一个服务要改的三处

1. `go/cmd/<name>/main.go`
2. `deploy/compose/docker-compose.yml` 加一个 service，`build.args.SERVICE` 填 `<name>`
3. `.github/workflows/release.yml` 的 `matrix.service` 加一项

**Dockerfile 与 CI 不改。** 这是把「一个 cmd 一个服务 + 一份参数化 Dockerfile + 平台层统一引导」三件事对齐之后的直接收益。

若新服务不是新二进制而是并入既有进程的新域，则这三处一处都不用改，见 [`architecture.md`](architecture.md) 第 4.2 节。

## 6. Makefile

`make help` 列出全部目标。分三组：

| 组 | 目标 |
|---|---|
| Go | `build` `run` `test` `cover` `fmt` `fmt-check` `vet` `lint` `tidy` `check` |
| Docker | `docker-build` `compose-config` `compose-up` `compose-down` `compose-logs` |
| 测试 | `e2e` |

`SERVICE`、`BASE_URL` 是可覆盖变量（`SERVICE=<二进制名> make build`），默认分别是 `gorge-render` 与 `http://127.0.0.1:8140`。`make build` 与 `make run` 一次只作用于一个二进制，别的二进制靠覆盖 `SERVICE` 来选。

`e2e` 跑 `tests/e2e/` 下的每一个脚本。它们**不共用一个 URL**，因为一个域一个端口的划分不是一比一的：render 与 diff 由一个进程在一个端口上服务（都打 `BASE_URL`），notification 一个进程两个端口（`NOTIFY_ADMIN_URL` 与 `NOTIFY_CLIENT_URL`），mailer 与 search 各自一个进程一个端口（`MAILER_URL`、`SEARCH_URL`）。所以 Makefile 里是一组变量而不是一个。

两个脚本对被测实例有前提，不满足时它们是**按设计失败**而不是环境问题：`mailer.sh` 要求配了至少一个后端（否则 `/readyz` 那条红），`search.sh` 同理，且它**会销毁索引**——只对着一次性部署跑。
