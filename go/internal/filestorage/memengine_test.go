package filestorage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
)

// memEngine is an in-memory backend. It exists so the routes, the priority
// order and the write fall-through can be driven without a database, a disk or
// a bucket — and, more to the point, so a *failing* backend can be produced on
// demand, which is the case the fall-through exists for and the one no real
// backend fails on request.
type memEngine struct {
	id        string
	priority  int
	noWrite   bool
	sizeLimit int64

	// writeErr fails every write. drainFirst makes it read src to exhaustion
	// before failing, which is how the blob backend behaves: it buffers the
	// whole file and only then hits the database. Whether a failed engine
	// consumed the bytes is what decides if the next one can be offered them.
	writeErr   error
	drainFirst bool

	// deleteErr fails every delete, and sizeUnknown makes a read report an
	// unknown length so the response has to omit Content-Length.
	deleteErr   error
	sizeUnknown bool
	readyErr    error
	closed      bool

	mu      sync.Mutex
	objects map[string][]byte
	written int
}

func newMemEngine(id string, priority int) *memEngine {
	return &memEngine{id: id, priority: priority, objects: map[string][]byte{}}
}

// seed puts an object in the engine under a handle of the caller's choosing,
// so a read can be asserted without a write having to happen first.
func (m *memEngine) seed(handle string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[handle] = data
}

func (m *memEngine) object(handle string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[handle]
	return data, ok
}

func (m *memEngine) Identifier() string { return m.id }
func (m *memEngine) Priority() int      { return m.priority }
func (m *memEngine) CanWrite() bool     { return !m.noWrite }
func (m *memEngine) HasSizeLimit() bool { return m.sizeLimit > 0 }
func (m *memEngine) MaxFileSize() int64 { return m.sizeLimit }

func (m *memEngine) Ready(context.Context) error { return m.readyErr }

func (m *memEngine) Close() error {
	m.closed = true
	return nil
}

func (m *memEngine) WriteFile(_ context.Context, src io.Reader, _ int64, _ WriteParams) (string, error) {
	if m.writeErr != nil {
		if m.drainFirst {
			if _, err := io.Copy(io.Discard, src); err != nil {
				return "", err
			}
		}
		return "", m.writeErr
	}

	data, err := io.ReadAll(src)
	if err != nil {
		return "", err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.written++
	handle := fmt.Sprintf("%s-handle-%d", m.id, m.written)
	m.objects[handle] = data
	return handle, nil
}

func (m *memEngine) ReadFile(_ context.Context, handle string) (io.ReadCloser, int64, error) {
	data, ok := m.object(handle)
	if !ok {
		return nil, 0, fmt.Errorf("object %q not found in %s", handle, m.id)
	}
	size := int64(len(data))
	if m.sizeUnknown {
		size = -1
	}
	return io.NopCloser(bytes.NewReader(data)), size, nil
}

func (m *memEngine) DeleteFile(_ context.Context, handle string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, handle)
	return nil
}
