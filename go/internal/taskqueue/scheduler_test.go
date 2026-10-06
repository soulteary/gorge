package taskqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

func epoch(v int64) *int64 { return &v }

const testSchedulerDatabaseID = "00000000-0000-0000-0000-000000000001"

func TestSchedulePlanGuards(t *testing.T) {
	snap := ScheduleSnapshot{ID: 7, Version: 3, ScheduledVersion: 3, Next: epoch(100), Now: 110}
	valid := SchedulePlan{Protocol: 1, ID: 7, Version: 3, Fire: true, Next: epoch(200), Task: &contracts.EnqueueRequest{TaskClass: "Example", Data: `{}`}}
	if err := validatePlan(snap, valid); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*SchedulePlan)
	}{
		{"stale version", func(p *SchedulePlan) { p.Version-- }},
		{"different trigger", func(p *SchedulePlan) { p.ID++ }},
		{"missing task", func(p *SchedulePlan) { p.Task = nil }},
		{"invalid payload", func(p *SchedulePlan) { p.Task = &contracts.EnqueueRequest{TaskClass: "X", Data: "{"} }},
		{"clock regression", func(p *SchedulePlan) { p.Next = epoch(100) }},
		{"early fire", func(p *SchedulePlan) { p.Fire = false }},
		{"epoch overflow", func(p *SchedulePlan) { p.Next = epoch(4294967296) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := valid
			tc.mutate(&p)
			if validatePlan(snap, p) == nil {
				t.Fatal("accepted invalid plan")
			}
		})
	}
	snap.ScheduledVersion = 2
	p := SchedulePlan{Protocol: 1, ID: 7, Version: 3, Next: epoch(90)}
	if err := validatePlan(snap, p); err != nil {
		t.Fatal("an edit may schedule in the past", err)
	}
	p.Next = nil
	if err := validatePlan(snap, p); err != nil {
		t.Fatal("never clock", err)
	}
}
func TestScheduleConduitTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/trigger.plan" || r.Header.Get("X-Service-Token") != "secret" {
			t.Error("invalid authenticated route")
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		var params map[string]any
		if err := json.Unmarshal([]byte(r.Form.Get("params")), &params); err != nil {
			t.Fatal(err)
		}
		if params["phase"] == "capabilities" {
			if params["databaseID"] != testSchedulerDatabaseID {
				t.Error("missing database identity")
			}
			if _, err := fmt.Fprintf(w, `{"result":{"protocol":1,"atomicEnqueue":true,"databaseID":%q}}`, testSchedulerDatabaseID); err != nil {
				t.Error(err)
			}
			return
		}
		if params["lastEpoch"] != nil || params["nextEpoch"] != float64(100) || params["now"] != float64(110) || params["databaseID"] != testSchedulerDatabaseID {
			t.Error("lost nullable clock context", params)
		}
		if _, err := fmt.Fprint(w, `{"result":{"protocol":1,"triggerID":7,"version":3,"fire":true,"nextEpoch":null,"task":{"taskClass":"Example","data":"{}"}}}`); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	source := NewScheduleSource(srv.URL, "secret")
	if err := source.Ready(t.Context(), testSchedulerDatabaseID); err != nil {
		t.Fatal(err)
	}
	snap := ScheduleSnapshot{ID: 7, Version: 3, ScheduledVersion: 3, Next: epoch(100), Now: 110}
	snap.DatabaseID = testSchedulerDatabaseID
	p, err := source.Plan(t.Context(), snap)
	if err != nil {
		t.Fatal(err)
	}
	if err = validatePlan(snap, p); err != nil {
		t.Fatal(err)
	}
}

type scheduleTestStore struct{ failures, applied []int64 }

func (s *scheduleTestStore) ScheduleReady(context.Context) error { return nil }
func (s *scheduleTestStore) ScheduleDatabaseID(context.Context) (string, error) {
	return testSchedulerDatabaseID, nil
}
func (s *scheduleTestStore) ScheduleCandidates(context.Context, int) ([]ScheduleSnapshot, error) {
	return []ScheduleSnapshot{{ID: 1, Version: 1}, {ID: 2, Version: 1}}, nil
}
func (s *scheduleTestStore) ApplySchedule(_ context.Context, snap ScheduleSnapshot, _ SchedulePlan) error {
	s.applied = append(s.applied, snap.ID)
	return nil
}
func (s *scheduleTestStore) ScheduleFailed(_ context.Context, snap ScheduleSnapshot) error {
	s.failures = append(s.failures, snap.ID)
	return nil
}

