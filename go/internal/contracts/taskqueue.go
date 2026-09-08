package contracts

// The task queue domain is Phorge's daemon work queue moved behind an HTTP
// API. Its wire structures are the contract shared by the Go service, the PHP
// client (PhabricatorWorkerLeaseQuery / PhabricatorWorkerActiveTask /
// PhabricatorWorker), the OpenAPI document and the contract fixtures.
//
// Every JSON field name below is Phorge's column name, spelled exactly as
// Phorge's PhabricatorWorkerActiveTask and PhabricatorWorkerArchiveTask spell
// it — taskClass, leaseOwner, leaseExpires, failureCount, dataID, failureTime,
// objectPHID, containerPHID. They are read and written by both
// implementations, so none may be renamed. See compat/phorge/README.md.
//
// The worker domain's ConsumerStats is the other structure here. It is the
// payload of GET /api/worker/stats, an in-process counter set rather than a
// database read, and it lives in this package for the same reason the webhook
// domain's DeliveryStats does: it is a wire contract a Phorge setup check
// reads, so its field names are the contract too.

// Task is one row of `{namespace}_worker.worker_activetask`, joined with its
// payload from worker_taskdata.
//
// The pointer-typed epoch fields (LeaseExpires, FailureTime) are nullable
// columns: a task that has never been leased has no leaseExpires, a task that
// has never failed has no failureTime, and encoding those as a present-but-zero
// integer would be indistinguishable from "leased at the epoch" to a reader.
// omitempty on them keeps a fresh task's JSON matching what Phorge's own row
// looks like.
type Task struct {
	ID            int64  `json:"id"`
	TaskClass     string `json:"taskClass"`
	LeaseOwner    string `json:"leaseOwner,omitempty"`
	LeaseExpires  *int64 `json:"leaseExpires,omitempty"`
	FailureCount  int    `json:"failureCount"`
	DataID        int64  `json:"dataID"`
	FailureTime   *int64 `json:"failureTime,omitempty"`
	Priority      int    `json:"priority"`
	ObjectPHID    string `json:"objectPHID,omitempty"`
	ContainerPHID string `json:"containerPHID,omitempty"`
	DateCreated   int64  `json:"dateCreated"`
	DateModified  int64  `json:"dateModified"`
	Data          string `json:"data,omitempty"`
}

// ArchivedTask is one row of worker_archivetask: a task that has left the
// active queue, whether it completed, failed permanently or was cancelled.
//
// It embeds Task so the archived row keeps every field the active row had —
// Phorge's archive table is the active table's columns plus the three below,
// and a reader inspecting a finished task expects the same shape it saw while
// the task was live.
type ArchivedTask struct {
	Task
	// Result is one of the ResultSuccess / ResultFailure / ResultCancelled
	// integers below, matching PhabricatorWorkerArchiveTask's own result
	// codes. It is an int and not a string because that is what the column is.
	Result        int    `json:"result"`
	Duration      int64  `json:"duration"`
	ArchivedEpoch *int64 `json:"archivedEpoch,omitempty"`
}

// EnqueueRequest is the body of POST /api/queue/enqueue. It is what Phorge's
// PhabricatorWorker::scheduleTask sends: the task class, its serialised data,
// and the optional scheduling knobs.
//
// Priority and DelayUntil are pointers so that "absent" is distinct from
// "zero": a caller that omits priority gets PriorityDefault, while one that
// sends 0 means the alerts band. DelayUntil absent means "runnable now".
type EnqueueRequest struct {
	TaskClass     string `json:"taskClass"`
	Data          string `json:"data"`
	Priority      *int   `json:"priority,omitempty"`
	ObjectPHID    string `json:"objectPHID,omitempty"`
	ContainerPHID string `json:"containerPHID,omitempty"`
	DelayUntil    *int64 `json:"delayUntil,omitempty"`
}

// LeaseRequest is the body of POST /api/queue/lease. Limit is how many tasks
// the caller wants; the lease owner travels in the X-Lease-Owner header rather
// than here, because it identifies the caller and not the request.
type LeaseRequest struct {
	Limit int `json:"limit"`
}

