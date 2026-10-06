// Package cleanup owns bounded, fenced cleanup of explicitly registered tables.
// It never interprets configurable SQL or deserializes PHP cache contents.
package cleanup

import (
	"encoding/json"
	"fmt"
	"io"
)

type Spec struct{ ID, Role, Table, Column string }

var specs = []Spec{
	{"cache.general.ttl", "cache", "cache_general", "cacheExpires"},
	{"cache.general", "cache", "cache_general", "cacheCreated"},
	{"cache.markup", "cache", "cache_markupcache", "dateCreated"},
	{"conduit.logs", "conduit", "conduit_methodcalllog", "dateCreated"},
	{"daemon.processes", "daemon", "daemon_logevent", "epoch"},
	{"daemon.lock-log", "daemon", "daemon_locklog", "dateCreated"},
}

func Specs() []Spec { return append([]Spec(nil), specs...) }
func Lookup(id string) (Spec, error) {
	for _, s := range specs {
		if s.ID == id {
			return s, nil
		}
	}
	return Spec{}, fmt.Errorf("unknown collector %q", id)
}

type Policy struct {
	ID               string `json:"id"`
	Mode             string `json:"mode"`
	RetentionSeconds int64  `json:"retentionSeconds"`
	IntervalSeconds  int64  `json:"intervalSeconds"`
	BatchRows        int    `json:"batchRows"`
	BatchTimeoutMS   int    `json:"batchTimeoutMS"`
	RunSeconds       int    `json:"runSeconds"`
	RunRows          int64  `json:"runRows"`
	Revision         string `json:"revision"`
}
type Export struct {
	Version      int      `json:"version"`
	GuardEnabled bool     `json:"guardEnabled"`
	ReadOnly     bool     `json:"readOnly"`
	Policies     []Policy `json:"policies"`
}

func Decode(r io.Reader) (*Export, error) {
	var e Export
	d := json.NewDecoder(io.LimitReader(r, 65537))
	d.DisallowUnknownFields()
	if err := d.Decode(&e); err != nil {
		return nil, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("policy export has trailing data")
	}
	if e.Version != 1 || !e.GuardEnabled || e.ReadOnly {
		return nil, fmt.Errorf("version 1 and enabled PHP ownership guard required")
	}
	if len(e.Policies) == 0 || len(e.Policies) > len(specs) {
		return nil, fmt.Errorf("invalid collector count")
	}
	seen := map[string]bool{}
	for _, p := range e.Policies {
		if seen[p.ID] {
			return nil, fmt.Errorf("duplicate collector")
		}
		seen[p.ID] = true
		if err := p.Validate(); err != nil {
			return nil, err
		}
	}
	return &e, nil
}
func (p Policy) Validate() error {
	s, err := Lookup(p.ID)
	if err != nil {
		return err
	}
	if len(p.Revision) == 0 || len(p.Revision) > 64 {
		return fmt.Errorf("%s: invalid revision", p.ID)
	}
	if p.Mode != "retention_seconds" && p.Mode != "expires_at" && p.Mode != "indefinite" {
		return fmt.Errorf("%s: invalid mode", p.ID)
	}
	if s.Column == "cacheExpires" && p.Mode != "expires_at" || s.Column != "cacheExpires" && p.Mode == "expires_at" {
		return fmt.Errorf("%s: incompatible mode", p.ID)
	}
	if p.RetentionSeconds < 0 || p.RetentionSeconds > 3153600000 || p.Mode == "retention_seconds" && p.RetentionSeconds == 0 || p.Mode != "retention_seconds" && p.RetentionSeconds != 0 {
		return fmt.Errorf("%s: invalid retention", p.ID)
	}
	if p.IntervalSeconds < 60 || p.IntervalSeconds > 604800 || p.BatchRows < 1 || p.BatchRows > 100 || p.BatchTimeoutMS < 100 || p.BatchTimeoutMS > 2000 || p.RunSeconds < 1 || p.RunSeconds > 30 || p.RunRows < 1 || p.RunRows > 10000 {
		return fmt.Errorf("%s: budget outside limits", p.ID)
	}
	return nil
}
func predicate(s Spec) string { return "`" + s.Column + "` IS NOT NULL AND `" + s.Column + "` < ?" }
func deleteSQL(s Spec) string {
	return "DELETE FROM `" + s.Table + "` WHERE " + predicate(s) + " ORDER BY `" + s.Column + "`, `id` LIMIT ?"
}
