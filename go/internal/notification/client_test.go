package notification

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"

	"github.com/soulteary/gorge/go/internal/notification/hub"
	"github.com/soulteary/gorge/go/internal/notification/peer"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// newClientEcho builds the client port the way cmd/gorge-notification does,
// SkipRootProbe included: without it the platform's GET / probe would shadow the
// 501 Phorge requires, which is the whole reason that option exists.
func newClientEcho(t *testing.T, messages *hub.Hub) *echo.Echo {
	t.Helper()
	silenceLogs(t)

	e := httpx.New(httpx.Config{SkipRootProbe: true}).Echo()
	RegisterClientRoutes(e, &ClientDeps{Hub: messages})
	return e
}

// newClientServer serves the client port over a real listener, which WebSocket
// tests need: httptest.NewRecorder cannot be hijacked.
func newClientServer(t *testing.T) (string, *hub.Hub) {
	t.Helper()

	messages := hub.New()
	srv := httptest.NewServer(newClientEcho(t, messages))
	t.Cleanup(srv.Close)
	return srv.URL, messages
}

func dialTo(t *testing.T, baseURL, path string) *websocket.Conn {
	t.Helper()

	wsURL := "ws" + strings.TrimPrefix(baseURL, "http") + path
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", wsURL, err)
	}
	_ = resp.Body.Close()
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func sendCommand(t *testing.T, conn *websocket.Conn, command string, data any) {
	t.Helper()

	if err := conn.WriteJSON(map[string]any{"command": command, "data": data}); err != nil {
		t.Fatalf("%s: %v", command, err)
	}
}

// syncCommands blocks until the read loop has worked through everything sent so
// far. Commands are handled in order, so a pong can only come back after the
// preceding subscribe has been applied; that makes these tests deterministic
// rather than racing a sleep against the server.
func syncCommands(t *testing.T, conn *websocket.Conn) {
	t.Helper()

	sendCommand(t, conn, "ping", nil)
	if got := readMessage(t, conn); got["type"] != "pong" {
		t.Fatalf("expected a pong, got %v", got)
	}
}

func readMessage(t *testing.T, conn *websocket.Conn) hub.Message {
	t.Helper()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var msg hub.Message
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("read: %v", err)
	}
	return msg
}

// expectFirstMessage asserts which message arrives first, and is how the
// filtering tests show they filter rather than merely deliver: the message that
// must be skipped is published first, so a broken filter is read here instead of
// the expected one. That is both stronger and steadier than waiting out a
// timeout to prove a non-delivery — a gorilla connection is unusable after a read
// deadline expires, so a test could not carry on afterwards anyway.
func expectFirstMessage(t *testing.T, conn *websocket.Conn, key string) {
	t.Helper()

	if got := readMessage(t, conn); got["key"] != key {
		t.Errorf("expected the first message delivered to be %q, got %v", key, got)
	}
}

// TestPlainRequestsGetTheAphlictRefusal is compatibility constraint two, and the
// reason httpx.Config.SkipRootProbe exists. Phorge reads 501 from this port as
// the healthy answer and reports a 200 as a broken server, so both the status and
// the body are pinned byte for byte.
func TestPlainRequestsGetTheAphlictRefusal(t *testing.T) {
	e := newClientEcho(t, hub.New())

	// Every path the wildcard route covers, since Phorge probes / but browsers
	// arrive on the instance paths.
	for _, path := range []string{"/", "/~prod/", "/anything", "/deeply/nested/path"} {
		rec := getFrom(e, path)

		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s: expected 501, got %d", path, rec.Code)
		}
		if rec.Body.String() != useWebsocketsBody {
			t.Errorf("%s: body = %q, want %q", path, rec.Body.String(), useWebsocketsBody)
		}
	}
}

