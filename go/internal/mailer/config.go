package mailer

import (
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/soulteary/gorge/go/internal/platform/config"
)

const (
	DefaultListenAddr   = ":8110"
	DefaultMaxRetries   = 2
	DefaultRetryWaitSec = 2
	DefaultBodyLimit    = 524288
	TransportBodyLimit  = "10M"
)

type MailerSpec struct {
	Key      string            `json:"key"`
	Type     string            `json:"type"`
	Priority int               `json:"priority,omitempty"`
	Options  map[string]string `json:"options,omitempty"`
}

type Config struct {
	config.Base
	Mailers      []MailerSpec `json:"mailers"`
	MaxRetries   int          `json:"maxRetries"`
	RetryWaitSec int          `json:"retryWaitSec"`
	BodyLimit    int          `json:"bodyLimit"`
}

func (c *Config) RetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxRetries: c.MaxRetries,
		RetryWait:  time.Duration(c.RetryWaitSec) * time.Second,
	}
}

func Load() (*Config, error) {
	if path := ConfigFilePath(); path != "" {
		return LoadFromFile(path)
	}
	return LoadFromEnv(), nil
}

func ConfigFilePath() string {
	return config.EnvStr("", "GORGE_MAILER_CONFIG_FILE")
}

// LoadFromEnv removes only standalone service aliases. Provider variables such
// as SMTP_HOST and MAILER_API_KEY remain part of the backend configuration
// contract and intentionally keep their provider-native names.
func LoadFromEnv() *Config {
	cfg := &Config{
		Base:         config.LoadBase(DefaultListenAddr),
		MaxRetries:   config.EnvInt(DefaultMaxRetries, "GORGE_MAILER_MAX_RETRIES"),
		RetryWaitSec: config.EnvInt(DefaultRetryWaitSec, "GORGE_MAILER_RETRY_WAIT"),
		BodyLimit:    config.EnvInt(DefaultBodyLimit, "GORGE_MAILER_BODY_LIMIT"),
	}

	if raw := config.EnvStr("", "GORGE_MAILER_CONFIG"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.Mailers); err != nil {
			slog.Error("GORGE_MAILER_CONFIG is not a valid mailer list", "error", err)
			cfg.Mailers = nil
		}
	}

	if cfg.Mailers == nil {
		cfg.Mailers = specFromEnv()
	}
	return cfg
}

func LoadFromFile(path string) (*Config, error) {
	cfg := &Config{
		Base: config.Base{
			ListenAddr:   DefaultListenAddr,
			ServiceToken: config.EnvStr("", "GORGE_SERVICE_TOKEN"),
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

// specFromEnv assembles one backend. Selection uses Gorge's canonical names;
// provider-native variables remain shared with deployment secrets.
func specFromEnv() []MailerSpec {
	t := config.EnvStr("", "GORGE_MAILER_TYPE")
	if t == "" {
		return nil
	}

	spec := MailerSpec{
		Key:     config.EnvStr("default", "GORGE_MAILER_KEY"),
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
