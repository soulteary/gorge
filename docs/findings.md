# 已知边界与改进建议

本文件区分当前限制和已落实的兼容决策，不作为历史覆盖率报告。事实以当前源码及配对验收为准；运行数据由测试重新生成。编号是供其他文档引用的稳定 ID，不重排；已修决策保留简短依据，过时的实现、行数与百分比不保留。

## 平台层

### 1. render 关闭等待与请求超时是两种配置

`GORGE_RENDER_TIMEOUT_SEC` 只传给 `httpx.Config.ShutdownTimeout`。当前使用说明已按此修正，但 render 未设置请求级超时；若新增请求超时或改名，须同时升级 Go 配置与两侧编排。

### 2. render 传输层上限不可由域配置提高

render 使用平台默认 `2M` BodyLimit。提高 `GORGE_RENDER_MAX_BYTES` 不会提高传输上限；其他域可在引导时设置自己的 BodyLimit。

## render 模块

### 3. 语言列表每次请求重建

`Highlighter.Languages()` 每次从 lexer 注册表构造小写列表。可考虑缓存；不是协议故障，修改时保留语言列表的既有内容。

## diff 模块

### 4. 共享进程的配置由 render 持有

render.Config 嵌入 config.Base；同进程 diff 借用其 token。新增同进程域时可考虑将进程配置上提，独立二进制不受此约束。

### 5. LCS 的资源与歧义对齐边界

unified 使用 LCS 表和 maxCells 护栏，不是 Myers。大行数乘积可能被拒绝，重复行的最小对齐不保证与 GNU 相同。不要仅提高护栏；算法替换须重新验收格式及无损不变量。见 [diff](modules/diff.md)。

## 文档

### 7. 兼容断言应指回协议依据

CSS 类名、别名与边界断言的依据是 [compat](../compat/phorge/README.md)。新增或修改测试时保留该联系；测试文件是否已加注释以当前源码为准，不把固定文件数写成待办。

### 8. 分层禁止列表仍需人工维护

platform/layering_test.go 的 forbiddenPrefixes 是手写清单。新域不会自动进入保护范围；新增时更新清单，或将它改成从内部目录发现。

## notification 模块

### 9. 通知失败与历史淘汰需要行为测试

测试应覆盖写失败摘除 listener、replay 写失败结束会话及 history 按年龄淘汰。包内覆盖率无法衡量跨包 WebSocket 调用；用当前用例和 `-coverpkg` 判断缺口，不保留旧百分比或行号。

### 10. Aphlict 配置文件并非所有键都生效

Go notification 读取监听与 cluster 字段，不实现 Aphlict ssl/logs/pidfile。TLS 在反代终止；容器 admin 的 listen 必须允许 PHP 访问。未知键仍可能被 JSON 解析静默忽略。

### 11. admin 根探针的已知协议差异

admin GET / 是平台 200 探针；client GET / 必须保持 501。不能为了统一探针而改变 client 协议，见 [notification](modules/notification.md)。

### 12. 多实例通知与重启重放边界

多实例需要文件中的 cluster peer 配置，没有对应 cluster 环境变量。history 仅存在于当前进程；重启不保留重放消息。

## mailer 模块

### 13. 邮件重试必须受外层等待预算约束

默认单适配器重试为 2 次、间隔 2 秒；旧独立服务的 250/15 不是当前默认。PHP 请求和任务外层重试的预算仍须一起考虑。见 [mailer](modules/mailer.md)。

### 14. Provider 假服务不等于真实供应商验收

SMTP TLS 与各 provider HTTP/MIME/签名边界应分别验证。HTTP fake 可以检验请求形状，不能证明真实供应商接受；当前测试覆盖以实际套件为准。

### 15. Mailer JSON 解析错误不必然终止进程

`GORGE_MAILER_CONFIG` 解析失败会记录日志，之后还可能从 GORGE_MAILER_TYPE/provider 环境构造后端；没有后端时 ready 失败。不能用 /healthz 或进程退出码证明配置正确。

## search 模块

### 17. Meilisearch filter 声明必须覆盖查询用法

exclude 使用的 id 必须在 filterableAttributes 内。当前实现已修复，测试要比较查询产生的属性与后端声明，不能拿同一函数同时生成实际值与期望值。

### 18. CJK mapping 变化需要重建

IndexIsSane 对照预期 mapping；分析器或字段类型变化可能使旧索引不再 sane。重建命令、破坏性和验收见 [search](modules/search.md)，ngrams 不是 Gorge 路径。

### 19. Search 配置解析失败与空后端

GORGE_SEARCH_BACKENDS JSON 解析错误记录日志；ready 的可读配置检查不能替代真实查询或索引完整性检查。

### 20. sane 请求必须包含 docTypes

当前 sane 拒绝空 docTypes，避免对空预期 mapping 报假成功。字段与请求形状见 [search](modules/search.md)。

### 21. 搜索域错误码没有 PHP 专用异常分支

SearchClient 采用通用 service exception，域错误码用于报告操作类别。不要因此删除 Go 的操作分类；PHP 是否需要分别处置应由实际调用行为决定。

### 46. Elasticsearch mapping 与目标版本配对

当前实现按版本区分 typed/typeless mapping，并使用 must_not 排除查询。升级 ES 或改变版本配置要重建并对真实后端验收；fake 的 200 不能证明 mapping 被接受。见 [search](modules/search.md)。

## file-storage 模块

### 22. blob 后端必须显式配置

