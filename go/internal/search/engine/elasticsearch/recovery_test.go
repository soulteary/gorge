package elasticsearch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/search/engine"
	"github.com/soulteary/gorge/go/internal/search/projection"
)

type recoveryTransport func(*http.Request) (*http.Response, error)

func (f recoveryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func recoveryResponse(status int) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}
}

func TestWriteOnlyHostRecoversAfterCooldown(t *testing.T) {
	for _, failure := range []string{"503", "network"} {
		t.Run(failure, func(t *testing.T) {
			now := time.Unix(1000, 0)
			b := New(engine.BackendDef{Hosts: []string{"es.invalid"}, Roles: []string{"write"}, Version: 8})
			b.now = func() time.Time { return now }
			calls := 0
			b.client = &http.Client{Transport: recoveryTransport(func(*http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					if failure == "network" {
						return nil, errors.New("connection lost")
					}
					return recoveryResponse(503), nil
				}
				return recoveryResponse(200), nil
			})}
			doc := &contracts.Document{PHID: "PHID-TASK-recovery", Type: "TASK"}
			if err := b.IndexDocument(doc); err == nil {
				t.Fatal("expected first failure")
			}
			if err := b.IndexDocument(doc); err == nil || calls != 1 {
				t.Fatalf("cooldown did not bound attempts: calls=%d err=%v", calls, err)
			}
			now = now.Add(time.Second)
			if err := b.IndexDocument(doc); err != nil {
				t.Fatal(err)
			}
			if calls != 2 || !b.health["es.invalid"] {
				t.Fatalf("write-only host did not recover: calls=%d", calls)
			}
		})
	}
}

func TestWriteFailsOverOncePerHostAndRecoversWithoutReads(t *testing.T) {
	now := time.Unix(1000, 0)
	b := New(engine.BackendDef{Hosts: []string{"down.invalid", "recover.invalid"}, Roles: []string{"write"}, Version: 8})
	b.now = func() time.Time { return now }
	recovered := false
	calls := map[string]int{}
	b.client = &http.Client{Transport: recoveryTransport(func(r *http.Request) (*http.Response, error) {
		calls[r.URL.Host]++
		if recovered && r.URL.Host == "recover.invalid" {
			return recoveryResponse(200), nil
		}
		return recoveryResponse(503), nil
	})}
	doc := &contracts.Document{PHID: "PHID-TASK-recovery", Type: "TASK"}
	if err := b.IndexDocument(doc); err == nil {
		t.Fatal("expected failure")
	}
	if calls["down.invalid"] != 1 || calls["recover.invalid"] != 1 {
		t.Fatalf("must try each host once: %v", calls)
	}
	now = now.Add(time.Second)
	recovered = true
	if err := b.IndexDocument(doc); err != nil {
		t.Fatal(err)
	}
	if calls["down.invalid"] != 2 || calls["recover.invalid"] != 2 {
		t.Fatalf("unexpected recovery attempts: %v", calls)
	}
	if !b.health["recover.invalid"] {
		t.Fatal("recovered host still excluded")
	}
}

func TestRecoveryAdmitsOnlyOneConcurrentProbe(t *testing.T) {
	now := time.Unix(1000, 0)
	b := New(engine.BackendDef{Hosts: []string{"recover.invalid"}, Roles: []string{"write"}})
	b.now = func() time.Time { return now }
	b.markHealth("recover.invalid", false)
	now = now.Add(time.Second)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	b.client = &http.Client{Transport: recoveryTransport(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return recoveryResponse(200), nil
	})}
	doc := &contracts.Document{PHID: "PHID-TASK-recovery", Type: "TASK"}
	done := make(chan error, 1)
	go func() { done <- b.IndexDocument(doc) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("recovery probe did not start")
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.IndexDocument(doc); err == nil {
				t.Error("another probe admitted")
			}
		}()
	}
	wg.Wait()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent probes=%d", calls.Load())
	}
}

