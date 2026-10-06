package cleanup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"
)

type progress struct {
	lease Lease
	start time.Time
	rows  int64
}

// Run is one bounded lane with round-robin batches. Its global 200ms spacing
// supplements the transactional database budget shared by every instance.
func (s *Store) Run(ctx context.Context) {
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		slog.Error("cleanup identity failed")
		return
	}
	owner := hex.EncodeToString(token)
	active := map[string]*progress{}
	for ctx.Err() == nil {
		for _, spec := range specs {
			if s.DBs[spec.Role] == nil {
				continue
			}
			if ctx.Err() != nil {
				break
			}
			p := active[spec.ID]
			if p == nil {
				st, err := s.Status(ctx, spec.ID)
				if err != nil {
					slog.Error("cleanup state unavailable", "collector", spec.ID)
					continue
				}
				if st.Owner != "gorge" {
					continue
				}
				lease, err := s.Claim(ctx, spec.ID, owner, false)
				if errors.Is(err, ErrBusy) || errors.Is(err, ErrConflict) {
					continue
				}
				if err != nil {
					slog.Error("cleanup claim failed", "collector", spec.ID)
					continue
				}
				p = &progress{lease: lease, start: time.Now()}
				active[spec.ID] = p
				if err = s.ValidateSchema(ctx, spec.ID); err != nil {
					s.finish(ctx, p, "retryable_failure", err)
					delete(active, spec.ID)
					continue
				}
			}
			if time.Since(p.start) >= time.Duration(p.lease.Policy.RunSeconds)*time.Second || p.rows >= p.lease.Policy.RunRows {
				s.finish(ctx, p, "budget_exhausted", nil)
				delete(active, spec.ID)
				continue
			}
			limit := p.lease.Policy.BatchRows
			if left := p.lease.Policy.RunRows - p.rows; left < int64(limit) {
				limit = int(left)
			}
			rows, err := s.Batch(ctx, p.lease, limit)
			if errors.Is(err, ErrRateLimited) {
				continue
			}
			if err != nil {
				if !errors.Is(err, ErrConflict) {
					s.finish(ctx, p, "retryable_failure", err)
				}
				delete(active, spec.ID)
			} else {
				p.rows += rows
				slog.Info("cleanup batch", "collector", spec.ID, "deleted", rows, "fence", p.lease.Fence)
				if rows < int64(limit) {
					s.finish(ctx, p, "exhausted", nil)
					delete(active, spec.ID)
				}
			}
			if !wait(ctx, 200*time.Millisecond) {
				break
			}
		}
		if !wait(ctx, time.Second) {
			break
		}
	}
	// Cancellation may leave an in-flight transaction rolled back. Lease expiry
	// is recovery; no unbounded shutdown work or unconditional lease release.
}
func (s *Store) finish(ctx context.Context, p *progress, status string, cause error) {
	if cause != nil {
		slog.Error("cleanup batch failed", "collector", p.lease.ID, "error", cause)
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.Finish(bounded, p.lease, status, cause); err != nil && !errors.Is(err, ErrConflict) {
		slog.Error("cleanup finish failed", "collector", p.lease.ID)
	}
}
func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func (s *Store) RunOnce(ctx context.Context, id string) (int64, error) {
	if err := s.ValidateSchema(ctx, id); err != nil {
		return 0, err
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return 0, err
	}
	l, err := s.Claim(ctx, id, hex.EncodeToString(token), true)
	if err != nil {
		return 0, err
	}
	p := &progress{lease: l, start: time.Now()}
	for {
		limit := l.Policy.BatchRows
		if left := l.Policy.RunRows - p.rows; left < int64(limit) {
			limit = int(left)
		}
		if limit == 0 || time.Since(p.start) >= time.Duration(l.Policy.RunSeconds)*time.Second {
			return p.rows, s.Finish(ctx, l, "budget_exhausted", nil)
		}
		n, err := s.Batch(ctx, l, limit)
		if errors.Is(err, ErrRateLimited) {
			if !wait(ctx, 200*time.Millisecond) {
				return p.rows, ctx.Err()
			}
			continue
		}
		if err != nil {
			s.finish(ctx, p, "retryable_failure", err)
			return p.rows, err
		}
		p.rows += n
		if n < int64(limit) {
			return p.rows, s.Finish(ctx, l, "exhausted", nil)
		}
		if !wait(ctx, 200*time.Millisecond) {
			return p.rows, ctx.Err()
		}
	}
}
