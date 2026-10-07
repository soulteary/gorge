package httpx

import (
	"net/url"
	"sort"
	"strings"

	"github.com/gofiber/fiber/v3"
)

const redactedValue = "[REDACTED]"

func credentialQueryKey(raw string) bool {
	key, err := url.QueryUnescape(raw)
	if err != nil {
		key = raw
	}
	key = strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(key))
	switch key {
	case "token", "servicetoken", "accesstoken", "refreshtoken", "apikey", "password", "passwd", "secret", "clientsecret", "authorization":
		return true
	}
	return false
}

// redactURI preserves path, query order and encoding without parsing values as
// URLs. Even malformed query values must not turn logging into a secret leak.
func redactURI(raw string) (string, []string) {
	path, query, ok := strings.Cut(raw, "?")
	if !ok {
		return raw, nil
	}
	var result strings.Builder
	result.WriteString(path)
	result.WriteByte('?')
	var secrets []string
	for len(query) > 0 {
		// fasthttp/Fiber treats a literal semicolon as part of a value, not a
		// separator. Splitting it would expose the remainder of a token.
		end := strings.IndexByte(query, '&')
		part, separator := query, ""
		if end >= 0 {
			part, separator, query = query[:end], query[end:end+1], query[end+1:]
		} else {
			query = ""
		}
		key, value, _ := strings.Cut(part, "=")
		if credentialQueryKey(key) {
			result.WriteString(key)
			result.WriteByte('=')
			result.WriteString(redactedValue)
			if value != "" {
				secrets = append(secrets, value)
				if decoded, err := url.QueryUnescape(value); err == nil && decoded != value {
					secrets = append(secrets, decoded)
				}
			}
		} else {
			result.WriteString(part)
		}
		result.WriteString(separator)
	}
	return result.String(), secrets
}

func requestLogURI(c fiber.Ctx) string {
	clean, _ := redactURI(c.OriginalURL())
	return clean
}

// An exception may repeat a credential value or the request URL. Protect the
// diagnostic fields as well as uri; sorting prevents a short value from
// partially replacing a longer secret first.
func requestLogError(c fiber.Ctx, message string) string {
	clean, secrets := redactURI(c.OriginalURL())
	message = strings.ReplaceAll(message, c.OriginalURL(), clean)
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		message = strings.ReplaceAll(message, secret, redactedValue)
	}
	return message
}
