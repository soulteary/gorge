package worker

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStartupRetriesBeforeAllowingExecution(t *testing.T) {
	calls := 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !WaitForExecution(ctx, time.Millisecond, func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("protocol unavailable")
		}
		return nil
	}) || calls != 3 {
		t.Fatalf("started without negotiating dependencies: calls=%d", calls)
	}
}

func TestStartupCancellationDoesNotRunConsumer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if WaitForExecution(ctx, time.Hour, func(context.Context) error { return nil }) {
		t.Fatal("cancelled startup must not allow execution")
	}
}
