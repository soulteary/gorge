package filestorage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// ErrNoEngine reports that no configured engine will accept a write. The
// handler turns it into 503 ERR_NO_ENGINE, which is a different statement from
// "the write was attempted and failed": nothing was attempted.
var ErrNoEngine = errors.New("no writable storage engine is available")

// readyTimeout bounds the whole readiness probe, however many engines have
// something to check. A probe that hangs is reported as unready by the
// orchestrator's own timeout, but only after a much longer wait.
const readyTimeout = 5 * time.Second

// Router holds the configured engines in the order a write will try them.
type Router struct {
	engines []StorageEngine
	byID    map[string]StorageEngine
}

// NewRouter orders the engines by ascending priority, which is the order a
// write tries them in.
func NewRouter(engines []StorageEngine) *Router {
	sorted := make([]StorageEngine, len(engines))
	copy(sorted, engines)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Priority() < sorted[j].Priority()
	})

	byID := make(map[string]StorageEngine, len(engines))
	for _, eng := range engines {
		byID[eng.Identifier()] = eng
	}

	return &Router{engines: sorted, byID: byID}
}

// Close releases whatever the engines hold — in practice the blob backend's
// connection pool.
func (r *Router) Close() error { return closeEngines(r.engines) }

// Ready reports whether the service can store anything at all. It backs
// /readyz.
//
// Two things are checked and no more: that at least one engine is registered,
// and that the engines holding a connection can reach it.
//
// **It must not check whether `file_storageblob` exists.** That table is
// created by Phorge's own `bin/storage upgrade`, which runs inside the Phorge
// application container — and that container waits on this service being
// healthy before it starts. A readiness probe that required the table would
// therefore deadlock the stack on first boot: Phorge waits for this service,
// this service waits for a table only Phorge can create. Pinging the database
// is safe because the database server is a separate container that depends on
// neither. See docs/modules/file-storage.md section 3.5.
func (r *Router) Ready() error {
	if len(r.engines) == 0 {
		return errors.New("no storage engines configured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), readyTimeout)
	defer cancel()

	for _, eng := range r.engines {
		checker, ok := eng.(readyChecker)
		if !ok {
			continue
		}
		if err := checker.Ready(ctx); err != nil {
			return fmt.Errorf("engine %s: %w", eng.Identifier(), err)
		}
	}
	return nil
}

// GetEngine returns the engine with the given identifier. An unknown
// identifier is the caller's mistake, not a server failure: a handle is
// meaningless without the engine that minted it, so there is nothing sensible
// to substitute.
func (r *Router) GetEngine(identifier string) (StorageEngine, error) {
	eng, ok := r.byID[identifier]
	if !ok {
		return nil, fmt.Errorf("unknown storage engine: %q", identifier)
	}
	return eng, nil
}

// ListEngines describes the engines in the order a write tries them.
func (r *Router) ListEngines() []contracts.EngineInfo {
	info := make([]contracts.EngineInfo, 0, len(r.engines))
	for _, eng := range r.engines {
		limit := int64(0)
		if eng.HasSizeLimit() {
			limit = eng.MaxFileSize()
		}
		info = append(info, contracts.EngineInfo{
			Identifier: eng.Identifier(),
			Priority:   eng.Priority(),
			CanWrite:   eng.CanWrite(),
			SizeLimit:  limit,
		})
	}
	return info
}

// Write stores the bytes in the first engine that accepts them, trying the
// engines in ascending priority order and moving on when one fails.
//
// Falling through on a *failure* — not just on a size limit — is what the
// standalone service did not do, and it is the behaviour Phorge's own
// buildFromFileData has always had. The case it exists for is concrete: while
// `bin/storage upgrade` has not run yet, `file_storageblob` does not exist, so
// every blob write fails. An upload must land on local disk instead of failing
// outright.
//
// Retrying costs something, though, and the cost is what bounds it. src can
// only be re-read as far as rewindBudget bytes, so an engine that failed after
// consuming more than that ends the attempt — see rewindReader. In practice
// that is exactly the right cut: the engine that both fails this way and needs
// a fallback is the blob backend, and it never reads past its own size limit.
func (r *Router) Write(ctx context.Context, src io.Reader, size int64, params WriteParams) (*contracts.WriteResult, error) {
	candidates := make([]StorageEngine, 0, len(r.engines))
	for _, eng := range r.engines {
		if !eng.CanWrite() {
			continue
		}
		if eng.HasSizeLimit() && size > eng.MaxFileSize() {
			continue
		}
		candidates = append(candidates, eng)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w for a file of %d bytes", ErrNoEngine, size)
	}

	rewindable := newRewindReader(src, r.rewindBudget())

	var lastErr error
	for i, eng := range candidates {
		if i > 0 {
			if err := rewindable.Rewind(); err != nil {
				// The bytes are gone, so no other engine can be offered them.
				// Report the write failure rather than this one: the failure
				// is why we are here, and it is what an operator has to fix.
				slog.Error("FILE_WRITE_NOT_RETRYABLE",
					"engine", eng.Identifier(), "size", size, "reason", err)
				break
			}
		}

		handle, err := eng.WriteFile(ctx, rewindable, size, params)
		if err == nil {
			return &contracts.WriteResult{
				Handle: handle,
				Engine: eng.Identifier(),
				Size:   size,
			}, nil
		}

		lastErr = fmt.Errorf("%s: %w", eng.Identifier(), err)
		slog.Warn("FILE_WRITE_FAILED",
			"engine", eng.Identifier(), "size", size,
			"remaining_candidates", len(candidates)-i-1, "error", err)
	}

	return nil, fmt.Errorf("every storage engine failed, last error: %w", lastErr)
}

// WriteTo stores the bytes in one named engine, with no fall-through. Phorge
// names an engine when it is re-writing a file whose engine is already
// recorded, and quietly landing those bytes somewhere else would leave the
// recorded engine pointing at nothing.
func (r *Router) WriteTo(ctx context.Context, identifier string, src io.Reader, size int64, params WriteParams) (*contracts.WriteResult, error) {
	eng, err := r.GetEngine(identifier)
	if err != nil {
		return nil, err
	}

	handle, err := eng.WriteFile(ctx, src, size, params)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", eng.Identifier(), err)
	}
	return &contracts.WriteResult{
		Handle: handle,
		Engine: eng.Identifier(),
		Size:   size,
	}, nil
}

// rewindBudget is how many bytes Write is willing to hold on to so that a
// failed engine can be followed by another.
//
// It is the largest size limit among the engines, which is the same thing as
// "the most an engine that refuses to stream will read". Engines with no limit
// stream, so a failure part-way through one of them is not retryable at any
// budget — and there is nothing after local disk and S3 to retry with anyway.
// With no size-limited engine configured the budget is zero and Write is a
// single attempt, which is all it ever was before.
func (r *Router) rewindBudget() int64 {
	var budget int64
	for _, eng := range r.engines {
		if eng.HasSizeLimit() && eng.MaxFileSize() > budget {
			budget = eng.MaxFileSize()
		}
	}
	return budget
}

// rewindReader records what has been read from src so a second engine can be
// handed the same bytes, as long as no more than budget of them were consumed.
//
// This is deliberately not "buffer the request body": the body limit is 16M
// and buffering all of it per request is what streaming to disk and to S3 was
// changed to avoid. The budget caps the memory at the size of the one engine
// that reads its input whole regardless.
type rewindReader struct {
	src    io.Reader
	buf    []byte
	off    int
	budget int64
	// spent is set once more than budget bytes have been read, at which point
	// buf no longer holds everything and rewinding is impossible.
	spent bool
}

func newRewindReader(src io.Reader, budget int64) *rewindReader {
	return &rewindReader{src: src, budget: budget}
}

func (r *rewindReader) Read(p []byte) (int, error) {
	// Replay first: after a Rewind, buf holds bytes the next engine has not
	// seen yet.
	if r.off < len(r.buf) {
		n := copy(p, r.buf[r.off:])
		r.off += n
		return n, nil
	}

	n, err := r.src.Read(p)
	if n > 0 {
		if r.spent || int64(len(r.buf))+int64(n) > r.budget {
			// Past the budget the recording stops and is discarded: keeping a
			// prefix would only mislead Rewind into replaying a truncated
			// file.
			r.spent = true
			r.buf = nil
			r.off = 0
		} else {
			r.buf = append(r.buf, p[:n]...)
			r.off = len(r.buf)
		}
	}
	return n, err
}

// Rewind prepares the reader to be read from the beginning again, or reports
// why it cannot be.
func (r *rewindReader) Rewind() error {
	if r.spent {
		return fmt.Errorf("the failed write consumed more than the %d byte rewind budget", r.budget)
	}
	r.off = 0
	return nil
}
