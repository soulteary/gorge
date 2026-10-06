package integrations

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/soulteary/gorge/go/internal/platform/conduitclient"
	"strconv"
	"strings"
)

type Fact struct {
	Key       string  `json:"key"`
	Object    string  `json:"objectPHID"`
	Dimension *string `json:"dimensionPHID"`
	Value     string  `json:"value"`
	Epoch     int64   `json:"epoch"`
}
type FactObject struct {
	PHID  string `json:"phid"`
	Facts []Fact `json:"facts"`
}
type FactPage struct {
	Next    string       `json:"next"`
	Objects []FactObject `json:"objects"`
}

func (c FactPage) Validate() error {
	if len(c.Next) > 64 || len(c.Objects) > 20 {
		return errors.New("oversized fact page")
	}
	seen := map[string]bool{}
	count := 0
	for _, o := range c.Objects {
		if o.PHID == "" || len(o.PHID) > 64 || seen[o.PHID] {
			return errors.New("invalid fact object")
		}
		seen[o.PHID] = true
		for _, f := range o.Facts {
			count++
			if f.Object != o.PHID || f.Key == "" || len(f.Key) > 64 || f.Epoch < 0 || f.Epoch > 4294967295 {
				return errors.New("invalid fact datapoint")
			}
			if f.Dimension != nil && (*f.Dimension == "" || len(*f.Dimension) > 64) {
				return errors.New("invalid fact dimension")
			}
			if _, e := strconv.ParseInt(f.Value, 10, 64); e != nil {
				return errors.New("invalid fact value")
			}
		}
	}
	if count > 4096 {
		return errors.New("oversized fact datapoints")
	}
	return nil
}
func FactReady(ctx context.Context, db *sql.DB) error {
	if e := requireInnoDB(ctx, db, []string{"fact_cursor", "fact_keydimension", "fact_objectdimension", "fact_intdatapoint"}); e != nil {
		return e
	}
	for _, q := range []string{"SELECT name,position FROM fact_cursor LIMIT 0", "SELECT id,factKey FROM fact_keydimension LIMIT 0", "SELECT id,objectPHID FROM fact_objectdimension LIMIT 0", "SELECT keyID,objectID,dimensionID,value,epoch FROM fact_intdatapoint LIMIT 0"} {
		r, e := db.QueryContext(ctx, q)
		if e != nil {
			return e
		}
		_ = r.Close()
	}
	return nil
}
func dimension(ctx context.Context, tx *sql.Tx, table, column, value string) (int64, error) {
	// table/column are literals at all call sites, never caller-supplied identifiers.
	r, e := tx.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s(%s) VALUES(?) ON DUPLICATE KEY UPDATE id=LAST_INSERT_ID(id)", table, column), value)
	if e != nil {
		return 0, e
	}
	return r.LastInsertId()
}
func applyFacts(ctx context.Context, tx *sql.Tx, p FactPage) error {
	if e := p.Validate(); e != nil {
		return e
	}
	for _, o := range p.Objects {
		id, e := dimension(ctx, tx, "fact_objectdimension", "objectPHID", o.PHID)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "DELETE FROM fact_intdatapoint WHERE objectID=?", id); e != nil {
			return e
		}
		for _, f := range o.Facts {
			k, e := dimension(ctx, tx, "fact_keydimension", "factKey", f.Key)
			if e != nil {
				return e
			}
			var d any
			if f.Dimension != nil {
				d, e = dimension(ctx, tx, "fact_objectdimension", "objectPHID", *f.Dimension)
				if e != nil {
					return e
				}
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO fact_intdatapoint(keyID,objectID,dimensionID,value,epoch) VALUES(?,?,?,?,?)", k, id, d, f.Value, f.Epoch); e != nil {
				return e
			}
		}
	}
	return nil
}

// The existing fact_cursor row fences each page and commits datapoints together
// with the source position. Empty fact lists explicitly clear old projection.
func ProjectFacts(ctx context.Context, db *sql.DB, c *conduitclient.Client) error {
	r, e := c.Call(ctx, "integration.fact", map[string]any{"phase": "catalog"})
	if e != nil {
		return e
	}
	var names []string
	if e = json.Unmarshal(r.Result, &names); e != nil {
		return e
	}
	for _, name := range names {
		if name == "" || len(name) > 64 {
			return errors.New("invalid fact class")
		}
		if e = projectFactClass(ctx, db, c, name); e != nil {
			return e
		}
	}
	return nil
}
func projectFactClass(ctx context.Context, db *sql.DB, c *conduitclient.Client, name string) error {
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	_, e = tx.ExecContext(ctx, "INSERT INTO fact_cursor(name,position) VALUES(?,'') ON DUPLICATE KEY UPDATE id=id", name)
	if e != nil {
		return e
	}
	var position string
	if e = tx.QueryRowContext(ctx, "SELECT position FROM fact_cursor WHERE name=? FOR UPDATE", name).Scan(&position); e != nil {
		return e
	}
	r, e := c.Call(ctx, "integration.fact", map[string]any{"phase": "scan", "className": name, "position": position})
	if e != nil {
		return e
	}
	var page FactPage
	if e = json.Unmarshal(r.Result, &page); e != nil {
		return e
	}
	if e = page.Validate(); e != nil {
		return e
	}
	if len(page.Objects) == 0 {
		return nil
	}
	if !factPositionAfter(position, page.Next) {
		return errors.New("fact cursor did not advance")
	}
	if e = applyFacts(ctx, tx, page); e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "UPDATE fact_cursor SET position=? WHERE name=?", page.Next, name)
	if e != nil {
		return e
	}
	return tx.Commit()
}

func factPositionAfter(old, next string) bool {
	parse := func(raw string) ([]uint64, bool) {
		parts := strings.Split(raw, ":")
		if len(parts) > 2 {
			return nil, false
		}
		values := []uint64{}
		for _, v := range parts {
			n, e := strconv.ParseUint(v, 10, 64)
			if e != nil {
				return nil, false
			}
			values = append(values, n)
		}
		return values, true
	}
	b, ok := parse(next)
	if !ok {
		return false
	}
	if old == "" {
		return true
	}
	a, ok := parse(old)
	if !ok || len(a) != len(b) {
		return false
	}
	for i := range a {
		if b[i] != a[i] {
			return b[i] > a[i]
		}
	}
	return false
}
