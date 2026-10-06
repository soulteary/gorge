package cleanup

import (
	"strings"
	"testing"
)

func policy(id string) Policy {
	mode := "retention_seconds"
	ttl := int64(2592000)
	if id == "cache.general.ttl" {
		mode = "expires_at"
		ttl = 0
	}
	if id == "daemon.lock-log" {
		mode = "indefinite"
		ttl = 0
	}
	return Policy{ID: id, Mode: mode, RetentionSeconds: ttl, IntervalSeconds: 14400, BatchRows: 100, BatchTimeoutMS: 2000, RunSeconds: 30, RunRows: 10000, Revision: "fixture-v1"}
}
func TestPolicyRejectsUnsafeInputs(t *testing.T) {
	cases := []Policy{policy("unknown")}
	p := policy("cache.general")
	p.RetentionSeconds = -1
	cases = append(cases, p)
	p = policy("cache.general.ttl")
	p.Mode = "retention_seconds"
	p.RetentionSeconds = 30
	cases = append(cases, p)
	p = policy("cache.general")
	p.BatchRows = 101
	cases = append(cases, p)
	p = policy("cache.general")
	p.RunRows = 10001
	cases = append(cases, p)
	p = policy("cache.general")
	p.Mode = "DELETE"
	cases = append(cases, p)
	for _, p := range cases {
		if p.Validate() == nil {
			t.Fatalf("unsafe policy accepted: %+v", p)
		}
	}
}
func TestExportRequiresGuardAndRejectsSQL(t *testing.T) {
	for _, raw := range []string{`{"version":1,"guardEnabled":false,"policies":[]}`, `{"version":1,"guardEnabled":true,"readOnly":true,"policies":[]}`, `{"version":1,"guardEnabled":true,"SQL":"DELETE FROM anything"}`, `{"version":1,"guardEnabled":true,"policies":[]} {}`} {
		if _, err := Decode(strings.NewReader(raw)); err == nil {
			t.Fatalf("unsafe export accepted: %s", raw)
		}
	}
}
