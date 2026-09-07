// Command gorge-notification serves the notification domain, replacing Phorge's
// Aphlict server. Unlike gorge-render it is its own binary on its own ports,
// because Phorge requires two of them: an admin port PHP posts messages to and a
// client port browsers hold WebSockets open against. Phorge validates that both
// exist and probes them differently, so they cannot be collapsed into one. See
// compat/phorge/README.md.
package main

import (
	"fmt"
	"os"

	"github.com/soulteary/gorge/go/internal/notification"
	"github.com/soulteary/gorge/go/internal/notification/hub"
	"github.com/soulteary/gorge/go/internal/notification/peer"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

func main() {
	cfg, err := notification.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gorge-notification: failed to load config: %v\n", err)
		os.Exit(1)
	}

	// One hub behind both ports: the admin port publishes into it and the
	// client port's listeners read out of it. That shared state is the reason
	// the two ports live in one process rather than two.
	messages := hub.New()

	peers := peer.NewList()
	fmt.Fprintf(os.Stderr, "gorge-notification: fingerprint %s\n", peers.Fingerprint())
	for _, spec := range cfg.Cluster {
		peers.AddPeer(peer.NewPeer(spec.Host, spec.Port, spec.Protocol))
	}

	servers := make([]*httpx.Server, 0, len(cfg.Servers))
	for _, spec := range cfg.Servers {
		// notification.Load has already rejected any other type.
		switch spec.Type {
		case notification.ServerKindClient:
			srv := httpx.New(httpx.Config{
				ListenAddr: spec.Addr(),
				// Phorge tests this port by asking for GET / over plain HTTP and
				// requires 501; the platform's probe would answer 200 and be
				// read as a broken server.
				SkipRootProbe: true,
				// The hub is in-memory, so readiness equals liveness.
				Ready: nil,
			})
			notification.RegisterClientRoutes(srv.App(), &notification.ClientDeps{Hub: messages})
			servers = append(servers, srv)

		case notification.ServerKindAdmin:
			srv := httpx.New(httpx.Config{ListenAddr: spec.Addr(), Ready: nil})
			notification.RegisterAdminRoutes(srv.App(), &notification.AdminDeps{
				Hub:   messages,
				Peers: peers,
			})
			servers = append(servers, srv)
		}
	}

	// RunAll rather than a Run per port: one signal registration shuts both
	// down, and a port that fails to bind takes the other with it instead of
	// leaving half a service listening.
	if err := httpx.RunAll(servers...); err != nil {
		fmt.Fprintf(os.Stderr, "gorge-notification: %v\n", err)
		os.Exit(1)
	}
}
