package notification

import (
	"os"
	"path/filepath"
	"testing"
)

func serverOf(t *testing.T, cfg *Config, kind string) ServerSpec {
	t.Helper()
	for _, spec := range cfg.Servers {
		if spec.Type == kind {
			return spec
		}
	}
	t.Fatalf("no %s server in %v", kind, cfg.Servers)
	return ServerSpec{}
}

func TestLoadFromEnvDefaults(t *testing.T) {
	cfg := LoadFromEnv()
	if len(cfg.Servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(cfg.Servers))
	}
	if got := serverOf(t, cfg, ServerKindClient).Port; got != DefaultClientPort {
		t.Errorf("client port = %d, want %d", got, DefaultClientPort)
	}
	if got := serverOf(t, cfg, ServerKindAdmin).Port; got != DefaultAdminPort {
		t.Errorf("admin port = %d, want %d", got, DefaultAdminPort)
	}
	if got := serverOf(t, cfg, ServerKindClient).Listen; got != DefaultListenAddr {
		t.Errorf("listen = %q, want %q", got, DefaultListenAddr)
	}
}

func TestLoadFromEnvCustom(t *testing.T) {
	t.Setenv("GORGE_NOTIFICATION_CLIENT_PORT", "9000")
	t.Setenv("GORGE_NOTIFICATION_ADMIN_PORT", "9001")
	t.Setenv("GORGE_NOTIFICATION_LISTEN_ADDR", "127.0.0.1")

	cfg := LoadFromEnv()
	if got := serverOf(t, cfg, ServerKindClient).Port; got != 9000 {
		t.Errorf("client port = %d, want 9000", got)
	}
	if got := serverOf(t, cfg, ServerKindAdmin).Port; got != 9001 {
		t.Errorf("admin port = %d, want 9001", got)
	}
	if got := serverOf(t, cfg, ServerKindAdmin).Listen; got != "127.0.0.1" {
		t.Errorf("listen = %q, want 127.0.0.1", got)
	}
}

func TestRetiredEnvNamesAreIgnored(t *testing.T) {
	t.Setenv("CLIENT_PORT", "9100")
	t.Setenv("ADMIN_PORT", "9101")
	t.Setenv("LISTEN_ADDR", "10.0.0.1")

	cfg := LoadFromEnv()
	if got := serverOf(t, cfg, ServerKindClient).Port; got != DefaultClientPort {
		t.Errorf("retired CLIENT_PORT changed client port to %d", got)
	}
	if got := serverOf(t, cfg, ServerKindAdmin).Port; got != DefaultAdminPort {
		t.Errorf("retired ADMIN_PORT changed admin port to %d", got)
	}
	if got := serverOf(t, cfg, ServerKindClient).Listen; got != DefaultListenAddr {
		t.Errorf("retired LISTEN_ADDR changed listen address to %q", got)
	}
}

func TestCanonicalNamesAreAuthoritative(t *testing.T) {
	t.Setenv("GORGE_NOTIFICATION_CLIENT_PORT", "9200")
	t.Setenv("CLIENT_PORT", "9100")
	t.Setenv("GORGE_NOTIFICATION_ADMIN_PORT", "9201")
	t.Setenv("ADMIN_PORT", "9101")
	t.Setenv("GORGE_NOTIFICATION_LISTEN_ADDR", "10.0.0.2")
	t.Setenv("LISTEN_ADDR", "10.0.0.1")

	cfg := LoadFromEnv()
	if got := serverOf(t, cfg, ServerKindClient).Port; got != 9200 {
		t.Errorf("client port = %d, want 9200", got)
	}
	if got := serverOf(t, cfg, ServerKindAdmin).Port; got != 9201 {
		t.Errorf("admin port = %d, want 9201", got)
	}
	if got := serverOf(t, cfg, ServerKindClient).Listen; got != "10.0.0.2" {
		t.Errorf("listen = %q, want 10.0.0.2", got)
	}
}

