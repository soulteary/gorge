-- Apply in the dedicated search control database, before enabling deliveries.
CREATE TABLE IF NOT EXISTS search_projection_target (
 namespace VARBINARY(64) NOT NULL, backendID VARBINARY(64) NOT NULL,
 generationID VARBINARY(64) NOT NULL, configHash VARBINARY(64) NOT NULL, indexUUID VARBINARY(128) NOT NULL,
 PRIMARY KEY(namespace,backendID,generationID), UNIQUE KEY key_index_uuid(indexUUID)
) ENGINE=InnoDB;
