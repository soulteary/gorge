package hub

import (
	"testing"
	"time"
)

func TestToStringSlice(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want []string
		ok   bool
	}{
		{"nil", nil, nil, false},
		{"string slice", []string{"a", "b"}, []string{"a", "b"}, true},
		{"any slice", []any{"x", "y"}, []string{"x", "y"}, true},
		{"empty any slice", []any{}, []string{}, false},
		{"mixed any", []any{"a", 42}, []string{"a"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ToStringSlice(tt.in)
			if ok != tt.ok {
				t.Errorf("ok = %v, want %v", ok, tt.ok)
			}
			if len(got) != len(tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestHubPublishAndHistory(t *testing.T) {
	h := New()

	msg := Message{
		"type":        "notification",
		"key":         "12345",
		"subscribers": []string{"PHID-USER-aaa"},
	}

	h.Publish("default", msg)

	history := h.GetHistory(time.Now().Add(-time.Second))
	if len(history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(history))
	}
	if history[0]["key"] != "12345" {
		t.Errorf("expected key=12345, got %v", history[0]["key"])
	}
}

// TestHistoryIsScopedByAge covers what a reconnecting client's replay window
// rests on: GetHistory answers with the messages at or after the cutoff only.
func TestHistoryIsScopedByAge(t *testing.T) {
	h := New()
	h.Publish("default", Message{"key": "old"})

	if got := h.GetHistory(time.Now().Add(time.Second)); len(got) != 0 {
		t.Errorf("expected nothing at or after a future cutoff, got %d entries", len(got))
	}
	if got := h.GetHistory(time.Now().Add(-time.Second)); len(got) != 1 {
		t.Errorf("expected 1 entry within the window, got %d", len(got))
	}
}

func TestHubStatus(t *testing.T) {
	h := New()
	status := h.Status("test")

	if status.Instance != "test" {
		t.Errorf("expected instance=test, got %q", status.Instance)
	}
	if status.Version != protocolVersion {
		t.Errorf("expected version=%d, got %d", protocolVersion, status.Version)
	}
	if status.MessagesIn != 0 {
		t.Errorf("expected messages.in=0, got %d", status.MessagesIn)
	}
	// Phorge reads history.age only when history.size is non-zero, so an empty
	// history must report null rather than a misleading 0.
	if status.HistorySize != 0 {
		t.Errorf("expected history.size=0, got %d", status.HistorySize)
	}
	if status.HistoryAge != nil {
		t.Errorf("expected a null history.age on an empty hub, got %d", *status.HistoryAge)
	}
}

func TestHubStatusCountsPublishedMessages(t *testing.T) {
	h := New()
	h.Publish("default", Message{"key": "1"})
	h.Publish("default", Message{"key": "2"})

	status := h.Status("default")
	if status.MessagesIn != 2 {
		t.Errorf("expected messages.in=2, got %d", status.MessagesIn)
	}
	if status.HistorySize != 2 {
		t.Errorf("expected history.size=2, got %d", status.HistorySize)
	}
	if status.HistoryAge == nil {
		t.Fatal("expected a history.age once messages are held")
	}
}

// TestStatusInstancesAreIndependent guards the fan-out boundary: a message for
// one instance must not show up in another instance's client counts.
func TestStatusInstancesAreIndependent(t *testing.T) {
	h := New()
	h.AddListener("prod", NewListener(h.NextID(), nil))

	if active := h.Status("prod").ClientsActive; active != 1 {
		t.Errorf("prod: expected 1 active client, got %d", active)
	}
	if active := h.Status("staging").ClientsActive; active != 0 {
		t.Errorf("staging: expected 0 active clients, got %d", active)
	}
}

func TestHubHistoryPurge(t *testing.T) {
	h := New()

	for i := 0; i < 10; i++ {
		h.Publish("default", Message{"key": i})
	}

	history := h.GetHistory(time.Now().Add(-time.Second))
	if len(history) != 10 {
		t.Fatalf("expected 10, got %d", len(history))
	}
}

// TestHistoryPurgeHonoursTheSizeLimit pushes past historySizeLimit, which the
// age limit alone would never reach in a test.
func TestHistoryPurgeHonoursTheSizeLimit(t *testing.T) {
	h := New()

	for i := 0; i < historySizeLimit+50; i++ {
		h.Publish("default", Message{"key": i})
	}

	if got := h.Status("default").HistorySize; got != historySizeLimit {
		t.Errorf("expected the history capped at %d, got %d", historySizeLimit, got)
	}
	// The oldest entries are the ones dropped.
	history := h.GetHistory(time.Now().Add(-time.Minute))
	if len(history) == 0 {
		t.Fatal("expected history entries")
	}
	if history[0]["key"] != 50 {
		t.Errorf("expected the oldest surviving key to be 50, got %v", history[0]["key"])
	}
}

// TestListenerSubscriptions covers the filter Publish applies to every message.
// A nil connection is enough here: none of these methods touch it.
func TestListenerSubscriptions(t *testing.T) {
	l := NewListener(1, nil)

	if l.IsSubscribedToAny([]string{"PHID-USER-aaa"}) {
		t.Error("a fresh listener should be subscribed to nothing")
	}

	l.Subscribe([]string{"PHID-USER-aaa", "PHID-USER-bbb"})
	if !l.IsSubscribedToAny([]string{"PHID-USER-bbb", "PHID-USER-zzz"}) {
		t.Error("expected a match on any of the listed PHIDs")
	}
	if l.IsSubscribedToAny([]string{"PHID-USER-zzz"}) {
		t.Error("expected no match on an unsubscribed PHID")
	}

	l.Unsubscribe([]string{"PHID-USER-aaa"})
	if l.IsSubscribedToAny([]string{"PHID-USER-aaa"}) {
		t.Error("expected the unsubscribed PHID to stop matching")
	}
	if !l.IsSubscribedToAny([]string{"PHID-USER-bbb"}) {
		t.Error("unsubscribing one PHID must not drop the others")
	}
}

func TestListenerIDsAreUnique(t *testing.T) {
	h := New()

	seen := make(map[uint64]struct{})
	for i := 0; i < 100; i++ {
		id := h.NextID()
		if _, dup := seen[id]; dup {
			t.Fatalf("id %d handed out twice", id)
		}
		seen[id] = struct{}{}
	}
}
