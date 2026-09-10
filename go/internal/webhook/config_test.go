package webhook

import (
	"os"
	"testing"
)

// clearEnv clears canonical variables and retired aliases used by negative
// regression tests, so a default is independent of the developer's shell.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"GORGE_LISTEN_ADDR", "LISTEN_ADDR", "GORGE_SERVICE_TOKEN", "SERVICE_TOKEN",
		"GORGE_WEBHOOK_MYSQL_HOST", "MYSQL_HOST",
		"GORGE_WEBHOOK_MYSQL_PORT", "MYSQL_PORT",
		"GORGE_WEBHOOK_MYSQL_USER", "MYSQL_USER",
		"GORGE_WEBHOOK_MYSQL_PASS", "MYSQL_PASS",
		"GORGE_WEBHOOK_NAMESPACE", "STORAGE_NAMESPACE",
		"GORGE_WEBHOOK_POLL_INTERVAL_MS", "POLL_INTERVAL_MS",
		"GORGE_WEBHOOK_DELIVERY_TIMEOUT", "DELIVERY_TIMEOUT",
		"GORGE_WEBHOOK_MAX_CONCURRENT", "MAX_CONCURRENT",
		"GORGE_WEBHOOK_ERROR_BACKOFF_SEC", "ERROR_BACKOFF_SEC",
		"GORGE_WEBHOOK_ERROR_THRESHOLD", "ERROR_THRESHOLD",
		"GORGE_WEBHOOK_RETRY_BACKOFF_SEC",
		"GORGE_WEBHOOK_CLAIM_LEASE_SEC",
	} {
		if _, set := os.LookupEnv(key); set {
			t.Setenv(key, "")
			_ = os.Unsetenv(key)
		}
	}
}

func TestDefaultsMatchPhorge(t *testing.T) {
	clearEnv(t)
	cfg := LoadFromEnv()

	for _, tc := range []struct {
		name       string
		got, want  int
		phorgeCite string
	}{
		{"delivery timeout", cfg.DeliveryTimeout, 15,
			"HeraldWebhookWorker's HTTPSFuture::setTimeout(15)"},
		{"error backoff window", cfg.ErrorBackoffSec, 300,
			"HeraldWebhook::getErrorBackoffWindow, 5 minutes"},
		{"error threshold", cfg.ErrorThreshold, 10,
			"HeraldWebhook::getErrorBackoffThreshold"},
		{"retry backoff", cfg.RetryBackoffSec, 60,
			"PhabricatorWorkerLeaseQuery::getDefaultWaitBeforeRetry"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d (%s)", tc.name, tc.got, tc.want, tc.phorgeCite)
		}
	}

	if cfg.ListenAddr != DefaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, DefaultListenAddr)
	}
	if cfg.PollIntervalMs != DefaultPollIntervalMs {
		t.Errorf("PollIntervalMs = %d, want %d", cfg.PollIntervalMs, DefaultPollIntervalMs)
	}
	if cfg.MaxConcurrent != DefaultMaxConcurrent {
		t.Errorf("MaxConcurrent = %d, want %d", cfg.MaxConcurrent, DefaultMaxConcurrent)
	}
}

func TestRetiredWebhookAliasesAreIgnored(t *testing.T) {
	clearEnv(t)
	t.Setenv("MYSQL_HOST", "legacy-db")
	t.Setenv("STORAGE_NAMESPACE", "legacy")
	t.Setenv("SERVICE_TOKEN", "legacy-token")
	t.Setenv("POLL_INTERVAL_MS", "111")

	cfg := LoadFromEnv()
	if cfg.MySQLHost == "legacy-db" {
		t.Error("retired MYSQL_HOST must be ignored")
	}
	if cfg.Namespace == "legacy" {
		t.Error("retired STORAGE_NAMESPACE must be ignored")
	}
	if cfg.ServiceToken != "" {
		t.Errorf("retired SERVICE_TOKEN must be ignored, got %q", cfg.ServiceToken)
	}
	if cfg.PollIntervalMs != DefaultPollIntervalMs {
		t.Errorf("retired POLL_INTERVAL_MS must be ignored, got %d", cfg.PollIntervalMs)
	}

	t.Setenv("GORGE_WEBHOOK_MYSQL_HOST", "current-db")
	t.Setenv("GORGE_WEBHOOK_NAMESPACE", "current")
	t.Setenv("GORGE_SERVICE_TOKEN", "current-token")
	t.Setenv("GORGE_WEBHOOK_POLL_INTERVAL_MS", "222")

	cfg = LoadFromEnv()
	if cfg.MySQLHost != "current-db" || cfg.Namespace != "current" ||
		cfg.ServiceToken != "current-token" || cfg.PollIntervalMs != 222 {
		t.Errorf("canonical Gorge variables must be honored: %+v", cfg)
	}
}

func TestHeraldDSN(t *testing.T) {
	cfg := &Config{
		MySQLUser: "phorge",
		MySQLPass: "secret",
		MySQLHost: "db",
		MySQLPort: 3306,
		Namespace: "phorge",
	}

	const want = "phorge:secret@tcp(db:3306)/phorge_herald?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s"
	if got := cfg.HeraldDSN(); got != want {
		t.Errorf("HeraldDSN() =\n%q\nwant\n%q", got, want)
	}
}

func TestNamespaceReachesTheDSN(t *testing.T) {
	clearEnv(t)
	t.Setenv("GORGE_WEBHOOK_NAMESPACE", "custom")

	if got := LoadFromEnv().HeraldDSN(); got != "phorge:@tcp(127.0.0.1:3306)/custom_herald"+
		"?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s" {
		t.Errorf("HeraldDSN() = %q", got)
	}
}

func TestClaimLeaseNeverUndercutsADelivery(t *testing.T) {
	for _, tc := range []struct {
		lease, timeout, want int
	}{
		{30, 15, 30},
		{5, 15, 15},
		{0, 15, 15},
		{120, 15, 120},
	} {
		cfg := &Config{ClaimLeaseSec: tc.lease, DeliveryTimeout: tc.timeout}
		if got := cfg.ClaimLease(); got != tc.want {
			t.Errorf("lease %d with a %ds timeout gave %d, want %d",
				tc.lease, tc.timeout, got, tc.want)
		}
	}
}

func TestAnUnparseableValueFallsBackToTheDefault(t *testing.T) {
	clearEnv(t)
	t.Setenv("GORGE_WEBHOOK_POLL_INTERVAL_MS", "soon")

	if got := LoadFromEnv().PollIntervalMs; got != DefaultPollIntervalMs {
		t.Errorf("PollIntervalMs = %d, want the default %d", got, DefaultPollIntervalMs)
	}
}
