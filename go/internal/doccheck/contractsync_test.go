package doccheck

// contractsync_test.go is the drift guard described in compat/phorge/README.md:
// it proves the cross-repository contract register in
// go/internal/contracts/manifest.go still agrees with both sides it pins
// together — the Go structs/routes/constants in this repository, and the PHP
// string literals in phorge-fork.
//
// It is three tests with three different jobs:
//
//   - TestManifestMatchesGoContracts runs everywhere, with no external
//     dependency. It reparses the contracts package and the domain packages and
//     asserts every name the manifest registers actually exists on the Go side:
//     each wire field is a real json tag, each route is a registered path, each
//     error code is a declared constant string. This is what stops the manifest
//     from becoming a place to write names that do not exist — it can not drift
//     away from the Go source it claims to describe.
//
//   - TestManifestMatchesPhorgeFork is the actual cross-repo check. It needs the
//     phorge-fork tree, whose path arrives in PHORGE_FORK_DIR; with the variable
//     unset it t.Skip()s, so a lone clone of gorge (which has no sibling
//     phorge-fork) does not turn CI red. When the tree is present it reads the
//     exact PHP files each manifest item names and asserts the same spelling
//     appears there as a quoted literal, failing with a message that names the
//     field and the file to edit.
//
//   - TestManifestMatchesPhorgeForkInReverse runs the same comparison backwards
//     and skips on the same variable. The forward check can only find names
//     somebody remembered to register; this one reads the wire-boundary PHP
//     files and fails on any key there with no manifest entry, which is how a
//     surplus or misspelled PHP-side key gets caught. What counts as a key to
//     check, and why the exception list is as short as it is, is documented on
//     contracts.ReverseScanFiles.
//
// The correct order for changing a shared name is: edit the Go side and its
// manifest entry together, run this (via `make docs-check`), then follow the
// failure to the phorge-fork file it names and change the PHP literal to match.
// See the header of go/internal/contracts/manifest.go.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
)

// phorgeForkEnv is the environment variable that points at a checked-out
// phorge-fork tree. Unset means "not available", which is a skip, not a
// failure: the drift guard is a cross-repo check and gorge is also cloned and
// tested on its own.
const phorgeForkEnv = "PHORGE_FORK_DIR"

// contractsDir is where the domain contract structs live, relative to the
// repository root. Their json tags are the Go half of every wire field.
var contractsDir = filepath.Join(repositoryRoot, "go", "internal", "contracts")

// domainGoSources maps each manifest domain to the Go package directory whose
// http.go registers its routes and whose files declare its error-code
// constants. The Go-side coverage test scans these for route paths and
// error-code strings.
var domainGoSources = map[string]string{
	"mailer":    filepath.Join(repositoryRoot, "go", "internal", "mailer"),
	"search":    filepath.Join(repositoryRoot, "go", "internal", "search"),
	"webhook":   filepath.Join(repositoryRoot, "go", "internal", "webhook"),
	"taskqueue": filepath.Join(repositoryRoot, "go", "internal", "taskqueue"),
	"dbapi":     filepath.Join(repositoryRoot, "go", "internal", "dbapi"),
}

