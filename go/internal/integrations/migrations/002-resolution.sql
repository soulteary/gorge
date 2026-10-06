-- Apply only when upgrading a database provisioned with the original two tables.
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
