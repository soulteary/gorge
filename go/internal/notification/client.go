package notification

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/contrib/v3/websocket"
	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/notification/hub"
)

// useWebsocketsBody is what a plain HTTP request to the client port answers
// with, byte for byte as Aphlict wrote it.
const useWebsocketsBody = "HTTP/501 Use Websockets\n"

// defaultReplayAge, in milliseconds, bounds a replay request that names no age.
const defaultReplayAge = 60000

// ClientDeps is everything the client routes need.
type ClientDeps struct {
	Hub *hub.Hub
}

// RegisterClientRoutes mounts the client port's WebSocket endpoint.
//
// Two routes, because the instance travels in the path: Phorge's
// getWebsocketURI() appends ~{instance}/ when cluster.instance is set, so the
// wildcard catches those while / serves the single-instance case. Fiber ranks
// static routes above the wildcard, so /healthz and /readyz keep answering
// underneath it. The one route that has to give way is the platform's GET /
// probe, which is what httpx.Config.SkipRootProbe exists for.
func RegisterClientRoutes(app fiber.Router, deps *ClientDeps) {
	h := serveClient(deps)
	app.Get("/", h)
	app.Get("/*", h)
}

// serveClient is the client-port handler. It answers a plain GET with 501 and
// upgrades a WebSocket request onto the hub.
//
// The contrib upgrader is built once and only invoked after the upgrade check,
// so the non-WebSocket 501 path — the one Phorge probes — never touches it and
// keeps answering with Aphlict's exact body rather than the upgrader's 426.
func serveClient(deps *ClientDeps) fiber.Handler {
	// Origins is left empty so every origin is accepted, and AllowEmptyOrigin is
	// true for non-browser clients: this reproduces gorilla's CheckOrigin=>true
	// posture Aphlict had. Nothing here is authenticated.
	upgrade := websocket.New(func(c *websocket.Conn) {
		// The instance was resolved from the request path in the outer handler
		// and stashed in Locals, which the contrib Conn copies off the
		// fiber.Ctx before fasthttp recycles it. Reading c.Params("*") here is
		// unreliable across the hijack, so the path is parsed while the
		// fiber.Ctx is still live.
		instance, _ := c.Locals(localInstance).(string)
		if instance == "" {
			instance = defaultInstance
		}
		listener := hub.NewListener(deps.Hub.NextID(), c.Conn)
		deps.Hub.AddListener(instance, listener)
		slog.Info("client connected",
			"listener", listener.ID(), "instance", instance, "remote", listener.RemoteAddr())

		defer func() {
			deps.Hub.RemoveListener(instance, listener.ID())
			listener.Close()
			slog.Info("client disconnected", "listener", listener.ID(), "instance", instance)
		}()

		readLoop(deps.Hub, listener)
	}, websocket.Config{AllowEmptyOrigin: true})

	return func(c fiber.Ctx) error {
		if !websocket.IsWebSocketUpgrade(c) {
			// A compatibility constraint, not an unimplemented endpoint: Phorge
			// probes this port with a plain GET / and reads 501 as the healthy
			// answer, reporting "Got HTTP 200, but expected HTTP 501" for
			// anything else (PhabricatorNotificationServerRef::testClient).
			//
			// SendString returns nil after writing this compatibility response, so
			// the platform error handler is never invoked to re-envelope it.
			return c.Status(http.StatusNotImplemented).SendString(useWebsocketsBody)
		}
		// The instance travels in the request path (Phorge's getWebsocketURI
		// encodes it as ~{instance}/). Resolve it here, while the fiber.Ctx is
		// live, and hand it to the hijacked handler through Locals.
		c.Locals(localInstance, parseInstance(c.Path()))
		// The handshake is hijacked by the upgrader, which owns the connection
		// for its whole lifetime. On a rejected handshake it returns a
		// *fiber.Error that the platform error handler answers; on a clean
		// upgrade it returns nil after the read loop ends. Either way the
		// response was written on the hijacked connection, not through httpx.
		return upgrade(c)
	}
}

// localInstance is the Locals key the client-port handler uses to carry the
// resolved Phorge instance from the live request into the hijacked WebSocket
// handler.
const localInstance = "notification_instance"

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
