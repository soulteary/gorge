package filestorage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracttest"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// fixtureDir holds the language-neutral contract fixtures. They live at the
// repository root rather than under go/ so a PHP runner can read the same
// files; see tests/contract/file-storage/README.md for what each one pins and
// for the seeded object a runner has to prepare.
const fixtureDir = "../../../tests/contract/file-storage"

// seededHandle and seededContent are the object the read fixture expects to
// find. A read fixture is a single request, so it cannot write the file it
// reads — the runner has to put it there first. Local disk is what makes that
// reproducible in any language: the handle is a path under the storage root,
// so seeding is one mkdir and one file write, with no database and no bucket.
const (
	seededHandle  = "ab/cd/0123456789abcdef0123456789ab"
	seededContent = "hello gorge"
)

func TestContractFixtures(t *testing.T) {
	// quietLogs also fails this test if any fixture was answered by way of a
	// recovered panic — a fixture asserting only a status code cannot tell the
	// difference on its own. See its comment.
	quietLogs(t)

	// One backend, and local disk rather than the other two: it is the only
	// one a second runner can stand up with no external service, and the
	// fixtures pin the wire contract rather than any backend's behaviour.
	root := t.TempDir()

	full := filepath.Join(root, seededHandle)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(seededContent), 0o644); err != nil {
		t.Fatal(err)
	}

	eng, err := NewLocalDiskEngine(root)
	if err != nil {
		t.Fatal(err)
	}

	router := NewRouter([]StorageEngine{eng})
	srv := httpx.New(httpx.Config{
		ListenAddr: ":0",
		BodyLimit:  TransportBodyLimit,
		Ready:      router.Ready,
	})
	RegisterRoutes(srv.Echo(), &Deps{Router: router, Token: contracttest.Token})

	contracttest.Run(t, srv.Echo(), fixtureDir)
}
