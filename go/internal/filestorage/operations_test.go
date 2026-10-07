package filestorage

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestUploadProcessInterruption(t *testing.T) {
	if root := os.Getenv("GORGE_UPLOAD_CRASH_ROOT"); root != "" {
		u, err := NewUploads(root)
		if err != nil {
			os.Exit(81)
		}
		id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		if _, err = u.Create(context.Background(), id, 3); err != nil {
			os.Exit(82)
		}
		if _, err = u.Put(context.Background(), id, 0, []byte("abc")); err != nil {
			os.Exit(83)
		}
		// Exit without cleanup after bytes + manifest acknowledgement were durable.
		if _, err = fmt.Fprintln(os.Stdout, "durable"); err != nil {
			os.Exit(84)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	root := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestUploadProcessInterruption$")
	cmd.Env = append(os.Environ(), "GORGE_UPLOAD_CRASH_ROOT="+root)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ack := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(pipe).ReadString('\n'); ack <- line }()
	select {
	case line := <-ack:
		if line != "durable\n" {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("no durable marker", line)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("crash fixture timeout")
	}
	_ = cmd.Process.Kill()
	err = cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != -1 {
		t.Fatalf("unexpected crash fixture: %v", err)
	}
	u, err := NewUploads(root)
	if err != nil {
		t.Fatal(err)
	}
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err = u.Put(t.Context(), id, 0, []byte("abc")); err != nil {
		t.Fatal("lost acknowledgement replay", err)
	}
	if _, err = u.Put(t.Context(), id, 0, []byte("xyz")); !errors.Is(err, ErrUploadConflict) {
		t.Fatal("conflicting replay", err)
	}
	if _, err = u.Complete(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err = u.Cancel(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	usage, err := u.Usage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if usage.Sessions["cancelled"] != 1 || usage.LogicalBytes == 0 || usage.InvalidManifests != 0 {
		t.Fatalf("lost tombstone inventory: %+v", usage)
	}
	if _, err = u.Create(t.Context(), id, 3); !errors.Is(err, ErrUploadExpired) {
		t.Fatal("cancelled identity resurrected", err)
	}
}

func TestUploadInventoryPagesAndRestore(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	u, err := NewUploads(root)
	if err != nil {
		t.Fatal(err)
	}
	id := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err = u.Create(ctx, id, 3); err != nil {
		t.Fatal(err)
	}
	if _, err = u.Put(ctx, id, 0, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err = u.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	whole, err := u.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var entries int
	var bytes int64
	cursor := ""
	pages := 0
	for {
		page, e := u.UsagePage(ctx, cursor, 2)
		if e != nil {
			t.Fatal(e)
		}
		entries += page.Entries
		bytes += page.LogicalBytes
		pages++
		if !page.Truncated {
			break
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			t.Fatal("stuck cursor")
		}
		cursor = page.NextCursor
	}
	if pages < 2 || entries != whole.Entries || bytes != whole.LogicalBytes {
		t.Fatal("incomplete paginated inventory", entries, bytes, whole)
	}
	backup := t.TempDir()
	if err = os.CopyFS(backup, os.DirFS(root)); err != nil {
		t.Fatal(err)
	}
	restored := t.TempDir()
	if err = os.CopyFS(restored, os.DirFS(backup)); err != nil {
		t.Fatal(err)
	}
	v, err := NewUploads(restored)
	if err != nil {
		t.Fatal(err)
	}
	if v.volumeID != u.volumeID {
		t.Fatal("restore changed volume identity")
	}
	if _, err = v.Create(ctx, id, 3); !errors.Is(err, ErrUploadExpired) {
		t.Fatal("restored tombstone resurrected", err)
	}
	if _, err = v.UsagePage(ctx, "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", 2); err == nil {
		t.Fatal("stale cursor accepted")
	}
}
