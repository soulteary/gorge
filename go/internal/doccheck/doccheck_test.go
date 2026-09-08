package doccheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const repositoryRoot = "../../.."

func TestDocumentationIndexesEveryBinary(t *testing.T) {
	index := readFile(t, filepath.Join(repositoryRoot, "docs", "README.md"))
	binaries := markdownTableCodeSpans(index, "二进制")
	entries, err := os.ReadDir(filepath.Join(repositoryRoot, "go", "cmd"))
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if entry.IsDir() && !binaries[entry.Name()] {
			t.Errorf("docs/README.md does not list go/cmd/%s", entry.Name())
		}
	}
}

func TestContractIndexCoversEveryDomain(t *testing.T) {
	index := readFile(t, filepath.Join(repositoryRoot, "tests", "contract", "README.md"))
	directories := markdownTableCodeSpans(index, "Directory")
	entries, err := os.ReadDir(filepath.Join(repositoryRoot, "tests", "contract"))
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if entry.IsDir() && !hasDirectoryEntry(directories, entry.Name()) {
			t.Errorf("tests/contract/README.md does not list %s/", entry.Name())
		}
	}
}

func TestMakefileRunsEveryE2EScript(t *testing.T) {
	makefile := readFile(t, filepath.Join(repositoryRoot, "Makefile"))
	scripts := e2eRecipeScripts(makefile)
	entries, err := os.ReadDir(filepath.Join(repositoryRoot, "tests", "e2e"))
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".sh" &&
			!scripts["tests/e2e/"+entry.Name()] {
			t.Errorf("Makefile e2e target does not run tests/e2e/%s", entry.Name())
		}
	}
}

func TestMarkdownTableCodeSpansIgnoreProse(t *testing.T) {
	document := "`gorge-worker` is discussed here.\n\n" +
		"| Module | Binary |\n|---|---|\n| render | `gorge-render` |\n"

	got := markdownTableCodeSpans(document, "Binary")
	if !got["gorge-render"] || got["gorge-worker"] {
		t.Fatalf("unexpected indexed binaries: %#v", got)
	}
}

func TestE2ERecipeScriptsIgnoreComments(t *testing.T) {
	makefile := ".PHONY: e2e\n" +
		"e2e:\n" +
		"\t# bash tests/e2e/commented.sh\n" +
		"\tBASE_URL=:8080 bash tests/e2e/active.sh\n" +
		"\t@echo tests/e2e/mentioned.sh\n\n" +
		"next:\n\t@true\n"

	got := e2eRecipeScripts(makefile)
	if !got["tests/e2e/active.sh"] || got["tests/e2e/commented.sh"] || got["tests/e2e/mentioned.sh"] {
		t.Fatalf("unexpected e2e recipe scripts: %#v", got)
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
		"c.Stream",
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

var codeSpanPattern = regexp.MustCompile("`([^`]+)`")

func markdownTableCodeSpans(contents, column string) map[string]bool {
	lines := strings.Split(contents, "\n")
	for i := 0; i+1 < len(lines); i++ {
		headings, ok := markdownTableRow(lines[i])
		if !ok {
			continue
		}
		columnIndex := -1
		for index, heading := range headings {
			if heading == column {
				columnIndex = index
				break
			}
		}
		if columnIndex < 0 || !markdownTableSeparator(lines[i+1], len(headings)) {
			continue
		}

		values := make(map[string]bool)
		for _, line := range lines[i+2:] {
			cells, row := markdownTableRow(line)
			if !row || len(cells) != len(headings) {
				break
			}
			for _, match := range codeSpanPattern.FindAllStringSubmatch(cells[columnIndex], -1) {
				values[match[1]] = true
			}
		}
		return values
	}
	return map[string]bool{}
}

func markdownTableRow(line string) ([]string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return nil, false
	}
	parts := strings.Split(strings.Trim(line, "|"), "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts, true
}

func markdownTableSeparator(line string, columns int) bool {
	cells, ok := markdownTableRow(line)
	if !ok || len(cells) != columns {
		return false
	}
	for _, cell := range cells {
		trimmed := strings.Trim(cell, ":")
		if len(trimmed) < 3 || strings.Trim(trimmed, "-") != "" {
			return false
		}
	}
	return true
}

func hasDirectoryEntry(entries map[string]bool, directory string) bool {
	for entry := range entries {
		if entry == directory+"/" || strings.HasPrefix(entry, directory+"/") {
			return true
		}
	}
	return false
}

func e2eRecipeScripts(makefile string) map[string]bool {
	scripts := make(map[string]bool)
	inE2E := false
	for _, line := range strings.Split(makefile, "\n") {
		if !inE2E {
			inE2E = strings.HasPrefix(line, "e2e:")
			continue
		}
		if line != "" && line[0] != '\t' {
			break
		}
		if line == "" {
			continue
		}
		command := strings.TrimSpace(line)
		if comment := strings.IndexByte(command, '#'); comment >= 0 {
			command = command[:comment]
		}
		fields := strings.Fields(command)
		for i := 1; i < len(fields); i++ {
			if (fields[i-1] == "bash" || fields[i-1] == "sh") && strings.HasPrefix(fields[i], "tests/e2e/") {
				scripts[fields[i]] = true
			}
		}
	}
	return scripts
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
