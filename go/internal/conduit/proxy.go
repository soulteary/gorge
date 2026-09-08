package conduit

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// Proxy relays a Conduit call to the upstream Phorge PHP app. It holds a
// standard-library http.Client rather than a Fiber client because the gateway
// speaks to a plain HTTP upstream and needs the ordinary redirect and timeout
// controls; nothing about the relay is Fiber-specific.
type Proxy struct {
	upstream string
	client   *http.Client
}

// NewProxy builds a proxy pointed at upstreamURL, with each upstream request
// bounded by timeoutSec. A non-positive timeout leaves the client without one,
// matching http.Client's zero-value behaviour.
func NewProxy(upstreamURL string, timeoutSec int) *Proxy {
	var timeout time.Duration
	if timeoutSec > 0 {
		timeout = time.Duration(timeoutSec) * time.Second
	}
	return &Proxy{
		upstream: strings.TrimRight(upstreamURL, "/"),
		client: &http.Client{
			Timeout: timeout,
			// Relay the upstream's own 3xx to the caller instead of chasing it:
			// a Conduit redirect is part of the response the client parses.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Handle relays the current request to `{upstream}/api/{method}` and copies the
// upstream response back verbatim. It is the Fiber-side rewrite of the Echo
// proxy: same target, same header discipline, same Conduit error envelopes.
//
// On any failure it answers a Conduit-protocol error envelope directly rather
// than through httpx, because a client parsing this endpoint expects the
// Conduit shape on both success and failure; see internal/contracts/conduit.go.
func (p *Proxy) Handle(c fiber.Ctx) error {
	method := c.Params("method")
	if method == "" {
		// Reached only if a route ever mounts this without the :method segment;
		// the group in http.go always supplies one. Kept as defence in depth so
		// a mis-mount fails as ERR-CONDUIT-CORE rather than a nil relay.
		return conduitError(c, http.StatusBadRequest, contracts.CodeConduitCore,
			"No Conduit method specified in URI.")
	}

	targetURL := fmt.Sprintf("%s/api/%s", p.upstream, method)
	if rawQ := c.Request().URI().QueryString(); len(rawQ) > 0 {
		targetURL += "?" + string(rawQ)
	}
	start := time.Now()

	// fasthttp fully buffers the request body, so c.Body() is the whole payload
	// with no copy; a bytes.Reader hands it to net/http without re-reading the
	// socket. c.Context() carries the request's cancellation to the upstream.
	proxyReq, err := http.NewRequestWithContext(c.Context(), c.Method(), targetURL, bytes.NewReader(c.Body()))
	if err != nil {
		slog.Error("CONDUIT_PROXY_BUILD", "method", method, "error", err.Error())
		return conduitError(c, http.StatusBadGateway, contracts.CodeConduitProxy,
			"Failed to construct upstream request.")
	}

	copyRequestHeaders(c, proxyReq.Header)
	proxyReq.Header.Set("X-Forwarded-For", c.IP())
	proxyReq.Header.Set("X-Forwarded-Proto", c.Protocol())
	proxyReq.Header.Set("X-Conduit-Gateway", "go-conduit")

	resp, err := p.client.Do(proxyReq)
	if err != nil {
		slog.Error("CONDUIT_UPSTREAM", "method", method,
			"duration_ms", time.Since(start).Milliseconds(), "error", err.Error())
		return conduitError(c, http.StatusBadGateway, contracts.CodeConduitProxy,
			"Upstream request failed.")
	}
	defer func() { _ = resp.Body.Close() }()

	// Buffer the upstream body rather than stream it. A Conduit reply is a small
	// JSON document, so the cost is negligible, and buffering avoids the reader
	// lifecycle trap SendStream carries: fasthttp reads a streamed body after
	// the handler returns, so closing resp.Body here (which we must, to release
	// the connection) would race that read and truncate the reply. Send takes
	// the bytes now, while the body is still open, and relays them verbatim.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("CONDUIT_UPSTREAM_READ", "method", method, "error", err.Error())
		return conduitError(c, http.StatusBadGateway, contracts.CodeConduitProxy,
			"Reading upstream response failed.")
	}

	slog.Info("CONDUIT_RELAY", "method", method, "status", resp.StatusCode,
		"duration_ms", time.Since(start).Milliseconds(), "client", c.IP())

	copyResponseHeaders(resp.Header, c)
	c.Status(resp.StatusCode)
	// Send relays the upstream bytes unchanged, so the Conduit envelope reaches
	// the caller byte-for-byte.
	return c.Send(body)
}

// hopByHopHeaders are connection-scoped and must not be forwarded across a
// proxy hop; see RFC 7230 §6.1.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// copyRequestHeaders copies the caller's headers onto the upstream request,
// dropping the hop-by-hop set. It reads fasthttp's request header map directly
// because Fiber has no net/http request to hand over.
func copyRequestHeaders(c fiber.Ctx, dst http.Header) {
	c.Request().Header.VisitAll(func(key, value []byte) {
		k := string(key)
		if hopByHopHeaders[http.CanonicalHeaderKey(k)] {
			return
		}
		dst.Add(k, string(value))
	})
}

// copyResponseHeaders copies the upstream response headers onto the Fiber
// response. The hop-by-hop set is dropped here too, so a keep-alive or
// transfer-encoding the upstream chose does not leak into the relayed reply and
// fight fasthttp's own framing.
func copyResponseHeaders(src http.Header, c fiber.Ctx) {
	for k, values := range src {
		if hopByHopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range values {
			c.Response().Header.Add(k, v)
		}
	}
}

// conduitError writes a Conduit-protocol error envelope with the given status.
// It is the gateway's stand-in for httpx.Fail, which would emit {data, error}.
func conduitError(c fiber.Ctx, status int, code, info string) error {
	return c.Status(status).JSON(contracts.NewConduitError(code, info))
}
