# Gorge 2026.10.08-r1

## 中文

本版本相对 `2026.10.07-r2` 加固投递、执行和恢复，并与 Phorge
`2026.10.08-r1` 配套使用。

- **邮件与搜索恢复。** 保留邮件投递结果未知的状态，限制供应商请求和 sendmail
  的时间、输出预算；修复 Elasticsearch 索引恢复，并统一 Elasticsearch / Meilisearch
  索引操作的超时预算。
- **常驻服务资源与执行权。** 通知连接使用有界发送队列，隔离慢连接，限制实例、
  消息和历史回放预算；改进 worker 调度、停机取消与租约保护，优化 diff 处理预算，
  对 HTTP 日志中的凭据脱敏。
- **数据库诊断。** db-api 契约升级到 `1.2`，区分连接健康和复制监控；单节点
  数据库跳过复制检查，保留真实连接错误及复制诊断。运行时先升级配对 Phorge PHP
  consumer，再升级 db-api 服务。
- **完整配对发布。** 固定两仓库源码，构建十四个多架构候选镜像，经配对验收生成
  `release-manifest.json` 后发布整套版本；发布入口按日期和整数修订号比较，避免旧构建
  晚完成时使 latest 倒退。发布工作流使用本文件的双语正文。
- **部署与维护说明。** 对齐通知服务端检查和浏览器 WebSocket 诊断、邮件结果、
  搜索投影、执行权、恢复与测试范围。

升级使用本 Release 附带的 `release-manifest.json`：固定两个源码 commit 和每个服务
的镜像 digest，再按 Phorge 的部署、备份、迁移和切换步骤验收。当前发布流程不生成
新的 `服务名-2026.10.08-r1` 或 `服务名-latest` 镜像标签；历史 Compose 默认标签
不能用于选择本版本，部署时须提供 digest override。

候选验收检查十四个镜像的包装，image/render 还以候选 digest 启动运行；其他服务
的业务验证使用固定配对源码的 Go/PHP 契约。双架构构建不代表两个架构均完成业务验收。
浏览器登录后的业务流程、真实邮件供应商投递与生产性能仍需在目标环境验证。

## English

This release hardens delivery, execution and recovery since `2026.10.07-r2`.
Use it with Phorge `2026.10.08-r1`.

- Preserve unknown mail delivery outcomes and bound provider requests and
  sendmail execution/output. Recover Elasticsearch indexes and share indexing
  deadlines across Elasticsearch and Meilisearch.
- Isolate slow notification connections with bounded writer queues and limit
  instances, messages and replay. Improve worker scheduling, shutdown cancellation
  and lease fencing, bound diff processing and redact HTTP log credentials.
- Upgrade the db-api contract to `1.2`, separating connection health from
  replication monitoring. Single-node databases skip replication checks while
  connection errors and replication diagnostics remain visible. Use the matching
  Phorge implementation, upgrading the PHP consumer before the db-api service.
- Build fourteen multi-architecture candidate images from an immutable source
  pair. Publish the complete version only after paired acceptance produces
  `release-manifest.json`. Calendar dates and integer revisions prevent late
  older builds from moving the release latest pointer backwards. The workflow
  publishes these bilingual notes.
- Align deployment and maintenance documentation with notification server and
  browser diagnostics, mail outcomes, search projections, ownership and recovery.

Deploy the two commits and per-service image digests in the attached manifest,
then follow Phorge's backup, migration and cutover checks. This workflow does
not create new `service-2026.10.08-r1` or `service-latest` image tags; provide a
digest override instead of selecting this version through historical Compose
defaults.

Acceptance checks packaging for all fourteen candidates and runs image/render
containers by their candidate digests. Other service behavior is tested through
paired Go/PHP source contracts. Multi-architecture builds do not prove business
acceptance on both architectures. Authenticated browser journeys, real mail
provider delivery and production performance require target-environment checks.
