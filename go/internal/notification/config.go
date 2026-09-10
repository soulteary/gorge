package notification

import (
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/soulteary/gorge/go/internal/platform/config"
)

// Defaults for the notification service. The two ports are Aphlict's wire
// contract and stay unchanged because Phorge and browsers already use them.
const (
	DefaultClientPort = 22280
	DefaultAdminPort  = 22281
	DefaultListenAddr = "0.0.0.0"
)

const (
	ServerKindClient = "client"
	ServerKindAdmin  = "admin"
)

type ServerSpec struct {
	Type   string `json:"type"`
	Port   int    `json:"port"`
	Listen string `json:"listen"`
}

func (s ServerSpec) Addr() string {
	return net.JoinHostPort(s.Listen, strconv.Itoa(s.Port))
}

type PeerSpec struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
}

// Config keeps the Aphlict JSON wire shape because that is still the protocol
// Phorge understands. Only the standalone environment-variable aliases have
// been retired; protocol compatibility is not legacy implementation code.
type Config struct {
	Servers []ServerSpec `json:"servers"`
	Cluster []PeerSpec   `json:"cluster"`
}

func Load() (*Config, error) {
	cfg := LoadFromEnv()
	if path := ConfigFilePath(); path != "" {
		fileCfg, err := LoadFromFile(path)
		if err != nil {
			return nil, err
		}
		cfg = fileCfg
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func ConfigFilePath() string {
	return config.EnvStr("", "GORGE_NOTIFICATION_CONFIG_FILE")
}

func LoadFromEnv() *Config {
	listen := config.EnvStr(DefaultListenAddr, "GORGE_NOTIFICATION_LISTEN_ADDR")
	return &Config{
		Servers: []ServerSpec{
			{
				Type:   ServerKindClient,
				Port:   config.EnvInt(DefaultClientPort, "GORGE_NOTIFICATION_CLIENT_PORT"),
				Listen: listen,
			},
			{
				Type:   ServerKindAdmin,
				Port:   config.EnvInt(DefaultAdminPort, "GORGE_NOTIFICATION_ADMIN_PORT"),
				Listen: listen,
			},
		},
	}
}

// LoadFromFile reads the established Aphlict-format protocol configuration.
// TLS/log/pidfile fields are intentionally not part of the Go service: TLS is
// terminated upstream, logs go to stdout, and the container owns lifecycle.
func LoadFromFile(path string) (*Config, error) {
	cfg := &Config{}
	if err := config.LoadJSONFile(path, cfg); err != nil {
		return nil, err
	}
	for i := range cfg.Servers {
		if cfg.Servers[i].Listen == "" {
			cfg.Servers[i].Listen = DefaultListenAddr
		}
	}
	return cfg, nil
}

func (c *Config) validate() error {
	var hasClient, hasAdmin bool
	for _, spec := range c.Servers {
		switch spec.Type {
		case ServerKindClient:
			hasClient = true
		case ServerKindAdmin:
			hasAdmin = true
		default:
			return fmt.Errorf("unknown server type %q: expected %q or %q",
				spec.Type, ServerKindClient, ServerKindAdmin)
		}
		if spec.Port <= 0 {
			return fmt.Errorf("%s server: %d is not a usable port", spec.Type, spec.Port)
		}
	}
	if !hasClient || !hasAdmin {
		return errors.New("both an admin and a client server must be configured")
	}
	return nil
}
