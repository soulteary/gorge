// Package notification is the real-time notification domain: it replaces
// Phorge's Aphlict server, accepting messages from PHP on an admin port and
// fanning them out to browsers over WebSocket on a client port. The two ports
// cannot be merged; see docs/modules/notification.md and compat/phorge/README.md
// for why, and for the two compatibility constraints that shape this package.
package notification

import (
	"encoding/json"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/notification/hub"
	"github.com/soulteary/gorge/go/internal/notification/peer"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// defaultInstance is the instance a request without an ?instance= parameter
// belongs to. Phorge only sends the parameter when cluster.instance is set.
const defaultInstance = "default"

// AdminDeps is everything the admin routes need.
type AdminDeps struct {
	Hub   *hub.Hub
	Peers *peer.List
}

// RegisterAdminRoutes mounts the admin port's endpoints.
//
// There is no auth.Token middleware here, and that is deliberate: Phorge's
// PhabricatorNotificationServerRef sends no credentials, so a token would reject
// every message it posts. This port must therefore stay on a private network —
// in the compose topology it is not published to the host at all.
func RegisterAdminRoutes(e *echo.Echo, deps *AdminDeps) {
	// POST / coexists with the health probe's GET /: they differ by method. The
	// visible consequence is that GET / now answers 200 where Aphlict answered
	// 405, which no caller notices because Phorge only ever probes /status/.
	e.POST("/", postMessage(deps))
	e.GET("/status/", serverStatus(deps))
}

func postMessage(deps *AdminDeps) echo.HandlerFunc {
	return func(c echo.Context) error {
		// Decoded straight off the body rather than through c.Bind, which
		// dispatches on Content-Type. Phorge's HTTPSFuture sends a raw JSON
		// payload under curl's default application/x-www-form-urlencoded, and
		// Echo's binder would take that label at its word: it percent-decodes
		// and splits the body as form data, rejecting a message that contains a
		// stray "%" and quietly turning any other one into a single garbage key.
		// Another label, text/plain say, would be a flat 415. Pinned by
		// tests/contract/notification/admin/post-form-content-type.json.
		//
		// An empty body decodes to io.EOF, which is the 400 below.
		var msg hub.Message
		if err := json.NewDecoder(c.Request().Body).Decode(&msg); err != nil {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		}

		instance := instanceOf(c)
		receipt := &contracts.AphlictReceipt{Fingerprint: deps.Peers.Fingerprint()}

		// A message already carrying our fingerprint has been round the cluster
		// and come back. Answering the receipt without republishing is what
		// stops a peer mesh from delivering it twice and relaying it forever.
		if !deps.Peers.AddFingerprint(msg) {
			return c.JSON(http.StatusOK, receipt)
		}

		deps.Hub.Publish(instance, msg)
		deps.Peers.BroadcastMessage(instance, msg)

		// c.JSON, not httpx.OK: Phorge reads this body with a bare
		// phutil_json_decode() and indexes "fingerprint" directly, so the
		// envelope would put it out of reach. See contracts/notification.go.
		return c.JSON(http.StatusOK, receipt)
	}
}

func serverStatus(deps *AdminDeps) echo.HandlerFunc {
	return func(c echo.Context) error {
		// Bare again, for the same reason: the cluster notification panel reads
		// these keys off the top level of the body.
		return c.JSON(http.StatusOK, deps.Hub.Status(instanceOf(c)))
	}
}

// instanceOf reads the Phorge instance a request addresses.
func instanceOf(c echo.Context) string {
	if instance := c.QueryParam("instance"); instance != "" {
		return instance
	}
	return defaultInstance
}
