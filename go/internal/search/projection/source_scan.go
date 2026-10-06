package projection

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
)

const SourceScanSchema = `CREATE TABLE IF NOT EXISTS search_projection_source_job (
 namespace VARBINARY(64) NOT NULL, jobID VARBINARY(64) NOT NULL,
 backendID VARBINARY(64) NOT NULL, generationID VARBINARY(64) NOT NULL,
 catalog LONGTEXT NOT NULL, createdEpoch BIGINT NOT NULL,
 PRIMARY KEY(namespace,jobID)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS search_projection_source_shard (
 namespace VARBINARY(64) NOT NULL, jobID VARBINARY(64) NOT NULL,
 className VARBINARY(128) NOT NULL, upperID BIGINT NOT NULL, cursorID BIGINT NOT NULL DEFAULT 0,
 status VARBINARY(16) NOT NULL DEFAULT 'scanning', pages BIGINT UNSIGNED NOT NULL DEFAULT 0,
 materialized BIGINT UNSIGNED NOT NULL DEFAULT 0, missing BIGINT UNSIGNED NOT NULL DEFAULT 0,
 noEngine BIGINT UNSIGNED NOT NULL DEFAULT 0, leaseOwner VARBINARY(64) NOT NULL DEFAULT '',
 leaseEpoch BIGINT UNSIGNED NOT NULL DEFAULT 0, leaseExpires BIGINT NOT NULL DEFAULT 0,
 nextAttempt BIGINT NOT NULL DEFAULT 0,
 PRIMARY KEY(namespace,jobID,className), KEY key_pending(namespace,status,nextAttempt,leaseExpires)
) ENGINE=InnoDB;`

var sourceClassPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

type SourceDescriptor struct {
	ClassName string `json:"className"`
	UpperID   string `json:"upperID"`
}
type SourceCatalog struct {
	Version                int                `json:"sourceScanVersion"`
	Namespace              string             `json:"namespace"`
	SerializerVersion      string             `json:"serializerVersion"`
	Scope                  string             `json:"scope"`
	Sources                []SourceDescriptor `json:"sources"`
	UnsupportedClasses     []string           `json:"unsupportedClasses"`
	SourceCoverageVerified bool               `json:"sourceCoverageVerified"`
}
type SourceItem struct {
	SourceID string          `json:"sourceID"`
	PHID     string          `json:"phid"`
	Status   string          `json:"status"`
	Event    json.RawMessage `json:"event,omitempty"`
}
type SourcePage struct {
	Version                int          `json:"sourceScanVersion"`
	Namespace              string       `json:"namespace"`
	SerializerVersion      string       `json:"serializerVersion"`
	ClassName              string       `json:"className"`
	AfterID                string       `json:"afterID"`
	UpperID                string       `json:"upperID"`
	NextID                 string       `json:"nextID"`
	Complete               bool         `json:"complete"`
	Materialized           bool         `json:"materialized"`
	SourceCoverageVerified bool         `json:"sourceCoverageVerified"`
	Items                  []SourceItem `json:"items"`
}
type SourceProvider interface {
	Catalog(context.Context) (*SourceCatalog, error)
	Scan(context.Context, string, string, string) (*SourcePage, error)
}
type SourceShard struct {
	ClassName    string `json:"className"`
	UpperID      string `json:"upperID"`
	CursorID     string `json:"cursorID"`
	Status       string `json:"status"`
	Pages        uint64 `json:"pages"`
	Materialized uint64 `json:"materialized"`
	Missing      uint64 `json:"missing"`
	NoEngine     uint64 `json:"noEngine"`
}
type SourceJob struct {
	Target
	ControlRebuildJobID    string         `json:"controlRebuildJobID"`
	Namespace              string         `json:"namespace"`
	JobID                  string         `json:"jobID"`
	Catalog                *SourceCatalog `json:"catalog"`
	Shards                 []SourceShard  `json:"shards"`
	SourceScanComplete     bool           `json:"sourceScanComplete"`
	SourceCoverageVerified bool           `json:"sourceCoverageVerified"`
	ActivationAllowed      bool           `json:"activationAllowed"`
}

