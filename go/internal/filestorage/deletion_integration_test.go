package filestorage

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestRealDeletionOutbox(t *testing.T) {
	dsn := os.Getenv("GORGE_TEST_FILE_LIFECYCLE_DSN")
	if dsn == "" {
		t.Skip("set GORGE_TEST_FILE_LIFECYCLE_DSN to isolated test DB")
	}
	db, e := OpenDB(dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = db.Close() }()
	var name string
	if e = db.QueryRow("SELECT DATABASE()").Scan(&name); e != nil || name != "gorge_lifecycle_test" {
		t.Fatal("requires isolated gorge_lifecycle_test database")
	}
	for _, q := range []string{"DROP TABLE IF EXISTS file_gorgedeletion", "DROP TABLE IF EXISTS file", `CREATE TABLE file(id INT PRIMARY KEY,storageEngine VARBINARY(32),storageHandle VARBINARY(255),KEY target(storageEngine,storageHandle(64))) ENGINE=InnoDB`} {
		if _, e = db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	// Match the shipped PHP migration, rather than a second test-only schema.
	raw, e := os.ReadFile("../../../../phorge-fork/resources/sql/autopatches/20261007.file.01.gorgedeletion.sql")
	if e != nil {
		t.Fatal(e)
	}
	q := strings.ReplaceAll(string(raw), "{$NAMESPACE}_file.", "")
	q = strings.ReplaceAll(q, "{$COLLATE_TEXT}", "utf8mb4_bin")
	if _, e = db.Exec(q); e != nil {
		t.Fatal(e)
	}
	root := t.TempDir()
	eng, e := NewLocalDiskEngine(root)
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	handle, e := eng.WriteFile(ctx, strings.NewReader("bytes"), 5, WriteParams{})
	if e != nil {
		t.Fatal(e)
	}
	target := "local-disk/" + handle
	if _, e = db.Exec(`INSERT INTO file VALUES(1,'gorge',?)`, target); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`INSERT INTO file_gorgedeletion(eventID,storageEngine,storageHandle,lastError) VALUES('first','gorge',?,'')`, target); e != nil {
		t.Fatal(e)
	}
	router := NewRouter([]StorageEngine{eng})
	if e = ProcessDeletion(ctx, db, router, nil); e != nil {
		t.Fatal(e)
	}
	r, _, e := eng.ReadFile(ctx, handle)
	if e != nil {
		t.Fatal("deleted referenced file", e)
	}
	_ = r.Close()
	if _, e = db.Exec(`DELETE FROM file WHERE id=1`); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`INSERT INTO file_gorgedeletion(eventID,storageEngine,storageHandle,lastError) VALUES('last','gorge',?,'')`, target); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- ProcessDeletion(ctx, db, router, nil) }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if r, _, e = eng.ReadFile(ctx, handle); e == nil {
		_ = r.Close()
		t.Fatal("physical file remains")
	}
	var state string
	if e = db.QueryRow(`SELECT state FROM file_gorgedeletion WHERE eventID='last'`).Scan(&state); e != nil || state != "done" {
		t.Fatal(state, e)
	}
	if _, e = db.Exec(`INSERT INTO file_gorgedeletion(eventID,storageEngine,storageHandle,lastError) VALUES('retry','gorge','missing/handle','')`); e != nil {
		t.Fatal(e)
	}
	if e = ProcessDeletion(ctx, db, router, nil); e != nil {
		t.Fatal(e)
	}
	var attempts, next int64
	if e = db.QueryRow(`SELECT state,attempts,nextAttempt FROM file_gorgedeletion WHERE eventID='retry'`).Scan(&state, &attempts, &next); e != nil || state != "pending" || attempts != 1 || next <= 0 {
		t.Fatal(state, attempts, next, e)
	}
}
