// Package hub holds the in-memory connection table and message history that
// back the notification domain. It is the whole of the service's state: nothing
// is persisted, so a restart drops every subscription and every held message,
// which is why the domain is safe to treat as degradable.
package hub

import (
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

const (
	// Public WebSocket paths may name arbitrary instances. Bound the retained
	// registry independently from history and per-connection message budgets.
	MaxInstances     = 1024
	MaxInstanceBytes = 512

	// historySizeLimit and historyAgeLimit bound what a reconnecting client can
	// replay. They match the limits Aphlict enforced, and the age limit is the
	// one that matters: a client asking to replay more than a minute of traffic
	// gets whatever is left, not an error.
	historySizeLimit = 4096
	historyAgeLimit  = 60 * time.Second
	historyByteLimit = 16 * 1024 * 1024

	// protocolVersion is the Aphlict wire protocol version, reported verbatim
	// to Phorge's cluster notification panel. It describes the protocol this
	// service speaks, not this service's own version, so it moves only when the
	// protocol does.
	protocolVersion = 8
)

// Message is one Aphlict message. Its shape is defined by the Phorge code that
// posts it, not here, so it stays an open map and travels through unchanged;
// only the "subscribers" and "touched" keys mean anything to this service.
type Message map[string]any

type historyEntry struct {
	instance  string
	timestamp time.Time
	message   Message
	bytes     int
}

// Hub fans messages out to the listeners of an instance and keeps a short
// replay history. One Hub is shared by both ports of the process: the admin
// port publishes into it and the client port's listeners read out of it.
type Hub struct {
	mu            sync.RWMutex
	instances     map[string]*listenerList
	instanceClock uint64
	history       []historyEntry
	historyBytes  int
	nextID        atomic.Uint64
	startTime     time.Time
	messagesIn    atomic.Int64
	messagesOut   atomic.Int64
}

// New builds an empty Hub and starts its uptime clock.
func New() *Hub {
	return &Hub{
		instances: make(map[string]*listenerList),
		startTime: time.Now(),
	}
}

// listenerList is the set of live connections for one instance. It carries its
// own lock so a fan-out to one instance does not block another.
type listenerList struct {
	mu         sync.RWMutex
	listeners  map[uint64]*Listener
	totalCount int64
	lastUsed   uint64 // protected by Hub.mu, including when the list is idle
}

func newListenerList() *listenerList {
	return &listenerList{
		listeners: make(map[uint64]*Listener),
	}
}

func (ll *listenerList) add(l *Listener) {
	ll.mu.Lock()
	defer ll.mu.Unlock()
	ll.listeners[l.id] = l
	ll.totalCount++
}

func (ll *listenerList) remove(id uint64) {
	ll.mu.Lock()
	defer ll.mu.Unlock()
	delete(ll.listeners, id)
}

// snapshot copies the listeners out so a fan-out can write to them without
// holding the list's lock: a slow client would otherwise stall every other
// delivery on the same instance.
func (ll *listenerList) snapshot() []*Listener {
	ll.mu.RLock()
	defer ll.mu.RUnlock()
	out := make([]*Listener, 0, len(ll.listeners))
	for _, l := range ll.listeners {
		out = append(out, l)
	}
	return out
}

func (ll *listenerList) activeCount() int {
	ll.mu.RLock()
	defer ll.mu.RUnlock()
	return len(ll.listeners)
}

func (ll *listenerList) totalCountVal() int64 {
	ll.mu.RLock()
	defer ll.mu.RUnlock()
	return ll.totalCount
}

// getList only looks up existing listeners. Publication, inspection and cleanup
// must not allocate a permanent instance for an arbitrary name.
func (h *Hub) getList(instance string) *listenerList {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.instances[instance]
}

// AddListener registers a connection under an instance.
func (h *Hub) AddListener(instance string, l *Listener) error {
	if len(instance) > MaxInstanceBytes {
		return ErrResourceLimit
	}
	// Registration and eviction are atomic under the hub lock. Lock order is
	// Hub.mu then listenerList.mu; fan-out never acquires Hub.mu while holding
	// a listener-list lock.
	h.mu.Lock()
	defer h.mu.Unlock()
	ll := h.instances[instance]
	if ll == nil {
		if len(h.instances) >= MaxInstances && !h.evictIdleInstance() {
			return ErrResourceLimit
		}
		ll = newListenerList()
		h.instances[strings.Clone(instance)] = ll
	}
	h.instanceClock++
	ll.lastUsed = h.instanceClock
	ll.add(l)
	return nil
}

// evictIdleInstance preserves every active subscription and retained replay
// message. Only an idle counter cache with no history can be replaced. The
// scoped historical connection counter resets when that cache is evicted.
// Caller holds Hub.mu.
func (h *Hub) evictIdleInstance() bool {
	h.purgeHistory()
	withHistory := make(map[string]bool, len(h.history))
	for _, entry := range h.history {
		withHistory[entry.instance] = true
	}
	var oldest string
	var candidate *listenerList
	for instance, ll := range h.instances {
		if withHistory[instance] || ll.activeCount() != 0 {
			continue
		}
		if candidate == nil || ll.lastUsed < candidate.lastUsed {
			oldest, candidate = instance, ll
		}
	}
	if candidate == nil {
		return false
	}
	delete(h.instances, oldest)
	return true
}

// RemoveListener drops a connection from an instance.
func (h *Hub) RemoveListener(instance string, id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ll := h.instances[instance]; ll != nil {
		ll.remove(id)
		h.instanceClock++
		ll.lastUsed = h.instanceClock
	}
}

// NextID hands out the connection ids that identify listeners in logs.
func (h *Hub) NextID() uint64 {
	return h.nextID.Add(1)
}

// Publish records a message in the history and delivers it to every listener of
// the instance that subscribes to one of its subscribers. A message with no
// subscribers goes to everyone, which is how Aphlict broadcasts.
func (h *Hub) Publish(instance string, msg Message) {
	if len(instance) > MaxInstanceBytes {
		return
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.messagesIn.Add(1)

	h.mu.Lock()
	h.history = append(h.history, historyEntry{
		instance:  strings.Clone(instance),
		timestamp: time.Now(),
		message:   msg,
		bytes:     len(data) + len(instance),
	})
	h.historyBytes += len(data) + len(instance)
	h.purgeHistory()
	h.mu.Unlock()

	subscribers, _ := ToStringSlice(msg["subscribers"])

	ll := h.getList(instance)
	if ll == nil {
		return
	}
	for _, l := range ll.snapshot() {
		if len(subscribers) > 0 && !l.IsSubscribedToAny(subscribers) {
			continue
		}
		// A write error means the peer is gone; reap it here rather than
		// waiting for its read loop to notice, so a dropped connection stops
		// counting as an active client straight away.
		if err := l.enqueue(data, func() { h.messagesOut.Add(1) }); err != nil {
			slog.Debug("dropping unreachable listener", "listener", l.id, "instance", instance, "error", err)
			ll.remove(l.id)
			l.Close()
			continue
		}
	}
}

// GetHistory returns only this instance's messages recorded at or after
// minAge, oldest first. Both addressed messages and broadcasts are isolated.
func (h *Hub) GetHistory(instance string, minAge time.Time) []Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.purgeHistory()

	var results []Message
	for _, e := range h.history {
		if e.instance == instance && !e.timestamp.Before(minAge) {
			results = append(results, e.message)
		}
	}
	return results
}

// purgeHistory drops entries that are over either limit. Callers hold h.mu.
func (h *Hub) purgeHistory() {
	if len(h.history) == 0 {
		return
	}

	keep := 0
	cutoff := time.Now().Add(-historyAgeLimit)
	for keep < len(h.history) {
		if len(h.history)-keep <= historySizeLimit && h.historyBytes <= historyByteLimit && !h.history[keep].timestamp.Before(cutoff) {
			break
		}
		h.historyBytes -= h.history[keep].bytes
		keep++
	}

	if keep > 0 {
		clear(h.history[:keep])
		h.history = h.history[keep:]
	}
}

// Status reports the instance's counters in the shape Phorge's cluster
// notification panel reads. It returns the contract type directly rather than a
// domain struct that would have to be converted: a second shape with its own
// json tags is a second thing to keep in step with the PHP side.
func (h *Hub) Status(instance string) *contracts.AphlictStatus {
	ll := h.getList(instance)

	status := &contracts.AphlictStatus{
		Instance:    instance,
		Uptime:      int64(time.Since(h.startTime) / time.Millisecond),
		MessagesIn:  h.messagesIn.Load(),
		MessagesOut: h.messagesOut.Load(),
		Version:     protocolVersion,
	}
	if ll != nil {
		status.ClientsActive = ll.activeCount()
		status.ClientsTotal = ll.totalCountVal()
	}

	h.mu.Lock()
	h.purgeHistory()
	for _, entry := range h.history {
		if entry.instance != instance {
			continue
		}
		status.HistorySize++
		if status.HistoryAge == nil {
			age := int64(time.Since(entry.timestamp) / time.Millisecond)
			status.HistoryAge = &age
		}
	}
	h.mu.Unlock()

	return status
}

// ToStringSlice reads a list of strings out of a decoded Aphlict message, where
// it arrives as []any. It reports false for anything that yields no strings, so
// callers can tell "no subscribers, deliver to everyone" from a list that named
// somebody.
func ToStringSlice(v any) ([]string, bool) {
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
