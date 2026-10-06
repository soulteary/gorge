package integrations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrConflict = errors.New("identity conflict")
var ErrUnknown = errors.New("external outcome unknown; inspect before retrying")

type Outcome struct {
	State   string            `json:"state"`
	Result  json.RawMessage   `json:"result,omitempty"`
	Status  int               `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}
type Store struct{ DB *sql.DB }

func digest(v any) (string, []byte, error) {
	raw, e := json.Marshal(v)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:]), raw, e
}
func (s Store) Ready(ctx context.Context) error {
	if e := requireInnoDB(ctx, s.DB, []string{"gorge_integration_effect", "gorge_integration_inbox", "gorge_integration_resolution"}); e != nil {
		return e
	}
	for _, q := range []string{"SELECT id,digest,state,result,httpStatus,updated FROM gorge_integration_effect LIMIT 0", "SELECT id,provider,payload,state,due,attempts FROM gorge_integration_inbox LIMIT 0", "SELECT domain,identity,digest,state,operator,evidence,created FROM gorge_integration_resolution LIMIT 0"} {
		r, e := s.DB.QueryContext(ctx, q)
		if e != nil {
			return e
		}
		_ = r.Close()
	}
	return nil
}

// External effects are never silently retried after an ambiguous submission.
// Only an explicit 429 rejection is eligible for retry. Lost replies replay
// the committed result, and a crashed in-flight submit remains unknown.
func (s Store) Effect(ctx context.Context, id, hash, kind string, send func() (json.RawMessage, int, error)) (Outcome, error) {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return Outcome{}, e
	}
	defer func() { _ = tx.Rollback() }()
	_, e = tx.ExecContext(ctx, "INSERT INTO gorge_integration_effect(id,digest,kind,state,result,httpStatus,updated) VALUES(?,?,?,'prepared',NULL,0,?) ON DUPLICATE KEY UPDATE id=id", id, hash, kind, time.Now().Unix())
	if e != nil {
		return Outcome{}, e
	}
	var old, state string
	var result []byte
	var status int
	e = tx.QueryRowContext(ctx, "SELECT digest,state,result,httpStatus FROM gorge_integration_effect WHERE id=? FOR UPDATE", id).Scan(&old, &state, &result, &status)
	if e != nil {
		return Outcome{}, e
	}
	if old != hash {
		return Outcome{}, ErrConflict
	}
	if state == "accepted" || state == "rejected" {
		return Outcome{State: state, Result: result, Status: status}, nil
	}
	if state == "submitting" || state == "unknown" {
		return Outcome{}, ErrUnknown
	}
	_, e = tx.ExecContext(ctx, "UPDATE gorge_integration_effect SET state='submitting',updated=? WHERE id=?", time.Now().Unix(), id)
	if e != nil {
		return Outcome{}, e
	}
	if e = tx.Commit(); e != nil {
		return Outcome{}, e
	}
	result, status, e = send()
	state = "accepted"
	if e != nil {
		state = "unknown"
	} else if status == 429 {
		state = "retry"
	} else if status >= 400 && status < 500 {
		state = "rejected"
	} else if status < 200 || status >= 300 {
		state = "unknown"
	}
	result = minimalResult(kind, result)
	// Use an independent bounded context to commit the outcome even if the caller disconnected.
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, save := s.DB.ExecContext(finish, "UPDATE gorge_integration_effect SET state=?,result=?,httpStatus=?,updated=? WHERE id=? AND state='submitting'", state, []byte(result), status, time.Now().Unix(), id)
	if save != nil || state == "unknown" {
		return Outcome{}, ErrUnknown
	}
	return Outcome{State: state, Result: result, Status: status}, nil
}
func (s Store) Accept(ctx context.Context, m Inbound) (string, error) {
	hash, raw, e := digest(m)
	if e != nil {
		return "", e
	}
	key := hash
	if v := m.Headers["message-id"]; v != "" {
		key, _, _ = digest([]string{m.Provider, v, m.Headers["to"]})
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return "", e
	}
	defer func() { _ = tx.Rollback() }()
	_, e = tx.ExecContext(ctx, "INSERT INTO gorge_integration_inbox(id,digest,provider,payload,state,due,attempts) VALUES(?,?,?,?,'pending',?,0) ON DUPLICATE KEY UPDATE id=id", key, hash, m.Provider, raw, time.Now().Unix())
	if e != nil {
		return "", e
	}
	var old string
	e = tx.QueryRowContext(ctx, "SELECT digest FROM gorge_integration_inbox WHERE id=? FOR UPDATE", key).Scan(&old)
	if e != nil {
		return "", e
	}
	if old != hash {
		return "", ErrConflict
	}
	return key, tx.Commit()
}
func (s Store) Usage(ctx context.Context) ([]map[string]any, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT kind,state,COUNT(*) FROM gorge_integration_effect GROUP BY kind,state")
	if e != nil {
		return nil, e
	}
	defer func() { _ = rows.Close() }()
	out := []map[string]any{}
	for rows.Next() {
		var kind, state string
		var n int64
		if e = rows.Scan(&kind, &state, &n); e != nil {
			return nil, e
		}
		out = append(out, map[string]any{"kind": kind, "state": state, "count": n})
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	rows2, e := s.DB.QueryContext(ctx, "SELECT provider,state,COUNT(*) FROM gorge_integration_inbox GROUP BY provider,state")
	if e != nil {
		return nil, e
	}
	defer func() { _ = rows2.Close() }()
	for rows2.Next() {
		var kind, state string
		var n int64
		if e = rows2.Scan(&kind, &state, &n); e != nil {
			return nil, e
		}
		out = append(out, map[string]any{"kind": "inbound:" + kind, "state": state, "count": n})
	}
	return out, rows2.Err()
}

// Durable results retain only provider identifiers required by PHP object
// mapping. Provider responses may echo mail/SMS bodies or connector notes.
func minimalResult(kind string, raw json.RawMessage) json.RawMessage {
	fields := map[string][]string{"twilio": {"sid"}, "sns": {"messageID"}, "asana": {"gid", "id"}, "jira": {"id", "key", "self"}}
	var data map[string]json.RawMessage
	if json.Unmarshal(raw, &data) != nil {
		return json.RawMessage(`{}`)
	}
	out := map[string]json.RawMessage{}
	for _, key := range fields[kind] {
		if value, ok := data[key]; ok {
			out[key] = value
		}
	}
	return mustJSON(out)
}

func requireInnoDB(ctx context.Context, db *sql.DB, tables []string) error {
	for _, table := range tables {
		var engine string
		if e := db.QueryRowContext(ctx, "SELECT ENGINE FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=?", table).Scan(&engine); e != nil {
			return e
		}
		if !strings.EqualFold(engine, "InnoDB") {
			return fmt.Errorf("%s must use InnoDB", table)
		}
	}
	return nil
}
