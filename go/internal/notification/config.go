package notification

import (
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/soulteary/gorge/go/internal/platform/config"
)

// Defaults for the notification service. The two ports are Aphlict's, and they
// stay: Phorge's notification.servers configuration already names them, and the
// client port's value reaches browsers.
const (
	DefaultClientPort = 22280
	DefaultAdminPort  = 22281
	// DefaultListenAddr is a bind host, not a host:port. Each port comes from
	// its own setting because the two are separate listeners.
	DefaultListenAddr = "0.0.0.0"
)

// The two server kinds Phorge requires. PhabricatorNotificationServersConfigType
// rejects a notification.servers list that does not name both, so a
// configuration reaching this service with only one of them is broken upstream.
const (
	ServerKindClient = "client"
	ServerKindAdmin  = "admin"
)

// ServerSpec is one listener. It mirrors an entry of the "servers" list in
// Aphlict's config file so that a file written for Aphlict can be handed
// straight to this service.
type ServerSpec struct {
	Type   string `json:"type"`
	Port   int    `json:"port"`
	Listen string `json:"listen"`
}

// Addr renders the spec as a Go listen address.
func (s ServerSpec) Addr() string {
	return net.JoinHostPort(s.Listen, strconv.Itoa(s.Port))
}

// PeerSpec is one entry of the "cluster" list: another notification server's
// admin port, which this one relays messages to.
type PeerSpec struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
}

// Config is the notification service configuration.
//
// It deliberately does not embed config.Base, and this is the one domain where
// that is the right call rather than an oversight. Base carries ListenAddr and
// ServiceToken; strict Aphlict compatibility rules out a token, because Phorge's
// notification client sends none and adding one would silently stop every
// message, and ListenAddr's host:port meaning does not fit a service that binds
// one host across two ports.
//
// Only "servers" and "cluster" are read from an Aphlict config file. Aphlict's
// format also carries ssl.*, logs and pidfile; those are dropped rather than
// parsed and ignored, since logging goes to stdout for the container runtime to
// collect and TLS is terminated upstream. A file setting ssl.cert therefore has
// no effect here, which is worth knowing before pointing this at a config
// written for the Node server.
type Config struct {
	Servers []ServerSpec `json:"servers"`
	Cluster []PeerSpec   `json:"cluster"`
}

// Load picks the configuration source: an Aphlict-format JSON file when one is
// pointed at, the environment otherwise. It is the single gate that validates
// the result, so callers can rely on both server kinds being present.
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

// ConfigFilePath returns the Aphlict-format JSON file to read, or "" for
// env-only configuration.
func ConfigFilePath() string {
	return config.EnvStr("", "GORGE_NOTIFICATION_CONFIG_FILE", "APHLICT_CONFIG")
}

// LoadFromEnv reads the configuration from the environment, producing the pair
// of listeners Phorge expects.
func LoadFromEnv() *Config {
	listen := config.EnvStr(DefaultListenAddr, "GORGE_NOTIFICATION_LISTEN_ADDR", "LISTEN_ADDR")

	return &Config{
		Servers: []ServerSpec{
			{
				Type:   ServerKindClient,
				Port:   config.EnvInt(DefaultClientPort, "GORGE_NOTIFICATION_CLIENT_PORT", "CLIENT_PORT"),
				Listen: listen,
			},
			{
				Type:   ServerKindAdmin,
				Port:   config.EnvInt(DefaultAdminPort, "GORGE_NOTIFICATION_ADMIN_PORT", "ADMIN_PORT"),
				Listen: listen,
			},
		},
	}
}

// LoadFromFile reads an Aphlict-format configuration file. Unlike the other
// domains this replaces the defaults rather than overriding field by field: the
// file's "servers" list is the complete set of listeners, so merging it with a
// default pair would start ports nobody asked for. Callers should go through
// Load, which validates the result.
func LoadFromFile(path string) (*Config, error) {
	cfg := &Config{}
	if err := config.LoadJSONFile(path, cfg); err != nil {
		return nil, err
	}

	// Aphlict lets an entry omit "listen", and Phorge's own config generator
	// does; fill it so every spec renders a usable address.
	for i := range cfg.Servers {
		if cfg.Servers[i].Listen == "" {
			cfg.Servers[i].Listen = DefaultListenAddr
		}
	}
	return cfg, nil
}

// validate enforces the invariant the PHP side already enforces: Phorge refuses
// to save a notification.servers list lacking either kind, so a config missing
// one is a misconfiguration to report at startup rather than a port this
// service silently never listens on.
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
