CREATE TABLE IF NOT EXISTS search_projection_inbox (
 namespace VARBINARY(64) NOT NULL, eventID VARBINARY(128) NOT NULL,
 envelopeHash VARBINARY(64) NOT NULL, envelope LONGTEXT NOT NULL, response LONGTEXT NOT NULL,
 PRIMARY KEY(namespace,eventID)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS search_projection_revision (
 namespace VARBINARY(64) NOT NULL, phid VARBINARY(64) NOT NULL,
 revision BIGINT NOT NULL, contentHash VARBINARY(64) NOT NULL,
 PRIMARY KEY(namespace,phid,revision)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS search_projection_head (
 namespace VARBINARY(64) NOT NULL, phid VARBINARY(64) NOT NULL,
 revision BIGINT NOT NULL, envelope LONGTEXT NOT NULL,
 PRIMARY KEY(namespace,phid)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS search_projection_delivery (
 namespace VARBINARY(64) NOT NULL, backendID VARBINARY(64) NOT NULL,
 generationID VARBINARY(64) NOT NULL, phid VARBINARY(64) NOT NULL,
 revision BIGINT NOT NULL, eventID VARBINARY(128) NOT NULL,
 status VARBINARY(16) NOT NULL DEFAULT 'pending', createdEpoch BIGINT NOT NULL DEFAULT 0,
 leaseOwner VARBINARY(128) NOT NULL DEFAULT '', leaseEpoch BIGINT UNSIGNED NOT NULL DEFAULT 0,
 leaseExpires BIGINT NOT NULL DEFAULT 0, leaseRenewals BIGINT UNSIGNED NOT NULL DEFAULT 0, attempts BIGINT UNSIGNED NOT NULL DEFAULT 0,
 nextAttempt BIGINT NOT NULL DEFAULT 0, lastError VARBINARY(128) NOT NULL DEFAULT '',
 appliedEpoch BIGINT NULL,
 PRIMARY KEY(namespace,backendID,generationID,phid,revision),
 KEY key_pending(status,namespace,backendID,generationID)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS search_projection_target (
 namespace VARBINARY(64) NOT NULL, backendID VARBINARY(64) NOT NULL,
 generationID VARBINARY(64) NOT NULL, configHash VARBINARY(64) NOT NULL, indexUUID VARBINARY(128) NOT NULL,
 PRIMARY KEY(namespace,backendID,generationID), UNIQUE KEY key_index_uuid(indexUUID)
) ENGINE=InnoDB;
