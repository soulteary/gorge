package worker

import (
	"fmt"
	"strings"
	"testing"
)

func TestLegacyPrintableASCIIBoundary(t *testing.T) {
	suffix := fmt.Sprintf(":%d:%d:gorge-worker", 123, int64(1700000000))
	for _, n := range []int{0, 1, 64 - len(suffix)} {
		host := strings.Repeat("a", n)
		want := host + suffix
		if got := boundedLeaseOwner(host, 123, 1700000000); got != want {
			t.Fatalf("legacy %d-byte input changed", len(want))
		}
	}
	host := strings.Repeat("a", 65-len(suffix))
	if got := boundedLeaseOwner(host, 123, 1700000000); len(got) != 64 || got == host+suffix {
		t.Fatal("65-byte identity not bounded")
	}
}
func TestLeaseOwnerLongContainerHostname(t *testing.T) {
	// A 36-byte container hostname reaches the native limit with a three-digit PID.
	host := strings.Repeat("h", 36)
	for _, pid := range []int{999, 1000, 99999} {
		raw := fmt.Sprintf("%s:%d:%d:gorge-worker", host, pid, int64(1794857000))
		if pid == 999 && len(raw) != 64 {
			t.Fatal("unexpected legacy boundary")
		}
		if pid >= 1000 && len(raw) <= 64 {
			t.Fatal("missing legacy overflow characterization")
		}
		if len(boundedLeaseOwner(host, pid, 1794857000)) > 64 {
			t.Fatal("candidate overflows native column")
		}
	}
}
func TestFullInputGoldenDigest(t *testing.T) {
	got := boundedLeaseOwner(strings.Repeat("x", 63), 12345, 1700000000)
	if got != "220d76d2b53c8c71b089d1c9f695b4ca8d7fa0ae81ab90b90ea5278f510a602c" {
		t.Fatal("full legacy tuple digest differs")
	}
}
func TestUnicodeAndInvalidHeaderBytesBecomeBoundedASCII(t *testing.T) {
	for _, host := range []string{"部门", strings.Repeat("部", 64), "a\nb", "a\x00b", string([]byte{0xff}), strings.Repeat("x", 4096)} {
		got := boundedLeaseOwner(host, 123, 1700000000)
		if len(got) != 64 {
			t.Fatalf("unexpected fallback length %d", len(got))
		}
		for _, c := range []byte(got) {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				t.Fatal("non-ASCII digest")
			}
		}
	}
}
func TestProcessDistinguishingInputsRemainDistinguishable(t *testing.T) {
	maxPID := int(^uint(0) >> 1)
	host := strings.Repeat("same-prefix", 20)
	inputs := []struct {
		host  string
		pid   int
		epoch int64
	}{{host, 123, 1700000000}, {host + "x", 123, 1700000000}, {host, 124, 1700000000}, {host, 123, 1700000001}, {host, maxPID, 1700000000}, {host, maxPID - 1, 1700000000}}
	seen := map[string]bool{}
	for _, in := range inputs {
		v := boundedLeaseOwner(in.host, in.pid, in.epoch)
		if seen[v] {
			t.Fatal("distinct tested process tuples collided")
		}
		seen[v] = true
	}
}
func TestLeaseOwnerDeterministicForSameProcessTuple(t *testing.T) {
	host := strings.Repeat("long", 30)
	if boundedLeaseOwner(host, 1, 1700000000) != boundedLeaseOwner(host, 1, 1700000000) {
		t.Fatal("candidate unexpectedly changed deterministic legacy tuple behavior")
	}
}
