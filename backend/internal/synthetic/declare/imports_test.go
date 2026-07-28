package declare

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const cadencePkg = "personal-crm/backend/internal/cadence"

// scanForbiddenImports walks root RECURSIVELY and reports every non-test Go
// file that imports forbidden. The recursion is the point: banning the import
// in the root package alone would leave the subpackage indirection open (root
// imports declare/helper, helper imports cadence), which is exactly the bypass
// the independence rule has to close. Returns "<relpath>: <import>" strings.
//
// `testdata` subtrees are skipped: the go tool never builds them, so they are
// not part of the package's compiled code — and this very guard's own negative
// fixture lives in one. Passing a testdata directory AS the root still scans it
// (the skip is on descendants, not the root itself).
func scanForbiddenImports(root, forbidden string) ([]string, error) {
	var hits []string
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		for _, imp := range f.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				return uerr
			}
			if p == forbidden {
				rel, rerr := filepath.Rel(root, path)
				if rerr != nil {
					rel = path
				}
				hits = append(hits, rel+": "+p)
			}
		}
		return nil
	})
	sort.Strings(hits)
	return hits, err
}

// TestDeclareDoesNotImportCadence is the independence-rule guard. Non-test code
// in this tree must never import internal/cadence: fixture math is stated
// locally (facts.go) so an app-side cadence regression makes fixtures FAIL
// rather than track the regression.
//
// Scope note, deliberate: this covers declare's OWN tree only. declare's
// RUNTIME path reaches cadence through replay → service → cadence — that is the
// app writing through its real services (required by the last_contacted honesty
// rule) and is not fixture math; banning it transitively would ban the harness
// itself.
func TestDeclareDoesNotImportCadence(t *testing.T) {
	hits, err := scanForbiddenImports(".", cadencePkg)
	require.NoError(t, err)
	assert.Empty(t, hits, "non-test files in internal/synthetic/declare must not import %s", cadencePkg)
}

// The guard is only worth having if it FAILS on the bypass it exists to close.
// The fixture tree under testdata/ contains exactly that shape: a root file
// importing a helper subpackage, and the helper importing cadence. testdata is
// invisible to the go tool, so the fixture never builds — the scanner parses it
// as source, which is all it needs.
func TestScanForbiddenImportsCatchesSubpackageIndirection(t *testing.T) {
	hits, err := scanForbiddenImports(filepath.Join("testdata", "bypass"), cadencePkg)
	require.NoError(t, err)
	require.Len(t, hits, 1, "expected the helper subpackage's cadence import to be reported")
	assert.Equal(t, filepath.Join("helper", "helper.go")+": "+cadencePkg, hits[0])
}

// ...and passes on a clean tree, so a green result means "no import", not
// "scanner walked nothing".
func TestScanForbiddenImportsPassesOnCleanTree(t *testing.T) {
	hits, err := scanForbiddenImports(filepath.Join("testdata", "clean"), cadencePkg)
	require.NoError(t, err)
	assert.Empty(t, hits)

	// Same tree, different forbidden import: proves the walk actually reached
	// the files rather than silently skipping the directory.
	found, err := scanForbiddenImports(filepath.Join("testdata", "clean"), "fmt")
	require.NoError(t, err)
	assert.NotEmpty(t, found, "the scanner must have parsed the clean fixture's files")
}

// _test.go files are exempt: facts_test.go imports cadence on purpose (the
// tripwire), and that must not trip the guard.
func TestScanForbiddenImportsSkipsTestFiles(t *testing.T) {
	hits, err := scanForbiddenImports(".", cadencePkg)
	require.NoError(t, err)
	assert.Empty(t, hits)

	// Sanity: the tripwire file really does import cadence, so the exemption is
	// load-bearing rather than vacuous.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "facts_test.go", nil, parser.ImportsOnly)
	require.NoError(t, err)
	var imports []string
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		imports = append(imports, p)
	}
	assert.Contains(t, imports, cadencePkg)
}
