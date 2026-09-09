package dbapi

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// The canonical contract fixtures are the one artifact the Go producer and the
// PHP consumer share by value rather than by convention. Each file is the exact
// JSON body of one endpoint's `data` section, serialized from the Go contract
// types below, and the PHP PhabricatorGorgeDBContractTestCase reads the very
// same bytes to build PhabricatorDatabaseRef / schema objects / SetupIssues and
// assert their fields.
//
// That shared-by-value property is what makes the pair regression-proof:
//
//   - If a Go JSON tag is renamed (say `replicationStatus` back to
//     `replicaStatus`), the marshalled output no longer matches the committed
//     file and TestDBAPICanonicalFixturesMatchContract fails here.
//   - If the PHP adapter reads the wrong key or drops a property, it builds an
//     object missing that value from this same file and the PHP test fails
//     there.
//
// So the file can not be hand-edited into agreement with only one side: it is
// generated from the Go types (regenerate with `GORGE_UPDATE_FIXTURES=1 go test
// ./internal/dbapi -run Canonical`), and the two repos are checked byte-for-byte
// identical by scripts/check-contract-fixtures.sh in the integration workflow.
const canonicalDir = "../../../tests/contract/dbapi/canonical"

// canonicalCases pins one representative, fully-populated value per endpoint.
// The values are chosen to exercise every field a consumer regression could
// silently drop: both a healthy master and an unreachable replica in servers;
// a database/table/column/index chain with charset, collation, engine, type,
// nullability, auto_increment and a composite prefixed unique key in
// schema-diff; a fatal and a non-fatal issue in setup-issues; a real utf8mb4
// set in charset-info; and an initialized master with applied patches and a
// state digest in migrations-status.
func canonicalCases() []struct {
	file  string
	value any
} {
	yes := true
	no := false
	delay := 12

	servers := []contracts.ServerRef{
		{
			RefKey: "db1:3306", Host: "db1", Port: 3306, User: "gorge_ro",
			IsMaster: true, Disabled: false, IsIndividual: true, IsDefaultPartition: true,
			ConnectionStatus: "okay", ConnectionLatency: 0.004,
			ReplicaStatus: "okay",
		},
		{
			RefKey: "db2:3306", Host: "db2", Port: 3306, User: "gorge_ro",
			IsMaster: false, Disabled: false, IsIndividual: false, IsDefaultPartition: false,
			ConnectionStatus: "okay", ConnectionLatency: 0.006,
			ReplicaStatus: "replica-slow", ReplicaMessage: "replica is behind", ReplicaDelay: &delay,
		},
	}

	schemaDiff := []*contracts.SchemaNode{{
		RefKey: "db1:3306", Status: "ok",
		Children: []*contracts.SchemaNode{{
			RefKey: "db1:3306", Database: "phorge_meta_data", CharacterSet: "utf8mb4", Collation: "utf8mb4_bin", Status: "ok",
			Children: []*contracts.SchemaNode{{
				RefKey: "db1:3306", Database: "phorge_meta_data", Table: "patch_status",
				Engine: "InnoDB", Collation: "utf8mb4_bin", Status: "ok",
				Keys: []contracts.SchemaKey{
					{Name: "PRIMARY", ColumnNames: []string{"id"}, Unique: true, IndexType: "BTREE"},
					{Name: "key_patch", ColumnNames: []string{"patch(64)", "kind"}, Unique: true, IndexType: "BTREE"},
				},
				Children: []*contracts.SchemaNode{
					{
						RefKey: "db1:3306", Database: "phorge_meta_data", Table: "patch_status", Column: "id",
						ColumnType: "int(10) unsigned", Nullable: &no, AutoIncrement: &yes, Status: "ok",
					},
					{
						RefKey: "db1:3306", Database: "phorge_meta_data", Table: "patch_status", Column: "patch",
						ColumnType: "varchar(255)", CharacterSet: "utf8mb4", Collation: "utf8mb4_bin",
						Nullable: &no, AutoIncrement: &no, Status: "ok",
					},
				},
			}},
		}},
	}}

	setupIssues := []contracts.SetupIssue{
		{
			Key: "mysql.innodb", Name: "MySQL InnoDB Engine Not Available",
			Summary: "InnoDB is required.",
			Message: "The \"InnoDB\" engine is not available in MySQL.",
			IsFatal: true, RefKey: "db1:3306",
		},
		{
			Key: "mysql.max_allowed_packet", Name: "Small MySQL \"max_allowed_packet\"",
			Message: "max_allowed_packet is smaller than recommended.",
			IsFatal: false, RefKey: "db1:3306",
		},
	}

	charsetInfo := []contracts.CharsetInfo{{
		RefKey: "db1:3306", CharsetDefault: "utf8mb4", CharsetSort: "utf8mb4",
		CharsetFulltext: "utf8mb4", CollateText: "utf8mb4_bin",
		CollateSort: "utf8mb4_bin", CollateFulltext: "utf8mb4_unicode_ci",
	}}

	migrations := []contracts.MigrationStatus{{
		RefKey: "db1:3306", Initialized: true,
		AppliedPatches:     []string{"phabricator:0001.legacy.sql", "phabricator:daemonstatus.sql"},
		ClusterStateDigest: "b0f3c2b0a1d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e",
	}}

	return []struct {
		file  string
		value any
	}{
		{"servers.json", servers},
		{"schema-diff.json", schemaDiff},
		{"setup-issues.json", setupIssues},
		{"charset-info.json", charsetInfo},
		{"migrations-status.json", migrations},
	}
}

