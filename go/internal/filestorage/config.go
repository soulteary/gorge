package filestorage

import (
	"github.com/soulteary/gorge/go/internal/platform/config"
)

// Defaults for the file storage service. The listen address is the
// pre-monorepo :8100, which the existing deployments already point at.
const (
	DefaultListenAddr = ":8100"

	// DefaultMySQLPort, DefaultMySQLUser and DefaultNamespace mirror a stock
	// Phorge deployment: the blob backend writes into Phorge's own
	// `{namespace}_file` database.
	DefaultMySQLPort = 3306
	DefaultMySQLUser = "phorge"
	DefaultNamespace = "phorge"

	// DefaultMySQLBlobMaxSize caps a single blob row at 1 MB. It is small
	// because a blob lives in a MySQL row and is read into memory whole; the
	// router sends anything larger to the next backend by priority.
	DefaultMySQLBlobMaxSize = 1000000

	// TransportBodyLimit overrides the platform's 2M default. Phorge's chunked
	// storage engine splits anything over 8 MB into 4 MB chunks before it
	// reaches this service, so 16M is roughly twice the largest single request
	// that can legitimately arrive. Raising it does not enable larger files —
	// it only removes the guard against a caller that ignores the chunking.
	TransportBodyLimit = "16M"
)

// Config is the file storage service configuration.
//
// Each backend is enabled by its own settings and nothing else; there is no
// list of backends to declare. A deployment with none of them configured is a
// legitimate state to boot into, which /readyz then reports as unavailable.
type Config struct {
	config.Base

	// MySQL blob backend. MySQLHost is the switch: without it the backend is
	// not registered at all, however the other MySQL settings are left.
	MySQLHost        string
	MySQLPort        int
	MySQLUser        string
	MySQLPass        string
	Namespace        string
	MySQLBlobMaxSize int64

	// Local disk backend. LocalDiskPath is the switch.
	LocalDiskPath string

	// S3 backend. All five settings are required together.
	S3Bucket    string
	S3AccessKey string
	S3SecretKey string
	S3Region    string
	S3Endpoint  string

	// InstanceName prefixes S3 keys, for a bucket shared by several Phorge
	// instances. It matches Phorge's own `storage.s3.bucket` layout.
	InstanceName string
}

// LoadFromEnv reads the configuration from the environment. The GORGE_FILE_
// names are the current ones; the unprefixed names are what the pre-monorepo
// deployment used and are kept as a fallback. See docs/platform.md section 4.
func LoadFromEnv() *Config {
	return &Config{
		Base: config.LoadBase(DefaultListenAddr),

		MySQLHost: config.EnvStr("", "GORGE_FILE_MYSQL_HOST", "MYSQL_HOST"),
		MySQLPort: config.EnvInt(DefaultMySQLPort, "GORGE_FILE_MYSQL_PORT", "MYSQL_PORT"),
		MySQLUser: config.EnvStr(DefaultMySQLUser, "GORGE_FILE_MYSQL_USER", "MYSQL_USER"),
		MySQLPass: config.EnvStr("", "GORGE_FILE_MYSQL_PASS", "MYSQL_PASS"),
		Namespace: config.EnvStr(DefaultNamespace, "GORGE_FILE_NAMESPACE", "STORAGE_NAMESPACE"),
		MySQLBlobMaxSize: int64(config.EnvInt(DefaultMySQLBlobMaxSize,
			"GORGE_FILE_MYSQL_BLOB_MAX_SIZE", "MYSQL_BLOB_MAX_SIZE")),

		LocalDiskPath: config.EnvStr("", "GORGE_FILE_LOCAL_DISK_PATH", "LOCAL_DISK_PATH"),

		S3Bucket:    config.EnvStr("", "GORGE_FILE_S3_BUCKET", "S3_BUCKET"),
		S3AccessKey: config.EnvStr("", "GORGE_FILE_S3_ACCESS_KEY", "S3_ACCESS_KEY"),
		S3SecretKey: config.EnvStr("", "GORGE_FILE_S3_SECRET_KEY", "S3_SECRET_KEY"),
		S3Region:    config.EnvStr("", "GORGE_FILE_S3_REGION", "S3_REGION"),
		S3Endpoint:  config.EnvStr("", "GORGE_FILE_S3_ENDPOINT", "S3_ENDPOINT"),

		InstanceName: config.EnvStr("", "GORGE_FILE_INSTANCE_NAME", "INSTANCE_NAME"),
	}
}

// MySQLBlobEnabled reports whether the blob backend should be registered.
//
// The host is part of the condition, not just the size limit, and that is a
// deliberate change from the standalone service: there the host defaulted to
// 127.0.0.1 and the size limit defaulted to 1 MB, so a deployment that
// configured only local disk still registered a blob backend pointed at a
// database that does not exist. That backend has priority 1, so it collected
// every small upload and failed it, and /readyz — which pings the database —
// reported the whole service unavailable. Requiring the host makes "no backend
// configured" and "blob configured" distinguishable, the way MAILER_TYPE does
// for the mailer.
func (c *Config) MySQLBlobEnabled() bool {
	return c.MySQLHost != "" && c.MySQLBlobMaxSize > 0
}

// LocalDiskEnabled reports whether the local disk backend should be registered.
func (c *Config) LocalDiskEnabled() bool {
	return c.LocalDiskPath != ""
}

// S3Enabled reports whether the S3 backend should be registered. All five
// settings are required: a partial configuration would build a client that
// fails every request, which is worse than not registering the backend.
func (c *Config) S3Enabled() bool {
	return c.S3Bucket != "" && c.S3AccessKey != "" && c.S3SecretKey != "" &&
		c.S3Region != "" && c.S3Endpoint != ""
}
