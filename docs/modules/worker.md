
## 原生 Feed 与 Outbox

`GORGE_WORKER_OUTBOX_DSN`：可选 MySQL 驱动 DSN，指向 Phorge feed 数据库。
配置后后台 relay 将 feed_gorgeoutbox 事件提交到 queue 的 enqueue-event。
在 bundled compose 中默认配置，必须先运行 Phorge storage upgrade。

`GORGE_WORKER_FEED_POLICY_FILE`：可选 JSON 文件，内容为
`{"silent": false, "uris": ["https://example.test/feed"]}`。
每次投递重新读取；文件缺失或字段缺失按临时失败处理，静默模式跳过投递，
已移除 URI 按永久失败处理。版本 1 Feed 快照直接在 Go 执行，无 PHP prepare。
旧 key 格式仍需委托 PHP，必须先排空历史任务再移除该兼容路径。
不配置政策文件时保留之前的 PHP prepare 路径；原生化部署必须配置它。

就绪检查现在验证 queue 的 v1 生命周期能力、配置的政策文件和 outbox
表。健康检查仍只表示存活。首次升级时缺少表会使 /readyz 返回失败，但
/healthz 不受影响；迁移角色不能依赖 Worker ready，避免首启闭环。
