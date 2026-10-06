// Package handlers holds the worker's built-in task handlers and the Conduit
// gateway they share.
//
// It is a subpackage rather than part of worker for the same reason render's
// highlight is: the handlers are the pluggable, task-class-specific half of the
// domain, and keeping them out of the loop package lets the loop be tested
// without dragging every handler's dependencies in. RegisterAll wires them into
// a worker.Registry.
package handlers

import (
	"context"
	"encoding/json"
	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/worker"
)

// RegisterAll installs the built-in handlers into the registry.
//
// When a Conduit URL is configured it also installs a fallback that delegates
// any unregistered class to Phorge's PHP backend, which is what lets one worker
// stand in for the whole taskmaster daemon: the explicit registrations below
// are the classes worth naming, and the fallback catches the rest. Without a
// Conduit URL the worker handles only what it implements natively.
func RegisterAll(registry *worker.Registry, conduitURL, conduitToken string, queues ...*worker.Client) {
	RegisterWithFeedPolicy(registry, conduitURL, conduitToken, "", queues...)
}

func RegisterWithFeedPolicy(registry *worker.Registry, conduitURL, conduitToken, policyPath string, queues ...*worker.Client) {
	// This class has a complete native implementation and must remain usable
	// even when no Conduit fallback is configured. Register it first so adding
	// the fallback does not replace the native path either.
	feed := NewFeedHTTPHandler()
	if policyPath != "" {
		feed = NewFeedPolicyHandler(policyPath)
	}
	registry.Register("FeedPublisherHTTPWorker", feed)

	if conduitURL == "" {
		return
	}

	conduit := NewConduitClient(conduitURL, conduitToken)
	delegate := NewConduitDelegateHandler(conduit, queues...)

	registry.Register("PhabricatorSearchWorker", delegate)
	registry.Register("PhabricatorMetaMTAWorker", delegate)
	registry.Register("PhabricatorApplicationTransactionPublishWorker", delegate)
	registry.Register("HeraldWebhookWorker", delegate)

	// Persisted legacy feed tasks contain a key, not a delivery snapshot.
	nativeFeed := newConduitExecutionHandler(conduit, NewFeedHTTPHandler(), queues...)
	registry.Register("FeedPublisherHTTPWorker", func(ctx context.Context, task *contracts.Task, data json.RawMessage) error {
		var snapshot FeedHTTPData
		if json.Unmarshal(data, &snapshot) == nil && snapshot.DeliveryVersion == 1 {
			if policyPath != "" {
				return feed(ctx, task, data)
			}
			return nativeFeed(ctx, task, data)
		}
		return delegate(ctx, task, data)
	})
	registry.SetFallback(delegate)
}
