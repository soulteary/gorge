package filestorage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
)

// WriteParams carries what a backend may record alongside the bytes. Both
// fields are advisory: local disk keeps neither, S3 maps MimeType onto the
// object's Content-Type, and the blob backend has no column for either.
//
// Name is never part of a handle. Phorge owns the file's name in its own
// database, and a handle derived from a caller-supplied name would be a path
// traversal waiting to happen.
type WriteParams struct {
	Name     string
	MimeType string
}

// StorageEngine is one storage backend.
//
// Handles are opaque to everything above this interface and are only
// meaningful to the engine that minted one. Their formats are part of the
// compatibility contract — an existing file becomes unreadable, silently, if
// an engine changes how it reads its own handles. See
// compat/phorge/README.md section 8.
type StorageEngine interface {
	// Identifier names the engine on the wire. Phorge stores it against every
	// file it writes here, so these strings cannot change.
	Identifier() string
	// Priority orders write candidates, lowest first.
	Priority() int
	CanWrite() bool
	HasSizeLimit() bool
	// MaxFileSize is meaningful only when HasSizeLimit reports true.
	MaxFileSize() int64

	// WriteFile stores exactly size bytes read from src and returns the handle
	// to read them back with. size is authoritative: it comes from the
	// request's Content-Length, and an engine may use it to reject the write
	// before reading anything.
	WriteFile(ctx context.Context, src io.Reader, size int64, params WriteParams) (handle string, err error)

	// ReadFile opens the object. The returned size is the object's length, or
	// -1 when the engine cannot know it without reading the whole object; the
	// handler turns a known length into the response's Content-Length.
	ReadFile(ctx context.Context, handle string) (rc io.ReadCloser, size int64, err error)

	// DeleteFile removes the object. Deleting an object that is not there is
	// not an error, in any engine: Phorge's garbage collection would otherwise
	// be unable to retire a row whose bytes are already gone.
	DeleteFile(ctx context.Context, handle string) error
}

// ErrBadHandle reports a handle the engine could not have minted, as opposed
// to one that is well formed and points at nothing.
//
// It exists because the two endpoints that take a handle need the distinction
// for opposite reasons. A read does not: it reports every failure as "not
// found", since the caller holds one engine/handle pair and has nothing else
// to try whichever it is. A delete does, and only a delete: it reports an
// object that is already gone as success, so a malformed handle is the one
// remaining failure that is not a backend fault — and left undistinguished it
// answers 500, telling an operator the service is broken when the caller sent
// nonsense.
var ErrBadHandle = errors.New("the handle is not one this engine could have minted")

// readyChecker is implemented by the engines that hold a connection worth
// probing. Only the blob backend does; see Router.Ready for what it must not
// probe.
type readyChecker interface {
	Ready(ctx context.Context) error
}

// NewRouterFromConfig builds every backend the configuration enables and
// returns the router over them.
//
// A backend that is configured but cannot be built fails the whole call rather
// than being skipped: a backend silently missing from the rotation is how a
// deployment ends up writing everything to its fallback without noticing. No
// backend configured at all is *not* an error — the service starts and reports
// itself not ready, which is the state a deployment is in while its
// configuration is still being written.
func NewRouterFromConfig(cfg *Config) (*Router, error) {
	var engines []StorageEngine

	// closeAll releases what has already been built when a later backend
	// fails, so a failed start does not leak a connection pool.
	closeAll := func() {
		for _, eng := range engines {
			if closer, ok := eng.(io.Closer); ok {
				_ = closer.Close()
			}
		}
	}

	if cfg.MySQLBlobEnabled() {
		// OpenDB does not dial, so a database that is not up yet does not stop
		// the service from starting; /readyz pings and reports it instead.
		db, err := OpenDB(cfg.FileDSN())
		if err != nil {
			return nil, fmt.Errorf("mysql blob backend: %w", err)
		}
		engines = append(engines, NewMySQLBlobEngine(db, cfg.MySQLBlobMaxSize))
		slog.Info("storage engine registered",
			"engine", identifierMySQLBlob, "maxSize", cfg.MySQLBlobMaxSize)
	}

	if cfg.LocalDiskEnabled() {
		eng, err := NewLocalDiskEngine(cfg.LocalDiskPath)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("local disk backend: %w", err)
		}
		engines = append(engines, eng)
		slog.Info("storage engine registered",
			"engine", identifierLocalDisk, "path", cfg.LocalDiskPath)
	}

	if cfg.S3Enabled() {
		eng, err := NewS3Engine(S3Config{
			Bucket:       cfg.S3Bucket,
			AccessKey:    cfg.S3AccessKey,
			SecretKey:    cfg.S3SecretKey,
			Region:       cfg.S3Region,
			Endpoint:     cfg.S3Endpoint,
			InstanceName: cfg.InstanceName,
		})
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("s3 backend: %w", err)
		}
		engines = append(engines, eng)
		slog.Info("storage engine registered",
			"engine", identifierS3, "bucket", cfg.S3Bucket)
	}

	if len(engines) == 0 {
		slog.Warn("no storage engine is configured; /readyz reports unavailable until " +
			"one of GORGE_FILE_LOCAL_DISK_PATH, GORGE_FILE_MYSQL_HOST or GORGE_FILE_S3_* is set")
	}

	return NewRouter(engines), nil
}

// closeEngines releases every engine holding something worth closing. It backs
// Router.Close.
func closeEngines(engines []StorageEngine) error {
	var errs []error
	for _, eng := range engines {
		if closer, ok := eng.(io.Closer); ok {
			errs = append(errs, closer.Close())
		}
	}
	return errors.Join(errs...)
}
