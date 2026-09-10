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

// LoadBase reads the shared settings from the canonical GORGE_ names.
//
// Pre-monorepo LISTEN_ADDR / SERVICE_TOKEN aliases were removed after Phorge
// made Gorge part of the default deployment stack. Keeping two spellings here
// would let a stale standalone environment silently override the deployment
// contract we now publish and test as one unit.
func LoadBase(defaultListenAddr string) Base {
	return Base{
		ListenAddr:   EnvStr(defaultListenAddr, "GORGE_LISTEN_ADDR"),
		ServiceToken: EnvStr("", "GORGE_SERVICE_TOKEN"),
	}
}

// EnvStr returns the value of the first key that is set to a non-empty string,
// or fallback when none is. The helper remains variadic for callers which have
// multiple current sources, but legacy aliases are no longer part of the
// monorepo service configuration contract.
func EnvStr(fallback string, keys ...string) string {
	for _, key := range keys {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	return fallback
}

// EnvInt behaves like EnvStr but parses the value as an integer. A key set to
// an unparseable value is skipped instead of becoming a zero-valued limit.
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
