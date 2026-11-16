package worker

import (
	"crypto/sha256"
	"fmt"
)

const maxLeaseOwnerBytes = 64

// boundedLeaseOwner preserves the legacy printable-ASCII identity when it fits
// Phorge's native text64 column. The fallback hashes the entire legacy tuple,
// not a truncated hostname, and remains valid in HTTP headers and native SQL.
// This retains the legacy PID/second-resolution incarnation assumptions: it
// does not promise distinct identities for identical host, PID and epoch input.
func boundedLeaseOwner(hostname string, pid int, epoch int64) string {
	raw := fmt.Sprintf("%s:%d:%d:gorge-worker", hostname, pid, epoch)
	safe := len(raw) <= maxLeaseOwnerBytes
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x20 || raw[i] > 0x7e {
			safe = false
			break
		}
	}
	if safe {
		return raw
	}
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", sum)
}
