package mailer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

var ErrDeliveryCancelConflict = errors.New("delivery cancellation too late")

var ErrDeliveryConflict = errors.New("delivery identity has different content")

// DeliveryService owns submission identity across HTTP retries and worker
// crashes. The queue is a wakeup mechanism, never evidence of acceptance.
type DeliveryService struct {
	DB         *sql.DB
	Dispatcher *Dispatcher
	Slots      chan struct{}
}

func (s *DeliveryService) Ready(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, "SELECT deliveryID,projectionPending,deadline,projectionNextAttempt FROM gorge_mail_delivery LIMIT 1")
	if err == nil {
		err = rows.Close()
	}
	if err != nil {
		return err
	}
	rows, err = s.DB.QueryContext(ctx, "SELECT deliveryID FROM gorge_mail_attempt LIMIT 1")
	if err == nil {
		err = rows.Close()
	}
	return err
}
func (s *DeliveryService) Deliver(ctx context.Context, req contracts.MailDeliveryRequest) (*contracts.MailDeliveryResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if s.Slots != nil {
		select {
		case s.Slots <- struct{}{}:
			defer func() { <-s.Slots }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	allow := req.AllowSend == nil || *req.AllowSend
	req.AllowSend = nil
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	// Rollback is cleanup and may return sql.ErrTxDone after a successful commit.
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO gorge_mail_delivery (deliveryID,payloadHash,payload,deadline,state,revision,attempt,nextAttempt,startedEpoch,result) VALUES (?,?,?,?,'prepared',0,0,0,0,'{}') ON DUPLICATE KEY UPDATE deliveryID=deliveryID`, req.DeliveryID, hash, raw, req.Deadline)
	if err != nil {
		return nil, err
	}
	var stored, state, result string
	var revision, attempt int
	var next, started int64
	err = tx.QueryRowContext(ctx, `SELECT payloadHash,state,revision,attempt,nextAttempt,startedEpoch,result FROM gorge_mail_delivery WHERE deliveryID=? FOR UPDATE`, req.DeliveryID).Scan(&stored, &state, &revision, &attempt, &next, &started, &result)
	if err != nil {
		return nil, err
	}
	if stored != hash {
		return nil, ErrDeliveryConflict
	}
	now := time.Now().Unix()
	out := &contracts.MailDeliveryResult{DeliveryID: req.DeliveryID, State: state, Revision: revision, Attempt: attempt, NextAttempt: next}
	if state == "accepted" || state == "failed" || state == "unknown" || state == "expired" || state == "cancelled" {
		if err = json.Unmarshal([]byte(result), out); err != nil {
			return nil, err
		}
		return out, tx.Commit()
	}
	if state == "submitting" {
		if now-started < 120 {
			out.NextAttempt = now + 5
			return out, tx.Commit()
		}
		// A dead submitter might have sent the message: never lease it again.
		out.State = "unknown"
		out.Revision++
		out.NextAttempt = 0
		if err = settleStaleSubmission(ctx, tx, out); err != nil {
			return nil, err
		}
		return out, tx.Commit()
	}
	if req.Deadline <= now {
		out.State = "expired"
		out.Revision++
		out.NextAttempt = 0
		if err = saveDelivery(ctx, tx, out, state); err != nil {
			return nil, err
		}
		return out, tx.Commit()
	}
	if next > now {
		return out, tx.Commit()
	}
	if !allow {
		out.NextAttempt = now + 60
		return out, tx.Commit()
	}
	if err = s.Dispatcher.ReadyFor(req.MailerKeys); err != nil {
		return nil, err
	}
	out.State = "submitting"
	out.Attempt++
	out.Revision++
	out.NextAttempt = now + 5
	b, _ := json.Marshal(out)
	_, err = tx.ExecContext(ctx, `UPDATE gorge_mail_delivery SET state='submitting',attempt=?,revision=?,startedEpoch=?,result=? WHERE deliveryID=?`, out.Attempt, out.Revision, now, b, req.DeliveryID)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO gorge_mail_attempt (deliveryID,attempt,startedEpoch,outcome) VALUES (?,?,?,'submitting')`, req.DeliveryID, out.Attempt, now)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	// The timeout is shorter than the submitting recovery threshold. Cancelling
	// the caller cannot prove a provider did not accept; classify conservatively.
	sendCtx, cancel := context.WithTimeout(ctx, 75*time.Second)
	receipt, sendErr := s.Dispatcher.SendWith(sendCtx, &req.Message, req.MailerKeys)
	cancel()
	out.NextAttempt = 0
	out.Revision++
	switch {
	case sendErr == nil:
		out.State = "accepted"
		out.MailerKey = receipt.MailerKey
		out.MessageID = receipt.MessageID
	case IsPermanent(sendErr):
		out.State = "failed"
	case CanRetry(sendErr):
		if out.Attempt >= 12 {
			out.State = "failed"
		} else {
			out.State = "retry_wait"
			out.NextAttempt = time.Now().Unix() + int64(30*(1<<min(out.Attempt-1, 7))) + rand.Int64N(16)
		}
	default:
		out.State = "unknown"
	}
	// Persist the outcome even if the HTTP caller has disconnected.
	reportCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	report, err := s.DB.BeginTx(reportCtx, nil)
	if err != nil {
		return nil, err
	}
	// Rollback is cleanup and may return sql.ErrTxDone after a successful commit.
	defer func() { _ = report.Rollback() }()
	if err = saveDelivery(reportCtx, report, out, "submitting"); err != nil {
		return nil, err
	}
	_, err = report.ExecContext(reportCtx, `UPDATE gorge_mail_attempt SET outcome=?,finishedEpoch=? WHERE deliveryID=? AND attempt=?`, out.State, time.Now().Unix(), req.DeliveryID, out.Attempt)
	if err != nil {
		return nil, err
	}
	return out, report.Commit()
}
func saveDelivery(ctx context.Context, tx *sql.Tx, out *contracts.MailDeliveryResult, oldState string) error {
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE gorge_mail_delivery SET state=?,revision=?,nextAttempt=?,result=?,projectionPending=?,projectionNextAttempt=0,projectionAttempts=0,projectionLastError='' WHERE deliveryID=? AND state=? AND attempt=?`, out.State, out.Revision, out.NextAttempt, b, terminalDelivery(out.State), out.DeliveryID, oldState, out.Attempt)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("delivery submission fence lost")
	}
	return nil
}

func terminalDelivery(state string) bool {
	return state == "accepted" || state == "failed" || state == "unknown" || state == "expired" || state == "cancelled"
}
func (s *DeliveryService) Cancel(ctx context.Context, id string) (*contracts.MailDeliveryResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	// Rollback is cleanup and may return sql.ErrTxDone after a successful commit.
	defer func() { _ = tx.Rollback() }()
	var state, storedResult string
	var revision, attempt int
	if err = tx.QueryRowContext(ctx, "SELECT state,revision,attempt,result FROM gorge_mail_delivery WHERE deliveryID=? FOR UPDATE", id).Scan(&state, &revision, &attempt, &storedResult); err != nil {
		return nil, err
	}
	if state == "cancelled" {
		var out contracts.MailDeliveryResult
		if err = json.Unmarshal([]byte(storedResult), &out); err != nil {
			return nil, err
		}
		return &out, tx.Commit()
	}
	if state != "prepared" && state != "retry_wait" {
		return nil, ErrDeliveryCancelConflict
	}
	out := &contracts.MailDeliveryResult{DeliveryID: id, State: "cancelled", Revision: revision + 1, Attempt: attempt}
	if err = saveDelivery(ctx, tx, out, state); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func settleStaleSubmission(ctx context.Context, tx *sql.Tx, out *contracts.MailDeliveryResult) error {
	if err := saveDelivery(ctx, tx, out, "submitting"); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE gorge_mail_attempt SET outcome='unknown',finishedEpoch=? WHERE deliveryID=? AND attempt=? AND outcome='submitting'`, time.Now().Unix(), out.DeliveryID, out.Attempt)
	return err
}

