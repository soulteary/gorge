// Package taskqueue is Phorge's daemon work queue moved behind an HTTP API.
package taskqueue

import "github.com/soulteary/gorge/go/internal/platform/config"

const (
	DefaultListenAddr     = ":8090"
	DefaultBackend        = "mysql"
	DefaultMySQLHost      = "127.0.0.1"
	DefaultMySQLPort      = 3306
	DefaultMySQLUser      = "phorge"
	DefaultNamespace      = "phorge"
	DefaultRedisAddr      = "127.0.0.1:6379"
	DefaultRedisDB        = 0
	DefaultRedisKeyPrefix = "gorge:tq:"
	DefaultLeaseDuration  = 7200
	DefaultRetryWait      = 300
)

type Config struct {
	config.Base
	Backend        string
	MySQLHost      string
	MySQLPort      int
	MySQLUser      string
	MySQLPass      string
	Namespace      string
	RedisAddr      string
	RedisPassword  string
	RedisDB        int
	RedisKeyPrefix string
	LeaseDuration  int
	RetryWait      int
}

// LoadFromEnv reads only the current GORGE_TASKQUEUE_* contract. Standalone
// QUEUE_/MYSQL_/REDIS_ aliases were retired with the pre-monorepo deployment.
func LoadFromEnv() *Config {
	return &Config{
		Base:           config.LoadBase(DefaultListenAddr),
		Backend:        config.EnvStr(DefaultBackend, "GORGE_TASKQUEUE_BACKEND"),
		MySQLHost:      config.EnvStr(DefaultMySQLHost, "GORGE_TASKQUEUE_MYSQL_HOST"),
		MySQLPort:      config.EnvInt(DefaultMySQLPort, "GORGE_TASKQUEUE_MYSQL_PORT"),
		MySQLUser:      config.EnvStr(DefaultMySQLUser, "GORGE_TASKQUEUE_MYSQL_USER"),
		MySQLPass:      config.EnvStr("", "GORGE_TASKQUEUE_MYSQL_PASS"),
		Namespace:      config.EnvStr(DefaultNamespace, "GORGE_TASKQUEUE_NAMESPACE"),
		RedisAddr:      config.EnvStr(DefaultRedisAddr, "GORGE_TASKQUEUE_REDIS_ADDR"),
		RedisPassword:  config.EnvStr("", "GORGE_TASKQUEUE_REDIS_PASSWORD"),
		RedisDB:        config.EnvInt(DefaultRedisDB, "GORGE_TASKQUEUE_REDIS_DB"),
		RedisKeyPrefix: config.EnvStr(DefaultRedisKeyPrefix, "GORGE_TASKQUEUE_REDIS_KEY_PREFIX"),
		LeaseDuration:  config.EnvInt(DefaultLeaseDuration, "GORGE_TASKQUEUE_LEASE_DURATION"),
		RetryWait:      config.EnvInt(DefaultRetryWait, "GORGE_TASKQUEUE_RETRY_WAIT"),
	}
}