func TestRecoveryBackoffIsBoundedAndResetBySuccess(t *testing.T) {
	now := time.Unix(1000, 0)
	b := New(engine.BackendDef{Hosts: []string{"es.invalid"}})
	b.now = func() time.Time { return now }
	for _, want := range []time.Duration{1, 2, 4, 8, 16, 30, 30} {
		b.markHealth("es.invalid", false)
		if got := b.recovery["es.invalid"].retryAt.Sub(now); got != want*time.Second {
			t.Fatalf("backoff=%v want %vs", got, want)
		}
	}
	b.markHealth("es.invalid", true)
	b.markHealth("es.invalid", false)
	if b.recovery["es.invalid"].retryAt.Sub(now) != time.Second {
		t.Fatal("success did not reset backoff")
	}
}

func TestBadDocumentDoesNotFailOverOrExcludeHost(t *testing.T) {
	b := New(engine.BackendDef{Hosts: []string{"a.invalid", "b.invalid"}, Roles: []string{"write"}})
	calls := 0
	b.client = &http.Client{Transport: recoveryTransport(func(*http.Request) (*http.Response, error) { calls++; return recoveryResponse(400), nil })}
	if err := b.IndexDocument(&contracts.Document{PHID: "PHID-TASK-bad", Type: "TASK"}); err == nil {
		t.Fatal("expected rejection")
	}
	if calls != 1 || !b.health["a.invalid"] || !b.health["b.invalid"] {
		t.Fatalf("4xx caused failover or exclusion: calls=%d", calls)
	}
}

func TestRejectedRecoveryProbeRestoresHostForNextDocument(t *testing.T) {
	now := time.Unix(1000, 0)
	b := New(engine.BackendDef{Hosts: []string{"es.invalid"}, Roles: []string{"write"}})
	b.now = func() time.Time { return now }
	b.markHealth("es.invalid", false)
	now = now.Add(time.Second)
	calls := 0
	b.client = &http.Client{Transport: recoveryTransport(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return recoveryResponse(400), nil
		}
		return recoveryResponse(200), nil
	})}
	doc := &contracts.Document{PHID: "PHID-TASK-recovery", Type: "TASK"}
	if err := b.IndexDocument(doc); err == nil || !b.health["es.invalid"] {
		t.Fatalf("explicit rejection must restore responsive host: %v", err)
	}
	if err := b.IndexDocument(doc); err != nil || calls != 2 {
		t.Fatalf("next valid document could not use recovered host: calls=%d err=%v", calls, err)
	}
}

