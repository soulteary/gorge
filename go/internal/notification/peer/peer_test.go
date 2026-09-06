package peer

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func TestListFingerprint(t *testing.T) {
	pl := NewList()
	fp := pl.Fingerprint()
	if len(fp) != fingerprintLen {
		t.Errorf("fingerprint length = %d, want %d", len(fp), fingerprintLen)
	}

	pl2 := NewList()
	if pl.Fingerprint() == pl2.Fingerprint() {
		t.Error("two lists should have different fingerprints")
	}
}

func TestAddFingerprint(t *testing.T) {
	pl := NewList()

	msg := map[string]any{
		"type": "notification",
	}

	if !pl.AddFingerprint(msg) {
		t.Error("first AddFingerprint should return true")
	}

	touched, ok := toStringSlice(msg["touched"])
	if !ok || len(touched) != 1 {
		t.Fatalf("expected touched with 1 entry, got %v", msg["touched"])
	}
	if touched[0] != pl.Fingerprint() {
		t.Errorf("touched[0] = %q, want %q", touched[0], pl.Fingerprint())
	}

	if pl.AddFingerprint(msg) {
		t.Error("second AddFingerprint should return false (already seen)")
	}
}

func TestAddFingerprintMultiplePeers(t *testing.T) {
	pl1 := NewList()
	pl2 := NewList()

	msg := map[string]any{
		"type": "notification",
	}

	pl1.AddFingerprint(msg)
	if !pl2.AddFingerprint(msg) {
		t.Error("pl2 should be able to add its fingerprint")
	}

	touched, _ := toStringSlice(msg["touched"])
	if len(touched) != 2 {
		t.Fatalf("expected 2 touches, got %d", len(touched))
	}
}

// TestAddFingerprintReadsDecodedMessages checks the shape that actually arrives
// over the wire: a JSON round trip turns "touched" into []any, and a list that
// failed to read it would republish a message the cluster has already handled.
func TestAddFingerprintReadsDecodedMessages(t *testing.T) {
	pl := NewList()

	encoded, err := json.Marshal(map[string]any{"touched": []string{pl.Fingerprint()}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}

	if pl.AddFingerprint(decoded) {
		t.Error("expected our own fingerprint to be recognised after a JSON round trip")
	}
}

// newReceiptServer stands in for a peer's admin port, recording what it received
// and answering with the bare receipt Phorge's format calls for.
func newReceiptServer(t *testing.T, fingerprint string) (*Peer, <-chan *http.Request, <-chan []byte) {
	t.Helper()

	requests := make(chan *http.Request, 1)
	bodies := make(chan []byte, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- r
		bodies <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"fingerprint":"` + fingerprint + `"}`))
	}))
	t.Cleanup(srv.Close)

	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}

	portNum, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return NewPeer(host, portNum, "http"), requests, bodies
}

// TestPeerBroadcastLearnsTheFingerprint covers the relay path end to end: the
// message reaches the peer's admin port with the instance attached, and the
// fingerprint from the receipt is what later lets the list skip that peer.
func TestPeerBroadcastLearnsTheFingerprint(t *testing.T) {
	const remote = "abcdefghijklmnop"

	p, requests, bodies := newReceiptServer(t, remote)
	if p.Fingerprint() != "" {
		t.Error("a fresh peer must have no fingerprint until it answers one")
	}

	p.BroadcastMessage("prod", map[string]any{"type": "notification", "key": "7"})

	select {
	case req := <-requests:
		if req.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", req.Method)
		}
		if got := req.URL.Query().Get("instance"); got != "prod" {
			t.Errorf("expected instance=prod, got %q", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("expected a JSON content type, got %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the peer was never called")
	}

	var relayed map[string]any
	if err := json.Unmarshal(<-bodies, &relayed); err != nil {
		t.Fatalf("the relayed body is not JSON: %v", err)
	}
	if relayed["key"] != "7" {
		t.Errorf("expected the message to travel unchanged, got %v", relayed)
	}

	if p.Fingerprint() != remote {
		t.Errorf("expected the peer's fingerprint to be learned as %q, got %q", remote, p.Fingerprint())
	}
}

// TestPeerBroadcastSurvivesAnUnreachablePeer pins the degradability the domain
// relies on: a dead peer must not panic or block, only be logged.
func TestPeerBroadcastSurvivesAnUnreachablePeer(t *testing.T) {
	// Port 1 is reserved and never listening, so this fails to connect quickly.
	p := NewPeer("127.0.0.1", 1, "http")
	p.BroadcastMessage("default", map[string]any{"type": "notification"})

	if p.Fingerprint() != "" {
		t.Error("an unreachable peer must not report a fingerprint")
	}
}

// TestListBroadcastSkipsPeersThatHaveSeenTheMessage is the loop prevention: once
// a peer's fingerprint is in "touched", relaying to it again would bounce the
// message back and forth forever.
func TestListBroadcastSkipsPeersThatHaveSeenTheMessage(t *testing.T) {
	const remote = "abcdefghijklmnop"

	p, requests, _ := newReceiptServer(t, remote)
	// Teach the list this peer's fingerprint the only way it can be learned.
	p.BroadcastMessage("default", map[string]any{"type": "warmup"})
	<-requests
	if p.Fingerprint() != remote {
		t.Fatalf("setup failed: fingerprint is %q", p.Fingerprint())
	}

	pl := NewList()
	pl.AddPeer(p)

	pl.BroadcastMessage("default", map[string]any{
		"type":    "notification",
		"touched": []string{remote},
	})

	select {
	case <-requests:
		t.Error("relayed to a peer that had already stamped the message")
	case <-time.After(250 * time.Millisecond):
	}
}

func TestListBroadcastReachesUnstampedPeers(t *testing.T) {
	p, requests, _ := newReceiptServer(t, "abcdefghijklmnop")

	pl := NewList()
	pl.AddPeer(p)
	pl.BroadcastMessage("default", map[string]any{"type": "notification"})

	select {
	case <-requests:
	case <-time.After(5 * time.Second):
		t.Error("an unstamped peer was never relayed to")
	}
}

func TestToStringSlice(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want int
		ok   bool
	}{
		{"nil", nil, 0, false},
		{"strings", []string{"a", "b"}, 2, true},
		{"any", []any{"x"}, 1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := toStringSlice(tt.in)
			if ok != tt.ok {
				t.Errorf("ok = %v, want %v", ok, tt.ok)
			}
			if len(got) != tt.want {
				t.Errorf("len = %d, want %d", len(got), tt.want)
			}
		})
	}
}
