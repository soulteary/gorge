# Gorge 本地联调测试手册

这份文档是 [`README.md`](README.md) 的补充：README 说明**怎么启动** demo，本文档说明
**怎么系统性地做一遍联调、怎么读结果、遇到问题怎么排查**。面向的是「想快速把
`gorge-search` / `gorge-mailer` / `gorge-webhook` 三条真实链路验一遍」的人。

所有命令都假设你在 gorge 仓库根 `gorge/` 下，除非另有说明。demo 编排位于
`deploy/compose/demo/`，本文用 `COMPOSE` 代指下面这条前缀：

```bash
export COMPOSE="docker compose -f deploy/compose/demo/docker-compose.demo.yml"
```

---

## 0. 这套 demo 联调的是什么

生产编排（`deploy/compose/docker-compose.yml`）**刻意不**声明搜索集群、邮箱、webhook
接收端这些后端。这份 demo 把它们一起拉起来，只为本机联调，专门覆盖三个**真正依赖
外部后端**的服务：

| gorge 服务 | 宿主端口（回环） | 依赖后端（demo 一并拉起） | 冒烟脚本 |
|---|---|---|---|
| `gorge-search` | 8120 | Meilisearch(`meili:7700`) + Elasticsearch(`es:9200`)，双写 fan-out | `tests/e2e/search.sh` |
| `gorge-mailer` | 8110 | OwlMail(`owlmail:1025` SMTP / `:1080` Web UI) | `tests/e2e/mailer.sh` |
| `gorge-webhook` | 8160 | MySQL(`mysql:3306`，预置 herald 库) + webhook 接收端(`webhook-receiver:9000`) | `tests/e2e/webhook.sh` |

> `gorge-render` / `gorge-notification` / `gorge-file-storage` 不在这套 demo 里——它们
> 是纯计算或不依赖外部后端，用生产编排或 `make run` 单独起即可。

所有对宿主发布的端口都绑 `127.0.0.1`，仅供本机排障；**服务之间一律用 compose 网络里的
服务名互访**，与宿主端口无关。

---

## 1. 一键启动

```bash
cd deploy/compose/demo
cp .env.demo .env          # 首次；compose 读同目录的 .env
docker compose -f docker-compose.demo.yml up -d --build
```

首次会构建三个 gorge 镜像并拉起五个后端（meili / es / owlmail / webhook-receiver /
mysql），随后启动三个 gorge 服务。等全部就绪约 30–60s。

**确认八个容器都 `Up`**（不是 `Created`）：

```bash
$COMPOSE ps
```