// TestManifestMatchesGoContracts asserts the manifest never registers a name
// that does not exist on the Go side. It is the guard that keeps the manifest
// honest: without it, someone could "fix" a drift failure by editing the
// manifest to match a renamed PHP literal, leaving the Go struct untouched.
//
// A name proves out on the Go side in one of two ways, and the check accepts
// either, because the contract genuinely has two shapes:
//
//   - camelCase wire fields (from, textBody, refKey, …) are json tags on the
//     domain's contract structs — the JSON envelope keys;
//   - the search domain's four-character names (titl, ownr, …) are not tags but
//     the string values of Go constants in internal/search/esquery (FieldTitle
//     = "titl"), the same values PHP pins as constant-class values.
//
// So a wire field passes if it is a json tag OR appears as a quoted literal in
// the domain source. Routes must appear as the sub-path registered after the
// app.Group prefix; error codes as a quoted constant value.
func TestManifestMatchesGoContracts(t *testing.T) {
	jsonTags := collectJSONTags(t)

	for _, domain := range contracts.Manifest() {
		goSource := readGoSourceConcatenated(t, domainGoSources[domain.Domain])

		for _, field := range domain.WireFields {
			if !jsonTags[field.Name] && !strings.Contains(goSource, `"`+field.Name+`"`) {
				t.Errorf(
					"manifest registers wire field %q for domain %q, but it is neither a json tag in %s nor a string literal in %s; "+
						"either the Go contract was renamed (change the manifest to match) or the manifest invented a field",
					field.Name, domain.Domain, contractsDir, domainGoSources[domain.Domain])
			}
		}

		for _, route := range domain.Routes {
			if !goSourceHasRoute(goSource, route.Name) {
				t.Errorf(
					"manifest registers route %q for domain %q, but its sub-path is not registered in %s; "+
						"the Go route was likely renamed",
					route.Name, domain.Domain, domainGoSources[domain.Domain])
			}
		}

		for _, code := range domain.ErrorCodes {
			if !strings.Contains(goSource, `"`+code.Name+`"`) {
				t.Errorf(
					"manifest registers error code %q for domain %q, but no constant string with that value exists in %s; "+
						"the Go error code was likely renamed",
					code.Name, domain.Domain, domainGoSources[domain.Domain])
			}
		}
	}
}

// requireForkDir returns the phorge-fork tree the cross-repo checks read, or
// skips. Unset is a skip rather than a failure because the drift guard is a
// cross-repo check and gorge is also cloned and tested on its own.
func requireForkDir(t *testing.T) string {
	t.Helper()
	forkDir := os.Getenv(phorgeForkEnv)
	if forkDir == "" {
		t.Skipf(
			"%s is not set; skipping the phorge-fork drift check. Set it to a checked-out phorge-fork tree to run it, e.g. "+
				"PHORGE_FORK_DIR=/path/to/phorge-fork go test ./internal/doccheck -run TestManifestMatchesPhorgeFork",
			phorgeForkEnv)
	}
	if info, err := os.Stat(forkDir); err != nil || !info.IsDir() {
		t.Fatalf("%s=%q is not a directory: %v", phorgeForkEnv, forkDir, err)
	}
	return forkDir
}

// TestManifestMatchesPhorgeFork is the cross-repository drift guard. For every
// manifest item that names phorge-fork files, it asserts the same spelling
// appears in each of them as a quoted literal. Items with no PHPFiles are
// intentionally skipped: no literal with that spelling exists in phorge-fork
// today, and the item's Note says why (see the manifest header).
func TestManifestMatchesPhorgeFork(t *testing.T) {
	forkDir := requireForkDir(t)

	// Cache file contents so each PHP file is read once even though many fields
	// point at the same client.
	cache := newPHPFileCache(forkDir)

	verified := 0
	for _, domain := range contracts.Manifest() {
		verified += checkItems(t, cache, domain.Domain, "route", domain.Routes)
		verified += checkItems(t, cache, domain.Domain, "error code", domain.ErrorCodes)
		verified += checkItems(t, cache, domain.Domain, "wire field", domain.WireFields)
	}

	if verified == 0 {
		t.Fatalf("the drift check verified nothing; the manifest or its PHPFiles wiring is broken")
	}
	t.Logf("verified %d contract items against %s=%s", verified, phorgeForkEnv, forkDir)
}

