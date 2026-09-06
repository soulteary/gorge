package mailer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// permanentExitCodes are the sysexits.h values that describe the message rather
// than the machine: a recipient nobody knows, a host that does not exist, a
// malformed input, a configuration or permission problem. Re-submitting the
// same message produces the same code.
//
// Everything else — EX_TEMPFAIL(75) above all, but also EX_OSERR(71),
// EX_IOERR(74) and any code not listed here — is treated as transient. That
// default is the safe one: an unrecognised code retried needlessly costs worker
// cycles, while one wrongly called permanent drops the mail.
var permanentExitCodes = map[int]string{
	64: "EX_USAGE",
	65: "EX_DATAERR",
	66: "EX_NOINPUT",
	67: "EX_NOUSER",
	68: "EX_NOHOST",
	77: "EX_NOPERM",
	78: "EX_CONFIG",
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

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if name, ok := permanentExitCodes[exitErr.ExitCode()]; ok {
			return "", permanentf("sendmail: exit %d (%s): %s",
				exitErr.ExitCode(), name, string(output))
		}
	}
	return "", fmt.Errorf("sendmail: %w: %s", err, string(output))
}
