package mailer

import (
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/soulteary/gorge/go/internal/platform/config"
)

// Defaults for the mailer service. The listen address is the pre-monorepo
// :8110, which Phorge deployments already point at.
const (
	DefaultListenAddr = ":8110"

	// DefaultMaxRetries and DefaultRetryWaitSec cover a single adapter, and
	// they are deliberately far smaller than the 250 / 15s this service shipped
	// with before the retry loop was wired up at all. Those values would have
	// blocked one HTTP request for over an hour, while the PHP client waits 30
	// seconds and Phorge's worker queue is the real retry loop. See
	// docs/findings.md.
	DefaultMaxRetries   = 2
	DefaultRetryWaitSec = 2

	// DefaultBodyLimit caps the text and HTML bodies of a single message. It is
	// unrelated to TransportBodyLimit below: this one truncates, that one
	// rejects.
	DefaultBodyLimit = 524288

	// TransportBodyLimit overrides the platform's 2M default, which is not
	// enough for a message carrying attachments — those arrive base64-encoded
	// inside the JSON body, so they cost about a third more than their own
	// size.
	TransportBodyLimit = "10M"
)

// MailerSpec is one configured backend. Options are backend-specific and are
// documented per adapter in docs/modules/mailer.md.
//
// Priority orders the failover chain, highest first. A spec that omits it is
// tried after every spec that sets one.
type MailerSpec struct {
	Key      string            `json:"key"`
	Type     string            `json:"type"`
	Priority int               `json:"priority,omitempty"`
	Options  map[string]string `json:"options,omitempty"`
}

// Config is the mailer service configuration.
type Config struct {
	config.Base
	Mailers      []MailerSpec `json:"mailers"`
	MaxRetries   int          `json:"maxRetries"`
	RetryWaitSec int          `json:"retryWaitSec"`
	BodyLimit    int          `json:"bodyLimit"`
}

// RetryPolicy renders the retry settings in the form the dispatcher takes.
func (c *Config) RetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxRetries: c.MaxRetries,
		RetryWait:  time.Duration(c.RetryWaitSec) * time.Second,
	}
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
	return config.EnvStr("", "GORGE_MAILER_CONFIG_FILE", "MAILER_CONFIG_FILE")
}

// LoadFromEnv reads the configuration from the environment.
func LoadFromEnv() *Config {
	cfg := &Config{
		Base:         config.LoadBase(DefaultListenAddr),
		MaxRetries:   config.EnvInt(DefaultMaxRetries, "GORGE_MAILER_MAX_RETRIES", "MAX_RETRIES"),
		RetryWaitSec: config.EnvInt(DefaultRetryWaitSec, "GORGE_MAILER_RETRY_WAIT", "RETRY_WAIT"),
		BodyLimit:    config.EnvInt(DefaultBodyLimit, "GORGE_MAILER_BODY_LIMIT", "BODY_LIMIT"),
	}

	if raw := config.EnvStr("", "GORGE_MAILER_CONFIG", "MAILER_CONFIG"); raw != "" {
		// A malformed list leaves no backends behind, which /readyz now reports
		// as unavailable rather than letting the service accept mail it cannot
		// deliver. Log it anyway: "unavailable" alone does not say the JSON was
		// the problem.
		if err := json.Unmarshal([]byte(raw), &cfg.Mailers); err != nil {
			slog.Error("MAILER_CONFIG is not a valid mailer list", "error", err)
			cfg.Mailers = nil
		}
	}

	if cfg.Mailers == nil {
		cfg.Mailers = specFromEnv()
	}

	return cfg
}

// LoadFromFile reads the configuration from a JSON file. Keys absent from the
// file fall back to the defaults, except the service token, which is still
// taken from the environment so secrets can be injected separately — and this
// matters more here than elsewhere, since a mailer config file otherwise holds
// every backend credential in the deployment.
func LoadFromFile(path string) (*Config, error) {
	cfg := &Config{
		Base: config.Base{
			ListenAddr:   DefaultListenAddr,
			ServiceToken: config.EnvStr("", "GORGE_SERVICE_TOKEN", "SERVICE_TOKEN"),
		},
		MaxRetries:   DefaultMaxRetries,
		RetryWaitSec: DefaultRetryWaitSec,
		BodyLimit:    DefaultBodyLimit,
	}
	if err := config.LoadJSONFile(path, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// specFromEnv assembles a single backend from the flat per-option variables, so
// a one-backend deployment needs no JSON at all. MAILER_TYPE is what turns it
// on; without it the service starts with no backends and reports itself not
// ready.
//
// The option names are the backend's, so one variable can feed several
// backends; only the ones a given type reads have any effect.
func specFromEnv() []MailerSpec {
	t := config.EnvStr("", "GORGE_MAILER_TYPE", "MAILER_TYPE")
	if t == "" {
		return nil
	}

	spec := MailerSpec{
		Key:     config.EnvStr("default", "GORGE_MAILER_KEY", "MAILER_KEY"),
		Type:    t,
		Options: make(map[string]string),
	}
	for _, kv := range [][2]string{
		{"host", "SMTP_HOST"},
		{"port", "SMTP_PORT"},
		{"user", "SMTP_USER"},
		{"password", "SMTP_PASSWORD"},
		{"protocol", "SMTP_PROTOCOL"},
		{"access-key", "MAILER_ACCESS_KEY"},
		{"secret-key", "MAILER_SECRET_KEY"},
		{"region", "MAILER_REGION"},
		{"endpoint", "MAILER_ENDPOINT"},
		{"api-key", "MAILER_API_KEY"},
		{"domain", "MAILER_DOMAIN"},
		{"api-hostname", "MAILER_API_HOSTNAME"},
		{"access-token", "MAILER_ACCESS_TOKEN"},
	} {
		if v := os.Getenv(kv[1]); v != "" {
			spec.Options[kv[0]] = v
		}
	}
	return []MailerSpec{spec}
}
