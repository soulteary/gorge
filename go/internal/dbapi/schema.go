package dbapi

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// DiffService builds the schema tree and derives the charset info, mirroring
// PhabricatorConfigSchemaQuery. It walks INFORMATION_SCHEMA in three tiers —
// databases, then tables, then columns — filtered to the namespace's own
// databases, and assembles a Server → Database → Table → Column tree.
type DiffService struct {
	config      *ClusterConfig
	password    string
	connFactory ConnFactory
}

func NewDiffService(cfg *ClusterConfig, password string) *DiffService {
	return &DiffService{config: cfg, password: password, connFactory: NewConn}
}

func (s *DiffService) SetConnFactory(f ConnFactory) { s.connFactory = f }

func (s *DiffService) buildDSN(ref *DatabaseRef) DSN {
	return DSN{
		Host:            ref.Host,
		Port:            ref.Port,
		User:            ref.User,
		Password:        ref.PasswordOr(s.password),
		ConnTimeoutSec:  2,
		QueryTimeoutSec: 30,
	}
}

// CollectRefs returns the enabled nodes the schema endpoints iterate.
func (s *DiffService) CollectRefs() []*DatabaseRef {
	var refs []*DatabaseRef
	for _, ref := range s.config.GetAllRefs() {
		if !ref.Disabled {
			refs = append(refs, ref)
		}
	}
	return refs
}

// LoadActualSchema returns the schema tree for one server. requestedDatabases
// is the optional set of expected database names the caller (Phorge) wants
// resolved even when they are not visible: a name that exists but the
// configured user cannot inspect is reported as an accessDenied database node,
// which the visible set alone cannot distinguish from an absent one.
func (s *DiffService) LoadActualSchema(ctx context.Context, ref *DatabaseRef, requestedDatabases ...string) (*contracts.SchemaNode, error) {
	dsn := s.buildDSN(ref)
	conn, err := s.connFactory(dsn, true)
	if err != nil {
		return nil, newDBError(kindUnreachable, "connect for schema on %s", ref.RefKey())
	}
	defer func() { _ = conn.Close() }()

	return s.loadServerSchema(ctx, conn, ref, requestedDatabases)
}

