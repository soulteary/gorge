package dbapi

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
)

func TestReadinessRequiresAnEnabledMaster(t *testing.T) {
	master := &DatabaseRef{Host: "master", IsMaster: true}
	replica := &DatabaseRef{Host: "replica"}
	cfg := &ClusterConfig{
		Refs:     []*DatabaseRef{master, replica},
		masters:  []*DatabaseRef{master},
		replicas: []*DatabaseRef{replica},
	}
	svc := NewHealthService(cfg)
	replicaProbed := false
	svc.SetConnFactory(func(dsn DSN, readOnly bool) (*Conn, error) {
		if dsn.Host == "replica" {
			replicaProbed = true
		}
		return nil, errors.New("unreachable")
	})

	if svc.anyReachable(context.Background(), "") {
		t.Fatal("a reachable replica must not make the service ready when every master is down")
	}
	if replicaProbed {
		t.Fatal("readiness should probe enabled masters only")
	}
}

func TestQueryAllClearsStaleProbeStateWithoutMutatingCluster(t *testing.T) {
	delay := 42
	ref := &DatabaseRef{
		Host: "db1", ConnectionStatus: StatusOkay,
		ConnectionMessage: "old message", ReplicaStatus: ReplicationSlow,
		ReplicaMessage: "old lag", ReplicaDelay: &delay,
	}
	svc := NewHealthService(&ClusterConfig{Refs: []*DatabaseRef{ref}})
	svc.SetConnFactory(func(DSN, bool) (*Conn, error) {
		return nil, errors.New("connection refused")
	})

	got, err := svc.QueryAll(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ConnectionStatus != string(StatusFail) {
		t.Fatalf("unexpected probe result: %+v", got)
	}
	if got[0].ReplicaStatus != "" || got[0].ReplicaMessage != "" || got[0].ReplicaDelay != nil {
		t.Fatalf("failed probe retained stale replication data: %+v", got[0])
	}
	if ref.ConnectionStatus != StatusOkay || ref.ReplicaStatus != ReplicationSlow || ref.ReplicaDelay == nil {
		t.Fatalf("request probe mutated the shared cluster ref: %+v", ref)
	}
}

func TestPingAuthenticationFailureReportsAuth(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectPing().WillReturnError(&mysql.MySQLError{Number: 1045, Message: "Access denied for user"})
	mock.ExpectClose()

	ref := &DatabaseRef{Host: "db1", Port: 3306}
	svc := NewHealthService(&ClusterConfig{Refs: []*DatabaseRef{ref}})
	svc.SetConnFactory(func(dsn DSN, readOnly bool) (*Conn, error) {
		return NewConnFromDB(db, dsn, readOnly), nil
	})

	got, err := svc.QueryOne(context.Background(), ref.RefKey(), "bad-password")
	if err != nil {
		t.Fatal(err)
	}
	if got.ConnectionStatus != string(StatusAuth) {
		t.Fatalf("connectionStatus = %q, want auth", got.ConnectionStatus)
	}
	if err := mock.ExpectationsWereMet(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatal(err)
	}
}

func TestReplicationProbeFallsBackForOlderMySQL8(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectPing()
	mock.ExpectQuery("SHOW REPLICA STATUS").WillReturnError(
		&mysql.MySQLError{Number: 1064, Message: "syntax error"})
	mock.ExpectQuery("SHOW SLAVE STATUS").WillReturnRows(
		sqlmock.NewRows([]string{"Seconds_Behind_Master"}).AddRow([]byte("3")))
	mock.ExpectClose()

	ref := &DatabaseRef{Host: "db1", Port: 3306}
	svc := NewHealthService(&ClusterConfig{Refs: []*DatabaseRef{ref}})
	svc.SetConnFactory(func(dsn DSN, readOnly bool) (*Conn, error) {
		return NewConnFromDB(db, dsn, readOnly), nil
	})

	got, gotErr := svc.QueryOne(context.Background(), ref.RefKey(), "secret")
	if gotErr != nil {
		t.Fatal(gotErr)
	}
	if got.ConnectionStatus != string(StatusOkay) || got.ReplicaDelay == nil || *got.ReplicaDelay != 3 {
		t.Fatalf("legacy replication probe result = %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatal(err)
	}
}

func TestReplicationProbeAcceptsModernLagColumn(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectPing()
	mock.ExpectQuery("SHOW REPLICA STATUS").WillReturnRows(
		sqlmock.NewRows([]string{"Seconds_Behind_Source"}).AddRow([]byte("7")))
	mock.ExpectClose()

	ref := &DatabaseRef{Host: "db1", Port: 3306}
	svc := NewHealthService(&ClusterConfig{Refs: []*DatabaseRef{ref}})
	svc.SetConnFactory(func(dsn DSN, readOnly bool) (*Conn, error) {
		return NewConnFromDB(db, dsn, readOnly), nil
	})

	got, gotErr := svc.QueryOne(context.Background(), ref.RefKey(), "secret")
	if gotErr != nil {
		t.Fatal(gotErr)
	}
	if got.ConnectionStatus != string(StatusOkay) || got.ReplicaDelay == nil || *got.ReplicaDelay != 7 {
		t.Fatalf("modern replication probe result = %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatal(err)
	}
}

func TestReplicationProbeReportsResultStreamFailure(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectPing()
	mock.ExpectQuery("SHOW REPLICA STATUS").WillReturnRows(
		sqlmock.NewRows([]string{"Seconds_Behind_Source"}).
			AddRow([]byte("7")).RowError(0, errors.New("connection reset")))
	mock.ExpectClose()

	ref := &DatabaseRef{Host: "db1", Port: 3306}
	svc := NewHealthService(&ClusterConfig{Refs: []*DatabaseRef{ref}})
	svc.SetConnFactory(func(dsn DSN, readOnly bool) (*Conn, error) {
		return NewConnFromDB(db, dsn, readOnly), nil
	})

	got, gotErr := svc.QueryOne(context.Background(), ref.RefKey(), "secret")
	if gotErr != nil {
		t.Fatal(gotErr)
	}
	if got.ConnectionStatus != string(StatusFail) || got.ReplicaStatus != "" {
		t.Fatalf("interrupted replication probe result = %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatal(err)
	}
}
