# 图片计算

`gorge-image`（:8190）计算 JPEG/PNG/GIF/WebP 图片，PHP 继续拥有文件权限、secret key、元数据和派生关系。默认部署不切换；独立运行隔离图片 CPU/内存。

## 接口与配置

仅接受 `X-Service-Token`，启动必须有非空 `GORGE_SERVICE_TOKEN`。

- `GET /api/image/capabilities`：协议1、recipeRevision、backendRevision、格式、限制与预设。
- `POST /api/image/probe`：原始二进制，返回 MIME、画布尺寸、完整帧数及动画状态。
- `POST /api/image/transform?recipe=preview&revision=phorge-v1&animation=legacy-static`：二进制输入/输出；成功含 MIME、ETag、实际宽高和版本响应头，错误使用平台 envelope。

预设：profile400×400允许放大；pinboard280×210不放大；thumbgrid最长边100；preview最长边220；workcard最长边526允许放大。单边模式四分之一短边下限并透明留白，固定尺寸居中裁剪。55组PHP浮点/整数几何oracle存放在testdata/geometry.json。

Go管理受限ImageMagick子进程，没有任意URL/命令入口。二进制由`GORGE_IMAGE_BINARY`指定（默认convert），`GORGE_IMAGE_POLICY_DIR`默认`/etc/gorge/image`；启动验证四种codec。并发默认2（1..8），`GORGE_IMAGE_TIMEOUT_SEC`默认10（1..10），超时终止进程组并清理独立临时目录。策略限制内存/map/临时盘各256MiB、线程1；容器默认2GiB内存、2CPU、512MiB临时盘。容器上限不是逐任务RSS保证，极大输入可能被策略拒绝。

输入≤16MiB，画布≤50,135,040像素；动画≤100帧，累计画布像素同样≤50,135,040。这比旧实现增加动画资源保护，不能宣称任意旧动画都兼容。只允许JPEG/PNG/GIF/WebP/INFO coder，禁delegate/filter。`legacy-preserve`只为≤512×512的GIF保留动画，其余单帧；非GIF动画不在首版动画保留范围。

进程缓存32条/64MiB/5分钟，以输入SHA256、预设、动画政策、recipe/backend版本为键，合并同一计算。仅单Token单实例服务，认证`GET /api/image/stats`提供计算次数、命中次数、合并等待及缓存条目/字节；冷计算记录耗时日志。缓存不是永久存储、没有跨副本唯一执行保证。失败不缓存；取消请求不会返回缓存结果，同一计算的等待者也受服务超时限制。输出重新探测并核对 MIME、尺寸与帧数。BodyLimit在框架缓冲层生效；大量并发请求仍需要入口并发限制，执行槽不等于全局请求内存限制。

## PHP灰度

`gorge.image.uri/token`、`gorge.image.mode=legacy|shadow|gorge`，默认legacy。shadow仅在派生缓存未命中的路径按源PHID稳定抽样，不主动重建历史结果；`gorge.image.shadow-percent`默认10；只持久化旧结果，记录新结果尺寸/MIME差异。gorge模式图片计算与上传尺寸探测调用Go，仍通过PHP创建派生文件。除413/415/422输入拒绝外，HTTP失败（包括认证和协议配置错误）、传输失败及结果契约校验失败显示默认图，不写成功关联；PHP核对输出几何、MIME、版本、长度及SHA256，并验证探测元数据。超过16MiB的文件在读取原始存储数据前拒绝变换及尺寸探测；显式regenerate失败保留旧结果。重生成先生成，再事务锁定替换原关联，最后删除旧文件。

Opt-in bundled deployment：在phorge-fork设置非空GORGE_IMAGE_TOKEN，执行：

```sh
docker compose -f docker-compose.yml -f docker-compose.image.yml up -d --build
```

migrate角色将URI/token/mode写入deployment配置，默认shadow。实际切换前验收生产样本，再设GORGE_IMAGE_MODE=gorge并重新生成部署配置。Go-only部署使用`docker compose --profile image ...`且设置GORGE_SERVICE_TOKEN。