func (s *DiffService) loadServerSchema(ctx context.Context, conn *Conn, ref *DatabaseRef, requestedDatabases []string) (*contracts.SchemaNode, error) {
	server := &contracts.SchemaNode{RefKey: ref.RefKey(), Status: "ok"}

	prefix := s.config.Namespace + "_"
	rows, err := conn.QueryContext(ctx,
		"SELECT SCHEMA_NAME, DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME "+
			"FROM INFORMATION_SCHEMA.SCHEMATA "+
			"WHERE LEFT(SCHEMA_NAME, CHAR_LENGTH(?)) = ?", prefix, prefix)
	if err != nil {
		return nil, classifyMySQLError(err)
	}
	defer func() { _ = rows.Close() }()

	type databaseInfo struct {
		name      string
		charset   string
		collation string
	}
	var databases []databaseInfo
	visible := make(map[string]bool)
	for rows.Next() {
		var name, charset, collation string
		if err := rows.Scan(&name, &charset, &collation); err != nil {
			return nil, classifyMySQLError(err)
		}
		databases = append(databases, databaseInfo{name: name, charset: charset, collation: collation})
		visible[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, classifyMySQLError(err)
	}

	for _, database := range databases {
		dbNode, err := s.loadDatabaseSchema(ctx, conn, ref.RefKey(), database.name)
		if err != nil {
			return nil, err
		}
		dbNode.CharacterSet = database.charset
		dbNode.Collation = database.collation
		server.Children = append(server.Children, dbNode)
	}

	// A requested database that is not visible either does not exist or exists
	// but is not inspectable by the configured user. Only the latter is worth
	// reporting as accessDenied; a genuinely missing database is Phorge's own
	// "expected but absent" case, handled by its comparison against the
	// expected schema, so it is left off the tree here.
	for _, name := range requestedDatabases {
		if name == "" || visible[name] {
			continue
		}
		denied, err := s.databaseExistsButDenied(ctx, conn, name)
		if err != nil {
			return nil, err
		}
		if denied {
			server.Children = append(server.Children, &contracts.SchemaNode{
				RefKey: ref.RefKey(), Database: name, AccessDenied: true, Status: "fail",
			})
		}
	}

	return server, nil
}

// databaseExistsButDenied probes a database the SCHEMATA read did not surface.
// An access-denied error (1044) means it exists but the user cannot see it; a
// "database missing" error (1049) means it is genuinely absent, not restricted,
// so it resolves to false and is left to Phorge's own expected-vs-actual
// comparison. Any other failure is returned classified.
func (s *DiffService) databaseExistsButDenied(ctx context.Context, conn *Conn, dbName string) (bool, error) {
	rows, err := conn.QueryContext(ctx, "SHOW TABLES IN `"+escapeIdent(dbName)+"`")
	if err != nil {
		var myErr *mysql.MySQLError
		if errors.As(err, &myErr) {
			switch myErr.Number {
			case 1044, 1142, 1227: // access denied (database / table / command)
				return true, nil
			case 1049: // unknown database
				return false, nil
			}
		}
		return false, classifyMySQLError(err)
	}
	_ = rows.Close()
	return false, nil
}

// escapeIdent makes a database name safe to interpolate into a backtick-quoted
// identifier. SHOW TABLES IN takes an identifier, not a placeholder, so the
// name (already constrained by the caller to a validated expected database)
// has its backticks doubled to prevent identifier injection.
func escapeIdent(name string) string {
	return strings.ReplaceAll(name, "`", "``")
}

func (s *DiffService) loadDatabaseSchema(ctx context.Context, conn *Conn, refKey, dbName string) (*contracts.SchemaNode, error) {
	dbNode := &contracts.SchemaNode{RefKey: refKey, Database: dbName, Status: "ok"}

	rows, err := conn.QueryContext(ctx,
		"SELECT TABLE_NAME, TABLE_COLLATION, ENGINE FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA = ?",
		dbName)
	if err != nil {
		return nil, classifyMySQLError(err)
	}
	defer func() { _ = rows.Close() }()

	type tableInfo struct {
		name      string
		collation string
		engine    string
	}
	var tables []tableInfo
	for rows.Next() {
		var tableName string
		var collation, engine sql.NullString
		if err := rows.Scan(&tableName, &collation, &engine); err != nil {
			return nil, classifyMySQLError(err)
		}
		tables = append(tables, tableInfo{
			name: tableName, collation: collation.String, engine: engine.String,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, classifyMySQLError(err)
	}

	// One COLUMNS read and one STATISTICS read for the whole database, grouped
	// in memory by table, rather than a query per table: the standalone
	// service's per-table walk was an N+1 against INFORMATION_SCHEMA.
	columnsByTable, err := s.loadColumns(ctx, conn, refKey, dbName)
	if err != nil {
		return nil, err
	}
	keysByTable, err := s.loadKeys(ctx, conn, dbName)
	if err != nil {
		return nil, err
	}

	for _, table := range tables {
		tableNode := &contracts.SchemaNode{
			RefKey: refKey, Database: dbName, Table: table.name,
			Collation: table.collation, Engine: table.engine, Status: "ok",
			Children: columnsByTable[table.name],
			Keys:     keysByTable[table.name],
		}
		dbNode.Children = append(dbNode.Children, tableNode)
	}

	return dbNode, nil
}

// loadColumns reads every column of a database in one query and groups them by
// table, preserving AUTO_INCREMENT from the EXTRA field the way Phorge's
// reflection does.
func (s *DiffService) loadColumns(ctx context.Context, conn *Conn, refKey, dbName string) (map[string][]*contracts.SchemaNode, error) {
	rows, err := conn.QueryContext(ctx,
		"SELECT TABLE_NAME, COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, CHARACTER_SET_NAME, COLLATION_NAME, EXTRA "+
			"FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = ?",
		dbName)
	if err != nil {
		return nil, classifyMySQLError(err)
	}
	defer func() { _ = rows.Close() }()

	byTable := make(map[string][]*contracts.SchemaNode)
	for rows.Next() {
		var tableName, colName, colType, nullable string
		var charset, colCollation, extra sql.NullString
		if err := rows.Scan(&tableName, &colName, &colType, &nullable, &charset, &colCollation, &extra); err != nil {
			return nil, classifyMySQLError(err)
		}
		isNullable := nullable == "YES"
		isAutoIncrement := strings.Contains(strings.ToLower(extra.String), "auto_increment")
		byTable[tableName] = append(byTable[tableName], &contracts.SchemaNode{
			RefKey: refKey, Database: dbName, Table: tableName, Column: colName,
			CharacterSet: charset.String, Collation: colCollation.String,
			ColumnType: colType, Nullable: &isNullable, AutoIncrement: &isAutoIncrement, Status: "ok",
		})
	}
	if err := rows.Err(); err != nil {
		return nil, classifyMySQLError(err)
	}
	return byTable, nil
}

// loadKeys reads every index of a database in one INFORMATION_SCHEMA.STATISTICS
// query and groups them by table then by index name, ordering columns by
// SEQ_IN_INDEX and preserving the Sub_part prefix, so each index matches the
// shape Phorge's SHOW INDEXES reflection produces.
func (s *DiffService) loadKeys(ctx context.Context, conn *Conn, dbName string) (map[string][]contracts.SchemaKey, error) {
	rows, err := conn.QueryContext(ctx,
		"SELECT TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX, COLUMN_NAME, SUB_PART, NON_UNIQUE, INDEX_TYPE "+
			"FROM INFORMATION_SCHEMA.STATISTICS WHERE TABLE_SCHEMA = ? "+
			"ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX",
		dbName)
	if err != nil {
		return nil, classifyMySQLError(err)
	}
	defer func() { _ = rows.Close() }()

	// Preserve first-seen order of indexes within a table so the key list is
	// stable across reads.
	type keyAccum struct {
		key   *contracts.SchemaKey
		order int
	}
	perTable := make(map[string]map[string]*keyAccum)
	tableOrder := make(map[string]int)
	for rows.Next() {
		var tableName, indexName, columnName, indexType string
		var seq int
		var subPart sql.NullInt64
		var nonUnique int
		if err := rows.Scan(&tableName, &indexName, &seq, &columnName, &subPart, &nonUnique, &indexType); err != nil {
			return nil, classifyMySQLError(err)
		}
		byName := perTable[tableName]
		if byName == nil {
			byName = make(map[string]*keyAccum)
			perTable[tableName] = byName
		}
		accum := byName[indexName]
		if accum == nil {
			accum = &keyAccum{
				key: &contracts.SchemaKey{
					Name: indexName, Unique: nonUnique == 0, IndexType: indexType,
				},
				order: tableOrder[tableName],
			}
			tableOrder[tableName]++
			byName[indexName] = accum
		}
		name := columnName
		if subPart.Valid {
			name = name + "(" + strconv.FormatInt(subPart.Int64, 10) + ")"
		}
		accum.key.ColumnNames = append(accum.key.ColumnNames, name)
	}
	if err := rows.Err(); err != nil {
		return nil, classifyMySQLError(err)
	}

	out := make(map[string][]contracts.SchemaKey, len(perTable))
	for tableName, byName := range perTable {
		keys := make([]contracts.SchemaKey, len(byName))
		for _, accum := range byName {
			keys[accum.order] = *accum.key
		}
		out[tableName] = keys
	}
	return out, nil
}

// CollectIssues walks every server's schema tree and flattens the issues found
// at any node into self-describing records.
func (s *DiffService) CollectIssues(ctx context.Context) ([]contracts.SchemaIssue, error) {
	issues := make([]contracts.SchemaIssue, 0)
	refs := s.CollectRefs()
	trees := make(map[string]*contracts.SchemaNode, len(refs))
	for _, ref := range refs {
		tree, err := s.LoadActualSchema(ctx, ref)
		if err != nil {
			issues = append(issues, contracts.SchemaIssue{
				RefKey: ref.RefKey(),
				Issue:  safeSchemaIssue(err),
				Status: "fail",
			})
			continue
		}
		trees[ref.RefKey()] = tree
	}

	for _, ref := range refs {
		tree := trees[ref.RefKey()]
		if tree == nil {
			continue
		}
		s.annotateSchemaAgainstMasters(ref, tree, trees)
		flattenIssues(tree, &issues)
	}
	return issues, nil
}

// annotateSchemaAgainstMasters compares each database on a selected replica
// with the master chosen for that application's partition. The canonical
// Phorge schema remains owned by the PHP SchemaSpec classes and is compared
// there against /schema-diff; this pass catches drift inside the configured
// cluster and gives /schema-issues concrete expected/actual diagnostics.
func (s *DiffService) annotateSchemaAgainstMasters(
	ref *DatabaseRef,
	actualServer *contracts.SchemaNode,
	trees map[string]*contracts.SchemaNode,
) {
	if ref.IsMaster {
		return
	}

	actualDatabases := childrenByName(actualServer.Children, schemaNodeName)
	for _, actualDatabase := range actualServer.Children {
		app, ok := s.applicationForDatabase(actualDatabase.Database)
		if !ok || !s.config.ServesApplication(ref, app) {
			continue
		}
		master := s.config.GetMasterForApplication(app)
		if master == nil {
			continue
		}
		expectedServer := trees[master.RefKey()]
		if expectedServer == nil {
			continue
		}
		expectedDatabase := childrenByName(expectedServer.Children, schemaNodeName)[actualDatabase.Database]
		if expectedDatabase == nil {
			setSchemaIssue(actualDatabase, "surplus", "missing", "present",
				"This schema is not expected to exist.", "fail")
			continue
		}
		compareSchemaNodes(expectedDatabase, actualDatabase)
	}

	// Also materialize databases missing from the selected replica, so the
	// flattened endpoint does not silently omit them.
	for _, expectedServer := range trees {
		for _, expectedDatabase := range expectedServer.Children {
			app, ok := s.applicationForDatabase(expectedDatabase.Database)
			if !ok || !s.config.ServesApplication(ref, app) {
				continue
			}
			master := s.config.GetMasterForApplication(app)
			if master == nil || master.RefKey() != expectedServer.RefKey {
				continue
			}
			if actualDatabases[expectedDatabase.Database] == nil {
				missing := emptySchemaClone(expectedDatabase, ref.RefKey())
				setSchemaIssue(missing, "missing", "present", "missing",
					"This schema is expected to exist, but does not.", "fail")
				actualServer.Children = append(actualServer.Children, missing)
			}
		}
	}
}

func (s *DiffService) applicationForDatabase(database string) (string, bool) {
	prefix := s.config.Namespace + "_"
	if !strings.HasPrefix(database, prefix) {
		return "", false
	}
	return strings.TrimPrefix(database, prefix), true
}

func compareSchemaNodes(expected, actual *contracts.SchemaNode) {
	switch {
	case actual.Column != "":
		compareSchemaProperty(actual, "charset", expected.CharacterSet, actual.CharacterSet,
			"This schema can use a better character set.", "warn")
		compareSchemaProperty(actual, "collation", expected.Collation, actual.Collation,
			"This schema can use a better collation.", "warn")
		if !sameColumnType(expected.ColumnType, actual.ColumnType) {
			setSchemaIssue(actual, "columntype", expected.ColumnType, actual.ColumnType,
				"This schema can use a better column type.", "warn")
		}
		if expected.Nullable != nil && actual.Nullable != nil && *expected.Nullable != *actual.Nullable {
			setSchemaIssue(actual, "nullable", strconv.FormatBool(*expected.Nullable), strconv.FormatBool(*actual.Nullable),
				"This schema has the wrong nullable setting.", "fail")
		}
		if expected.AutoIncrement != nil && actual.AutoIncrement != nil && *expected.AutoIncrement != *actual.AutoIncrement {
			setSchemaIssue(actual, "autoincrement", strconv.FormatBool(*expected.AutoIncrement), strconv.FormatBool(*actual.AutoIncrement),
				"This column has the wrong autoincrement setting.", "warn")
		}
	case actual.Table != "":
		compareSchemaProperty(actual, "collation", expected.Collation, actual.Collation,
			"This schema can use a better collation.", "warn")
		compareSchemaProperty(actual, "engine", expected.Engine, actual.Engine,
			"This table can use a better table engine.", "warn")
	case actual.Database != "":
		compareSchemaProperty(actual, "charset", expected.CharacterSet, actual.CharacterSet,
			"This schema can use a better character set.", "warn")
		compareSchemaProperty(actual, "collation", expected.Collation, actual.Collation,
			"This schema can use a better collation.", "warn")
	}

	expectedChildren := childrenByName(expected.Children, schemaNodeName)
	actualChildren := childrenByName(actual.Children, schemaNodeName)
	for name, actualChild := range actualChildren {
		expectedChild := expectedChildren[name]
		if expectedChild == nil {
			setSchemaIssue(actualChild, "surplus", "missing", "present",
				"This schema is not expected to exist.", "fail")
			continue
		}
		compareSchemaNodes(expectedChild, actualChild)
	}
	for name, expectedChild := range expectedChildren {
		if actualChildren[name] != nil {
			continue
		}
		missing := emptySchemaClone(expectedChild, actual.RefKey)
		setSchemaIssue(missing, "missing", "present", "missing",
			"This schema is expected to exist, but does not.", "fail")
		actual.Children = append(actual.Children, missing)
	}
}

func compareSchemaProperty(node *contracts.SchemaNode, key, expected, actual, issue, status string) {
	if expected != actual {
		setSchemaIssue(node, key, expected, actual, issue, status)
	}
}

// SchemaNode's wire shape carries one expected/actual pair. Keep the first
// local mismatch, matching the most specific comparison order above, while
// still descending into children so every location can report independently.
func setSchemaIssue(node *contracts.SchemaNode, key, expected, actual, issue, status string) {
	node.Diagnostics = append(node.Diagnostics, contracts.SchemaDiagnostic{
		Key: key, Expected: expected, Actual: actual, Issue: issue, Status: status,
	})
	node.Issues = append(node.Issues, issue)
	if node.Key == "" {
		node.Key = key
		node.Expected = expected
		node.Actual = actual
	}
	if status == "fail" || node.Status == "ok" {
		node.Status = status
	}
}

func schemaNodeName(node *contracts.SchemaNode) string {
	switch {
	case node.Column != "":
		return node.Column
	case node.Table != "":
		return node.Table
	default:
		return node.Database
	}
}

func childrenByName(
	children []*contracts.SchemaNode,
	name func(*contracts.SchemaNode) string,
) map[string]*contracts.SchemaNode {
	out := make(map[string]*contracts.SchemaNode, len(children))
	for _, child := range children {
		out[name(child)] = child
	}
	return out
}

func emptySchemaClone(node *contracts.SchemaNode, refKey string) *contracts.SchemaNode {
	return &contracts.SchemaNode{
		RefKey: refKey, Database: node.Database, Table: node.Table, Column: node.Column,
		Status: "ok",
	}
}

func sameColumnType(expected, actual string) bool {
	normalize := func(value string) string {
		switch value {
		case "int(10) unsigned":
			return "int unsigned"
		case "int(10)":
			return "int"
		case "bigint(20) unsigned":
			return "bigint unsigned"
		case "bigint(20)":
			return "bigint"
		default:
			return value
		}
	}
	return normalize(expected) == normalize(actual)
}

func safeSchemaIssue(err error) string {
	var dbErr *DBError
	if errors.As(err, &dbErr) {
		return genericMessage(dbErr.Kind)
	}
	return genericMessage(kindInternal)
}

func flattenIssues(node *contracts.SchemaNode, out *[]contracts.SchemaIssue) {
	if len(node.Diagnostics) > 0 {
		for _, diagnostic := range node.Diagnostics {
			*out = append(*out, contracts.SchemaIssue{
				RefKey: node.RefKey, Database: node.Database, Table: node.Table, Column: node.Column,
				Key: diagnostic.Key, Expected: diagnostic.Expected, Actual: diagnostic.Actual,
				Issue: diagnostic.Issue, Status: diagnostic.Status,
			})
		}
	} else {
		for _, issue := range node.Issues {
			*out = append(*out, contracts.SchemaIssue{
				RefKey:   node.RefKey,
				Database: node.Database,
				Table:    node.Table,
				Column:   node.Column,
				Key:      node.Key,
				Expected: node.Expected,
				Actual:   node.Actual,
				Issue:    issue,
				Status:   node.Status,
			})
		}
	}
	for _, child := range node.Children {
		flattenIssues(child, out)
	}
}

// GetCharsetInfo reports the charset and collation set each server supports,
// choosing utf8mb4 when available and falling back to the binary/utf8 pair
// otherwise. This is the same choice PhabricatorStorageManagementAPI makes, so
// Phorge's schema comparison sees the inputs it expects.
func (s *DiffService) GetCharsetInfo(ctx context.Context) ([]contracts.CharsetInfo, error) {
	results := make([]contracts.CharsetInfo, 0)
	for _, ref := range s.config.GetAllRefs() {
		if ref.Disabled {
			continue
		}
		info, err := s.charsetInfoForRef(ctx, ref)
		if err != nil {
			return nil, err
		}
		results = append(results, *info)
	}
	return results, nil
}

func (s *DiffService) charsetInfoForRef(ctx context.Context, ref *DatabaseRef) (*contracts.CharsetInfo, error) {
	dsn := s.buildDSN(ref)
	dsn.QueryTimeoutSec = 10
	conn, err := s.connFactory(dsn, true)
	if err != nil {
		return nil, newDBError(kindUnreachable, "connect for charset on %s", ref.RefKey())
	}
	defer func() { _ = conn.Close() }()

	var name string
	row := conn.QueryRowContext(ctx,
		"SELECT CHARACTER_SET_NAME FROM INFORMATION_SCHEMA.CHARACTER_SETS WHERE CHARACTER_SET_NAME = 'utf8mb4'")
	err = row.Scan(&name)
	hasUTF8MB4 := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, classifyMySQLError(err)
	}

	info := &contracts.CharsetInfo{RefKey: ref.RefKey()}
	if hasUTF8MB4 {
		info.CharsetDefault = "utf8mb4"
		info.CharsetSort = "utf8mb4"
		info.CharsetFulltext = "utf8mb4"
		info.CollateText = "utf8mb4_bin"
		info.CollateSort = "utf8mb4_unicode_ci"
		info.CollateFulltext = "utf8mb4_unicode_ci"
	} else {
		info.CharsetDefault = "binary"
		info.CharsetSort = "utf8"
		info.CharsetFulltext = "utf8"
		info.CollateText = "binary"
		info.CollateSort = "utf8_general_ci"
		info.CollateFulltext = "utf8_general_ci"
	}
	return info, nil
}
