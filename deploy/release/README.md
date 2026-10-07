# 配对发布门禁

发布必须提供已存在的 `YYYY.MM.DD-rN` tag，以及 `PHORGE_CONTRACT_REF`（或手动输入
`phorge_ref`）中的完整 Phorge commit SHA。流程不再从 main 隐式取代码，也不更新各服务
的 `*-latest` 标签。

顺序为：固定源码对 → 契约和静态检查 → 十四个多架构候选镜像 → 每个镜像的源码标签、
非 root 用户、可执行文件和健康端口验证 → 候选 image/render 的真实运行与完整配对验收
→ `release-manifest.json` → 发布 GitHub Release。MySQL、Redis、S3、Elasticsearch、
Meilisearch 和图片服务的必测用例被跳过时，验收失败。

GitHub Release 是整套版本的发布入口。草稿在最终一步前不可见；任何构建或验收失败都
不会提升入口。已存在的版本禁止覆写。候选标签包含 run ID 与 attempt，最终部署使用清单
中的镜像 digest 和两个 commit，不从候选标签或独立的历史 `*-latest` 推断一套版本。

所有版本的最终发布任务共用仓库级互斥锁。锁内先读取当前 GitHub Release `latest`，
按真实日历日期和整数修订号比较 `YYYY.MM.DD-rN`（例如 `r10` 大于 `r9`）。草稿先以
`--draft=false --latest=false` 发布，只有版本严格更新时才提升 `latest`；旧版本晚完成仍
可发布，但不会让入口倒退。只有明确 HTTP 404 才视为没有既有 release；认证、限流、
无效响应和未知旧版本格式都会在写入前停止。每个 GitHub CLI 调用限时 30 秒。

此锁覆盖本工作流中的发布；其他自动化也必须使用同一个发布锁，不应另行修改入口。
命令超时或失败可能留下隐藏草稿，或已发布但尚未提升的版本。流程保留该状态并失败，
需先核对 GitHub 实际状态再处理，不能自动覆写既有版本。

清单明确记录验收未覆盖的浏览器业务流程、真实邮件供应商投递和生产性能；这些仍是切流
前的环境验收。每个镜像均验证当前运行架构的包装，多架构编译由 Buildx 完成；这不代表
另一架构已经完成全部运行时验收。

本地门禁负面测试：`python3 -m unittest discover -s deploy/release -p '*_test.py'`。
发布需要先提交两个仓库的配套修改，并更新固定 Phorge revision。旧 Phorge 版本缺少候选
镜像与必测验收入口时会失败，不能降级绕过。
