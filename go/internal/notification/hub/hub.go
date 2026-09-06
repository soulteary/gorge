// Package hub holds the in-memory connection table and message history that
// back the notification domain. It is the whole of the service's state: nothing
// is persisted, so a restart drops every subscription and every held message,
// which is why the domain is safe to treat as degradable.
package hub

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

const (
	// historySizeLimit and historyAgeLimit bound what a reconnecting client can
	// replay. They match the limits Aphlict enforced, and the age limit is the
	// one that matters: a client asking to replay more than a minute of traffic
	// gets whatever is left, not an error.
	historySizeLimit = 4096
	historyAgeLimit  = 60 * time.Second

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
	timestamp time.Time
	message   Message
}

// Hub fans messages out to the listeners of an instance and keeps a short
// replay history. One Hub is shared by both ports of the process: the admin
// port publishes into it and the client port's listeners read out of it.
type Hub struct {
	mu          sync.RWMutex
	instances   map[string]*listenerList
	history     []historyEntry
	nextID      atomic.Uint64
	startTime   time.Time
	messagesIn  atomic.Int64
	messagesOut atomic.Int64
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

// getList returns the instance's list, creating it on first use. The read lock
// is taken first because every published message and every status request goes
// through here, while creation happens once per instance.
func (h *Hub) getList(instance string) *listenerList {
	h.mu.RLock()
	ll, ok := h.instances[instance]
	h.mu.RUnlock()
	if ok {
		return ll
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if ll, ok = h.instances[instance]; ok {
		return ll
	}
	ll = newListenerList()
	h.instances[instance] = ll
	return ll
}

// AddListener registers a connection under an instance.
func (h *Hub) AddListener(instance string, l *Listener) {
	h.getList(instance).add(l)
}

// RemoveListener drops a connection from an instance.
func (h *Hub) RemoveListener(instance string, id uint64) {
	h.getList(instance).remove(id)
}

// NextID hands out the connection ids that identify listeners in logs.
func (h *Hub) NextID() uint64 {
	return h.nextID.Add(1)
}

// Publish records a message in the history and delivers it to every listener of
// the instance that subscribes to one of its subscribers. A message with no
// subscribers goes to everyone, which is how Aphlict broadcasts.
func (h *Hub) Publish(instance string, msg Message) {
	h.messagesIn.Add(1)

	h.mu.Lock()
	h.history = append(h.history, historyEntry{
		timestamp: time.Now(),
		message:   msg,
	})
	h.purgeHistory()
	h.mu.Unlock()

	subscribers, _ := ToStringSlice(msg["subscribers"])

	ll := h.getList(instance)
	for _, l := range ll.snapshot() {
		if len(subscribers) > 0 && !l.IsSubscribedToAny(subscribers) {
			continue
		}
		// A write error means the peer is gone; reap it here rather than
		// waiting for its read loop to notice, so a dropped connection stops
		// counting as an active client straight away.
		if err := l.WriteJSON(msg); err != nil {
			slog.Debug("dropping unreachable listener", "listener", l.id, "instance", instance, "error", err)
			ll.remove(l.id)
			l.Close()
			continue
		}
		h.messagesOut.Add(1)
	}
}

// GetHistory returns the messages recorded at or after minAge, oldest first.
func (h *Hub) GetHistory(minAge time.Time) []Message {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var results []Message
	for _, e := range h.history {
		if !e.timestamp.Before(minAge) {
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
	if len(h.history) > historySizeLimit {
		keep = len(h.history) - historySizeLimit
	}

	cutoff := time.Now().Add(-historyAgeLimit)
	for keep < len(h.history) {
		if !h.history[keep].timestamp.Before(cutoff) {
			break
		}
		keep++
	}

	if keep > 0 {
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
		Instance:      instance,
		Uptime:        int64(time.Since(h.startTime) / time.Millisecond),
		ClientsActive: ll.activeCount(),
		ClientsTotal:  ll.totalCountVal(),
		MessagesIn:    h.messagesIn.Load(),
		MessagesOut:   h.messagesOut.Load(),
		Version:       protocolVersion,
	}

	h.mu.RLock()
	status.HistorySize = len(h.history)
	if len(h.history) > 0 {
		age := int64(time.Since(h.history[0].timestamp) / time.Millisecond)
		status.HistoryAge = &age
	}
	h.mu.RUnlock()

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
