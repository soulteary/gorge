package worker

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

type leaseSessionKey struct{}

// leaseSession owns the mutable lease. Handlers receive a separate task copy,
// so renewal and result reporting cannot race on LeaseExpires.
type leaseSession struct {
	mu     sync.Mutex
	task   contracts.Task
	client *Client
	err    error
}

func (s *leaseSession) renew(ctx context.Context, duration int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.err = s.client.renew(ctx, &s.task, duration)
	return s.err
}
func (s *leaseSession) snapshot() (contracts.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.task, s.err
}
func (c *Client) startHeartbeat(ctx context.Context, task *contracts.Task) (context.Context, func() (*contracts.Task, error)) {
	s := &leaseSession{task: *task, client: c}
	runCtx, cancel := context.WithCancel(context.WithValue(ctx, leaseSessionKey{}, s))
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			current, err := s.snapshot()
			if err != nil {
				cancel()
				return
			}
			remaining := time.Until(time.Unix(*current.LeaseExpires, 0))
			if remaining <= 0 {
				s.mu.Lock()
				s.err = errors.New("execution lease expired")
				s.mu.Unlock()
				cancel()
				return
			}
			interval := remaining / 3
			if interval > 30*time.Second {
				interval = 30 * time.Second
			}
			timer := time.NewTimer(interval)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-runCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			// Renew before expiry. The HTTP call is bounded by the current ownership
			// window even if the queue becomes unreachable.
			renewCtx, renewCancel := context.WithDeadline(runCtx, time.Unix(*current.LeaseExpires, 0))
			duration := 60
			err = s.renew(renewCtx, duration)
			renewCancel()
			if err != nil {
				cancel()
				return
			}
		}
	}()
	return runCtx, func() (*contracts.Task, error) {
		close(stop)
		<-done
		cancel()
		current, err := s.snapshot()
		return &current, err
	}
}
