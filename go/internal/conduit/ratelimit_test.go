package conduit

import (
	"sync"
	"testing"
)

func TestRateLimiterBurstThenRefill(t *testing.T) {
	rl := NewRateLimiter(1000, 3, nil)
	defer rl.Stop()

	// A fresh visitor starts with a full burst, so the first three pass.
	for i := 0; i < 3; i++ {
		if !rl.Allow("1.2.3.4", "differential.query") {
			t.Fatalf("request %d within burst should pass", i+1)
		}
	}
	// The fourth exhausts the bucket. With a high refill rate the very next
	// call could refill, so this only asserts the bucket can be emptied, not
	// that the fourth is always refused; a tight-burst refusal is covered by
	// TestRateLimiterRefusesWhenEmpty.
}

func TestRateLimiterRefusesWhenEmpty(t *testing.T) {
	// rps 0.0...: model a limiter that never refills within the test by using a
	// tiny rate. A burst of 1 lets exactly one request through per visitor.
	rl := NewRateLimiter(1, 1, nil)
	defer rl.Stop()

	if !rl.Allow("10.0.0.1", "differential.query") {
		t.Fatal("first request should pass")
	}
	if rl.Allow("10.0.0.1", "differential.query") {
		t.Error("second request should be refused: burst of 1 and no meaningful refill")
	}
}

func TestRateLimiterExemptMethodAlwaysPasses(t *testing.T) {
	rl := NewRateLimiter(1, 1, []string{"conduit.ping"})
	defer rl.Stop()

	for i := 0; i < 100; i++ {
		if !rl.Allow("10.0.0.2", "conduit.ping") {
			t.Fatalf("exempt method should always pass, failed at %d", i)
		}
	}
	// A non-exempt method from the same IP is still limited.
	if !rl.Allow("10.0.0.2", "differential.query") {
		t.Fatal("first non-exempt request should pass")
	}
	if rl.Allow("10.0.0.2", "differential.query") {
		t.Error("second non-exempt request should be refused")
	}
}

func TestRateLimiterDisabledPassesEverything(t *testing.T) {
	rl := NewRateLimiter(0, 0, nil)
	defer rl.Stop()
	for i := 0; i < 1000; i++ {
		if !rl.Allow("10.0.0.3", "differential.query") {
			t.Fatalf("rps 0 disables the limiter, failed at %d", i)
		}
	}
}

func TestRateLimiterPerIPIsolation(t *testing.T) {
	rl := NewRateLimiter(1, 1, nil)
	defer rl.Stop()

	// One IP exhausts its bucket; a different IP is unaffected.
	rl.Allow("10.0.0.4", "differential.query")
	if rl.Allow("10.0.0.4", "differential.query") {
		t.Error("second request from the same IP should be refused")
	}
	if !rl.Allow("10.0.0.5", "differential.query") {
		t.Error("a different IP should have its own full bucket")
	}
}

func TestRateLimiterConcurrentAccessIsSafe(t *testing.T) {
	rl := NewRateLimiter(1000, 100, nil)
	defer rl.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ip := "192.168.1." + string(rune('0'+n%10))
			for j := 0; j < 200; j++ {
				rl.Allow(ip, "differential.query")
			}
		}(i)
	}
	wg.Wait()
	// The test passes if the race detector finds no data race and nothing
	// panics; the exact allow/deny counts are timing-dependent.
}

func TestRateLimiterStopIsIdempotent(t *testing.T) {
	rl := NewRateLimiter(1, 1, nil)
	rl.Stop()
	rl.Stop() // must not panic on a double close.
}
