package projection

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// VersionedWriter must fence backend writes independently of the SQL lease and
// make uncertain submissions safely replayable. Async Meili is not compatible.
type VersionedWriter interface {
	ApplyProjection(context.Context, *contracts.SearchProjection, Target) (string, error)
}
type Worker struct {
	Store            *MySQLStore
	Writer           VersionedWriter
	Namespace, Owner string
	Target           Target
}

func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		err := w.Once(ctx)
		if err != nil && !errors.Is(err, sql.ErrNoRows) && ctx.Err() == nil {
			slog.Error("search delivery pending", "backendID", w.Target.BackendID, "generationID", w.Target.GenerationID)
		}
		if err != nil {
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}
func (w *Worker) Once(ctx context.Context) error {
	claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	d, err := w.Store.Claim(claimCtx, w.Namespace, w.Target, w.Owner, 30*time.Second)
	cancel()
	if err != nil {
		return err
	}
	jobCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
	defer stop()
	// Cancel the backend request if renewal fails. Backend external versions are
	// still essential: cancellation cannot retract an already submitted write.
	heartDone := make(chan struct{})
	stopHeart := make(chan struct{})
	go func() {
		defer close(heartDone)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeart:
				return
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				renewCtx, cancel := context.WithTimeout(jobCtx, 5*time.Second)
				err := w.Store.Renew(renewCtx, d, 30*time.Second)
				cancel()
				if err != nil {
					stop()
					return
				}
			}
		}
	}()
	event, err := w.Store.LoadEvent(jobCtx, d.Namespace, d.EventID)
	status := ""
	if err == nil && (event.PHID != d.PHID || event.Revision != strconv.FormatInt(d.Revision, 10)) {
		err = fmt.Errorf("delivery envelope mismatch")
	}
	if err == nil {
		status, err = w.Writer.ApplyProjection(jobCtx, event, d.Target)
	}
	close(stopHeart)
	<-heartDone
	// A shutdown/timeout may leave an accepted backend write. Persist retry only
	// while our lease is still valid; otherwise recovery belongs to the next owner.
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err != nil || jobCtx.Err() != nil {
		delay := time.Duration(int64(1)<<min(d.Attempts, 11)) * time.Second
		if updateErr := w.Store.Finish(reportCtx, d, "retry", delay); updateErr != nil {
			return updateErr
		}
		return fmt.Errorf("backend delivery pending")
	}
	return w.Store.Finish(reportCtx, d, status, 0)
}

// BindTarget pins both configuration and physical index UUID. Reusing target
// IDs for another destination is rejected; new generations need new IDs.
func (s *MySQLStore) BindTarget(ctx context.Context, namespace string, target Target, hash, indexUUID string) error {
	if err := ValidateNamespace(namespace); err != nil {
		return err
	}
	if err := ValidateTargets([]Target{target}); err != nil {
		return err
	}
	if len(hash) != 64 || indexUUID == "" || len(indexUUID) > 128 {
		return fmt.Errorf("invalid target configuration hash")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO search_projection_target(namespace,backendID,generationID,configHash,indexUUID) VALUES(?,?,?,?,?) ON DUPLICATE KEY UPDATE backendID=backendID`, namespace, target.BackendID, target.GenerationID, hash, indexUUID)
	if err != nil {
		return err
	}
	var stored, storedUUID string
	if err = tx.QueryRowContext(ctx, `SELECT configHash,indexUUID FROM search_projection_target WHERE namespace=? AND backendID=? AND generationID=? FOR UPDATE`, namespace, target.BackendID, target.GenerationID).Scan(&stored, &storedUUID); err != nil {
		return err
	}
	if stored != hash || storedUUID != indexUUID {
		return fmt.Errorf("projection target configuration changed")
	}
	return tx.Commit()
}
