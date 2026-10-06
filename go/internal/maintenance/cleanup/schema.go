package cleanup

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ValidateSchema checks the live target; Go never creates application tables.
func (s *Store) ValidateSchema(ctx context.Context, id string) error {
	db, spec, err := s.db(id)
	if err != nil {
		return err
	}
	var readonly int
	if err = db.QueryRowContext(ctx, "SELECT @@global.read_only").Scan(&readonly); err != nil {
		return err
	}
	if readonly != 0 {
		return fmt.Errorf("database is read-only")
	}
	for _, name := range []string{table, spec.Table} {
		var engine string
		err = db.QueryRowContext(ctx, "SELECT ENGINE FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=?", name).Scan(&engine)
		if err != nil {
			return fmt.Errorf("%s schema missing: %w", name, err)
		}
		if !strings.EqualFold(engine, "InnoDB") {
			return fmt.Errorf("%s requires InnoDB", name)
		}
	}
	var controlPK int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND INDEX_NAME='PRIMARY' AND COLUMN_NAME='collectorID' AND SEQ_IN_INDEX=1", table).Scan(&controlPK)
	if err != nil {
		return err
	}
	if controlPK != 1 {
		return fmt.Errorf("control table requires collectorID primary key")
	}
	var pk int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND INDEX_NAME='PRIMARY' AND COLUMN_NAME='id' AND SEQ_IN_INDEX=1", spec.Table).Scan(&pk)
	if err != nil {
		return err
	}
	if pk != 1 {
		return fmt.Errorf("%s requires id primary key", spec.Table)
	}
	// MySQL exposes invisible indexes in STATISTICS; existence is insufficient.
	// MariaDB versions without IS_VISIBLE are checked by the actual plan below.
	var visibilityColumn int
	if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='information_schema' AND TABLE_NAME='STATISTICS' AND COLUMN_NAME='IS_VISIBLE'").Scan(&visibilityColumn); err != nil {
		return err
	}
	visibility := ""
	if visibilityColumn != 0 {
		visibility = " AND IS_VISIBLE='YES'"
	}
	var indexed int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME=? AND SEQ_IN_INDEX=1 AND SUB_PART IS NULL"+visibility, spec.Table, spec.Column).Scan(&indexed)
	if err != nil {
		return err
	}
	if indexed == 0 {
		return fmt.Errorf("%s requires leading %s index", spec.Table, spec.Column)
	}
	var dataType string
	err = db.QueryRowContext(ctx, "SELECT DATA_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME=?", spec.Table, spec.Column).Scan(&dataType)
	if err != nil {
		return err
	}
	switch dataType {
	case "int", "bigint":
	default:
		return fmt.Errorf("unsupported timestamp type %s", dataType)
	}
	return validatePlan(ctx, db, spec, time.Now().Unix(), 100)
}
func (s *Store) Ready(ctx context.Context) error {
	if len(s.DBs) == 0 {
		return fmt.Errorf("no databases configured")
	}
	for _, spec := range specs {
		if s.DBs[spec.Role] == nil {
			continue
		}
		if err := s.ValidateSchema(ctx, spec.ID); err != nil {
			return err
		}
		st, err := s.Status(ctx, spec.ID)
		if err != nil {
			return err
		}
		if st.Owner != "php" && st.Owner != "gorge" && st.Owner != "paused" {
			return fmt.Errorf("invalid cleanup owner")
		}
		if _, err = decodeState(st); err != nil {
			return err
		}
	}
	return nil
}

// Explain the exact deletion, rejecting invisible/unused indexes and table scans.
type planQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func validatePlan(ctx context.Context, q planQuerier, spec Spec, cutoff int64, limit int) error {
	rows, err := q.QueryContext(ctx, "EXPLAIN "+deleteSQL(spec), cutoff, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		values := make([]sql.NullString, len(columns))
		args := make([]any, len(columns))
		for i := range values {
			args[i] = &values[i]
		}
		if err := rows.Scan(args...); err != nil {
			return err
		}
		var key, access string
		for i, c := range columns {
			if c == "key" {
				key = values[i].String
			}
			if c == "type" {
				access = values[i].String
			}
		}
		if key == "" || access != "range" {
			return fmt.Errorf("%s deletion requires an indexed range plan (key=%s, type=%s)", spec.Table, key, access)
		}
		found = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("missing deletion plan")
	}
	return nil
}
