package mailer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mailerEnvKeys is every variable Load consults, new names and legacy ones.
var mailerEnvKeys = []string{
	"GORGE_LISTEN_ADDR", "LISTEN_ADDR",
	"GORGE_SERVICE_TOKEN", "SERVICE_TOKEN",
	"GORGE_MAILER_CONFIG", "MAILER_CONFIG",
	"GORGE_MAILER_CONFIG_FILE", "MAILER_CONFIG_FILE",
	"GORGE_MAILER_MAX_RETRIES", "MAX_RETRIES",
	"GORGE_MAILER_RETRY_WAIT", "RETRY_WAIT",
	"GORGE_MAILER_BODY_LIMIT", "BODY_LIMIT",
	"GORGE_MAILER_TYPE", "MAILER_TYPE",
	"GORGE_MAILER_KEY", "MAILER_KEY",
	"SMTP_HOST", "SMTP_PORT", "SMTP_USER", "SMTP_PASSWORD", "SMTP_PROTOCOL",
	"MAILER_ACCESS_KEY", "MAILER_SECRET_KEY", "MAILER_REGION", "MAILER_ENDPOINT",
	"MAILER_API_KEY", "MAILER_DOMAIN", "MAILER_API_HOSTNAME", "MAILER_ACCESS_TOKEN",
}

func clearMailerEnv(t *testing.T) {
	t.Helper()
	for _, key := range mailerEnvKeys {
		t.Setenv(key, "")
	}
}

func TestMailerConfigDefaults(t *testing.T) {
	clearMailerEnv(t)

	cfg := LoadFromEnv()
	if cfg.ListenAddr != ":8110" {
		t.Errorf("expected :8110, got %s", cfg.ListenAddr)
	}
	if cfg.BodyLimit != 524288 {
		t.Errorf("expected 524288, got %d", cfg.BodyLimit)
	}
	if len(cfg.Mailers) != 0 {
		t.Errorf("expected no mailers, got %d", len(cfg.Mailers))
	}
}

// TestMailerRetryDefaultsStaySmall guards the deliberate change from the
// pre-monorepo 250 / 15s, which would have blocked one request for over an
// hour once the retry loop was actually wired up. Phorge's worker queue is the
// authoritative retry loop; this one only absorbs seconds. See
// docs/findings.md.
func TestMailerRetryDefaultsStaySmall(t *testing.T) {
	clearMailerEnv(t)

	cfg := LoadFromEnv()
	if cfg.MaxRetries != 2 {
		t.Errorf("expected 2 retries, got %d", cfg.MaxRetries)
	}
	if cfg.RetryWaitSec != 2 {
		t.Errorf("expected a 2s retry wait, got %ds", cfg.RetryWaitSec)
	}

	policy := cfg.RetryPolicy()
	if worst := time.Duration(cfg.MaxRetries) * policy.RetryWait; worst > 10*time.Second {
		t.Errorf("one adapter may block a request for %v, longer than the PHP client waits", worst)
	}
}

func TestMailerConfigFromEnv(t *testing.T) {
	clearMailerEnv(t)
	t.Setenv("GORGE_LISTEN_ADDR", ":9999")
	t.Setenv("GORGE_SERVICE_TOKEN", "tok123")
	t.Setenv("GORGE_MAILER_MAX_RETRIES", "5")
	t.Setenv("GORGE_MAILER_RETRY_WAIT", "7")
	t.Setenv("GORGE_MAILER_BODY_LIMIT", "1024")

	cfg := LoadFromEnv()
	if cfg.ListenAddr != ":9999" {
		t.Errorf("expected :9999, got %s", cfg.ListenAddr)
	}
	if cfg.ServiceToken != "tok123" {
		t.Errorf("expected tok123, got %s", cfg.ServiceToken)
	}
	if cfg.MaxRetries != 5 || cfg.RetryWaitSec != 7 {
		t.Errorf("expected 5 retries / 7s, got %d / %ds", cfg.MaxRetries, cfg.RetryWaitSec)
	}
	if cfg.BodyLimit != 1024 {
		t.Errorf("expected 1024, got %d", cfg.BodyLimit)
	}
}

// TestMailerConfigLegacyEnv covers the variables the pre-monorepo compose file
// sets. They must keep working until every deployment has been migrated.
func TestMailerConfigLegacyEnv(t *testing.T) {
	clearMailerEnv(t)
	t.Setenv("LISTEN_ADDR", ":8888")
	t.Setenv("SERVICE_TOKEN", "legacy-tok")
	t.Setenv("MAX_RETRIES", "9")
	t.Setenv("RETRY_WAIT", "11")
	t.Setenv("BODY_LIMIT", "2048")

	cfg := LoadFromEnv()
	if cfg.ListenAddr != ":8888" {
		t.Errorf("expected :8888, got %s", cfg.ListenAddr)
	}
	if cfg.ServiceToken != "legacy-tok" {
		t.Errorf("expected legacy-tok, got %s", cfg.ServiceToken)
	}
	if cfg.MaxRetries != 9 || cfg.RetryWaitSec != 11 {
		t.Errorf("expected 9 retries / 11s, got %d / %ds", cfg.MaxRetries, cfg.RetryWaitSec)
	}
	if cfg.BodyLimit != 2048 {
		t.Errorf("expected 2048, got %d", cfg.BodyLimit)
	}
}

