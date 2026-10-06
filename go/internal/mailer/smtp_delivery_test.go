package mailer

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

func fakeSubmissionSMTP(t *testing.T, accept bool) *smtpAdapter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "220 test\r\n")
		reader := textproto.NewReader(bufio.NewReader(conn))
		for {
			line, err := reader.ReadLine()
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "DATA") {
				fmt.Fprint(conn, "354 continue\r\n")
				if _, err = reader.ReadDotBytes(); err != nil {
					return
				}
				if !accept {
					return
				}
				fmt.Fprint(conn, "250 accepted\r\n")
				continue
			}
			if strings.HasPrefix(line, "QUIT") {
				return
			}
			fmt.Fprint(conn, "250 ok\r\n")
		}
	}()
	tcp := ln.Addr().(*net.TCPAddr)
	return &smtpAdapter{host: "127.0.0.1", port: tcp.Port}
}
func TestSMTPAcceptedDespiteQuitDisconnect(t *testing.T) {
	if _, err := fakeSubmissionSMTP(t, true).Send(context.Background(), testMessage()); err != nil {
		t.Fatalf("accepted mail became retryable: %v", err)
	}
}
func TestSMTPDataDisconnectIsUnknown(t *testing.T) {
	_, err := fakeSubmissionSMTP(t, false).Send(context.Background(), testMessage())
	if err == nil || CanRetry(err) || IsPermanent(err) {
		t.Fatalf("ambiguous DATA classified as safe: %v", err)
	}
}
func TestSMTPContextCancelsGreeting(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			defer conn.Close()
			bufio.NewReader(conn).ReadByte()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	addr := ln.Addr().(*net.TCPAddr)
	start := time.Now()
	_, err = (&smtpAdapter{host: "127.0.0.1", port: addr.Port}).Send(ctx, testMessage())
	if err == nil || !CanRetry(err) || time.Since(start) > time.Second {
		t.Fatalf("uncancellable pre-submit connection: %v", err)
	}
}
