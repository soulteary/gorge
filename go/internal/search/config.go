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

const DefaultListenAddr = ":8120"

type Config struct {
	config.Base
	Backends []engine.BackendDef `json:"backends"`
}

func Load() (*Config, error) {
	if path := ConfigFilePath(); path != "" {
		return LoadFromFile(path)
	}
	return LoadFromEnv(), nil
}

func ConfigFilePath() string {
	return config.EnvStr("", "GORGE_SEARCH_CONFIG_FILE")
}

// LoadFromEnv accepts canonical Gorge service variables. Backend-native ES_*
// and MEILI_* names intentionally remain unchanged because they describe the
// selected backend rather than a retired standalone process shell.
func LoadFromEnv() *Config {
	cfg := &Config{Base: config.LoadBase(DefaultListenAddr)}

	if raw := config.EnvStr("", "GORGE_SEARCH_BACKENDS"); raw != "" {
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

func LoadFromFile(path string) (*Config, error) {
	cfg := &Config{
		Base: config.Base{
			ListenAddr:   DefaultListenAddr,
			ServiceToken: config.EnvStr("", "GORGE_SERVICE_TOKEN"),
		},
	}
	if err := config.LoadJSONFile(path, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func defFromEnv() []engine.BackendDef {
	switch config.EnvStr("", "GORGE_SEARCH_ENGINE") {
	case "meilisearch":
		return meilisearchFromEnv()
	default:
		return elasticsearchFromEnv()
	}
}

func elasticsearchFromEnv() []engine.BackendDef {
	host := config.EnvStr("", "ES_HOST")
	if host == "" { return nil }
	hosts := strings.Split(host, ",")
	for i := range hosts { hosts[i] = strings.TrimSpace(hosts[i]) }
	return []engine.BackendDef{{
		Type: "elasticsearch",
		Hosts: hosts,
		Index: config.EnvStr(engine.DefaultIndexName, "ES_INDEX"),
		Version: config.EnvInt(5, "ES_VERSION"),
		Timeout: config.EnvInt(15, "ES_TIMEOUT"),
		Protocol: config.EnvStr("http", "ES_PROTOCOL"),
		Roles: []string{"read", "write"},
	}}
}

func meilisearchFromEnv() []engine.BackendDef {
	host := config.EnvStr("", "MEILI_HOST")
	if host == "" { return nil }
	return []engine.BackendDef{{
		Type: "meilisearch",
		Hosts: []string{host},
		Index: config.EnvStr(engine.DefaultIndexName, "MEILI_INDEX"),
		APIKey: config.EnvStr("", "MEILI_MASTER_KEY"),
		Timeout: config.EnvInt(15, "MEILI_TIMEOUT"),
		Protocol: config.EnvStr("http", "MEILI_PROTOCOL"),
		Roles: []string{"read", "write"},
	}}
}

func NewEngine(defs []engine.BackendDef) (*engine.SearchEngine, error) {
	backends := make([]engine.SearchBackend, 0, len(defs))
	for _, d := range defs {
		switch d.Type {
		case "meilisearch":
			backends = append(backends, meilisearch.New(d))
		case "test":
			b, err := engine.NewTestBackend(d)
			if err != nil { return nil, err }
			backends = append(backends, b)
		default:
			backends = append(backends, elasticsearch.New(d))
		}
	}
	return engine.New(backends), nil
}
