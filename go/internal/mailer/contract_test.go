package mailer

import (
	"testing"

	"github.com/soulteary/gorge/go/internal/contracttest"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// fixtureDir holds the language-neutral contract fixtures. They live at the
// repository root rather than under go/ so a PHP runner can read the same
// files; see tests/contract/mailer/README.md for what each one pins.
const fixtureDir = "../../../tests/contract/mailer"

func TestContractFixtures(t *testing.T) {
	// Separate providers pin acceptance, permanent rejection, proven safe retry
	// and uncertain submission. Fixtures select them through mailerKeys.
	//
	// Retries stay off: they change how long a failure takes, not what it
	// answers, and a fixture that waited on them would make the suite slow for
	// nothing.
	srv := httpx.New(httpx.Config{ListenAddr: ":0", BodyLimit: TransportBodyLimit})
	RegisterRoutes(srv.App(), &Deps{
		Dispatcher: newTestDispatcher(t,
			MailerSpec{Key: "test-mailer", Type: "test", Priority: 100},
			MailerSpec{Key: "rejects", Type: "test", Priority: 50,
				Options: map[string]string{"fail": "permanent"}},
			MailerSpec{Key: "down", Type: "test", Priority: 10,
				Options: map[string]string{"fail": "temporary"}},
			MailerSpec{Key: "unknown", Type: "test", Priority: 5,
				Options: map[string]string{"fail": "unknown"}},
		),
		Token:     contracttest.Token,
		BodyLimit: DefaultBodyLimit,
	})

	contracttest.Run(t, srv.App(), fixtureDir)
}