func TestProjectionReleasesRecoveryProbeAfterReadFailure(t *testing.T) {
	for _, operation := range []string{"apply", "uuid"} {
		for _, outcome := range []string{"success", "503", "network", "400"} {
			t.Run(operation+"/"+outcome, func(t *testing.T) {
				now := time.Unix(1000, 0)
				b := New(engine.BackendDef{Hosts: []string{"es.invalid"}, Index: "shadow", Version: 8, Options: map[string]string{"projection": "true"}})
				b.now = func() time.Time { return now }
				event := projectionEvent(t, "2", false)
				calls, projectionCalls := 0, 0
				b.client = &http.Client{Transport: recoveryTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if strings.HasSuffix(r.URL.Path, "/_stats/") {
						return recoveryResponse(503), nil
					}
					projectionCalls++
					if projectionCalls == 1 {
						switch outcome {
						case "503":
							return recoveryResponse(503), nil
						case "400":
							return recoveryResponse(400), nil
						case "network":
							return nil, errors.New("connection lost")
						}
					}
					raw := `{"version":{"number":"8.1.0"}}`
					switch {
					case r.Method == http.MethodPut:
						raw = `{"_id":"` + event.PHID + `","_version":2,"result":"updated","_shards":{"failed":0}}`
					case strings.HasSuffix(r.URL.Path, "/_settings"):
						raw = `{"shadow":{"settings":{"index":{"uuid":"stable"}}}}`
					case strings.HasSuffix(r.URL.Path, "/_mapping"):
						raw = `{"shadow":{"mappings":{"properties":{"_gorge":{"properties":{"namespace":{"type":"keyword"},"generation":{"type":"keyword"},"hash":{"type":"keyword"},"deleted":{"type":"boolean"}}}}}}}`
					}
					resp := recoveryResponse(200)
					resp.Body = io.NopCloser(strings.NewReader(raw))
					return resp, nil
				})}
				invoke := func() error {
					if operation == "uuid" {
						_, err := b.ProjectionIndexUUID(context.Background())
						return err
					}
					_, err := b.ApplyProjection(context.Background(), event, projection.Target{BackendID: "es", GenerationID: "g1"})
					return err
				}
				if _, err := b.IndexExists(); err == nil || b.health["es.invalid"] {
					t.Fatalf("read failure did not exclude host: %v", err)
				}
				now = now.Add(time.Second)
				err := invoke()
				if (err == nil) != (outcome == "success") || b.recovery["es.invalid"].probing {
					t.Fatalf("projection left probe occupied: outcome=%s err=%v state=%+v", outcome, err, b.recovery["es.invalid"])
				}
				if outcome == "503" || outcome == "network" {
					before := calls
					if err := invoke(); err == nil || calls != before || b.health["es.invalid"] {
						t.Fatalf("projection failure ignored cooldown: %v", err)
					}
					now = now.Add(2 * time.Second)
				}
				if err := invoke(); err != nil || !b.health["es.invalid"] || b.recovery["es.invalid"].probing {
					t.Fatalf("subsequent projection could not recover: err=%v state=%+v", err, b.recovery["es.invalid"])
				}
			})
		}
	}
}

func TestDocumentFailoverSharesOverallDeadline(t *testing.T) {
	b := New(engine.BackendDef{Hosts: []string{"a.invalid", "b.invalid", "c.invalid", "d.invalid", "e.invalid", "f.invalid", "g.invalid", "h.invalid"}, Roles: []string{"write"}})
	b.indexTimeout = 55 * time.Millisecond
	calls := 0
	b.client = &http.Client{Timeout: 20 * time.Millisecond, Transport: recoveryTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	start := time.Now()
	err := b.IndexDocument(&contracts.Document{PHID: "PHID-TASK-budget", Type: "TASK"})
	if !errors.Is(err, context.DeadlineExceeded) || calls == 0 || calls >= len(b.hosts) || time.Since(start) > time.Second {
		t.Fatalf("host failover escaped total deadline: calls=%d elapsed=%v err=%v", calls, time.Since(start), err)
	}
}

func TestDocumentWriteHonorsEarlierCallerDeadline(t *testing.T) {
	b := New(engine.BackendDef{Hosts: []string{"a.invalid", "b.invalid"}, Roles: []string{"write"}})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	deadline, _ := ctx.Deadline()
	calls := 0
	b.client = &http.Client{Transport: recoveryTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		got, ok := r.Context().Deadline()
		if !ok || !got.Equal(deadline) {
			t.Errorf("caller deadline was replaced: %v", got)
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	err := b.IndexDocumentContext(ctx, &contracts.Document{PHID: "PHID-TASK-budget", Type: "TASK"})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 || !b.health["a.invalid"] && !b.health["b.invalid"] {
		t.Fatalf("caller cancellation retried another host: calls=%d health=%v err=%v", calls, b.health, err)
	}
}

func TestDocumentWriteHasDefaultDeadlineWithoutCallerContext(t *testing.T) {
	b := New(engine.BackendDef{Hosts: []string{"a.invalid"}, Roles: []string{"write"}})
	b.client = &http.Client{Transport: recoveryTransport(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		remaining := time.Until(deadline)
		if !ok || remaining <= 24*time.Second || remaining > engine.DefaultIndexTimeout {
			t.Errorf("missing or excessive default write budget: %v", remaining)
		}
		return recoveryResponse(200), nil
	})}
	if err := b.IndexDocument(&contracts.Document{PHID: "PHID-TASK-budget", Type: "TASK"}); err != nil {
		t.Fatal(err)
	}
}
