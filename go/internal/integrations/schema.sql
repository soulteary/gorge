-- Provision in a dedicated integration database. Never auto-migrated on boot.
CREATE TABLE gorge_integration_effect (
 id VARBINARY(128) NOT NULL PRIMARY KEY,
 digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 kind VARCHAR(32) NOT NULL,
 state VARCHAR(16) NOT NULL,
 result MEDIUMBLOB NULL,
 httpStatus INT NOT NULL,
 updated BIGINT NOT NULL,
 KEY kind_state(kind,state)
) ENGINE=InnoDB;
CREATE TABLE gorge_integration_inbox (
 id CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
 digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 provider VARCHAR(16) NOT NULL,
 payload MEDIUMBLOB NOT NULL,
 state VARCHAR(16) NOT NULL,
 due BIGINT NOT NULL,
 attempts INT NOT NULL,
 KEY ready(state,due)
) ENGINE=InnoDB;
CREATE TABLE gorge_integration_resolution (
 id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
 domain VARCHAR(16) NOT NULL,
 identity VARBINARY(128) NOT NULL,
 digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
 state VARCHAR(16) NOT NULL,
 operator VARCHAR(128) NOT NULL,
 evidence VARCHAR(2048) NOT NULL,
 created BIGINT NOT NULL,
 KEY identity(domain,identity)
) ENGINE=InnoDB;
