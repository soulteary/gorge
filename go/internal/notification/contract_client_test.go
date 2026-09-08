package notification

import (
	"testing"

	"github.com/soulteary/gorge/go/internal/contracttest"
	"github.com/soulteary/gorge/go/internal/notification/hub"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// clientFixtureDir holds the language-neutral contract fixtures for the client
// port. They live at the repository root rather than under go/ so a PHP runner
// can read the same files; see tests/contract/notification/README.md for why
// the 501 in them is a health signal rather than an unimplemented endpoint.
const clientFixtureDir = "../../../tests/contract/notification/client"

func TestClientContractFixtures(t *testing.T) {
	// SkipRootProbe is what the fixtures are really checking: with the
	// platform's GET / probe registered, every one of them would see the
	// probe's 200 instead of the 501 Phorge requires. Two ports means two
	// Fiber apps, so the client fixtures need their own server rather
	// than sharing the admin runner's.
	srv := httpx.New(httpx.Config{ListenAddr: ":0", SkipRootProbe: true})
	RegisterClientRoutes(srv.App(), &ClientDeps{Hub: hub.New()})

	// The successful handshake is absent on purpose: httptest.ResponseRecorder
	// does not implement http.Hijacker, so an upgrade cannot complete in
	// memory and the 101 is covered by tests/e2e/notification.sh instead.
	contracttest.Run(t, srv.App(), clientFixtureDir)
}
