package filestorage

import (
	"context"
	"strings"
	"testing"
)

// newBlobEngine builds the engine over a pool pointed at a port nothing
// listens on. sql.Open does not dial, so this is enough for everything that
// happens before a query — and for the readiness probe, which is the one thing
// here worth checking against an unreachable database.
func newBlobEngine(t *testing.T, maxSize int64) *MySQLBlobEngine {
	t.Helper()

	db, err := OpenDB("phorge:@tcp(127.0.0.1:1)/phorge_file?timeout=1s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewMySQLBlobEngine(db, maxSize)
}

func TestMySQLBlobDescribesItself(t *testing.T) {
	eng := newBlobEngine(t, 1000)

	// `blob` rather than `mysql`, because that is the identifier Phorge's own
	// MySQL engine uses and both write the same table.
	if eng.Identifier() != "blob" {
		t.Errorf("Identifier() = %q, want blob", eng.Identifier())
	}
	if eng.Priority() != 1 {
		t.Errorf("Priority() = %d, want 1", eng.Priority())
	}
	if !eng.HasSizeLimit() || eng.MaxFileSize() != 1000 {
		t.Error("the blob engine always has a size limit")
	}
	if !eng.CanWrite() {
		t.Error("a positive size limit means it accepts writes")
	}
	// A zero limit is how an operator turns the backend off, so it must not
	// merely mean "unlimited".
	if newBlobEngine(t, 0).CanWrite() {
		t.Error("a zero size limit must disable writes")
	}
}

// TestMySQLBlobRefusesAnOversizedFileBeforeReading: the row is buffered whole,
// so the limit has to be enforced on the declared length first. Reading 20 MB
// into memory and then refusing it would be the failure mode the limit exists
// to prevent.
func TestMySQLBlobRefusesAnOversizedFileBeforeReading(t *testing.T) {
	eng := newBlobEngine(t, 10)

	// A reader that fails if it is touched at all: reaching it means the size
	// check did not come first.
	if _, err := eng.WriteFile(context.Background(), failingReader{}, 500, WriteParams{}); err == nil {
		t.Fatal("expected the oversized file to be refused")
	} else if !strings.Contains(err.Error(), "exceeds the blob limit") {
		t.Errorf("expected a size refusal, got %v", err)
	}
}

// TestMySQLBlobRefusesABodyLongerThanItsDeclaredLength: a Content-Length is a
// claim, not a promise. Without the second check a caller could declare 10
// bytes and send 10 MB.
func TestMySQLBlobRefusesABodyLongerThanItsDeclaredLength(t *testing.T) {
	eng := newBlobEngine(t, 10)

	_, err := eng.WriteFile(context.Background(), strings.NewReader(strings.Repeat("x", 500)), 5, WriteParams{})
	if err == nil {
		t.Fatal("expected the write to be refused")
	}
	if !strings.Contains(err.Error(), "exceeds the blob limit") {
		t.Errorf("expected a size refusal, got %v", err)
	}
}

// TestMySQLBlobRejectsAMalformedHandle: MySQL would coerce a non-numeric
// string to 0 and answer "no rows", so without the check a malformed handle
// would report the same thing as a file that was genuinely deleted.
func TestMySQLBlobRejectsAMalformedHandle(t *testing.T) {
	eng := newBlobEngine(t, 1000)
	ctx := context.Background()

	for _, handle := range []string{"", "ab/cd/ef", "12x", "-1", "1 OR 1=1"} {
		if _, _, err := eng.ReadFile(ctx, handle); err == nil ||
			!strings.Contains(err.Error(), "malformed blob handle") {
			t.Errorf("ReadFile(%q) = %v, want a malformed handle error", handle, err)
		}
		if err := eng.DeleteFile(ctx, handle); err == nil ||
			!strings.Contains(err.Error(), "malformed blob handle") {
			t.Errorf("DeleteFile(%q) = %v, want a malformed handle error", handle, err)
		}
	}
}

// TestMySQLBlobReadyReportsAnUnreachableDatabase is the readiness half. What
// it must *not* do is check whether `file_storageblob` exists: that table is
// created by Phorge's bin/storage upgrade, and the Phorge container waits for
// this service to be healthy first, so such a check would deadlock the stack.
func TestMySQLBlobReadyReportsAnUnreachableDatabase(t *testing.T) {
	err := newBlobEngine(t, 1000).Ready(context.Background())
	if err == nil {
		t.Fatal("expected the unreachable database to be reported")
	}
	if !strings.Contains(err.Error(), "ping database") {
		t.Errorf("expected a ping failure, got %v", err)
	}
}
