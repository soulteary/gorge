package projection

import (
	"os"
	"strings"
	"testing"
)

func TestSearchControlSchemaArtifact(t *testing.T) {
	raw, err := os.ReadFile("../../../../resources/sql/search/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != strings.TrimSpace(Schema) {
		t.Fatal("operator schema differs from tested control schema")
	}
}
