package hub

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEmptyInstanceConnectionChurnKeepsRegistryBounded(t *testing.T) {
	h := New()
	for i := 0; i < MaxInstances*4; i++ {
		instance := fmt.Sprintf("public-empty-%d", i)
		l := NewListener(h.NextID(), nil)
		if err := h.AddListener(instance, l); err != nil {
			t.Fatalf("idle cache blocked admission at %d: %v", i, err)
		}
		h.RemoveListener(instance, l.ID())
		l.Close()
		if len(h.instances) > MaxInstances {
			t.Fatalf("registry grew to %d instances", len(h.instances))
		}
	}
	if h.getList("public-empty-0") != nil {
		t.Fatal("old empty instance was never reclaimed")
	}
}

func TestInspectionPublicationAndUnknownCleanupDoNotAllocateInstances(t *testing.T) {
	h := New()
	for i := 0; i < MaxInstances*2; i++ {
		instance := fmt.Sprintf("unknown-%d", i)
		h.Status(instance)
		h.RemoveListener(instance, 1)
		h.Publish(instance, Message{"key": instance})
	}
	if len(h.instances) != 0 {
		t.Fatalf("nonconnection requests allocated %d instance caches", len(h.instances))
	}
	if history := h.GetHistory("unknown-0", time.Now().Add(-time.Minute)); len(history) != 1 {
		t.Fatal("publication without listeners lost replay history")
	}
}

func TestInstanceAdmissionProtectsActiveConnectionsAndHistory(t *testing.T) {
	for _, protectedBy := range []string{"connections", "history"} {
		t.Run(protectedBy, func(t *testing.T) {
			h := New()
			for i := 0; i < MaxInstances; i++ {
				instance := fmt.Sprintf("protected-%d", i)
				l := NewListener(h.NextID(), nil)
				if err := h.AddListener(instance, l); err != nil {
					t.Fatal(err)
				}
				if protectedBy == "history" {
					h.RemoveListener(instance, l.ID())
					l.Close()
					h.Publish(instance, Message{"key": instance})
				} else {
					defer l.Close()
				}
			}
			extra := NewListener(h.NextID(), nil)
			defer extra.Close()
			if err := h.AddListener("extra", extra); !errors.Is(err, ErrResourceLimit) {
				t.Fatalf("protected capacity was not enforced: %v", err)
			}
			if h.getList("extra") != nil || len(h.instances) != MaxInstances {
				t.Fatal("failed admission allocated or displaced a protected instance")
			}
			if protectedBy == "history" {
				if history := h.GetHistory("protected-0", time.Now().Add(-time.Minute)); len(history) != 1 {
					t.Fatal("capacity eviction lost protected history")
				}
			} else if h.Status("protected-0").ClientsActive != 1 {
				t.Fatal("capacity eviction removed an active listener")
			}
			// A full registry still admits another connection to an existing
			// instance: it does not need an additional registry entry.
			if err := h.AddListener("protected-0", extra); err != nil {
				t.Fatalf("existing instance was blocked by registry capacity: %v", err)
			}
		})
	}
}

func TestIdleEvictionPreservesLiveHistory(t *testing.T) {
	h := New()
	for i := 0; i < MaxInstances; i++ {
		instance := fmt.Sprintf("idle-%d", i)
		l := NewListener(h.NextID(), nil)
		if err := h.AddListener(instance, l); err != nil {
			t.Fatal(err)
		}
		h.RemoveListener(instance, l.ID())
		l.Close()
	}
	h.Publish("idle-0", Message{"key": "retained"})
	l := NewListener(h.NextID(), nil)
	defer l.Close()
	if err := h.AddListener("new", l); err != nil {
		t.Fatal(err)
	}
	if h.getList("idle-0") == nil || h.getList("idle-1") != nil {
		t.Fatal("eviction did not choose the oldest unprotected idle instance")
	}
	if history := h.GetHistory("idle-0", time.Now().Add(-time.Minute)); len(history) != 1 {
		t.Fatal("idle eviction lost retained history")
	}
}

func TestInstanceNameBytesAreBounded(t *testing.T) {
	h := New()
	l := NewListener(h.NextID(), nil)
	defer l.Close()
	if err := h.AddListener(strings.Repeat("x", MaxInstanceBytes+1), l); !errors.Is(err, ErrResourceLimit) || len(h.instances) != 0 {
		t.Fatalf("oversized instance allocated a registry entry: %v", err)
	}
	if err := h.AddListener(strings.Repeat("x", MaxInstanceBytes), l); err != nil {
		t.Fatal(err)
	}
}

func TestOversizedPublishedInstanceRetainsNoState(t *testing.T) {
	h := New()
	for i := 0; i < historySizeLimit; i++ {
		h.Publish(strings.Repeat("x", MaxInstanceBytes+1), Message{"key": "small"})
	}
	if len(h.instances) != 0 || len(h.history) != 0 || h.historyBytes != 0 || h.messagesIn.Load() != 0 {
		t.Fatalf("oversized publication retained state: instances=%d history=%d bytes=%d", len(h.instances), len(h.history), h.historyBytes)
	}
}

func TestHistoryBudgetIncludesInstanceNameBytes(t *testing.T) {
	h := New()
	instance := strings.Repeat("x", MaxInstanceBytes)
	h.Publish(instance, Message{})
	// The tiny encoded message must not conceal the retained instance string.
	if h.historyBytes != MaxInstanceBytes+len(`{}`) || len(h.history) != 1 {
		t.Fatalf("instance bytes missing from history budget: %d", h.historyBytes)
	}
}

func TestConcurrentInstanceChurnDoesNotEvictActiveListeners(t *testing.T) {
	h := New()
	var wg sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < MaxInstances/4; i++ {
				instance := fmt.Sprintf("worker-%d-%d", worker, i)
				l := NewListener(h.NextID(), nil)
				if err := h.AddListener(instance, l); err != nil {
					t.Errorf("idle capacity did not admit concurrent connection: %v", err)
					l.Close()
					return
				}
				if h.Status(instance).ClientsActive != 1 {
					t.Error("active instance was evicted")
				}
				h.RemoveListener(instance, l.ID())
				l.Close()
			}
		}(worker)
	}
	wg.Wait()
	if len(h.instances) > MaxInstances {
		t.Fatalf("concurrent admission escaped registry budget: %d", len(h.instances))
	}
	for _, ll := range h.instances {
		if ll.activeCount() != 0 {
			t.Fatal("disconnected listener retained after concurrent churn")
		}
	}
}
