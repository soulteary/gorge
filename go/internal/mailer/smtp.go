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
	"time"

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

// Send uses a cancellable connection. Only a completed DATA response proves
// acceptance. QUIT failures after acceptance must not cause a resend.
func (a *smtpAdapter) Send(ctx context.Context, msg *contracts.EmailMessage) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(a.host, strconv.Itoa(a.port)))
	if err != nil {
		return "", &SafeRetryError{Err: err}
	}
	defer conn.Close()
	rawConn := conn
	stop := context.AfterFunc(ctx, func() { rawConn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	if a.protocol == "ssl" || a.protocol == "tls" {
		secured := tls.Client(conn, &tls.Config{ServerName: a.host, MinVersion: tls.VersionTLS12})
		if err := secured.HandshakeContext(ctx); err != nil {
			return "", &SafeRetryError{Err: err, Backend: true}
		}
		conn = secured
	}
	client, err := smtp.NewClient(conn, a.host)
	if err != nil {
		return "", &SafeRetryError{Err: err}
	}
	defer client.Close()
	if a.protocol != "ssl" && a.protocol != "tls" {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: a.host, MinVersion: tls.VersionTLS12}); err != nil {
				return "", &SafeRetryError{Err: err, Backend: true}
			}
		}
	}
	if a.user != "" {
		if err := client.Auth(smtp.PlainAuth("", a.user, a.password, a.host)); err != nil {
			return "", &SafeRetryError{Err: err, Backend: true}
		}
	}
	if err := client.Mail(msg.From.Address); err != nil {
		return "", classifySMTPError(err)
	}
	// Do not submit DATA if any recipient was rejected: the accepted RCPTs
	// have not received message content and can safely participate in a retry.
	for _, r := range recipients(msg) {
		if err := client.Rcpt(r); err != nil {
			return "", classifySMTPError(err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return "", classifySMTPError(err)
	}
	if _, err = w.Write(buildMIME(msg)); err != nil {
		return "", fmt.Errorf("smtp data write outcome unknown: %w", err)
	}
	if err = w.Close(); err != nil {
		var reply *textproto.Error
		if errors.As(err, &reply) {
			return "", classifySMTPError(err)
		}
		return "", fmt.Errorf("smtp data acceptance unknown: %w", err)
	}
	_ = client.Quit()
	return "", nil
}

func classifySMTPError(err error) error {
	if err == nil {
		return nil
	}
	var reply *textproto.Error
	if errors.As(err, &reply) && reply.Code >= 500 && reply.Code < 600 {
		return &PermanentError{Err: fmt.Errorf("smtp: %w", err)}
	}
	return &SafeRetryError{Err: fmt.Errorf("smtp: %w", err)}
}
