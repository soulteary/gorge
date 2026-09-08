package dbapi

import (
	"context"
	"database/sql"
	"errors"
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
