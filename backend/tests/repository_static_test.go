// Package tests — repository-layer structural checks.
//
// Two defects are structurally banned in non-test files of
// internal/repository:
//
//   - A nil-tx fallback: any nil comparison of a pgx.Tx parameter other than
//     `if tx == nil { return ..., err }`. A nil tx must be an error, never a
//     silent switch to the pool (the query then runs outside the caller's
//     transaction).
//   - A `.(time.Time)` or `.(*time.Time)` assertion, or a type-switch case on
//     either. sqlc types an untyped MAX() aggregate as interface{}; the
//     comma-ok and type-switch forms turn any type mismatch into "no value",
//     and the plain form panics on NULL. Callers use scanNullableTime, which
//     errors on a mismatch.
package tests

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// nullableTimeHelperFile is the only repository file allowed a comma-ok
// time.Time assertion: it holds scanNullableTime(v any) (*time.Time, error).
const nullableTimeHelperFile = "internal/repository/scan_nullable_time.go"

// Allowlists hold "internal/repository/file.go:line" keys. Both stay empty:
// neither rule has a legitimate exception.
var (
	repositoryNilTxAllowlist       = map[string]bool{}
	repositoryCommaOkTimeAllowlist = map[string]bool{}
)

func TestRepository_NoNilTxFallbackToPool(t *testing.T) {
	hits := scanRepository(t, func(rel string, fset *token.FileSet, f *ast.File) []string {
		var out []string
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			txParams := pgxTxParams(fn)
			if len(txParams) == 0 {
				continue
			}
			// Conditions of the one allowed shape; every other nil comparison of a
			// tx parameter is a hit.
			allowed := map[ast.Expr]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.IfStmt:
					if isNilTxGuard(x, fn, txParams) {
						allowed[x.Cond] = true
					}
				case *ast.BinaryExpr:
					if allowed[x] || (x.Op != token.EQL && x.Op != token.NEQ) {
						return true
					}
					if isTxNilComparison(x, txParams) {
						out = append(out, fmt.Sprintf("%s:%d", rel, fset.Position(x.Pos()).Line))
					}
				}
				return true
			})
		}
		return out
	}, repositoryNilTxAllowlist)
	if len(hits) > 0 {
		t.Fatalf("nil-tx comparison: a nil pgx.Tx must be an error, never a fallback to the pool (the query then runs outside the caller's transaction).\n"+
			"The only allowed form is `if tx == nil { return ..., errNilTx }` in a function returning error, where the returned error is an identifier or an fmt.Errorf / errors.New call.\n%s", strings.Join(hits, "\n"))
	}
}

func TestRepository_NoCommaOkTimeAssertion(t *testing.T) {
	hits := scanRepository(t, func(rel string, fset *token.FileSet, f *ast.File) []string {
		if rel == nullableTimeHelperFile {
			return nil
		}
		var out []string
		record := func(pos token.Pos) {
			out = append(out, fmt.Sprintf("%s:%d", rel, fset.Position(pos).Line))
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.TypeAssertExpr:
				// Type is nil for the `.(type)` of a type switch.
				if x.Type != nil && isTimeType(x.Type) {
					record(x.Pos())
				}
			case *ast.TypeSwitchStmt:
				for _, stmt := range x.Body.List {
					cc, ok := stmt.(*ast.CaseClause)
					if !ok {
						continue
					}
					for _, e := range cc.List {
						if isTimeType(e) {
							record(e.Pos())
						}
					}
				}
			}
			return true
		})
		return out
	}, repositoryCommaOkTimeAllowlist)
	if len(hits) > 0 {
		t.Fatalf("`time.Time` / `*time.Time` type assertion or type-switch case: the comma-ok and type-switch forms turn a type mismatch into \"no value\", and the plain form panics on a NULL aggregate.\n"+
			"Use scanNullableTime (%s), which returns an error on a mismatch.\n%s", nullableTimeHelperFile, strings.Join(hits, "\n"))
	}
}

// isTimeType reports whether e is time.Time or *time.Time.
func isTimeType(e ast.Expr) bool {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Time" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "time"
}

// scanRepository applies check to every non-test .go file in
// backend/internal/repository and returns the sorted hits not in allow.
func scanRepository(t *testing.T, check func(rel string, fset *token.FileSet, f *ast.File) []string, allow map[string]bool) []string {
	t.Helper()
	moduleRoot, err := backendModuleRoot()
	if err != nil {
		t.Fatalf("locate backend module root: %v", err)
	}
	dir := filepath.Join(moduleRoot, "internal", "repository")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var hits []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, h := range check("internal/repository/"+name, fset, f) {
			if !allow[h] {
				hits = append(hits, h)
			}
		}
	}
	sort.Strings(hits)
	return hits
}

// pgxTxParams returns the names of fn's parameters typed pgx.Tx.
func pgxTxParams(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	for _, field := range fn.Type.Params.List {
		sel, ok := field.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Tx" {
			continue
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "pgx" {
			for _, name := range field.Names {
				out[name.Name] = true
			}
		}
	}
	return out
}

func isNilIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

// isTxNilComparison reports whether b compares a tx parameter to nil, in
// either operand order.
func isTxNilComparison(b *ast.BinaryExpr, txParams map[string]bool) bool {
	isTx := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && txParams[id.Name]
	}
	return (isTx(b.X) && isNilIdent(b.Y)) || (isNilIdent(b.X) && isTx(b.Y))
}

// isNilTxGuard reports whether ifs is `if tx == nil { return ..., err }` with
// no else branch, in a function whose last result is error and where err is a
// non-nil identifier or an fmt.* / errors.* call.
func isNilTxGuard(ifs *ast.IfStmt, fn *ast.FuncDecl, txParams map[string]bool) bool {
	cond, ok := ifs.Cond.(*ast.BinaryExpr)
	if !ok || cond.Op != token.EQL || !isTxNilComparison(cond, txParams) || ifs.Else != nil {
		return false
	}
	if fn.Type.Results == nil || len(fn.Type.Results.List) == 0 {
		return false
	}
	last, ok := fn.Type.Results.List[len(fn.Type.Results.List)-1].Type.(*ast.Ident)
	if !ok || last.Name != "error" || len(ifs.Body.List) != 1 {
		return false
	}
	ret, ok := ifs.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) == 0 {
		return false
	}
	switch e := ret.Results[len(ret.Results)-1].(type) {
	case *ast.Ident:
		return e.Name != "nil"
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && (pkg.Name == "fmt" || pkg.Name == "errors")
	}
	return false
}
