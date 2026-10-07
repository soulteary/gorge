package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type budgetConnection struct {
	blocked   bool
	closed    chan struct{}
	frames    chan []byte
	mu        sync.Mutex
	deadline  time.Time
	readLimit int64
	once      sync.Once
}

func (c *budgetConnection) WriteMessage(_ int, data []byte) error {
	if c.blocked {
		<-c.closed
		return net.ErrClosed
	}
	select {
	case c.frames <- append([]byte(nil), data...):
		return nil
	case <-c.closed:
		return net.ErrClosed
	}
}
func (c *budgetConnection) ReadMessage() (int, []byte, error) {
	<-c.closed
	return 0, nil, net.ErrClosed
}
func (c *budgetConnection) SetWriteDeadline(d time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = d
	return nil
}
func (c *budgetConnection) SetReadLimit(n int64) { c.readLimit = n }
func (c *budgetConnection) RemoteAddr() net.Addr { return &net.TCPAddr{} }
func (c *budgetConnection) Close() error         { c.once.Do(func() { close(c.closed) }); return nil }
func budgetConn(blocked bool) *budgetConnection {
	return &budgetConnection{blocked: blocked, closed: make(chan struct{}), frames: make(chan []byte, 256)}
}

type pacedBudgetConnection struct {
	*budgetConnection
	active  atomic.Int32
	maximum atomic.Int32
}

func (c *pacedBudgetConnection) WriteMessage(kind int, data []byte) error {
	active := c.active.Add(1)
	defer c.active.Add(-1)
	if active > c.maximum.Load() {
		c.maximum.Store(active)
	}
	time.Sleep(time.Millisecond)
	return c.budgetConnection.WriteMessage(kind, data)
}

func TestPacedReplayExceedsQueueWithoutDisconnectingHealthyListener(t *testing.T) {
	const count = listenerQueueFrames * 4
	h := New()
	for i := 0; i < count; i++ {
		h.Publish("prod", Message{"key": strconv.Itoa(i)})
	}
	conn := &pacedBudgetConnection{budgetConnection: budgetConn(false)}
	l := newListener(1, conn)
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, msg := range h.GetHistory("prod", time.Time{}) {
		if err := l.WriteJSONContext(ctx, msg); err != nil {
			t.Fatalf("healthy replay rejected %v: %v", msg, err)
		}
	}
	for i := 0; i < count; i++ {
		var msg Message
		if err := json.Unmarshal(<-conn.frames, &msg); err != nil {
			t.Fatal(err)
		}
		if msg["key"] != strconv.Itoa(i) {
			t.Fatalf("replay changed history order: %v", msg)
		}
	}
	if conn.maximum.Load() != 1 {
		t.Fatal("replay bypassed the single writer")
	}
	l.queueMu.Lock()
	remaining := l.queuedBytes
	l.queueMu.Unlock()
	if remaining != 0 || len(l.queue) != 0 {
		t.Fatal("replay returned before writes completed")
	}
	select {
	case <-conn.closed:
		t.Fatal("healthy replay closed the connection")
	default:
	}
}

func TestReplayDeadlineAndCloseWakeBlockedWriter(t *testing.T) {
	for _, timeout := range []bool{true, false} {
		t.Run(strconv.FormatBool(timeout), func(t *testing.T) {
			conn := budgetConn(true)
			l := newListener(1, conn)
			defer l.Close()
			ctx := context.Background()
			if timeout {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
				defer cancel()
			}
			result := make(chan error, 1)
			go func() { result <- l.WriteJSONContext(ctx, Message{"key": "blocked"}) }()
			if !timeout {
				l.Close()
			}
			select {
			case err := <-result:
				if timeout && !errors.Is(err, context.DeadlineExceeded) || !timeout && !errors.Is(err, net.ErrClosed) {
					t.Fatalf("unexpected replay termination: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("replay wait outlived its deadline or close")
			}
			select {
			case <-conn.closed:
			default:
				t.Fatal("terminated replay left its writer blocked")
			}
		})
	}
}

func TestSlowListenerCannotDelayHealthyDelivery(t *testing.T) {
	h := New()
	slowConn, fastConn := budgetConn(true), budgetConn(false)
	slow, fast := newListener(1, slowConn), newListener(2, fastConn)
	defer slow.Close()
	defer fast.Close()
	if err := h.AddListener("prod", slow); err != nil {
		t.Fatal(err)
	}
	if err := h.AddListener("prod", fast); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	for i := 0; i < listenerQueueFrames+2; i++ {
		h.Publish("prod", Message{"key": strconv.Itoa(i)})
		select {
		case frame := <-fastConn.frames:
			var msg Message
			if err := json.Unmarshal(frame, &msg); err != nil {
				t.Fatal(err)
			}
			if msg["key"] != strconv.Itoa(i) {
				t.Fatalf("healthy delivery lost ordering: %v", msg)
			}
		case <-time.After(time.Second):
			t.Fatalf("healthy browser missed frame %d", i)
		}
	}
	if time.Since(started) > time.Second {
		t.Fatal("publish waited for a slow browser")
	}
	select {
	case <-slowConn.closed:
	case <-time.After(time.Second):
		t.Fatal("saturated browser was not disconnected")
	}
	fastConn.mu.Lock()
	deadline := fastConn.deadline
	fastConn.mu.Unlock()
	if deadline.IsZero() || deadline.After(time.Now().Add(listenerWriteTimeout)) {
		t.Fatal("writer omitted bounded deadline")
	}
	if fastConn.readLimit != MaxFrameBytes {
		t.Fatal("frame budget not installed")
	}
}

func TestListenerByteAndSubscriptionBudgets(t *testing.T) {
	l := newListener(1, nil)
	defer l.Close()
	if err := l.enqueue(make([]byte, listenerQueueBytes+1), nil); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("oversized frame accepted: %v", err)
	}
	subs := NewListener(2, nil)
	defer subs.Close()
	phids := make([]string, MaxSubscriptions)
	for i := range phids {
		phids[i] = strconv.Itoa(i)
	}
	if err := subs.Subscribe(phids); err != nil {
		t.Fatal(err)
	}
	if err := subs.Subscribe([]string{"overflow"}); !errors.Is(err, ErrResourceLimit) {
		t.Fatal("subscription budget bypassed")
	}
	if subs.IsSubscribedToAny([]string{"overflow"}) {
		t.Fatal("failed subscription partially applied")
	}
	if err := subs.Subscribe([]string{strings.Repeat("x", maxSubscriptionBytes+1)}); !errors.Is(err, ErrResourceLimit) {
		t.Fatal("unbounded subscription string accepted")
	}
}

func TestHistoryHasByteBudgetAndExpiresWithoutNewPublish(t *testing.T) {
	h := New()
	payload := strings.Repeat("x", 1024*1024)
	for i := 0; i < 20; i++ {
		h.Publish("prod", Message{"key": i, "body": payload})
	}
	h.mu.Lock()
	if h.historyBytes > historyByteLimit || len(h.history) >= 20 {
		t.Fatal("history byte budget not enforced")
	}
	for i := range h.history {
		h.history[i].timestamp = time.Now().Add(-historyAgeLimit - time.Second)
	}
	h.mu.Unlock()
	if got := h.GetHistory("prod", time.Time{}); len(got) != 0 {
		t.Fatal("expired history retained until another publish")
	}
	if h.Status("prod").HistoryAge != nil {
		t.Fatal("expired history age is not null")
	}
}
