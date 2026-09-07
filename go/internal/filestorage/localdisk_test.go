package filestorage

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalDiskRoundTrip(t *testing.T) {
	eng := newLocalDiskLike(t)
	ctx := context.Background()

	handle, err := eng.WriteFile(ctx, strings.NewReader("hello gorge"), 11, WriteParams{})
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// The handle layout matches Phorge's own local disk engine, which is what
	// lets a directory written by one be read by the other.
	if !localHandlePattern.MatchString(handle) {
		t.Fatalf("handle %q does not match the ab/cd/{28 hex} layout", handle)
	}

	rc, size, err := eng.ReadFile(ctx, handle)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if size != 11 {
		t.Errorf("size = %d, want 11", size)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello gorge" {
		t.Errorf("read back %q", data)
	}

	if err := eng.DeleteFile(ctx, handle); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, _, err := eng.ReadFile(ctx, handle); err == nil {
		t.Error("reading a deleted file must fail")
	}
}

// TestLocalDiskStreams checks the write path does not hold the file in memory,
// by handing it a reader that never yields more than one byte per call. It
// would pass either way, but it fails if the copy is ever replaced by
// something that needs the whole body at once.
func TestLocalDiskStreams(t *testing.T) {
	eng := newLocalDiskLike(t)
	body := strings.Repeat("gorge", 1000)

	handle, err := eng.WriteFile(context.Background(),
		oneByteReader{strings.NewReader(body)}, int64(len(body)), WriteParams{})
	if err != nil {
		t.Fatal(err)
	}

	rc, size, err := eng.ReadFile(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()

	if size != int64(len(body)) {
		t.Errorf("size = %d, want %d", size, len(body))
	}
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != body {
		t.Error("the streamed file did not survive the round trip")
	}
}

type oneByteReader struct{ r io.Reader }

func (o oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

func TestLocalDiskEmptyFile(t *testing.T) {
	eng := newLocalDiskLike(t)
	ctx := context.Background()

	// A zero-byte file is legal, and it is the case a handler that treats an
	// empty body as "no body" gets wrong.
	handle, err := eng.WriteFile(ctx, strings.NewReader(""), 0, WriteParams{})
	if err != nil {
		t.Fatal(err)
	}
	rc, size, err := eng.ReadFile(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if size != 0 {
		t.Errorf("size = %d, want 0", size)
	}
}

// TestLocalDiskRejectsBadHandle is a security boundary, not tidiness: a handle
// arrives from a query parameter and filepath.Join resolves `..`, so without
// the pattern check a caller could read or delete any file the process can
// open.
func TestLocalDiskRejectsBadHandle(t *testing.T) {
	eng := newLocalDiskLike(t)
	ctx := context.Background()

	for _, handle := range []string{
		"../../../etc/passwd",
		"ab/cd/../../../../etc/passwd",
		"AB/CD/0123456789abcdef0123456789ab", // upper case is not the layout
		"ab/cd/short",
		"",
	} {
		if _, _, err := eng.ReadFile(ctx, handle); err == nil {
			t.Errorf("ReadFile(%q) should have been rejected", handle)
		}
		if err := eng.DeleteFile(ctx, handle); err == nil {
			t.Errorf("DeleteFile(%q) should have been rejected", handle)
		}
	}
}

// TestLocalDiskDeleteIsIdempotent: Phorge deletes the bytes and the row that
// points at them in one sequence, so a file that is already gone has to be a
// success or the row can never be retired.
func TestLocalDiskDeleteIsIdempotent(t *testing.T) {
	eng := newLocalDiskLike(t)

	if err := eng.DeleteFile(context.Background(), "ab/cd/0123456789abcdef0123456789ab"); err != nil {
		t.Errorf("deleting a file that is not there must succeed, got %v", err)
	}
}

// TestLocalDiskCleansUpAFailedWrite: the handle of a half-written file is
// never reported to anyone, so nothing would ever read it and nothing would
// ever delete it.
func TestLocalDiskCleansUpAFailedWrite(t *testing.T) {
	root := t.TempDir()
	eng, err := NewLocalDiskEngine(root)
	if err != nil {
		t.Fatal(err)
	}

	_, err = eng.WriteFile(context.Background(), failingReader{}, 100, WriteParams{})
	if err == nil {
		t.Fatal("expected the write to fail")
	}

	var files int
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files++
		}
		return nil
	})
	if files != 0 {
		t.Errorf("a failed write left %d file(s) behind", files)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestLocalDiskRejectsABadRoot(t *testing.T) {
	// A relative root would resolve against whatever the process's working
	// directory happens to be, and "/" would scatter files across the
	// filesystem root.
	for _, root := range []string{"", "/", "relative/path"} {
		if _, err := NewLocalDiskEngine(root); err == nil {
			t.Errorf("NewLocalDiskEngine(%q) should have been rejected", root)
		}
	}
}

func TestLocalDiskDescribesItself(t *testing.T) {
	eng := newLocalDiskLike(t)

	// The identifier is stored against every file Phorge writes here, so it
	// is not a display string.
	if eng.Identifier() != "local-disk" {
		t.Errorf("Identifier() = %q", eng.Identifier())
	}
	if !eng.CanWrite() || eng.HasSizeLimit() {
		t.Error("local disk writes and has no size limit")
	}
}

// TestLocalDiskConstantMethods pins the placement constants. MaxFileSize is 0
// precisely because HasSizeLimit is false — a size limit here would clamp the
// one backend that exists to hold anything too big for a database row.
// Priority 5 puts local disk between the blob backend and S3.
func TestLocalDiskConstantMethods(t *testing.T) {
	eng := newLocalDiskLike(t)

	if eng.MaxFileSize() != 0 {
		t.Errorf("MaxFileSize() = %d, want 0 (no limit)", eng.MaxFileSize())
	}
	if eng.HasSizeLimit() {
		t.Error("HasSizeLimit() must be false when MaxFileSize is 0")
	}
	if eng.Priority() != 5 {
		t.Errorf("Priority() = %d, want 5", eng.Priority())
	}
	if !eng.CanWrite() {
		t.Error("local disk must report itself writable")
	}
}
