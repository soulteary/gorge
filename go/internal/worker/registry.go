package worker

import (
	"context"
	"encoding/json"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// TaskHandler processes a single leased task. Its return value decides how the
// result is reported back to the queue:
//
//   - nil is success (Complete).
//   - *PermanentError is a failure that will never succeed (Fail, permanent):
//     bad task data, a missing handler. It matches Phorge's
//     PhabricatorWorkerPermanentFailureException.
//   - *YieldError asks for the task to be retried shortly (Yield), matching
//     PhabricatorWorkerYieldException.
//   - any other error is a temporary failure (Fail, temporary): the task
//     returns to the queue with its failureCount incremented.
type TaskHandler func(ctx context.Context, task *contracts.Task, data json.RawMessage) error

// PermanentError marks a failure that retrying cannot fix.
type PermanentError struct{ Msg string }

func (e *PermanentError) Error() string { return e.Msg }

// YieldError asks the queue to try the task again after Duration seconds. The
// queue floors the value at 5, matching Phorge's own minimum.
type YieldError struct {
	Msg      string
	Duration int
}

func (e *YieldError) Error() string { return e.Msg }

// Registry maps a task class to the handler that runs it, with an optional
// fallback for classes not registered explicitly.
type Registry struct {
	handlers map[string]TaskHandler
	fallback TaskHandler
}

func NewRegistry() *Registry {
	return &Registry{handlers: make(map[string]TaskHandler)}
}

// Register binds a handler to a task class.
func (r *Registry) Register(taskClass string, handler TaskHandler) {
	r.handlers[taskClass] = handler
}

// SetFallback installs a handler for any task class not registered explicitly.
// This is how the Conduit delegate turns the worker into a universal consumer:
// with a fallback set, every class the worker leases is handled, either
// natively or by handing it back to PHP. See handlers.RegisterAll.
func (r *Registry) SetFallback(handler TaskHandler) {
	r.fallback = handler
}

// Get returns the handler for a class, or the fallback, reporting whether one
// exists.
func (r *Registry) Get(taskClass string) (TaskHandler, bool) {
	if h, ok := r.handlers[taskClass]; ok {
		return h, true
	}
	if r.fallback != nil {
		return r.fallback, true
	}
	return nil, false
}

// Has reports whether the class can be handled at all.
func (r *Registry) Has(taskClass string) bool {
	if _, ok := r.handlers[taskClass]; ok {
		return true
	}
	return r.fallback != nil
}

// SupportedClasses lists the registered classes, with "*" appended when a
// fallback is set. It is the Supported field of the stats endpoint, so a setup
// check can tell "delegates everything to PHP" from "handles a fixed set".
func (r *Registry) SupportedClasses() []string {
	classes := make([]string, 0, len(r.handlers)+1)
	for k := range r.handlers {
		classes = append(classes, k)
	}
	if r.fallback != nil {
		classes = append(classes, "*")
	}
	return classes
}
