package filestorage

import "github.com/soulteary/gorge/go/internal/platform/config"

const (
	DefaultListenAddr = ":8100"
	DefaultMySQLPort = 3306
	DefaultMySQLUser = "phorge"
	DefaultNamespace = "phorge"
	DefaultMySQLBlobMaxSize = 1000000
	TransportBodyLimit = "16M"
)

type Config struct {
	config.Base
	MySQLHost string
	MySQLPort int
	MySQLUser string
	MySQLPass string
	Namespace string
	MySQLBlobMaxSize int64
	LocalDiskPath string
	S3Bucket string
	S3AccessKey string
	S3SecretKey string
	S3Region string
	S3Endpoint string
	InstanceName string
}

// LoadFromEnv reads only the canonical GORGE_FILE_* variables. The old bare
// MySQL/S3/local-disk names were standalone-service compatibility and are no
// longer part of the bundled Phorge/Gorge deployment contract.
func LoadFromEnv() *Config {
	return &Config{
		Base: config.LoadBase(DefaultListenAddr),
		MySQLHost: config.EnvStr("", "GORGE_FILE_MYSQL_HOST"),
		MySQLPort: config.EnvInt(DefaultMySQLPort, "GORGE_FILE_MYSQL_PORT"),
		MySQLUser: config.EnvStr(DefaultMySQLUser, "GORGE_FILE_MYSQL_USER"),
		MySQLPass: config.EnvStr("", "GORGE_FILE_MYSQL_PASS"),
		Namespace: config.EnvStr(DefaultNamespace, "GORGE_FILE_NAMESPACE"),
		MySQLBlobMaxSize: int64(config.EnvInt(DefaultMySQLBlobMaxSize, "GORGE_FILE_MYSQL_BLOB_MAX_SIZE")),
		LocalDiskPath: config.EnvStr("", "GORGE_FILE_LOCAL_DISK_PATH"),
		S3Bucket: config.EnvStr("", "GORGE_FILE_S3_BUCKET"),
		S3AccessKey: config.EnvStr("", "GORGE_FILE_S3_ACCESS_KEY"),
		S3SecretKey: config.EnvStr("", "GORGE_FILE_S3_SECRET_KEY"),
		S3Region: config.EnvStr("", "GORGE_FILE_S3_REGION"),
		S3Endpoint: config.EnvStr("", "GORGE_FILE_S3_ENDPOINT"),
		InstanceName: config.EnvStr("", "GORGE_FILE_INSTANCE_NAME"),
	}
}

func (c *Config) MySQLBlobEnabled() bool {
	return c.MySQLHost != "" && c.MySQLBlobMaxSize > 0
}

func (c *Config) LocalDiskEnabled() bool { return c.LocalDiskPath != "" }

func (c *Config) S3Enabled() bool {
	return c.S3Bucket != "" && c.S3AccessKey != "" && c.S3SecretKey != "" &&
		c.S3Region != "" && c.S3Endpoint != ""
}
