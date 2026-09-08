package doccheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const repositoryRoot = "../../.."

func TestDocumentationIndexesEveryBinary(t *testing.T) {
	index := readFile(t, filepath.Join(repositoryRoot, "docs", "README.md"))
	entries, err := os.ReadDir(filepath.Join(repositoryRoot, "go", "cmd"))
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if entry.IsDir() && !strings.Contains(index, "`"+entry.Name()+"`") {
			t.Errorf("docs/README.md does not list go/cmd/%s", entry.Name())
		}
	}
}

func TestContractIndexCoversEveryDomain(t *testing.T) {
	index := readFile(t, filepath.Join(repositoryRoot, "tests", "contract", "README.md"))
	entries, err := os.ReadDir(filepath.Join(repositoryRoot, "tests", "contract"))
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if entry.IsDir() && !strings.Contains(index, "`"+entry.Name()+"/") {
			t.Errorf("tests/contract/README.md does not list %s/", entry.Name())
		}
	}
}

func TestMakefileRunsEveryE2EScript(t *testing.T) {
	makefile := readFile(t, filepath.Join(repositoryRoot, "Makefile"))
	entries, err := os.ReadDir(filepath.Join(repositoryRoot, "tests", "e2e"))
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".sh" &&
			!strings.Contains(makefile, "tests/e2e/"+entry.Name()) {
			t.Errorf("Makefile e2e target does not run tests/e2e/%s", entry.Name())
		}
	}
}

func TestCurrentDocumentationAvoidsRetiredHTTPAPIs(t *testing.T) {
	retired := []string{
		"srv.Echo()",
		"*echo.Echo",
		"e.HTTPErrorHandler",
		"e.GET(",
		"e.Routes()",
		"c.JSON(http.Status",
		"c.Stream(",
		"c.Response().Committed",
		"Echo v4 路由",
		"gorilla/websocket",
		"golang:1.27-alpine3.22",
	}
	paths := []string{
		filepath.Join(repositoryRoot, "README.md"),
		filepath.Join(repositoryRoot, "docs"),
		filepath.Join(repositoryRoot, "compat"),
		filepath.Join(repositoryRoot, "api", "openapi"),
		filepath.Join(repositoryRoot, "tests", "contract"),
	}

	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			checkRetiredTerms(t, path, retired)
			continue
		}

		err = filepath.WalkDir(path, func(candidate string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			ext := filepath.Ext(candidate)
			if !entry.IsDir() && (ext == ".md" || ext == ".yaml" || ext == ".yml") {
				checkRetiredTerms(t, candidate, retired)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func checkRetiredTerms(t *testing.T, path string, retired []string) {
	t.Helper()
	contents := readFile(t, path)
	for _, term := range retired {
		if strings.Contains(contents, term) {
			rel, err := filepath.Rel(repositoryRoot, path)
			if err != nil {
				rel = path
			}
			t.Errorf("%s contains retired current-API reference %q", rel, term)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}
