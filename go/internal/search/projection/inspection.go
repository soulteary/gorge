package projection

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

type DeliveryState struct {
	Target
	Status       string `json:"status"`
	Attempts     uint64 `json:"attempts"`
	LeaseExpires int64  `json:"leaseExpires"`
	LeaseExpired bool   `json:"leaseExpired"`
	NextAttempt  int64  `json:"nextAttempt"`
	CreatedEpoch int64  `json:"createdEpoch"`
	AppliedEpoch *int64 `json:"appliedEpoch"`
	LastError    string `json:"lastError,omitempty"`
}
type EventStatus struct {
	LatestRevision string          `json:"latestRevision"`
	Receipt        Receipt         `json:"receipt"`
	ObservedEpoch  int64           `json:"observedEpoch"`
	Deliveries     []DeliveryState `json:"deliveries"`
}
type TargetStats struct {
	Target
	Pending            uint64 `json:"pending"`
	Running            uint64 `json:"running"`
	Applied            uint64 `json:"applied"`
	Superseded         uint64 `json:"superseded"`
	Retrying           uint64 `json:"retrying"`
	ExpiredRunning     uint64 `json:"expiredRunning"`
	UnknownPendingAge  uint64 `json:"unknownPendingAge"`
	OldestPendingEpoch *int64 `json:"oldestPendingEpoch"`
}
type DeliveryStats struct {
	SourceOutbox  *SourceOutboxStats `json:"sourceOutbox,omitempty"`
	ObservedEpoch int64              `json:"observedEpoch"`
	Targets       []TargetStats      `json:"targets"`
}

func ValidateEventID(id string) error {
	if id == "" || len(id) > 128 || !utf8.ValidString(id) || strings.ContainsAny(id, "\x00\r\n") {
		return fmt.Errorf("invalid event ID")
	}
	return nil
}
func (s *MySQLStore) EventStatus(ctx context.Context, namespace, id string) (*EventStatus, error) {
	if err := ValidateNamespace(namespace); err != nil {
		return nil, err
	}
	if err := ValidateEventID(id); err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	result := &EventStatus{Deliveries: []DeliveryState{}}
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT response FROM search_projection_inbox WHERE namespace=? AND eventID=?`, namespace, id).Scan(&raw); err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(raw), &result.Receipt); err != nil {
		return nil, fmt.Errorf("invalid stored receipt")
	}
	if result.Receipt.EventID != id {
		return nil, fmt.Errorf("receipt identity mismatch")
	}
	if err = tx.QueryRowContext(ctx, "SELECT UNIX_TIMESTAMP()").Scan(&result.ObservedEpoch); err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT revision FROM search_projection_head WHERE namespace=? AND phid=?", namespace, result.Receipt.PHID).Scan(&result.LatestRevision); err != nil {
		return nil, fmt.Errorf("projection head unavailable")
	}
	for _, target := range result.Receipt.Targets {
		state := DeliveryState{Target: target}
		var applied sql.NullInt64
		var errorCode string
		err = tx.QueryRowContext(ctx, `SELECT status,attempts,leaseExpires,nextAttempt,createdEpoch,appliedEpoch,lastError FROM search_projection_delivery WHERE namespace=? AND backendID=? AND generationID=? AND phid=? AND revision=?`, namespace, target.BackendID, target.GenerationID, result.Receipt.PHID, result.Receipt.Revision).Scan(&state.Status, &state.Attempts, &state.LeaseExpires, &state.NextAttempt, &state.CreatedEpoch, &applied, &errorCode)
		if err != nil {
			return nil, fmt.Errorf("delivery state unavailable")
		}
		state.LeaseExpired = state.Status == "running" && state.LeaseExpires <= result.ObservedEpoch
		if applied.Valid {
			state.AppliedEpoch = &applied.Int64
		}
		// Never expose arbitrary database error text from older prototypes.
		if errorCode != "" {
			state.LastError = "backend_delivery_failed"
		}
		result.Deliveries = append(result.Deliveries, state)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
func (s *MySQLStore) DeliveryStats(ctx context.Context, namespace string, targets []Target) (*DeliveryStats, error) {
	if err := ValidateNamespace(namespace); err != nil {
		return nil, err
	}
	if err := ValidateTargets(targets); err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	result := &DeliveryStats{Targets: []TargetStats{}}
	if err = tx.QueryRowContext(ctx, "SELECT UNIX_TIMESTAMP()").Scan(&result.ObservedEpoch); err != nil {
		return nil, err
	}
	for _, target := range targets {
		state := TargetStats{Target: target}
		var oldest sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT
 COALESCE(SUM(status='pending'),0),COALESCE(SUM(status='running'),0),
 COALESCE(SUM(status='applied'),0),COALESCE(SUM(status='superseded'),0),
 COALESCE(SUM(status='pending' AND attempts>0),0),
 COALESCE(SUM(status='running' AND leaseExpires<=?),0),
 COALESCE(SUM(status IN ('pending','running') AND createdEpoch=0),0),
 MIN(CASE WHEN status IN ('pending','running') AND createdEpoch>0 THEN createdEpoch END)
 FROM search_projection_delivery WHERE namespace=? AND backendID=? AND generationID=?`, result.ObservedEpoch, namespace, target.BackendID, target.GenerationID).Scan(&state.Pending, &state.Running, &state.Applied, &state.Superseded, &state.Retrying, &state.ExpiredRunning, &state.UnknownPendingAge, &oldest)
		if err != nil {
			return nil, err
		}
		if oldest.Valid {
			state.OldestPendingEpoch = &oldest.Int64
		}
		result.Targets = append(result.Targets, state)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// SourceOutboxStats cannot report age because the PHP source schema has no
// creation timestamp. Its database clock may differ from the control database.
type SourceOutboxStats struct {
	ObservedEpoch int64  `json:"observedEpoch"`
	Pending       uint64 `json:"pending"`
	Due           uint64 `json:"due"`
	Retrying      uint64 `json:"retrying"`
	AgeAvailable  bool   `json:"ageAvailable"`
}

func InspectSourceOutbox(ctx context.Context, db *sql.DB) (*SourceOutboxStats, error) {
	result := &SourceOutboxStats{}
	err := db.QueryRowContext(ctx, `SELECT UNIX_TIMESTAMP(),COUNT(*),COALESCE(SUM(nextAttempt<=UNIX_TIMESTAMP()),0),COALESCE(SUM(attempts>0),0) FROM search_gorgeoutbox WHERE deliveredEpoch IS NULL`).Scan(&result.ObservedEpoch, &result.Pending, &result.Due, &result.Retrying)
	if err != nil {
		return nil, err
	}
	return result, nil
}