未提供 MySQL host 时不启用 blob；默认启用的不是一个必然坏掉的连接。配置和引擎列表以 [file-storage](modules/file-storage.md) 为准。

### 23. 文件删除保持幂等

已经不存在的对象删除视为成功，非法 handle 与后端故障仍应区分。这是迟到重试和删除 outbox 恢复的前提。

### 24. 数据库连接池由域拥有

filestorage 在自己的 db.go 管理连接池。共享驱动不意味着共享 DSN、预算或事务；真正共享同一连接预算时再考虑平台抽象。

### 25. S3 流式写入需要正确签名策略

S3 PutObject 的不可 seek body 使用 unsignedPayload 策略。测试必须保留不可 seek reader 的场景；真实对象存储签名验收见 [operations](operations.md)。

### 26. 引擎回退受 rewind 预算约束

Router.Write 只在输入仍可完整重放时尝试下一后端；流式后端消耗超过预算后失败不会重试到另一个引擎。不能把部分前缀当完整文件重新上传。

### 27. 状态码通过不证明 handler 没有 panic

httpx.Fail 写成功时返回 nil。若 handler 继续执行并 panic，已应答保护可能保留原状态码；测试应检查 PANIC_RECOVERED 日志，后台任务同样需要失败可见性。

### 28. AWS 签名实现各有依赖边界

S3 使用 SDK，SES 使用自身 SigV4 实现。统一算法实现前评估流式重试与依赖成本，不因同一算法就强行共享生命周期。

### 29. 非法 handle 的读删错误不同

读非法 handle 返回 404；删除非法 handle 返回 400，删除已不存在返回成功。此问题已修复，保留为兼容依据而不是未完成待办。

### 43. 文件就绪与首次迁移不得闭环

blob ready 需要 Phorge 创建的 file 数据库，不能要求 migrate 在创建库前等它 ready。当前角色编排将迁移与消费者区分，升级时检查完整依赖图而非只看一条 depends_on。

## webhook 模块

### 30. Webhook claim 限制并发竞争

乐观 claim 与租约避免多 Go 实例同时领取一行。它不是第三方副作用的 exactly-once 保证；发出 POST 后回写失败仍可能重投。

### 31. Webhook 失败退避是额外候选条件

失败间隔与 claim lease 同时约束候选查询。不能将二者合并成一个条件，实际间隔见第 45 条。

### 32. Webhook 存活与就绪必须分开

healthz 不访问数据库，readyz 检查 herald 可达。stats/hooks 只读接口无法证明后台投递成功。

### 33. Webhook 内部错误不返回驱动细节

HTTP 500 使用通用信封文案，原始数据库错误只记录日志；不同于第 34 条的业务投递详情。

### 34. Webhook 传输错误仍写原始 errorCode

非超时网络错误的 err.Error() 写入请求 properties，可能包含目的主机或端口。若要收敛为短码，须同步 PHP UI 的展示语义。

### 35. 非法 properties JSON 无法原样保留

正常 JSON 的未知键随结果写回；非法 JSON 无法恢复其中内容，回写只保留可解析/本次结果字段。不要将这种损坏路径当成正常元数据更新。

### 36. 退出取消会影响在途结果回写

投递和 UpdateResult 使用运行 context；关闭期间取消可能导致结果写不回。租约到期是恢复路径，已发生的外部副作用可能重投，不能据此声称 exactly-once。

### 37. 全局静默仍在 PHP 生产边界处理

Go webhook 不读取 Phorge 全局配置。PHP 委派谓词检查 phabricator.silent，静默任务不走正常 Go 投递。当前 PHP 投递 worker 已退役，不应再描述成恢复原 PHP 网络发送。升级时验收静默任务的实际终态。

### 38. Webhook 消费者必须明确唯一

禁止旧 PHP 网络消费者和 Go 同时排空一张表。当前 fork 的 PHP 投递已退役，owner=phorge 不能恢复投递；Go 多实例扩容使用 claim，而非启动旧 worker。

### 39. Webhook 数据库也是域内资源

webhook 与 filestorage 的 DSN、事务和连接预算不同，没有因此新增 platform/db。重新抽象须有真实共享关切，参见第 24 条。

### 40. PHP 索引迁移与模型声明同步

autopatch 索引必须同步 CONFIG_KEY_SCHEMA，否则 storage adjust 可将其当 surplus key 删除。Herald 的 key_status 同时影响 Go 候选查询。

### 41. Webhook 首次启动不能等迁移后的库

migrate 创建 herald schema 前不能等待 webhook ready。Web 消费者启动门禁可在迁移完成后要求依赖就绪，两者不是同一种依赖强度。

### 42. Webhook claim 的驱动边界需真实 MySQL

SQL 字符串断言或内存 store 不能证明同秒版本推进、并发 UPDATE 的 RowsAffected 语义。应以真实 MySQL 并发竞争验证；旧覆盖率快照不作为当前测试范围依据。

### 44. Webhook 抢占与熔断的日志信噪比

CLAIM_LOST 为 Debug，默认日志级别不会输出；IN_ERROR_BACKOFF 为逐候选 Warn，可能重复刷屏。若增加观测，优先计数/状态变化汇总，不将正常竞争每行提升为 Info。

### 45. Webhook 重投间隔取较严的条件

UpdateResult 同时刷新 dateModified 与 lastRequestEpoch，实际失败间隔至少为 max(ClaimLease(), RetryBackoffSec)。将退避降到 lease 以下不会缩短重投；ClaimLease 还会抬到不低于投递超时。
