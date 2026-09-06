package notification

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"

	"github.com/soulteary/gorge/go/internal/notification/hub"
)

// useWebsocketsBody is what a plain HTTP request to the client port answers
// with, byte for byte as Aphlict wrote it.
const useWebsocketsBody = "HTTP/501 Use Websockets\n"

// defaultReplayAge, in milliseconds, bounds a replay request that names no age.
const defaultReplayAge = 60000

var upgrader = websocket.Upgrader{
	// Browsers reach this port from the Phorge origin, which is never this
	// server's own: the client port is a separate host and port by design, and
	// Phorge hands the browser its address explicitly. gorilla's default
	// same-origin check would reject every real connection. Nothing here is
	// authenticated and no credentials are read, so this is the posture Aphlict
	// had rather than a relaxation of one.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// ClientDeps is everything the client routes need.
type ClientDeps struct {
	Hub *hub.Hub
}

// RegisterClientRoutes mounts the client port's WebSocket endpoint.
//
// Two routes, because the instance travels in the path: Phorge's
// getWebsocketURI() appends ~{instance}/ when cluster.instance is set, so the
// wildcard catches those while / serves the single-instance case. Echo ranks
// static routes above the wildcard, so /healthz and /readyz keep answering
// underneath it. The one route that has to give way is the platform's GET /
// probe, which is what httpx.Config.SkipRootProbe exists for.
func RegisterClientRoutes(e *echo.Echo, deps *ClientDeps) {
	e.GET("/", serveClient(deps))
	e.GET("/*", serveClient(deps))
}

func serveClient(deps *ClientDeps) echo.HandlerFunc {
	return func(c echo.Context) error {
		req := c.Request()

		if !websocket.IsWebSocketUpgrade(req) {
			// A compatibility constraint, not an unimplemented endpoint: Phorge
			// probes this port with a plain GET / and reads 501 as the healthy
			// answer, reporting "Got HTTP 200, but expected HTTP 501" for
			// anything else (PhabricatorNotificationServerRef::testClient).
			return c.String(http.StatusNotImplemented, useWebsocketsBody)
		}

		conn, err := upgrader.Upgrade(c.Response(), req, nil)
		if err != nil {
			// Upgrade has already answered; there is nothing left to write.
			slog.Warn("websocket upgrade failed", "uri", req.RequestURI, "error", err)
			return nil
		}

		// The handshake went straight onto the hijacked connection, so
		// echo.Response never saw it and still believes nothing has been sent.
		// Marking it committed is what keeps the global error handler from
		// serialising an error envelope onto a connection that now belongs to
		// the WebSocket, which is what a panic in the read loop below would
		// otherwise cause. The status is set for the access log's benefit.
		c.Response().Committed = true
		c.Response().Status = http.StatusSwitchingProtocols

		instance := parseInstance(req.URL.Path)
		listener := hub.NewListener(deps.Hub.NextID(), conn)
		deps.Hub.AddListener(instance, listener)
		slog.Info("client connected",
			"listener", listener.ID(), "instance", instance, "remote", listener.RemoteAddr())

		defer func() {
			deps.Hub.RemoveListener(instance, listener.ID())
			listener.Close()
			slog.Info("client disconnected", "listener", listener.ID(), "instance", instance)
		}()

		readLoop(deps.Hub, listener)
		return nil
	}
}

// readLoop serves a client's commands until it goes away, which is the whole
// lifetime of the connection and therefore of the request handler.
//
// Malformed frames and unknown commands are skipped rather than reported.
// Aphlict answered nothing either, and a browser has no way to act on a
// protocol complaint; the alternative would be closing connections over a
// version skew between the JS client and this service.
func readLoop(h *hub.Hub, l *hub.Listener) {
	for {
		data, err := l.ReadMessage()
		if err != nil {
			return
		}

		var cmd struct {
			Command string          `json:"command"`
			Data    json.RawMessage `json:"data"`
		}
		if json.Unmarshal(data, &cmd) != nil {
			continue
		}

		switch cmd.Command {
		case "subscribe":
			var phids []string
			if json.Unmarshal(cmd.Data, &phids) == nil {
				l.Subscribe(phids)
			}

		case "unsubscribe":
			var phids []string
			if json.Unmarshal(cmd.Data, &phids) == nil {
				l.Unsubscribe(phids)
			}

		case "replay":
			if err := replay(h, l, cmd.Data); err != nil {
				return
			}

		case "ping":
			_ = l.WriteJSON(map[string]string{"type": "pong"})
		}
	}
}

// replay re-sends the recent history a client missed while it was away, filtered
// by what it has subscribed to. A write failure ends the session rather than
// being skipped: the client is already gone, and every remaining message would
// fail the same way.
func replay(h *hub.Hub, l *hub.Listener, data json.RawMessage) error {
	var opts struct {
		Age int64 `json:"age"`
	}
	if json.Unmarshal(data, &opts) != nil || opts.Age == 0 {
		opts.Age = defaultReplayAge
	}

	minAge := time.Now().Add(-time.Duration(opts.Age) * time.Millisecond)
	for _, msg := range h.GetHistory(minAge) {
		subscribers, _ := hub.ToStringSlice(msg["subscribers"])
		if len(subscribers) > 0 && !l.IsSubscribedToAny(subscribers) {
			continue
		}
		if err := l.WriteJSON(msg); err != nil {
			return err
		}
	}
	return nil
}

// parseInstance reads the Phorge instance out of the request path, where
// getWebsocketURI() encodes it as ~{instance}/. Anything else, a bare ~
// included, is the unnamed default instance.
func parseInstance(path string) string {
	idx := strings.Index(path, "~")
	if idx == -1 {
		return defaultInstance
	}

	instance := strings.Trim(path[idx+1:], "/")
	if instance == "" {
		return defaultInstance
	}
	return instance
}