// CompleteRequest is the body of POST /api/queue/complete. Duration is the
// task's runtime in microseconds, which is what Phorge stores in the archive
// row and shows in its daemon console.
type CompleteRequest struct {
	TaskID   int64 `json:"taskID"`
	Duration int64 `json:"duration"`
}

// FailRequest is the body of POST /api/queue/fail.
//
// Permanent distinguishes the two failures Phorge's worker layer distinguishes:
// a permanent failure archives the task with ResultFailure and never retries
// it (PhabricatorWorkerPermanentFailureException), while a temporary one
// returns it to the queue with its failureCount incremented and a retry
// backoff applied. RetryWait overrides the service default for the temporary
// case; a nil value means "use the configured RetryWait".
type FailRequest struct {
	TaskID    int64  `json:"taskID"`
	Permanent bool   `json:"permanent"`
	RetryWait *int   `json:"retryWait,omitempty"`
	Message   string `json:"message,omitempty"`
}

// YieldRequest is the body of POST /api/queue/yield: a task that asked to be
// tried again shortly (PhabricatorWorkerYieldException). Duration is the yield
// window in seconds; the store floors it at 5, matching Phorge's own minimum.
type YieldRequest struct {
	TaskID   int64 `json:"taskID"`
	Duration int   `json:"duration"`
}

// AwakenRequest is the body of POST /api/queue/awaken: the ids of yielded tasks
// Phorge wants pulled forward, which is what PhabricatorWorker::awakenTaskIDs
// sends.
type AwakenRequest struct {
	TaskIDs []int64 `json:"taskIDs"`
}

// CancelRequest is the body of POST /api/queue/cancel.
type CancelRequest struct {
	TaskID int64 `json:"taskID"`
}

// QueueStats is the payload of GET /api/queue/stats.
//
// Each count is a live query over the worker tables rather than a counter the
// service keeps, so the numbers survive a restart and describe the queue as
// Phorge's own daemon console sees it — including the work of every worker
// against it.
type QueueStats struct {
	ActiveCount   int64 `json:"activeCount"`
	LeasedCount   int64 `json:"leasedCount"`
	ArchivedCount int64 `json:"archivedCount"`
	FailedCount   int64 `json:"failedCount"`
}

// ConsumerStats is the payload of GET /api/worker/stats.
//
// Unlike QueueStats it is an in-process counter set, so it resets on restart
// and describes only this worker's lifetime: how many tasks it processed, how
// many it failed, how many it is running right now, and which task classes it
// knows how to handle. Supported is `["*"]`-inclusive when a Conduit fallback
// is configured, so a setup check can tell "delegates everything to PHP" from
// "handles a fixed set".
type ConsumerStats struct {
	Processed int64    `json:"processed"`
	Failed    int64    `json:"failed"`
	Active    int32    `json:"active"`
	Supported []string `json:"supported"`
}

// Task result codes, matching PhabricatorWorkerArchiveTask::RESULT_*. They are
// written into worker_archivetask.result and read back by Phorge's daemon
// console, so the integer values are the contract, not just their names.
const (
	ResultSuccess   = 0
	ResultFailure   = 1
	ResultCancelled = 2
)

// Priority bands, matching the values PhabricatorWorker uses when it schedules
// a task. Lower is more urgent; a task with no explicit priority is enqueued at
// PriorityDefault. The queue orders by priority ascending, then id ascending,
// so these decide what a worker leases first.
const (
	PriorityAlerts  = 1000
	PriorityDefault = 2000
	PriorityCommit  = 2500
	PriorityBulk    = 3000
	PriorityIndex   = 3500
	PriorityImport  = 4000
)

// YieldOwner is the sentinel leaseOwner a yielded task carries. It is a value
// in the leaseOwner column rather than a separate status because, like the
// webhook domain's queue, this table's shape is Phorge's and cannot grow a
// column: a yielded task is "leased" by nobody real, and awaken recognises it
// by exactly this string. Renaming it would strand every yielded task.
const YieldOwner = "(yield)"