// canonicalBytes marshals a value exactly as the HTTP layer's `data` section
// would: indented for a readable, diffable file, with a trailing newline so a
// POSIX text file and `git diff` are happy.
func canonicalBytes(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append(raw, '\n')
}

// TestDBAPICanonicalFixturesMatchContract is the generator and the guard in one
// test. With GORGE_UPDATE_FIXTURES set it (re)writes each file from the Go
// types, so the fixtures can never be hand-authored into a shape the code does
// not actually emit. Without it — the normal CI run — it fails if any committed
// file drifts from what the current types marshal to, which is exactly what a
// renamed or retyped JSON tag would do.
func TestDBAPICanonicalFixturesMatchContract(t *testing.T) {
	update := os.Getenv("GORGE_UPDATE_FIXTURES") != ""

	for _, tc := range canonicalCases() {
		t.Run(tc.file, func(t *testing.T) {
			want := canonicalBytes(t, tc.value)
			path := filepath.Join(canonicalDir, tc.file)

			if update {
				if err := os.MkdirAll(canonicalDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, want, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read canonical fixture (regenerate with GORGE_UPDATE_FIXTURES=1): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("canonical fixture %s drifted from the Go contract types.\n"+
					"Regenerate with GORGE_UPDATE_FIXTURES=1 go test ./internal/dbapi -run Canonical.\n"+
					"--- on disk ---\n%s\n--- from types ---\n%s",
					tc.file, got, want)
			}
		})
	}
}

// TestDBAPICanonicalFixturesRoundTrip is the reverse-verification the plan asks
// for: every canonical file unmarshals back into its Go contract type and
// re-marshals to the identical bytes. A file that was edited by hand into a
// shape the types can parse but not reproduce (an extra key, a reordered field,
// a number written as a string) fails here even before the equality guard.
func TestDBAPICanonicalFixturesRoundTrip(t *testing.T) {
	roundTrip := func(t *testing.T, file string, into any) {
		path := filepath.Join(canonicalDir, file)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(into); err != nil {
			t.Fatalf("%s does not round-trip into its contract type: %v", file, err)
		}
		got := canonicalBytes(t, into)
		if !bytes.Equal(got, raw) {
			t.Fatalf("%s re-marshalled differently than it was stored:\n--- stored ---\n%s\n--- re-marshalled ---\n%s",
				file, raw, got)
		}
	}

	var servers []contracts.ServerRef
	roundTrip(t, "servers.json", &servers)
	var schema []*contracts.SchemaNode
	roundTrip(t, "schema-diff.json", &schema)
	var setup []contracts.SetupIssue
	roundTrip(t, "setup-issues.json", &setup)
	var charset []contracts.CharsetInfo
	roundTrip(t, "charset-info.json", &charset)
	var migrations []contracts.MigrationStatus
	roundTrip(t, "migrations-status.json", &migrations)
}