// Recover settles abandoned submissions and expired pending snapshots without
// requiring their queue tasks to survive. It never performs network submission.
func (s *DeliveryService) Recover(ctx context.Context) error {
	now := time.Now().Unix()
	for _, kind := range []string{"submitting", "prepared", "retry_wait"} {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		query := `SELECT deliveryID,revision,attempt FROM gorge_mail_delivery WHERE state=? AND deadline<=? ORDER BY deadline,deliveryID LIMIT 32 FOR UPDATE SKIP LOCKED`
		threshold := now
		if kind == "submitting" {
			query = `SELECT deliveryID,revision,attempt FROM gorge_mail_delivery WHERE state=? AND startedEpoch<=? ORDER BY startedEpoch,deliveryID LIMIT 32 FOR UPDATE SKIP LOCKED`
			threshold = now - 120
		}
		rows, err := tx.QueryContext(ctx, query, kind, threshold)
		if err != nil {
			_ = tx.Rollback() // Preserve the operation error.
			return err
		}
		var pending []*contracts.MailDeliveryResult
		for rows.Next() {
			out := &contracts.MailDeliveryResult{}
			if err = rows.Scan(&out.DeliveryID, &out.Revision, &out.Attempt); err != nil {
				_ = rows.Close()  // Preserve the scan error.
				_ = tx.Rollback() // Preserve the operation error.
				return err
			}
			out.Revision++
			out.State = "expired"
			if kind == "submitting" {
				out.State = "unknown"
			}
			pending = append(pending, out)
		}
		err = rows.Err()
		if closeErr := rows.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = tx.Rollback() // Preserve the operation error.
			return err
		}
		for _, out := range pending {
			if kind == "submitting" {
				err = settleStaleSubmission(ctx, tx, out)
			} else {
				err = saveDelivery(ctx, tx, out, kind)
			}
			if err != nil {
				_ = tx.Rollback() // Preserve the operation error.
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
