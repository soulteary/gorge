package imagetransform

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const cacheBudget = 64 << 20
const cacheTTL = 5 * time.Minute

type cachedResult struct {
	result  Result
	expires time.Time
	last    time.Time
}
type flight struct {
	done   chan struct{}
	result Result
	err    error
}
type resultCache struct {
	mu      sync.Mutex
	entries map[string]cachedResult
	flights map[string]*flight
	bytes   int
}

// This is a per-service-token, process-local computation cache. It never
// supplies permanent storage handles or holds source bytes. Singleflight does
// not claim cross-replica exactly-once execution; PHP's unique key decides that.
func (s *Service) Transform(ctx context.Context, data []byte, recipe, animation string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if _, ok := Recipes[recipe]; !ok {
		return Result{}, fail(400, "ERR_BAD_REQUEST", "unknown recipe")
	}
	if animation != "legacy-static" && animation != "legacy-preserve" {
		return Result{}, fail(400, "ERR_BAD_REQUEST", "unknown animation policy")
	}
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}
	if len(data) == 0 {
		return Result{}, fail(422, "ERR_INVALID_IMAGE", "empty image")
	}
	if len(data) > MaxBytes {
		return Result{}, fail(413, "ERR_TOO_LARGE", "invalid image byte size")
	}
	digest := sha256.Sum256(data)
	key := fmt.Sprintf("%x/%s/%s/%s/%s", digest, recipe, animation, Revision, s.BackendRevision)
	c := &s.cache
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]cachedResult)
		c.flights = make(map[string]*flight)
	}
	now := time.Now()
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			c.bytes -= len(e.result.Data)
			delete(c.entries, k)
		}
	}
	if e, ok := c.entries[key]; ok {
		s.cacheHits.Add(1)
		e.last = now
		c.entries[key] = e
		c.mu.Unlock()
		return e.result, nil
	}
	if f, ok := c.flights[key]; ok {
		s.sharedWaits.Add(1)
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-f.done:
			return f.result, f.err
		}
	}
	f := &flight{done: make(chan struct{})}
	c.flights[key] = f
	c.mu.Unlock()
	s.computations.Add(1)
	started := time.Now()
	result, err := s.compute(ctx, data, recipe, animation)
	slog.Info("image computation", "recipe", recipe, "duration_ms", time.Since(started).Milliseconds(), "success", err == nil)
	c.mu.Lock()
	f.result, f.err = result, err
	if err == nil {
		for c.bytes+len(result.Data) > cacheBudget || len(c.entries) >= 32 {
			oldKey := ""
			var old time.Time
			for k, e := range c.entries {
				if oldKey == "" || e.last.Before(old) {
					oldKey, old = k, e.last
				}
			}
			if oldKey == "" {
				break
			}
			c.bytes -= len(c.entries[oldKey].result.Data)
			delete(c.entries, oldKey)
		}
		c.entries[key] = cachedResult{result, now.Add(cacheTTL), now}
		c.bytes += len(result.Data)
	}
	delete(c.flights, key)
	close(f.done)
	c.mu.Unlock()
	return result, err
}
