package contracts

// ContractVersion is the wire-contract version the db-api service implements.
// It is exposed on the capability endpoint (GET /api/db/meta) so the PHP
// consumer can refuse to switch the database console over to a service whose
// contract it does not understand, rather than silently reading fields that
// have moved or changed meaning.
//
// Bump the major when a field is removed or its type/meaning changes in a way
// the current PHP adapter can not read; bump the minor for additive changes a
// current consumer can ignore. It is a string so it can carry the
// "major.minor" form without a second field.
const ContractVersion = "1.0"

// Capabilities is the body of GET /api/db/meta: the small, cheap-to-serve
// description the PHP consumer reads before it routes the database console
// through the service. It names no host, credential or database — only the
// contract the service speaks, the namespace it was configured for, where its
// topology came from, and which capability endpoints it serves — so it is safe
// to answer even though nothing past it is.
//
// ContractVersion is compared by the consumer against the version it was
// written for. Namespace must equal Phorge's storage.default-namespace, or the
// service is reporting on databases named with a different prefix than Phorge
// reads. TopologySource is "file" when the cluster was read from a
// GORGE_DB_CONFIG_FILE local.json and "single-node" when it was built from the
// scalar GORGE_DB_MYSQL_* settings, so the consumer can tell a real cluster
// report from a single-node one. Capabilities lists the /api/db routes this
// build serves, so a consumer can detect a build that predates a route it
// wants without probing for a 404.
type Capabilities struct {
	ContractVersion string   `json:"contractVersion"`
	Namespace       string   `json:"namespace"`
	TopologySource  string   `json:"topologySource"`
	Capabilities    []string `json:"capabilities"`
}

// The db-api types describe the JSON contract of the `gorge-db-api` binary,
// which reports on Phorge's MySQL cluster: which servers are configured, how
// healthy each one is, how its schema compares to what Phorge expects, and how
// far its migrations have run.
//
// This service owns no state of its own. Every field here is derived live from
// a `SHOW`/`SELECT`/`INFORMATION_SCHEMA` query or from the configured cluster
// topology, and it is read straight back by PhabricatorGorgeDBClient on the
// PHP side. So these field names are the wire contract itself — the PHP
// consumers (PhabricatorDatabaseRef, DatabaseSetupCheck, MySQLSetupCheck,
// PhabricatorConfigSchemaQuery) read exactly these camelCase keys.
//
// The pre-monorepo service answered snake_case (`ref_key`, `is_fatal`,
// `connection_status`); the monorepo convention is camelCase and this file is
// where the rename lands. See compat/phorge/README.md.

// ServerRef is one database node, as GET /api/db/servers returns it (an array)
// and GET /api/db/servers/{ref}/health returns it (a single object).
//
// RefKey is `host:port`, the stable identifier a caller uses to address the
// node on the health endpoint. It matches Phorge's own PhabricatorDatabaseRef
// key so the two describe the same server.
//
// The connection and replica halves are populated by a live probe: a short
// read-only connect, a ping, and — for MySQL — `SHOW REPLICA STATUS`. A node
// that cannot be reached still appears, with ConnectionStatus "fail" and the
// reason in ConnectionMessage, because "this server is down" is exactly what a
// health report exists to say.
type ServerRef struct {
	RefKey             string `json:"refKey"`
	Host               string `json:"host"`
	Port               int    `json:"port"`
	User               string `json:"user"`
	IsMaster           bool   `json:"isMaster"`
	Disabled           bool   `json:"disabled"`
	IsIndividual       bool   `json:"isIndividual"`
	IsDefaultPartition bool   `json:"isDefaultPartition"`

	// ConnectionStatus is one of okay, fail, auth, replication-client. The
	// last is not a failure: it means the node answered but the probing user
	// lacks the privilege to run SHOW REPLICA STATUS, which is a permission to
	// grant rather than a server to fix.
	ConnectionStatus  string  `json:"connectionStatus"`
	ConnectionLatency float64 `json:"connectionLatencySec"`
	ConnectionMessage string  `json:"connectionMessage,omitempty"`

	// ReplicaStatus is exposed as replicationStatus and is one of okay,
	// master-replica, replica-none,
	// replica-slow, not-replicating, and is empty for a node whose connection
	// failed. ReplicaDelay is Seconds_Behind_Master, nil when the node is not
	// replicating or the value is NULL.
	ReplicaStatus  string `json:"replicationStatus,omitempty"`
	ReplicaMessage string `json:"replicaMessage,omitempty"`
	ReplicaDelay   *int   `json:"secondsBehindMaster,omitempty"`
}

