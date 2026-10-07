package contracttest

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"testing"
)

// RestoreFixture exercises binary-safe schema and row restoration on owned test
// databases only. Call after stopping writers. Never call with business tables.
func RestoreFixture(t *testing.T, db *sql.DB, tables ...string) {
	t.Helper()
	ctx := context.Background()
	var name string
	if err := db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	allowed := name == "gorge_integrations_test" || name == "gorge_lifecycle_test" || strings.HasPrefix(name, "gorge_mail_test_") || strings.HasPrefix(name, "gorge_search_test_")
	if !allowed {
		t.Fatal("refusing restore outside owned fixture database")
	}
	type snapshot struct {
		name, ddl string
		rows      [][]any
	}
	backup := []snapshot{}
	identifier := regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	for _, table := range tables {
		if !identifier.MatchString(table) {
			t.Fatal("invalid fixture table")
		}
		var ignored, ddl string
		if err := db.QueryRowContext(ctx, "SHOW CREATE TABLE `"+table+"`").Scan(&ignored, &ddl); err != nil {
			t.Fatal(err)
		}
		rows, err := db.QueryContext(ctx, "SELECT * FROM `"+table+"`")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		data := [][]any{}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err = rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			for i, v := range values {
				if raw, ok := v.([]byte); ok {
					saved := make([]byte, len(raw))
					copy(saved, raw)
					values[i] = saved
				}
			}
			data = append(data, values)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		backup = append(backup, snapshot{table, ddl, data})
	}
	for _, s := range backup {
		if _, err := db.ExecContext(ctx, "DROP TABLE `"+s.name+"`"); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range backup {
		if _, err := db.ExecContext(ctx, s.ddl); err != nil {
			t.Fatal(err)
		}
		for _, row := range s.rows {
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(row)), ",")
			if _, err := db.ExecContext(ctx, "INSERT INTO `"+s.name+"` VALUES ("+placeholders+")", row...); err != nil {
				t.Fatal(err)
			}
		}
	}
}