func TestMailerConfigSMTPFromEnv(t *testing.T) {
	clearMailerEnv(t)
	t.Setenv("MAILER_TYPE", "smtp")
	t.Setenv("SMTP_HOST", "mail.example.com")
	t.Setenv("SMTP_PORT", "587")
	t.Setenv("SMTP_USER", "alice")
	t.Setenv("SMTP_PASSWORD", "secret")

	cfg := LoadFromEnv()
	if len(cfg.Mailers) != 1 {
		t.Fatalf("expected 1 mailer, got %d", len(cfg.Mailers))
	}
	m := cfg.Mailers[0]
	if m.Type != "smtp" {
		t.Errorf("expected smtp, got %s", m.Type)
	}
	if m.Key != "default" {
		t.Errorf("expected the default key, got %s", m.Key)
	}
	if m.Options["host"] != "mail.example.com" || m.Options["port"] != "587" {
		t.Errorf("unexpected options: %v", m.Options)
	}
}

// TestMailerConfigDropsTwilioOptions pins the removal of three option mappings
// that survived from a Twilio adapter this service never had. SMS stays with
// Phorge's own PhabricatorMailTwilioAdapter, so a deployment setting these gets
// nothing, and it should not look as though it might.
func TestMailerConfigDropsTwilioOptions(t *testing.T) {
	clearMailerEnv(t)
	t.Setenv("MAILER_TYPE", "smtp")
	t.Setenv("MAILER_FROM_NUMBER", "+15550100")
	t.Setenv("MAILER_ACCOUNT_SID", "AC123")
	t.Setenv("MAILER_AUTH_TOKEN", "secret")

	cfg := LoadFromEnv()
	if len(cfg.Mailers) != 1 {
		t.Fatalf("expected 1 mailer, got %d", len(cfg.Mailers))
	}
	for _, opt := range []string{"from-number", "account-sid", "auth-token"} {
		if _, ok := cfg.Mailers[0].Options[opt]; ok {
			t.Errorf("option %q should no longer be mapped from the environment", opt)
		}
	}
}

func TestMailerConfigJSONList(t *testing.T) {
	clearMailerEnv(t)
	t.Setenv("GORGE_MAILER_CONFIG",
		`[{"key":"sg","type":"sendgrid","priority":10,"options":{"api-key":"SG.xxx"}}]`)

	cfg := LoadFromEnv()
	if len(cfg.Mailers) != 1 {
		t.Fatalf("expected 1 mailer, got %d", len(cfg.Mailers))
	}
	m := cfg.Mailers[0]
	if m.Type != "sendgrid" || m.Priority != 10 || m.Options["api-key"] != "SG.xxx" {
		t.Errorf("unexpected mailer: %+v", m)
	}
}

// TestMailerConfigMalformedJSONLeavesNoMailers records what a typo in the JSON
// list costs: no backends, which /readyz reports as unavailable instead of the
// service accepting mail it cannot deliver.
func TestMailerConfigMalformedJSONLeavesNoMailers(t *testing.T) {
	clearMailerEnv(t)
	t.Setenv("MAILER_CONFIG", `[{"key":"sg","type":`)

	cfg := LoadFromEnv()
	if len(cfg.Mailers) != 0 {
		t.Fatalf("expected no mailers, got %d", len(cfg.Mailers))
	}

	d, err := NewDispatcher(cfg.Mailers, cfg.RetryPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if d.Ready() == nil {
		t.Error("a service with no backends must not report ready")
	}
}

func TestMailerConfigFromFile(t *testing.T) {
	clearMailerEnv(t)
	t.Setenv("GORGE_SERVICE_TOKEN", "from-env")

	content := `{"listenAddr":":7777","maxRetries":4,` +
		`"mailers":[{"key":"t","type":"test"}]}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":7777" {
		t.Errorf("expected :7777, got %s", cfg.ListenAddr)
	}
	if cfg.MaxRetries != 4 {
		t.Errorf("expected 4, got %d", cfg.MaxRetries)
	}
	if cfg.RetryWaitSec != DefaultRetryWaitSec {
		t.Errorf("a key absent from the file should keep its default, got %d", cfg.RetryWaitSec)
	}
	if len(cfg.Mailers) != 1 || cfg.Mailers[0].Type != "test" {
		t.Errorf("unexpected mailers: %+v", cfg.Mailers)
	}
	// Credentials are the point of this: a mailer config file otherwise holds
	// every backend secret in the deployment, so the service token stays in the
	// environment.
	if cfg.ServiceToken != "from-env" {
		t.Errorf("expected the token from the environment, got %s", cfg.ServiceToken)
	}
}

func TestMailerConfigFileNotFound(t *testing.T) {
	if _, err := LoadFromFile("/nonexistent/config.json"); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestMailerLoadPrefersConfigFile(t *testing.T) {
	clearMailerEnv(t)

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"listenAddr":":7777"}`), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAILER_CONFIG_FILE", path)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":7777" {
		t.Errorf("expected :7777 from the config file, got %s", cfg.ListenAddr)
	}
}
