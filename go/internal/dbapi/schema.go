package dbapi

import (
	"context"

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
		Password:        s.password,
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

// LoadActualSchema returns the schema tree for one server.
func (s *DiffService) LoadActualSchema(ctx context.Context, ref *DatabaseRef) (*contracts.SchemaNode, error) {
	dsn := s.buildDSN(ref)
	conn, err := s.connFactory(dsn, true)
	if err != nil {
		return nil, newDBError(kindUnreachable, "connect for schema on %s", ref.RefKey())
	}
	defer func() { _ = conn.Close() }()

	return s.loadServerSchema(ctx, conn, ref)
}

func (s *DiffService) loadServerSchema(ctx context.Context, conn *Conn, ref *DatabaseRef) (*contracts.SchemaNode, error) {
	server := &contracts.SchemaNode{RefKey: ref.RefKey(), Status: "ok"}

	prefix := s.config.Namespace + "_%"
	rows, err := conn.QueryContext(ctx,
		"SELECT SCHEMA_NAME, DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME "+
			"FROM INFORMATION_SCHEMA.SCHEMATA WHERE SCHEMA_NAME LIKE ?", prefix)
	if err != nil {
		return nil, classifyMySQLError(err)
	}
	defer func() { _ = rows.Close() }()

	var databases []string
	for rows.Next() {
		var name, charset, collation string
		if err := rows.Scan(&name, &charset, &collation); err != nil {
			continue
		}
		databases = append(databases, name)
	}

	for _, dbName := range databases {
		dbNode, err := s.loadDatabaseSchema(ctx, conn, ref.RefKey(), dbName)
		if err != nil {
			dbNode = &contracts.SchemaNode{
				RefKey:   ref.RefKey(),
				Database: dbName,
				Status:   "fail",
				Issues:   []string{err.Error()},
			}
		}
		server.Children = append(server.Children, dbNode)
	}

	return server, nil
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

	var tables []string
	for rows.Next() {
		var tableName, collation, engine string
		if err := rows.Scan(&tableName, &collation, &engine); err != nil {
			continue
		}
		tables = append(tables, tableName)
	}

	for _, tableName := range tables {
		tableNode := &contracts.SchemaNode{RefKey: refKey, Database: dbName, Table: tableName, Status: "ok"}
		colRows, err := conn.QueryContext(ctx,
			"SELECT COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, CHARACTER_SET_NAME, COLLATION_NAME "+
				"FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?",
			dbName, tableName)
		if err == nil {
			for colRows.Next() {
				var colName, colType, nullable string
				var charset, colCollation *string
				if err := colRows.Scan(&colName, &colType, &nullable, &charset, &colCollation); err != nil {
					continue
				}
				tableNode.Children = append(tableNode.Children, &contracts.SchemaNode{
					RefKey:   refKey,
					Database: dbName,
					Table:    tableName,
					Column:   colName,
					Status:   "ok",
				})
			}
			_ = colRows.Close()
		}
		dbNode.Children = append(dbNode.Children, tableNode)
	}

	return dbNode, nil
}

// CollectIssues walks every server's schema tree and flattens the issues found
// at any node into self-describing records.
func (s *DiffService) CollectIssues(ctx context.Context) ([]contracts.SchemaIssue, error) {
	issues := make([]contracts.SchemaIssue, 0)
	for _, ref := range s.config.GetAllRefs() {
		if ref.Disabled {
			continue
		}
		tree, err := s.LoadActualSchema(ctx, ref)
		if err != nil {
			issues = append(issues, contracts.SchemaIssue{
				RefKey: ref.RefKey(),
				Issue:  err.Error(),
				Status: "fail",
			})
			continue
		}
		flattenIssues(tree, &issues)
	}
	return issues, nil
}

func flattenIssues(node *contracts.SchemaNode, out *[]contracts.SchemaIssue) {
	for _, issue := range node.Issues {
		*out = append(*out, contracts.SchemaIssue{
			RefKey:   node.RefKey,
			Database: node.Database,
			Table:    node.Table,
			Column:   node.Column,
			Key:      node.Key,
			Issue:    issue,
			Status:   node.Status,
		})
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
	hasUTF8MB4 := row.Scan(&name) == nil

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
