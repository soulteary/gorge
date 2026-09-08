package dbapi

import (
	"encoding/json"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
)

func TestDBAPIWireFieldNamesMatchPhorge(t *testing.T) {
	delay := 7
	cases := []struct {
		name   string
		value  any
		want   []string
		absent []string
	}{
		{
			name: "server health",
			value: contracts.ServerRef{
				ReplicaStatus: "okay", ReplicaMessage: "healthy", ReplicaDelay: &delay,
			},
			want:   []string{"replicationStatus", "secondsBehindMaster"},
			absent: []string{"replicaStatus", "replicaDelaySec"},
		},
		{
			name: "schema node",
			value: contracts.SchemaNode{
				Database: "phorge_meta_data", Table: "patch_status", Column: "patch",
				Status: "warn",
			},
			want:   []string{"databaseName", "tableName", "columnName"},
			absent: []string{"database", "table", "column"},
		},
		{
			name: "schema issue",
			value: contracts.SchemaIssue{
				Database: "phorge_meta_data", Table: "patch_status", Column: "patch",
				Key: "column.type", Expected: "varchar(255)", Actual: "text",
				Issue: "wrong type", Status: "fail",
			},
			want:   []string{"databaseName", "tableName", "columnName", "issueKey", "expected", "actual"},
			absent: []string{"database", "table", "column", "key"},
		},
		{
			name:   "setup issue",
			value:  contracts.SetupIssue{Key: "mysql.version", Name: "version", Message: "old"},
			want:   []string{"issueKey"},
			absent: []string{"key"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			for _, key := range tc.want {
				if _, ok := got[key]; !ok {
					t.Errorf("missing wire key %q in %s", key, raw)
				}
			}
			for _, key := range tc.absent {
				if _, ok := got[key]; ok {
					t.Errorf("legacy wire key %q is present in %s", key, raw)
				}
			}
		})
	}
}

func TestFlattenIssuesCarriesExpectedAndActual(t *testing.T) {
	node := &contracts.SchemaNode{
		RefKey: "db1:3306", Database: "phorge_meta_data", Table: "patch_status",
		Column: "patch", Key: "column.type", Expected: "varchar(255)", Actual: "text",
		Issues: []string{"wrong type"}, Status: "fail",
	}
	var got []contracts.SchemaIssue
	flattenIssues(node, &got)
	if len(got) != 1 || got[0].Expected != node.Expected || got[0].Actual != node.Actual {
		t.Fatalf("flattened issue lost expected/actual: %+v", got)
	}
}
