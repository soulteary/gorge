package webhook

import "github.com/soulteary/gorge/go/internal/platform/config"

const (
	DefaultListenAddr      = ":8160"
	DefaultMySQLHost       = "127.0.0.1"
	DefaultMySQLPort       = 3306
	DefaultMySQLUser       = "phorge"
	DefaultNamespace       = "phorge"
	DefaultPollIntervalMs  = 1000
	DefaultDeliveryTimeout = 15
	DefaultMaxConcurrent   = 8
	DefaultErrorBackoffSec = 300
	DefaultErrorThreshold  = 10
	DefaultRetryBackoffSec = 60
	DefaultClaimLeaseSec   = 30
)

type Config struct {
	config.Base
	MySQLHost       string
	MySQLPort       int
	MySQLUser       string
	MySQLPass       string
	Namespace       string
	PollIntervalMs  int
	DeliveryTimeout int
	MaxConcurrent   int
	ErrorBackoffSec int
	ErrorThreshold  int
	RetryBackoffSec int
	ClaimLeaseSec   int
}

// LoadFromEnv reads only the current GORGE_WEBHOOK_* variables. The standalone
// MYSQL_*/STORAGE_NAMESPACE and delivery aliases are intentionally no longer
// accepted now that Gorge is released and deployed as one versioned stack.
func LoadFromEnv() *Config {
	return &Config{
		Base:            config.LoadBase(DefaultListenAddr),
		MySQLHost:       config.EnvStr(DefaultMySQLHost, "GORGE_WEBHOOK_MYSQL_HOST"),
		MySQLPort:       config.EnvInt(DefaultMySQLPort, "GORGE_WEBHOOK_MYSQL_PORT"),
		MySQLUser:       config.EnvStr(DefaultMySQLUser, "GORGE_WEBHOOK_MYSQL_USER"),
		MySQLPass:       config.EnvStr("", "GORGE_WEBHOOK_MYSQL_PASS"),
		Namespace:       config.EnvStr(DefaultNamespace, "GORGE_WEBHOOK_NAMESPACE"),
		PollIntervalMs:  config.EnvInt(DefaultPollIntervalMs, "GORGE_WEBHOOK_POLL_INTERVAL_MS"),
		DeliveryTimeout: config.EnvInt(DefaultDeliveryTimeout, "GORGE_WEBHOOK_DELIVERY_TIMEOUT"),
		MaxConcurrent:   config.EnvInt(DefaultMaxConcurrent, "GORGE_WEBHOOK_MAX_CONCURRENT"),
		ErrorBackoffSec: config.EnvInt(DefaultErrorBackoffSec, "GORGE_WEBHOOK_ERROR_BACKOFF_SEC"),
		ErrorThreshold:  config.EnvInt(DefaultErrorThreshold, "GORGE_WEBHOOK_ERROR_THRESHOLD"),
		RetryBackoffSec: config.EnvInt(DefaultRetryBackoffSec, "GORGE_WEBHOOK_RETRY_BACKOFF_SEC"),
		ClaimLeaseSec:   config.EnvInt(DefaultClaimLeaseSec, "GORGE_WEBHOOK_CLAIM_LEASE_SEC"),
	}
}

func (c *Config) ClaimLease() int {
	if c.ClaimLeaseSec < c.DeliveryTimeout {
		return c.DeliveryTimeout
	}
	return c.ClaimLeaseSec
}
