package webhook

import (
	"github.com/soulteary/gorge/go/internal/platform/config"
)

// Defaults for the webhook delivery service. The listen address is the
// pre-monorepo :8160, which the existing deployments already point at.
const (
	DefaultListenAddr = ":8160"

	// DefaultMySQLHost is a real default rather than an empty switch, unlike
	// the file storage domain's. There is nothing this service can do without
	// the database — both endpoints read it and the dispatcher is a loop
	// around it — so "not configured" is not a state worth distinguishing
	// from "unreachable". Either way /readyz reports it.
	DefaultMySQLHost = "127.0.0.1"
	DefaultMySQLPort = 3306
	DefaultMySQLUser = "phorge"
	DefaultNamespace = "phorge"

	// DefaultPollIntervalMs is how often the queue is examined. It is short
	// because it is also the delivery latency for a change Phorge has just
	// recorded, and the claim is what makes a short interval safe: before it,
	// a tick faster than a delivery re-fetched rows that were still in flight.
	DefaultPollIntervalMs = 1000

	// DefaultDeliveryTimeout matches the 15 seconds HeraldWebhookWorker gives
	// its own HTTPSFuture. A receiver that Phorge would have timed out must
	// not be given longer here, or the two implementations disagree about
	// whether a slow endpoint succeeded.
	DefaultDeliveryTimeout = 15

	DefaultMaxConcurrent = 8

	// DefaultErrorBackoffSec and DefaultErrorThreshold are the per-hook
	// circuit breaker, and both are Phorge's own values:
	// HeraldWebhook::getErrorBackoffWindow returns 5 minutes and
	// getErrorBackoffThreshold returns 10. They stop delivery to a hook whose
	// endpoint is broken rather than to one request.
	DefaultErrorBackoffSec = 300
	DefaultErrorThreshold  = 10

	// DefaultRetryBackoffSec is how long one failed request waits before it is
	// attempted again, and 60 seconds is what Phorge waits: a worker that
	// throws is re-leased by PhabricatorWorkerLeaseQuery::getDefaultWaitBefore-
	// Retry(). The standalone service had no equivalent and retried on the
	// next tick, so a request needed ten failures a second apart to reach the
	// circuit breaker above.
	//
	// It is a different quantity from ErrorBackoffSec and must stay one: this
	// paces the retries of a single request, that one stops a whole hook.
	DefaultRetryBackoffSec = 60

	// DefaultClaimLeaseSec is how long a claimed request is left alone before
	// another attempt may take it. It is the answer to "the process that
	// claimed this row died", and nothing else — a delivery that finishes
	// writes a result and leaves the queue.
	//
	// It has to cover a whole delivery attempt, so it is checked against
	// DeliveryTimeout at startup; see ClaimLease.
	DefaultClaimLeaseSec = 30
)

// Config is the webhook delivery service configuration.
type Config struct {
	config.Base

	// Phorge's database. The service reads herald_webhook and reads *and
	// writes* herald_webhookrequest; it needs no other schema and no DDL
	// rights.
	MySQLHost string
	MySQLPort int
	MySQLUser string
	MySQLPass string
	Namespace string

	// Delivery behaviour.
	PollIntervalMs  int
	DeliveryTimeout int
	MaxConcurrent   int
	ErrorBackoffSec int
	ErrorThreshold  int
	RetryBackoffSec int
	ClaimLeaseSec   int
}

// LoadFromEnv reads the configuration from the environment. The GORGE_WEBHOOK_
// names are the current ones; the unprefixed names are what the pre-monorepo
// deployment used and are kept as a fallback. See docs/platform.md section 4.
//
// RetryBackoffSec and ClaimLeaseSec have no unprefixed spelling on purpose:
// neither setting existed before the monorepo, so there is no old deployment
// whose environment could be naming them.
func LoadFromEnv() *Config {
	return &Config{
		Base: config.LoadBase(DefaultListenAddr),

		MySQLHost: config.EnvStr(DefaultMySQLHost, "GORGE_WEBHOOK_MYSQL_HOST", "MYSQL_HOST"),
		MySQLPort: config.EnvInt(DefaultMySQLPort, "GORGE_WEBHOOK_MYSQL_PORT", "MYSQL_PORT"),
		MySQLUser: config.EnvStr(DefaultMySQLUser, "GORGE_WEBHOOK_MYSQL_USER", "MYSQL_USER"),
		MySQLPass: config.EnvStr("", "GORGE_WEBHOOK_MYSQL_PASS", "MYSQL_PASS"),
		Namespace: config.EnvStr(DefaultNamespace, "GORGE_WEBHOOK_NAMESPACE", "STORAGE_NAMESPACE"),

		PollIntervalMs:  config.EnvInt(DefaultPollIntervalMs, "GORGE_WEBHOOK_POLL_INTERVAL_MS", "POLL_INTERVAL_MS"),
		DeliveryTimeout: config.EnvInt(DefaultDeliveryTimeout, "GORGE_WEBHOOK_DELIVERY_TIMEOUT", "DELIVERY_TIMEOUT"),
		MaxConcurrent:   config.EnvInt(DefaultMaxConcurrent, "GORGE_WEBHOOK_MAX_CONCURRENT", "MAX_CONCURRENT"),
		ErrorBackoffSec: config.EnvInt(DefaultErrorBackoffSec, "GORGE_WEBHOOK_ERROR_BACKOFF_SEC", "ERROR_BACKOFF_SEC"),
		ErrorThreshold:  config.EnvInt(DefaultErrorThreshold, "GORGE_WEBHOOK_ERROR_THRESHOLD", "ERROR_THRESHOLD"),
		RetryBackoffSec: config.EnvInt(DefaultRetryBackoffSec, "GORGE_WEBHOOK_RETRY_BACKOFF_SEC"),
		ClaimLeaseSec:   config.EnvInt(DefaultClaimLeaseSec, "GORGE_WEBHOOK_CLAIM_LEASE_SEC"),
	}
}

// ClaimLease is the lease actually used, which is never shorter than a
// delivery attempt may take.
//
// A lease below DeliveryTimeout would reintroduce the duplicate delivery the
// claim exists to prevent: the row stays `queued` for the whole attempt — the
// status values are Phorge's and cannot grow a `claimed` member — so once the
// lease expires a second attempt is free to take a row whose first POST is
// still in flight.
func (c *Config) ClaimLease() int {
	if c.ClaimLeaseSec < c.DeliveryTimeout {
		return c.DeliveryTimeout
	}
	return c.ClaimLeaseSec
}
