package mailer

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// The test adapter accepts everything and delivers nothing. It backs
// tests/e2e/mailer.sh and the contract fixtures, and it is also what a
// deployment configures to prove the wiring works before pointing it at a real
// backend.
//
// Its failure injection is what makes the permanent-failure and retry paths
// testable without a network: no other backend can be made to fail on demand.
const (
	failModePermanent = "permanent"
	failModeTemporary = "temporary"
)

type testAdapter struct {
	mu       sync.Mutex
	messages []*contracts.EmailMessage
	counter  int
	attempts int

	// failMode is "", "permanent" or "temporary"; failTimes limits the failure
	// to the first N attempts, and 0 means every attempt.
	failMode  string
	failTimes int
}

func newTestAdapter(opts map[string]string) (*testAdapter, error) {
	a := &testAdapter{failMode: opts["fail"]}

	switch a.failMode {
	case "", failModePermanent, failModeTemporary:
	default:
		return nil, fmt.Errorf("test: unknown fail mode %q: expected %q or %q",
			a.failMode, failModePermanent, failModeTemporary)
	}

	if v := opts["fail-times"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("test: fail-times %q is not a number", v)
		}
		a.failTimes = n
	}

	return a, nil
}

func (a *testAdapter) Type() string { return "test" }

func (a *testAdapter) Send(_ context.Context, msg *contracts.EmailMessage) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.attempts++

	if a.failMode != "" && (a.failTimes == 0 || a.attempts <= a.failTimes) {
		err := fmt.Errorf("test: injected %s failure on attempt %d", a.failMode, a.attempts)
		if a.failMode == failModePermanent {
			return "", &PermanentError{Err: err}
		}
		return "", err
	}

	a.counter++
	a.messages = append(a.messages, msg)
	return fmt.Sprintf("test-%d", a.counter), nil
}

// Messages returns the accepted messages, oldest first.
func (a *testAdapter) Messages() []*contracts.EmailMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	cp := make([]*contracts.EmailMessage, len(a.messages))
	copy(cp, a.messages)
	return cp
}

// Attempts returns how many times Send was called, failures included. This is
// how the retry tests count what the dispatcher did.
func (a *testAdapter) Attempts() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.attempts
}

// Reset clears the recorded messages and counters.
func (a *testAdapter) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = nil
	a.counter = 0
	a.attempts = 0
}
