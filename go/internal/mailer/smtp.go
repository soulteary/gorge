package mailer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"

	"github.com/soulteary/gorge/go/internal/contracts"
)

type smtpAdapter struct {
	host     string
	port     int
	user     string
	password string
	protocol string // "tls", "ssl", or "" for plain / STARTTLS
}

func newSMTPAdapter(opts map[string]string) (*smtpAdapter, error) {
	a := &smtpAdapter{
		host:     opts["host"],
		port:     25,
		protocol: opts["protocol"],
		user:     opts["user"],
		password: opts["password"],
	}
	if p, ok := opts["port"]; ok {
		if n, err := strconv.Atoi(p); err == nil {
			a.port = n
		}
	}
	if a.host == "" {
		a.host = "localhost"
	}
	return a, nil
}

func (a *smtpAdapter) Type() string { return "smtp" }

// Send transmits the message over SMTP.
//
// ctx is only consulted before the transaction starts: net/smtp predates
// context and offers no way to cancel one in flight. The dispatcher's retry
// loop is where cancellation actually takes effect for this backend.
func (a *smtpAdapter) Send(ctx context.Context, msg *contracts.EmailMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	addr := net.JoinHostPort(a.host, strconv.Itoa(a.port))
	raw := buildMIME(msg)

	var auth smtp.Auth
	if a.user != "" {
		auth = smtp.PlainAuth("", a.user, a.password, a.host)
	}

	var err error
	switch a.protocol {
	case "ssl", "tls":
		err = a.sendTLS(addr, auth, msg.From.Address, recipients(msg), raw)
	default:
		err = smtp.SendMail(addr, auth, msg.From.Address, recipients(msg), raw)
	}

	// SMTP has no message id to report back; the receiving MTA mints one.
	return "", classifySMTPError(err)
}

// classifySMTPError applies the reply-code rule RFC 5321 defines: a 5xx is a
// permanent negative completion — unknown mailbox, relay refused, message
// rejected — while a 4xx is explicitly "try again later". Anything without a
// reply code at all is a connection or TLS failure, which is transient by the
// same reasoning.
func classifySMTPError(err error) error {
	if err == nil {
		return nil
	}
	var replyErr *textproto.Error
	if errors.As(err, &replyErr) && replyErr.Code >= 500 && replyErr.Code < 600 {
		return &PermanentError{Err: fmt.Errorf("smtp: %w", err)}
	}
	return fmt.Errorf("smtp: %w", err)
}

// sendTLS runs the transaction over an implicit TLS connection (port 465), the
// one case net/smtp's SendMail cannot do on its own.
func (a *smtpAdapter) sendTLS(addr string, auth smtp.Auth, from string, to []string, raw []byte) error {
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: a.host})
	if err != nil {
		return fmt.Errorf("tls dial: %w", err)
	}
	client, err := smtp.NewClient(conn, a.host)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer func() { _ = client.Close() }()

	if auth != nil {
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("smtp mail: %w", err)
	}
	for _, r := range to {
		if err := client.Rcpt(r); err != nil {
			return fmt.Errorf("smtp rcpt %s: %w", r, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close data: %w", err)
	}
	return client.Quit()
}
