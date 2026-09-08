package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/fiber/v3/middleware/requestid"

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
	// SkipRootProbe leaves GET / unregistered so a domain can answer it
	// instead. Only one port in the repository needs this; see health.Register
	// for the single reason it exists.
	SkipRootProbe bool
}

// Server is a Fiber app preloaded with the platform middleware stack and the
// health probes.
type Server struct {
	app *fiber.App
	cfg Config

	// listenerAddr records the address the app actually bound to, filled in by
	// Fiber's ListenerAddrFunc once Listen has a socket. Tests read it through
	// ListenerAddr to wait for and locate a server on 127.0.0.1:0.
	mu           sync.RWMutex
	listenerAddr net.Addr
}

// New builds the server. Domain packages register their routes on App().
func New(cfg Config) *Server {
	if cfg.BodyLimit == "" {
		cfg.BodyLimit = defaultBodyLimit
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = defaultShutdownTimeout
	}

	app := fiber.New(fiber.Config{
		// Echo distinguished /status/ from /status; Fiber collapses them unless
		// strict routing is on. The notification admin contract requires
		// /status (no trailing slash) to be a 404, so this preserves it. Every
		// route in the repository is called with its exact path, so nothing
		// else depends on the lenient behaviour.
		StrictRouting: true,
		// A body over this limit is rejected by fasthttp before any handler
		// runs; serverErrorHandler turns that into fiber.ErrRequestEntityTooLarge,
		// which errorHandler maps to ERR_TOO_LARGE. The render domain's own
		// GORGE_RENDER_MAX_BYTES answers 413 with the same code, so a caller
		// sees one code no matter which limit it tripped.
		BodyLimit: parseBodyLimit(cfg.BodyLimit),
		// Errors Fiber raises on its own must answer in the envelope too, not
		// in Fiber's default plain-text shape. See errors.go.
		ErrorHandler: errorHandler,
	})

	// RequestID reuses an inbound X-Request-Id when the caller already has one,
	// so a request keeps a single id as it crosses Phorge and the Go services.
	app.Use(requestid.New())
	app.Use(requestLogger())
	app.Use(recover.New(recover.Config{
		EnableStackTrace: true,
		// Fiber would print the stack through its own logger; route it to slog
		// instead, which is where the rest of the process logs. The response
		// only ever carries a generic 5xx message, so this log line is the only
		// record of what actually broke. recover then returns the panic as an
		// error, which errorHandler turns into a 500 error envelope.
		StackTraceHandler: func(c fiber.Ctx, e any) {
			slog.Error("PANIC_RECOVERED",
				"method", c.Method(),
				"uri", c.OriginalURL(),
				"request_id", c.Get(fiber.HeaderXRequestID),
				"error", fmt.Sprintf("%v", e),
				"stack", string(stackTrace()))
		},
	}))

	s := &Server{app: app, cfg: cfg}
	health.Register(app, cfg.Ready, cfg.SkipRootProbe)
	return s
}

// requestLogger is the platform's access log, ported from echo's
// RequestLoggerWithConfig onto Fiber. It logs after the handler runs so the
// status is known.
func requestLogger() fiber.Handler {
	return func(c fiber.Ctx) error {
		err := c.Next()
		slog.Info("REQUEST",
			"method", c.Method(),
			"uri", c.OriginalURL(),
			"status", c.Response().StatusCode(),
			"request_id", c.Get(fiber.HeaderXRequestID))
		return err
	}
}

// App exposes the underlying router for route registration and tests.
func (s *Server) App() *fiber.App { return s.app }

// ListenerAddr reports the address the server bound to, or nil before it has
// started listening. It is filled in by Fiber's ListenerAddrFunc during Listen.
func (s *Server) ListenerAddr() net.Addr {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listenerAddr
}

func (s *Server) setListenerAddr(addr net.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listenerAddr = addr
}

// Run listens until SIGINT or SIGTERM arrives, then drains in-flight requests
// within ShutdownTimeout before returning.
func (s *Server) Run() error { return RunAll(s) }

