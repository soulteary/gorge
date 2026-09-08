package dbapi

import (
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/contracttest"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// The language-neutral contract fixtures live at the repository root so a PHP
// runner can read the same files; see tests/contract/dbapi/README.md for what
// each one pins and the state a runner has to prepare.
//
// This domain differs from webhook/taskqueue in one way that shapes the whole
// file: its four read services talk to MySQL directly rather than through a
// Store interface, so there is no in-memory backend to seed. The contract
// server instead hands every service a sqlmock-backed connection whose
// expectations are matched by regexp and out of order, seeded to answer as one
// healthy single-node cluster would. What the fixtures pin is the wire shape
// the PHP client (PhabricatorGorgeDBClient) parses; MySQL's own behaviour is
// not under test here.
const (
	fixtureDir            = "../../../tests/contract/dbapi"
	unavailableFixtureDir = fixtureDir + "/unavailable"
)

// contractCluster is the one node every fixture is written against: a single
// master at db1:3306 that is its own default partition. Its refKey, db1:3306,
// is the stable identifier the fixtures address /servers/:ref/health with.
func contractCluster() *ClusterConfig {
	ref := &DatabaseRef{
		Host: "db1", Port: 3306, User: "root",
		IsMaster: true, IsIndividual: true, IsDefaultPartition: true,
	}
	return &ClusterConfig{
		Refs:      []*DatabaseRef{ref},
		Namespace: "phorge",
		masters:   []*DatabaseRef{ref},
	}
}

// seedHealthyNode queues every query the four read endpoints issue against a
// healthy node, at values that produce a clean report: a modern MySQL, InnoDB
// present, the meta_data database created, all server variables sane, one
// applied patch, and utf8mb4 available. Because the mock matches out of order,
// one seeded set answers whichever endpoint the fixture drove, and because each
// connect opens a fresh *sql.DB, the four services do not share one
// expectation set.
func seedHealthyNode(mock sqlmock.Sqlmock) {
	// Health: SHOW REPLICA STATUS. Empty rows => a master that is not itself a
	// replica, which is the "okay" replication state.
	mock.ExpectQuery("SHOW REPLICA STATUS").
		WillReturnRows(sqlmock.NewRows([]string{"Master_Host"}))

	// Schema-diff: the three INFORMATION_SCHEMA tiers. One database, one table,
	// one column, so the tree has a leaf without being large.
	mock.ExpectQuery("INFORMATION_SCHEMA.SCHEMATA").
		WillReturnRows(sqlmock.NewRows([]string{"SCHEMA_NAME", "DEFAULT_CHARACTER_SET_NAME", "DEFAULT_COLLATION_NAME"}).
			AddRow("phorge_meta_data", "utf8mb4", "utf8mb4_bin"))
	mock.ExpectQuery("INFORMATION_SCHEMA.TABLES").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME", "TABLE_COLLATION", "ENGINE"}).
			AddRow("patch_status", "utf8mb4_bin", "InnoDB"))
	mock.ExpectQuery("INFORMATION_SCHEMA.COLUMNS").
		WillReturnRows(sqlmock.NewRows([]string{"COLUMN_NAME", "COLUMN_TYPE", "IS_NULLABLE", "CHARACTER_SET_NAME", "COLLATION_NAME"}).
			AddRow("patch", "varchar(255)", "NO", "utf8mb4", "utf8mb4_bin"))

	// Charset-info: utf8mb4 is present.
	mock.ExpectQuery("INFORMATION_SCHEMA.CHARACTER_SETS").
		WillReturnRows(sqlmock.NewRows([]string{"CHARACTER_SET_NAME"}).AddRow("utf8mb4"))

	// Setup: version, engines, databases, and the five server variables.
	mock.ExpectQuery("SELECT VERSION").
		WillReturnRows(sqlmock.NewRows([]string{"VERSION()"}).AddRow("8.0.33"))
	mock.ExpectQuery("SHOW ENGINES").
		WillReturnRows(sqlmock.NewRows([]string{"Engine", "Support", "c1", "c2", "c3", "c4"}).
			AddRow("InnoDB", "DEFAULT", nil, nil, nil, nil))
	mock.ExpectQuery("SHOW DATABASES").
		WillReturnRows(sqlmock.NewRows([]string{"Database"}).AddRow("phorge_meta_data"))
	mock.ExpectQuery("SELECT @@max_allowed_packet").
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(int64(64 * 1024 * 1024)))
	mock.ExpectQuery("SELECT @@sql_mode").
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow("STRICT_ALL_TABLES,NO_ENGINE_SUBSTITUTION"))
	mock.ExpectQuery("SELECT @@innodb_buffer_pool_size").
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(int64(512 * 1024 * 1024)))
	mock.ExpectQuery("SELECT @@local_infile").
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(0))
	mock.ExpectQuery("SELECT UNIX_TIMESTAMP").
		WillReturnRows(sqlmock.NewRows([]string{"v"}).AddRow(time.Now().Unix()))

	// Migration: the meta_data patch_status table with one applied patch, and
	// the hoststate read the service discards.
	mock.ExpectQuery("SELECT patch FROM patch_status").
		WillReturnRows(sqlmock.NewRows([]string{"patch"}).AddRow("phabricator:0001.legacy.sql"))
	mock.ExpectQuery("SELECT stateValue FROM hoststate").
		WillReturnRows(sqlmock.NewRows([]string{"stateValue"}))
}

// newContractServer builds the routes the way cmd/gorge-db-api does, then swaps
// each service's connection opener for the seeded mock so the endpoints answer
// without a real MySQL. The signature mirrors webhook/taskqueue's; only the
// backend it injects differs.
func newContractServer(t *testing.T, seed func(sqlmock.Sqlmock)) *fiber.App {
	t.Helper()
	deps := NewDeps(contractCluster(), "secret", contracttest.Token)
	setConnFactory(deps, mockFactory(t, seed))
	srv := httpx.New(httpx.Config{Ready: deps.Ready})
	RegisterRoutes(srv.App(), deps)
	return srv.App()
}

// TestContractFixtures replays the happy-path fixtures against a healthy node.
// quietLogs also fails the test if a fixture was answered by way of a recovered
// panic, which a status-only fixture could not otherwise detect.
func TestContractFixtures(t *testing.T) {
	quietLogs(t)
	contracttest.Run(t, newContractServer(t, seedHealthyNode), fixtureDir)
}

// TestContractFixturesWithAnUnreachableDatabase covers the other half of the
// contract — what the endpoints answer when no node is reachable — and needs
// its own service configuration rather than its own request, so it gets its own
// directory. The load-bearing assertion is not the status but the body: a
// caller past the token check still gets no host, database name or SQL.
func TestContractFixturesWithAnUnreachableDatabase(t *testing.T) {
	quietLogs(t)
	deps := NewDeps(contractCluster(), "secret", contracttest.Token)
	setConnFactory(deps, unreachableFactory())
	srv := httpx.New(httpx.Config{Ready: deps.Ready})
	RegisterRoutes(srv.App(), deps)
	contracttest.Run(t, srv.App(), unavailableFixtureDir)
}
