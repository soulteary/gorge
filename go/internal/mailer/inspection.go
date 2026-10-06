package mailer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// DeliveryInspection contains operational metadata only. Message contents and
// recipient addresses never leave the ledger through this read-only API.
type DeliveryInspection struct {
	Result                contracts.MailDeliveryResult `json:"result"`
	Deadline              int64                        `json:"deadline"`
	StartedEpoch          int64                        `json:"startedEpoch"`
	ProjectionPending     bool                         `json:"projectionPending"`
	ProjectionAttempts    int                          `json:"projectionAttempts"`
	ProjectionNextAttempt int64                        `json:"projectionNextAttempt"`
	ProjectionLastError   string                       `json:"projectionLastError"`
	Attempts              []DeliveryAttemptInspection  `json:"attempts"`
}

type DeliveryAttemptInspection struct {
	Attempt       int    `json:"attempt"`
	StartedEpoch  int64  `json:"startedEpoch"`
	FinishedEpoch *int64 `json:"finishedEpoch"`
	Outcome       string `json:"outcome"`
}

// Inspect uses a consistent read transaction so recovery or submission cannot
// produce a result paired with attempt audit from a different ledger revision.
func (s *DeliveryService) Inspect(ctx context.Context, id string) (*DeliveryInspection, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := &DeliveryInspection{Attempts: []DeliveryAttemptInspection{}}
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT state,revision,attempt,nextAttempt,result,deadline,startedEpoch,projectionPending,projectionAttempts,projectionNextAttempt,projectionLastError FROM gorge_mail_delivery WHERE deliveryID=?`, id).Scan(
		&out.Result.State, &out.Result.Revision, &out.Result.Attempt, &out.Result.NextAttempt, &raw, &out.Deadline, &out.StartedEpoch, &out.ProjectionPending, &out.ProjectionAttempts, &out.ProjectionNextAttempt, &out.ProjectionLastError)
	if err != nil {
		return nil, err
	}
	// Prepared rows have an empty result; ledger columns are authoritative.
	var receipt contracts.MailDeliveryResult
	if err = json.Unmarshal(raw, &receipt); err != nil {
		return nil, fmt.Errorf("invalid persisted delivery receipt")
	}
	out.Result.DeliveryID = id
	out.Result.MailerKey = receipt.MailerKey
	out.Result.MessageID = receipt.MessageID
	rows, err := tx.QueryContext(ctx, `SELECT attempt,startedEpoch,finishedEpoch,outcome FROM gorge_mail_attempt WHERE deliveryID=? ORDER BY attempt DESC LIMIT 12`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a DeliveryAttemptInspection
		if err = rows.Scan(&a.Attempt, &a.StartedEpoch, &a.FinishedEpoch, &a.Outcome); err != nil {
			rows.Close()
			return nil, err
		}
		out.Attempts = append(out.Attempts, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
