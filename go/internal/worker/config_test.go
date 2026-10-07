package worker

import (
	"testing"
	"time"
)

func TestValidatePollingConfiguration(t *testing.T) {
	for _, field := range []string{"poll", "workers", "lease", "idle", "drain-negative", "drain-overflow"} {
		t.Run(field, func(t *testing.T) {
			cfg := testConfig()
			switch field {
			case "poll":
				cfg.PollIntervalMs = 0
			case "workers":
				cfg.MaxWorkers = 0
			case "lease":
				cfg.LeaseLimit = 0
			case "idle":
				cfg.IdleTimeoutSec = -1
			case "drain-negative":
				cfg.DrainTimeoutSec = -1
			case "drain-overflow":
				cfg.DrainTimeoutSec = 3601
			}
			if cfg.Validate() == nil {
				t.Fatal("invalid polling configuration was accepted")
			}
		})
	}
	if err := testConfig().Validate(); err != nil {
		t.Fatalf("disabling idle logs must preserve polling: %v", err)
	}
}

func TestDrainTimeoutConfiguration(t *testing.T) {
	cfg := testConfig()
	consumer := NewConsumer(nil, NewRegistry(), cfg)
	if consumer.drainTimeout != DefaultDrainTimeoutSec*time.Second {
		t.Fatal("zero-value drain budget must retain a finite default")
	}
	t.Setenv("GORGE_WORKER_DRAIN_TIMEOUT_SEC", "7")
	loaded := LoadFromEnv()
	if loaded.DrainTimeoutSec != 7 || loaded.Validate() != nil {
		t.Fatal("explicit drain budget was not loaded or accepted")
	}
}
