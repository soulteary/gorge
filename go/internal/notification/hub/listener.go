package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/fasthttp/websocket"
)

const (
	MaxFrameBytes        = 64 * 1024
	MaxSubscriptions     = 4096
	maxSubscriptionBytes = 512
	listenerQueueFrames  = 64
	listenerQueueBytes   = 4 * 1024 * 1024
	listenerWriteTimeout = 5 * time.Second
)

var ErrResourceLimit = errors.New("notification listener resource limit exceeded")

type listenerConnection interface {
	WriteMessage(int, []byte) error
	ReadMessage() (int, []byte, error)
	SetWriteDeadline(time.Time) error
	SetReadLimit(int64)
	RemoteAddr() net.Addr
	Close() error
}

type outboundFrame struct {
	data []byte
	sent func()
}

// Listener is one WebSocket client and the set of PHIDs it has subscribed to.
// It does not record which instance it belongs to: the Hub already keys it under
// one, and a second copy of that could only ever disagree with the first.
type Listener struct {
	id            uint64
	conn          listenerConnection
	subscriptions map[string]struct{}
	mu            sync.RWMutex
	queue         chan outboundFrame
	done          chan struct{}
	closeOnce     sync.Once
	queueMu       sync.Mutex
	queuedBytes   int
}

// NewListener wraps an upgraded connection.
func NewListener(id uint64, conn *websocket.Conn) *Listener {
	if conn == nil {
		return newListener(id, nil)
	}
	return newListener(id, conn)
}

func newListener(id uint64, conn listenerConnection) *Listener {
	l := &Listener{
		id:            id,
		conn:          conn,
		subscriptions: make(map[string]struct{}),
		queue:         make(chan outboundFrame, listenerQueueFrames),
		done:          make(chan struct{}),
	}
	if conn != nil {
		conn.SetReadLimit(MaxFrameBytes)
		go l.writeLoop()
	}
	return l
}

// ID reports the connection id this listener is logged under.
func (l *Listener) ID() uint64 { return l.id }

// Subscribe adds PHIDs to the set this listener wants messages for.
func (l *Listener) Subscribe(phids []string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	additional := make(map[string]struct{})
	for _, p := range phids {
		if len(p) > maxSubscriptionBytes {
			return ErrResourceLimit
		}
		if _, exists := l.subscriptions[p]; !exists {
			additional[p] = struct{}{}
		}
	}
	if len(l.subscriptions)+len(additional) > MaxSubscriptions {
		return ErrResourceLimit
	}
	for p := range additional {
		l.subscriptions[p] = struct{}{}
	}
	return nil
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
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return l.enqueue(data, nil)
}

// WriteJSONContext waits for this frame's single-writer completion. Replay
// uses it to pace a bounded history instead of turning a healthy browser's
// finite queue into a limit on the number of messages it can replay. Live
// publication continues to use the nonblocking enqueue path.
func (l *Listener) WriteJSONContext(ctx context.Context, v any) error {
	if err := ctx.Err(); err != nil {
		l.Close()
		return err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	written := make(chan struct{})
	if err := l.enqueue(data, func() { close(written) }); err != nil {
		return err
	}
	select {
	case <-written:
		return nil
	case <-l.done:
		return net.ErrClosed
	case <-ctx.Done():
		l.Close()
		return ctx.Err()
	}
}

// enqueue never waits for a browser. A full budget disconnects only that
// listener; the normal Aphlict reconnect/replay path remains available.
func (l *Listener) enqueue(data []byte, sent func()) error {
	l.queueMu.Lock()
	select {
	case <-l.done:
		l.queueMu.Unlock()
		return net.ErrClosed
	default:
	}
	if l.queuedBytes+len(data) > listenerQueueBytes {
		l.queueMu.Unlock()
		l.Close()
		return ErrResourceLimit
	}
	select {
	case l.queue <- outboundFrame{data: data, sent: sent}:
		l.queuedBytes += len(data)
		l.queueMu.Unlock()
		return nil
	default:
		l.queueMu.Unlock()
		l.Close()
		return ErrResourceLimit
	}
}

func (l *Listener) writeLoop() {
	defer l.Close()
	for {
		select {
		case <-l.done:
			return
		case frame := <-l.queue:
			select {
			case <-l.done:
				return
			default:
			}
			if err := l.conn.SetWriteDeadline(time.Now().Add(listenerWriteTimeout)); err != nil {
				return
			}
			if err := l.conn.WriteMessage(websocket.TextMessage, frame.data); err != nil {
				return
			}
			l.queueMu.Lock()
			l.queuedBytes -= len(frame.data)
			l.queueMu.Unlock()
			if frame.sent != nil {
				frame.sent()
			}
		}
	}
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
	l.closeOnce.Do(func() {
		close(l.done)
		if l.conn != nil {
			_ = l.conn.Close()
		}
	})
}