现有派生图直接沿用，不重建历史、不改secret URL、不要求旧存储数据先迁移。PHP保留legacy用于灰度回滚；GD还用于内置头像、图标、Meme和SpriteSheet，不能删除GD扩展。首版只检查几何/MIME与动画结构，重采样、JPEG质量、GIF调色板等像素差异仍须shadow样本人工验收；libvips替换与可信blob引用是后续优化，未实施。

## 验证

```sh
GORGE_TEST_IMAGE_URL=http://127.0.0.1:18150 GORGE_TEST_IMAGE_TOKEN=... go test -race ./internal/imagetransform
GORGE_TEST_ARCANIST_DIR=... GORGE_TEST_IMAGE_URL=... GORGE_TEST_IMAGE_TOKEN=... php tests/contract/image/runtime.php
```

真实后端测试覆盖四种格式、五预设、GIF帧数/时间/循环、静态政策、损坏/不支持格式；本地无URL时集成套件跳过，CI必须启动实际镜像。PHP geometry导出脚本可以刷新oracle，刷新需连同算法变更评审。

尚未完成的退役验收：真实 MySQL 下并发 regenerate 和源文件删除竞态、生产样本像素差异验收、入口请求并发限制，以及头像/图标/Meme/SpriteSheet 的独立迁移。以上完成前保留 PHP legacy 和 GD。

## Additional recipes: Meme and builtin composition

`POST /api/image/meme?revision=meme-v1&animation=legacy-static|legacy-preserve`
accepts the original binary image, plus base64 UTF-8 `X-Gorge-Meme-Above` and
`X-Gorge-Meme-Below` headers. Total text is limited to 4096 bytes and 16 lines per
block; controls other than newline are refused. Go draws the text into a PNG
layer, so text never enters ImageMagick expressions. The service fits fonts
between 5 and 72 points, centers each line and draws a black outline. Oversized
text/canvases and missing glyphs fail explicitly. This is a versioned new layout,
not a claim of pixel equality with PHP GD.

The default font is embedded Go Bold. An operator may set
`GORGE_IMAGE_MEME_FONT` to a mounted trusted TTF/OTF (maximum 8 MiB) for other
scripts. The SHA-256 font revision is advertised in capabilities and included
with the backend revision in the PHP Meme cache key. GIF preservation retains
frames, delay and loop behavior within the total-frame pixel budget. The canvas
limit is 16 Mi pixels; output uses the source JPEG/PNG/GIF/WebP format.

`POST /api/image/compose` accepts JSON `{revision:"compose-v1", recipe,
background, border, width, height, layers}`. Each layer is base64 binary image.
Recipes are fixed: `avatar` (400x400, RGBA border), `icon` (200x200), `favicon`
(square 16/32/64/128, base then top-right/bottom-right/bottom-left/top-left emblems).
Background is six hex RGB digits, optionally prefixed with `#`. Avatar border is
[R,G,B,A] with alpha 0..1. Empty emblem strings preserve corner positions.
Favicons start with a transparent canvas; their background value is ignored.
Composition outputs PNG, uses bounded Go decoders and does not invoke a shell.
Maximum five layers, 8 MiB total compressed bytes, 16 Mi pixels decoded total.
Favicon resampling uses Catmull-Rom; compare visual output before cutover.

Phorge `GORGE_IMAGE_MEME_MODE` / `gorge.image.meme-mode` and
`GORGE_IMAGE_BUILTIN_MODE` / `gorge.image.builtin-mode` independently select
legacy/shadow/gorge. Defaults remain legacy. Shadow stores only the legacy
result; Gorge mode requires the versioned capability and validates binary
geometry, MIME, revision and digest before persistence. Builtin/favicons cache
keys include the rollout/recipe revision. Existing generated files are retained.
Runtime sprite generation is not part of these endpoints; sprite build tooling
remains a separate build concern.
