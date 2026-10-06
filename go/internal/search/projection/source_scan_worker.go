package projection

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// StepSourceShard commits progress only after all materialized envelopes have
// reached the target inbox. A PHP capture committed before a lost response is
// safely recaptured; unchanged snapshots retain the publisher's revision.
func (s *MySQLStore) StepSourceShard(ctx context.Context, namespace, job, class, owner string, provider SourceProvider) error {
	if ValidateNamespace(namespace) != nil || !namespacePattern.MatchString(job) || !namespacePattern.MatchString(owner) || !sourceClassPattern.MatchString(class) || provider == nil {
		return fmt.Errorf("invalid source shard identity")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var raw string
	var target Target
	if err = tx.QueryRowContext(ctx, `SELECT backendID,generationID,catalog FROM search_projection_source_job WHERE namespace=? AND jobID=?`, namespace, job).Scan(&target.BackendID, &target.GenerationID, &raw); err != nil {
		return err
	}
	var catalog SourceCatalog
	if err = json.Unmarshal([]byte(raw), &catalog); err != nil {
		return err
	}
	if err = ValidateCatalog(&catalog, namespace); err != nil {
		return err
	}
	var cursor, upper string
	var epoch uint64
	if err = tx.QueryRowContext(ctx, `SELECT cursorID,upperID,leaseEpoch FROM search_projection_source_shard WHERE namespace=? AND jobID=? AND className=? FOR UPDATE`, namespace, job, class).Scan(&cursor, &upper, &epoch); err != nil {
		return err
	}
	found := false
	for _, d := range catalog.Sources {
		if d.ClassName == class && d.UpperID == upper {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("source shard differs from frozen catalog")
	}
	result, err := tx.ExecContext(ctx, `UPDATE search_projection_source_shard SET leaseOwner=?,leaseEpoch=leaseEpoch+1,leaseExpires=UNIX_TIMESTAMP()+30 WHERE namespace=? AND jobID=? AND className=? AND status='scanning' AND leaseExpires<=UNIX_TIMESTAMP() AND nextAttempt<=UNIX_TIMESTAMP()`, owner, namespace, job, class)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return sql.ErrNoRows
	}
	epoch++
	if err = tx.Commit(); err != nil {
		return err
	}
	finished := false
	defer func() {
		if !finished {
			report, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, _ = s.DB.ExecContext(report, `UPDATE search_projection_source_shard SET leaseOwner='',leaseExpires=0,nextAttempt=UNIX_TIMESTAMP()+30 WHERE namespace=? AND jobID=? AND className=? AND leaseOwner=? AND leaseEpoch=?`, namespace, job, class, owner, epoch)
		}
	}()
	page, err := provider.Scan(ctx, class, cursor, upper)
	if err != nil {
		return err
	}
	if err = ValidatePage(page, &catalog, class, cursor, upper); err != nil {
		return err
	}
	var materialized, missing, noEngine uint64
	for _, item := range page.Items {
		switch item.Status {
		case "materialized":
			event, err := Decode(item.Event)
			if err != nil {
				return err
			}
			// Original source receipts may already belong to another target; a distinct
			// job receipt schedules this immutable revision without rewriting history.
			event.EventID = sourceEventID(namespace, job, class, item.SourceID, event.Revision)
			if _, err = s.Accept(ctx, event, []Target{target}); err != nil {
				return err
			}
			materialized++
		case "missing":
			missing++
		case "no-engine":
			noEngine++
		}
	}
	status := "scanning"
	if page.Complete {
		status = "complete"
	}

	// Serialize final commits on the parent so concurrent last shards cannot
	// both miss completion. Cursor and control repair creation commit together.
	progress, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = progress.Rollback() }()
	var parent string
	if err = progress.QueryRowContext(ctx, `SELECT jobID FROM search_projection_source_job WHERE namespace=? AND jobID=? FOR UPDATE`, namespace, job).Scan(&parent); err != nil {
		return err
	}
	result, err = progress.ExecContext(ctx, `UPDATE search_projection_source_shard SET cursorID=?,status=?,pages=pages+1,materialized=materialized+?,missing=missing+?,noEngine=noEngine+?,leaseOwner='',leaseExpires=0,nextAttempt=UNIX_TIMESTAMP()+1 WHERE namespace=? AND jobID=? AND className=? AND leaseOwner=? AND leaseEpoch=? AND leaseExpires>UNIX_TIMESTAMP()`, page.NextID, status, materialized, missing, noEngine, namespace, job, class, owner, epoch)
	if err != nil {
		return err
	}
	n, err = result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrLeaseLost
	}

	var pending uint64
	if err = progress.QueryRowContext(ctx, `SELECT COUNT(*) FROM search_projection_source_shard WHERE namespace=? AND jobID=? AND status<>'complete'`, namespace, job).Scan(&pending); err != nil {
		return err
	}
	if pending == 0 {
		repairID := sourceRebuildID(namespace, job)
		if _, err = progress.ExecContext(ctx, `INSERT INTO search_projection_rebuild(namespace,jobID,backendID,generationID,upperPHID,createdEpoch) SELECT ?,?,?,?,COALESCE(MAX(phid),''),UNIX_TIMESTAMP() FROM search_projection_head WHERE namespace=? ON DUPLICATE KEY UPDATE jobID=jobID`, namespace, repairID, target.BackendID, target.GenerationID, namespace); err != nil {
			return err
		}
		var backend, generation string
		if err = progress.QueryRowContext(ctx, `SELECT backendID,generationID FROM search_projection_rebuild WHERE namespace=? AND jobID=?`, namespace, repairID).Scan(&backend, &generation); err != nil {
			return err
		}
		if backend != target.BackendID || generation != target.GenerationID {
			return ErrRebuildConflict
		}
	}
	if err = progress.Commit(); err != nil {
		return err
	}
	finished = true
	return nil
}
func (s *MySQLStore) RunSourceScans(ctx context.Context, namespace, owner string, targets []Target, provider SourceProvider) {
	if ValidateNamespace(namespace) != nil || ValidateTargets(targets) != nil || !namespacePattern.MatchString(owner) || provider == nil {
		return
	}
	filters := make([]string, 0, len(targets))
	args := []any{namespace}
	for _, t := range targets {
		filters = append(filters, "(j.backendID=? AND j.generationID=?)")
		args = append(args, t.BackendID, t.GenerationID)
	}
	query := `SELECT s.jobID,s.className FROM search_projection_source_shard s JOIN search_projection_source_job j ON j.namespace=s.namespace AND j.jobID=s.jobID WHERE s.namespace=? AND s.status='scanning' AND s.nextAttempt<=UNIX_TIMESTAMP() AND s.leaseExpires<=UNIX_TIMESTAMP() AND (` + strings.Join(filters, " OR ") + `) ORDER BY s.nextAttempt,s.jobID,s.className LIMIT 1`
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		poll, cancel := context.WithTimeout(ctx, 5*time.Second)
		var job, class string
		err := s.DB.QueryRowContext(poll, query, args...).Scan(&job, &class)
		cancel()
		if err == nil {
			err = s.StepSourceShard(ctx, namespace, job, class, owner, provider)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) && ctx.Err() == nil {
			slog.Error("search source scan pending", "jobID", job, "className", class)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
