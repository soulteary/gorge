// Package conduit is the gateway domain: a reverse proxy in front of Phorge's
// Conduit API. Every call to `ANY /api/:method` passes shared-token auth and a
// per-IP token-bucket rate limiter before being relayed to the upstream
// Phorge PHP app at `{upstream}/api/{method}`.
//
// It is the only domain whose consumers are other Go services rather than
// Phorge itself, and the only one that speaks the Conduit protocol envelope
// `{result, error_code, error_info}` instead of the platform {data, error};
// see internal/contracts/conduit.go and docs/modules/conduit.md.
package conduit

import (
	"strings"

	"github.com/soulteary/gorge/go/internal/platform/config"
)

// Defaults for the gateway. The listen address is conduit's original :8150,
// carried over unchanged from the pre-monorepo deployment.
const (
	DefaultListenAddr      = ":8150"
	DefaultUpstreamURL     = "http://phorge:80"
	DefaultProxyTimeoutSec = 30
	DefaultMaxBodySize     = "10M"
	DefaultRateLimitRPS    = 0 // disabled: a gateway with no limit configured relays everything
	DefaultRateLimitBurst  = 20
	// DefaultRateLimitExempt keeps the two cheap discovery calls out of the
	// limiter: Phorge and arcanist poll conduit.ping for health and fetch
	// conduit.getcapabilities on connect, and rate-limiting those would make a
	// healthy gateway look down.
	DefaultRateLimitExempt = "conduit.ping,conduit.getcapabilities"
)

// Config is the gateway configuration. The domain-specific settings carry a
// GORGE_CONDUIT_ prefix so they never collide with another domain sharing the
// process, while the bare legacy names (UPSTREAM_URL, RATE_LIMIT_RPS, ...) stay
// as fallbacks so pre-monorepo compose files keep working unchanged.
type Config struct {
	config.Base
	// UpstreamURL is the Phorge PHP app the gateway relays to, e.g.
	// http://phorge:80. A trailing slash is trimmed at proxy construction.
	UpstreamURL string `json:"upstreamURL"`
	// ProxyTimeoutSec bounds a single upstream request.
	ProxyTimeoutSec int `json:"proxyTimeoutSec"`
	// MaxBodySize is the request body limit, spelled the way httpx parses it
	// (e.g. "10M"). It is handed to httpx.Config.BodyLimit.
	MaxBodySize string `json:"maxBodySize"`
	// RateLimitRPS is the per-IP refill rate; 0 disables the limiter entirely.
	RateLimitRPS int `json:"rateLimitRPS"`
	// RateLimitBurst is the per-IP bucket size.
	RateLimitBurst int `json:"rateLimitBurst"`
	// RateLimitExempt are the Conduit methods that bypass the limiter.
	RateLimitExempt []string `json:"rateLimitExempt"`
}

// Load picks the configuration source: a JSON file when one is pointed at, the
// environment otherwise. It mirrors render.Load's branch.
func Load() (*Config, error) {
	if path := ConfigFilePath(); path != "" {
		return LoadFromFile(path)
	}
	return LoadFromEnv(), nil
}

// ConfigFilePath returns the JSON config file to read, or "" for env-only.
func ConfigFilePath() string {
	return config.EnvStr("", "GORGE_CONFIG_FILE", "CONDUIT_CONFIG_FILE")
}

// LoadFromEnv reads the configuration from the environment.
func LoadFromEnv() *Config {
	return &Config{
		Base:            config.LoadBase(DefaultListenAddr),
		UpstreamURL:     config.EnvStr(DefaultUpstreamURL, "GORGE_CONDUIT_UPSTREAM_URL", "UPSTREAM_URL"),
		ProxyTimeoutSec: config.EnvInt(DefaultProxyTimeoutSec, "GORGE_CONDUIT_PROXY_TIMEOUT_SEC", "PROXY_TIMEOUT_SEC"),
		MaxBodySize:     config.EnvStr(DefaultMaxBodySize, "GORGE_CONDUIT_MAX_BODY_SIZE", "MAX_BODY_SIZE"),
		RateLimitRPS:    config.EnvInt(DefaultRateLimitRPS, "GORGE_CONDUIT_RATE_LIMIT_RPS", "RATE_LIMIT_RPS"),
		RateLimitBurst:  config.EnvInt(DefaultRateLimitBurst, "GORGE_CONDUIT_RATE_LIMIT_BURST", "RATE_LIMIT_BURST"),
		RateLimitExempt: splitCSV(config.EnvStr(DefaultRateLimitExempt, "GORGE_CONDUIT_RATE_LIMIT_EXEMPT", "RATE_LIMIT_EXEMPT")),
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

// splitCSV parses a comma-separated list, trimming blanks. An empty string
// yields nil rather than a one-element slice holding "".
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
