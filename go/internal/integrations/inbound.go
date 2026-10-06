package integrations

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/soulteary/gorge/go/internal/platform/conduitclient"
	"regexp"
	"strings"
	"time"
)

type Attachment struct {
	Name string `json:"name"`
	Data string `json:"data"`
}
type Inbound struct {
	IngressID   string            `json:"ingressID,omitempty"`
	Provider    string            `json:"provider"`
	Headers     map[string]string `json:"headers"`
	Text        string            `json:"text"`
	HTML        string            `json:"html"`
	Attachments []Attachment      `json:"attachments"`
}
type Intake struct {
	IngressID         string            `json:"ingressID,omitempty"`
	Provider          string            `json:"provider"`
	Fields            map[string]string `json:"fields"`
	Postmark          json.RawMessage   `json:"postmark,omitempty"`
	Attachments       []Attachment      `json:"attachments"`
	Raw               string            `json:"raw,omitempty"`
	ProcessDuplicates bool              `json:"processDuplicates,omitempty"`
}

func Normalize(in Intake) (Inbound, error) {
	m := Inbound{Provider: in.Provider, Headers: map[string]string{}, Attachments: in.Attachments}
	put := func(k, v string) { m.Headers[strings.ToLower(strings.TrimSpace(k))] = v }
	switch in.Provider {
	case "raw":
		var err error
		m, err = ParseRaw(in.Raw)
		if err != nil {
			return m, err
		}
		if in.ProcessDuplicates {
			m.Headers["message-id"] = in.IngressID
		}
	case "mailgun":
		var headers [][2]string
		if raw := in.Fields["message-headers"]; raw != "" {
			if json.Unmarshal([]byte(raw), &headers) != nil {
				return m, errors.New("invalid mail headers")
			}
		}
		for _, h := range headers {
			put(h[0], h[1])
		}
		put("to", in.Fields["recipient"])
		put("from", in.Fields["from"])
		put("subject", in.Fields["subject"])
		m.Text = in.Fields["stripped-text"]
		m.HTML = in.Fields["stripped-html"]
	case "sendgrid":
		for _, h := range strings.Split(in.Fields["headers"], "\n") {
			if k, v, ok := strings.Cut(h, ":"); ok {
				put(k, strings.TrimSpace(v))
			}
		}
		put("to", in.Fields["to"])
		put("from", in.Fields["from"])
		put("subject", in.Fields["subject"])
		m.Text = in.Fields["text"]
		m.HTML = in.Fields["html"]
	case "postmark":
		var p struct {
			To, From, Cc, Subject, TextBody, HtmlBody, MessageID string
			Headers                                              []struct{ Name, Value string }
			Attachments                                          []struct{ Name, Content string }
		}
		if json.Unmarshal(in.Postmark, &p) != nil {
			return m, errors.New("invalid Postmark payload")
		}
		for _, h := range p.Headers {
			put(h.Name, h.Value)
		}
		put("to", p.To)
		put("from", p.From)
		put("cc", p.Cc)
		put("subject", p.Subject)
		if m.Headers["message-id"] == "" {
			put("message-id", p.MessageID)
		}
		m.Text = p.TextBody
		m.HTML = p.HtmlBody
		for _, a := range p.Attachments {
			m.Attachments = append(m.Attachments, Attachment{Name: a.Name, Data: a.Content})
		}
	default:
		return m, errors.New("unsupported provider")
	}
	if m.Headers["to"] == "" || m.Headers["from"] == "" || len(m.Headers) > 100 || len(m.Attachments) > 50 {
		return m, errors.New("invalid inbound message")
	}
	if m.Headers["message-id"] == "" {
		if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(in.IngressID) {
			return m, errors.New("mail without Message-ID requires an ingress identity")
		}
		m.IngressID = in.IngressID
	}
	size := len(m.Text) + len(m.HTML)
	for k, v := range m.Headers {
		if len(k) > 128 || len(v) > 65536 {
			return m, errors.New("oversized header")
		}
		size += len(k) + len(v)
	}
	for _, a := range m.Attachments {
		if len(a.Name) > 255 {
			return m, errors.New("invalid attachment name")
		}
		b, e := base64.StdEncoding.DecodeString(a.Data)
		if e != nil {
			return m, errors.New("invalid attachment encoding")
		}
		size += len(b)
	}
	if size > 6*1024*1024 {
		return m, errors.New("inbound message exceeds 6 MiB")
	}
	return m, nil
}

// Replay is safe at the PHP receipt boundary. A business execution interrupted
// after its processing marker is retained as unknown instead of executed twice.
func (s Store) Relay(ctx context.Context, c *conduitclient.Client) error {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	var raw []byte
	var attempts int
	e = tx.QueryRowContext(ctx, "SELECT id,payload,attempts FROM gorge_integration_inbox WHERE state IN ('pending','retry','processing') AND due<=? ORDER BY due,id LIMIT 1 FOR UPDATE SKIP LOCKED", time.Now().Unix()).Scan(&id, &raw, &attempts)
	if e != nil {
		return e
	}
	now := time.Now().Unix()
	_, e = tx.ExecContext(ctx, "UPDATE gorge_integration_inbox SET state='processing',due=?,attempts=attempts+1 WHERE id=?", now+120, id)
	if e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	var m Inbound
	if e = json.Unmarshal(raw, &m); e != nil {
		return e
	}
	call, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	r, e := c.Call(call, "integration.inbound", map[string]any{"eventID": id, "message": m})
	state := "done"
	due := now
	if e != nil {
		state = "retry"
		wait := int64(30) << min(attempts, 6)
		due = now + wait
	} else {
		var v struct {
			State string `json:"state"`
		}
		if json.Unmarshal(r.Result, &v) != nil {
			return errors.New("invalid inbound result")
		}
		if v.State == "unknown" {
			state = "unknown"
		} else if v.State != "done" {
			return errors.New("unexpected inbound state")
		}
	}
	finish, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	_, save := s.DB.ExecContext(finish, "UPDATE gorge_integration_inbox SET state=?,due=? WHERE id=? AND state='processing' AND attempts=?", state, due, id, attempts+1)
	return save
}
