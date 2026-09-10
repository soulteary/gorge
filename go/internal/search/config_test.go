package search

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/soulteary/gorge/go/internal/search/engine"
)

func TestDefaultsWithNothingSet(t *testing.T) {
	cfg := LoadFromEnv()

	// :8120 predates the monorepo and Phorge deployments already point at it.
	if cfg.ListenAddr != DefaultListenAddr {
		t.Errorf("expected %s, got %s", DefaultListenAddr, cfg.ListenAddr)
	}
	if cfg.ServiceToken != "" {
		t.Errorf("expected no token, got %q", cfg.ServiceToken)
	}
	// No backends is a supported state, not an error: /readyz is what reports
	// it, and it is how a deployment boots while its configuration is written.
	if len(cfg.Backends) != 0 {
		t.Errorf("expected no backends, got %v", cfg.Backends)
	}
}

func TestCanonicalServiceNamesAndRetiredAlias(t *testing.T) {
	t.Run("retired alias is ignored", func(t *testing.T) {
		t.Setenv("GORGE_LISTEN_ADDR", "")
		t.Setenv("LISTEN_ADDR", ":9000")
		if got := LoadFromEnv().ListenAddr; got != DefaultListenAddr {
			t.Errorf("retired LISTEN_ADDR must be ignored, got %s", got)
		}
	})
	t.Run("canonical name works", func(t *testing.T) {
		t.Setenv("GORGE_LISTEN_ADDR", ":9001")
		t.Setenv("LISTEN_ADDR", ":9000")
		if got := LoadFromEnv().ListenAddr; got != ":9001" {
			t.Errorf("expected canonical address :9001, got %s", got)
		}
	})
}

func TestBackendListFromEnv(t *testing.T) {
	t.Setenv("GORGE_SEARCH_BACKENDS",
		`[{"type":"elasticsearch","hosts":["es1:9200","es2:9200"],"index":"phorge","roles":["read","write"]}]`)

	cfg := LoadFromEnv()
	if len(cfg.Backends) != 1 {
		t.Fatalf("expected one backend, got %d", len(cfg.Backends))
	}
	b := cfg.Backends[0]
	if b.Type != "elasticsearch" || b.Index != "phorge" || len(b.Hosts) != 2 {
		t.Errorf("unexpected backend: %+v", b)
	}
}

func TestRetiredBackendListNameIsIgnored(t *testing.T) {
	t.Setenv("GORGE_SEARCH_BACKENDS", "")
	t.Setenv("SEARCH_BACKENDS", `[{"type":"meilisearch","hosts":["meili:7700"]}]`)

	cfg := LoadFromEnv()
	if len(cfg.Backends) != 0 {
		t.Fatalf("retired SEARCH_BACKENDS must be ignored, got %+v", cfg.Backends)
	}
}

// A malformed list leaves no backends behind, which /readyz reports as
// unavailable rather than letting the service accept searches it cannot run.
// It is not a startup failure, because "not configured yet" and "configured
// wrongly" are both states a deployment passes through; the difference is
// registered in docs/findings.md.
func TestAMalformedBackendListLeavesNoBackends(t *testing.T) {
	t.Setenv("GORGE_SEARCH_BACKENDS", `[{"type":`)

	cfg := LoadFromEnv()
	if len(cfg.Backends) != 0 {
		t.Errorf("expected no backends, got %v", cfg.Backends)
	}

	se, err := NewEngine(cfg.Backends)
	if err != nil {
		t.Fatal(err)
	}
	if se.Ready() == nil {
		t.Error("a service with no usable backend must not report itself ready")
	}
}

// The flat per-option variables keep their unprefixed names, the same way the
// mailer domain kept SMTP_HOST: they name a setting of the backend rather than
// of this service.
func TestSingleElasticsearchBackendFromFlatVariables(t *testing.T) {
	t.Setenv("ES_HOST", "es1:9200, es2:9200")
	t.Setenv("ES_INDEX", "phorge")
	t.Setenv("ES_VERSION", "7")
	t.Setenv("ES_PROTOCOL", "https")

	cfg := LoadFromEnv()
	if len(cfg.Backends) != 1 {
		t.Fatalf("expected one backend, got %d", len(cfg.Backends))
	}
	b := cfg.Backends[0]

	if b.Type != "elasticsearch" {
		t.Errorf("expected elasticsearch, got %q", b.Type)
	}
	if len(b.Hosts) != 2 || b.Hosts[0] != "es1:9200" || b.Hosts[1] != "es2:9200" {
		t.Errorf("unexpected hosts: %q", b.Hosts)
	}
	if b.Index != "phorge" || b.Version != 7 || b.Protocol != "https" {
		t.Errorf("unexpected backend: %+v", b)
	}
	if len(b.Roles) != 2 {
		t.Errorf("a single backend built from the flat variables must take both roles, got %v", b.Roles)
	}
}

