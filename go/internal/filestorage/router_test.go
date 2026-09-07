package filestorage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// quietLogs silences the warnings the failure paths emit on purpose, so a
// passing run says nothing — and fails the test if a request was answered by
// way of a recovered panic.
//
// That second half is not decoration. A handler that panicked and a handler
// that answered deliberately are indistinguishable from the response: the
// platform's Recover middleware turns the panic into a 500, and the error
// handler then leaves an already-committed response alone, so an assertion on
// the status code passes while the handler is crashing. That is exactly how a
// nil engine reached ReadFile here — every status assertion was green and the
// only evidence was a stack trace in the log nobody was reading.
func quietLogs(t *testing.T) {
	t.Helper()

	logs := &syncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))

	t.Cleanup(func() {
		slog.SetDefault(previous)
		if written := logs.String(); strings.Contains(written, "PANIC_RECOVERED") {
			t.Errorf("a request was answered by way of a recovered panic:\n%s", written)
		}
	})
}

// syncBuffer collects log output. The lock is for the goroutines behind the
// test servers, which log while the test body reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// blobLike is a size-limited backend at the blob engine's priority, and
// diskLike an unlimited one at the local disk engine's. The two together are
// the arrangement every interesting routing decision comes down to.
func blobLike(limit int64) *memEngine {
	eng := newMemEngine("blob", 1)
	eng.sizeLimit = limit
	return eng
}

func diskLike() *memEngine { return newMemEngine("local-disk", 5) }

func TestRouterPrefersTheLowestPriorityEngineThatFits(t *testing.T) {
	blob, disk := blobLike(1000), diskLike()
	// Registered out of priority order, so the sort is what is under test.
	router := NewRouter([]StorageEngine{disk, blob})

	small, err := router.Write(context.Background(), strings.NewReader("small"), 5, WriteParams{})
	if err != nil {
		t.Fatal(err)
	}
	if small.Engine != "blob" {
		t.Errorf("a file inside the blob limit should land in blob, got %s", small.Engine)
	}

	// Skipped rather than failed: an engine that cannot hold the file is not
	// an error, it is simply not a candidate.
	large, err := router.Write(context.Background(), strings.NewReader(strings.Repeat("x", 2000)), 2000, WriteParams{})
	if err != nil {
		t.Fatal(err)
	}
	if large.Engine != "local-disk" {
		t.Errorf("a file over the blob limit should fall to local disk, got %s", large.Engine)
	}
}

