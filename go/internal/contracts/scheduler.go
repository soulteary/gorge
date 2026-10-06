package contracts

// TriggerPlan is the versioned, read-only PHP clock/action export. It is applied
// only against a matching writer snapshot in the queue/event transaction.
type TriggerPlan struct {
	Protocol int             `json:"protocol"`
	ID       int64           `json:"triggerID"`
	Version  int64           `json:"version"`
	Fire     bool            `json:"fire"`
	Next     *int64          `json:"nextEpoch"`
	Task     *EnqueueRequest `json:"task"`
}
