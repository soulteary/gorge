package taskqueue

import (
	"context"
	"database/sql"
	"errors"
)

func (s *MySQLStore) ScheduleReady(ctx context.Context) error {
	// Also verifies InnoDB for every participant. MyISAM must never silently turn
	// the event/task boundary into independent writes.
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE()
 AND table_name IN ('worker_gorgeschedulercontrol','worker_gorgeschedule','worker_trigger','worker_triggerevent','worker_taskdata','worker_activetask','lisk_counter') AND engine = 'InnoDB'`).Scan(&count)
	if err != nil {
		return err
	}
	if count != 7 {
		return errors.New("scheduler requires seven migrated InnoDB tables")
	}
	var owner string
	var epoch int64
	if err := s.db.QueryRowContext(ctx, "SELECT owner, epoch FROM worker_gorgeschedulercontrol WHERE id = 1").Scan(&owner, &epoch); err != nil {
		return err
	}
	if epoch < 1 || (owner != "php" && owner != "paused" && owner != "gorge") {
		return errors.New("invalid scheduler ownership")
	}
	return nil
}
func (s *MySQLStore) ScheduleDatabaseID(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, "SELECT databaseID FROM worker_gorgeschedulercontrol WHERE id=1").Scan(&id)
	if err == nil && len(id) != 36 {
		err = errors.New("scheduler database identity is missing or invalid")
	}
	return id, err
}
func (s *MySQLStore) ScheduleCandidates(ctx context.Context, limit int) ([]ScheduleSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.triggerVersion, c.epoch, COALESCE(e.id,0), e.lastEventEpoch, e.nextEventEpoch,
 COALESCE(g.scheduledVersion,0), UNIX_TIMESTAMP(), c.databaseID
 FROM worker_trigger t CROSS JOIN worker_gorgeschedulercontrol c
 LEFT JOIN worker_triggerevent e ON e.triggerID=t.id LEFT JOIN worker_gorgeschedule g ON g.triggerID=t.id
 WHERE c.id=1 AND c.owner='gorge' AND (g.retryAfter IS NULL OR g.retryAfter<=UNIX_TIMESTAMP())
 AND (g.scheduledVersion IS NULL OR g.scheduledVersion<>t.triggerVersion OR e.nextEventEpoch<=UNIX_TIMESTAMP())
 ORDER BY COALESCE(g.lastEvaluatedEpoch,0), t.id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ScheduleSnapshot
	for rows.Next() {
		var snap ScheduleSnapshot
		if err = rows.Scan(&snap.ID, &snap.Version, &snap.OwnerEpoch, &snap.EventID, &snap.Last, &snap.Next, &snap.ScheduledVersion, &snap.Now, &snap.DatabaseID); err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}
func sameEpoch(a, b *int64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
func (s *MySQLStore) lockSchedule(ctx context.Context, tx *sql.Tx, snap ScheduleSnapshot) error {
	var owner string
	var databaseID string
	var epoch, version int64
	if err := tx.QueryRowContext(ctx, "SELECT owner, epoch, databaseID FROM worker_gorgeschedulercontrol WHERE id=1 FOR UPDATE").Scan(&owner, &epoch, &databaseID); err != nil {
		return err
	}
	if owner != "gorge" || epoch != snap.OwnerEpoch || databaseID != snap.DatabaseID || len(databaseID) != 36 {
		return ErrScheduleConflict
	}
	if err := tx.QueryRowContext(ctx, "SELECT triggerVersion FROM worker_trigger WHERE id=? FOR UPDATE", snap.ID).Scan(&version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrScheduleConflict
		}
		return err
	}
	if version != snap.Version {
		return ErrScheduleConflict
	}
	var id int64
	var last, next *int64
	err := tx.QueryRowContext(ctx, "SELECT id, lastEventEpoch, nextEventEpoch FROM worker_triggerevent WHERE triggerID=? FOR UPDATE", snap.ID).Scan(&id, &last, &next)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if id != snap.EventID || !sameEpoch(last, snap.Last) || !sameEpoch(next, snap.Next) {
		return ErrScheduleConflict
	}
	var scheduled int64
	err = tx.QueryRowContext(ctx, "SELECT scheduledVersion FROM worker_gorgeschedule WHERE triggerID=? FOR UPDATE", snap.ID).Scan(&scheduled)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if scheduled != snap.ScheduledVersion {
		return ErrScheduleConflict
	}
	return nil
}
func (s *MySQLStore) ApplySchedule(ctx context.Context, snap ScheduleSnapshot, p SchedulePlan) error {
	if err := validatePlan(snap, p); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.lockSchedule(ctx, tx, snap); err != nil {
		return err
	}
	last := snap.Last
	if p.Fire {
		if _, err = enqueueInTx(ctx, tx, p.Task); err != nil {
			return err
		}
		last = snap.Next
	}
	// Existing trigger UI and PHP clock prediction continue to see the same
	// event table. There is only one committed occurrence, not an HTTP enqueue.
	_, err = tx.ExecContext(ctx, `INSERT INTO worker_triggerevent (triggerID,lastEventEpoch,nextEventEpoch) VALUES (?,?,?)
 ON DUPLICATE KEY UPDATE lastEventEpoch=VALUES(lastEventEpoch),nextEventEpoch=VALUES(nextEventEpoch)`, snap.ID, last, p.Next)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO worker_gorgeschedule (triggerID,scheduledVersion,retryAfter,lastEvaluatedEpoch) VALUES (?,?,0,UNIX_TIMESTAMP())
 ON DUPLICATE KEY UPDATE scheduledVersion=VALUES(scheduledVersion),retryAfter=0,lastEvaluatedEpoch=UNIX_TIMESTAMP()`, snap.ID, snap.Version)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *MySQLStore) ScheduleFailed(ctx context.Context, snap ScheduleSnapshot) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.lockSchedule(ctx, tx, snap); err != nil {
		if errors.Is(err, ErrScheduleConflict) {
			return nil
		}
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO worker_gorgeschedule (triggerID,scheduledVersion,retryAfter,lastEvaluatedEpoch) VALUES (?,?,UNIX_TIMESTAMP()+30,UNIX_TIMESTAMP())
 ON DUPLICATE KEY UPDATE retryAfter=UNIX_TIMESTAMP()+30,lastEvaluatedEpoch=UNIX_TIMESTAMP()`, snap.ID, snap.ScheduledVersion)
	if err != nil {
		return err
	}
	return tx.Commit()
}
