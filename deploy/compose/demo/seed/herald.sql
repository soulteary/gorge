-- Gorge demo —— gorge-webhook 需要的最小 herald schema + 种子数据
-- ==================================================================
--
-- 这**不是** Phorge 的完整数据库，只是让 gorge-webhook 跑起来所需的最小子集：
--   1) 一个名为 phabricator_herald 的库（库名前缀必须与 GORGE_WEBHOOK_NAMESPACE 一致）
--   2) 两张表：herald_webhook（hook 定义，服务只读）
--             herald_webhookrequest（投递队列，服务读 + 写回结果）
--   3) 一条指向 demo 里 webhook-receiver 的 hook + 一条 status='queued' 的请求
--
-- 列名、大小写严格照 gorge/go/internal/webhook/{model,store}.go 里的 SQL 写，
-- 改动任何列名都会让 Scan/查询失败。
--
-- 首次初始化（mysql_data 卷为空）时 MySQL 的 entrypoint 会自动执行本文件。
-- 想重跑：docker compose -f docker-compose.demo.yml down -v 后再 up。

CREATE DATABASE IF NOT EXISTS `phabricator_herald`
  CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;

-- 普通账号（compose 里的 MYSQL_USER）默认只拿到自己那个库的权限，
-- 这里显式把 phabricator_herald 的权限授给它，让 gorge-webhook 连得进来。
GRANT ALL PRIVILEGES ON `phabricator_herald`.* TO 'phorge'@'%';
FLUSH PRIVILEGES;

USE `phabricator_herald`;

-- herald_webhook：hook 定义。gorge-webhook 只读它（GetWebhook / Stats / CountHooks）。
-- 服务实际用到的列：phid, name, webhookURI, status, hmacKey（外加 id）。
CREATE TABLE IF NOT EXISTS `herald_webhook` (
  `id`         INT UNSIGNED NOT NULL AUTO_INCREMENT,
  `phid`       VARBINARY(64)  NOT NULL,
  `name`       VARCHAR(255)   NOT NULL,
  `webhookURI` LONGTEXT       NOT NULL,
  -- status 取 'enabled' / 'disabled' / 'firing' 等；只有 'disabled' 会被特殊对待。
  `status`     VARCHAR(32)    NOT NULL,
  `hmacKey`    VARBINARY(255) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `key_phid` (`phid`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- herald_webhookrequest：投递队列。gorge-webhook 读候选行、抢占（更新 dateModified）、
-- 投递后写回 status/lastRequestResult/lastRequestEpoch/properties/dateModified。
-- 关键：dateCreated = dateModified 表示「未被抢占」，poll 会立刻把它当候选。
CREATE TABLE IF NOT EXISTS `herald_webhookrequest` (
  `id`                INT UNSIGNED NOT NULL AUTO_INCREMENT,
  `phid`              VARBINARY(64) NOT NULL,
  `webhookPHID`       VARBINARY(64) NOT NULL,
  `objectPHID`        VARBINARY(64) NOT NULL,
  -- status 只在 'queued' / 'failed' / 'sent' 之间取值（不可扩展，见 model.go）。
  `status`            VARCHAR(32)   NOT NULL,
  `properties`        LONGTEXT      NOT NULL,
  `lastRequestResult` VARCHAR(32),
  `lastRequestEpoch`  INT UNSIGNED,
  `dateCreated`       INT UNSIGNED  NOT NULL,
  `dateModified`      INT UNSIGNED  NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `key_phid` (`phid`),
  KEY `key_status` (`status`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 一个启用的 hook，指向 demo 里的 webhook 接收端。
-- webhookURI 用 compose 网络里的服务名 webhook-receiver:9000，hook 路径见 hooks/hooks.json。
-- hmacKey 是签名密钥：gorge-webhook 会用它对 payload 做 HMAC-SHA256，写进
-- X-Phabricator-Webhook-Signature 头。demo 的接收端不校验它，仅打印。
INSERT INTO `herald_webhook` (`phid`, `name`, `webhookURI`, `status`, `hmacKey`)
VALUES (
  'PHID-WHOK-demohook0000000001',
  'demo receiver',
  'http://webhook-receiver:9000/hooks/gorge-demo',
  'enabled',
  'demo-hmac-key'
);

-- 一条待投递请求。properties 必须是合法 JSON（retry=never 表示失败即退，不重排）。
-- dateCreated = dateModified，且 lastRequestResult='none'，保证首个 poll 就能领取。
INSERT INTO `herald_webhookrequest` (
  `phid`, `webhookPHID`, `objectPHID`, `status`,
  `properties`, `lastRequestResult`, `lastRequestEpoch`,
  `dateCreated`, `dateModified`
) VALUES (
  'PHID-HWBR-demoreq00000000001',
  'PHID-WHOK-demohook0000000001',
  'PHID-TASK-demoobject00000001',
  'queued',
  '{"retry":"never"}',
  'none',
  0,
  UNIX_TIMESTAMP(),
  UNIX_TIMESTAMP()
);
