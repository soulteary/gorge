package render

import (
	"github.com/soulteary/gorge/go/internal/platform/config"
)

// Defaults for the render service. The listen address is highlight's original
// :8140; diff's :8130 is retired when it merges into this binary.
const (
	DefaultListenAddr = ":8140"
	DefaultMaxBytes   = 1048576
	DefaultTimeoutSec = 15
)

// Config is the render service configuration. The domain-specific limits carry
// a GORGE_RENDER_ prefix because bare MAX_BYTES / TIMEOUT_SEC would collide as
// soon as a second domain shares this process.
type Config struct {
	config.Base
	MaxBytes   int `json:"maxBytes"`
	TimeoutSec int `json:"timeoutSec"`
}

// Load picks the configuration source: a JSON file when one is pointed at,
// the environment otherwise.
func Load() (*Config, error) {
	if path := ConfigFilePath(); path != "" {
		return LoadFromFile(path)
	}
	return LoadFromEnv(), nil
}

// ConfigFilePath returns the JSON config file to read, or "" for env-only.
func ConfigFilePath() string {
	return config.EnvStr("", "GORGE_CONFIG_FILE", "HIGHLIGHT_CONFIG_FILE")
}

// LoadFromEnv reads the configuration from the environment.
func LoadFromEnv() *Config {
	return &Config{
		Base:       config.LoadBase(DefaultListenAddr),
		MaxBytes:   config.EnvInt(DefaultMaxBytes, "GORGE_RENDER_MAX_BYTES", "MAX_BYTES"),
		TimeoutSec: config.EnvInt(DefaultTimeoutSec, "GORGE_RENDER_TIMEOUT_SEC", "TIMEOUT_SEC"),
	}
}

// LoadFromFile reads the configuration from a JSON file. Keys absent from the
// file fall back to the defaults, except the service token, which is still
// taken from the environment so secrets can be injected separately.
func LoadFromFile(path string) (*Config, error) {
	cfg := &Config{
		Base: config.Base{
			ListenAddr:   DefaultListenAddr,
			ServiceToken: config.EnvStr("", "GORGE_SERVICE_TOKEN", "SERVICE_TOKEN"),
		},
		MaxBytes:   DefaultMaxBytes,
		TimeoutSec: DefaultTimeoutSec,
	}
	if err := config.LoadJSONFile(path, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
