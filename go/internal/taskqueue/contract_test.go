package taskqueue

import (
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/contracttest"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// The language-neutral contract fixtures live at the repository root so a PHP
// runner can read the same files; see tests/contract/taskqueue/README.md for
// what each one pins and the state a runner has to prepare.
const (
	fixtureDir            = "../../../tests/contract/taskqueue"
	unavailableFixtureDir = fixtureDir + "/unavailable"
)

// The seeded queue. Every count differs on purpose: with two equal, a fixture
// would pass against an implementation that returned the wrong field.
const (
	seededActive   = 3
	seededArchived = 2
)

// contractStore builds the state the fixtures are written against. Its task
// ids are deterministic (1..seededActive), so a fixture may GET
// /api/queue/tasks/1 and assert its shape.
func contractStore(t *testing.T) *memStore {
	t.Helper()

	store := newMemStore(1_700_000_000)

	for i := 0; i < seededActive; i++ {
		if _, err := store.Enqueue(t.Context(), &contracts.EnqueueRequest{
			TaskClass:  "PhabricatorTestWorker",
			Data:       `{"key":"value"}`,
			ObjectPHID: "PHID-TASK-abcdefghijklmnopqrst",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Two archived: enqueue then complete, so they leave the active set.
	for i := 0; i < seededArchived; i++ {
		task, err := store.Enqueue(t.Context(), &contracts.EnqueueRequest{
			TaskClass: "PhabricatorTestWorker",
			Data:      `{}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Complete(t.Context(), task.ID, 1234); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func TestContractFixtures(t *testing.T) {
	contracttest.Run(t, newContractServer(contractStore(t)), fixtureDir)
}

// TestContractFixturesWithAnUnreachableBackend covers what the read endpoints
// answer when the backend does not — a service configuration a fixture cannot
// ask for on its own, so it gets its own directory.
func TestContractFixturesWithAnUnreachableBackend(t *testing.T) {
	store := newMemStore(1_700_000_000)
	store.statsErr = errStoreDown
	store.getErr = errStoreDown
	store.listErr = errStoreDown
	store.readyErr = errStoreDown

	contracttest.Run(t, newContractServer(store), unavailableFixtureDir)
}

func newContractServer(store Store) *fiber.App {
	srv := httpx.New(httpx.Config{Ready: ReadyProbe(store)})
	RegisterRoutes(srv.App(), &Deps{Store: store, Token: contracttest.Token})
	return srv.App()
}
