package gitea

import "testing"

func TestConfigRequiresBridgeSecrets(t *testing.T) {
	cfg := &Config{}
	if cfg.Ready() == nil {
		t.Fatal("incomplete configuration must not be ready")
	}
	cfg.BaseURL = "https://git.example.com"
	cfg.WebhookSecret = "secret"
	cfg.ConduitURL = "http://conduit:8150"
	cfg.ConduitToken = "api-token"
	if err := cfg.Ready(); err != nil {
		t.Fatalf("complete configuration is not ready: %v", err)
	}
}

func TestConfigDefaults(t *testing.T) {
	for _, key := range []string{
		"GORGE_LISTEN_ADDR", "LISTEN_ADDR", "GORGE_SERVICE_TOKEN", "SERVICE_TOKEN",
		"GORGE_GITEA_BASE_URL", "GORGE_GITEA_WEBHOOK_SECRET", "GORGE_GITEA_CONDUIT_URL",
		"GORGE_GITEA_CONDUIT_TOKEN", "GORGE_GITEA_GATEWAY_TOKEN", "GORGE_GITEA_TIMEOUT_SEC",
	} {
		t.Setenv(key, "")
	}
	cfg := LoadFromEnv()
	if cfg.ListenAddr != ":8180" || cfg.TimeoutSec != 15 {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
}
