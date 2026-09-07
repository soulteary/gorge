# Gorge 轻量测试 demo

一份**自包含、跑起来就能验**的 docker compose，把 gorge 生产编排刻意留给使用者
自建的后端一起拉起来，用来快速联调三个真正依赖外部后端的 gorge 服务：

| gorge 服务 | 端口（回环） | 依赖的后端（本 demo 一并拉起） |
|---|---|---|
| `gorge-search` | 8120 | **Meilisearch** + **Elasticsearch**（同时挂两个后端，fan-out 写、失败回落读） |
| `gorge-mailer` | 8110 | **OwlMail**（测试邮箱，SMTP 接收 + Web UI） |
| `gorge-webhook` | 8160 | **MySQL**（预置最小 herald 库）+ **soulteary/webhook**（投递接收端） |

> 这不是生产编排。生产编排在 `deploy/compose/docker-compose.yml`，它**刻意不**声明
> 搜索集群等存储后端（原因见该文件注释）。本 demo 把它们塞进来只是为了本机验证，
> 数据都在命名卷里，`down -v` 一并清掉。

## 目录

```
demo/
├── docker-compose.demo.yml   编排本体
├── .env.demo                 环境变量（cp 成 .env）
├── seed/herald.sql           MySQL 首次初始化时建 herald 库 + 塞一条 hook/请求
└── hooks/hooks.json          soulteary/webhook 接收端的 hook 定义
```

## 启动

```bash
cd deploy/compose/demo
cp .env.demo .env
docker compose -f docker-compose.demo.yml up -d --build
```

首次会构建三个 gorge 镜像并拉起五个后端。等各服务健康（约 30–60s）：

```bash
docker compose -f docker-compose.demo.yml ps
```

## 逐项验证

### 1. 搜索（Meilisearch + Elasticsearch）

`gorge-search` 的 `/readyz` 通过即表示两个后端都已配好可读：

```bash
curl -s http://127.0.0.1:8120/readyz        # {"status":"ok"}
```

跑仓库自带的搜索冒烟脚本（会建/删索引，写入 fan-out 到 ES 与 Meili 两边，
用真后端能验到 CJK 分析器那两条用例）：

```bash
cd ../../..                                   # 回到 gorge 仓库根
BASE_URL=http://127.0.0.1:8120 bash tests/e2e/search.sh
```

也可直接看两个后端：

```bash
curl -s http://127.0.0.1:9200/_cat/indices?v                        # ES
curl -s -H "Authorization: Bearer demo-master-key" \
     http://127.0.0.1:7700/indexes                                   # Meili
```

### 2. 邮件（OwlMail）

`gorge-mailer` 把邮件投递到 OwlMail，浏览器打开收件箱看结果：

```bash
BASE_URL=http://127.0.0.1:8110 bash tests/e2e/mailer.sh   # 仓库根目录下
open http://127.0.0.1:1080                                # OwlMail Web UI
```

OwlMail 也提供 API：`curl -s http://127.0.0.1:1080/email` 可拉取收到的邮件做断言。

### 3. Webhook（MySQL 队列 → soulteary/webhook 接收端）

seed 已在 `phabricator_herald` 库里塞了一个指向接收端的 hook 和一条 `queued` 请求。
`gorge-webhook` 起来后每秒轮询一次，几秒内就会把它投递出去：

```bash
curl -s http://127.0.0.1:8160/readyz                      # {"status":"ok"} = 连得上 herald 库
docker compose -f docker-compose.demo.yml logs gorge-webhook | grep WEBHOOK_DELIVERED
docker compose -f docker-compose.demo.yml logs webhook-receiver
```

看到接收端日志里出现 `gorge-webhook 投递成功` 那行，就说明整条链路通了：
MySQL 队列 → gorge-webhook 抢占 → 带 `X-Phabricator-Webhook-Signature`(HMAC-SHA256)
的 POST → soulteary/webhook 执行 hook。

想再触发一次投递，往队列里再插一行：

```bash
docker compose -f docker-compose.demo.yml exec mysql \
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

## 清理

```bash
docker compose -f docker-compose.demo.yml down -v      # 连命名卷一起删
```

## 说明与注意

- 所有对宿主发布的端口都绑 `127.0.0.1`，只供本机排障；服务间一律用 compose
  网络里的**服务名**互访（`meili:7700` / `es:9200` / `owlmail:1025` /
  `webhook-receiver:9000` / `mysql:3306`）。
- `GORGE_SERVICE_TOKEN` 默认留空 = 不鉴权。要验鉴权就在 `.env` 里设一个值，
  调用时带 `X-Service-Token`（或 `?token=`）。
- `GORGE_WEBHOOK_NAMESPACE` 必须与 `seed/herald.sql` 建的库名前缀一致
  （默认都是 `phabricator` → `phabricator_herald`）；改一处要改两处。
- seed 只在 **MySQL 数据卷为空**时执行一次。改了 `seed/` 想重来，先 `down -v`。
- `es` 单节点集群会停在 `yellow`（副本分片没处放），属正常，不影响使用。
