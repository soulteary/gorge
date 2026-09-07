package httpx

import (
	"net/http"

	"github.com/gofiber/fiber/v3"
)

// Error codes shared by every service. Domain packages may define additional
// codes of their own, but must not redefine these.
const (
	CodeBadRequest       = "ERR_BAD_REQUEST"
	CodeUnauthorized     = "ERR_UNAUTHORIZED"
	CodeNotFound         = "ERR_NOT_FOUND"
	CodeMethodNotAllowed = "ERR_METHOD_NOT_ALLOWED"
	CodeTooLarge         = "ERR_TOO_LARGE"
	CodeInternal         = "ERR_INTERNAL"
)

// committedLocal is the Locals key that records a handler having answered
// through OK or Fail. Fiber has no equivalent of echo's Response().Committed
// flag — a handler's write into the response buffer and the framework's own
// error path are not otherwise distinguishable — so the two helpers set this
// key and errorHandler reads it. See errors.go for why it is load-bearing.
const committedLocal = "httpx_committed"

// Response is the envelope every API endpoint answers with. Exactly one of the
// two fields is populated. Health endpoints are the documented exception, see
// platform/health.
type Response struct {
	Data  any    `json:"data,omitempty"`
	Error *Error `json:"error,omitempty"`
}

// Error is the machine-readable failure description carried by Response.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// markCommitted records that the handler has written its own response, so the
// platform error handler leaves it alone even if the handler then returns an
// error. This is the Fiber stand-in for echo's Response().Committed.
func markCommitted(c fiber.Ctx) {
	c.Locals(committedLocal, true)
}

// isCommitted reports whether OK or Fail already answered on this context.
func isCommitted(c fiber.Ctx) bool {
	committed, _ := c.Locals(committedLocal).(bool)
	return committed
}

// OK writes data inside the envelope with status 200.
func OK(c fiber.Ctx, data any) error {
	markCommitted(c)
	return c.Status(http.StatusOK).JSON(&Response{Data: data})
}

// Fail writes an error envelope with the given HTTP status.
func Fail(c fiber.Ctx, status int, code, message string) error {
	markCommitted(c)
	return c.Status(status).JSON(&Response{Error: &Error{Code: code, Message: message}})
}
