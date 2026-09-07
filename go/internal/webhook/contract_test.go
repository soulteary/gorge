package webhook

import (
	"context"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/soulteary/gorge/go/internal/contracttest"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// The language-neutral contract fixtures live at the repository root rather
// than under go/ so a PHP runner can read the same files; see
// tests/contract/webhook/README.md for what each one pins and for the exact
// state a runner has to prepare.
const (
	fixtureDir            = "../../../tests/contract/webhook"
	unavailableFixtureDir = fixtureDir + "/unavailable"
)

// The seeded queue. Every count is a different number on purpose: with two of
// them equal, a fixture would pass against an implementation that returned the
// wrong field.
const (
	seededQueued = 3
	seededSent   = 2
	seededFailed = 1
	// Two hooks, one of them disabled, which is what makes the difference
	// between stats.activeWebhooks and hooks.total observable.
	seededHooks       = 2
	seededActiveHooks = 1
)

// contractStore builds the state the fixtures are written against.
//
// This domain is the reason Store is an interface. Both of its endpoints read
// the database, so unlike file storage — whose local-disk backend lets its
// fixtures run against a directory — there is no configuration of this service
// that answers a fixture without one. A second runner in another language
// needs the same freedom: what the fixtures pin is the wire shape of the two
// status endpoints, not MySQL's ability to count.
func contractStore(t *testing.T) *memStore {
	t.Helper()

	store := newMemStore(1_700_000_000).
		addHook(testHook()).
		addHook(&Hook{PHID: "PHID-HWBH-disabled0000000000", Status: HookStatusDisabled})

	var id int64
	for i := 0; i < seededQueued; i++ {
		id++
		store.addRequest(id, testHookPHID, RequestProperties{Retry: RetryForever})
	}
	for i := 0; i < seededSent; i++ {
		id++
		req := store.addRequest(id, testHookPHID, RequestProperties{})
		if err := store.UpdateResult(context.Background(), req.ID,
			delivered(&RequestProperties{}, "200", store.clock())); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < seededFailed; i++ {
		id++
		req := store.addRequest(id, testHookPHID, RequestProperties{Retry: RetryNever})
		if err := store.UpdateResult(context.Background(), req.ID,
			attemptFailed(&RequestProperties{Retry: RetryNever}, ErrorTypeHTTP, "500",
				store.clock())); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func TestContractFixtures(t *testing.T) {
	// quietLogs also fails this test if any fixture was answered by way of a
	// recovered panic — a fixture asserting only a status code cannot tell the
	// difference on its own. See its comment.
	quietLogs(t)

	store := contractStore(t)
	if _, err := store.CountHooks(context.Background()); err != nil {
		t.Fatal(err)
	}

	contracttest.Run(t, newContractServer(store), fixtureDir)
}

// TestContractFixturesWithAnUnreachableDatabase covers the other half of the
// contract, and it needs its own service configuration rather than its own
// request: what an endpoint answers when the database does not is not
// something a fixture can ask for. The search domain's unavailable/ directory
// exists for the same reason.
func TestContractFixturesWithAnUnreachableDatabase(t *testing.T) {
	quietLogs(t)

	store := newMemStore(1_700_000_000)
	store.statsErr = errStoreDown
	store.countErr = errStoreDown
	store.readyErr = errStoreDown

	contracttest.Run(t, newContractServer(store), unavailableFixtureDir)
}

func newContractServer(store Store) *echo.Echo {
	srv := httpx.New(httpx.Config{Ready: ReadyProbe(store)})
	RegisterRoutes(srv.Echo(), &Deps{Store: store, Token: contracttest.Token})
	return srv.Echo()
}
