// Package config reads service configuration from the environment and from
// JSON files. It is deliberately free of any domain knowledge: every service
// binary in the monorepo composes its own config struct around Base.
package config

import (
	"encoding/json"
	"os"
	"strconv"
)

// Base holds the settings every gorge service has. Domain configs embed it.
type Base struct {
	ListenAddr   string `json:"listenAddr"`
	ServiceToken string `json:"serviceToken"`
}

// LoadBase reads the shared settings, honouring the GORGE_ prefixed names and
// falling back to the unprefixed ones used by the pre-monorepo deployments.
func LoadBase(defaultListenAddr string) Base {
	return Base{
		ListenAddr:   EnvStr(defaultListenAddr, "GORGE_LISTEN_ADDR", "LISTEN_ADDR"),
		ServiceToken: EnvStr("", "GORGE_SERVICE_TOKEN", "SERVICE_TOKEN"),
	}
}

// EnvStr returns the value of the first key that is set to a non-empty string,
// or fallback when none is. Callers pass keys newest-name-first so that a
// GORGE_ prefixed variable always wins over the legacy unprefixed one it
// replaced; this is what lets old compose files keep working unchanged.
func EnvStr(fallback string, keys ...string) string {
	for _, key := range keys {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	return fallback
}

// EnvInt behaves like EnvStr but parses the value as an integer. A key set to
// an unparseable value is skipped, so a typo degrades to the next key rather
// than to a zero-valued limit.
func EnvInt(fallback int, keys ...string) int {
	for _, key := range keys {
		v := os.Getenv(key)
		if v == "" {
			continue
		}
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// EnvBool behaves like EnvStr but parses the value with strconv.ParseBool, so
// 1/t/T/true/TRUE and their false counterparts are all accepted.
func EnvBool(fallback bool, keys ...string) bool {
	for _, key := range keys {
		v := os.Getenv(key)
		if v == "" {
			continue
		}
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

// LoadJSONFile decodes the JSON document at path into dst. Fields absent from
// the file keep the value dst already holds, so callers pre-fill dst with
// their defaults and let the file override only what it mentions.
func LoadJSONFile[T any](path string, dst *T) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}
