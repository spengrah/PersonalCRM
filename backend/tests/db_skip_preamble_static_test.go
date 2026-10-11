// Package tests — DB-availability skip preamble AST guard.
//
// Every DB-backed test gets its availability check from
// testsupport.RequireDatabase(t), which skips on short mode or an unset
// DATABASE_URL and FAILS (not skips) when the database is unreachable. This
// guard flags the hand-rolled variants that made a broken database report as
// skipped-green:
//   - t.Skipf whose message is about a database connection;
//   - t.Skip inside an `if` whose condition is testing.Short();
//   - t.Skip inside an `if` whose condition checks DATABASE_URL or
//     TEST_DATABASE_URL (directly via os.Getenv, or via a variable assigned
//     from it in the same function).
//
// testsupport itself is excluded: it holds RequireDatabase and
// RequireLongTests, the two sanctioned owners of these conditions. Build-tag
// stub helpers call t.Skip unconditionally, outside an `if`, so they never
// match.
package tests

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// dbSkipAllowlist is keyed by "<path relative to backend>:<enclosing func>".
// It is empty on purpose: a legitimate exception needs a justification here.
var dbSkipAllowlist = map[string]string{}

var dbURLEnvVars = map[string]bool{"DATABASE_URL": true, "TEST_DATABASE_URL": true}

type dbSkipHit struct {
	file string
	line int
	fn   string
	why  string
}

func TestDBSkipPreamble_OnlyRequireDatabase(t *testing.T) {
	t.Parallel()
	moduleRoot, err := backendModuleRoot()
	if err != nil {
		t.Fatalf("locate backend module root: %v", err)
	}

	var hits []dbSkipHit
	fset := token.NewFileSet()
	walkErr := filepath.Walk(moduleRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			if info.Name() == "testsupport" || info.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(moduleRoot, path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		hits = append(hits, findDBSkipPreambles(filepath.ToSlash(rel), file, fset)...)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", moduleRoot, walkErr)
	}

	var violations []dbSkipHit
	for _, h := range hits {
		if _, ok := dbSkipAllowlist[h.file+":"+h.fn]; !ok {
			violations = append(violations, h)
		}
	}
	if len(violations) == 0 {
		return
	}
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].file != violations[j].file {
			return violations[i].file < violations[j].file
		}
		return violations[i].line < violations[j].line
	})
	var msg strings.Builder
	msg.WriteString("DB skip preamble guard: replace these with testsupport.RequireDatabase(t) (backend/tests/testsupport), which fails instead of skipping when the database is unreachable.\n")
	for _, v := range violations {
		msg.WriteString("  " + v.file + ":" + strconv.Itoa(v.line) + " (" + v.fn + "): " + v.why + "\n")
	}
	t.Error(msg.String())
}

func findDBSkipPreambles(rel string, file *ast.File, fset *token.FileSet) []dbSkipHit {
	var hits []dbSkipHit
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		urlVars := dbURLVars(fd)
		seen := map[token.Pos]bool{}
		add := func(pos token.Pos, why string) {
			if seen[pos] {
				return
			}
			seen[pos] = true
			hits = append(hits, dbSkipHit{file: rel, line: fset.Position(pos).Line, fn: fd.Name.Name, why: why})
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.IfStmt:
				if !bodySkips(n.Body) {
					return true
				}
				if mentionsShort(n.Cond) {
					add(n.Pos(), "t.Skip on testing.Short()")
				}
				if mentionsDBURL(n.Cond, urlVars) {
					add(n.Pos(), "t.Skip on unset DATABASE_URL")
				}
			case *ast.CallExpr:
				if name := skipCallName(n); name == "Skipf" && len(n.Args) > 0 && isConnectMessage(n.Args[0]) {
					add(n.Pos(), "t.Skipf on database connect failure")
				}
			}
			return true
		})
	}
	return hits
}

// skipCallName returns "Skip", "Skipf" or "SkipNow" for x.Skip*(...) calls.
func skipCallName(call *ast.CallExpr) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	switch sel.Sel.Name {
	case "Skip", "Skipf", "SkipNow":
		return sel.Sel.Name
	}
	return ""
}

func bodySkips(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && skipCallName(call) != "" {
			found = true
		}
		return !found
	})
	return found
}

func isConnectMessage(arg ast.Expr) bool {
	lit, ok := arg.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && strings.Contains(strings.ToLower(lit.Value), "connect")
}

func isDBURLGetenv(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Getenv" {
		return false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	name, err := strconv.Unquote(lit.Value)
	return err == nil && dbURLEnvVars[name]
}

// dbURLVars returns identifiers assigned from os.Getenv("DATABASE_URL") in fd.
func dbURLVars(fd *ast.FuncDecl) map[string]bool {
	vars := map[string]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, rhs := range as.Rhs {
			if id, ok := as.Lhs[i].(*ast.Ident); ok && isDBURLGetenv(rhs) {
				vars[id.Name] = true
			}
		}
		return true
	})
	return vars
}

func mentionsShort(cond ast.Expr) bool {
	found := false
	ast.Inspect(cond, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Short" {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "testing" {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

func mentionsDBURL(cond ast.Expr, urlVars map[string]bool) bool {
	found := false
	ast.Inspect(cond, func(n ast.Node) bool {
		if isDBURLGetenv(n) {
			found = true
		}
		if id, ok := n.(*ast.Ident); ok && urlVars[id.Name] {
			found = true
		}
		return !found
	})
	return found
}
