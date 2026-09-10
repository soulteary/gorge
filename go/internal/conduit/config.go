// Package conduit is the gateway domain: a reverse proxy in front of Phorge's
// Conduit API. Every call to `ANY /api/:method` passes shared-token auth and a
// per-IP token-bucket rate limiter before being relayed to the upstream
// Phorge PHP app at `{upstream}/api/{method}`.
package conduit

import (
	"strings"

	"github.com/soulteary/gorge/go/internal/platform/config"
)

const (
	DefaultListenAddr      = ":8150"
	DefaultUpstreamURL     = "http://phorge:80"
	DefaultProxyTimeoutSec = 30
	DefaultMaxBodySize     = "10M"
	DefaultRateLimitRPS    = 0
	DefaultRateLimitBurst  = 20
	DefaultRateLimitExempt = "conduit.ping,conduit.getcapabilities"
)

type Config struct {
	config.Base
	UpstreamURL     string   `json:"upstreamURL"`
	ProxyTimeoutSec int      `json:"proxyTimeoutSec"`
	MaxBodySize     string   `json:"maxBodySize"`
	RateLimitRPS    int      `json:"rateLimitRPS"`
	RateLimitBurst  int      `json:"rateLimitBurst"`
	RateLimitExempt []string `json:"rateLimitExempt"`
}

func Load() (*Config, error) {
	if path := ConfigFilePath(); path != "" {
		return LoadFromFile(path)
	}
	return LoadFromEnv(), nil
}

func ConfigFilePath() string {
	return config.EnvStr("", "GORGE_CONFIG_FILE")
}

// LoadFromEnv accepts only the monorepo GORGE_CONDUIT_* contract. The bare
// names belonged to the standalone service and were retired once Phorge and
// Gorge gained a versioned default deployment stack.
func LoadFromEnv() *Config {
	return &Config{
		Base:            config.LoadBase(DefaultListenAddr),
		UpstreamURL:     config.EnvStr(DefaultUpstreamURL, "GORGE_CONDUIT_UPSTREAM_URL"),
		ProxyTimeoutSec: config.EnvInt(DefaultProxyTimeoutSec, "GORGE_CONDUIT_PROXY_TIMEOUT_SEC"),
		MaxBodySize:     config.EnvStr(DefaultMaxBodySize, "GORGE_CONDUIT_MAX_BODY_SIZE"),
		RateLimitRPS:    config.EnvInt(DefaultRateLimitRPS, "GORGE_CONDUIT_RATE_LIMIT_RPS"),
		RateLimitBurst:  config.EnvInt(DefaultRateLimitBurst, "GORGE_CONDUIT_RATE_LIMIT_BURST"),
		RateLimitExempt: splitCSV(config.EnvStr(DefaultRateLimitExempt, "GORGE_CONDUIT_RATE_LIMIT_EXEMPT")),
	}
}

func LoadFromFile(path string) (*Config, error) {
	cfg := &Config{
		Base: config.Base{
			ListenAddr:   DefaultListenAddr,
			ServiceToken: config.EnvStr("", "GORGE_SERVICE_TOKEN"),
		},
		UpstreamURL:     DefaultUpstreamURL,
		ProxyTimeoutSec: DefaultProxyTimeoutSec,
		MaxBodySize:     DefaultMaxBodySize,
		RateLimitRPS:    DefaultRateLimitRPS,
		RateLimitBurst:  DefaultRateLimitBurst,
		RateLimitExempt: splitCSV(DefaultRateLimitExempt),
	}
	if err := config.LoadJSONFile(path, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
