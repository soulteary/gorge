-- Upgrade an existing acceptance-only search control database.
-- Apply once, before restarting gorge-search with projection configuration.
-- Older inbox schemas also require envelope restoration and outbox replay;
-- this migration does not reconstruct historical event bodies.
ALTER TABLE search_projection_delivery
 ADD COLUMN leaseOwner VARBINARY(128) NOT NULL DEFAULT '',
 ADD COLUMN leaseEpoch BIGINT UNSIGNED NOT NULL DEFAULT 0,
 ADD COLUMN leaseExpires BIGINT NOT NULL DEFAULT 0,
 ADD COLUMN leaseRenewals BIGINT UNSIGNED NOT NULL DEFAULT 0,
 ADD COLUMN attempts BIGINT UNSIGNED NOT NULL DEFAULT 0,
 ADD COLUMN nextAttempt BIGINT NOT NULL DEFAULT 0,
 ADD COLUMN lastError VARBINARY(128) NOT NULL DEFAULT '',
 ADD COLUMN appliedEpoch BIGINT NULL;
