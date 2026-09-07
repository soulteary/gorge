package httpx

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gofiber/fiber/v3"
)

// internalMessage is what every 5xx this file writes reports, whatever went
// wrong. Panic values, stack traces and internal error strings belong in the
// log, not in a response body. A handler answering deliberately through Fail
// keeps its own wording.
const internalMessage = "internal server error"

// statusCodes maps HTTP statuses onto the envelope's codes.
//
// A status reports the same code whether Fiber or a handler produced it. 413 is
// the case that actually happens: the render domain enforces its own
// GORGE_RENDER_MAX_BYTES, but a value configured above Config.BodyLimit lets
// fasthttp's body-limit answer first, and clients must not have to tell the two
// apart.
var statusCodes = map[int]string{
	http.StatusBadRequest:            CodeBadRequest,
	http.StatusUnauthorized:          CodeUnauthorized,
	http.StatusNotFound:              CodeNotFound,
	http.StatusMethodNotAllowed:      CodeMethodNotAllowed,
	http.StatusRequestEntityTooLarge: CodeTooLarge,
}

// errorHandler replaces Fiber's default handler, which answers a bare
// text/plain status message. Without it every failure the framework raises
// before or around a handler — an unknown path, a body over the limit, a
// recovered panic — would escape the {data, error} envelope that /api/**
// promises.
//
// The health probes are unaffected: they answer 200 or 503 from their own
// handlers with a nil error and never reach this function, so they keep the
// flat {"status": "..."} shape orchestrators are configured against.
func errorHandler(c fiber.Ctx, err error) error {
	status, code, message := classify(err)

	if status >= http.StatusInternalServerError {
		slog.Error("REQUEST_FAILED",
			"method", c.Method(),
			"uri", c.OriginalURL(),
			"status", status,
			"request_id", c.Get(fiber.HeaderXRequestID),
			"error", err.Error())
	}

	// A handler that already answered owns its response. This is what keeps the
	// render domain's ERR_HIGHLIGHT_FAILED intact: every httpx.Fail commits the
	// response, so a later error cannot replace the domain code with a generic
	// one, nor append a second JSON document to the body. Fiber has no
	// Response().Committed flag, so OK/Fail record the fact in Locals instead.
	if isCommitted(c) {
		return nil
	}

	if c.Method() == http.MethodHead {
		// A HEAD response carries no body, so only the status is meaningful.
		return c.SendStatus(status)
	}
	return c.Status(status).JSON(&Response{Error: &Error{Code: code, Message: message}})
}

// classify turns an arbitrary handler or middleware error into the status, code
// and client-visible message of an error envelope.
func classify(err error) (int, string, string) {
	var httpErr *fiber.Error
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

	// Fiber's own 4xx messages are plain status text ("Not Found", "Request
	// Entity Too Large") and carry nothing internal.
	if httpErr.Message != "" {
		return status, code, httpErr.Message
	}
	return status, code, http.StatusText(status)
}
