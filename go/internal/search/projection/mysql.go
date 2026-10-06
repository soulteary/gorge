package projection

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/soulteary/gorge/go/internal/contracts"
)

var ErrEventConflict = errors.New("search event identity reused with different payload")
var ErrRevisionConflict = errors.New("search revision reused with different payload")

// Target IDs identify immutable backend/generation configurations. The caller
// must reconcile a new target against existing heads before activating it.
type Target struct {
	BackendID    string `json:"backendID"`
	GenerationID string `json:"generationID"`
}
type Receipt struct {
	EventID  string   `json:"eventID"`
	PHID     string   `json:"phid"`
	Revision string   `json:"revision"`
	Status   string   `json:"status"`
	Targets  []Target `json:"targets"`
}
type MySQLStore struct{ DB *sql.DB }

// Schema must be applied by an operator in a dedicated search control database,
// not by the query service at startup. All identities compare byte-for-byte.
const Schema = `CREATE TABLE IF NOT EXISTS search_projection_inbox (
 namespace VARBINARY(64) NOT NULL, eventID VARBINARY(128) NOT NULL,
 envelopeHash VARBINARY(64) NOT NULL, envelope LONGTEXT NOT NULL, response LONGTEXT NOT NULL,
 PRIMARY KEY(namespace,eventID)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS search_projection_revision (
 namespace VARBINARY(64) NOT NULL, phid VARBINARY(64) NOT NULL,
 revision BIGINT NOT NULL, contentHash VARBINARY(64) NOT NULL,
 PRIMARY KEY(namespace,phid,revision)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS search_projection_head (
 namespace VARBINARY(64) NOT NULL, phid VARBINARY(64) NOT NULL,
 revision BIGINT NOT NULL, envelope LONGTEXT NOT NULL,
 PRIMARY KEY(namespace,phid)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS search_projection_delivery (
 namespace VARBINARY(64) NOT NULL, backendID VARBINARY(64) NOT NULL,
 generationID VARBINARY(64) NOT NULL, phid VARBINARY(64) NOT NULL,
 revision BIGINT NOT NULL, eventID VARBINARY(128) NOT NULL,
 status VARBINARY(16) NOT NULL DEFAULT 'pending', createdEpoch BIGINT NOT NULL DEFAULT 0,
 leaseOwner VARBINARY(128) NOT NULL DEFAULT '', leaseEpoch BIGINT UNSIGNED NOT NULL DEFAULT 0,
 leaseExpires BIGINT NOT NULL DEFAULT 0, leaseRenewals BIGINT UNSIGNED NOT NULL DEFAULT 0, attempts BIGINT UNSIGNED NOT NULL DEFAULT 0,
 nextAttempt BIGINT NOT NULL DEFAULT 0, lastError VARBINARY(128) NOT NULL DEFAULT '',
 appliedEpoch BIGINT NULL,
 PRIMARY KEY(namespace,backendID,generationID,phid,revision),
 KEY key_pending(status,namespace,backendID,generationID)
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS search_projection_target (
 namespace VARBINARY(64) NOT NULL, backendID VARBINARY(64) NOT NULL,
 generationID VARBINARY(64) NOT NULL, configHash VARBINARY(64) NOT NULL, indexUUID VARBINARY(128) NOT NULL,
 PRIMARY KEY(namespace,backendID,generationID), UNIQUE KEY key_index_uuid(indexUUID)
) ENGINE=InnoDB;`