type scheduleTestSource struct{}

func (scheduleTestSource) Ready(context.Context, string) error { return nil }
func (scheduleTestSource) Plan(_ context.Context, snap ScheduleSnapshot) (SchedulePlan, error) {
	if snap.ID == 1 {
		return SchedulePlan{}, errors.New("unsupported action")
	}
	return SchedulePlan{Protocol: 1, ID: snap.ID, Version: snap.Version}, nil
}
func TestSchedulerBadTriggerDoesNotBlockOtherTriggers(t *testing.T) {
	store := new(scheduleTestStore)
	s := Scheduler{Store: store, Source: scheduleTestSource{}}
	if s.Tick(t.Context()) == nil {
		t.Fatal("failure must be observable")
	}
	if len(store.failures) != 1 || store.failures[0] != 1 || len(store.applied) != 1 || store.applied[0] != 2 {
		t.Fatal("poison trigger blocked batch", store)
	}
}

type cancellingScheduleSource struct {
	cancel context.CancelFunc
	calls  int
}

func (s *cancellingScheduleSource) Ready(context.Context, string) error { return nil }
func (s *cancellingScheduleSource) Plan(_ context.Context, snap ScheduleSnapshot) (SchedulePlan, error) {
	s.calls++
	s.cancel()
	return SchedulePlan{Protocol: 1, ID: snap.ID, Version: snap.Version}, nil
}

func TestSchedulerCancellationStopsBatch(t *testing.T) {
	for _, beforePlan := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		store := new(scheduleTestStore)
		source := &cancellingScheduleSource{cancel: cancel}
		if beforePlan {
			cancel()
		}
		s := Scheduler{Store: store, Source: source}
		err := s.Tick(ctx)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
		wantCalls := 1
		if beforePlan {
			wantCalls = 0
		}
		if source.calls != wantCalls || len(store.applied) != 0 || len(store.failures) != 0 {
			t.Fatalf("cancelled batch continued: calls=%d store=%+v", source.calls, store)
		}
	}
}

// Capability metadata must not authorize takeover while the source is down.
type schedulerMetaStore struct {
	executionHTTPStore
	scheduleTestStore
}

func TestSchedulerMetadataFailsClosed(t *testing.T) {
	for _, readyErr := range []error{nil, errors.New("clock source unavailable")} {
		srv := httpx.New(httpx.Config{})
		RegisterRoutes(srv.App(), &Deps{Store: &schedulerMetaStore{}, Token: testToken, SchedulerEnabled: true,
			SchedulerReady: func(context.Context) error { return readyErr }})
		resp, body := get(t, srv.App(), "/api/queue/meta")
		if readyErr != nil {
			if resp.StatusCode != 500 {
				t.Fatal("unready scheduler advertised", body)
			}
			continue
		}
		var caps contracts.ExecutionCapabilities
		if err := json.Unmarshal(envelope(t, body).Data, &caps); err != nil {
			t.Fatal(err)
		}
		if caps.SchedulerProtocol != 1 || !caps.SchedulerAtomicEnqueue || caps.SchedulerDatabaseID != testSchedulerDatabaseID {
			t.Fatal("missing scheduler capabilities", body)
		}
	}
}

func TestScheduleSourceRejectsOtherDatabase(t *testing.T) {
	for _, id := range []string{"", "00000000-0000-0000-0000-000000000002"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := fmt.Fprintf(w, `{"result":{"protocol":1,"atomicEnqueue":true,"databaseID":%q}}`, id); err != nil {
				t.Error(err)
			}
		}))
		err := NewScheduleSource(srv.URL, "secret").Ready(t.Context(), testSchedulerDatabaseID)
		srv.Close()
		if err == nil {
			t.Fatal("source from another database accepted")
		}
	}
}
