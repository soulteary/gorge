package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

// Resolution acknowledges a verified external outcome; it never resubmits it.
// Accepted connector results must be the actual provider result so downstream
// business mapping can continue after the original worker is retried.
type Resolution struct {
	Domain   string          `json:"domain"`
	ID       string          `json:"id"`
	Digest   string          `json:"digest"`
	State    string          `json:"state"`
	Status   int             `json:"status"`
	Result   json.RawMessage `json:"result"`
	Operator string          `json:"operator"`
	Evidence string          `json:"evidence"`
}

func (r Resolution) Validate() error {
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(r.Digest) || r.ID == "" || len(r.ID) > 128 || len(r.Operator) < 1 || len(r.Operator) > 128 || len(r.Evidence) < 16 || len(r.Evidence) > 2048 {
		return errors.New("invalid resolution identity/evidence")
	}
	switch r.Domain {
	case "effect":
		var result any
		if json.Unmarshal(r.Result, &result) != nil {
			return errors.New("invalid verified result")
		}
		switch result.(type) {
		case map[string]any, []any:
		default:
			return errors.New("verified result must be an object or array")
		}
		if (r.State != "accepted" && r.State != "rejected") || (r.State == "accepted" && (r.Status < 200 || r.Status >= 300)) || (r.State == "rejected" && (r.Status < 400 || r.Status >= 500 || r.Status == 429)) || !json.Valid(r.Result) || len(r.Result) > 1024*1024 {
			return errors.New("invalid verified effect")
		}
	case "inbound":
		if r.State != "done" || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(r.ID) {
			return errors.New("invalid verified inbox")
		}
	default:
		return errors.New("invalid resolution domain")
	}
	return nil
}
func (s Store) Resolve(ctx context.Context, r Resolution, reconcile func(json.RawMessage) error) error {
	if e := r.Validate(); e != nil {
		return e
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	var hash, state, kind string
	var updated int64
	var raw []byte
	if r.Domain == "effect" {
		e = tx.QueryRowContext(ctx, "SELECT digest,state,updated,kind FROM gorge_integration_effect WHERE id=? FOR UPDATE", r.ID).Scan(&hash, &state, &updated, &kind)
	} else {
		e = tx.QueryRowContext(ctx, "SELECT digest,state,payload FROM gorge_integration_inbox WHERE id=? FOR UPDATE", r.ID).Scan(&hash, &state, &raw)
	}
	if e != nil {
		return e
	}
	if hash != r.Digest {
		return ErrConflict
	}
	resolvable := state == "unknown" || (r.Domain == "effect" && state == "submitting" && updated <= time.Now().Unix()-120)
	if !resolvable {
		return ErrConflict
	}
	// Write evidence within the same transaction as the local state transition.
	_, e = tx.ExecContext(ctx, "INSERT INTO gorge_integration_resolution(domain,identity,digest,state,operator,evidence,created) VALUES(?,?,?,?,?,?,?)", r.Domain, r.ID, r.Digest, r.State, r.Operator, r.Evidence, time.Now().Unix())
	if e != nil {
		return e
	}
	if r.Domain == "effect" {
		_, e = tx.ExecContext(ctx, "UPDATE gorge_integration_effect SET state=?,result=?,httpStatus=?,updated=? WHERE id=?", r.State, []byte(minimalResult(kind, r.Result)), r.Status, time.Now().Unix(), r.ID)
	} else {
		if reconcile == nil {
			return errors.New("source receipt verification required")
		}
		if e = reconcile(raw); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "UPDATE gorge_integration_inbox SET state='done',due=? WHERE id=?", time.Now().Unix(), r.ID)
	}
	if e != nil {
		return e
	}
	return tx.Commit()
}

// Purge only acknowledged payloads, keeping identity and digest tombstones
// indefinitely so delayed provider retries cannot execute the email again.
func (s Store) Purge(ctx context.Context, days int) (int64, error) {
	if days < 1 {
		return 0, nil
	}
	result, e := s.DB.ExecContext(ctx, "UPDATE gorge_integration_inbox SET payload='' WHERE state='done' AND due<? AND OCTET_LENGTH(payload)>0 ORDER BY due LIMIT 500", time.Now().Add(-time.Duration(days)*24*time.Hour).Unix())
	if e != nil {
		return 0, e
	}
	return result.RowsAffected()
}

type Health struct {
	UnknownEffects       int64 `json:"unknownEffects"`
	UnknownInbound       int64 `json:"unknownInbound"`
	PendingInbound       int64 `json:"pendingInbound"`
	OldestOverdueSeconds int64 `json:"oldestOverdueSeconds"`
}

func (s Store) Health(ctx context.Context) (Health, error) {
	var h Health
	now := time.Now().Unix()
	e := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM gorge_integration_effect WHERE state='unknown' OR (state='submitting' AND updated<=?)", now-120).Scan(&h.UnknownEffects)
	if e != nil {
		return h, e
	}
	var due int64
	e = s.DB.QueryRowContext(ctx, "SELECT COALESCE(SUM(state='unknown'),0),COALESCE(SUM(state IN ('pending','retry','processing')),0),COALESCE(MIN(CASE WHEN state IN ('pending','retry','processing') THEN due END),?) FROM gorge_integration_inbox", now).Scan(&h.UnknownInbound, &h.PendingInbound, &due)
	if due < now {
		h.OldestOverdueSeconds = now - due
	}
	return h, e
}
