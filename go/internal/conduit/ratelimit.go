package conduit

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// maxVisitors caps the per-IP bucket map so a flood of distinct source
// addresses cannot grow it without bound. A client that arrives once the map is
// full is rejected rather than admitted uncounted.
const maxVisitors = 100_000

// visitor is one client's token bucket. Buckets refill lazily on access rather
// than on a timer, so an idle client costs nothing until the cleanup loop
// evicts it.
type visitor struct {
	tokens     float64
	lastSeen   time.Time
	maxTokens  float64
	refillRate float64
}

// RateLimiter is a per-IP token-bucket limiter with method-level exemptions. It
// is carried over from the pre-monorepo gateway rather than replaced with
// x/time/rate so the exemption list, the visitor cap and the eviction loop keep
// their exact behaviour.
type RateLimiter struct {
	mu       sync.Mutex
	visitors map[string]*visitor
	rps      float64
	burst    int
	exempt   map[string]bool
	done     chan struct{}
	stopOnce sync.Once
}

// NewRateLimiter builds a limiter refilling at rps tokens per second per IP,
// with a burst-sized bucket, and starts the background eviction loop. Methods
// in exempt bypass the limiter entirely. Call Stop to end the loop.
func NewRateLimiter(rps, burst int, exempt []string) *RateLimiter {
	exemptMap := make(map[string]bool, len(exempt))
	for _, m := range exempt {
		exemptMap[m] = true
	}
	rl := &RateLimiter{
		visitors: make(map[string]*visitor),
		rps:      float64(rps),
		burst:    burst,
		exempt:   exemptMap,
		done:     make(chan struct{}),
	}
	go rl.cleanupLoop()
	return rl
}

// Stop ends the background eviction loop. It is safe to call more than once.
func (rl *RateLimiter) Stop() {
	rl.stopOnce.Do(func() { close(rl.done) })
}

// Allow reports whether a request from clientIP for method may proceed,
// consuming one token when it may. An exempt method and a non-positive rate
// always pass.
func (rl *RateLimiter) Allow(clientIP, method string) bool {
	if rl.rps <= 0 {
		return true
	}
	if rl.exempt[method] {
		return true
	}

	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	v, ok := rl.visitors[clientIP]
	if !ok {
		if len(rl.visitors) >= maxVisitors {
			slog.Warn("CONDUIT_RATELIMIT_FULL", "capacity", maxVisitors, "client", clientIP)
			return false
		}
		v = &visitor{
			tokens:     float64(rl.burst),
			maxTokens:  float64(rl.burst),
			refillRate: rl.rps,
			lastSeen:   now,
		}
		rl.visitors[clientIP] = v
	}

	elapsed := now.Sub(v.lastSeen).Seconds()
	v.tokens += elapsed * v.refillRate
	if v.tokens > v.maxTokens {
		v.tokens = v.maxTokens
	}
	v.lastSeen = now

	if v.tokens < 1 {
		return false
	}
	v.tokens--
	return true
}

// Middleware guards a route group: it keys on the caller's IP and the :method
// path segment, and answers a Conduit-protocol 429 envelope when the bucket is
// empty. A passing request falls through to the next handler.
func (rl *RateLimiter) Middleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		if !rl.Allow(c.IP(), c.Params("method")) {
			return conduitError(c, http.StatusTooManyRequests, contracts.CodeRateLimit,
				"Too many requests. Please slow down.")
		}
		return c.Next()
	}
}

// cleanupLoop evicts buckets untouched for longer than ten minutes every five,
// keeping the visitor map proportional to recent traffic rather than to every
// IP ever seen.
func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-rl.done:
			return
		case <-ticker.C:
			rl.mu.Lock()
			cutoff := time.Now().Add(-10 * time.Minute)
			for ip, v := range rl.visitors {
				if v.lastSeen.Before(cutoff) {
					delete(rl.visitors, ip)
				}
			}
			rl.mu.Unlock()
		}
	}
}
