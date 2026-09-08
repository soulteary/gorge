package dbapi

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
)

func TestLoadActualSchemaClassifiesLazyDialFailureAsUnreachable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectQuery("INFORMATION_SCHEMA.SCHEMATA").
		WillReturnError(errors.New("dial tcp: connection refused"))
	mock.ExpectClose()

	ref := &DatabaseRef{Host: "db1", Port: 3306}
	svc := NewDiffService(&ClusterConfig{Refs: []*DatabaseRef{ref}, Namespace: "phorge"}, "secret")
	svc.SetConnFactory(mockConnFactory(db))

	_, gotErr := svc.LoadActualSchema(context.Background(), ref)
	var dbErr *DBError
	if !errors.As(gotErr, &dbErr) || dbErr.Kind != kindUnreachable {
		t.Fatalf("error = %v, want kindUnreachable", gotErr)
	}
}

func TestCharsetInfoPropagatesQueryFailures(t *testing.T) {
	cases := []struct {
		name     string
		queryErr error
		wantKind errorKind
	}{
		{name: "lazy dial", queryErr: errors.New("dial tcp: connection refused"), wantKind: kindUnreachable},
		{name: "access denied", queryErr: &mysql.MySQLError{Number: 1142, Message: "denied"}, wantKind: kindAccessDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			mock.ExpectQuery("INFORMATION_SCHEMA.CHARACTER_SETS").WillReturnError(tc.queryErr)
			mock.ExpectClose()

			ref := &DatabaseRef{Host: "db1", Port: 3306}
			svc := NewDiffService(&ClusterConfig{Refs: []*DatabaseRef{ref}}, "secret")
			svc.SetConnFactory(mockConnFactory(db))
			_, gotErr := svc.charsetInfoForRef(context.Background(), ref)
			var dbErr *DBError
			if !errors.As(gotErr, &dbErr) || dbErr.Kind != tc.wantKind {
				t.Fatalf("error = %v, want kind %d", gotErr, tc.wantKind)
			}
		})
	}
}

func TestCharsetInfoFallsBackOnlyWhenUTF8MB4IsAbsent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectQuery("INFORMATION_SCHEMA.CHARACTER_SETS").
		WillReturnRows(sqlmock.NewRows([]string{"CHARACTER_SET_NAME"}))
	mock.ExpectClose()

	ref := &DatabaseRef{Host: "db1", Port: 3306}
	svc := NewDiffService(&ClusterConfig{Refs: []*DatabaseRef{ref}}, "secret")
	svc.SetConnFactory(mockConnFactory(db))
	info, gotErr := svc.charsetInfoForRef(context.Background(), ref)
	if gotErr != nil {
		t.Fatal(gotErr)
	}
	if info.CharsetDefault != "binary" {
		t.Fatalf("charsetDefault = %q, want binary fallback", info.CharsetDefault)
	}
	if err := mock.ExpectationsWereMet(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatal(err)
	}
}

func TestLoadActualSchemaPreservesDatabaseTableAndColumnProperties(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectQuery("INFORMATION_SCHEMA.SCHEMATA").
		WillReturnRows(sqlmock.NewRows([]string{"SCHEMA_NAME", "DEFAULT_CHARACTER_SET_NAME", "DEFAULT_COLLATION_NAME"}).
			AddRow("phorge_config", "utf8mb4", "utf8mb4_bin"))
	mock.ExpectQuery("INFORMATION_SCHEMA.TABLES").
		WillReturnRows(sqlmock.NewRows([]string{"TABLE_NAME", "TABLE_COLLATION", "ENGINE"}).
			AddRow("config_entry", "utf8mb4_unicode_ci", "InnoDB"))
	mock.ExpectQuery("INFORMATION_SCHEMA.COLUMNS").
		WillReturnRows(sqlmock.NewRows([]string{"COLUMN_NAME", "COLUMN_TYPE", "IS_NULLABLE", "CHARACTER_SET_NAME", "COLLATION_NAME"}).
			AddRow("configValue", "longtext", "YES", "utf8mb4", "utf8mb4_bin"))
	mock.ExpectClose()

	ref := &DatabaseRef{Host: "db1", Port: 3306}
	svc := NewDiffService(&ClusterConfig{Refs: []*DatabaseRef{ref}, Namespace: "phorge"}, "secret")
	svc.SetConnFactory(mockConnFactory(db))
	tree, gotErr := svc.LoadActualSchema(context.Background(), ref)
	if gotErr != nil {
		t.Fatal(gotErr)
	}
	database := tree.Children[0]
	if database.CharacterSet != "utf8mb4" || database.Collation != "utf8mb4_bin" {
		t.Fatalf("database properties were lost: %+v", database)
	}
	table := database.Children[0]
	if table.Engine != "InnoDB" || table.Collation != "utf8mb4_unicode_ci" {
		t.Fatalf("table properties were lost: %+v", table)
	}
	column := table.Children[0]
	if column.ColumnType != "longtext" || column.CharacterSet != "utf8mb4" ||
		column.Collation != "utf8mb4_bin" || column.Nullable == nil || !*column.Nullable {
		t.Fatalf("column properties were lost: %+v", column)
	}
}