// SchemaNode is one level of the schema tree GET /api/db/schema-diff returns,
// per server: Server → Database → Table → Column. Every node repeats RefKey
// and its parents' names so a single node carries its full location without a
// walk back up the tree; the deeper fields are omitted at the levels above
// them.
//
// Status is one of ok, warn, fail. Issues holds the human-readable problems
// found at this node; GET /api/db/schema-issues is the same information
// flattened into SchemaIssue records. The property fields preserve the actual
// INFORMATION_SCHEMA values at their applicable level: character set and
// collation on databases, collation and engine on tables, and character set,
// collation, complete type, nullability, and auto_increment on columns. Keys
// carries a table's indexes, one SchemaKey per index.
//
// AccessDenied marks a database node that exists but the configured user is
// not permitted to inspect. It is only ever set on a database node, and only
// in response to an explicit `databases` request parameter naming an expected
// database (the visible-set walk cannot tell "restricted" from "absent" on its
// own). Phorge maps it onto PhabricatorConfigDatabaseSchema::setAccessDenied.
type SchemaNode struct {
	RefKey        string             `json:"refKey"`
	Database      string             `json:"databaseName,omitempty"`
	Table         string             `json:"tableName,omitempty"`
	Column        string             `json:"columnName,omitempty"`
	Key           string             `json:"key,omitempty"`
	CharacterSet  string             `json:"characterSet,omitempty"`
	Collation     string             `json:"collation,omitempty"`
	Engine        string             `json:"engine,omitempty"`
	ColumnType    string             `json:"columnType,omitempty"`
	Nullable      *bool              `json:"nullable,omitempty"`
	AutoIncrement *bool              `json:"autoIncrement,omitempty"`
	AccessDenied  bool               `json:"accessDenied,omitempty"`
	Keys          []SchemaKey        `json:"keys,omitempty"`
	Expected      string             `json:"expected,omitempty"`
	Actual        string             `json:"actual,omitempty"`
	Issues        []string           `json:"issues,omitempty"`
	Status        string             `json:"status"`
	Children      []*SchemaNode      `json:"children,omitempty"`
	Diagnostics   []SchemaDiagnostic `json:"-"`
}

// SchemaKey is one index on a table, as it appears in a table SchemaNode's
// Keys. It is derived from INFORMATION_SCHEMA.STATISTICS: ColumnNames is the
// index's columns ordered by SEQ_IN_INDEX, each carrying a `(prefix)` suffix
// when the index covers only a leading Sub_part of the column, so it matches
// the shape Phorge's `SHOW INDEXES` reflection produces. Unique is the inverse
// of NON_UNIQUE and IndexType is INDEX_TYPE (BTREE, FULLTEXT, …). It is a
// distinct structure from the issue key field: an index is not a diagnostic.
type SchemaKey struct {
	Name        string   `json:"name"`
	ColumnNames []string `json:"columnNames"`
	Unique      bool     `json:"unique"`
	IndexType   string   `json:"indexType,omitempty"`
}

// SchemaDiagnostic keeps each local comparison distinct while SchemaNode's
// compatibility fields expose the first one on the tree response. It is not a
// wire field; /schema-issues flattens it into SchemaIssue records.
type SchemaDiagnostic struct {
	Key      string
	Expected string
	Actual   string
	Issue    string
	Status   string
}

// SchemaIssue is one schema problem, GET /api/db/schema-issues returns an array
// of them. It is a SchemaNode's issue lifted out with the node's location
// attached, so each record is self-describing and ready to render in a report
// without the tree.
type SchemaIssue struct {
	RefKey   string `json:"refKey"`
	Database string `json:"databaseName"`
	Table    string `json:"tableName,omitempty"`
	Column   string `json:"columnName,omitempty"`
	Key      string `json:"issueKey,omitempty"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
	Issue    string `json:"issue"`
	Status   string `json:"status"`
}

// SetupIssue is one environment problem, GET /api/db/setup-issues returns an
// array of them. It mirrors Phorge's own PhabricatorSetupIssue, which is why
// IsFatal is load-bearing rather than advisory: Phorge blocks startup on a
// fatal issue (version too old, InnoDB missing, the meta_data database absent)
// and only warns on the rest (buffer pool small, sql_mode not strict).
//
// Key is the stable identifier of the check (mysql.version, storage.upgrade,
// …), Name is its short title, Message the detail. RefKey names the server the
// issue was found on, empty for a cluster-wide issue.
type SetupIssue struct {
	Key     string `json:"issueKey"`
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
	Message string `json:"message"`
	IsFatal bool   `json:"isFatal"`
	RefKey  string `json:"refKey,omitempty"`
}

// CharsetInfo reports the character set and collation a server supports, GET
// /api/db/charset-info returns one per server. Phorge's schema query needs it
// to know whether it can use utf8mb4 or must fall back, so the six fields are
// the exact inputs PhabricatorStorageManagementAPI::getCharsetInfo produces.
type CharsetInfo struct {
	RefKey          string `json:"refKey"`
	CharsetDefault  string `json:"charsetDefault"`
	CharsetSort     string `json:"charsetSort"`
	CharsetFulltext string `json:"charsetFulltext"`
	CollateText     string `json:"collateText"`
	CollateSort     string `json:"collateSort"`
	CollateFulltext string `json:"collateFulltext"`
}

// MigrationStatus reports how far `bin/storage upgrade` has run on a master,
// GET /api/db/migrations/status returns one per master that serves the
// meta_data partition. It is read from the server's own
// `{namespace}_meta_data.patch_status` table, so Initialized false means that
// database does not exist yet — the pre-upgrade state, not an error.
//
// AppliedPatches is the observed set of patch keys, and nothing more: the
// service owns no expected patch list, so it does not report a total or a
// missing set. Phorge holds the canonical list (PhabricatorSQLPatchList) and
// diffs it against AppliedPatches on the PHP side.
//
// ClusterStateDigest is the SHA-256 of the raw `cluster.databases` state this
// master carries in its hoststate table, or empty when that row is absent. The
// raw state is never returned — it names hosts — but the digest lets Phorge
// detect that two masters disagree on the committed cluster configuration
// (db.state.desync) without the service holding the topology.
//
// Only masters are reported: a replica's patch_status arrives through
// replication, not through a migration of its own.
type MigrationStatus struct {
	RefKey             string   `json:"refKey"`
	Initialized        bool     `json:"initialized"`
	AppliedPatches     []string `json:"appliedPatches"`
	ClusterStateDigest string   `json:"clusterStateDigest,omitempty"`
}
