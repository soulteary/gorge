# Gorge 2026.10.07-r2

发布准备稿；标签、镜像和 GitHub Release 尚未创建。范围为已发布的
`2026.10.07-r1` 至 `88928283bc1b50165df24e67eb61c6a7f92cf524`，正式标签须指向
本准备 PR 合并并通过检查后的 main 提交。[范围与发布步骤](docs/releases/2026.10.07-r2.md)。

## 中文

- **数据库观察结果与 Phorge 原生路径对齐。** schema 树不再包含没有表的数据库，
  同时保留其可访问性记录，避免将空数据库误判为权限不足；新增回归测试。
- **跨仓库测试改用 Phorge 内置 PHP runtime。** 固定配对源码提交，移除外部
  Arcanist checkout；测试前构建 parser 工具，关闭覆盖率驱动，并让 MySQL SQL mode
  与 Phorge 部署一致。历史迁移保留 surplus 处理，其他迁移错误仍会失败。
- **文档和 OpenAPI 与当前实现对齐。** 更新配置、路由、重试和持久状态说明，
  增加文档索引、端口、路由和本地链接检查，清理过期示例。

建议与 Phorge `2026.10.07-r2` 配套升级。先确认所有 Gorge 服务的固定版本镜像
发布成功，再更新 Phorge；按 Phorge 升级步骤备份、迁移并验收。
图片、清理和集成仍按各自模式及执行权配置启用。

本轮基础 CI 和真实 MySQL 的 native/Gorge 配对比较已通过；这是源码验证，
新标签镜像的构建与部署验收仍待发布时执行。

## English

Release preparation only: the tag, images and GitHub Release have not been created.
The reviewed range is `2026.10.07-r1` through
`88928283bc1b50165df24e67eb61c6a7f92cf524`; tag the checked main commit after
this preparation PR is merged.

- Align database schema observations with Phorge's native path: omit databases
  without tables from the schema tree while retaining visibility information.
  Regression coverage prevents empty databases from appearing access denied.
- Run cross-repository tests with Phorge's bundled PHP runtime and an immutable
  paired source commit. Build parser tools before testing, disable coverage
  drivers and align MySQL SQL mode with the deployment configuration. Historical
  migrations allow surplus warnings while other migration failures remain fatal.
- Refresh configuration, API, retry and durable-state documentation and OpenAPI
  descriptions. Add documentation index, port, route and local-link checks.

Upgrade with Phorge `2026.10.07-r2` after every required fixed-version Gorge
image has been published. Follow Phorge's backup, migration and acceptance
steps. Image, maintenance and integration capabilities keep their explicit
mode and ownership controls. Main CI and the real-MySQL paired comparison have
passed; release-image builds and deployment acceptance are still pending.