期望看到 8 个容器，STATUS 均为 `Up`。若有服务停在 `Created`，见 [§5 排查](#5-常见问题排查)。

---

## 2. 就绪探针（联调第一关）

三个服务的 `/readyz` 全部返回 `{"status":"ok"}`，就表示后端都接上了：

```bash
for p in 8120 8110 8160; do printf "%s -> " "$p"; curl -s "http://127.0.0.1:$p/readyz"; echo; done
```

`/readyz` 与 `/healthz` 的区别很重要，联调时按这个理解读状态：

- `/healthz`：进程活着就 200。**它不代表能干活。**
- `/readyz`：真正的就绪条件——search 至少一个可读后端、mailer 至少配了一个后端、
  webhook 能连上 `{namespace}_herald` 库。**联调看这个。**

---

## 3. 三条链路逐项验证

### 3.1 搜索（Meilisearch + Elasticsearch）

```bash
BASE_URL=http://127.0.0.1:8120 bash tests/e2e/search.sh
```

- ⚠️ **有破坏性**：脚本第 5 步会 `POST /api/search/init`，先删索引再重建。只对这种
  一次性 demo 跑，别指向有真实数据的实例。
- 期望 **18 passed, 0 failed**。其中「两字中文查询」「`登录跳转` 只命中中文文档」两条
  是这套 demo 相对内存后端多出来的价值——它们验证的是真后端的 **CJK 分析器链**，
  内存 `test` 后端只会假装通过。

直接看两个后端的数据：

```bash
curl -s http://127.0.0.1:9200/_cat/indices?v                                    # ES
curl -s -H "Authorization: Bearer demo-master-key" http://127.0.0.1:7700/indexes # Meili
```

> `es` 单节点集群会停在 `yellow`（副本分片没处放），属正常，不影响使用。

### 3.2 邮件（OwlMail）

```bash
BASE_URL=http://127.0.0.1:8110 bash tests/e2e/mailer.sh
```

期望 **7 passed, 0 failed**。脚本会实际发一封带附件的邮件，可在收件箱里核对：

```bash
open http://127.0.0.1:1080                 # OwlMail Web UI
curl -s http://127.0.0.1:1080/email        # API，可用于脚本断言
```

应能看到主题 `gorge e2e attachment`、带 `note.txt` 附件的那封。

### 3.3 Webhook（MySQL 队列 → 接收端）

```bash
BASE_URL=http://127.0.0.1:8160 bash tests/e2e/webhook.sh
```

期望 **9 passed, 0 failed**（token 为空时会跳过两条鉴权相关场景；设了 `TOKEN` 则更多）。
但要理解这个脚本的局限：它一共 11 个场景，覆盖两个**只读**端点、两个探针、一条跨端点
不变量，外加鉴权（缺 token 401 / query-param token 兜底）、只读拒绝（POST/PUT/DELETE 均
被拒）、未知路径仍走 `{data,error}` 信封、以及 hooks 端点不泄露 URI/HMAC 等断言；但
**碰不到投递本身**——投递是后台轮询队列触发的，没有任何请求能启动它。所以「真实投递
通没通」要单独看下面这步。

**验证真实投递链路**（seed 已预置一条 `queued` 请求，gorge-webhook 每秒轮询）：

```bash
$COMPOSE logs gorge-webhook | grep -E "WEBHOOK_DELIVERED|WEBHOOK_DELIVERY_REJECTED"
```

- `WEBHOOK_DELIVERED ... status=200` = 整条链路通了：
  MySQL 队列 → 抢占 → HMAC-SHA256 签名 POST → 接收端执行 hook → 回写 `sent`。
- 也可以直接查队列表确认落库状态：

```bash
$COMPOSE exec mysql \
  mysql -uphorge -pphorge phabricator_herald \
  -e "SELECT id, status, lastRequestResult FROM herald_webhookrequest ORDER BY id;"
```

**再触发一次投递**（往队列里插一行 `queued`，几秒内会被投出）：

```bash
$COMPOSE exec mysql \
  mysql -uphorge -pphorge phabricator_herald -e "
    INSERT INTO herald_webhookrequest
      (phid, webhookPHID, objectPHID, status, properties,
       lastRequestResult, lastRequestEpoch, dateCreated, dateModified)
    VALUES
      (CONCAT('PHID-HWBR-', LPAD(FLOOR(RAND()*1e12),20,'0')),
       'PHID-WHOK-demohook0000000001', 'PHID-TASK-demoobject00000001',
       'queued', '{\"retry\":\"never\"}', 'none', 0,
       UNIX_TIMESTAMP(), UNIX_TIMESTAMP());"
```

---

## 4. 一次性全量验证

三条脚本连着跑（注意 search.sh 会重建索引）：

```bash
BASE_URL=http://127.0.0.1:8120 bash tests/e2e/search.sh
BASE_URL=http://127.0.0.1:8110 bash tests/e2e/mailer.sh
BASE_URL=http://127.0.0.1:8160 bash tests/e2e/webhook.sh
```

全绿的判定：search 18/18、mailer 7/7、webhook 9/9，且 gorge-webhook 日志里出现
`WEBHOOK_DELIVERED ... status=200`。

**验鉴权（可选）**：demo 默认 `GORGE_SERVICE_TOKEN=` 为空 = 不鉴权，所以脚本里的 401
场景都是 SKIP。想验鉴权，在 `.env` 里给 `GORGE_SERVICE_TOKEN` 设一个值、重启，再带
`TOKEN=` 跑脚本：

```bash
# .env 里设 GORGE_SERVICE_TOKEN=dev-token 后
$COMPOSE up -d
TOKEN=dev-token BASE_URL=http://127.0.0.1:8120 bash tests/e2e/search.sh
```

---

## 5. 常见问题排查

### 5.1 服务停在 `Created`，端口连不上（`curl: (7) Couldn't connect`）

`$COMPOSE ps -a` 看到某些容器 STATUS 是 `Created` 而非 `Up`，多半是上一次
`up -d` 在等健康检查时被中断（Ctrl-C / 超时），依赖服务被创建但没启动。

**直接重跑一次即可**（镜像已在，不必 `--build`）：

```bash
$COMPOSE up -d
```

### 5.2 端口被占用（`bind: address already in use`）

典型是 **OwlMail 的 1025 / Web 1080**，或 MySQL 3306 与你机器上已有的服务冲突。
先定位谁占了端口：

```bash
lsof -nP -iTCP:1025 -sTCP:LISTEN     # 换成报错里的端口
docker ps -a --format '{{.Names}}\t{{.Ports}}' | grep 1025
```

两种解法：

1. **停掉占用端口的旧容器/进程**（若那是个你不再需要的常驻服务）。
2. **改 demo 的宿主端口**：这些宿主端口只用于本机排障，gorge-mailer 连 OwlMail 走的是
   内网 `owlmail:1025`（容器端口，不受宿主映射影响），所以改宿主端口不影响联调链路。
   在 `.env` 里改对应变量后 `$COMPOSE up -d`：

   | 变量 | 默认 | 用途 |
   |---|---|---|
   | `OWLMAIL_SMTP_PORT` | 1025 | OwlMail SMTP（宿主直连排障用） |
   | `OWLMAIL_WEB_PORT` | 1080 | OwlMail Web UI |
   | `MYSQL_PUBLISH_PORT` | 3306 | MySQL |
   | `MEILI_PUBLISH_PORT` / `ES_PUBLISH_PORT` | 7700 / 9200 | 两个搜索后端 |
   | `GORGE_SEARCH_PUBLISH_PORT` / `GORGE_MAILER_PORT` / `GORGE_WEBHOOK_PORT` | 8120 / 8110 / 8160 | 三个 gorge 服务 |

### 5.3 webhook 那条 seed 请求显示 `failed`

如果 `SELECT ... FROM herald_webhookrequest` 里 id=1 那条是 `failed`，而 `WEBHOOK_DELIVERED`
只对后来插入的请求出现 200，那是**首次启动竞态**：seed 请求在接收端（`webhook-receiver`
带 `-hotreload`）还没热加载完 `hooks.json` 时就被投出，接收端回了 500；又因为 seed 的
`properties` 是 `{"retry":"never"}`，失败即退、不重投，于是永久停在 `failed`。

这**不是 bug**，链路本身是好的。验证方法：手动往队列插一条新请求（见 §3.3），它会
投递成功变 `sent`。想让 id=1 也变绿，最干净的办法是 §6 的 `down -v` 重来——这次接收端
已就绪，首投就会成功。

### 5.4 手动验接收端是否活着

绕过队列，直接在 compose 网络里 POST 接收端的 hook 端点：

```bash
docker run --rm --network gorge-demo_gorge-demo curlimages/curl:latest \
  -s -w "\nHTTP=%{http_code}\n" -X POST \
  -H "Content-Type: application/json" --data '{"test":"manual"}' \
  http://webhook-receiver:9000/hooks/gorge-demo
```

期望 `HTTP=200` 并打印 hook 输出。hook 定义在 `hooks/hooks.json`，投递目标 URI 在
`seed/herald.sql`（`http://webhook-receiver:9000/hooks/gorge-demo`）。

### 5.5 改了 seed 不生效

`seed/herald.sql` 只在 **MySQL 数据卷为空时执行一次**。改了 seed 想重来，必须先
`down -v` 清卷再 `up`（见 §6）。另外 `GORGE_WEBHOOK_NAMESPACE`（`.env`，默认
`phabricator`）必须与 seed 建的库名前缀 `phabricator_herald` 一致，改一处要改两处。

---

## 6. 清理与重来

```bash
$COMPOSE down          # 停容器，保留数据卷（索引、邮件、herald 库都还在）
$COMPOSE down -v       # 连命名卷一起删（meili_data / es_data / mysql_data），彻底重来
```

想要一个「seed 首投即成功、无历史 failed」的干净环境，用 `down -v` 后重新
`up -d --build` 即可。

---

## 附：端口与服务名速查

| 组件 | 宿主端口（回环） | compose 服务名:端口 | 备注 |
|---|---|---|---|
| gorge-search | 8120 | `gorge-search:8120` | `/readyz` `/healthz` `/api/search/*` |
| gorge-mailer | 8110 | `gorge-mailer:8110` | `/readyz` `/healthz` `/api/mailer/*` |
| gorge-webhook | 8160 | `gorge-webhook:8160` | `/readyz` `/healthz` `/api/webhook/{stats,hooks}` |
| Meilisearch | 7700 | `meili:7700` | 主密钥 `demo-master-key` |
| Elasticsearch | 9200 | `es:9200` | 单节点，正常停在 yellow |
| OwlMail | 1080(Web)/1025(SMTP) | `owlmail:1080` / `owlmail:1025` | 收件箱 API：`/email` |
| webhook 接收端 | 9000 | `webhook-receiver:9000` | hook: `/hooks/gorge-demo` |
| MySQL | 3306 | `mysql:3306` | 库 `phabricator_herald`，账号 `phorge/phorge` |
