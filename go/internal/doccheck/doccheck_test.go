package doccheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/soulteary/gorge/go/internal/maintenance/cleanup"
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
		// The Go tool ignores directories whose name starts with "_", so they
		// hold no buildable binary and the index has nothing to list. Skipping
		// them here matches that convention — and keeps a leftover scratch
		// directory, which git does not track when it is empty and so will not
		// show up in `git status`, from failing this test for no reason.
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), "_") && !binaries[entry.Name()] {
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

// Documentation links are part of the navigation contract. Ignore fenced code
// so Markdown examples and signatures such as [](path, dst *T) are not links.
func TestDocumentationLocalLinksExist(t *testing.T) {
	roots := []string{"README.md", "docs", "compat", "tests/contract", "deploy/compose"}
	if fork := os.Getenv(phorgeForkEnv); fork != "" {
		for _, rel := range []string{"README.md", "DOCKER.md", "PRODUCTION-CUTOVER.md", "I18N-zh_CN.md", "scripts/operations", "scripts/i18n"} {
			checkMarkdownLinks(t, filepath.Join(fork, rel))
		}
	}
	for _, rel := range roots {
		checkMarkdownLinks(t, filepath.Join(repositoryRoot, rel))
	}
}

var localLinkPattern = regexp.MustCompile(`\[[^\]\n]*\]\(([^\s)]+)\)`)

func checkMarkdownLinks(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		fenced := false
		for _, line := range strings.Split(readFile(t, path), "\n") {
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
				fenced = !fenced
				continue
			}
			if fenced {
				continue
			}
			for _, match := range localLinkPattern.FindAllStringSubmatch(line, -1) {
				target := match[1]
				if strings.Contains(target, ":") || strings.HasPrefix(target, "#") {
					continue
				}
				target = strings.SplitN(target, "#", 2)[0]
				if _, err := os.Stat(filepath.Join(filepath.Dir(path), target)); err != nil {
					t.Errorf("%s links to missing local target %s", path, target)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Use the production registry rather than a duplicate list in this test.
func TestDocumentationListsCleanupRegistry(t *testing.T) {
	paths := []string{filepath.Join(repositoryRoot, "docs/modules/maintenance.md")}
	if fork := os.Getenv(phorgeForkEnv); fork != "" {
		paths = append(paths, filepath.Join(fork, "PRODUCTION-CUTOVER.md"))
	}
	for _, path := range paths {
		contents := readFile(t, path)
		for _, spec := range cleanup.Specs() {
			if !strings.Contains(contents, "`"+spec.ID+"`") {
				t.Errorf("%s does not list registered collector %s", path, spec.ID)
			}
		}
	}
}

var defaultListenPattern = regexp.MustCompile(`DefaultListenAddr\s*=\s*"(:[0-9]+)"`)
var notificationPortPattern = regexp.MustCompile(`Default(?:Client|Admin)Port\s*=\s*([0-9]+)`)
var internalImportPattern = regexp.MustCompile(`"github.com/soulteary/gorge/go/(internal/[^"]+)"`)

func TestModuleIndexUsesConfiguredDefaultPorts(t *testing.T) {
	index := readFile(t, filepath.Join(repositoryRoot, "docs/README.md"))
	entries, err := os.ReadDir(filepath.Join(repositoryRoot, "go/cmd"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), "_") {
			continue
		}
		main := readFile(t, filepath.Join(repositoryRoot, "go/cmd", entry.Name(), "main.go"))
		for _, imported := range internalImportPattern.FindAllStringSubmatch(main, -1) {
			sources, err := filepath.Glob(filepath.Join(repositoryRoot, "go", imported[1], "*.go"))
			if err != nil {
				t.Fatal(err)
			}
			for _, source := range sources {
				if strings.HasSuffix(source, "_test.go") {
					continue
				}
				contents := readFile(t, source)
				addresses := defaultListenPattern.FindAllStringSubmatch(contents, -1)
				for _, port := range notificationPortPattern.FindAllStringSubmatch(contents, -1) {
					addresses = append(addresses, []string{port[0], ":" + port[1]})
				}
				for _, address := range addresses {
					found := false
					for _, line := range strings.Split(index, "\n") {
						cells, ok := markdownTableRow(line)
						if ok && len(cells) >= 3 && strings.Contains(cells[1], "`"+entry.Name()+"`") && strings.Contains(cells[2], "`"+address[1]+"`") {
							found = true
						}
					}
					if !found {
						t.Errorf("module index does not pair %s with configured default %s", entry.Name(), address[1])
					}
				}
			}
		}
	}
}

func TestPlatformDocumentationIndexesEveryPackage(t *testing.T) {
	index := markdownTableCodeSpans(readFile(t, filepath.Join(repositoryRoot, "docs/platform.md")), "包")
	entries, err := os.ReadDir(filepath.Join(repositoryRoot, "go/internal/platform"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() && !index[entry.Name()] {
			t.Errorf("docs/platform.md does not list shared package %s", entry.Name())
		}
	}
}

// Guard literal API groups mounted in a function. Helpers receiving an already
// mounted router and dynamically constructed paths remain runtime-contract work.
func TestMountedAPIRoutesHaveDocumentation(t *testing.T) {
	var documentation strings.Builder
	for _, root := range []string{"docs", "api/openapi"} {
		if err := filepath.WalkDir(filepath.Join(repositoryRoot, root), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && (filepath.Ext(path) == ".md" || filepath.Ext(path) == ".yaml") {
				documentation.WriteString(readFile(t, path))
				documentation.WriteByte('\n')
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	parameter := regexp.MustCompile(`:(\w+)`)
	err := filepath.WalkDir(filepath.Join(repositoryRoot, "go/internal"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			groups := map[string]string{}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if assignment, ok := node.(*ast.AssignStmt); ok && len(assignment.Lhs) == 1 && len(assignment.Rhs) == 1 {
					name, named := assignment.Lhs[0].(*ast.Ident)
					call, called := assignment.Rhs[0].(*ast.CallExpr)
					if named && called && len(call.Args) > 0 {
						selector, selected := call.Fun.(*ast.SelectorExpr)
						if selected && selector.Sel.Name == "Group" {
							if mount, literal := stringLiteral(call.Args[0]); literal {
								parent := ""
								if receiver, ok := selector.X.(*ast.Ident); ok {
									parent = groups[receiver.Name]
								}
								groups[name.Name] = parent + mount
							}
						}
					}
				}
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !strings.Contains("|Get|Post|Put|Patch|Delete|All|", "|"+selector.Sel.Name+"|") {
					return true
				}
				receiver, ok := selector.X.(*ast.Ident)
				if !ok || !strings.HasPrefix(groups[receiver.Name], "/api/") {
					return true
				}
				if suffix, literal := stringLiteral(call.Args[0]); literal {
					route := groups[receiver.Name] + suffix
					if !strings.Contains(documentation.String(), route) && !strings.Contains(documentation.String(), parameter.ReplaceAllString(route, "{$1}")) {
						t.Errorf("%s mounts %s %s without a docs/OpenAPI reference", path, selector.Sel.Name, route)
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func stringLiteral(expression ast.Expr) (string, bool) {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	return value, err == nil
}

// OpenAPI uses block mappings with components at two spaces and names at four.
// Checking these named local references does not replace YAML/schema validation.
func TestOpenAPILocalComponentReferencesExist(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(repositoryRoot, "api/openapi/*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sectionPattern := regexp.MustCompile(`^  (schemas|responses|securitySchemes):\s*$`)
	namePattern := regexp.MustCompile(`^    ([A-Za-z0-9_-]+):\s*$`)
	referencePattern := regexp.MustCompile(`\$ref:\s*['"]?(#/components/(?:schemas|responses|securitySchemes)/[A-Za-z0-9_-]+)`)
	for _, path := range files {
		contents := readFile(t, path)
		definitions := map[string]bool{}
		section := ""
		for _, line := range strings.Split(contents, "\n") {
			if match := sectionPattern.FindStringSubmatch(line); match != nil {
				section = match[1]
				continue
			}
			if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if section != "" {
				if match := namePattern.FindStringSubmatch(line); match != nil {
					definitions["#/components/"+section+"/"+match[1]] = true
				}
				if !strings.HasPrefix(line, "    ") {
					section = ""
				}
			}
		}
		for _, match := range referencePattern.FindAllStringSubmatch(contents, -1) {
			if !definitions[match[1]] {
				t.Errorf("%s references undefined %s", path, match[1])
			}
		}
	}
}
