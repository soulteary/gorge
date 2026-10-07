// Package peer relays messages between the notification servers of a cluster.
// Each server stamps the messages it handles with its own fingerprint, and a
// message is never sent back to a server already in that stamp list; that is the
// whole of the loop prevention, and it is what makes a mesh of peers safe.
package peer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// fingerprintAlphabet omits the characters that are easy to confuse when a
	// fingerprint is read out of a log or a status page.
	fingerprintAlphabet = "23456789abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ"
	fingerprintLen      = 16

	// broadcastTimeout bounds a relay to a peer. Notification delivery is
	// degradable, so an unreachable peer must cost a bounded wait rather than
	// pile goroutines up behind a stalled connection.
	broadcastTimeout = 5 * time.Second
	maxPeers         = 32
	relayQueueFrames = 64
	relayQueueBytes  = 4 * 1024 * 1024
	maxReceiptBytes  = 64 * 1024
)

type relay struct {
	instance string
	data     []byte
}

// Peer is another notification server's admin port.
type Peer struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	// fingerprint is learned from the peer's own POST receipts, so it is empty
	// until the first successful relay.
	fingerprint string
	mu          sync.RWMutex
	client      *http.Client
	queue       chan relay
	queueMu     sync.Mutex
	queuedBytes int
	startOnce   sync.Once
	closeOnce   sync.Once
	ctx         context.Context
	cancel      context.CancelFunc
	dropped     atomic.Uint64
}

// NewPeer describes a peer's admin port.
func NewPeer(host string, port int, protocol string) *Peer {
	ctx, cancel := context.WithCancel(context.Background())
	return &Peer{
		Host:     host,
		Port:     port,
		Protocol: protocol,
		client: &http.Client{
			Timeout:       broadcastTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		queue: make(chan relay, relayQueueFrames),
		ctx:   ctx, cancel: cancel,
	}
}

// Fingerprint reports the peer's fingerprint, or "" if none has been learned.
func (p *Peer) Fingerprint() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.fingerprint
}

func (p *Peer) setFingerprint(fp string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fingerprint = fp
}

// BroadcastMessage relays a message to the peer's admin port and remembers the
// fingerprint the peer answers with, which is what later lets List skip it.
// Failures are logged and dropped: a peer that cannot be reached must not hold
// up the local delivery that already succeeded.
func (p *Peer) BroadcastMessage(instance string, message map[string]any) {
	data, err := json.Marshal(message)
	if err != nil {
		return
	}
	p.send(instance, data)
}

func (p *Peer) send(instance string, data []byte) {
	address := fmt.Sprintf("%s://%s:%d/?instance=%s", p.Protocol, p.Host, p.Port, url.QueryEscape(instance))
	req, err := http.NewRequestWithContext(p.ctx, http.MethodPost, address, bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		slog.Warn("peer broadcast failed", "host", p.Host, "port", p.Port, "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReceiptBytes+1))
	if err != nil || len(body) > maxReceiptBytes {
		return
	}
	var receipt struct {
		Fingerprint string `json:"fingerprint"`
	}
	if json.Unmarshal(body, &receipt) == nil && receipt.Fingerprint != "" {
		p.setFingerprint(receipt.Fingerprint)
	}
}

// enqueue isolates peers: one ordered relay writer per peer, with a bounded
// count and byte budget. Saturated peers drop new realtime messages, not local
// deliveries or another peer's work. This is not a durable delivery queue.
func (p *Peer) enqueue(instance string, data []byte) bool {
	p.startOnce.Do(func() { go p.run() })
	p.queueMu.Lock()
	defer p.queueMu.Unlock()
	if p.ctx.Err() != nil || p.queuedBytes+len(data) > relayQueueBytes {
		return false
	}
	select {
	case p.queue <- relay{instance, data}:
		p.queuedBytes += len(data)
		return true
	default:
		return false
	}
}

func (p *Peer) run() {
	for {
		if p.ctx.Err() != nil {
			return
		}
		select {
		case <-p.ctx.Done():
			return
		case next := <-p.queue:
			if p.ctx.Err() != nil {
				return
			}
			p.send(next.instance, next.data)
			p.queueMu.Lock()
			p.queuedBytes -= len(next.data)
			p.queueMu.Unlock()
		}
	}
}

func (p *Peer) Close() { p.closeOnce.Do(p.cancel) }

// List is this server's own fingerprint plus the peers it relays to.
type List struct {
	mu          sync.RWMutex
	peers       []*Peer
	fingerprint string
}

// NewList mints this server's fingerprint. It is generated per process rather
// than configured, so two servers can never collide on one by misconfiguration.
func NewList() *List {
	return &List{
		fingerprint: generateFingerprint(),
	}
}

// AddPeer registers a peer to relay to.
func (pl *List) AddPeer(p *Peer) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if len(pl.peers) >= maxPeers {
		p.Close()
		return
	}
	pl.peers = append(pl.peers, p)
}

// Close cancels pending relay I/O when the owning notification service stops.
func (pl *List) Close() {
	pl.mu.RLock()
	defer pl.mu.RUnlock()
	for _, p := range pl.peers {
		p.Close()
	}
}

// Fingerprint reports this server's own fingerprint.
func (pl *List) Fingerprint() string {
	return pl.fingerprint
}

// AddFingerprint stamps the message with this server's fingerprint and reports
// whether the message is new here. False means the stamp was already present, so
// the message has been round the cluster and back: publishing it again would
// deliver it twice and relay it forever.
func (pl *List) AddFingerprint(message map[string]any) bool {
	fp := pl.fingerprint

	touched, _ := toStringSlice(message["touched"])
	for _, t := range touched {
		if t == fp {
			return false
		}
	}

	message["touched"] = append(touched, fp)
	return true
}

// BroadcastMessage relays the message to every peer that has not already seen
// it. Relays run concurrently because one unreachable peer must not delay the
// others by its whole timeout.
func (pl *List) BroadcastMessage(instance string, message map[string]any) {
	data, err := json.Marshal(message)
	if err != nil {
		return
	}
	touched, _ := toStringSlice(message["touched"])
	touchSet := make(map[string]struct{}, len(touched))
	for _, t := range touched {
		touchSet[t] = struct{}{}
	}

	pl.mu.RLock()
	peers := make([]*Peer, len(pl.peers))
	copy(peers, pl.peers)
	pl.mu.RUnlock()

	for _, p := range peers {
		// An unknown fingerprint means we have never had a receipt from this
		// peer, so it cannot be in the stamp list and has to be tried.
		if fp := p.Fingerprint(); fp != "" {
			if _, seen := touchSet[fp]; seen {
				continue
			}
		}
		if !p.enqueue(instance, data) {
			if count := p.dropped.Add(1); count == 1 || count%128 == 0 {
				slog.Warn("peer relay budget exhausted", "host", p.Host, "port", p.Port, "dropped", count)
			}
		}
	}
}

func generateFingerprint() string {
	out := make([]byte, fingerprintLen)
	for i := range out {
		idx, _ := rand.Int(rand.Reader, big.NewInt(int64(len(fingerprintAlphabet))))
		out[i] = fingerprintAlphabet[idx.Int64()]
	}
	return string(out)
}

func toStringSlice(v any) ([]string, bool) {
	if v == nil {
		return nil, false
	}
	switch arr := v.(type) {
	case []string:
		return arr, true
	case []any:
		out := make([]string, 0, len(arr))
		for _, item := range arr {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out, len(out) > 0
	}
	return nil, false
}
