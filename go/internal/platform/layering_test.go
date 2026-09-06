// Package platform has no code of its own; this file guards the one rule that
// makes its subpackages reusable.
package platform

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// forbiddenPrefixes are the domain packages platform must never import. The
// dependency has to point one way only: domains build on platform, so that a
// platform subpackage can be lifted into its own module the day the services
// are split out of this repository.
var forbiddenPrefixes = []string{
	"github.com/soulteary/gorge/go/internal/render",
	"github.com/soulteary/gorge/go/internal/diff",
	"github.com/soulteary/gorge/go/internal/contracts",
}

func TestPlatformDoesNotImportDomainPackages(t *testing.T) {
	fset := token.NewFileSet()

	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			for _, forbidden := range forbiddenPrefixes {
				if imported == forbidden || strings.HasPrefix(imported, forbidden+"/") {
					t.Errorf("%s imports %s: platform must not depend on a domain package", path, imported)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
