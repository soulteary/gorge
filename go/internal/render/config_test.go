package render

import (
	"os"
	"path/filepath"
	"testing"
)

var renderEnvKeys = []string{
	"GORGE_LISTEN_ADDR", "LISTEN_ADDR",
	"GORGE_SERVICE_TOKEN", "SERVICE_TOKEN",
	"GORGE_CONFIG_FILE", "HIGHLIGHT_CONFIG_FILE",
	"GORGE_RENDER_MAX_BYTES", "MAX_BYTES",
	"GORGE_RENDER_TIMEOUT_SEC", "TIMEOUT_SEC",
	"GORGE_RENDER_ENABLE_DIFF",
}

func clearRenderEnv(t *testing.T) {
	t.Helper()
	for _, key := range renderEnvKeys {
		t.Setenv(key, "")
	}
}

func TestRenderConfigDefaults(t *testing.T) {
	clearRenderEnv(t)

	cfg := LoadFromEnv()
	if cfg.ListenAddr != ":8140" {
		t.Errorf("expected :8140, got %s", cfg.ListenAddr)
	}
	if cfg.MaxBytes != 1048576 {
		t.Errorf("expected 1048576, got %d", cfg.MaxBytes)
	}
	if cfg.TimeoutSec != 15 {
		t.Errorf("expected 15, got %d", cfg.TimeoutSec)
	}
	if cfg.ServiceToken != "" {
		t.Errorf("expected an empty token, got %s", cfg.ServiceToken)
	}
	if !cfg.EnableDiff {
		t.Error("expected diff routes enabled by default")
	}
}

func TestRenderConfigFromEnv(t *testing.T) {
	clearRenderEnv(t)
	t.Setenv("GORGE_LISTEN_ADDR", ":9999")
	t.Setenv("GORGE_SERVICE_TOKEN", "tok123")
	t.Setenv("GORGE_RENDER_MAX_BYTES", "2048")
	t.Setenv("GORGE_RENDER_TIMEOUT_SEC", "30")
	t.Setenv("GORGE_RENDER_ENABLE_DIFF", "false")

	cfg := LoadFromEnv()
	if cfg.ListenAddr != ":9999" {
		t.Errorf("expected :9999, got %s", cfg.ListenAddr)
	}
	if cfg.ServiceToken != "tok123" {
		t.Errorf("expected tok123, got %s", cfg.ServiceToken)
	}
	if cfg.MaxBytes != 2048 {
		t.Errorf("expected 2048, got %d", cfg.MaxBytes)
	}
	if cfg.TimeoutSec != 30 {
		t.Errorf("expected 30, got %d", cfg.TimeoutSec)
	}
	if cfg.EnableDiff {
		t.Error("expected diff routes disabled")
	}
}

func TestRenderConfigIgnoresRetiredEnv(t *testing.T) {
	clearRenderEnv(t)
	t.Setenv("LISTEN_ADDR", ":8888")
	t.Setenv("SERVICE_TOKEN", "retired-tok")
	t.Setenv("MAX_BYTES", "4096")
	t.Setenv("TIMEOUT_SEC", "45")

	cfg := LoadFromEnv()
	if cfg.ListenAddr != DefaultListenAddr {
		t.Errorf("retired LISTEN_ADDR changed config: %s", cfg.ListenAddr)
	}
	if cfg.ServiceToken != "" {
		t.Errorf("retired SERVICE_TOKEN changed config: %s", cfg.ServiceToken)
	}
	if cfg.MaxBytes != DefaultMaxBytes {
		t.Errorf("retired MAX_BYTES changed config: %d", cfg.MaxBytes)
	}
	if cfg.TimeoutSec != DefaultTimeoutSec {
		t.Errorf("retired TIMEOUT_SEC changed config: %d", cfg.TimeoutSec)
	}
}

func TestRenderConfigFromFile(t *testing.T) {
	clearRenderEnv(t)

	content := `{"listenAddr":":7777","maxBytes":512,"timeoutSec":5,"enableDiff":false}`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
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
	if cfg.MaxBytes != 512 {
		t.Errorf("expected 512, got %d", cfg.MaxBytes)
	}
	if cfg.TimeoutSec != 5 {
		t.Errorf("expected 5, got %d", cfg.TimeoutSec)
	}
	if cfg.EnableDiff {
		t.Error("expected diff routes disabled")
	}
}

func TestRenderConfigFileNotFound(t *testing.T) {
	if _, err := LoadFromFile("/nonexistent/config.json"); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestRenderLoadPrefersConfigFile(t *testing.T) {
	clearRenderEnv(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"listenAddr":":7777"}`), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GORGE_CONFIG_FILE", path)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":7777" {
		t.Errorf("expected :7777 from the config file, got %s", cfg.ListenAddr)
	}
}
