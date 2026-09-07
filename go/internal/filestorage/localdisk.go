package filestorage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// identifierLocalDisk is what Phorge records against every file this backend
// writes. Changing it makes those files unreadable, without an error.
const identifierLocalDisk = "local-disk"

// localHandlePattern is both the shape this backend mints and the only shape
// it will read.
//
// Validating on read is not belt-and-braces: a handle arrives from a query
// parameter, and `filepath.Join(root, handle)` happily resolves `../../..`.
// The pattern is what stops a caller from reading any file the process can
// open. TestLocalDiskRejectsBadHandle covers it.
var localHandlePattern = regexp.MustCompile(`^[a-f0-9]{2}/[a-f0-9]{2}/[a-f0-9]{28}$`)

// LocalDiskEngine stores each file as one file on disk, under a two-level
// directory fan-out so no single directory collects every file.
type LocalDiskEngine struct {
	root string
}

// NewLocalDiskEngine prepares the storage root, creating it when it is
// missing.
func NewLocalDiskEngine(root string) (*LocalDiskEngine, error) {
	if root == "" || root == "/" || root[0] != '/' {
		return nil, fmt.Errorf("local disk root must be an absolute path, got %q", root)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create storage root: %w", err)
	}
	return &LocalDiskEngine{root: root}, nil
}

func (e *LocalDiskEngine) Identifier() string { return identifierLocalDisk }

// Priority puts local disk between the blob backend and S3: it is the
// fallback for anything too large for a database row, and the one to lose to
// S3 when a deployment has object storage.
func (e *LocalDiskEngine) Priority() int      { return 5 }
func (e *LocalDiskEngine) CanWrite() bool     { return true }
func (e *LocalDiskEngine) HasSizeLimit() bool { return false }
func (e *LocalDiskEngine) MaxFileSize() int64 { return 0 }

// WriteFile streams src to disk. size is not used: the caller's Content-Length
// is not authority over how many bytes a file gets, and io.Copy needs no
// length.
func (e *LocalDiskEngine) WriteFile(_ context.Context, src io.Reader, _ int64, _ WriteParams) (string, error) {
	handle, err := generateLocalHandle()
	if err != nil {
		return "", err
	}

	fullPath := filepath.Join(e.root, handle)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(fullPath), err)
	}

	f, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", fmt.Errorf("create file: %w", err)
	}

	// A copy that fails part-way leaves a truncated file behind, and its
	// handle has not been reported to anyone: nothing will ever read it, and
	// nothing will ever delete it either. Remove it here or it stays on disk
	// forever.
	if _, err := io.Copy(f, src); err != nil {
		_ = f.Close()
		_ = os.Remove(fullPath)
		return "", fmt.Errorf("write file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(fullPath)
		return "", fmt.Errorf("close file: %w", err)
	}

	return handle, nil
}

// ReadFile opens the file and reports its length, so the response can carry a
// Content-Length.
func (e *LocalDiskEngine) ReadFile(_ context.Context, handle string) (io.ReadCloser, int64, error) {
	if !localHandlePattern.MatchString(handle) {
		return nil, 0, fmt.Errorf("malformed local disk handle %q: %w", handle, ErrBadHandle)
	}

	f, err := os.Open(filepath.Join(e.root, handle))
	if err != nil {
		return nil, 0, fmt.Errorf("open file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("stat file: %w", err)
	}
	return f, info.Size(), nil
}

// DeleteFile removes the file. A file that is already gone is a success; see
// StorageEngine.DeleteFile.
func (e *LocalDiskEngine) DeleteFile(_ context.Context, handle string) error {
	if !localHandlePattern.MatchString(handle) {
		return fmt.Errorf("malformed local disk handle %q: %w", handle, ErrBadHandle)
	}

	err := os.Remove(filepath.Join(e.root, handle))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove file: %w", err)
	}
	return nil
}

// generateLocalHandle mints `ab/cd/{28 hex}` from 16 random bytes. The layout
// matches Phorge's own local disk engine, which is what lets a directory
// written by one be read by the other.
func generateLocalHandle() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random handle: %w", err)
	}
	h := hex.EncodeToString(b)
	return fmt.Sprintf("%s/%s/%s", h[:2], h[2:4], h[4:]), nil
}