// RunAll listens on every server at once, sharing one signal registration, and
// returns when the first listener stops or when SIGINT or SIGTERM arrives.
// Whatever is still listening at that point is drained.
//
// A binary that spreads one service over several ports cannot usefully outlive
// any of them: a notification client port with no admin port beside it accepts
// browsers and then never has anything to tell them. So a single failed
// listener takes the whole set down rather than leaving a half-reachable
// service behind for orchestration to keep in rotation.
func RunAll(servers ...*Server) error {
	if len(servers) == 0 {
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Bind every address before any app starts serving. If one bind fails, the
	// listeners opened earlier are closed before RunAll returns, so a sibling
	// goroutine cannot race past shutdown and leave a partial service running.
	listeners := make([]net.Listener, 0, len(servers))
	for _, s := range servers {
		ln, err := net.Listen("tcp", s.cfg.ListenAddr)
		if err != nil {
			closeListeners(listeners)
			return fmt.Errorf("failed to listen: %w", err)
		}
		s.setListenerAddr(ln.Addr())
		listeners = append(listeners, ln)
	}

	// Buffered for every server, so the goroutines behind the listeners we do
	// not wait for still finish instead of blocking on the send forever.
	serveErr := make(chan error, len(servers))
	for i, s := range servers {
		go func() {
			slog.Info("listening", "addr", listeners[i].Addr())
			err := s.app.Listener(listeners[i], fiber.ListenConfig{
				DisableStartupMessage: true,
			})
			serveErr <- err
		}()
	}

	select {
	case err := <-serveErr:
		// The remaining listeners are drained too, but their shutdown errors
		// are dropped: this err is the root cause and the only one worth
		// reporting.
		_ = shutdownAll(servers)
		return err
	case <-ctx.Done():
	}

	return shutdownAll(servers)
}

func closeListeners(listeners []net.Listener) {
	for _, ln := range listeners {
		_ = ln.Close()
	}
}

// shutdownAll drains the servers concurrently, so the wait is the longest
// ShutdownTimeout rather than the sum of them.
func shutdownAll(servers []*Server) error {
	errs := make([]error, len(servers))

	var wg sync.WaitGroup
	for i, s := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slog.Info("shutting down", "addr", s.cfg.ListenAddr, "timeout", s.cfg.ShutdownTimeout)
			errs[i] = s.app.ShutdownWithTimeout(s.cfg.ShutdownTimeout)
		}()
	}
	wg.Wait()

	return errors.Join(errs...)
}

// stackTrace captures the current goroutine's stack for the panic log line.
func stackTrace() []byte {
	buf := make([]byte, 8192)
	n := runtime.Stack(buf, false)
	return buf[:n]
}

// parseBodyLimit turns a human-readable size like "2M" or "1K" into a byte
// count. Echo's body-limit middleware accepted the same spellings, so the
// existing config strings keep working. An unparseable value falls back to the
// default rather than failing the boot.
func parseBodyLimit(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return mustParseBodyLimit(defaultBodyLimit)
	}
	if n, err := parseSize(s); err == nil {
		return n
	}
	return mustParseBodyLimit(defaultBodyLimit)
}

func mustParseBodyLimit(s string) int {
	n, err := parseSize(s)
	if err != nil {
		panic(err)
	}
	return n
}

func parseSize(s string) (int, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	multiplier := 1
	switch {
	case strings.HasSuffix(s, "GB"), strings.HasSuffix(s, "G"):
		multiplier = 1 << 30
		s = strings.TrimRight(s, "GB")
	case strings.HasSuffix(s, "MB"), strings.HasSuffix(s, "M"):
		multiplier = 1 << 20
		s = strings.TrimRight(s, "MB")
	case strings.HasSuffix(s, "KB"), strings.HasSuffix(s, "K"):
		multiplier = 1 << 10
		s = strings.TrimRight(s, "KB")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimRight(s, "B")
	}
	value, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("invalid body limit %q: %w", s, err)
	}
	return value * multiplier, nil
}
