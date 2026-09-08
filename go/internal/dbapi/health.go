package dbapi

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// HealthService probes each configured node's connection and replication state,
// mirroring PhabricatorDatabaseRef::queryAll. It holds no connection of its
// own: each probe dials with a short timeout and no retry, closing the
// connection when it is done, because a health check must report the state now
// rather than wait out a retry loop.
type HealthService struct {
	config      *ClusterConfig
	connFactory ConnFactory
}

func NewHealthService(cfg *ClusterConfig) *HealthService {
	return &HealthService{config: cfg, connFactory: NewConn}
}

// SetConnFactory overrides the connection opener, for tests.
func (s *HealthService) SetConnFactory(f ConnFactory) { s.connFactory = f }

// QueryAll probes every node and returns their wire shapes.
func (s *HealthService) QueryAll(ctx context.Context, password string) ([]contracts.ServerRef, error) {
	refs := s.config.GetAllRefs()
	out := make([]contracts.ServerRef, 0, len(refs))
	for _, ref := range refs {
		probe := cloneRefForProbe(ref)
		s.probeRef(ctx, probe, password)
		out = append(out, probe.toContract())
	}
	return out, nil
}

// QueryOne probes the one node with the given key. A key that matches no
// configured node is reported to the handler as not-found.
func (s *HealthService) QueryOne(ctx context.Context, refKey, password string) (*contracts.ServerRef, error) {
	for _, ref := range s.config.GetAllRefs() {
		if ref.RefKey() == refKey {
			probe := cloneRefForProbe(ref)
			s.probeRef(ctx, probe, password)
			c := probe.toContract()
			return &c, nil
		}
	}
	return nil, fmt.Errorf("ref %q not found", refKey)
}

// anyReachable reports whether at least one enabled master answered a ping. It
// backs the readiness probe; replicas cannot make a master-dependent service
// ready on their own.
func (s *HealthService) anyReachable(ctx context.Context, password string) bool {
	for _, ref := range s.config.Masters() {
		if ref.Disabled {
			continue
		}
		dsn := s.buildDSN(ref, password)
		conn, err := s.connFactory(dsn, true)
		if err != nil {
			continue
		}
		err = conn.Ping(ctx)
		_ = conn.Close()
		if err == nil {
			return true
		}
	}
	return false
}

func (s *HealthService) probeRef(ctx context.Context, ref *DatabaseRef, password string) {
	resetProbeState(ref)
	dsn := s.buildDSN(ref, password)
	start := time.Now()

	conn, err := s.connFactory(dsn, true)
	if err != nil {
		recordConnectionFailure(ref, err, start)
		return
	}
	defer func() { _ = conn.Close() }()

	if err := conn.Ping(ctx); err != nil {
		recordConnectionFailure(ref, err, start)
		return
	}

	s.probeReplication(ctx, conn, ref, start)
}

func cloneRefForProbe(ref *DatabaseRef) *DatabaseRef {
	probe := *ref
	resetProbeState(&probe)
	return &probe
}

func resetProbeState(ref *DatabaseRef) {
	ref.ConnectionStatus = ""
	ref.ConnectionLatency = 0
	ref.ConnectionMessage = ""
	ref.ReplicaStatus = ""
	ref.ReplicaMessage = ""
	ref.ReplicaDelay = nil
}

func recordConnectionFailure(ref *DatabaseRef, err error, start time.Time) {
	if isAuthFailure(err) {
		ref.ConnectionStatus = StatusAuth
	} else {
		ref.ConnectionStatus = StatusFail
	}
	ref.ConnectionMessage = err.Error()
	ref.ConnectionLatency = time.Since(start).Seconds()
}

func isAuthFailure(err error) bool {
	return classifyMySQLError(err).Kind == kindAccessDenied || isAuthMsg(err.Error())
}

func (s *HealthService) buildDSN(ref *DatabaseRef, password string) DSN {
	return DSN{
		Host:            ref.Host,
		Port:            ref.Port,
		User:            ref.User,
		Password:        password,
		MaxRetries:      0,
		ConnTimeoutSec:  2,
		QueryTimeoutSec: 2,
	}
}

// probeReplication runs SHOW REPLICA STATUS and classifies the result. A
// permission error is StatusReplicationClient, not a failure: the node is up,
// the probing user just cannot see replication. Anything else that fails the
// query is StatusFail.
func (s *HealthService) probeReplication(ctx context.Context, conn *Conn, ref *DatabaseRef, start time.Time) {
	rows, err := conn.QueryContext(ctx, "SHOW REPLICA STATUS")
	if err != nil {
		msg := err.Error()
		switch {
		case isAccessDeniedMsg(msg):
			ref.ConnectionStatus = StatusReplicationClient
			ref.ConnectionMessage = "No permission to run SHOW REPLICA STATUS"
		case isAuthMsg(msg):
			ref.ConnectionStatus = StatusAuth
			ref.ConnectionMessage = msg
		default:
			ref.ConnectionStatus = StatusFail
			ref.ConnectionMessage = msg
		}
		ref.ConnectionLatency = time.Since(start).Seconds()
		return
	}
	defer func() { _ = rows.Close() }()

	ref.ConnectionStatus = StatusOkay
	ref.ConnectionLatency = time.Since(start).Seconds()

	columns, _ := rows.Columns()
	isReplica := rows.Next() && len(columns) > 0

	switch {
	case ref.IsMaster && isReplica:
		ref.ReplicaStatus = ReplicationMasterReplica
		ref.ReplicaMessage = "This host has a master role, but is replicating data from another host"
	case !ref.IsMaster && !isReplica:
		ref.ReplicaStatus = ReplicationReplicaNone
		ref.ReplicaMessage = "This host has a replica role, but is not replicating"
	default:
		ref.ReplicaStatus = ReplicationOkay
	}

	if isReplica {
		s.analyzeReplicaLag(rows, columns, ref)
	}
}

// analyzeReplicaLag extracts Seconds_Behind_Master by column name — the column
// set of SHOW REPLICA STATUS varies by MySQL version, so a positional index
// would break — and flags a slow or stopped replica.
func (s *HealthService) analyzeReplicaLag(rows *sql.Rows, columns []string, ref *DatabaseRef) {
	vals := make([]any, len(columns))
	ptrs := make([]any, len(columns))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	_ = rows.Scan(ptrs...)

	sbmIdx := -1
	for i, col := range columns {
		if col == "Seconds_Behind_Master" {
			sbmIdx = i
			break
		}
	}
	if sbmIdx >= 0 && vals[sbmIdx] != nil {
		var delay int
		if _, err := fmt.Sscanf(fmt.Sprintf("%s", vals[sbmIdx]), "%d", &delay); err == nil {
			ref.ReplicaDelay = &delay
			if delay > 30 {
				ref.ReplicaStatus = ReplicationSlow
				ref.ReplicaMessage = "This replica is lagging far behind the master"
			}
		} else {
			ref.ReplicaStatus = ReplicationNotReplicating
		}
	} else {
		ref.ReplicaStatus = ReplicationNotReplicating
	}
}

// isAccessDeniedMsg and isAuthMsg classify a SHOW REPLICA STATUS error by its
// text. The probe cannot use the errno tables the query path uses, because a
// permission error here is not a health failure — it is a distinct state — so
// it matches on the message the way the standalone service did.
func isAccessDeniedMsg(msg string) bool {
	return strings.Contains(msg, "Access denied") ||
		strings.Contains(msg, "1227") || strings.Contains(msg, "1044")
}

func isAuthMsg(msg string) bool {
	return strings.Contains(msg, "1045") || strings.Contains(msg, "Access denied for user")
}
