package projection

import (
	"encoding/json"
	"fmt"
	"strings"
)

// encoding/json accepts duplicate keys and case-insensitive field aliases.
// Reject ambiguous objects before their contents participate in an event hash.
func uniqueJSONKeys(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return fmt.Errorf("projection JSON nesting too deep")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return fmt.Errorf("invalid JSON key")
			}
			folded := strings.ToLower(name)
			if seen[folded] {
				return fmt.Errorf("duplicate JSON key %q", name)
			}
			seen[folded] = true
			if err := uniqueJSONKeys(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := uniqueJSONKeys(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}
