// Package worker is Phorge's taskmaster daemon moved into a standalone process.
// It leases tasks from the taskqueue service over HTTP, runs them, and reports
// each result back.
package worker

import (
	"strings"

	"github.com/soulteary/gorge/go/internal/platform/config"
)

const (
	DefaultListenAddr   = ":8170"
	DefaultTaskQueueURL = "http://gorge-taskqueue:8090"

	DefaultLeaseLimit     = 4
	DefaultPollIntervalMs = 1000
	DefaultMaxWorkers     = 4
	DefaultIdleTimeoutSec = 180
)

type Config struct {
	config.Base
	TaskQueueURL    string
	TaskQueueToken  string
	LeaseLimit      int
	PollIntervalMs  int
	MaxWorkers      int
	IdleTimeoutSec  int
	ConduitURL      string
	ConduitToken    string
	TaskClassFilter []string
}

// LoadFromEnv reads only the monorepo GORGE_WORKER_* contract. Standalone
// TASK_QUEUE_*/CONDUIT_* spellings were retired after the bundled deployment
// became authoritative.
func LoadFromEnv() *Config {
	return &Config{
		Base: config.LoadBase(DefaultListenAddr),

		TaskQueueURL:   config.EnvStr(DefaultTaskQueueURL, "GORGE_WORKER_TASK_QUEUE_URL"),
		TaskQueueToken: config.EnvStr("", "GORGE_WORKER_TASK_QUEUE_TOKEN"),

		LeaseLimit:     config.EnvInt(DefaultLeaseLimit, "GORGE_WORKER_LEASE_LIMIT"),
		PollIntervalMs: config.EnvInt(DefaultPollIntervalMs, "GORGE_WORKER_POLL_INTERVAL_MS"),
		MaxWorkers:     config.EnvInt(DefaultMaxWorkers, "GORGE_WORKER_MAX_WORKERS"),
		IdleTimeoutSec: config.EnvInt(DefaultIdleTimeoutSec, "GORGE_WORKER_IDLE_TIMEOUT_SEC"),

		ConduitURL:   config.EnvStr("", "GORGE_WORKER_CONDUIT_URL"),
		ConduitToken: config.EnvStr("", "GORGE_WORKER_CONDUIT_TOKEN"),

		TaskClassFilter: splitCSV(config.EnvStr("", "GORGE_WORKER_TASK_CLASS_FILTER")),
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
