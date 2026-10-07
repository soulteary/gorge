package mailer

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSendmailCancellationBoundsInheritedPipes(t *testing.T) {
	dir := t.TempDir()
	pidPath, path := filepath.Join(dir, "child-pid"), filepath.Join(dir, "sendmail")
	t.Setenv("GORGE_TEST_SENDMAIL_CHILD_PID", pidPath)
	script := "#!/bin/sh\ncat >/dev/null\nsleep 60 &\necho $! > \"$GORGE_TEST_SENDMAIL_CHILD_PID\"\nwait\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := newSendmailAdapter(map[string]string{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := a.Send(ctx, testMessage()); finished <- err }()
	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for pid == 0 && time.Now().Before(deadline) {
		if value, err := os.ReadFile(pidPath); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(value)))
		}
		if pid == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if pid == 0 {
		t.Fatal("sendmail fixture did not start its pipe-holding child")
	}
	t.Cleanup(func() {
		if child, err := os.FindProcess(pid); err == nil {
			_ = child.Kill()
		}
	})
	cancel()
	select {
	case err := <-finished:
		if err == nil || CanRetry(err) || IsPermanent(err) {
			t.Fatalf("cancelled submission must have unknown outcome: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("inherited output pipes exceeded the cancellation cleanup budget")
	}
}

func TestSendmailDiagnosticsAreBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sendmail")
	script := "#!/bin/sh\ncat >/dev/null\ndd if=/dev/zero bs=1024 count=256 2>/dev/null\nexit 75\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := newSendmailAdapter(map[string]string{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = a.Send(ctx, testMessage())
	if err == nil || len(err.Error()) > int(ProviderResponseLimit)+128 {
		t.Fatalf("sendmail diagnostics exceeded their memory budget: %v", err == nil)
	}
	if CanRetry(err) || IsPermanent(err) {
		t.Fatal("temporary local exit does not prove nonacceptance")
	}
}

func TestSendmailAcceptedParentDoesNotWaitForDiagnosticChild(t *testing.T) {
	dir := t.TempDir()
	pidPath, path := filepath.Join(dir, "child-pid"), filepath.Join(dir, "sendmail")
	t.Setenv("GORGE_TEST_SENDMAIL_CHILD_PID", pidPath)
	script := "#!/bin/sh\ncat >/dev/null\nsleep 60 &\necho $! > \"$GORGE_TEST_SENDMAIL_CHILD_PID\"\nexit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := newSendmailAdapter(map[string]string{"path": path})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, sendErr := a.Send(ctx, testMessage())
	if value, err := os.ReadFile(pidPath); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(value))); err == nil {
			if child, err := os.FindProcess(pid); err == nil {
				_ = child.Kill()
			}
		}
	}
	if sendErr != nil {
		t.Fatalf("accepted exit 0 must remain accepted after losing diagnostics: %v", sendErr)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("accepted sendmail waited for its diagnostic child")
	}
}
