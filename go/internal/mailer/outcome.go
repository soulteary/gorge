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
