package cleanup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPHPExportCompatibility(t *testing.T) {
	path := os.Getenv("GORGE_TEST_CLEANUP_EXPORT")
	if path == "" {
		t.Skip("set GORGE_TEST_CLEANUP_EXPORT to the PHP contract output")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}()
	e, err := Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Policies) != len(Specs()) {
		t.Fatal("PHP registry drift")
	}
}
func TestPHPControlSchemaContract(t *testing.T) {
	root := os.Getenv("PHORGE_FORK_DIR")
	if root == "" {
		t.Skip("set PHORGE_FORK_DIR for paired migration check")
	}
	for _, role := range []string{"cache", "conduit", "daemon", "differential", "multimeter"} {
		date := "20261006"
		if role == "differential" || role == "multimeter" {
			date = "20261007"
		}
		raw, err := os.ReadFile(filepath.Join(root, "resources/sql/autopatches/"+date+"."+role+".01.gorgecleanup.sql"))
		if err != nil {
			t.Fatal(err)
		}
		got := strings.ReplaceAll(string(raw), "{$NAMESPACE}_"+role+".", "")
		normalize := func(s string) string {
			return strings.Join(strings.Fields(strings.TrimSuffix(strings.TrimSpace(s), ";")), " ")
		}
		if normalize(got) != normalize(Schema) {
			t.Fatalf("%s control schema drift", role)
		}
	}
}
