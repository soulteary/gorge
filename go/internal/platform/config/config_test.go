package config

import (
	"os"
	"path/filepath"
	"testing"
)

func clearEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		t.Setenv(key, "")
	}
}

func TestLoadFromEnvDefaults(t *testing.T) {
	clearEnv(t, "GORGE_LISTEN_ADDR", "LISTEN_ADDR", "GORGE_SERVICE_TOKEN", "SERVICE_TOKEN")
	base := LoadBase(":8080")
	if base.ListenAddr != ":8080" {
		t.Errorf("expected :8080, got %s", base.ListenAddr)
	}
	if base.ServiceToken != "" {
		t.Errorf("expected empty token, got %s", base.ServiceToken)
	}
	if got := EnvStr("fallback", "GORGE_UNSET_STR"); got != "fallback" {
		t.Errorf("expected fallback, got %s", got)
	}
	if got := EnvInt(42, "GORGE_UNSET_INT"); got != 42 {
		t.Errorf("expected 42, got %d", got)
	}
	if got := EnvBool(true, "GORGE_UNSET_BOOL"); !got {
		t.Error("expected true")
	}
}

func TestLoadFromEnvOverride(t *testing.T) {
	t.Setenv("GORGE_LISTEN_ADDR", ":9999")
	t.Setenv("GORGE_SERVICE_TOKEN", "tok123")
	t.Setenv("GORGE_TEST_MAX_BYTES", "2048")
	t.Setenv("GORGE_TEST_ENABLED", "true")
	base := LoadBase(":8080")
	if base.ListenAddr != ":9999" {
		t.Errorf("expected :9999, got %s", base.ListenAddr)
	}
	if base.ServiceToken != "tok123" {
		t.Errorf("expected tok123, got %s", base.ServiceToken)
	}
	if got := EnvInt(1048576, "GORGE_TEST_MAX_BYTES"); got != 2048 {
		t.Errorf("expected 2048, got %d", got)
	}
	if got := EnvBool(false, "GORGE_TEST_ENABLED"); !got {
		t.Error("expected true")
	}
}

func TestLoadBaseIgnoresRetiredAliases(t *testing.T) {
	clearEnv(t, "GORGE_LISTEN_ADDR", "GORGE_SERVICE_TOKEN")
	t.Setenv("LISTEN_ADDR", ":7002")
	t.Setenv("SERVICE_TOKEN", "legacy-token")
	base := LoadBase(":8140")
	if base.ListenAddr != ":8140" {
		t.Errorf("retired LISTEN_ADDR must be ignored, got %s", base.ListenAddr)
	}
	if base.ServiceToken != "" {
		t.Errorf("retired SERVICE_TOKEN must be ignored, got %s", base.ServiceToken)
	}
}

func TestLoadFromFile(t *testing.T) {
	type serviceConfig struct {
		Base
		MaxBytes int `json:"maxBytes"`
	}
	content := `{"listenAddr":":7777","maxBytes":512}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := &serviceConfig{Base: Base{ListenAddr: ":8140", ServiceToken: "preset"}, MaxBytes: 1048576}
	if err := LoadJSONFile(path, cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":7777" {
		t.Errorf("expected :7777, got %s", cfg.ListenAddr)
	}
	if cfg.MaxBytes != 512 {
		t.Errorf("expected 512, got %d", cfg.MaxBytes)
	}
	if cfg.ServiceToken != "preset" {
		t.Errorf("keys absent from file should keep preset value, got %s", cfg.ServiceToken)
	}
}

func TestLoadFromFileNotFound(t *testing.T) {
	var cfg Base
	if err := LoadJSONFile("/nonexistent/config.json", &cfg); err == nil {
		t.Error("expected error for missing file")
	}
}