func TestLoadActualSchemaUsesLiteralNamespacePrefix(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	query := "SELECT SCHEMA_NAME, DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME " +
		"FROM INFORMATION_SCHEMA.SCHEMATA WHERE LEFT(SCHEMA_NAME, CHAR_LENGTH(?)) = ?"
	mock.ExpectQuery(regexp.QuoteMeta(query)).
		WithArgs("tenant_%_", "tenant_%_").
		WillReturnRows(sqlmock.NewRows([]string{"SCHEMA_NAME", "DEFAULT_CHARACTER_SET_NAME", "DEFAULT_COLLATION_NAME"}))
	mock.ExpectClose()

	ref := &DatabaseRef{Host: "db1", Port: 3306}
	svc := NewDiffService(&ClusterConfig{Refs: []*DatabaseRef{ref}, Namespace: "tenant_%"}, "secret")
	svc.SetConnFactory(mockConnFactory(db))
	if _, gotErr := svc.LoadActualSchema(context.Background(), ref); gotErr != nil {
		t.Fatal(gotErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatal(err)
	}
}

func TestLoadActualSchemaDetectsRowIterationFailuresAtEveryLevel(t *testing.T) {
	streamErr := errors.New("connection reset while reading rows")
	tests := []struct {
		name string
		seed func(sqlmock.Sqlmock)
		load func(*DiffService, *Conn, *DatabaseRef) error
	}{
		{
			name: "databases",
			seed: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery("INFORMATION_SCHEMA.SCHEMATA").WillReturnRows(
					sqlmock.NewRows([]string{"SCHEMA_NAME", "DEFAULT_CHARACTER_SET_NAME", "DEFAULT_COLLATION_NAME"}).
						AddRow("phorge_config", "utf8mb4", "utf8mb4_bin").RowError(0, streamErr))
			},
			load: func(svc *DiffService, conn *Conn, ref *DatabaseRef) error {
				_, err := svc.loadServerSchema(context.Background(), conn, ref)
				return err
			},
		},
		{
			name: "tables",
			seed: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery("INFORMATION_SCHEMA.TABLES").WillReturnRows(
					sqlmock.NewRows([]string{"TABLE_NAME", "TABLE_COLLATION", "ENGINE"}).
						AddRow("config_entry", "utf8mb4_bin", "InnoDB").RowError(0, streamErr))
			},
			load: func(svc *DiffService, conn *Conn, ref *DatabaseRef) error {
				_, err := svc.loadDatabaseSchema(context.Background(), conn, ref.RefKey(), "phorge_config")
				return err
			},
		},
		{
			name: "columns",
			seed: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery("INFORMATION_SCHEMA.TABLES").WillReturnRows(
					sqlmock.NewRows([]string{"TABLE_NAME", "TABLE_COLLATION", "ENGINE"}).
						AddRow("config_entry", "utf8mb4_bin", "InnoDB"))
				mock.ExpectQuery("INFORMATION_SCHEMA.COLUMNS").WillReturnRows(
					sqlmock.NewRows([]string{"COLUMN_NAME", "COLUMN_TYPE", "IS_NULLABLE", "CHARACTER_SET_NAME", "COLLATION_NAME"}).
						AddRow("configValue", "longtext", "YES", "utf8mb4", "utf8mb4_bin").RowError(0, streamErr))
			},
			load: func(svc *DiffService, conn *Conn, ref *DatabaseRef) error {
				_, err := svc.loadDatabaseSchema(context.Background(), conn, ref.RefKey(), "phorge_config")
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			test.seed(mock)
			mock.ExpectClose()
			ref := &DatabaseRef{Host: "db1", Port: 3306}
			svc := NewDiffService(&ClusterConfig{Namespace: "phorge"}, "secret")
			conn := NewConnFromDB(db, DSN{}, true)
			gotErr := test.load(svc, conn, ref)
			_ = conn.Close()
			var dbErr *DBError
			if !errors.As(gotErr, &dbErr) || dbErr.Kind != kindUnreachable {
				t.Fatalf("error = %v, want kindUnreachable", gotErr)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
