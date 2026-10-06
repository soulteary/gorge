package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"os"
	"sync/atomic"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/notification/publisher"
	"github.com/soulteary/gorge/go/internal/worker"
)

type NotificationPolicy struct {
	Version   int      `json:"version"`
	Mode      string   `json:"mode"`
	Instance  string   `json:"instance"`
	Endpoints []string `json:"endpoints"`
}

type NotificationPublishData = contracts.NotificationPublishData

var notificationCounters struct {
	Accepted, Degraded, Suppressed, Retry, Invalid, PolicyError, ShadowError, NoEndpoints atomic.Uint64
}

func NotificationStats() any {
	return map[string]uint64{
		"accepted": notificationCounters.Accepted.Load(), "degraded": notificationCounters.Degraded.Load(),
		"suppressed": notificationCounters.Suppressed.Load(), "retry": notificationCounters.Retry.Load(),
		"invalid": notificationCounters.Invalid.Load(), "policyError": notificationCounters.PolicyError.Load(),
		"shadowError": notificationCounters.ShadowError.Load(), "noEndpoints": notificationCounters.NoEndpoints.Load(),
	}
}

// Shadow validates without making HTTP requests, then lets the existing PHP
// delegate remain the only sender. Errors are observable, not delivery gates.
func RegisterNotificationMode(registry *worker.Registry, path, mode string) error {
	explicitDelegated := mode == "delegated"
	if mode == "" || mode == "auto" {
		if path == "" {
			mode = "delegated"
		} else {
			mode = "native"
		}
	}
	if mode == "delegated" {
		if explicitDelegated && !registry.Has("PhabricatorNotificationPublishWorker") {
			return fmt.Errorf("notification delegated mode requires a PHP delegate")
		}
		return nil
	}
	if mode != "native" && mode != "shadow" {
		return fmt.Errorf("invalid notification execution mode")
	}
	if path == "" {
		return fmt.Errorf("notification execution policy is required")
	}
	if mode == "native" {
		RegisterNotification(registry, path)
		return nil
	}
	delegate, ok := registry.Get("PhabricatorNotificationPublishWorker")
	if !ok {
		return fmt.Errorf("notification shadow requires a PHP delegate")
	}
	audit := newNotificationPublishHandler(path, true)
	registry.Register("PhabricatorNotificationPublishWorker", func(ctx context.Context, task *contracts.Task, data json.RawMessage) error {
		if err := audit(ctx, task, data); err != nil {
			notificationCounters.ShadowError.Add(1)
			slog.Warn("notification shadow validation failed")
		}
		return delegate(ctx, task, data)
	})
	return nil
}

func readNotificationPolicy(path string) (*NotificationPolicy, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("notification policy unavailable: %w", err)
	}
	var p NotificationPolicy
	if json.Unmarshal(raw, &p) != nil || p.Version != 1 || p.Instance == "" || p.Endpoints == nil {
		return nil, fmt.Errorf("invalid notification policy")
	}
	switch p.Mode {
	case "required", "fallback", "off":
	default:
		return nil, fmt.Errorf("invalid notification mode")
	}
	for _, endpoint := range p.Endpoints {
		u, err := url.Parse(endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
			return nil, fmt.Errorf("invalid notification endpoint")
		}
	}
	return &p, nil
}
func ValidateNotificationPolicy(path string) error {
	_, err := readNotificationPolicy(path)
	return err
}

// Register explicitly, after the delegate registration. A configured native
// notification policy never invokes PHP prepare/execute, even for old tasks.
func RegisterNotification(registry *worker.Registry, path string) {
	if path != "" {
		registry.Register("PhabricatorNotificationPublishWorker", NewNotificationPublishHandler(path))
	}
}

func NewNotificationPublishHandler(path string) worker.TaskHandler {
	return newNotificationPublishHandler(path, false)
}
func newNotificationPublishHandler(path string, shadow bool) worker.TaskHandler {
	client := publisher.New()
	return func(ctx context.Context, task *contracts.Task, data json.RawMessage) error {
		p, err := readNotificationPolicy(path)
		if err != nil {
			notificationCounters.PolicyError.Add(1)
			return err
		}
		var d NotificationPublishData
		bad := func(reason string) error {
			notificationCounters.Invalid.Add(1)
			return &worker.PermanentError{Msg: reason}
		}
		if json.Unmarshal(data, &d) != nil || d.Message == nil {
			return bad("invalid notification message")
		}
		if d.DeliveryVersion != 0 && d.DeliveryVersion != 1 {
			return bad("unsupported notification delivery version")
		}
		if d.DeliveryVersion == 1 && (d.EventID == "" || d.Instance == "") {
			return bad("incomplete notification envelope")
		}
		var kind, key string
		var subscribers []string
		if json.Unmarshal(d.Message["type"], &kind) != nil || kind != "notification" || json.Unmarshal(d.Message["key"], &key) != nil || key == "" || json.Unmarshal(d.Message["subscribers"], &subscribers) != nil || len(subscribers) == 0 {
			return bad("invalid private notification recipients or identity")
		}
		for _, subscriber := range subscribers {
			if subscriber == "" {
				return bad("empty notification recipient")
			}
		}
		if d.Instance == "" {
			d.Instance = p.Instance
		}
		// One policy file belongs to one trusted deployment instance. Do not allow
		// queued data to select another tenant on the same notification service.
		if d.Instance != p.Instance {
			return bad("notification instance mismatch")
		}
		var id string
		if raw, ok := d.Message["uniqueID"]; ok {
			if json.Unmarshal(raw, &id) != nil || id == "" {
				return bad("invalid notification uniqueID")
			}
		} else {
			id = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s/task/%d", d.Instance, task.ID))))
			d.Message["uniqueID"], _ = json.Marshal(id)
		}
		if shadow {
			return nil
		}
		if p.Mode == "off" {
			notificationCounters.Suppressed.Add(1)
			return nil
		}
		if len(p.Endpoints) == 0 {
			notificationCounters.NoEndpoints.Add(1)
			slog.Warn("notification has no enabled endpoints")
			return nil
		}
		payload, _ := json.Marshal(d.Message)
		// Spread load across enabled servers, matching PHP shuffle().
		rand.Shuffle(len(p.Endpoints), func(i, j int) { p.Endpoints[i], p.Endpoints[j] = p.Endpoints[j], p.Endpoints[i] })
		credentialsRejected := false
		for _, endpoint := range p.Endpoints {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			err := client.Publish(ctx, endpoint, d.Instance, payload)
			if err == nil {
				notificationCounters.Accepted.Add(1)
				return nil
			}
			credentialsRejected = credentialsRejected || errors.Is(err, publisher.ErrCredentials)
		}
		if credentialsRejected {
			notificationCounters.Retry.Add(1)
			return &worker.RetryError{Message: "notification credentials rejected", Wait: 60}
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p.Mode == "fallback" {
			notificationCounters.Degraded.Add(1)
			slog.Warn("notification degraded")
			return nil
		}
		notificationCounters.Retry.Add(1)
		return &worker.RetryError{Message: "notification endpoints unavailable", Wait: 60}
	}
}