// Accept atomically persists receipt, immutable revision identity, latest head
// and every target delivery. Ack means durable acceptance, never indexed.
// Delivery execution is a separate state machine; this primitive has no HTTP
// side effects; its HTTP endpoint is explicitly opt-in.
func (s *MySQLStore) Accept(ctx context.Context, event *contracts.SearchProjection, targets []Target) (*Receipt, error) {
	revision, err := Validate(event)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("at least one delivery target required")
	}
	if err := ValidateTargets(targets); err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encoded)
	eventHash := hex.EncodeToString(sum[:])
	content := *event
	content.EventID = ""
	canonical, _ := json.Marshal(content)
	sum = sha256.Sum256(canonical)
	contentHash := hex.EncodeToString(sum[:])
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO search_projection_inbox(namespace,eventID,envelopeHash,envelope,response) VALUES(?,?,?,?,'') ON DUPLICATE KEY UPDATE eventID=eventID`, event.Namespace, event.EventID, eventHash, string(encoded))
	if err != nil {
		return nil, err
	}
	var storedHash, response string
	err = tx.QueryRowContext(ctx, `SELECT envelopeHash,response FROM search_projection_inbox WHERE namespace=? AND eventID=? FOR UPDATE`, event.Namespace, event.EventID).Scan(&storedHash, &response)
	if err != nil {
		return nil, err
	}
	if storedHash != eventHash {
		return nil, ErrEventConflict
	}
	if response != "" {
		var receipt Receipt
		if err = json.Unmarshal([]byte(response), &receipt); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return &receipt, nil
	}
	// Acquiring the head row first serializes different event IDs for one object.
	_, err = tx.ExecContext(ctx, `INSERT INTO search_projection_head(namespace,phid,revision,envelope) VALUES(?,?,0,'') ON DUPLICATE KEY UPDATE phid=phid`, event.Namespace, event.PHID)
	if err != nil {
		return nil, err
	}
	var head int64
	err = tx.QueryRowContext(ctx, `SELECT revision FROM search_projection_head WHERE namespace=? AND phid=? FOR UPDATE`, event.Namespace, event.PHID).Scan(&head)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO search_projection_revision(namespace,phid,revision,contentHash) VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE phid=phid`, event.Namespace, event.PHID, revision, contentHash)
	if err != nil {
		return nil, err
	}
	err = tx.QueryRowContext(ctx, `SELECT contentHash FROM search_projection_revision WHERE namespace=? AND phid=? AND revision=?`, event.Namespace, event.PHID, revision).Scan(&storedHash)
	if err != nil {
		return nil, err
	}
	if storedHash != contentHash {
		return nil, ErrRevisionConflict
	}
	receipt := &Receipt{EventID: event.EventID, PHID: event.PHID, Revision: event.Revision, Status: "accepted", Targets: append([]Target(nil), targets...)}
	if revision < head {
		receipt.Status = "superseded"
		receipt.Targets = nil
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE search_projection_head SET revision=?,envelope=? WHERE namespace=? AND phid=?`, revision, string(encoded), event.Namespace, event.PHID)
		if err != nil {
			return nil, err
		}
		for _, t := range targets {
			_, err = tx.ExecContext(ctx, `INSERT INTO search_projection_delivery(namespace,backendID,generationID,phid,revision,eventID,status,createdEpoch) VALUES(?,?,?,?,?,?,'pending',UNIX_TIMESTAMP()) ON DUPLICATE KEY UPDATE phid=phid`, event.Namespace, t.BackendID, t.GenerationID, event.PHID, revision, event.EventID)
			if err != nil {
				return nil, err
			}
		}
	}
	raw, _ := json.Marshal(receipt)
	_, err = tx.ExecContext(ctx, `UPDATE search_projection_inbox SET response=? WHERE namespace=? AND eventID=?`, string(raw), event.Namespace, event.EventID)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return receipt, nil
}

// LoadEvent reads an immutable envelope even after a newer head is accepted.
// Durable consumers must never substitute the latest head for an old receipt.
func (s *MySQLStore) LoadEvent(ctx context.Context, namespace, eventID string) (*contracts.SearchProjection, error) {
	var storedHash, raw string
	if err := s.DB.QueryRowContext(ctx, `SELECT envelopeHash,envelope FROM search_projection_inbox WHERE namespace=? AND eventID=?`, namespace, eventID).Scan(&storedHash, &raw); err != nil {
		return nil, err
	}
	event, err := Decode([]byte(raw))
	if err != nil {
		return nil, err
	}
	if event.Namespace != namespace || event.EventID != eventID {
		return nil, fmt.Errorf("stored event identity mismatch")
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(encoded)
	if hex.EncodeToString(sum[:]) != storedHash {
		return nil, fmt.Errorf("stored event envelope hash mismatch")
	}
	return event, nil
}

// ValidateTargets rejects ambiguous destination identities before serving traffic.
func ValidateTargets(targets []Target) error {
	if len(targets) == 0 {
		return fmt.Errorf("at least one delivery target required")
	}
	seen := map[Target]bool{}
	for _, t := range targets {
		if !namespacePattern.MatchString(t.BackendID) || !namespacePattern.MatchString(t.GenerationID) || seen[t] {
			return fmt.Errorf("invalid/duplicate delivery target")
		}
		seen[t] = true
	}
	return nil
}
func ValidateNamespace(namespace string) error {
	if !namespacePattern.MatchString(namespace) {
		return fmt.Errorf("invalid projection namespace")
	}
	return nil
}
