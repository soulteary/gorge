package taskqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/platform/conduitclient"
)

var ErrScheduleConflict = errors.New("schedule ownership or snapshot changed")

// ScheduleSnapshot is read from the writer database. A plan is read-only: PHP
// computes clock semantics, while ApplySchedule alone advances events/enqueues.
type ScheduleSnapshot struct {
	ID               int64  `json:"triggerID"`
	Version          int64  `json:"version"`
	OwnerEpoch       int64  `json:"ownerEpoch"`
	EventID          int64  `json:"eventID"`
	Last             *int64 `json:"lastEpoch"`
	Next             *int64 `json:"nextEpoch"`
	ScheduledVersion int64  `json:"scheduledVersion"`
	Now              int64  `json:"now"`
	DatabaseID       string `json:"databaseID"`
}
type SchedulePlan = contracts.TriggerPlan

type ScheduleSource interface {
	Plan(context.Context, ScheduleSnapshot) (SchedulePlan, error)
	Ready(context.Context, string) error
}
type ScheduleStore interface {
	ScheduleCandidates(context.Context, int) ([]ScheduleSnapshot, error)
	ApplySchedule(context.Context, ScheduleSnapshot, SchedulePlan) error
	ScheduleFailed(context.Context, ScheduleSnapshot) error
	ScheduleReady(context.Context) error
	ScheduleDatabaseID(context.Context) (string, error)
}
type ConduitScheduleSource struct{ client *conduitclient.Client }

func NewScheduleSource(uri, token string) *ConduitScheduleSource {
	return &ConduitScheduleSource{client: conduitclient.NewBounded(uri, token, 1024*1024)}
}
func (s *ConduitScheduleSource) Ready(ctx context.Context, databaseID string) error {
	r, err := s.client.Call(ctx, "trigger.plan", map[string]any{"phase": "capabilities", "protocol": 1, "databaseID": databaseID})
	if err != nil {
		return err
	}
	var caps struct {
		Protocol      int    `json:"protocol"`
		AtomicEnqueue bool   `json:"atomicEnqueue"`
		DatabaseID    string `json:"databaseID"`
	}
	if err = json.Unmarshal(r.Result, &caps); err != nil {
		return err
	}
	if caps.Protocol != 1 || !caps.AtomicEnqueue || databaseID == "" || caps.DatabaseID != databaseID {
		return errors.New("trigger plan protocol unavailable")
	}
	return nil
}
func (s *ConduitScheduleSource) Plan(ctx context.Context, snap ScheduleSnapshot) (SchedulePlan, error) {
	params := map[string]any{"phase": "plan", "protocol": 1, "triggerID": snap.ID, "version": snap.Version,
		"lastEpoch": snap.Last, "nextEpoch": snap.Next, "scheduledVersion": snap.ScheduledVersion, "now": snap.Now, "databaseID": snap.DatabaseID}
	r, err := s.client.Call(ctx, "trigger.plan", params)
	if err != nil {
		return SchedulePlan{}, err
	}
	var p SchedulePlan
	err = json.Unmarshal(r.Result, &p)
	return p, err
}
func due(s ScheduleSnapshot) bool {
	return s.ScheduledVersion == s.Version && s.Next != nil && *s.Next <= s.Now
}
func validatePlan(s ScheduleSnapshot, p SchedulePlan) error {
	if p.Protocol != 1 || p.ID != s.ID || p.Version != s.Version || p.Fire != due(s) {
		return errors.New("invalid trigger plan identity or phase")
	}
	if p.Next != nil && (*p.Next <= 0 || *p.Next > 4294967295 || (p.Fire && *p.Next <= *s.Next)) {
		return errors.New("trigger clock must move forward")
	}
	if p.Fire {
		if p.Task == nil || p.Task.TaskClass == "" || !json.Valid([]byte(p.Task.Data)) {
			return errors.New("firing requires a valid task")
		}
	} else if p.Task != nil {
		return errors.New("rescheduling must not enqueue a task")
	}
	return nil
}

// Transactional row locks are the short execution claims: there is no remote
// side effect under a claim, and process death rolls back both queue and clock.
// Stale plans lose the snapshot CAS even across an owner switch and switchback.
type Scheduler struct {
	Store  ScheduleStore
	Source ScheduleSource
}

func (s *Scheduler) Ready(ctx context.Context) error {
	if err := s.Store.ScheduleReady(ctx); err != nil {
		return err
	}
	id, err := s.Store.ScheduleDatabaseID(ctx)
	if err != nil {
		return err
	}
	return s.Source.Ready(ctx, id)
}
func (s *Scheduler) Tick(ctx context.Context) error {
	candidates, err := s.Store.ScheduleCandidates(ctx, 100)
	if err != nil {
		return err
	}
	var failures []error
	for _, snap := range candidates {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		planCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		p, err := s.Source.Plan(planCtx, snap)
		cancel()
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if err == nil {
			err = validatePlan(snap, p)
		}
		if err == nil {
			err = s.Store.ApplySchedule(ctx, snap, p)
		}
		if err != nil && !errors.Is(err, ErrScheduleConflict) {
			// Persist a bounded retry delay, not sensitive upstream response bodies.
			if retryErr := s.Store.ScheduleFailed(ctx, snap); retryErr != nil {
				failures = append(failures, retryErr)
			}
			failures = append(failures, fmt.Errorf("trigger %d: %w", snap.ID, err))
		}
	}
	return errors.Join(failures...)
}
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		cycle, cancel := context.WithTimeout(ctx, 45*time.Second)
		// Capability checks fail closed on mixed-version deployments; ownership is
		// rechecked inside every transaction rather than cached at startup.
		err := s.Ready(cycle)
		if err == nil {
			err = s.Tick(cycle)
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Error("scheduler cycle unavailable", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
