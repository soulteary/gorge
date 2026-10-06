package projection

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrLeaseLost = errors.New("search delivery lease lost")

// Delivery is a fencing token for one target revision. Backend writes must
// independently enforce revision ordering; SQL leases cannot fence HTTP writes.
type Delivery struct {
	Target
	Namespace, PHID, EventID, Owner string
	Revision                        int64
	Epoch, Attempts                 uint64
}

func leaseSeconds(ttl time.Duration) (int64, error) {
	if ttl < time.Second || ttl > time.Hour || ttl%time.Second != 0 {
		return 0, fmt.Errorf("lease must be whole seconds between 1 second and 1 hour")
	}
	return int64(ttl / time.Second), nil
}

// Claim uses the database clock and locks only the chosen delivery. Expired
// owners can be replaced, but their later success/failure updates are fenced.
// Requires MySQL 8 SKIP LOCKED. An empty result is sql.ErrNoRows.
func (s *MySQLStore) Claim(ctx context.Context, namespace string, target Target, owner string, ttl time.Duration) (*Delivery, error) {
	seconds, err := leaseSeconds(ttl)
	if err != nil {
		return nil, err
	}
	if err = ValidateNamespace(namespace); err != nil {
		return nil, err
	}
	if err = ValidateTargets([]Target{target}); err != nil {
		return nil, err
	}
	if !namespacePattern.MatchString(owner) {
		return nil, fmt.Errorf("invalid lease owner")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	d := &Delivery{Target: target, Namespace: namespace, Owner: owner}
	err = tx.QueryRowContext(ctx, `SELECT phid,revision,eventID,leaseEpoch,attempts FROM search_projection_delivery
 WHERE namespace=? AND backendID=? AND generationID=? AND nextAttempt<=UNIX_TIMESTAMP()
 AND (status='pending' OR (status='running' AND leaseExpires<=UNIX_TIMESTAMP()))
 ORDER BY revision,phid LIMIT 1 FOR UPDATE SKIP LOCKED`, namespace, target.BackendID, target.GenerationID).Scan(&d.PHID, &d.Revision, &d.EventID, &d.Epoch, &d.Attempts)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE search_projection_delivery SET status='running',leaseOwner=?,leaseEpoch=leaseEpoch+1,
 leaseExpires=UNIX_TIMESTAMP()+?,attempts=attempts+1 WHERE namespace=? AND backendID=? AND generationID=? AND phid=? AND revision=?`, owner, seconds, namespace, target.BackendID, target.GenerationID, d.PHID, d.Revision)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	d.Epoch++
	d.Attempts++
	return d, nil
}
func (s *MySQLStore) Renew(ctx context.Context, d *Delivery, ttl time.Duration) error {
	seconds, err := leaseSeconds(ttl)
	if err != nil {
		return err
	}
	return s.updateLease(ctx, d, `leaseExpires=UNIX_TIMESTAMP()+?,leaseRenewals=leaseRenewals+1`, seconds)
}

// Finish records backend application, not query visibility. Retry errors use
// fixed diagnostic codes so backend responses and credentials are not persisted.
func (s *MySQLStore) Finish(ctx context.Context, d *Delivery, status string, retryAfter time.Duration) error {
	switch status {
	case "applied":
		return s.updateLease(ctx, d, `status='applied',appliedEpoch=UNIX_TIMESTAMP(),leaseOwner='',leaseExpires=0,nextAttempt=0,lastError=''`)
	case "superseded":
		return s.updateLease(ctx, d, `status='superseded',leaseOwner='',leaseExpires=0,nextAttempt=0,lastError=''`)
	case "retry":
		if retryAfter < time.Second || retryAfter > time.Hour || retryAfter%time.Second != 0 {
			return fmt.Errorf("invalid retry delay")
		}
		return s.updateLease(ctx, d, `status='pending',leaseOwner='',leaseExpires=0,nextAttempt=UNIX_TIMESTAMP()+?,lastError='backend_delivery_failed'`, int64(retryAfter/time.Second))
	default:
		return fmt.Errorf("invalid delivery result")
	}
}
func (s *MySQLStore) updateLease(ctx context.Context, d *Delivery, set string, args ...any) error {
	if d == nil {
		return fmt.Errorf("delivery required")
	}
	args = append(args, d.Namespace, d.BackendID, d.GenerationID, d.PHID, d.Revision, d.EventID, d.Owner, d.Epoch)
	result, err := s.DB.ExecContext(ctx, `UPDATE search_projection_delivery SET `+set+`
 WHERE namespace=? AND backendID=? AND generationID=? AND phid=? AND revision=? AND eventID=?
 AND status='running' AND leaseOwner=? AND leaseEpoch=? AND leaseExpires>UNIX_TIMESTAMP()`, args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrLeaseLost
	}
	return nil
}
