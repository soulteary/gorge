package notification

import (
	"testing"

	"github.com/soulteary/gorge/go/internal/contracttest"
	"github.com/soulteary/gorge/go/internal/notification/hub"
	"github.com/soulteary/gorge/go/internal/notification/peer"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// adminFixtureDir holds the language-neutral contract fixtures for the admin
// port. They live at the repository root rather than under go/ so a PHP runner
// can read the same files; see tests/contract/notification/README.md for what
// each one pins and why this domain's successful responses carry no envelope.
const adminFixtureDir = "../../../tests/contract/notification/admin"

func TestAdminContractFixtures(t *testing.T) {
	// No contracttest.Token, unlike the render and diff runners: this port has
	// no auth middleware to configure, because Phorge's notification client
	// sends no credentials. There is no unauthorized.json here either.
	//
	// httptest serves this in memory, so the listen address is never used.
	srv := httpx.New(httpx.Config{ListenAddr: ":0"})
	RegisterAdminRoutes(srv.App(), &AdminDeps{
		Hub: hub.New(),
		// A peer list with no peers still mints this server's fingerprint,
		// which is the whole of the POST / receipt.
		Peers: peer.NewList(),
	})

	contracttest.Run(t, srv.App(), adminFixtureDir)
}
