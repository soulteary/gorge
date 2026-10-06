package integrations

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"

	"golang.org/x/net/html/charset"
)

const maxMail = 6 * 1024 * 1024

// ParseRaw replaces the PHP mailparse dependency. Parsing is bounded before
// and after transfer/charset decoding, including nested MIME parts.
func ParseRaw(encoded string) (Inbound, error) {
	m := Inbound{Provider: "raw", Headers: map[string]string{}}
	if len(encoded) > (maxMail+2)/3*4 {
		return m, errors.New("raw message too large")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) > maxMail {
		return m, errors.New("invalid raw message")
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return m, errors.New("invalid RFC mail")
	}
	decoder := &mime.WordDecoder{CharsetReader: func(label string, r io.Reader) (io.Reader, error) { return charset.NewReaderLabel(label, r) }}
	for k, values := range msg.Header {
		value := strings.Join(values, ", ")
		decoded, e := decoder.DecodeHeader(value)
		if e != nil {
			return m, errors.New("invalid encoded header")
		}
		m.Headers[strings.ToLower(k)] = decoded
	}
	parts, total := 0, 0
	var parse func(textproto.MIMEHeader, io.Reader, int) error
	parse = func(h textproto.MIMEHeader, r io.Reader, depth int) error {
		parts++
		if depth > 16 || parts > 200 {
			return errors.New("too many MIME parts")
		}
		typ, params, e := mime.ParseMediaType(h.Get("Content-Type"))
		if h.Get("Content-Type") == "" {
			typ = "text/plain"
			params = map[string]string{}
			e = nil
		}
		if e != nil {
			return errors.New("invalid MIME content type")
		}
		switch strings.ToLower(h.Get("Content-Transfer-Encoding")) {
		case "base64":
			r = base64.NewDecoder(base64.StdEncoding, r)
		case "quoted-printable":
			r = quotedprintable.NewReader(r)
		case "", "7bit", "8bit", "binary":
		default:
			return errors.New("unsupported transfer encoding")
		}
		if strings.HasPrefix(typ, "multipart/") {
			if params["boundary"] == "" {
				return errors.New("missing MIME boundary")
			}
			reader := multipart.NewReader(r, params["boundary"])
			for {
				part, e := reader.NextRawPart()
				if e == io.EOF {
					return nil
				}
				if e != nil {
					return errors.New("invalid MIME multipart")
				}
				e = parse(part.Header, part, depth+1)
				_ = part.Close()
				if e != nil {
					return e
				}
			}
		}
		disposition, dp, e := mime.ParseMediaType(h.Get("Content-Disposition"))
		if h.Get("Content-Disposition") == "" {
			e = nil
		}
		if e != nil {
			return errors.New("invalid MIME disposition")
		}
		name := dp["filename"]
		if name == "" {
			name = params["name"]
		}
		bodyPart := (typ == "text/plain" || typ == "text/html") && disposition != "attachment" && (name == "" || disposition == "inline")
		if bodyPart && params["charset"] != "" {
			r, e = charset.NewReaderLabel(params["charset"], r)
			if e != nil {
				return errors.New("unsupported MIME charset")
			}
		}
		data, e := io.ReadAll(io.LimitReader(r, int64(maxMail-total+1)))
		if e != nil {
			return errors.New("invalid MIME content")
		}
		total += len(data)
		if total > maxMail {
			return errors.New("decoded message too large")
		}
		if bodyPart {
			if typ == "text/plain" {
				m.Text += string(data)
			} else {
				m.HTML += string(data)
			}
			return nil
		}
		if name == "" {
			name = "attachment"
		}
		name, e = decoder.DecodeHeader(name)
		if e != nil {
			return errors.New("invalid attachment name")
		}
		m.Attachments = append(m.Attachments, Attachment{Name: name, Data: base64.StdEncoding.EncodeToString(data)})
		return nil
	}
	err = parse(textproto.MIMEHeader(msg.Header), msg.Body, 0)
	return m, err
}
