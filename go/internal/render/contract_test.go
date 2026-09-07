package render

import (
	"testing"

	"github.com/soulteary/gorge/go/internal/contracttest"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
	"github.com/soulteary/gorge/go/internal/render/highlight"
)

// fixtureDir holds the language-neutral contract fixtures. They live at the
// repository root rather than under go/ so a PHP runner can read the same
// files; see tests/contract/render/README.md for what each one pins.
const fixtureDir = "../../../tests/contract/render"

func TestContractFixtures(t *testing.T) {
	// app.Test serves this in memory, so the listen address is never used.
	srv := httpx.New(httpx.Config{ListenAddr: ":0"})
	RegisterRoutes(srv.App(), &Deps{
		Highlighter: highlight.New(),
		Token:       contracttest.Token,
		MaxBytes:    DefaultMaxBytes,
	})

	contracttest.Run(t, srv.App(), fixtureDir)
}
