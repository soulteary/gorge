# Gitea 模块

把签名过的 Gitea 事件单向写入相关 Maniphest 任务的时间线。二进制为
`gorge-gitea`，默认监听 `:8180`。

## 1. 职责边界

服务从 issue、pull request、push 和 release 的标题、正文或提交信息中提取
`T123` 引用，并通过 `transaction.search` + `maniphest.edit` 写一条评论。

它不做双向同步，不修改 Gitea，不同步评审意见，也不把 Gitea 用户映射成 Phorge
用户。所有评论由一个最小权限 Conduit 服务账号提交；SSO、账号生命周期和身份
绑定继续由 Stargate 维护。

## 2. 路由与安全

| 方法 | 路径 | 鉴权 |
|---|---|---|
| `POST` | `/webhooks/gitea` | `X-Gitea-Signature` HMAC-SHA256 |
| `GET` | `/api/gitea/meta` | `X-Service-Token`，不接受查询参数 token |

事件 URL 只有与 `GORGE_GITEA_BASE_URL` 同源时才会进入评论，避免用合法 webhook
把任意外链注入任务。Gitea delivery ID 同时写入稳定的文本标记；重投前先查
任务最近的 transactions，已经出现该标记就跳过，因而进程重启后仍可幂等。

## 3. 配置

| 变量 | 默认值 | 说明 |
|---|---|---|
| `GORGE_GITEA_BASE_URL` | 无 | Gitea 对外基准 URI |
| `GORGE_GITEA_WEBHOOK_SECRET` | 无 | Gitea webhook 共享密钥 |
| `GORGE_GITEA_CONDUIT_URL` | 无 | Gorge Conduit 网关地址 |
| `GORGE_GITEA_CONDUIT_TOKEN` | 无 | 专用 Phorge bot 的 API token |
| `GORGE_GITEA_GATEWAY_TOKEN` | 无 | 调用 Conduit 网关的服务 token |
| `GORGE_GITEA_TIMEOUT_SEC` | `15` | 单次 Conduit 请求超时 |

四项必填连接配置缺失时 `/readyz` 返回 503，进程仍保持存活以便编排和诊断。

## 4. Gitea 配置

在组织或仓库 webhook 中选择 `Issues`、`Pull Request`、`Push`、`Release`，目标为
`https://<bridge>/webhooks/gitea`，密钥与 `GORGE_GITEA_WEBHOOK_SECRET` 相同。不要
启用评论和 review 事件：本集成刻意只提供代码活动到任务时间线的单向摘要。

Compose 中该服务位于可选的 `gitea` profile，使用
`docker compose --profile gitea up -d` 启动；未配置 Gitea 的既有部署不会多拉镜像。

## 5. 失败与重试

签名或请求错误返回 4xx；Phorge/Conduit 失败返回 502 +
`ERR_GITEA_DELIVERY_FAILED`。Gitea 应按非 2xx 重试，相同 delivery ID 不会重复写入
已经成功的任务。
