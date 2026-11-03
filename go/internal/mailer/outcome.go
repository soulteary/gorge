package mailer

import "errors"

// SafeRetryError proves the provider did not accept this submission.
// Unclassified failures are deliberately ambiguous, never failover-safe.
type SafeRetryError struct {
	Err     error
	Backend bool
}

func (e *SafeRetryError) Error() string { return e.Err.Error() }
func (e *SafeRetryError) Unwrap() error { return e.Err }
func CanRetry(err error) bool           { var e *SafeRetryError; return errors.As(err, &e) }

// SourceGateError stops this dispatch without changing the wrapped outcome.
// A retryable source error retries the durable delivery later, never another
// provider in this dispatch; the provider has not been called for this gate.
type SourceGateError struct{ Err error }

func (e *SourceGateError) Error() string { return e.Err.Error() }
func (e *SourceGateError) Unwrap() error { return e.Err }
func isSourceGateError(err error) bool   { var e *SourceGateError; return errors.As(err, &e) }
