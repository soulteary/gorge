package mailer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

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

	output, err := cmd.CombinedOutput()
	if err == nil {
		// Local delivery reports no message id.
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
				exitErr.ExitCode(), name, string(output))
		}
	}
	return "", fmt.Errorf("sendmail: %w: %s", err, string(output))
}
