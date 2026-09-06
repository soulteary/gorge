// Package health exposes the container probe endpoints shared by every gorge
// service.
package health

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// ReadyFunc reports whether the service can currently serve traffic. A nil
// ReadyFunc means the service has no external dependency and is ready as soon
// as it is listening.
type ReadyFunc func() error

// Register wires the probe endpoints onto e.
//
// These endpoints answer with a bare {"status":"ok"} body and deliberately do
// NOT use the httpx {data,error} envelope. They are consumed by Docker
// HEALTHCHECK, Kubernetes probes and load balancers, which are configured
// against this flat shape; wrapping them "for consistency" would silently
// break every probe in every deployment. Leave them unwrapped.
//
// skipRoot hands GET / to the caller. It exists for exactly one reason: Phorge
// probes a notification client port with a plain GET / and reads HTTP 501 as
// the healthy answer, treating the 200 this package would otherwise return as
// a broken server (PhabricatorNotificationServerRef::testClient). /healthz and
// /readyz are registered either way, so container probes keep working and no
// other service has a reason to set this.
func Register(e *echo.Echo, ready ReadyFunc, skipRoot bool) {
	if !skipRoot {
		e.GET("/", Live())
	}
	e.GET("/healthz", Live())
	e.GET("/readyz", Ready(ready))
}

// Live reports that the process is up and its HTTP stack is serving.
func Live() echo.HandlerFunc {
	return func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	}
}

// Ready reports whether the service's dependencies are usable. It answers 503
// so that orchestrators pull the instance out of rotation without killing it.
func Ready(ready ReadyFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if ready != nil {
			if err := ready(); err != nil {
				return c.JSON(http.StatusServiceUnavailable, map[string]string{
					"status": "unavailable",
					"reason": err.Error(),
				})
			}
		}
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	}
}
