package conduit

import (
	"reflect"
	"testing"
)

func TestLoadFromEnvDefaults(t *testing.T) {
	for _, k := range allConduitEnvKeys() {
		t.Setenv(k, "")
	}
	cfg := LoadFromEnv()

	if cfg.ListenAddr != DefaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, DefaultListenAddr)
	}
	if cfg.UpstreamURL != DefaultUpstreamURL {
		t.Errorf("UpstreamURL = %q, want %q", cfg.UpstreamURL, DefaultUpstreamURL)
	}
	if cfg.ProxyTimeoutSec != DefaultProxyTimeoutSec {
		t.Errorf("ProxyTimeoutSec = %d, want %d", cfg.ProxyTimeoutSec, DefaultProxyTimeoutSec)
	}
	if cfg.MaxBodySize != DefaultMaxBodySize {
		t.Errorf("MaxBodySize = %q, want %q", cfg.MaxBodySize, DefaultMaxBodySize)
	}
	if cfg.RateLimitRPS != DefaultRateLimitRPS {
		t.Errorf("RateLimitRPS = %d, want %d", cfg.RateLimitRPS, DefaultRateLimitRPS)
	}
	if cfg.RateLimitBurst != DefaultRateLimitBurst {
		t.Errorf("RateLimitBurst = %d, want %d", cfg.RateLimitBurst, DefaultRateLimitBurst)
	}
	want := []string{"conduit.ping", "conduit.getcapabilities"}
	if !reflect.DeepEqual(cfg.RateLimitExempt, want) {
		t.Errorf("RateLimitExempt = %v, want %v", cfg.RateLimitExempt, want)
	}
}

func TestLoadFromEnvCanonicalNamesWin(t *testing.T) {
	t.Setenv("GORGE_CONDUIT_UPSTREAM_URL", "http://canonical:80")
	t.Setenv("UPSTREAM_URL", "http://retired:80")
	t.Setenv("GORGE_CONDUIT_RATE_LIMIT_RPS", "50")
	t.Setenv("RATE_LIMIT_RPS", "5")

	cfg := LoadFromEnv()
	if cfg.UpstreamURL != "http://canonical:80" {
		t.Errorf("UpstreamURL = %q, want canonical value", cfg.UpstreamURL)
	}
	if cfg.RateLimitRPS != 50 {
		t.Errorf("RateLimitRPS = %d, want 50", cfg.RateLimitRPS)
	}
}

func TestLoadFromEnvIgnoresRetiredAliases(t *testing.T) {
	for _, k := range allConduitEnvKeys() {
		t.Setenv(k, "")
	}
	t.Setenv("UPSTREAM_URL", "http://retired:80")
	t.Setenv("RATE_LIMIT_RPS", "7")
	t.Setenv("RATE_LIMIT_BURST", "13")
	t.Setenv("PROXY_TIMEOUT_SEC", "11")
	t.Setenv("MAX_BODY_SIZE", "3M")
	t.Setenv("RATE_LIMIT_EXEMPT", "a.b,c.d")
	t.Setenv("SERVICE_TOKEN", "retired-secret")

	cfg := LoadFromEnv()
	if cfg.UpstreamURL != DefaultUpstreamURL {
		t.Errorf("retired UPSTREAM_URL changed config: %q", cfg.UpstreamURL)
	}
	if cfg.RateLimitRPS != DefaultRateLimitRPS ||
		cfg.RateLimitBurst != DefaultRateLimitBurst ||
		cfg.ProxyTimeoutSec != DefaultProxyTimeoutSec {
		t.Errorf("retired limit aliases changed config: rps %d burst %d timeout %d",
			cfg.RateLimitRPS, cfg.RateLimitBurst, cfg.ProxyTimeoutSec)
	}
	if cfg.MaxBodySize != DefaultMaxBodySize {
		t.Errorf("retired MAX_BODY_SIZE changed config: %q", cfg.MaxBodySize)
	}
	if cfg.ServiceToken != "" {
		t.Errorf("retired SERVICE_TOKEN changed config: %q", cfg.ServiceToken)
	}
	want := []string{"conduit.ping", "conduit.getcapabilities"}
	if !reflect.DeepEqual(cfg.RateLimitExempt, want) {
		t.Errorf("retired RATE_LIMIT_EXEMPT changed config: %v", cfg.RateLimitExempt)
	}
}

func TestLoadFromEnvInvalidIntFallsBack(t *testing.T) {
	for _, k := range allConduitEnvKeys() {
		t.Setenv(k, "")
	}
	t.Setenv("GORGE_CONDUIT_RATE_LIMIT_RPS", "not-a-number")
	cfg := LoadFromEnv()
	if cfg.RateLimitRPS != DefaultRateLimitRPS {
		t.Errorf("an unparseable RPS should fall back to %d, got %d", DefaultRateLimitRPS, cfg.RateLimitRPS)
	}
}

func TestSplitCSV(t *testing.T) {
	if got := splitCSV(""); got != nil {
		t.Errorf("splitCSV(\"\") = %v, want nil", got)
	}
	if got := splitCSV("  ,  ,"); len(got) != 0 {
		t.Errorf("splitCSV of only blanks = %v, want empty", got)
	}
	got := splitCSV(" one , two ,three")
	want := []string{"one", "two", "three"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("splitCSV = %v, want %v", got, want)
	}
}

func allConduitEnvKeys() []string {
	return []string{
		"GORGE_LISTEN_ADDR", "LISTEN_ADDR",
		"GORGE_SERVICE_TOKEN", "SERVICE_TOKEN",
		"GORGE_CONDUIT_UPSTREAM_URL", "UPSTREAM_URL",
		"GORGE_CONDUIT_PROXY_TIMEOUT_SEC", "PROXY_TIMEOUT_SEC",
		"GORGE_CONDUIT_MAX_BODY_SIZE", "MAX_BODY_SIZE",
		"GORGE_CONDUIT_RATE_LIMIT_RPS", "RATE_LIMIT_RPS",
		"GORGE_CONDUIT_RATE_LIMIT_BURST", "RATE_LIMIT_BURST",
		"GORGE_CONDUIT_RATE_LIMIT_EXEMPT", "RATE_LIMIT_EXEMPT",
	}
}
