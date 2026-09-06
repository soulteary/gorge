package diff

import (
	"github.com/soulteary/gorge/go/internal/platform/config"
)

// DefaultMaxBytes caps the combined size of the two sides of a comparison.
//
// It sits below the 2M transport limit in platform/httpx on purpose, so an
// oversized request is refused by this domain check — which knows what it
// measured — rather than by the middleware. See modules/diff.md for the two
// paths and why they report the same code.
const DefaultMaxBytes = 1048576

// Config is the diff domain's configuration. It deliberately does not embed
// config.Base: the listen address and the service token belong to the process,
// which serves the render domain from the same binary, and two domains each
// claiming to own GORGE_LISTEN_ADDR would leave it ambiguous which one wins.
type Config struct {
	MaxBytes int `json:"maxBytes"`
}

// LoadFromEnv reads the configuration from the environment.
//
// There is no legacy fallback here, unlike every other setting in this
// repository. The pre-monorepo service read MAX_BODY_SIZE, but that was an
// Echo transport limit expressed as a string ("10M") and applied to the whole
// request body, whereas GORGE_DIFF_MAX_BYTES is a byte count applied to
// len(old)+len(new). Accepting the old name would silently reinterpret its
// value, so it is a rename rather than a fallback.
func LoadFromEnv() *Config {
	return &Config{
		MaxBytes: config.EnvInt(DefaultMaxBytes, "GORGE_DIFF_MAX_BYTES"),
	}
}
