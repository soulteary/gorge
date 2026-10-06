package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// The ledger row's projectionPending flag is a transactional result outbox.
// Queue cancellation or archival cannot discard an accepted-mail receipt.
func ProjectMailResultsOnce(ctx context.Context, db *sql.DB, conduit *ConduitClient) error {
	rows, err := db.QueryContext(ctx, `SELECT deliveryID,payload,result,revision,projectionAttempts FROM gorge_mail_delivery WHERE projectionPending=1 AND projectionNextAttempt<=? ORDER BY projectionNextAttempt,deliveryID LIMIT 32`, time.Now().Unix())
	if err != nil {
		return err
	}
	type projection struct {
		id              string
		payload, result []byte
		revision        int
		attempts        int
	}
	var pending []projection
	for rows.Next() {
		var p projection
		if err = rows.Scan(&p.id, &p.payload, &p.result, &p.revision, &p.attempts); err != nil {
			_ = rows.Close() // Preserve the scan error.
			return err
		}
		pending = append(pending, p)
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	var first error
	for _, p := range pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ref mailRef
		err = json.Unmarshal(p.payload, &ref)
		if err == nil && ref.MailID <= 0 {
			err = fmt.Errorf("invalid mail projection identity")
		}
		if err == nil {
			err = applyMailResult(ctx, conduit, ref.MailID, string(p.result))
		}
		if err == nil {
			_, err = db.ExecContext(ctx, `UPDATE gorge_mail_delivery SET projectionPending=0 WHERE deliveryID=? AND revision=?`, p.id, p.revision)
		}
		if err != nil {
			if first == nil {
				first = err
			}
			// Persist backoff even when this RPC exhausted the batch context.
			report, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_, updateErr := db.ExecContext(report, `UPDATE gorge_mail_delivery SET projectionAttempts=projectionAttempts+1,projectionNextAttempt=?,projectionLastError='projection acknowledgment unavailable' WHERE deliveryID=? AND revision=? AND projectionPending=1`, time.Now().Unix()+int64(15*(1<<min(p.attempts, 6))), p.id, p.revision)
			stop()
			if updateErr != nil {
				return updateErr
			}
		}
	}
	return first
}

func applyMailResult(ctx context.Context, conduit *ConduitClient, id int64, raw string) error {
	response, err := conduit.Call(ctx, "mail.delivery", map[string]any{"phase": "apply", "mailID": id, "result": raw})
	if err != nil {
		return err
	}
	var ack struct {
		Applied bool `json:"applied"`
	}
	if err = json.Unmarshal(response.Result, &ack); err != nil || !ack.Applied {
		return fmt.Errorf("mail projection acknowledgment missing")
	}
	return nil
}
