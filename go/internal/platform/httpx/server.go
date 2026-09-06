package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/soulteary/gorge/go/internal/platform/health"
)

const (
	defaultBodyLimit       = "2M"
	defaultShutdownTimeout = 10 * time.Second
)

// Config describes how a service exposes itself over HTTP. It carries no
// domain knowledge, so every gorge binary boots the same way.
type Config struct {
	ListenAddr      string
	BodyLimit       string
	ShutdownTimeout time.Duration
	// Ready is handed to the /readyz probe; nil means "no external dependency".
	Ready health.ReadyFunc
}

// Server is an Echo instance preloaded with the platform middleware stack and
// the health probes.
type Server struct {
	echo *echo.Echo
	cfg  Config
}

// New builds the server. Domain packages register their routes on Echo().
func New(cfg Config) *Server {
	if cfg.BodyLimit == "" {
		cfg.BodyLimit = defaultBodyLimit
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = defaultShutdownTimeout
	}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	// Errors Echo raises on its own must answer in the envelope too, not in
	// Echo's default {"message": "..."} shape. See errors.go.
	e.HTTPErrorHandler = errorHandler

	// RequestID reuses an inbound X-Request-Id when the caller already has one,
	// so a request keeps a single id as it crosses Phorge and the Go services.
	e.Use(middleware.RequestID())
	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogStatus: true, LogURI: true, LogMethod: true, LogRequestID: true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			slog.Info("REQUEST",
				"method", v.Method, "uri", v.URI, "status", v.Status, "request_id", v.RequestID)
			return nil
		},
	}))
	e.Use(middleware.RecoverWithConfig(middleware.RecoverConfig{
		// Echo would print the stack through its own logger; route it to slog
		// instead, which is where the rest of the process logs. The response
		// only ever carries a generic 5xx message, so this log line is the only
		// record of what actually broke.
		LogErrorFunc: func(c echo.Context, err error, stack []byte) error {
			slog.Error("PANIC_RECOVERED",
				"method", c.Request().Method,
				"uri", c.Request().RequestURI,
				"request_id", c.Response().Header().Get(echo.HeaderXRequestID),
				"error", err.Error(),
				"stack", string(stack))
			// Returning the error keeps HTTPErrorHandler in play, which is what
			// turns the panic into a 500 error envelope.
			return err
		},
	}))
	e.Use(middleware.BodyLimit(cfg.BodyLimit))

	health.Register(e, cfg.Ready)

	return &Server{echo: e, cfg: cfg}
}

// Echo exposes the underlying router for route registration and tests.
func (s *Server) Echo() *echo.Echo { return s.echo }

// Run listens until SIGINT or SIGTERM arrives, then drains in-flight requests
// within ShutdownTimeout before returning.
func (s *Server) Run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", s.cfg.ListenAddr)
		err := s.echo.Start(s.cfg.ListenAddr)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	slog.Info("shutting down", "timeout", s.cfg.ShutdownTimeout)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	return s.echo.Shutdown(shutdownCtx)
}