func TestServerSpecAddr(t *testing.T) {
	tests := []struct {
		spec ServerSpec
		want string
	}{
		{ServerSpec{Listen: "0.0.0.0", Port: 22280}, "0.0.0.0:22280"},
		{ServerSpec{Listen: "127.0.0.1", Port: 22281}, "127.0.0.1:22281"},
		{ServerSpec{Listen: "", Port: 22280}, ":22280"},
		{ServerSpec{Listen: "::1", Port: 22280}, "[::1]:22280"},
	}
	for _, tt := range tests {
		if got := tt.spec.Addr(); got != tt.want {
			t.Errorf("Addr() = %q, want %q", got, tt.want)
		}
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aphlict.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadFromFile(t *testing.T) {
	path := writeConfig(t, `{
		"servers": [
			{"type": "client", "port": 22280},
			{"type": "admin", "port": 22281, "listen": "127.0.0.1"}
		],
		"cluster": [
			{"host": "peer1.example.com", "port": 22281, "protocol": "http"}
		]
	}`)

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(cfg.Servers))
	}
	if cfg.Servers[0].Listen != DefaultListenAddr {
		t.Errorf("an entry without listen should default to %q, got %q", DefaultListenAddr, cfg.Servers[0].Listen)
	}
	if cfg.Servers[1].Listen != "127.0.0.1" {
		t.Errorf("admin listen = %q, want 127.0.0.1", cfg.Servers[1].Listen)
	}
	if len(cfg.Cluster) != 1 {
		t.Fatalf("expected 1 cluster peer, got %d", len(cfg.Cluster))
	}
	if cfg.Cluster[0].Host != "peer1.example.com" {
		t.Errorf("peer host = %q", cfg.Cluster[0].Host)
	}
}

func TestLoadPrefersTheFileOverTheEnvironment(t *testing.T) {
	path := writeConfig(t, `{"servers": [
		{"type": "client", "port": 1234},
		{"type": "admin", "port": 5678}
	]}`)
	t.Setenv("GORGE_NOTIFICATION_CONFIG_FILE", path)
	t.Setenv("GORGE_NOTIFICATION_CLIENT_PORT", "9000")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 2 {
		t.Fatalf("expected exactly the file's 2 servers, got %d", len(cfg.Servers))
	}
	if got := serverOf(t, cfg, ServerKindClient).Port; got != 1234 {
		t.Errorf("client port = %d, want 1234", got)
	}
}

func TestRetiredConfigFileNameIsIgnored(t *testing.T) {
	path := writeConfig(t, `{"servers": [
		{"type": "client", "port": 1234},
		{"type": "admin", "port": 5678}
	]}`)
	t.Setenv("APHLICT_CONFIG", path)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := serverOf(t, cfg, ServerKindAdmin).Port; got != DefaultAdminPort {
		t.Errorf("retired APHLICT_CONFIG changed admin port to %d", got)
	}
}

func TestLoadDefaultsWithoutAnyConfiguration(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := serverOf(t, cfg, ServerKindClient).Port; got != DefaultClientPort {
		t.Errorf("client port = %d, want %d", got, DefaultClientPort)
	}
}

func TestLoadReportsAMissingFile(t *testing.T) {
	t.Setenv("GORGE_NOTIFICATION_CONFIG_FILE", filepath.Join(t.TempDir(), "absent.json"))
	if _, err := Load(); err == nil {
		t.Error("expected an error for a config file that does not exist")
	}
}

func TestLoadRejectsIncompleteConfigurations(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"client only", `{"servers": [{"type": "client", "port": 22280}]}`},
		{"admin only", `{"servers": [{"type": "admin", "port": 22281}]}`},
		{"no servers at all", `{"cluster": []}`},
		{"unknown kind", `{"servers": [
			{"type": "client", "port": 22280},
			{"type": "admin", "port": 22281},
			{"type": "gossip", "port": 22282}
		]}`},
		{"unusable port", `{"servers": [
			{"type": "client", "port": 0},
			{"type": "admin", "port": 22281}
		]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GORGE_NOTIFICATION_CONFIG_FILE", writeConfig(t, tt.content))
			if _, err := Load(); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestLoadAcceptsExtraServersOfEachKind(t *testing.T) {
	t.Setenv("GORGE_NOTIFICATION_CONFIG_FILE", writeConfig(t, `{"servers": [
		{"type": "client", "port": 22280},
		{"type": "client", "port": 22283},
		{"type": "admin", "port": 22281}
	]}`))

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 3 {
		t.Errorf("expected 3 servers, got %d", len(cfg.Servers))
	}
}
