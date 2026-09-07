// Package filestorage is the file storage domain: it takes the bytes of one
// file from Phorge and puts them in whichever configured backend accepts them,
// and hands them back on request. It owns no metadata — Phorge's own database
// holds the file's name, size, MIME type and the engine/handle pair that
// points here. See docs/modules/file-storage.md.
package filestorage

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/platform/auth"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// CodeNoEngine is this domain's only error code: no configured backend will
// take the file. It is a distinct code because it is the one write failure
// that is a *configuration* problem rather than a storage problem, and the one
// a Phorge setup check can act on. Every other failure collapses into the
// platform codes.
const CodeNoEngine = "ERR_NO_ENGINE"

// contentTypeBlob is what a successful read answers with, whatever the file
// actually is. This service does not store MIME types; Phorge does, and Phorge
// is what sets the Content-Type on the response it serves to a browser.
const contentTypeBlob = "application/octet-stream"

// Deps is everything the file storage routes need to serve a request.
type Deps struct {
	Router *Router
	Token  string
}

// RegisterRoutes mounts the file storage endpoints.
//
// One path carries three methods, and the handle travels as a query parameter
// rather than a path segment. Both follow from the handle format: a local disk
// handle is `ab/cd/{28 hex}` and an S3 key is `phabricator/ab/cd/{16 hex}`, so
// a handle contains slashes and cannot be a path segment without being
// escaped by every client that ever calls this service.
//
// The paths are named after the domain and must not change:
// PhabricatorGorgeFileStorageClient calls them as written.
func RegisterRoutes(e *echo.Echo, deps *Deps) {
	g := e.Group("/api/file")
	g.Use(auth.Token(deps.Token))

	g.POST("/blob", writeBlob(deps))
	g.GET("/blob", readBlob(deps))
	g.DELETE("/blob", deleteBlob(deps))
	g.GET("/engines", listEngines(deps))
}

// writeBlob stores a raw request body.
//
// The body is the file, not a JSON document carrying it. That is the whole
// reason this domain's contract differs from every other one in the
// repository: base64 inside JSON costs a third more bytes and forces both
// sides to hold the entire file in memory to encode and decode it.
func writeBlob(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		req := c.Request()

		// A body of unknown length cannot be streamed to S3, which needs the
		// length to sign the request, and cannot be size-checked against a
		// backend's limit before it is read. Refusing it is better than
		// silently buffering it: every real caller sends one, since both
		// Phorge's HTTPSFuture and curl set Content-Length for a body they
		// hold in memory or on disk.
		size := req.ContentLength
		if size < 0 {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest,
				"a Content-Length is required: this endpoint takes the file as the raw request body")
		}

		params := WriteParams{
			Name:     c.QueryParam("name"),
			MimeType: c.QueryParam("mimeType"),
		}

		if identifier := c.QueryParam("engine"); identifier != "" {
			eng, err := deps.Router.GetEngine(identifier)
			if err != nil {
				return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
			}
			// The limit is reported here rather than left to the engine so
			// that "too large for this backend" answers 413 like every other
			// size refusal in the repository, instead of a 500 the caller
			// cannot act on.
			if eng.HasSizeLimit() && size > eng.MaxFileSize() {
				return httpx.Fail(c, http.StatusRequestEntityTooLarge, httpx.CodeTooLarge,
					"file of "+strconv.FormatInt(size, 10)+" bytes exceeds the limit of engine "+
						identifier+" ("+strconv.FormatInt(eng.MaxFileSize(), 10)+" bytes)")
			}

			result, err := deps.Router.WriteTo(req.Context(), identifier, req.Body, size, params)
			if err != nil {
				return err
			}
			return httpx.OK(c, result)
		}

		result, err := deps.Router.Write(req.Context(), req.Body, size, params)
		if err != nil {
			if errors.Is(err, ErrNoEngine) {
				// 503 rather than 500: nothing was attempted and nothing is
				// broken. The deployment has no backend that will take a file
				// this size, which is a state an operator fixes by
				// configuring one.
				return httpx.Fail(c, http.StatusServiceUnavailable, CodeNoEngine, err.Error())
			}
			// Every backend failed. The platform error handler reports it as
			// 500 ERR_INTERNAL with a generic message, and the per-engine
			// errors are already in the log.
			return err
		}
		return httpx.OK(c, result)
	}
}

