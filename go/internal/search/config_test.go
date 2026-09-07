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

// New names win, old ones still work. The unprefixed names are what the
// pre-monorepo compose files set, and dropping them would break every existing
// deployment on upgrade.
func TestNewNamesWinAndOldOnesStillWork(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"legacy only", map[string]string{"LISTEN_ADDR": ":9000"}, ":9000"},
		{"new only", map[string]string{"GORGE_LISTEN_ADDR": ":9001"}, ":9001"},
		{"new wins", map[string]string{
			"GORGE_LISTEN_ADDR": ":9001",
			"LISTEN_ADDR":       ":9000",
		}, ":9001"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := LoadFromEnv().ListenAddr; got != tc.want {
				t.Errorf("expected %s, got %s", tc.want, got)
			}
		})
	}
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

func TestLegacyBackendListName(t *testing.T) {
	t.Setenv("SEARCH_BACKENDS", `[{"type":"meilisearch","hosts":["meili:7700"]}]`)

	cfg := LoadFromEnv()
	if len(cfg.Backends) != 1 || cfg.Backends[0].Type != "meilisearch" {
		t.Fatalf("the legacy name must still be read, got %+v", cfg.Backends)
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
	// Comma-separated, and the whitespace around an entry is trimmed: a host
	// list copied out of a document otherwise yields a hostname with a leading
	// space, which resolves to nothing.
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

// ES_HOST is what turns the flat form on. Without it the service starts with
// no backends and reports itself not ready, rather than inventing a localhost
// default that would look configured and never answer.
func TestNoHostMeansNoBackend(t *testing.T) {
	t.Setenv("ES_INDEX", "phorge")
	t.Setenv("ES_VERSION", "7")

	if got := LoadFromEnv().Backends; len(got) != 0 {
		t.Errorf("expected no backends without a host, got %v", got)
	}
}

// An unset engine name means Elasticsearch, which is also what an empty type
// in Phorge's cluster.search entry means.
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

	// The token comes from the environment even when a file is in play, so a
	// secret can be injected separately from the file that holds the backend
	// addresses.
	t.Setenv("GORGE_SERVICE_TOKEN", "from-env")

	cfg, err := LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServiceToken != "from-env" {
		t.Errorf("expected the token from the environment, got %q", cfg.ServiceToken)
	}
	// A key the file does not mention keeps its default rather than becoming
	// the zero value.
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

	// A config file that was named and cannot be read is a startup failure,
	// unlike a backend list that was never written: the operator said where
	// the configuration is and it is not there.
	if _, err := Load(); err == nil {
		t.Error("expected an error for a missing config file")
	}
}

func TestLegacyConfigFileName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "search.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SEARCH_CONFIG_FILE", path)

	if ConfigFilePath() != path {
		t.Errorf("the legacy config file name must still be read, got %q", ConfigFilePath())
	}
}

func TestNewEngineDispatchesOnType(t *testing.T) {
	se, err := NewEngine([]engine.BackendDef{
		{Type: "elasticsearch", Hosts: []string{"es:9200"}},
		{Type: "meilisearch", Hosts: []string{"meili:7700"}},
		{Type: "test"},
		// An unknown type is Elasticsearch rather than an error: that is the
		// pre-monorepo behaviour, and an empty type in Phorge's cluster.search
		// entry means the same thing.
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
