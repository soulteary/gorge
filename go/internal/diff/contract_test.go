package diff

import (
	"testing"

	"github.com/soulteary/gorge/go/internal/contracttest"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// fixtureDir holds the language-neutral contract fixtures. They live at the
// repository root rather than under go/ so a PHP runner can read the same
// files; see tests/contract/diff/README.md for what each one pins and why
// this domain asserts on exact bytes where render asserts on substrings.
const fixtureDir = "../../../tests/contract/diff"

func TestContractFixtures(t *testing.T) {
	// httptest serves this in memory, so the listen address is never used.
	srv := httpx.New(httpx.Config{ListenAddr: ":0"})
	RegisterRoutes(srv.Echo(), &Deps{
		Token:    contracttest.Token,
		MaxBytes: DefaultMaxBytes,
	})

	contracttest.Run(t, srv.Echo(), fixtureDir)
}