// TestClientProbesOutrankTheWildcard checks the routing assumption
// RegisterClientRoutes rests on: GET /* must not swallow the container probes,
// which is what keeps the Docker HEALTHCHECK working on this port.
func TestClientProbesOutrankTheWildcard(t *testing.T) {
	e := newClientEcho(t, hub.New())

	for _, path := range []string{"/healthz", "/readyz"} {
		rec := getFrom(e, path)

		if rec.Code != http.StatusOK {
			t.Errorf("%s: expected 200, got %d (%s)", path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
			t.Errorf("%s: expected the probe response, got %q", path, rec.Body.String())
		}
	}
}

func TestWebSocketPingAnswersPong(t *testing.T) {
	baseURL, _ := newClientServer(t)
	conn := dialTo(t, baseURL, "/")

	syncCommands(t, conn)
}

// TestUpgradedResponseIsMarkedCommitted pins the flag the global error handler
// reads. gorilla writes the 101 handshake straight onto the hijacked connection,
// so echo.Response never learns a response was sent; leaving Committed false
// would let a panic anywhere in the read loop hand the error handler a hijacked
// writer to serialise a JSON envelope onto.
func TestUpgradedResponseIsMarkedCommitted(t *testing.T) {
	messages := hub.New()
	e := newClientEcho(t, messages)

	committed := make(chan bool, 1)
	status := make(chan int, 1)
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			err := next(c)
			committed <- c.Response().Committed
			status <- c.Response().Status
			return err
		}
	})

	srv := httptest.NewServer(e)
	defer srv.Close()

	conn := dialTo(t, srv.URL, "/")
	syncCommands(t, conn)
	// Ending the session returns the handler, which releases the middleware.
	_ = conn.Close()

	select {
	case got := <-committed:
		if !got {
			t.Error("an upgraded response must be marked committed, or the error handler will write to a hijacked connection")
		}
		if got := <-status; got != http.StatusSwitchingProtocols {
			t.Errorf("status = %d, want %d for the access log", got, http.StatusSwitchingProtocols)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never returned")
	}
}

func TestSubscribedListenerReceivesItsMessages(t *testing.T) {
	baseURL, messages := newClientServer(t)
	conn := dialTo(t, baseURL, "/")

	sendCommand(t, conn, "subscribe", []string{"PHID-USER-test"})
	syncCommands(t, conn)

	messages.Publish(defaultInstance, hub.Message{
		"type":        "notification",
		"key":         "42",
		"subscribers": []string{"PHID-USER-test"},
	})

	if got := readMessage(t, conn); got["key"] != "42" {
		t.Errorf("expected key=42, got %v", got)
	}
}

// TestUnsubscribedListenerIsSkipped is the filter that keeps one user's
// notifications from reaching another's browser.
func TestUnsubscribedListenerIsSkipped(t *testing.T) {
	baseURL, messages := newClientServer(t)
	conn := dialTo(t, baseURL, "/")

	sendCommand(t, conn, "subscribe", []string{"PHID-USER-mine"})
	syncCommands(t, conn)

	messages.Publish(defaultInstance, hub.Message{
		"type":        "notification",
		"key":         "not-for-me",
		"subscribers": []string{"PHID-USER-somebody-else"},
	})
	messages.Publish(defaultInstance, hub.Message{
		"type":        "notification",
		"key":         "for-me",
		"subscribers": []string{"PHID-USER-mine"},
	})

	expectFirstMessage(t, conn, "for-me")
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	baseURL, messages := newClientServer(t)
	conn := dialTo(t, baseURL, "/")

	sendCommand(t, conn, "subscribe", []string{"PHID-USER-dropped", "PHID-USER-kept"})
	sendCommand(t, conn, "unsubscribe", []string{"PHID-USER-dropped"})
	syncCommands(t, conn)

	messages.Publish(defaultInstance, hub.Message{
		"type":        "notification",
		"key":         "dropped",
		"subscribers": []string{"PHID-USER-dropped"},
	})
	messages.Publish(defaultInstance, hub.Message{
		"type":        "notification",
		"key":         "kept",
		"subscribers": []string{"PHID-USER-kept"},
	})

	expectFirstMessage(t, conn, "kept")
}

// TestMessageWithoutSubscribersReachesEveryone covers the Aphlict broadcast: an
// unaddressed message goes to every listener of the instance.
func TestMessageWithoutSubscribersReachesEveryone(t *testing.T) {
	baseURL, messages := newClientServer(t)
	conn := dialTo(t, baseURL, "/")

	syncCommands(t, conn)
	messages.Publish(defaultInstance, hub.Message{"type": "notification", "key": "broadcast"})

	if got := readMessage(t, conn); got["key"] != "broadcast" {
		t.Errorf("expected the broadcast, got %v", got)
	}
}

// TestReplayDeliversHeldMessages covers what a browser does after a reconnect:
// ask for the recent history rather than miss whatever arrived while it was away.
func TestReplayDeliversHeldMessages(t *testing.T) {
	baseURL, messages := newClientServer(t)

	messages.Publish(defaultInstance, hub.Message{"type": "notification", "key": "earlier"})

	conn := dialTo(t, baseURL, "/")
	sendCommand(t, conn, "replay", map[string]any{"age": 60000})

	if got := readMessage(t, conn); got["key"] != "earlier" {
		t.Errorf("expected the held message, got %v", got)
	}
}

