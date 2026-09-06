package httpx

import (
	"net/http"

	"github.com/labstack/echo/v4"
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

// OK writes data inside the envelope with status 200.
func OK(c echo.Context, data any) error {
	return c.JSON(http.StatusOK, &Response{Data: data})
}

// Fail writes an error envelope with the given HTTP status.
func Fail(c echo.Context, status int, code, message string) error {
	return c.JSON(status, &Response{Error: &Error{Code: code, Message: message}})
}