func sourceID(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != s {
		return 0, fmt.Errorf("invalid source ID")
	}
	return n, nil
}
func ValidateCatalog(c *SourceCatalog, namespace string) error {
	if c == nil || c.Version != 1 || c.Namespace != namespace || ValidateNamespace(namespace) != nil || c.Scope != "registered-lisk-fulltext" || c.SourceCoverageVerified || c.SerializerVersion == "" || len(c.SerializerVersion) > 128 || len(c.Sources) == 0 || len(c.Sources) > 64 || len(c.UnsupportedClasses) > 128 {
		return fmt.Errorf("invalid source catalog")
	}
	last := ""
	for _, d := range c.Sources {
		if !sourceClassPattern.MatchString(d.ClassName) || d.ClassName <= last {
			return fmt.Errorf("invalid source descriptors")
		}
		if _, err := sourceID(d.UpperID); err != nil {
			return err
		}
		last = d.ClassName
	}
	last = ""
	for _, name := range c.UnsupportedClasses {
		for _, d := range c.Sources {
			if d.ClassName == name {
				return fmt.Errorf("source listed as unsupported")
			}
		}
		if !sourceClassPattern.MatchString(name) || name <= last {
			return fmt.Errorf("invalid unsupported class list")
		}
		last = name
	}
	return nil
}
func ValidatePage(p *SourcePage, c *SourceCatalog, class, after, upper string) error {
	if c == nil || p == nil || p.Version != 1 || p.Namespace != c.Namespace || p.SerializerVersion != c.SerializerVersion || p.ClassName != class || p.AfterID != after || p.UpperID != upper || !p.Materialized || p.SourceCoverageVerified || len(p.Items) > 32 {
		return fmt.Errorf("invalid source page identity")
	}
	a, err := sourceID(after)
	if err != nil {
		return err
	}
	u, err := sourceID(upper)
	if err != nil {
		return err
	}
	next, err := sourceID(p.NextID)
	if err != nil {
		return err
	}
	if a > u || next < a || next > u || p.Complete && next != u || !p.Complete && (next == a || len(p.Items) == 0) {
		return fmt.Errorf("invalid source page progress")
	}
	last := a
	for _, item := range p.Items {
		id, err := sourceID(item.SourceID)
		if err != nil || id <= last || id > next || (len(item.PHID) > 64 || !phidPattern.MatchString(item.PHID)) {
			return fmt.Errorf("invalid source item")
		}
		last = id
		switch item.Status {
		case "materialized":
			event, err := Decode(item.Event)
			if err != nil {
				return err
			}
			if event.Namespace != c.Namespace || event.PHID != item.PHID || event.Operation != "upsert" || event.SerializerVersion != c.SerializerVersion {
				return fmt.Errorf("invalid materialized source event")
			}
		case "missing", "no-engine":
			if len(item.Event) != 0 {
				return fmt.Errorf("absence cannot carry a deletion")
			}
		default:
			return fmt.Errorf("unresolved source item")
		}
	}
	if !p.Complete && last != next {
		return fmt.Errorf("cursor skipped source items")
	}
	return nil
}
func (s *MySQLStore) CreateSourceJob(ctx context.Context, namespace, id string, target Target, catalog *SourceCatalog) (*SourceJob, error) {
	if err := validateJob(namespace, id, target); err != nil {
		return nil, err
	}
	if err := ValidateCatalog(catalog, namespace); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(catalog)
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var bound string
	if err = tx.QueryRowContext(ctx, `SELECT configHash FROM search_projection_target WHERE namespace=? AND backendID=? AND generationID=?`, namespace, target.BackendID, target.GenerationID).Scan(&bound); err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO search_projection_source_job(namespace,jobID,backendID,generationID,catalog,createdEpoch) VALUES(?,?,?,?,?,UNIX_TIMESTAMP()) ON DUPLICATE KEY UPDATE jobID=jobID`, namespace, id, target.BackendID, target.GenerationID, string(raw))
	if err != nil {
		return nil, err
	}
	var backend, generation, stored string
	if err = tx.QueryRowContext(ctx, `SELECT backendID,generationID,catalog FROM search_projection_source_job WHERE namespace=? AND jobID=? FOR UPDATE`, namespace, id).Scan(&backend, &generation, &stored); err != nil {
		return nil, err
	}
	if backend != target.BackendID || generation != target.GenerationID {
		return nil, ErrRebuildConflict
	}
	var frozen SourceCatalog
	if err = json.Unmarshal([]byte(stored), &frozen); err != nil {
		return nil, err
	}
	if err = ValidateCatalog(&frozen, namespace); err != nil {
		return nil, err
	}
	for _, d := range frozen.Sources {
		_, err = tx.ExecContext(ctx, `INSERT INTO search_projection_source_shard(namespace,jobID,className,upperID) VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE className=className`, namespace, id, d.ClassName, d.UpperID)
		if err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s.SourceJob(ctx, namespace, id)
}
func (s *MySQLStore) SourceJob(ctx context.Context, namespace, id string) (*SourceJob, error) {
	if ValidateNamespace(namespace) != nil || !namespacePattern.MatchString(id) {
		return nil, fmt.Errorf("invalid source job identity")
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	j := &SourceJob{ControlRebuildJobID: sourceRebuildID(namespace, id), Namespace: namespace, JobID: id, Shards: []SourceShard{}, SourceScanComplete: true}
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT backendID,generationID,catalog FROM search_projection_source_job WHERE namespace=? AND jobID=?`, namespace, id).Scan(&j.BackendID, &j.GenerationID, &raw); err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(raw), &j.Catalog); err != nil {
		return nil, err
	}
	if err = ValidateCatalog(j.Catalog, namespace); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT className,upperID,cursorID,status,pages,materialized,missing,noEngine FROM search_projection_source_shard WHERE namespace=? AND jobID=? ORDER BY className`, namespace, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var shard SourceShard
		if err = rows.Scan(&shard.ClassName, &shard.UpperID, &shard.CursorID, &shard.Status, &shard.Pages, &shard.Materialized, &shard.Missing, &shard.NoEngine); err != nil {
			_ = rows.Close()
			return nil, err
		}
		j.Shards = append(j.Shards, shard)
		if shard.Status != "complete" {
			j.SourceScanComplete = false
		}
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(j.Shards) != len(j.Catalog.Sources) {
		return nil, fmt.Errorf("source shard inventory mismatch")
	}
	for i, shard := range j.Shards {
		d := j.Catalog.Sources[i]
		cursor, cursorErr := sourceID(shard.CursorID)
		upper, upperErr := sourceID(shard.UpperID)
		if cursorErr != nil || upperErr != nil || cursor > upper || (shard.Status != "scanning" && shard.Status != "complete") || (shard.Status == "complete" && cursor != upper) {
			return nil, fmt.Errorf("invalid persisted source progress")
		}
		if shard.ClassName != d.ClassName || shard.UpperID != d.UpperID {
			return nil, fmt.Errorf("source shard range mismatch")
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return j, nil
}
func sourceEventID(namespace, job, class, id, revision string) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + job + "\x00" + class + "\x00" + id + "\x00" + revision))
	return "source-scan/" + hex.EncodeToString(sum[:])
}

// The source job owns one immutable control repair job; prefix/hash avoid
// collisions with operator-chosen source job IDs and stay within 64 bytes.
func sourceRebuildID(namespace, job string) string {
	sum := sha256.Sum256([]byte("source-repair\x00" + namespace + "\x00" + job))
	return "source-" + hex.EncodeToString(sum[:28])
}

// SourceScanCheck separates range visitation from exact-revision delivery.
// Neither proves uncaptured business mutations or backend query equivalence.
type SourceScanCheck struct {
	Job                    *SourceJob    `json:"job"`
	Control                *RebuildCheck `json:"control,omitempty"`
	TransportCaughtUp      bool          `json:"transportCaughtUp"`
	SourceCoverageVerified bool          `json:"sourceCoverageVerified"`
	ActivationAllowed      bool          `json:"activationAllowed"`
}

func (s *MySQLStore) CheckSourceJob(ctx context.Context, namespace, id string) (*SourceScanCheck, error) {
	job, err := s.SourceJob(ctx, namespace, id)
	if err != nil {
		return nil, err
	}
	check := &SourceScanCheck{Job: job}
	if !job.SourceScanComplete {
		return check, nil
	}
	// Recover completed jobs created before automatic repair was introduced.
	if _, err = s.CreateRebuild(ctx, namespace, job.ControlRebuildJobID, job.Target); err != nil {
		return nil, err
	}
	control, err := s.CheckRebuild(ctx, namespace, job.ControlRebuildJobID)
	if err != nil {
		return nil, err
	}
	if control.Job.Target != job.Target {
		return nil, ErrRebuildConflict
	}
	check.Control = control
	check.TransportCaughtUp = control.TransportCaughtUp
	return check, nil
}
