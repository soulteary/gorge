package worker

import (
	"context"
	"log/slog"
	"time"
)

// WaitForExecution keeps tasks unleased while dependencies negotiate. The
// listener stays available so PHP can bootstrap without a dependency cycle.
func WaitForExecution(ctx context.Context, interval time.Duration, check func(context.Context) error) bool {
	for {
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := check(probe)
		cancel()
		if ctx.Err() != nil {
			return false
		}
		if err == nil {
			return true
		}
		slog.Warn("worker awaiting execution dependencies", "error", err)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}
