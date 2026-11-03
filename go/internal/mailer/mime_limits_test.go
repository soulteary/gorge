package mailer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

func TestBuildMIMELargePartsRespectLineLimitsAndRoundTrip(t *testing.T) {
	text := strings.Repeat("Synthetic native body. ", 4096)
	html := strings.Repeat("<p>Synthetic HTML.</p>", 4096)
	attachment := bytes.Repeat([]byte{0, 1, 2, 250, 251, 252}, 9000)
	msg := &contracts.EmailMessage{From: contracts.Address{Address: "sender@example.invalid"}, To: []contracts.Address{{Address: "recipient@example.invalid"}}, Subject: "Synthetic", TextBody: text, HTMLBody: html, Attachments: []contracts.Attachment{{Filename: "synthetic.bin", MimeType: "application/octet-stream", Data: base64.StdEncoding.EncodeToString(attachment[:57]) + "\r\n" + base64.StdEncoding.EncodeToString(attachment[57:])}}}
	raw := buildMIME(msg)
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string][]byte{}
	var visit func(textproto.MIMEHeader, io.Reader)
	visit = func(header textproto.MIMEHeader, reader io.Reader) {
		kind, params, err := mime.ParseMediaType(header.Get("Content-Type"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(kind, "multipart/") {
			parts := multipart.NewReader(reader, params["boundary"])
			for {
				part, err := parts.NextRawPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				visit(part.Header, part)
			}
			return
		}
		if header.Get("Content-Transfer-Encoding") != "base64" {
			t.Fatal("expected base64 leaf")
		}
		encoded, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(encoded, []byte("\r\n")) {
			if len(line) > 76 {
				t.Fatalf("encoded %s line has %d characters, max76", kind, len(line))
			}
		}
		decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(encoded)))
		if err != nil {
			t.Fatal(err)
		}
		seen[kind] = decoded
	}
	visit(textproto.MIMEHeader(parsed.Header), parsed.Body)
	if !bytes.Equal(seen["text/plain"], []byte(text)) || !bytes.Equal(seen["text/html"], []byte(html)) || !bytes.Equal(seen["application/octet-stream"], attachment) {
		t.Fatal("decoded native body/HTML/attachment bytes changed")
	}
}

func TestSMTPLargeBodyAcceptedByStrictLineReceiver(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	type observed struct {
		raw      []byte
		longLine bool
		err      error
	}
	done := make(chan observed, 1)
	go func() {
		result := observed{}
		defer func() { done <- result }()
		conn, err := listener.Accept()
		if err != nil {
			result.err = err
			return
		}
		defer func() { _ = conn.Close() }()
		if err := conn.SetDeadline(time.Now().Add(8 * time.Second)); err != nil {
			result.err = err
			return
		}
		reader := bufio.NewReaderSize(conn, 1024)
		reply := func(value string) bool {
			_, err := io.WriteString(conn, value)
			if err != nil {
				result.err = err
			}
			return err == nil
		}
		if !reply("220 synthetic ESMTP\r\n") {
			return
		}
		var data bytes.Buffer
		for {
			line, err := reader.ReadSlice('\n')
			if err != nil {
				result.err = err
				return
			}
			command := strings.ToUpper(strings.TrimSpace(string(line)))
			switch {
			case strings.HasPrefix(command, "EHLO "), strings.HasPrefix(command, "HELO "), strings.HasPrefix(command, "MAIL FROM:"), strings.HasPrefix(command, "RCPT TO:"):
				if !reply("250 synthetic\r\n") {
					return
				}
			case command == "DATA":
				if !reply("354 send data\r\n") {
					return
				}
				for {
					part, readErr := reader.ReadSlice('\n')
					if readErr == bufio.ErrBufferFull || len(part) > 1000 {
						result.longLine = true
						reply("500 5.2.3 Line too long\r\n")
						return
					}
					if readErr != nil {
						result.err = readErr
						return
					}
					if bytes.Equal(part, []byte(".\r\n")) {
						break
					}
					if len(part) > 1 && part[0] == '.' {
						part = part[1:]
					}
					if data.Len()+len(part) > 512<<10 {
						result.err = fmt.Errorf("synthetic message budget exceeded")
						return
					}
					_, _ = data.Write(part)
				}
				result.raw = append([]byte(nil), data.Bytes()...)
				if !reply("250 accepted\r\n") {
					return
				}
			case command == "QUIT":
				reply("221 bye\r\n")
				return
			default:
				reply("500 unsupported\r\n")
				return
			}
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	adapter, err := newSMTPAdapter(map[string]string{"host": "127.0.0.1", "port": fmt.Sprint(port)})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("Synthetic native body. ", 4096)
	msg := &contracts.EmailMessage{From: contracts.Address{Address: "sender@example.invalid"}, To: []contracts.Address{{Address: "recipient@example.invalid"}}, Subject: "Synthetic", TextBody: text}
	_, sendErr := adapter.Send(context.Background(), msg)
	var result observed
	select {
	case result = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("strict SMTP receiver cleanup timeout")
	}
	if sendErr != nil || result.err != nil || result.longLine {
		t.Fatalf("strict SMTP submission refused: send=%v receiver=%v longLine=%v", sendErr, result.err, result.longLine)
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(result.raw))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, parsed.Body))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, []byte(text)) {
		t.Fatal("SMTP changed decoded body bytes")
	}
}

func TestBase64LineFoldingEmptyAndSplitBoundaries(t *testing.T) {
	for _, size := range []int{0, 76, 77, 152} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			// Formatting must preserve every character even at a non-quantum boundary;
			// this is not a claim that arbitrary input is valid base64.
			canonical := strings.Repeat("A", size)
			var b strings.Builder
			writeBase64Lines(&b, canonical)
			if !strings.HasSuffix(b.String(), "\r\n") {
				t.Fatal("final CRLF missing")
			}
			lines := strings.Split(strings.TrimSuffix(b.String(), "\r\n"), "\r\n")
			for _, line := range lines {
				if len(line) > 76 {
					t.Fatal("encoded line exceeded76")
				}
			}
			if strings.Join(lines, "") != canonical {
				t.Fatal("folding changed encoded characters")
			}
		})
	}
}
