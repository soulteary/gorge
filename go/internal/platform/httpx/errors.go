package httpx

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"
)

// internalMessage is what every 5xx this file writes reports, whatever went
// wrong. Panic values, stack traces and internal error strings belong in the
// log, not in a response body. A handler answering deliberately through Fail
// keeps its own wording.
const internalMessage = "internal server error"

// statusCodes maps HTTP statuses onto the envelope's codes.
//
// A status reports the same code whether Echo or a handler produced it. 413 is
// the case that actually happens: the render domain enforces its own
// GORGE_RENDER_MAX_BYTES, but a value configured above Config.BodyLimit lets
// the body-limit middleware answer first, and clients must not have to tell the
// two apart.
var statusCodes = map[int]string{
	http.StatusBadRequest:            CodeBadRequest,
	http.StatusUnauthorized:          CodeUnauthorized,
	http.StatusNotFound:              CodeNotFound,
	http.StatusMethodNotAllowed:      CodeMethodNotAllowed,
	http.StatusRequestEntityTooLarge: CodeTooLarge,
}

// errorHandler replaces Echo's default handler, which answers a bare
// {"message": "..."} body. Without it every failure the framework raises before
// or around a handler — an unknown path, a body over the limit, a recovered
// panic — would escape the {data, error} envelope that /api/** promises.
//
// The health probes are unaffected: they answer 200 or 503 from their own
// handlers and never reach this function, so they keep the flat
// {"status": "..."} shape orchestrators are configured against.
func errorHandler(err error, c echo.Context) {
	status, code, message := classify(err)

	if status >= http.StatusInternalServerError {
		slog.Error("REQUEST_FAILED",
			"method", c.Request().Method,
			"uri", c.Request().RequestURI,
			"status", status,
			"request_id", c.Response().Header().Get(echo.HeaderXRequestID),
			"error", err.Error())
	}

	// A handler that already answered owns its response. This is what keeps the
	// render domain's ERR_HIGHLIGHT_FAILED intact: every httpx.Fail commits the
	// response, so a later error cannot replace the domain code with a generic
	// one, nor append a second JSON document to the body.
	if c.Response().Committed {
		return
	}

	var writeErr error
	if c.Request().Method == http.MethodHead {
		// A HEAD response carries no body, so only the status is meaningful.
		writeErr = c.NoContent(status)
	} else {
		writeErr = c.JSON(status, &Response{Error: &Error{Code: code, Message: message}})
	}
	if writeErr != nil {
		slog.Error("failed to write error response", "error", writeErr)
	}
}

// classify turns an arbitrary handler or middleware error into the status, code
// and client-visible message of an error envelope.
func classify(err error) (int, string, string) {
	var httpErr *echo.HTTPError
	if !errors.As(err, &httpErr) {
		// A recovered panic, or a handler returning a raw error. Neither has a
		// status of its own and neither is safe to describe to the caller.
		return http.StatusInternalServerError, CodeInternal, internalMessage
	}

	status := httpErr.Code
	if status >= http.StatusInternalServerError {
		return status, CodeInternal, internalMessage
	}

	code, mapped := statusCodes[status]
	if !mapped {
		// Some other 4xx the framework grew or a handler chose. It is still the
		// caller's request at fault, which is all ERR_BAD_REQUEST claims;
		// minting a code per status would grow the enum clients switch on for
		// statuses this service never returns.
		code = CodeBadRequest
	}

	// Echo's own 4xx messages are plain status text ("Not Found", "Request
	// Entity Too Large") and carry nothing internal.
	if message, ok := httpErr.Message.(string); ok && message != "" {
		return status, code, message
	}
	return status, code, http.StatusText(status)
}
