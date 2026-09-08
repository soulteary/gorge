// Package taskqueue is Phorge's daemon work queue moved behind an HTTP API. It
// owns `{namespace}_worker.worker_activetask`, worker_taskdata and
// worker_archivetask: it enqueues tasks, leases them to workers, and archives
// them when they complete, fail permanently or are cancelled.
//
// Like the webhook domain it depends on Phorge's own database and defines a
// Store interface over it, so its handlers and contract fixtures can run
// against an in-memory implementation rather than a live MySQL. Unlike webhook
// it has a second, optional backend — Redis — for deployments that want the
// queue off the primary database; both satisfy the same Store.
//
// See docs/modules/taskqueue.md.
package taskqueue

import (
	"github.com/soulteary/gorge/go/internal/platform/config"
)

// Defaults for the task queue service. The listen address is the pre-monorepo
// :8090, which the existing gorge-task-queue deployments already point at.
const (
	DefaultListenAddr = ":8090"

	DefaultBackend = "mysql"

	// DefaultMySQLHost is a real default rather than an empty switch, for the
	// same reason the webhook domain's is: there is nothing this service can
	// do without a backend, so "not configured" is not a state worth
	// distinguishing from "unreachable". Either way /readyz reports it.
	DefaultMySQLHost = "127.0.0.1"
	DefaultMySQLPort = 3306
	DefaultMySQLUser = "phorge"
	DefaultNamespace = "phorge"

	DefaultRedisAddr      = "127.0.0.1:6379"
	DefaultRedisDB        = 0
	DefaultRedisKeyPrefix = "gorge:tq:"

	// DefaultLeaseDuration is how long a leased task is left alone before its
	// lease is considered expired and another worker may take it. Two hours is
	// Phorge's own PhabricatorWorkerLeaseQuery default lease.
	DefaultLeaseDuration = 7200

	// DefaultRetryWait is how long a temporarily failed task waits before it
	// becomes leasable again. Five minutes matches Phorge's
	// getWaitBeforeRetry for a task with no override.
	DefaultRetryWait = 300
)

// Config is the task queue service configuration.
type Config struct {
	config.Base

	// Backend selects the store: "mysql" (the default) or "redis".
	Backend string

	// Phorge's database. The service reads and writes worker_activetask,
	// worker_taskdata and worker_archivetask; it needs no other schema and no
	// DDL rights.
	MySQLHost string
	MySQLPort int
	MySQLUser string
	MySQLPass string
	Namespace string

	// Redis backend, used only when Backend is "redis".
	RedisAddr      string
	RedisPassword  string
	RedisDB        int
	RedisKeyPrefix string

	// Queue behaviour.
	LeaseDuration int
	RetryWait     int
}

// LoadFromEnv reads the configuration from the environment. The
// GORGE_TASKQUEUE_ names are the current ones; the unprefixed names are what
// the pre-monorepo gorge-task-queue deployment used and are kept as a
// fallback. See docs/platform.md section 4.
func LoadFromEnv() *Config {
	return &Config{
		Base: config.LoadBase(DefaultListenAddr),

		Backend: config.EnvStr(DefaultBackend, "GORGE_TASKQUEUE_BACKEND", "QUEUE_BACKEND"),

		MySQLHost: config.EnvStr(DefaultMySQLHost, "GORGE_TASKQUEUE_MYSQL_HOST", "MYSQL_HOST"),
		MySQLPort: config.EnvInt(DefaultMySQLPort, "GORGE_TASKQUEUE_MYSQL_PORT", "MYSQL_PORT"),
		MySQLUser: config.EnvStr(DefaultMySQLUser, "GORGE_TASKQUEUE_MYSQL_USER", "MYSQL_USER"),
		MySQLPass: config.EnvStr("", "GORGE_TASKQUEUE_MYSQL_PASS", "MYSQL_PASS"),
		Namespace: config.EnvStr(DefaultNamespace, "GORGE_TASKQUEUE_NAMESPACE", "STORAGE_NAMESPACE"),

		RedisAddr:      config.EnvStr(DefaultRedisAddr, "GORGE_TASKQUEUE_REDIS_ADDR", "REDIS_ADDR"),
		RedisPassword:  config.EnvStr("", "GORGE_TASKQUEUE_REDIS_PASSWORD", "REDIS_PASSWORD"),
		RedisDB:        config.EnvInt(DefaultRedisDB, "GORGE_TASKQUEUE_REDIS_DB", "REDIS_DB"),
		RedisKeyPrefix: config.EnvStr(DefaultRedisKeyPrefix, "GORGE_TASKQUEUE_REDIS_KEY_PREFIX", "REDIS_KEY_PREFIX"),

		LeaseDuration: config.EnvInt(DefaultLeaseDuration, "GORGE_TASKQUEUE_LEASE_DURATION", "LEASE_DURATION"),
		RetryWait:     config.EnvInt(DefaultRetryWait, "GORGE_TASKQUEUE_RETRY_WAIT", "RETRY_WAIT"),
	}
}
