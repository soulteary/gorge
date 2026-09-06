package mailer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// RetryPolicy governs how often a *single* adapter is retried before the
// dispatcher gives up on it and fails over to the next one.
type RetryPolicy struct {
	MaxRetries int
	RetryWait  time.Duration
}

// Dispatcher holds the configured backends in the order they will be tried.
type Dispatcher struct {
	adapters []namedAdapter
	retry    RetryPolicy
}

type namedAdapter struct {
	key      string
	priority int
	adapter  Adapter
}

// NewDispatcher builds every adapter the specs name and orders them by
// descending priority. A spec that cannot be built fails the whole call rather
// than being skipped: a backend silently missing from the rotation is how a
// deployment ends up sending everything through its fallback without noticing.
//
// A spec that omits priority is placed after every explicitly prioritised one,
// in declaration order.
func NewDispatcher(specs []MailerSpec, retry RetryPolicy) (*Dispatcher, error) {
	adapters := make([]namedAdapter, 0, len(specs))

	nextPriority := -1
	for _, spec := range specs {
		a, err := NewAdapter(spec)
		if err != nil {
			return nil, fmt.Errorf("mailer %q: %w", spec.Key, err)
		}

		pri := spec.Priority
		if pri == 0 {
			pri = nextPriority
			nextPriority--
		}

		adapters = append(adapters, namedAdapter{
			key:      spec.Key,
			priority: pri,
			adapter:  a,
		})
	}

	sort.SliceStable(adapters, func(i, j int) bool {
		return adapters[i].priority > adapters[j].priority
	})

	return &Dispatcher{adapters: adapters, retry: retry}, nil
}

// Ready reports whether the service can deliver anything at all. It backs
// /readyz, and the only thing it checks is that at least one adapter was
// configured — which is exactly the state that used to pass as healthy while
// every single message failed.
//
// It deliberately does not dial SMTP or ping a provider: readiness would then
// flip with every third-party hiccup, and having several backends to fail over
// between is the answer to those already.
func (d *Dispatcher) Ready() error {
	if len(d.adapters) == 0 {
		return errors.New("no mailers configured")
	}
	return nil
}

// Send delivers msg through the first adapter that accepts it.
func (d *Dispatcher) Send(ctx context.Context, msg *contracts.EmailMessage) (*contracts.SendResult, error) {
	return d.SendWith(ctx, msg, nil)
}

// SendWith restricts delivery to the named mailers. The adapters are still
// tried in priority order, not in the order the keys were given: the caller is
// narrowing the set, not reordering it. An empty key list means "any".
func (d *Dispatcher) SendWith(ctx context.Context, msg *contracts.EmailMessage, mailerKeys []string) (*contracts.SendResult, error) {
	if err := d.Ready(); err != nil {
		return nil, err
	}

	var keySet map[string]bool
	if len(mailerKeys) > 0 {
		keySet = make(map[string]bool, len(mailerKeys))
		for _, k := range mailerKeys {
			keySet[k] = true
		}
	}

	var lastErr error
	matched := 0
	for _, na := range d.adapters {
		if keySet != nil && !keySet[na.key] {
			continue
		}
		matched++

		messageID, err := d.sendThrough(ctx, na, msg)
		if err == nil {
			return &contracts.SendResult{MailerKey: na.key, MessageID: messageID}, nil
		}
		// A permanent failure is a property of the message, not of the backend,
		// so trying the next one would only produce the same rejection more
		// slowly.
		if IsPermanent(err) {
			return nil, err
		}
		lastErr = err
	}

	if matched == 0 {
		return nil, fmt.Errorf("no matching mailers for keys: %v", mailerKeys)
	}
	return nil, fmt.Errorf("all mailers failed, last error: %w", lastErr)
}

// sendThrough retries one adapter before the caller moves on to the next.
//
// The retries absorb second-scale flakiness only, and the whole loop is bound
// by ctx: Phorge's client waits 30 seconds and its worker queue is the
// authoritative retry loop, so there is nothing to gain by outliving the caller
// that asked for the send. See docs/modules/mailer.md section 4 for why the
// defaults are as small as they are.
func (d *Dispatcher) sendThrough(ctx context.Context, na namedAdapter, msg *contracts.EmailMessage) (string, error) {
	var lastErr error

	for attempt := 0; ; attempt++ {
		messageID, err := na.adapter.Send(ctx, msg)
		if err == nil {
			return messageID, nil
		}

		slog.Warn("MAILER_SEND_FAILED",
			"mailer", na.key, "type", na.adapter.Type(),
			"attempt", attempt+1, "permanent", IsPermanent(err), "error", err)

		if IsPermanent(err) {
			return "", err
		}
		lastErr = fmt.Errorf("%s (%s): %w", na.key, na.adapter.Type(), err)

		if attempt >= d.retry.MaxRetries {
			return "", lastErr
		}
		if waitErr := waitBeforeRetry(ctx, d.retry.RetryWait); waitErr != nil {
			return "", fmt.Errorf("%w (last error: %v)", waitErr, lastErr)
		}
	}
}

// waitBeforeRetry sleeps, or returns early with the context's error. A
// zero wait still consults the context, so a cancelled request stops even when
// retries were configured to be immediate.
func waitBeforeRetry(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return ctx.Err()
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// MailerKeys lists the configured mailer keys in the order they are tried.
func (d *Dispatcher) MailerKeys() []string {
	keys := make([]string, len(d.adapters))
	for i, na := range d.adapters {
		keys[i] = na.key
	}
	return keys
}

// MailerInfo describes the configured mailers for GET /api/mailer/mailers.
func (d *Dispatcher) MailerInfo() []contracts.MailerInfo {
	info := make([]contracts.MailerInfo, len(d.adapters))
	for i, na := range d.adapters {
		info[i] = contracts.MailerInfo{
			Key:      na.key,
			Type:     na.adapter.Type(),
			Priority: na.priority,
		}
	}
	return info
}
