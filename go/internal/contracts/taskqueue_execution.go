package contracts

// ExecutionLease identifies a particular ownership window without changing
// Phorge's tables. Both owner and expiry must match the current row.
type ExecutionLease struct {
	TaskID       int64  `json:"taskID"`
	LeaseOwner   string `json:"leaseOwner"`
	LeaseExpires int64  `json:"leaseExpires"`
}

// FinalizeRequest commits a successful parent and every staged child together.
// Replaying the same ownership window after a lost response is idempotent.
type FinalizeRequest struct {
	ExecutionLease
	Duration  int64            `json:"duration"`
	Followups []EnqueueRequest `json:"followups"`
}

type RenewRequest struct {
	ExecutionLease
	Duration int `json:"duration"`
}

type ExecutionCapabilities struct {
	ExecutionVersion int  `json:"executionVersion"`
	LeaseOutcomes    bool `json:"leaseOutcomes"`
}

// ResolveRequest records a fenced failure or yield. Success uses Finalize.
type ResolveRequest struct {
	ExecutionLease
	Outcome   string `json:"outcome"`
	RetryWait *int   `json:"retryWait,omitempty"`
	Duration  int    `json:"duration,omitempty"`
}

type EnqueueEventRequest struct {
	EventID string         `json:"eventID"`
	Task    EnqueueRequest `json:"task"`
}
