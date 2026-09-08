// Package worker is Phorge's taskmaster daemon moved into a standalone process.
// It leases tasks from the taskqueue service over HTTP, runs them, and reports
// each result back — completing, failing, or yielding — exactly as
// PhabricatorTaskmasterDaemon does against the local queue.
//
// Its one HTTP endpoint, GET /api/worker/stats, is read-only: the real work is
// the lease loop in Consumer, which nothing calls in to start. That makes it
// the sibling of the webhook domain, whose work is also a background loop with
// a status endpoint bolted on for orchestration to probe.
//
// The domain talks to Phorge only through the taskqueue service and, for task
// classes it does not handle itself, through a Conduit gateway. It touches no
// database directly, so unlike taskqueue it has no db.go and its readiness is
// liveness. See docs/modules/taskqueue.md.
package worker

import (
	"strings"

	"github.com/soulteary/gorge/go/internal/platform/config"
)

// Defaults for the worker service. The listen address is the pre-monorepo
// :8170, and the task queue URL points at the taskqueue service's compose name
// on its :8090.
const (
	DefaultListenAddr   = ":8170"
	DefaultTaskQueueURL = "http://gorge-taskqueue:8090"

	DefaultLeaseLimit     = 4
	DefaultPollIntervalMs = 1000
	DefaultMaxWorkers     = 4
	DefaultIdleTimeoutSec = 180
)

// Config is the worker service configuration.
type Config struct {
	config.Base

	// TaskQueueURL is the base URL of the taskqueue service this worker leases
	// from; TaskQueueToken is the service token it presents. They are the one
	// dependency that makes this a two-binary domain rather than one: the
	// worker is the taskqueue's HTTP client, not its co-process.
	TaskQueueURL   string
	TaskQueueToken string

	// Lease loop behaviour.
	LeaseLimit     int
	PollIntervalMs int
	MaxWorkers     int
	IdleTimeoutSec int

	// ConduitURL, when set, installs a fallback handler that delegates any
	// unregistered task class to Phorge's PHP backend through Conduit. Without
	// it the worker only handles the classes it implements natively and fails
	// the rest.
	ConduitURL   string
	ConduitToken string

	// TaskClassFilter restricts the worker to a subset of the classes it could
	// otherwise handle; empty means "everything supported".
	TaskClassFilter []string
}

// LoadFromEnv reads the configuration from the environment. The GORGE_WORKER_
// names are the current ones; the unprefixed names are what the pre-monorepo
// gorge-worker deployment used and are kept as a fallback. See
// docs/platform.md section 4.
func LoadFromEnv() *Config {
	return &Config{
		Base: config.LoadBase(DefaultListenAddr),

		TaskQueueURL:   config.EnvStr(DefaultTaskQueueURL, "GORGE_WORKER_TASK_QUEUE_URL", "TASK_QUEUE_URL"),
		TaskQueueToken: config.EnvStr("", "GORGE_WORKER_TASK_QUEUE_TOKEN", "TASK_QUEUE_TOKEN"),

		LeaseLimit:     config.EnvInt(DefaultLeaseLimit, "GORGE_WORKER_LEASE_LIMIT", "LEASE_LIMIT"),
		PollIntervalMs: config.EnvInt(DefaultPollIntervalMs, "GORGE_WORKER_POLL_INTERVAL_MS", "POLL_INTERVAL_MS"),
		MaxWorkers:     config.EnvInt(DefaultMaxWorkers, "GORGE_WORKER_MAX_WORKERS", "MAX_WORKERS"),
		IdleTimeoutSec: config.EnvInt(DefaultIdleTimeoutSec, "GORGE_WORKER_IDLE_TIMEOUT_SEC", "IDLE_TIMEOUT_SEC"),

		ConduitURL:   config.EnvStr("", "GORGE_WORKER_CONDUIT_URL", "CONDUIT_URL"),
		ConduitToken: config.EnvStr("", "GORGE_WORKER_CONDUIT_TOKEN", "CONDUIT_TOKEN"),

		TaskClassFilter: splitCSV(config.EnvStr("", "GORGE_WORKER_TASK_CLASS_FILTER", "TASK_CLASS_FILTER")),
	}
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
