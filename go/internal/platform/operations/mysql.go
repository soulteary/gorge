// Package operations provides read-only inventory without business payloads.
package operations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"regexp"
	"time"
)

type Table struct {
	Name        string
	StateColumn string
}

var identifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// MySQL bounds each observation and preserves unavailable independently per table.
// Allocated bytes are estimates, counts are exact at each individual query time.
func MySQL(ctx context.Context, db *sql.DB, tables []Table) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out := map[string]any{"backend": "mysql", "snapshot": "observational", "generatedEpoch": time.Now().Unix()}
	var uuid, name string
	if err := db.QueryRowContext(ctx, "SELECT @@server_uuid,DATABASE()").Scan(&uuid, &name); err == nil {
		out["physicalDatabaseIdentity"] = fmt.Sprintf("%x", sha256.Sum256([]byte(uuid+"/"+name)))
	} else {
		out["physicalIdentityState"] = "unavailable"
	}
	records := []map[string]any{}
	for _, table := range tables {
		record := map[string]any{"table": table.Name, "state": "unavailable"}
		records = append(records, record)
		if !identifier.MatchString(table.Name) || (table.StateColumn != "" && !identifier.MatchString(table.StateColumn)) {
			continue
		}
		var data, index, count int64
		err := db.QueryRowContext(ctx, `SELECT COALESCE(DATA_LENGTH,0),COALESCE(INDEX_LENGTH,0) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=?`, table.Name).Scan(&data, &index)
		if err != nil {
			continue
		}
		if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table.Name+"`").Scan(&count); err != nil {
			continue
		}
		if table.StateColumn != "" {
			rows, err := db.QueryContext(ctx, "SELECT `"+table.StateColumn+"`,COUNT(*) FROM `"+table.Name+"` GROUP BY `"+table.StateColumn+"`")
			if err != nil {
				continue
			}
			states := map[string]int64{}
			valid := true
			for rows.Next() {
				var state string
				var n int64
				if rows.Scan(&state, &n) != nil {
					valid = false
					break
				}
				states[state] = n
			}
			if rows.Err() != nil {
				valid = false
			}
			_ = rows.Close()
			if !valid {
				continue
			}
			record["states"] = states
		}
		record["state"] = "observed"
		record["records"] = count
		record["approximateDataBytes"] = data
		record["approximateIndexBytes"] = index
	}
	out["tables"] = records
	return out
}