func TestSingleMeilisearchBackendFromFlatVariables(t *testing.T) {
	t.Setenv("GORGE_SEARCH_ENGINE", "meilisearch")
	t.Setenv("MEILI_HOST", "meili:7700")
	t.Setenv("MEILI_MASTER_KEY", "secret")

	cfg := LoadFromEnv()
	if len(cfg.Backends) != 1 {
		t.Fatalf("expected one backend, got %d", len(cfg.Backends))
	}
	if cfg.Backends[0].Type != "meilisearch" || cfg.Backends[0].APIKey != "secret" {
		t.Errorf("unexpected backend: %+v", cfg.Backends[0])
	}
}

func TestNoHostMeansNoBackend(t *testing.T) {
	t.Setenv("ES_INDEX", "phorge")
	t.Setenv("ES_VERSION", "7")

	if got := LoadFromEnv().Backends; len(got) != 0 {
		t.Errorf("expected no backends without a host, got %v", got)
	}
}

func TestUnsetEngineMeansElasticsearch(t *testing.T) {
	t.Setenv("ES_HOST", "es:9200")

	cfg := LoadFromEnv()
	if len(cfg.Backends) != 1 || cfg.Backends[0].Type != "elasticsearch" {
		t.Fatalf("unexpected backends: %v", cfg.Backends)
	}
}

func TestLoadFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "search.json")
	body := `{"backends":[{"type":"elasticsearch","hosts":["es:9200"],"roles":["read"]}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GORGE_SERVICE_TOKEN", "from-env")

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServiceToken != "from-env" {
		t.Errorf("expected the token from the environment, got %q", cfg.ServiceToken)
	}
	if cfg.ListenAddr != DefaultListenAddr {
		t.Errorf("expected the default listen address, got %q", cfg.ListenAddr)
	}
	if len(cfg.Backends) != 1 {
		t.Fatalf("expected one backend, got %d", len(cfg.Backends))
	}
}

func TestLoadPrefersTheFileWhenOneIsPointedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "search.json")
	if err := os.WriteFile(path, []byte(`{"listenAddr":":9999"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GORGE_SEARCH_CONFIG_FILE", path)
	t.Setenv("GORGE_LISTEN_ADDR", ":8888")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":9999" {
		t.Errorf("the file wins over the environment, got %q", cfg.ListenAddr)
	}
}

func TestLoadReportsAMissingConfigFile(t *testing.T) {
	t.Setenv("GORGE_SEARCH_CONFIG_FILE", filepath.Join(t.TempDir(), "absent.json"))

	if _, err := Load(); err == nil {
		t.Error("expected an error for a missing config file")
	}
}

func TestRetiredConfigFileNameIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "search.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GORGE_SEARCH_CONFIG_FILE", "")
	t.Setenv("SEARCH_CONFIG_FILE", path)

	if got := ConfigFilePath(); got != "" {
		t.Errorf("retired SEARCH_CONFIG_FILE must be ignored, got %q", got)
	}
}

func TestNewEngineDispatchesOnType(t *testing.T) {
	se, err := NewEngine([]engine.BackendDef{
		{Type: "elasticsearch", Hosts: []string{"es:9200"}},
		{Type: "meilisearch", Hosts: []string{"meili:7700"}},
		{Type: "test"},
		{Type: "", Hosts: []string{"es:9200"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	types := make([]string, 0, 4)
	for _, info := range se.BackendInfo() {
		types = append(types, info["type"].(string))
	}
	want := []string{"elasticsearch", "meilisearch", "test", "elasticsearch"}
	for i := range want {
		if types[i] != want[i] {
			t.Errorf("backend %d: expected %s, got %s", i, want[i], types[i])
		}
	}
}
