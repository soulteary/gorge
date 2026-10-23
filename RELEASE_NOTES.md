# Gorge 2026.10.23-r2

## 中文

本版本与包含 PHP 8.3.36 和 session 加固的 Phorge `2026.10.23-r2` 配对。
修正发布依赖陈旧仓库变量而配对旧 Phorge 的问题：从标签源码中的
`deploy/release/source-pair.json` 读取唯一不可变 Phorge 提交，拒绝不一致的手动输入，
并在候选构建前验证实际 PHP 镜像版本与生效的 session 设置。

修正配对 Phorge 的 Debian snapshot 为 `20261023T000000Z`，使锁定 PHP 8.3.36
基础镜像与 curl 开发包版本一致。新增安全 CI 已按正式参数完整构建 Phorge 应用镜像，
并在镜像内验证 PHP 与安装后的 session 设置。`2026.10.23-r1` 因旧快照依赖冲突
在验收基础镜像构建阶段失败，未公开发布；其 tag 保留不可变。

保留 Go 1.27.3、十四项多架构候选镜像、完整真实后端配对验收和 manifest 门禁。
`2026.10.22-r1/r2` 保持不可变，不构成本版 PHP 安全交付的证明。
按本 Release 的 `release-manifest.json` 固定两个源码提交与逐服务 digest。
浏览器业务、真实邮件投递和生产性能仍须在目标环境另行验收。

## English

Pair with Phorge `2026.10.23-r2`, containing PHP 8.3.36 and hardened sessions.
Resolve the exact Phorge commit from the tag-owned source-pair lock, reject a conflicting
manual override, and verify the effective PHP runtime baseline before building.
This prevents a stale repository variable from silently selecting the old Phorge source.
Align the paired Phorge Debian snapshot at `20261023T000000Z` with the patched PHP
base image. Production-parameter image builds now verify the installed PHP/session
settings in CI. The `2026.10.23-r1` attempt failed its base-image build because of the old
snapshot and was not published; its tag remains immutable.

Keep Go 1.27.3, all fourteen multi-platform candidates, mandatory paired real-backend
acceptance and manifest publication gates. Published `2026.10.22-r1/r2` remain immutable.
Deploy the source commits and service digests recorded in the attached manifest.
Browser journeys, provider delivery and production performance require deployment checks.

---

# Gorge 2026.10.22-r1

## 中文

本版本与 Phorge `2026.10.22-r1` 配套，基于 `2026.10.08-r1`。

- **工具链与依赖。** 更新 Go 模块依赖、容器构建所用的锁定 Go/Alpine 基础镜像以及平台校验。
- **持续集成与质量报告。** 调整 CI 和跨仓库检查，新增可重复运行的 Go 测试、覆盖率和 Go Report Card 报告产物与徽章。
- **发布门禁。** 持续维护固定的 Phorge/Gorge 源码配对验证、发布基础镜像约束和交付文档。
- **安全配对。** 必须与包含 PHP2Shell 修复（CVE-2026-109618 / CVE-2026-109619）的 Phorge `2026.10.22-r1` 一起部署。本次 Gorge 不声称直接修复 PHP Core。

本版按 Release 工作流构建十四项多架构候选镜像，完成配对验收后发布 `release-manifest.json`；只有清单中的不可变提交与镜像 digest 是部署依据。不会提升独立服务的历史 latest 标签。完整浏览器业务、真实邮件投递及生产性能仍须另行验收。

## English

This release pairs with Phorge `2026.10.22-r1` and supersedes `2026.10.08-r1`.

- Refresh Go module dependencies and locked Go/Alpine build images, including platform verification.
- Improve CI and cross-repository checks; add reproducible Go test, coverage, Go Report Card outputs and badges.
- Maintain immutable paired-source verification, release-image constraints and delivery documentation.
- Require the matching Phorge release containing the PHP2Shell (CVE-2026-109618 / CVE-2026-109619) fix. Gorge itself does not patch PHP Core.

The Release workflow must build fourteen multi-architecture candidate images and complete paired acceptance before it publishes `release-manifest.json`. Deploy exact commit pairs and image digests from that manifest, not historical latest tags. Authenticated browser journeys, real provider delivery and production performance remain separate operational checks.

---

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