// TestReplayHonoursSubscriptions keeps replay under the same filter as live
// delivery; otherwise a reconnect would leak other users' notifications.
func TestReplayHonoursSubscriptions(t *testing.T) {
	baseURL, messages := newClientServer(t)

	messages.Publish(defaultInstance, hub.Message{
		"type":        "notification",
		"key":         "not-for-me",
		"subscribers": []string{"PHID-USER-somebody-else"},
	})
	messages.Publish(defaultInstance, hub.Message{
		"type":        "notification",
		"key":         "for-me",
		"subscribers": []string{"PHID-USER-mine"},
	})

	conn := dialTo(t, baseURL, "/")
	sendCommand(t, conn, "subscribe", []string{"PHID-USER-mine"})
	sendCommand(t, conn, "replay", nil)

	expectFirstMessage(t, conn, "for-me")
}

// TestUnknownCommandsAreIgnored pins the deliberate silence: a version skew
// between Phorge's JS client and this service must not close connections.
func TestUnknownCommandsAreIgnored(t *testing.T) {
	baseURL, _ := newClientServer(t)
	conn := dialTo(t, baseURL, "/")

	sendCommand(t, conn, "vacuum-the-carpet", nil)
	if err := conn.WriteMessage(websocket.TextMessage, []byte("{not json")); err != nil {
		t.Fatal(err)
	}

	// The session survives both, which the pong proves.
	syncCommands(t, conn)
}

// TestMessagePostedToAdminReachesASubscribedBrowser is the product behaviour end
// to end, across both ports and the shared hub that is the reason they live in
// one process. It also pins the instance routing: the instance travels in the
// path on the client side and in a query parameter on the admin side, and the two
// have to agree or notifications go nowhere.
//
// It is deliberately also the one place where both halves of the Content-Type
// constraint are held by a single assertion. The posts go through postAsPhorge,
// so the label is the one Phorge really sends, and the message is read back off
// a WebSocket field by field rather than counted, so both ways a binder can
// break this are caught here: rejecting the illegal percent escape outright, or
// accepting the body and quietly form-parsing it into keys that no longer
// include the ones asserted below. Asserting the label and the content
// separately, which is what the two tests either side of this one do, leaves
// exactly the real failure uncovered — see compat/phorge/README.md section 5.4.
func TestMessagePostedToAdminReachesASubscribedBrowser(t *testing.T) {
	messages := hub.New()

	clientSrv := httptest.NewServer(newClientEcho(t, messages))
	defer clientSrv.Close()

	adminEcho := httpx.New(httpx.Config{}).Echo()
	RegisterAdminRoutes(adminEcho, &AdminDeps{Hub: messages, Peers: peer.NewList()})

	conn := dialTo(t, clientSrv.URL, "/~prod/")
	sendCommand(t, conn, "subscribe", []string{"PHID-USER-x"})
	syncCommands(t, conn)

	// Posted first, and to the instance this browser is not on: if the instance
	// did not survive the trip across both ports, this is what would arrive.
	if rec := postAsPhorge(adminEcho, "/?instance=staging",
		`{"type":"notification","key":"wrong-instance","subscribers":["PHID-USER-x"]}`); rec.Code != http.StatusOK {
		t.Fatalf("staging post: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := postAsPhorge(adminEcho, "/?instance=prod",
		`{"type":"notification","key":"right-instance","title":"`+illegalEscapeText+
			`","subscribers":["PHID-USER-x"]}`); rec.Code != http.StatusOK {
		t.Fatalf("prod post: expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	// Read field by field rather than through expectFirstMessage: which message
	// arrives first is the instance-routing half, and the fields it arrives with
	// are the Content-Type half.
	got := readMessage(t, conn)
	if got["key"] != "right-instance" {
		t.Errorf("expected the first message delivered to be right-instance, got %v", got)
	}
	if got["title"] != illegalEscapeText {
		t.Errorf("title = %v, want %q: the posted body must reach the browser as written",
			got["title"], illegalEscapeText)
	}
}

// TestDisconnectFreesTheListener checks the bookkeeping the status page reports:
// a closed connection must stop counting as an active client.
func TestDisconnectFreesTheListener(t *testing.T) {
	baseURL, messages := newClientServer(t)

	conn := dialTo(t, baseURL, "/")
	syncCommands(t, conn)

	if active := messages.Status(defaultInstance).ClientsActive; active != 1 {
		t.Fatalf("expected 1 active client, got %d", active)
	}

	_ = conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status := messages.Status(defaultInstance)
		if status.ClientsActive == 0 {
			if status.ClientsTotal != 1 {
				t.Errorf("clients.total should still count the session, got %d", status.ClientsTotal)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("the listener was never released after the client disconnected")
}

func TestParseInstance(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/", defaultInstance},
		{"/~prod/", "prod"},
		{"/~staging", "staging"},
		{"/some/path", defaultInstance},
		{"/~", defaultInstance},
		{"/~/", defaultInstance},
	}
	for _, tt := range tests {
		if got := parseInstance(tt.path); got != tt.want {
			t.Errorf("parseInstance(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}
