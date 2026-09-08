package taskqueue

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// MySQLStore is the Store backed by Phorge's own worker database.
type MySQLStore struct {
	db            *sql.DB
	leaseDuration int
	retryWait     int
}

// NewMySQLStore opens the pool. It does not dial; see OpenDB.
func NewMySQLStore(cfg *Config) (*MySQLStore, error) {
	db, err := OpenDB(cfg.WorkerDSN())
	if err != nil {
		return nil, err
	}
	return &MySQLStore{
		db:            db,
		leaseDuration: cfg.LeaseDuration,
		retryWait:     cfg.RetryWait,
	}, nil
}

func (s *MySQLStore) Close() error { return s.db.Close() }

// Ready pings the database and nothing more.
//
// Like the webhook domain it does not check that the worker tables exist:
// worker_activetask is created by Phorge's own `bin/storage upgrade`, whose
// container may start after this one, so requiring the table would report a
// healthy deployment as broken for as long as its first migration takes.
func (s *MySQLStore) Ready(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

func (s *MySQLStore) Enqueue(ctx context.Context, req *contracts.EnqueueRequest) (*contracts.Task, error) {
	priority := contracts.PriorityDefault
	if req.Priority != nil {
		priority = *req.Priority
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()

	res, err := tx.ExecContext(ctx,
		"INSERT INTO worker_taskdata (data) VALUES (?)",
		req.Data)
	if err != nil {
		return nil, fmt.Errorf("insert taskdata: %w", err)
	}
	// worker_taskdata uses Phorge's default IDS_AUTOINCREMENT, so its id column
	// is AUTO_INCREMENT and LastInsertId is the row's id.
	dataID, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("get taskdata id: %w", err)
	}

	// worker_activetask is different: PhabricatorWorkerActiveTask declares
	// CONFIG_IDS => IDS_COUNTER, so its id column is `int unsigned NOT NULL`
	// with no AUTO_INCREMENT — Phorge assigns the id in the application layer
	// from the shared lisk_counter table. A plain INSERT that omits id fails
	// with "Field 'id' doesn't have a default value", and even if the column
	// were AUTO_INCREMENT the two paths would allocate from different sources
	// and eventually collide. So we allocate the id the same way Phorge's
	// LiskDAO::loadNextCounterValue does, from the same counter row, and write
	// it explicitly. This keeps a native `phd` enqueue and a Go enqueue drawing
	// ids from one sequence. See compat/phorge/README.md section 10.1.
	taskID, err := nextCounterValue(ctx, tx, "worker_activetask")
	if err != nil {
		return nil, fmt.Errorf("allocate activetask id: %w", err)
	}

	var leaseExpires *int64
	if req.DelayUntil != nil && *req.DelayUntil > 0 {
		leaseExpires = req.DelayUntil
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO worker_activetask
			(id, taskClass, leaseOwner, leaseExpires, failureCount, dataID, failureTime,
			 priority, objectPHID, containerPHID, dateCreated, dateModified)
		VALUES (?, ?, NULL, ?, 0, ?, NULL, ?, ?, ?, ?, ?)`,
		taskID,
		req.TaskClass,
		leaseExpires,
		dataID,
		priority,
		nullStr(req.ObjectPHID),
		nullStr(req.ContainerPHID),
		now,
		now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert activetask: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return &contracts.Task{
		ID:            taskID,
		TaskClass:     req.TaskClass,
		DataID:        dataID,
		Priority:      priority,
		ObjectPHID:    req.ObjectPHID,
		ContainerPHID: req.ContainerPHID,
		DateCreated:   now,
		DateModified:  now,
		LeaseExpires:  leaseExpires,
	}, nil
}

func (s *MySQLStore) Lease(ctx context.Context, limit int, leaseOwner string, taskClasses []string) ([]*contracts.Task, error) {
	if limit <= 0 {
		limit = 1
	}

	now := time.Now().Unix()
	leaseExp := now + int64(s.leaseDuration)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var leased int
	var leasedIDs []int64
	classClause, classArgs := taskClassClause(taskClasses)

	// Phase 1: unleased tasks (new tasks first), ordered by priority then id.
	{
		args := append(append([]any{}, classArgs...), limit)
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM worker_activetask
			 WHERE leaseOwner IS NULL AND leaseExpires IS NULL`+classClause+`
			 ORDER BY priority ASC, id ASC
			 LIMIT ?`, args...)
		if err != nil {
			return nil, fmt.Errorf("select unleased: %w", err)
		}
		ids := scanIDs(rows)
		_ = rows.Close()

		if len(ids) > 0 {
			res, err := tx.ExecContext(ctx,
				fmt.Sprintf(
					`UPDATE worker_activetask
					 SET leaseOwner = ?, leaseExpires = ?
					 WHERE leaseOwner IS NULL AND leaseExpires IS NULL AND id IN (%s)`,
					placeholders(len(ids))),
				appendArgs(leaseOwner, leaseExp, ids)...)
			if err != nil {
				return nil, fmt.Errorf("update unleased: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return nil, fmt.Errorf("count updated unleased: %w", err)
			}
			leased += int(n)
			leasedIDs = append(leasedIDs, ids...)
		}
	}

	// Phase 2: tasks whose lease expired (retry failed / abandoned tasks).
	if remaining := limit - leased; remaining > 0 {
		args := append([]any{now}, classArgs...)
		args = append(args, remaining)
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM worker_activetask
			 WHERE leaseExpires < ?`+classClause+`
			 ORDER BY priority ASC, id ASC
			 LIMIT ?`, args...)
		if err != nil {
			return nil, fmt.Errorf("select expired: %w", err)
		}
		ids := scanIDs(rows)
		_ = rows.Close()

		if len(ids) > 0 {
			_, err := tx.ExecContext(ctx,
				fmt.Sprintf(
					`UPDATE worker_activetask
					 SET leaseOwner = ?, leaseExpires = ?
					 WHERE leaseExpires < ? AND id IN (%s)`,
					placeholders(len(ids))),
				appendArgs(leaseOwner, leaseExp, now, ids)...)
			if err != nil {
				return nil, fmt.Errorf("update expired: %w", err)
			}
			leasedIDs = append(leasedIDs, ids...)
		}
	}

	if len(leasedIDs) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit: %w", err)
		}
		return nil, nil
	}

	// Fetch only rows acquired by this call. A stable leaseOwner may already
	// hold other work, and returning those rows again would execute them twice.
	fetchRows, err := tx.QueryContext(ctx,
		fmt.Sprintf(`SELECT t.id, t.taskClass, t.leaseOwner, t.leaseExpires,
		        t.failureCount, t.dataID, t.failureTime,
		        t.priority, t.objectPHID, t.containerPHID,
		        t.dateCreated, t.dateModified,
		        COALESCE(d.data, '')
		 FROM worker_activetask t
		 LEFT JOIN worker_taskdata d ON d.id = t.dataID
		 WHERE t.id IN (%s) AND t.leaseOwner = ? AND t.leaseExpires > ?
		 ORDER BY t.priority ASC, t.id ASC
		 LIMIT ?`, placeholders(len(leasedIDs))),
		appendArgs(leasedIDs, leaseOwner, now, limit)...)
	if err != nil {
		return nil, fmt.Errorf("fetch leased: %w", err)
	}
	defer func() { _ = fetchRows.Close() }()

	var tasks []*contracts.Task
	for fetchRows.Next() {
		t := &contracts.Task{}
		if err := scanTaskWithData(fetchRows, t); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		tasks = append(tasks, t)
	}
	if err := fetchRows.Err(); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return tasks, nil
}

func (s *MySQLStore) Complete(ctx context.Context, taskID int64, duration int64) (*contracts.ArchivedTask, error) {
	return s.archiveTask(ctx, taskID, contracts.ResultSuccess, duration)
}

func (s *MySQLStore) Fail(ctx context.Context, req *contracts.FailRequest) error {
	if req.Permanent {
		_, err := s.archiveTask(ctx, req.TaskID, contracts.ResultFailure, 0)
		return err
	}

	retryWait := s.retryWait
	if req.RetryWait != nil {
		retryWait = *req.RetryWait
	}

	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`UPDATE worker_activetask
		 SET failureCount = failureCount + 1,
		     failureTime = ?,
		     leaseOwner = NULL,
		     leaseExpires = ?,
		     dateModified = ?
		 WHERE id = ?`,
		now,
		now+int64(retryWait),
		now,
		req.TaskID,
	)
	if err != nil {
		return fmt.Errorf("fail task %d: %w", req.TaskID, err)
	}
	return nil
}

func (s *MySQLStore) Yield(ctx context.Context, taskID int64, duration int) error {
	if duration < 5 {
		duration = 5
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`UPDATE worker_activetask
		 SET leaseOwner = ?,
		     leaseExpires = ?,
		     dateModified = ?
		 WHERE id = ?`,
		contracts.YieldOwner,
		now+int64(duration),
		now,
		taskID,
	)
	if err != nil {
		return fmt.Errorf("yield task %d: %w", taskID, err)
	}
	return nil
}

func (s *MySQLStore) Cancel(ctx context.Context, taskID int64) (*contracts.ArchivedTask, error) {
	return s.archiveTask(ctx, taskID, contracts.ResultCancelled, 0)
}

func (s *MySQLStore) Awaken(ctx context.Context, taskIDs []int64) (int64, error) {
	if len(taskIDs) == 0 {
		return 0, nil
	}

	// Only yielded tasks that have not failed and whose yield window is still
	// open are pulled forward, matching PhabricatorWorker::awakenTaskIDs. The
	// window is an hour, so a task yielded long ago is left alone.
	window := int64(3600)
	epochAgo := time.Now().Unix() - window

	res, err := s.db.ExecContext(ctx,
		fmt.Sprintf(
			`UPDATE worker_activetask
			 SET leaseExpires = ?
			 WHERE id IN (%s)
			   AND leaseOwner = ?
			   AND leaseExpires > ?
			   AND failureCount = 0`,
			placeholders(len(taskIDs))),
		appendArgs(epochAgo, taskIDs, contracts.YieldOwner, epochAgo)...)
	if err != nil {
		return 0, fmt.Errorf("awaken tasks: %w", err)
	}
	return res.RowsAffected()
}

func (s *MySQLStore) Stats(ctx context.Context) (*contracts.QueueStats, error) {
	stats := &contracts.QueueStats{}
	now := time.Now().Unix()

	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM worker_activetask").Scan(&stats.ActiveCount); err != nil {
		return nil, fmt.Errorf("count active: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM worker_activetask WHERE leaseOwner IS NOT NULL AND leaseExpires >= ?",
		now).Scan(&stats.LeasedCount); err != nil {
		return nil, fmt.Errorf("count leased: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM worker_archivetask").Scan(&stats.ArchivedCount); err != nil {
		return nil, fmt.Errorf("count archived: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM worker_activetask WHERE failureCount > 0").Scan(&stats.FailedCount); err != nil {
		return nil, fmt.Errorf("count failed: %w", err)
	}
	return stats, nil
}

func (s *MySQLStore) GetTask(ctx context.Context, taskID int64) (*contracts.Task, error) {
	t := &contracts.Task{}
	row := s.db.QueryRowContext(ctx,
		`SELECT t.id, t.taskClass, t.leaseOwner, t.leaseExpires,
		        t.failureCount, t.dataID, t.failureTime,
		        t.priority, t.objectPHID, t.containerPHID,
		        t.dateCreated, t.dateModified,
		        COALESCE(d.data, '')
		 FROM worker_activetask t
		 LEFT JOIN worker_taskdata d ON d.id = t.dataID
		 WHERE t.id = ?`,
		taskID)
	err := scanTaskWithData(row, t)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan task %d: %w", taskID, err)
	}
	return t, nil
}

func (s *MySQLStore) ListActive(ctx context.Context, limit, offset int) ([]*contracts.Task, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.id, t.taskClass, t.leaseOwner, t.leaseExpires,
		        t.failureCount, t.dataID, t.failureTime,
		        t.priority, t.objectPHID, t.containerPHID,
		        t.dateCreated, t.dateModified
		 FROM worker_activetask t
		 ORDER BY t.priority ASC, t.id ASC
		 LIMIT ? OFFSET ?`,
		limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list active: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var tasks []*contracts.Task
	for rows.Next() {
		t := &contracts.Task{}
		if err := scanTaskNoData(rows, t); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// archiveTask moves one active task into worker_archivetask under the given
// result, then deletes the active row — the whole thing in one transaction so
// a task is never in both tables or neither.
func (s *MySQLStore) archiveTask(ctx context.Context, taskID int64, result int, duration int64) (*contracts.ArchivedTask, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var t contracts.Task
	var leaseOwnerN, objectPHID, containerPHID sql.NullString
	var leaseExp, failureTime sql.NullInt64

	err = tx.QueryRowContext(ctx,
		`SELECT id, taskClass, leaseOwner, leaseExpires,
		        failureCount, dataID, failureTime,
		        priority, objectPHID, containerPHID,
		        dateCreated, dateModified
		 FROM worker_activetask WHERE id = ?`,
		taskID).Scan(
		&t.ID, &t.TaskClass, &leaseOwnerN, &leaseExp,
		&t.FailureCount, &t.DataID, &failureTime,
		&t.Priority, &objectPHID, &containerPHID,
		&t.DateCreated, &t.DateModified,
	)
	if err != nil {
		return nil, fmt.Errorf("select activetask %d: %w", taskID, err)
	}

	if leaseOwnerN.Valid {
		t.LeaseOwner = leaseOwnerN.String
	}
	if leaseExp.Valid {
		t.LeaseExpires = &leaseExp.Int64
	}
	if failureTime.Valid {
		t.FailureTime = &failureTime.Int64
	}
	if objectPHID.Valid {
		t.ObjectPHID = objectPHID.String
	}
	if containerPHID.Valid {
		t.ContainerPHID = containerPHID.String
	}

	now := time.Now().Unix()

	// worker_archivetask deliberately has no `failureTime` column: it is an
	// active-only field driving retry backoff, and Phorge's
	// PhabricatorWorkerArchiveTask drops it when it archives a task (the
	// archive row's columns are the active row's minus failureTime, plus
	// result/duration/archivedEpoch). Including it here fails the INSERT with
	// "Unknown column 'failureTime'", leaving the task stuck in the active
	// table to be re-leased forever. See compat/phorge/README.md section 10.
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO worker_archivetask
			(id, taskClass, leaseOwner, leaseExpires, failureCount, dataID,
			 priority, objectPHID, containerPHID,
			 result, duration, dateCreated, dateModified, archivedEpoch)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.TaskClass,
		nullStr(t.LeaseOwner), leaseExp,
		t.FailureCount, t.DataID,
		t.Priority,
		nullStr(t.ObjectPHID), nullStr(t.ContainerPHID),
		result, duration,
		t.DateCreated, now, now,
	); err != nil {
		return nil, fmt.Errorf("insert archivetask: %w", err)
	}

	if _, err = tx.ExecContext(ctx,
		"DELETE FROM worker_activetask WHERE id = ?", taskID); err != nil {
		return nil, fmt.Errorf("delete activetask: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return &contracts.ArchivedTask{
		Task:          t,
		Result:        result,
		Duration:      duration,
		ArchivedEpoch: &now,
	}, nil
}

// scanner is the shared surface of *sql.Row and *sql.Rows, so one scan helper
// serves both GetTask (a single row) and Lease/ListActive (a cursor).
type scanner interface {
	Scan(dest ...any) error
}

// scanTaskWithData scans the twelve task columns plus the joined data column.
func scanTaskWithData(sc scanner, t *contracts.Task) error {
	var leaseOwnerN, objectPHID, containerPHID sql.NullString
	var leaseExp, failureTime sql.NullInt64
	if err := sc.Scan(
		&t.ID, &t.TaskClass, &leaseOwnerN, &leaseExp,
		&t.FailureCount, &t.DataID, &failureTime,
		&t.Priority, &objectPHID, &containerPHID,
		&t.DateCreated, &t.DateModified,
		&t.Data,
	); err != nil {
		return err
	}
	applyNullable(t, leaseOwnerN, objectPHID, containerPHID, leaseExp, failureTime)
	return nil
}

// scanTaskNoData scans the twelve task columns without the data column, for
// the list endpoint where payloads are not returned.
func scanTaskNoData(sc scanner, t *contracts.Task) error {
	var leaseOwnerN, objectPHID, containerPHID sql.NullString
	var leaseExp, failureTime sql.NullInt64
	if err := sc.Scan(
		&t.ID, &t.TaskClass, &leaseOwnerN, &leaseExp,
		&t.FailureCount, &t.DataID, &failureTime,
		&t.Priority, &objectPHID, &containerPHID,
		&t.DateCreated, &t.DateModified,
	); err != nil {
		return err
	}
	applyNullable(t, leaseOwnerN, objectPHID, containerPHID, leaseExp, failureTime)
	return nil
}

func applyNullable(t *contracts.Task, leaseOwner, objectPHID, containerPHID sql.NullString, leaseExp, failureTime sql.NullInt64) {
	if leaseOwner.Valid {
		t.LeaseOwner = leaseOwner.String
	}
	if leaseExp.Valid {
		v := leaseExp.Int64
		t.LeaseExpires = &v
	}
	if failureTime.Valid {
		v := failureTime.Int64
		t.FailureTime = &v
	}
	if objectPHID.Valid {
		t.ObjectPHID = objectPHID.String
	}
	if containerPHID.Valid {
		t.ContainerPHID = containerPHID.String
	}
}

// nextCounterValue allocates the next id for a Phorge IDS_COUNTER table from
// the shared lisk_counter table, mirroring LiskDAO::loadNextCounterValue. It
// must run inside the same transaction as the INSERT it feeds so a concurrent
// enqueue cannot hand out the same id.
//
// The LAST_INSERT_ID(...) trick is Phorge's own: the INSERT half seeds a new
// counter at 1 while updating LAST_INSERT_ID, and the ON DUPLICATE KEY UPDATE
// half increments an existing counter and republishes the new value through
// LAST_INSERT_ID, so LastInsertId returns the allocated value in both cases
// without a separate SELECT. Reusing this exact statement is what keeps the id
// sequence identical to a native `phd`/LiskDAO enqueue against the same row.
func nextCounterValue(ctx context.Context, tx *sql.Tx, counterName string) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`INSERT INTO lisk_counter (counterName, counterValue)
			VALUES (?, LAST_INSERT_ID(1))
		 ON DUPLICATE KEY UPDATE
			counterValue = LAST_INSERT_ID(counterValue + 1)`,
		counterName)
	if err != nil {
		return 0, fmt.Errorf("bump counter %q: %w", counterName, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read counter %q: %w", counterName, err)
	}
	return id, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func scanIDs(rows *sql.Rows) []int64 {
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func appendArgs(args ...any) []any {
	var out []any
	for _, a := range args {
		switch v := a.(type) {
		case []int64:
			for _, id := range v {
				out = append(out, id)
			}
		default:
			out = append(out, a)
		}
	}
	return out
}

// taskClassClause returns an optional SQL filter and its arguments. Keeping
// the filter in the candidate SELECTs ensures a dedicated worker never takes
// ownership of work intended for another pool.
func taskClassClause(taskClasses []string) (string, []any) {
	if len(taskClasses) == 0 {
		return "", nil
	}
	args := make([]any, 0, len(taskClasses))
	for _, taskClass := range taskClasses {
		args = append(args, taskClass)
	}
	return " AND taskClass IN (" + placeholders(len(taskClasses)) + ")", args
}
