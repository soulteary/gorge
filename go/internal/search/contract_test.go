package search

import (
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/contracttest"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/search/engine"
)

// The fixtures live at the repository root rather than under go/ so a PHP
// runner can read the same files; see tests/contract/search/README.md.
//
// Two directories rather than one, because this domain has no per-request way
// to pick a backend. The mailer's fixtures reach a failing adapter through
// mailerKeys in the body; here the engine selects by role alone, so a healthy
// store and a broken one are two service configurations and cannot be pinned
// by one runner.
const (
	fixtureDir            = "../../../tests/contract/search"
	unavailableFixtureDir = "../../../tests/contract/search/unavailable"
)

func TestContractFixtures(t *testing.T) {
	contracttest.Run(t, newFixtureServer(t, engine.BackendDef{Type: "test"}), fixtureDir)
}

// The five domain error codes have no other way to be reached. Every one of
// them means a store misbehaved, and a store that misbehaves on demand is the
// only thing that can produce all five in a suite with no cluster to break.
func TestContractFixturesAgainstAnUnavailableStore(t *testing.T) {
	handler := newFixtureServer(t, engine.BackendDef{
		Type:    "test",
		Options: map[string]string{"fail": "index,search,init,exists,sane,stats"},
	})
	contracttest.Run(t, handler, unavailableFixtureDir)
}

func newFixtureServer(t *testing.T, defs ...engine.BackendDef) *fiber.App {
	t.Helper()

	se, err := NewEngine(defs)
	if err != nil {
		t.Fatal(err)
	}

	srv := httpx.New(httpx.Config{ListenAddr: ":0", Ready: se.Ready})
	RegisterRoutes(srv.App(), &Deps{Engine: se, Token: contracttest.Token})
	return srv.App()
}
