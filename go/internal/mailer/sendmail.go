package mailer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// permanentExitCodes are the sysexits.h values that describe the message rather
// than the machine: a recipient nobody knows, a host that does not exist, a
// malformed input. Configuration and permission failures permit backend
// failover. Other exits leave acceptance unknown and must not be retried.
var permanentExitCodes = map[int]string{
	64: "EX_USAGE",
	65: "EX_DATAERR",
	66: "EX_NOINPUT",
	67: "EX_NOUSER",
	68: "EX_NOHOST",
}

type sendmailAdapter struct {
	path string
}

func newSendmailAdapter(opts map[string]string) (*sendmailAdapter, error) {
	path := opts["path"]
	if path == "" {
		path = "/usr/sbin/sendmail"
	}
	return &sendmailAdapter{path: path}, nil
}

func (a *sendmailAdapter) Type() string { return "sendmail" }

func (a *sendmailAdapter) Send(ctx context.Context, msg *contracts.EmailMessage) (string, error) {
	// -oi keeps a lone "." in the body from ending the message; -- stops a
	// recipient that starts with a dash from being read as a flag.
	args := append([]string{"-oi", "-f", msg.From.Address, "--"}, recipients(msg)...)
	cmd := exec.CommandContext(ctx, a.path, args...)
	cmd.Stdin = bytes.NewReader(buildMIME(msg))
	// A forked delivery helper can inherit these pipes after the main process
	// is killed. Bound waiting for its EOF independently of submission time.
	cmd.WaitDelay = time.Second
	var output sendmailOutput
	cmd.Stdout, cmd.Stderr = &output, &output

	err := cmd.Run()
	if err == nil || (cmd.ProcessState != nil && cmd.ProcessState.Success()) {
		// Local delivery reports no message id.
		// Exit 0 confirms acceptance even when inherited diagnostic pipes do
		// not close. Losing diagnostics cannot undo that accepted receipt.
		return "", nil
	}

	var startErr *exec.Error
	if errors.As(err, &startErr) {
		return "", &SafeRetryError{Err: err, Backend: true}
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && pathErr.Op == "fork/exec" {
		return "", &SafeRetryError{Err: err, Backend: true}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.ExitCode() == 77 || exitErr.ExitCode() == 78 {
			return "", &SafeRetryError{Err: fmt.Errorf("sendmail backend configuration failed"), Backend: true}
		}
		if name, ok := permanentExitCodes[exitErr.ExitCode()]; ok {
			return "", permanentf("sendmail: exit %d (%s): %s",
				exitErr.ExitCode(), name, output.buffer.String())
		}
	}
	return "", fmt.Errorf("sendmail: %w: %s", err, output.buffer.String())
}

// Drain all diagnostics without retaining unbounded output in memory.
// os/exec serializes writes when stdout and stderr share the same writer.
type sendmailOutput struct{ buffer bytes.Buffer }

func (w *sendmailOutput) Write(p []byte) (int, error) {
	if remaining := int(ProviderResponseLimit) - w.buffer.Len(); remaining > 0 {
		_, _ = w.buffer.Write(p[:min(len(p), remaining)])
	}
	return len(p), nil
}
