package hub

import (
	"sync"

	"github.com/gorilla/websocket"
)

// Listener is one WebSocket client and the set of PHIDs it has subscribed to.
// It does not record which instance it belongs to: the Hub already keys it under
// one, and a second copy of that could only ever disagree with the first.
type Listener struct {
	id            uint64
	conn          *websocket.Conn
	subscriptions map[string]struct{}
	mu            sync.RWMutex
	// writeMu serialises writes. A gorilla connection permits only one writer
	// at a time, and two are in play here: the fan-out from a published message
	// and the replies the client's own read loop sends.
	writeMu sync.Mutex
}

// NewListener wraps an upgraded connection.
func NewListener(id uint64, conn *websocket.Conn) *Listener {
	return &Listener{
		id:            id,
		conn:          conn,
		subscriptions: make(map[string]struct{}),
	}
}

// ID reports the connection id this listener is logged under.
func (l *Listener) ID() uint64 { return l.id }

// Subscribe adds PHIDs to the set this listener wants messages for.
func (l *Listener) Subscribe(phids []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range phids {
		l.subscriptions[p] = struct{}{}
	}
}

// Unsubscribe removes PHIDs from the set.
func (l *Listener) Unsubscribe(phids []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range phids {
		delete(l.subscriptions, p)
	}
}

// IsSubscribedToAny reports whether the listener asked for any of these PHIDs.
func (l *Listener) IsSubscribedToAny(phids []string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, p := range phids {
		if _, ok := l.subscriptions[p]; ok {
			return true
		}
	}
	return false
}

// WriteJSON sends a JSON frame to the client.
func (l *Listener) WriteJSON(v any) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	return l.conn.WriteJSON(v)
}

// ReadMessage reads the next frame from the client.
func (l *Listener) ReadMessage() ([]byte, error) {
	_, data, err := l.conn.ReadMessage()
	return data, err
}

// RemoteAddr reports the peer address, for logging.
func (l *Listener) RemoteAddr() string {
	return l.conn.RemoteAddr().String()
}

// Close hangs up on the client.
func (l *Listener) Close() {
	_ = l.conn.Close()
}
