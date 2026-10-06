package imagetransform

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"
)

func TestCacheHitAndCanceledWaiter(t *testing.T) {
	data := []byte("test input")
	digest := sha256.Sum256(data)
	key := fmt.Sprintf("%x/profile/legacy-static/%s/backend", digest, Revision)
	s := &Service{BackendRevision: "backend"}
	s.cache.entries = map[string]cachedResult{key: {result: Result{Data: []byte("encoded result")}, expires: time.Now().Add(time.Minute)}}
	s.cache.flights = map[string]*flight{}
	got, e := s.Transform(context.Background(), data, "profile", "legacy-static")
	if e != nil || string(got.Data) != "encoded result" {
		t.Fatalf("cache miss: %+v %v", got, e)
	}
	delete(s.cache.entries, key)
	s.cache.flights[key] = &flight{done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = s.Transform(ctx, data, "profile", "legacy-static"); e != context.Canceled {
		t.Fatalf("waiter did not stop: %v", e)
	}
}

func TestCanceledRequestCannotUseCachedResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Service{}
	if _, err := s.Transform(ctx, []byte("source"), "profile", "legacy-static"); err != context.Canceled {
		t.Fatalf("canceled request admitted: %v", err)
	}
}
func TestSharedWaitHasServiceDeadline(t *testing.T) {
	data := []byte("source")
	d := sha256.Sum256(data)
	key := fmt.Sprintf("%x/profile/legacy-static/%s/backend", d, Revision)
	s := &Service{BackendRevision: "backend", Timeout: 20 * time.Millisecond}
	s.cache.entries = map[string]cachedResult{}
	s.cache.flights = map[string]*flight{key: {done: make(chan struct{})}}
	if _, err := s.Transform(context.Background(), data, "profile", "legacy-static"); err != context.DeadlineExceeded {
		t.Fatalf("wait exceeded service budget: %v", err)
	}
}