// TestManifestMatchesPhorgeForkInReverse is the other direction. The forward
// check proves every registered name still exists in phorge-fork; it says
// nothing about a key phorge-fork uses that nobody ever registered, and that is
// drift too — a misspelled or renamed PHP-side key looks exactly like a key the
// manifest simply has not caught up with.
//
// So this reads the wire-boundary files contracts.ReverseScanFiles names,
// extracts every lowerCamelCase key sitting in one of the three wire-key
// positions, and fails on any that is neither registered anywhere in the
// manifest nor listed as a known non-contract key for that file. The scoping
// rules and the reason each exception is an exception live on that function.
func TestManifestMatchesPhorgeForkInReverse(t *testing.T) {
	forkDir := requireForkDir(t)
	cache := newPHPFileCache(forkDir)
	registered := manifestNames()

	scanned := 0
	for _, file := range contracts.ReverseScanFiles() {
		contents, err := cache.read(file.Path)
		if err != nil {
			t.Errorf(
				"the reverse drift check names phorge-fork file %q, but it could not be read: %v; "+
					"the PHP file was likely moved — update its path constant in go/internal/contracts/manifest.go",
				file.Path, err)
			continue
		}

		known := map[string]bool{}
		for _, key := range file.NonContractKeys {
			known[key] = true
		}

		for _, key := range phpWireKeys(contents) {
			scanned++
			if registered[key] || known[key] {
				continue
			}
			t.Errorf(
				"phorge-fork uses wire key %q in %s, but no manifest item registers it; "+
					"either the PHP side was renamed away from the Go contract (change it back, or change the Go side and the manifest together), "+
					"or this is a new shared name that belongs in go/internal/contracts/manifest.go. "+
					"If it is not a wire key at all, add it to that file's NonContractKeys with the reason (%s)",
				key, file.Path, file.Note)
		}
	}

	if scanned == 0 {
		t.Fatalf("the reverse drift check found no wire keys at all; its extraction or its file list is broken")
	}
	t.Logf("reverse-checked %d wire-key occurrences across %d phorge-fork files",
		scanned, len(contracts.ReverseScanFiles()))
}

// wireKeyPattern matches the three PHP positions that mean "this string is a
// wire key": reading one out of a decoded response (idx($node, 'refKey')),
// writing one into an array being sent or a schema being declared
// ('taskClass' => …), and subscripting either (($spec['offset'])). The key
// itself is restricted to a leading lower-case letter followed by letters and
// digits, which is the lowerCamelCase rule from contracts.ReverseScanFiles —
// no underscores, so INFORMATION_SCHEMA column names never reach the check.
var wireKeyPattern = regexp.MustCompile(
	`idx\(\s*\$[A-Za-z_][A-Za-z0-9_]*\s*,\s*'([a-z][A-Za-z0-9]*)'` +
		`|'([a-z][A-Za-z0-9]*)'\s*=>` +
		`|\$[A-Za-z_][A-Za-z0-9_]*\['([a-z][A-Za-z0-9]*)'\]`)

// phpWireKeys returns the distinct wire keys wireKeyPattern finds in one file.
func phpWireKeys(contents string) []string {
	seen := map[string]bool{}
	var keys []string
	for _, match := range wireKeyPattern.FindAllStringSubmatch(contents, -1) {
		// Exactly one of the three alternatives captured.
		for _, group := range match[1:] {
			if group == "" || seen[group] {
				continue
			}
			seen[group] = true
			keys = append(keys, group)
		}
	}
	return keys
}

// manifestNames returns every name the manifest registers, across all domains
// and all three kinds. The reverse check asks only whether a PHP-side key is
// known to the contract at all, not which domain claims it — see the limitation
// recorded on contracts.ReverseScanFiles.
func manifestNames() map[string]bool {
	names := map[string]bool{}
	for _, domain := range contracts.Manifest() {
		for _, items := range [][]contracts.ContractItem{domain.Routes, domain.ErrorCodes, domain.WireFields} {
			for _, item := range items {
				names[item.Name] = true
			}
		}
	}
	return names
}

// checkItems verifies one kind of item for one domain and returns how many were
// actually checked against phorge-fork (items with no PHPFiles are not).
func checkItems(t *testing.T, cache *phpFileCache, domain, kind string, items []contracts.ContractItem) int {
	t.Helper()
	checked := 0
	for _, item := range items {
		if len(item.PHPFiles) == 0 {
			continue
		}
		for _, rel := range item.PHPFiles {
			contents, err := cache.read(rel)
			if err != nil {
				t.Errorf(
					"%s %s %q in the manifest names phorge-fork file %q, but it could not be read: %v; "+
						"the PHP file was likely moved — update its path constant in go/internal/contracts/manifest.go",
					domain, kind, item.Name, rel, err)
				continue
			}
			if !phpHasLiteral(contents, item.Name) {
				t.Errorf(
					"%s %s %q is not present as a string literal in %s; "+
						"the Go side and phorge-fork have drifted — change that literal in %s to %q to match the Go contract%s",
					domain, kind, item.Name, rel, rel, item.Name, noteSuffix(item.Note))
				continue
			}
			checked++
		}
	}
	return checked
}

func noteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return " (" + note + ")"
}

// phpFileCache reads phorge-fork files at most once each.
type phpFileCache struct {
	root  string
	files map[string]string
	errs  map[string]error
}

func newPHPFileCache(root string) *phpFileCache {
	return &phpFileCache{root: root, files: map[string]string{}, errs: map[string]error{}}
}

func (c *phpFileCache) read(rel string) (string, error) {
	if contents, ok := c.files[rel]; ok {
		return contents, nil
	}
	if err, ok := c.errs[rel]; ok {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(c.root, rel))
	if err != nil {
		c.errs[rel] = err
		return "", err
	}
	c.files[rel] = string(raw)
	return c.files[rel], nil
}

// phpHasLiteral reports whether name appears as a single- or double-quoted PHP
// string literal. A raw substring match would accept name buried inside a
// longer identifier or comment word; requiring the quotes keeps it to actual
// literals — the route paths, field keys and constant values the contract is
// made of. Routes contain slashes and never collide, but field names like
// "data" or "name" are common English words, so the quotes matter.
func phpHasLiteral(contents, name string) bool {
	return strings.Contains(contents, "'"+name+"'") ||
		strings.Contains(contents, `"`+name+`"`)
}

// groupPrefixPattern extracts the router group prefix a domain mounts its
// routes under, e.g. app.Group("/api/mailer"). Routes are then registered as
// the sub-path after that prefix (g.Post("/send", …)), so the full path never
// appears verbatim in the source.
var groupPrefixPattern = regexp.MustCompile(`\.Group\("([^"]+)"\)`)

// goSourceHasRoute reports whether route is registered in the domain source.
// It finds the app.Group prefix, strips it from the route to get the sub-path
// as registered, and requires that sub-path to appear as a quoted literal —
// which is exactly how g.Get/g.Post spell it. This catches a renamed segment
// (the sub-path literal changes) without being fooled by the prefix/sub-path
// split or by multi-segment paths like /api/db/migrations/status.
func goSourceHasRoute(goSource, route string) bool {
	prefix := ""
	if m := groupPrefixPattern.FindStringSubmatch(goSource); m != nil {
		prefix = m[1]
	}
	sub := strings.TrimPrefix(route, prefix)
	if sub == "" {
		// The route is exactly the group prefix; require the prefix literal.
		return strings.Contains(goSource, `"`+route+`"`)
	}
	return strings.Contains(goSource, `"`+sub+`"`)
}

// collectJSONTags parses every .go file in the contracts package and returns
// the set of json tag names declared on struct fields. It is how the Go-side
// coverage test proves a manifest wire field is a real tag rather than a name
// someone typed into the manifest.
func collectJSONTags(t *testing.T) map[string]bool {
	t.Helper()
	tags := map[string]bool{}

	entries, err := os.ReadDir(contractsDir)
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".go" {
			continue
		}
		if strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(contractsDir, entry.Name())
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			field, ok := n.(*ast.Field)
			if !ok || field.Tag == nil {
				return true
			}
			// field.Tag.Value includes the surrounding backticks.
			raw := strings.Trim(field.Tag.Value, "`")
			jsonTag := reflect.StructTag(raw).Get("json")
			if jsonTag == "" {
				return true
			}
			name := strings.Split(jsonTag, ",")[0]
			if name != "" && name != "-" {
				tags[name] = true
			}
			return true
		})
	}

	if len(tags) == 0 {
		t.Fatalf("found no json tags in %s; the parser wiring is broken", contractsDir)
	}
	return tags
}

// readGoSourceConcatenated returns every non-test .go file in dir joined into
// one string, for substring scanning of route paths and error-code constants.
func readGoSourceConcatenated(t *testing.T, dir string) string {
	t.Helper()
	if dir == "" {
		t.Fatalf("no Go source directory registered for a manifest domain")
	}
	var builder strings.Builder
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		builder.Write(raw)
		builder.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatalf("read Go sources under %s: %v", dir, err)
	}
	return builder.String()
}
