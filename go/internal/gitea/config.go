// Package gitea receives signed Gitea events and appends references to linked
// Maniphest tasks. It deliberately owns no identity mapping: Stargate remains
// the single authority for users, while this service writes as one Conduit bot.
package gitea

import (
	"errors"
	"strings"

	"github.com/soulteary/gorge/go/internal/platform/config"
)

const (
	DefaultListenAddr = ":8180"
	DefaultTimeoutSec = 15
)

type Config struct {
	config.Base
	BaseURL       string
	WebhookSecret string
	ConduitURL    string
	ConduitToken  string
	GatewayToken  string
	TimeoutSec    int
}

func LoadFromEnv() *Config {
	return &Config{
		Base:          config.LoadBase(DefaultListenAddr),
		BaseURL:       strings.TrimRight(config.EnvStr("", "GORGE_GITEA_BASE_URL"), "/"),
		WebhookSecret: config.EnvStr("", "GORGE_GITEA_WEBHOOK_SECRET"),
		ConduitURL:    strings.TrimRight(config.EnvStr("", "GORGE_GITEA_CONDUIT_URL"), "/"),
		ConduitToken:  config.EnvStr("", "GORGE_GITEA_CONDUIT_TOKEN"),
		GatewayToken:  config.EnvStr("", "GORGE_GITEA_GATEWAY_TOKEN"),
		TimeoutSec:    config.EnvInt(DefaultTimeoutSec, "GORGE_GITEA_TIMEOUT_SEC"),
	}
}

func (c *Config) Ready() error {
	if c.BaseURL == "" || c.WebhookSecret == "" || c.ConduitURL == "" || c.ConduitToken == "" {
		return errors.New("gitea bridge configuration is incomplete")
	}
	return nil
}
