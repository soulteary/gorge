package search

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/soulteary/gorge/go/internal/platform/config"
	"github.com/soulteary/gorge/go/internal/search/engine"
	"github.com/soulteary/gorge/go/internal/search/engine/elasticsearch"
	"github.com/soulteary/gorge/go/internal/search/engine/meilisearch"
)

// DefaultListenAddr is the pre-monorepo :8120, which Phorge deployments
// already point at.
const DefaultListenAddr = ":8120"

// Config is the search service configuration.
type Config struct {
	config.Base
	Backends []engine.BackendDef `json:"backends"`
}

// Load picks the configuration source: a JSON file when one is pointed at, the
// environment otherwise.
func Load() (*Config, error) {
	if path := ConfigFilePath(); path != "" {
		return LoadFromFile(path)
	}
	return LoadFromEnv(), nil
}

// ConfigFilePath returns the JSON config file to read, or "" for env-only.
func ConfigFilePath() string {
	return config.EnvStr("", "GORGE_SEARCH_CONFIG_FILE", "SEARCH_CONFIG_FILE")
}

// LoadFromEnv reads the configuration from the environment.
//
// Only service-level and domain-level names carry the GORGE_SEARCH_ prefix.
// The per-backend option variables below (ES_*, MEILI_*) keep their flat
// names, the same way the mailer domain kept SMTP_HOST: they name a setting of
// the backend rather than of this service, and renaming them would break every
// existing deployment for no gain.
func LoadFromEnv() *Config {
	cfg := &Config{Base: config.LoadBase(DefaultListenAddr)}

	if raw := config.EnvStr("", "GORGE_SEARCH_BACKENDS", "SEARCH_BACKENDS"); raw != "" {
		// A malformed list leaves no backends behind, which /readyz reports as
		// unavailable rather than letting the service accept searches it
		// cannot run. Log it anyway: "unavailable" alone does not say the JSON
		// was the problem. See docs/findings.md.
		if err := json.Unmarshal([]byte(raw), &cfg.Backends); err != nil {
			slog.Error("GORGE_SEARCH_BACKENDS is not a valid backend list", "error", err)
			cfg.Backends = nil
		}
	}

	if cfg.Backends == nil {
		cfg.Backends = defFromEnv()
	}

	return cfg
}

// LoadFromFile reads the configuration from a JSON file. Keys absent from the
// file keep their defaults, except the service token, which is still taken
// from the environment so a secret can be injected separately from the file
// that holds the backend addresses.
func LoadFromFile(path string) (*Config, error) {
	cfg := &Config{
		Base: config.Base{
			ListenAddr:   DefaultListenAddr,
			ServiceToken: config.EnvStr("", "GORGE_SERVICE_TOKEN", "SERVICE_TOKEN"),
		},
	}
	if err := config.LoadJSONFile(path, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// defFromEnv assembles a single backend from the flat per-option variables, so
// the common one-backend deployment needs no JSON at all.
//
// ES_HOST or MEILI_HOST is what turns it on; without one the service starts
// with no backends and reports itself not ready.
func defFromEnv() []engine.BackendDef {
	switch config.EnvStr("", "GORGE_SEARCH_ENGINE", "SEARCH_ENGINE") {
	case "meilisearch":
		return meilisearchFromEnv()
	default:
		return elasticsearchFromEnv()
	}
}

func elasticsearchFromEnv() []engine.BackendDef {
	host := config.EnvStr("", "ES_HOST")
	if host == "" {
		return nil
	}

	// Comma-separated, because an Elasticsearch cluster is several hosts and
	// this backend keeps a health table across them.
	hosts := strings.Split(host, ",")
	for i := range hosts {
		hosts[i] = strings.TrimSpace(hosts[i])
	}

	return []engine.BackendDef{{
		Type:     "elasticsearch",
		Hosts:    hosts,
		Index:    config.EnvStr(engine.DefaultIndexName, "ES_INDEX"),
		Version:  config.EnvInt(5, "ES_VERSION"),
		Timeout:  config.EnvInt(15, "ES_TIMEOUT"),
		Protocol: config.EnvStr("http", "ES_PROTOCOL"),
		Roles:    []string{"read", "write"},
	}}
}

func meilisearchFromEnv() []engine.BackendDef {
	host := config.EnvStr("", "MEILI_HOST")
	if host == "" {
		return nil
	}

	return []engine.BackendDef{{
		Type:     "meilisearch",
		Hosts:    []string{host},
		Index:    config.EnvStr(engine.DefaultIndexName, "MEILI_INDEX"),
		APIKey:   config.EnvStr("", "MEILI_MASTER_KEY"),
		Timeout:  config.EnvInt(15, "MEILI_TIMEOUT"),
		Protocol: config.EnvStr("http", "MEILI_PROTOCOL"),
		Roles:    []string{"read", "write"},
	}}
}

// NewEngine builds every backend the definitions name.
//
// A definition that cannot be built fails the whole call rather than being
// skipped, mirroring the mailer dispatcher: a backend silently missing from
// the rotation is how an install ends up searching one index and writing to
// another without noticing.
//
// An unknown type is Elasticsearch, not an error. That is the pre-monorepo
// behaviour and it is kept because the type field is written by Phorge's
// cluster.search entry, where "elasticsearch" is what an empty value means.
func NewEngine(defs []engine.BackendDef) (*engine.SearchEngine, error) {
	backends := make([]engine.SearchBackend, 0, len(defs))
	for _, d := range defs {
		switch d.Type {
		case "meilisearch":
			backends = append(backends, meilisearch.New(d))
		case "test":
			b, err := engine.NewTestBackend(d)
			if err != nil {
				return nil, err
			}
			backends = append(backends, b)
		default:
			backends = append(backends, elasticsearch.New(d))
		}
	}
	return engine.New(backends), nil
}
