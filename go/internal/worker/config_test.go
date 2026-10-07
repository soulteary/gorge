package worker

import "testing"

func TestValidatePollingConfiguration(t *testing.T) {
	for _, field := range []string{"poll", "workers", "lease", "idle"} {
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