// readBlob answers with the file's bytes.
//
// **This is the only success response in `/api/**` that is not the
// `{data, error}` envelope.** A failure still is, so a client branches on the
// status code and only then looks at the body. The platform needs no change
// for this: httpx does not impose the envelope on a handler, and the error
// handler keeps producing it on the failure paths. See docs/platform.md
// section 1.1.
func readBlob(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		eng, handle, reason := resolveTarget(deps, c)
		if reason != "" {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, reason)
		}

		rc, size, readErr := eng.ReadFile(c.Request().Context(), handle)
		if readErr != nil {
			// Every read failure is reported as "not found", including a
			// malformed handle and a backend that is unreachable. The
			// distinction is not one the caller can act on — Phorge holds one
			// engine/handle pair per file and has nothing else to try — and
			// the details are in the log.
			return httpx.Fail(c, http.StatusNotFound, httpx.CodeNotFound, readErr.Error())
		}
		defer func() { _ = rc.Close() }()

		// A Content-Length is what lets the client tell a complete file from a
		// truncated one, and a zero-byte file is a legitimate answer: an empty
		// body with status 200. Without the header the response would be
		// chunked and the two would look alike.
		if size >= 0 {
			c.Response().Header().Set(echo.HeaderContentLength, strconv.FormatInt(size, 10))
		}
		return c.Stream(http.StatusOK, contentTypeBlob, rc)
	}
}

// deleteBlob removes the file's bytes.
//
// Deleting something that is already gone succeeds. Phorge deletes the bytes
// and the row that points at them in one sequence, so an error here would
// leave it unable to retire a row whose bytes it has already lost.
//
// That is also why this handler, unlike readBlob, separates a malformed
// handle from a backend failure: with "already gone" reported as success, a
// raw error is otherwise always a 500, and a caller sending a handle no
// engine could have minted would be told the service is broken.
func deleteBlob(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		eng, handle, reason := resolveTarget(deps, c)
		if reason != "" {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, reason)
		}

		if err := eng.DeleteFile(c.Request().Context(), handle); err != nil {
			if errors.Is(err, ErrBadHandle) {
				return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
			}
			// A real backend failure. 500 is honest here and it matters that
			// it stays one: Phorge must not retire a row while the bytes it
			// points at are still on disk.
			return err
		}
		return httpx.OK(c, &contracts.DeleteResult{Status: "deleted"})
	}
}

func listEngines(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		return httpx.OK(c, deps.Router.ListEngines())
	}
}

// resolveTarget validates the two query parameters the read and delete
// endpoints share. It reports the refusal as a message rather than answering
// the request itself, and returns an empty one when the request is good.
//
// It does not write the response, deliberately. A helper that answered and
// returned "the error" would be returning whatever httpx.Fail gave it — and
// that is nil when the write succeeded, which reads as "no problem" at the
// call site and lets the handler carry on with a nil engine.
//
// The engine is required, not inferred: handle formats overlap across
// backends, so guessing would sometimes read the wrong file rather than fail.
func resolveTarget(deps *Deps, c echo.Context) (StorageEngine, string, string) {
	identifier := c.QueryParam("engine")
	handle := c.QueryParam("handle")
	if identifier == "" || handle == "" {
		return nil, "", "both the engine and handle query parameters are required"
	}

	eng, err := deps.Router.GetEngine(identifier)
	if err != nil {
		return nil, "", err.Error()
	}
	return eng, handle, ""
}
