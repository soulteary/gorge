// Package render is the rendering domain: it turns source artifacts into HTML
// for Phorge, which today means syntax highlighting. The diff domain shares
// this binary under /api/diff/* but is a package of its own; see
// docs/modules/render.md and docs/modules/diff.md.
package render

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/platform/auth"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/render/highlight"
)

// CodeHighlightFailed is domain-specific and predates the monorepo. Phorge's
// client already branches on it, so it stays instead of collapsing into
// httpx.CodeInternal.
const CodeHighlightFailed = "ERR_HIGHLIGHT_FAILED"

// Deps is everything the render routes need to serve a request.
type Deps struct {
	Highlighter *highlight.Highlighter
	Token       string
	MaxBytes    int
}

// RegisterRoutes mounts the render endpoints.
//
// The paths are named after the domain, not the binary, and must not change:
// Phorge's PhabricatorGoHighlightClient already calls them, and keeping the
// domain segment is what lets /api/diff/* land in this same process later.
func RegisterRoutes(e *echo.Echo, deps *Deps) {
	g := e.Group("/api/highlight")
	g.Use(auth.Token(deps.Token))

	g.POST("/render", renderHighlight(deps))
	g.GET("/languages", listLanguages(deps))
}

func renderHighlight(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		var req contracts.HighlightRequest
		if err := c.Bind(&req); err != nil {
			// A body over the platform limit surfaces here, as Echo's 413,
			// whenever the client streams without a Content-Length; handing
			// those back to the platform error handler is what keeps them
			// reported as ERR_TOO_LARGE rather than flattened into
			// ERR_BAD_REQUEST. Malformed JSON is a genuine 400 and stays here.
			var httpErr *echo.HTTPError
			if errors.As(err, &httpErr) && httpErr.Code != http.StatusBadRequest {
				return err
			}
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		}

		if req.Source == "" {
			return httpx.OK(c, &contracts.HighlightResult{HTML: "", Language: req.Language})
		}

		if deps.MaxBytes > 0 && len(req.Source) > deps.MaxBytes {
			return httpx.Fail(c, http.StatusRequestEntityTooLarge,
				httpx.CodeTooLarge, "source exceeds maximum allowed size")
		}

		result, err := deps.Highlighter.Highlight(req.Source, req.Language)
		if err != nil {
			return httpx.Fail(c, http.StatusInternalServerError, CodeHighlightFailed, err.Error())
		}

		return httpx.OK(c, result)
	}
}

func listLanguages(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		return httpx.OK(c, deps.Highlighter.Languages())
	}
}
