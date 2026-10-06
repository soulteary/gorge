package integrations

import (
	"sync"
	"time"
)

type ProjectionHealth struct {
	Enabled             bool  `json:"enabled"`
	LastAttempt         int64 `json:"lastAttempt"`
	LastSuccess         int64 `json:"lastSuccess"`
	ConsecutiveFailures int   `json:"consecutiveFailures"`
	Stale               bool  `json:"stale"`
}
type progress struct {
	mu      sync.Mutex
	started int64
	fact    ProjectionHealth
}

func (s *Service) RecordFactResult(err error) {
	s.progress.mu.Lock()
	defer s.progress.mu.Unlock()
	now := time.Now().Unix()
	s.progress.fact.LastAttempt = now
	if err == nil {
		s.progress.fact.LastSuccess = now
		s.progress.fact.ConsecutiveFailures = 0
	} else {
		s.progress.fact.ConsecutiveFailures++
	}
}
func (s *Service) FactHealth() ProjectionHealth {
	s.progress.mu.Lock()
	defer s.progress.mu.Unlock()
	out := s.progress.fact
	last := out.LastSuccess
	if last == 0 {
		last = s.progress.started
	}
	out.Stale = out.Enabled && time.Now().Unix()-last > 300
	return out
}