func TestRouterReportsTheSizeItWasGiven(t *testing.T) {
	router := NewRouter([]StorageEngine{diskLike()})

	result, err := router.Write(context.Background(), strings.NewReader("hello gorge"), 11, WriteParams{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Size != 11 || result.Handle == "" {
		t.Errorf("unexpected result: %+v", result)
	}
}

// TestRouterFallsThroughOnWriteFailure is the behaviour the standalone service
// did not have. The case it exists for is a real one: until Phorge's
// `bin/storage upgrade` has run, `file_storageblob` does not exist and every
// blob write fails, and an upload has to land on local disk rather than fail.
func TestRouterFallsThroughOnWriteFailure(t *testing.T) {
	quietLogs(t)

	blob := blobLike(1000)
	blob.writeErr = errors.New("Table 'phorge_file.file_storageblob' doesn't exist")
	blob.drainFirst = true // the blob backend buffers the file before it fails
	disk := diskLike()

	router := NewRouter([]StorageEngine{blob, disk})

	result, err := router.Write(context.Background(), strings.NewReader("hello gorge"), 11, WriteParams{})
	if err != nil {
		t.Fatalf("the write should have fallen through to local disk: %v", err)
	}
	if result.Engine != "local-disk" {
		t.Fatalf("expected local-disk, got %s", result.Engine)
	}

	// The bytes matter as much as the engine: replaying a truncated body would
	// look like a success and store a corrupt file.
	data, ok := disk.object(result.Handle)
	if !ok {
		t.Fatal("local disk did not record the object")
	}
	if string(data) != "hello gorge" {
		t.Errorf("the retried engine received %q, want %q", data, "hello gorge")
	}
}

// TestRouterFallsThroughOnAnEmptyFile guards the zero-byte case separately: a
// write that read nothing must still be retryable, and a 0-byte file is legal.
func TestRouterFallsThroughOnAnEmptyFile(t *testing.T) {
	quietLogs(t)

	blob := blobLike(1000)
	blob.writeErr = errors.New("blob backend is down")
	blob.drainFirst = true
	disk := diskLike()

	router := NewRouter([]StorageEngine{blob, disk})

	result, err := router.Write(context.Background(), strings.NewReader(""), 0, WriteParams{})
	if err != nil {
		t.Fatalf("an empty file should still fall through: %v", err)
	}
	if result.Engine != "local-disk" || result.Size != 0 {
		t.Errorf("unexpected result: %+v", result)
	}
}

// TestRouterStopsWhenTheFailedWriteConsumedTheBody is the other half of the
// fall-through, and the reason it is bounded. The bytes only exist once; an
// engine that read past what the router is willing to remember cannot be
// followed by another, and pretending otherwise would store a truncated file.
func TestRouterStopsWhenTheFailedWriteConsumedTheBody(t *testing.T) {
	quietLogs(t)

	// An unlimited engine that fails after draining the body, tried first, and
	// a 100-byte budget from a size-limited engine that this 500-byte file is
	// too big for. So the body is gone by the time local disk could be tried.
	failing := newMemEngine("amazon-s3", 0)
	failing.writeErr = errors.New("s3 put: connection reset")
	failing.drainFirst = true
	disk := diskLike()

	router := NewRouter([]StorageEngine{failing, blobLike(100), disk})
	if budget := router.rewindBudget(); budget != 100 {
		t.Fatalf("this test needs a budget below the file size, got %d", budget)
	}

	_, err := router.Write(context.Background(),
		strings.NewReader(strings.Repeat("x", 500)), 500, WriteParams{})
	if err == nil {
		t.Fatal("a write whose bytes are gone must not report success")
	}
	if disk.written != 0 {
		t.Error("the next engine must not be handed a body that was already consumed")
	}
}

// TestRouterWriteToDoesNotFallThrough: Phorge names an engine when the file's
// engine is already recorded against it, so landing the bytes somewhere else
// would leave that record pointing at nothing.
func TestRouterWriteToDoesNotFallThrough(t *testing.T) {
	quietLogs(t)

	blob := blobLike(1000)
	blob.writeErr = errors.New("blob backend is down")
	disk := diskLike()

	router := NewRouter([]StorageEngine{blob, disk})

	if _, err := router.WriteTo(context.Background(), "blob", strings.NewReader("x"), 1, WriteParams{}); err == nil {
		t.Fatal("expected the named engine's failure to surface")
	}
	if disk.written != 0 {
		t.Error("WriteTo must not try another engine")
	}

	if _, err := router.WriteTo(context.Background(), "nope", strings.NewReader("x"), 1, WriteParams{}); err == nil {
		t.Error("an unknown engine must be an error")
	}
}

// TestRewindReaderBudget pins the rewind rule directly, since the engine-level
// tests can only reach it indirectly.
func TestRewindReaderBudget(t *testing.T) {
	t.Run("within the budget the bytes replay", func(t *testing.T) {
		r := newRewindReader(strings.NewReader("hello gorge"), 100)

		first, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if string(first) != "hello gorge" {
			t.Fatalf("first pass read %q", first)
		}

		if err := r.Rewind(); err != nil {
			t.Fatalf("Rewind: %v", err)
		}
		second, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if string(second) != "hello gorge" {
			t.Errorf("second pass read %q, want the same bytes", second)
		}
	})

	t.Run("past the budget rewinding is refused", func(t *testing.T) {
		r := newRewindReader(strings.NewReader(strings.Repeat("x", 500)), 100)

		if _, err := io.ReadAll(r); err != nil {
			t.Fatal(err)
		}
		if err := r.Rewind(); err == nil {
			t.Error("a reader that overran its budget must refuse to rewind")
		}
	})

	t.Run("a partial read stays replayable", func(t *testing.T) {
		r := newRewindReader(strings.NewReader("hello gorge"), 100)

		buf := make([]byte, 5)
		if _, err := io.ReadFull(r, buf); err != nil {
			t.Fatal(err)
		}
		if err := r.Rewind(); err != nil {
			t.Fatalf("Rewind: %v", err)
		}

		all, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if string(all) != "hello gorge" {
			t.Errorf("after a partial read and a rewind, got %q", all)
		}
	})

	t.Run("a zero budget allows one pass", func(t *testing.T) {
		r := newRewindReader(strings.NewReader("x"), 0)

		if _, err := io.ReadAll(r); err != nil {
			t.Fatal(err)
		}
		if err := r.Rewind(); err == nil {
			t.Error("with no budget there is nothing to replay")
		}
	})
}

// TestRouterBudgetFollowsTheSizeLimitedEngines: the budget is not a tunable,
// it is derived. An arrangement with no size-limited engine has nothing that
// buffers, so it holds nothing.
func TestRouterBudgetFollowsTheSizeLimitedEngines(t *testing.T) {
	if got := NewRouter([]StorageEngine{blobLike(1000), diskLike()}).rewindBudget(); got != 1000 {
		t.Errorf("budget = %d, want the blob limit of 1000", got)
	}
	if got := NewRouter([]StorageEngine{diskLike()}).rewindBudget(); got != 0 {
		t.Errorf("budget = %d, want 0 with no size-limited engine", got)
	}
}

func TestRouterNoWritableEngine(t *testing.T) {
	quietLogs(t)

	t.Run("no engines at all", func(t *testing.T) {
		_, err := NewRouter(nil).Write(context.Background(), strings.NewReader("x"), 1, WriteParams{})
		if !errors.Is(err, ErrNoEngine) {
			t.Errorf("expected ErrNoEngine, got %v", err)
		}
	})

	t.Run("every engine refuses writes", func(t *testing.T) {
		readonly := diskLike()
		readonly.noWrite = true

		_, err := NewRouter([]StorageEngine{readonly}).Write(
			context.Background(), strings.NewReader("x"), 1, WriteParams{})
		if !errors.Is(err, ErrNoEngine) {
			t.Errorf("expected ErrNoEngine, got %v", err)
		}
	})

	t.Run("the file is over every limit", func(t *testing.T) {
		// Distinct from "nothing is configured", and the reason the error
		// carries the size: the deployment stores files, just not this one.
		_, err := NewRouter([]StorageEngine{blobLike(10)}).Write(
			context.Background(), strings.NewReader("x"), 5000, WriteParams{})
		if !errors.Is(err, ErrNoEngine) {
			t.Errorf("expected ErrNoEngine, got %v", err)
		}
	})
}

func TestRouterEveryEngineFailingIsNotErrNoEngine(t *testing.T) {
	quietLogs(t)

	disk := diskLike()
	disk.writeErr = errors.New("disk full")

	_, err := NewRouter([]StorageEngine{disk}).Write(
		context.Background(), strings.NewReader("x"), 1, WriteParams{})
	if err == nil {
		t.Fatal("expected a failure")
	}
	// The two are different statements and the handler answers them
	// differently: 503 for "nothing will take it", 500 for "it was attempted
	// and broke".
	if errors.Is(err, ErrNoEngine) {
		t.Error("a failed write must not be reported as a missing engine")
	}
}

func TestRouterGetEngine(t *testing.T) {
	router := NewRouter([]StorageEngine{blobLike(1000)})

	eng, err := router.GetEngine("blob")
	if err != nil {
		t.Fatal(err)
	}
	if eng.Identifier() != "blob" {
		t.Errorf("got %q", eng.Identifier())
	}
	if _, err := router.GetEngine("nope"); err == nil {
		t.Error("an unknown engine must be an error, not a substitution")
	}
}

func TestRouterListEngines(t *testing.T) {
	info := NewRouter([]StorageEngine{diskLike(), blobLike(1000)}).ListEngines()

	if len(info) != 2 {
		t.Fatalf("expected 2 engines, got %d", len(info))
	}
	// Reported in write order, which is what makes the order verifiable from
	// outside rather than implied.
	if info[0].Identifier != "blob" || info[1].Identifier != "local-disk" {
		t.Fatalf("expected priority order, got %+v", info)
	}
	if info[0].SizeLimit != 1000 || !info[0].CanWrite {
		t.Errorf("blob entry is wrong: %+v", info[0])
	}
	// An engine with no limit reports 0, which is why canWrite is a separate
	// field: "unlimited" must not read as "refuses writes".
	if info[1].SizeLimit != 0 {
		t.Errorf("an unlimited engine must report sizeLimit 0, got %d", info[1].SizeLimit)
	}
}

func TestRouterReady(t *testing.T) {
	t.Run("no engines is not ready", func(t *testing.T) {
		if err := NewRouter(nil).Ready(); err == nil {
			t.Error("a service with no backend must not report itself ready")
		}
	})

	t.Run("an engine with nothing to probe is ready", func(t *testing.T) {
		// The local disk backend has no connection, so its presence alone is
		// readiness.
		if err := NewRouter([]StorageEngine{newLocalDiskLike(t)}).Ready(); err != nil {
			t.Errorf("expected ready, got %v", err)
		}
	})

	t.Run("an unreachable connection is not ready", func(t *testing.T) {
		blob := blobLike(1000)
		blob.readyErr = errors.New("ping database: dial tcp: connection refused")

		err := NewRouter([]StorageEngine{blob}).Ready()
		if err == nil {
			t.Fatal("expected the engine's failure to surface")
		}
		// The engine is named, because "unavailable" alone does not say which
		// of three backends to go and look at.
		if !strings.Contains(err.Error(), "blob") {
			t.Errorf("the reason must name the engine, got %q", err)
		}
	})
}

func TestRouterCloseReleasesTheEngines(t *testing.T) {
	blob := blobLike(1000)
	// Local disk holds nothing closeable, so a router over both has to close
	// the one that does and leave the other alone.
	if err := NewRouter([]StorageEngine{blob, newLocalDiskLike(t)}).Close(); err != nil {
		t.Fatal(err)
	}
	if !blob.closed {
		t.Error("Close must release the engines that hold a connection")
	}
}

// newLocalDiskLike is a real local disk engine on a temporary root, for the
// cases that need an engine with nothing to close and nothing to probe.
func newLocalDiskLike(t *testing.T) *LocalDiskEngine {
	t.Helper()
	eng, err := NewLocalDiskEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return eng
}
