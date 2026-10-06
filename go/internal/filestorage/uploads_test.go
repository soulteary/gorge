package filestorage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUploadsResumeCompleteAndRanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	u, e := NewUploads(root)
	if e != nil {
		t.Fatal(e)
	}
	id := "0123456789abcdef0123456789abcdef"
	size := UploadChunkSize + 3
	if _, e = u.Create(ctx, id, size); e != nil {
		t.Fatal(e)
	}
	if _, e = u.Create(ctx, id, size+1); !errors.Is(e, ErrUploadConflict) {
		t.Fatal(e)
	}
	first := bytes.Repeat([]byte("a"), int(UploadChunkSize))
	if _, e = u.Put(ctx, id, 0, first); e != nil {
		t.Fatal(e)
	}
	u, _ = NewUploads(root)
	if _, e = u.Put(ctx, id, 0, first); e != nil {
		t.Fatal(e)
	}
	if _, e = u.Put(ctx, id, 0, bytes.Repeat([]byte("b"), len(first))); !errors.Is(e, ErrUploadConflict) {
		t.Fatal(e)
	}
	if _, e = u.Complete(ctx, id); !errors.Is(e, ErrUploadConflict) {
		t.Fatal(e)
	}
	if _, e = u.Put(ctx, id, UploadChunkSize, []byte("xyz")); e != nil {
		t.Fatal(e)
	}
	s, e := u.Complete(ctx, id)
	if e != nil || s.State != "complete" || len(s.Digest) != 64 {
		t.Fatalf("%+v %v", s, e)
	}
	r, n, e := u.Read(ctx, id, UploadChunkSize-2, size)
	if e != nil {
		t.Fatal(e)
	}
	data, e := io.ReadAll(r)
	_ = r.Close()
	if e != nil || n != 5 || string(data) != "aaxyz" {
		t.Fatalf("%q %v", data, e)
	}
	if _, _, e = u.Read(ctx, id, 0, size); !errors.Is(e, ErrUploadConflict) {
		t.Fatal(e)
	}
	if e = u.Cancel(ctx, id); e != nil {
		t.Fatal(e)
	}
	if _, e = u.Create(ctx, id, size); !errors.Is(e, ErrUploadExpired) {
		t.Fatal(e)
	}
	if e = u.Cancel(ctx, id); e != nil {
		t.Fatal(e)
	}
}
func TestUploadCorruptionAndExpiry(t *testing.T) {
	ctx := context.Background()
	u, _ := NewUploads(t.TempDir())
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	s, e := u.Create(ctx, id, 3)
	if e != nil {
		t.Fatal(e)
	}
	_, _ = u.Put(ctx, id, 0, []byte("abc"))
	if e = os.WriteFile(filepath.Join(u.root, id, "0"), []byte("xyz"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = u.Complete(ctx, id); e == nil {
		t.Fatal("accepted corrupted bytes")
	}
	s.Expires = time.Now().Unix() - 1
	if e = u.save(s); e != nil {
		t.Fatal(e)
	}
	if _, e = u.Status(ctx, id); !errors.Is(e, ErrUploadExpired) {
		t.Fatal(e)
	}
	if e = u.SweepExpired(ctx, 10); e != nil {
		t.Fatal(e)
	}
	if _, e = u.Status(ctx, "../bad"); !errors.Is(e, ErrBadHandle) {
		t.Fatal(e)
	}
}
func TestUploadLockCancellation(t *testing.T) {
	u, _ := NewUploads(t.TempDir())
	id := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	unlock, e := u.lock(context.Background(), id)
	if e != nil {
		t.Fatal(e)
	}
	defer unlock()
	ctx, c := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer c()
	_, e = u.Status(ctx, id)
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
}

func TestCancellationCannotResurrectUnknownID(t *testing.T) {
	u, _ := NewUploads(t.TempDir())
	id := "cccccccccccccccccccccccccccccccc"
	dir := filepath.Join(u.root, id)
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	// Simulate a crash before the initial manifest rename.
	if e := os.WriteFile(filepath.Join(dir, ".pending-crashed"), []byte("orphan"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := u.Cancel(context.Background(), id); e != nil {
		t.Fatal(e)
	}
	entries, e := os.ReadDir(dir)
	if e != nil || len(entries) != 1 || entries[0].Name() != "manifest.json" {
		t.Fatal("cancel left crash-uncommitted bytes", entries, e)
	}
	if _, e := u.Create(context.Background(), id, 3); e == nil {
		t.Fatal("cancelled unknown id resurrected")
	}
}

func TestCompletedUploadIntegrityAfterCorruption(t *testing.T) {
	u, _ := NewUploads(t.TempDir())
	id := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	ctx := context.Background()
	if _, e := u.Create(ctx, id, 3); e != nil {
		t.Fatal(e)
	}
	if _, e := u.Put(ctx, id, 0, []byte("abc")); e != nil {
		t.Fatal(e)
	}
	if _, e := u.Complete(ctx, id); e != nil {
		t.Fatal(e)
	}
	if _, e := u.Verify(ctx, id); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(u.root, id, "0"), []byte("bad"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := u.Verify(ctx, id); e == nil {
		t.Fatal("verification accepted corrupt bytes")
	}
	if r, _, e := u.Read(ctx, id, 0, 3); e == nil {
		_ = r.Close()
		t.Fatal("read accepted corrupt bytes")
	}
}
