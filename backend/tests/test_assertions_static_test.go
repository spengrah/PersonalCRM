// Package tests — test-assertion guard (type-checked AST).
//
// Two rules over every _test.go file in the module:
//
//   - T6 (nonfatal-guard): a non-fatal assert.NotNil / NotEmpty / Len /
//     NoError used as a guard, where one of the next two statements indexes,
//     dereferences, or selects through the guarded value. When the guard
//     fails, the test keeps running and panics on the next line, burying the
//     real failure. Use require.* so the test stops at the guard.
//     For NoError(t, err), the guarded values are the other variables
//     assigned by the immediately preceding `x, err := f()` statement.
//     Map index is not flagged (a nil map lookup does not panic), and an
//     `if assert.X(t, v) {` condition is not flagged (it is not a statement).
//
//   - T8 (discarded-error): `a, _ := f(...)` where f belongs to this module,
//     f's last result is error, and a is used later. A failed call leaves a
//     zero value that later assertions pass or fail on vacuously. Calls
//     inside a t.Cleanup func literal are exempt: teardown errors have no
//     test to fail.
//
// Test files carry `//go:build integration_testdb` (or its negation), so the
// module is type-checked twice, once per tag state, and findings are merged.
//
// Design: same as sole_writer_static_test.go. Hits are compared with a
// per-(file, function, rule) allowlist. An allowlist entry needs a reason.
package tests

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

const (
	testifyAssertPath = "github.com/stretchr/testify/assert"
	backendModulePath = "personal-crm/backend"
)

// assertionGuardAllowlist maps "relative/path_test.go:FuncName:RULE" to the
// reason the hit is legitimate. Keep empty unless a site cannot use require
// or must discard the error.
var assertionGuardAllowlist = map[string]string{}

// nonfatalGuards are the assert functions that read as "stop here if this
// is wrong" but do not stop the test.
var nonfatalGuards = map[string]bool{"NotNil": true, "NotEmpty": true, "Len": true, "NoError": true}

func TestTestAssertions_NoNonfatalGuardsOrDiscardedErrors(t *testing.T) {
	root, err := filepath.Abs("..")
	require.NoError(t, err)

	hits := map[string]string{} // "rel|line|FuncName|RULE" -> message
	for _, tags := range []string{"integration_testdb", ""} {
		for _, pkg := range loadModuleWithTests(t, root, tags) {
			for _, file := range pkg.Syntax {
				name := pkg.Fset.Position(file.Pos()).Filename
				if !strings.HasSuffix(name, "_test.go") {
					continue
				}
				rel, err := filepath.Rel(root, name)
				require.NoError(t, err)
				scanTestFile(pkg, file, filepath.ToSlash(rel), hits)
			}
		}
	}

	var failures []string
	for key, msg := range hits {
		parts := strings.Split(key, "|") // rel|line|func|rule
		if _, ok := assertionGuardAllowlist[parts[0]+":"+parts[2]+":"+parts[3]]; ok {
			continue
		}
		failures = append(failures, msg)
	}
	sort.Strings(failures)
	require.Empty(t, failures, "test assertion guard violations (%d). Use require.* for guards that precede a dereference, "+
		"and check the error from module calls instead of `_`. Cleanup funcs are exempt from T8.\n%s",
		len(failures), strings.Join(failures, "\n"))
}

func loadModuleWithTests(t *testing.T, root, tags string) []*packages.Package {
	t.Helper()
	cfg := &packages.Config{
		Mode:  packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports,
		Dir:   root,
		Tests: true,
	}
	if tags != "" {
		cfg.BuildFlags = []string{"-tags=" + tags}
	}
	pkgs, err := packages.Load(cfg, "./...")
	require.NoError(t, err)
	var local []*packages.Package
	for _, p := range pkgs {
		require.Empty(t, p.Errors, "errors loading %s (tags=%q)", p.ID, tags)
		if strings.HasPrefix(p.PkgPath, backendModulePath) {
			local = append(local, p)
		}
	}
	return local
}

func scanTestFile(pkg *packages.Package, file *ast.File, rel string, hits map[string]string) {
	info := pkg.TypesInfo
	uses := map[types.Object][]token.Pos{}
	for id, obj := range info.Uses {
		uses[obj] = append(uses[obj], id.Pos())
	}
	var cleanups []*ast.FuncLit
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Cleanup" {
			if lit, ok := call.Args[0].(*ast.FuncLit); ok {
				cleanups = append(cleanups, lit)
			}
		}
		return true
	})
	inCleanup := func(p token.Pos) bool {
		for _, lit := range cleanups {
			if lit.Pos() <= p && p < lit.End() {
				return true
			}
		}
		return false
	}

	for _, decl := range file.Decls {
		fn := "<package>"
		if fd, ok := decl.(*ast.FuncDecl); ok {
			fn = fd.Name.Name
		}
		report := func(rule string, pos token.Pos, msg string) {
			line := pkg.Fset.Position(pos).Line
			hits[fmt.Sprintf("%s|%d|%s|%s", rel, line, fn, rule)] = fmt.Sprintf("%s:%d: [%s] %s (in %s)", rel, line, rule, msg, fn)
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BlockStmt:
				checkGuards(info, n.List, report)
			case *ast.CaseClause:
				checkGuards(info, n.Body, report)
			case *ast.CommClause:
				checkGuards(info, n.Body, report)
			case *ast.AssignStmt:
				if !inCleanup(n.Pos()) {
					checkDiscardedError(info, uses, n, report)
				}
			}
			return true
		})
	}
}

func checkGuards(info *types.Info, list []ast.Stmt, report func(rule string, pos token.Pos, msg string)) {
	for i, stmt := range list {
		es, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := es.X.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			continue
		}
		name, ok := testifyAssertName(info, call)
		if !ok || !nonfatalGuards[name] {
			continue
		}
		var subjects []ast.Expr
		if name == "NoError" {
			subjects = valuesAssignedWithError(list, i, call.Args[1])
		} else {
			subjects = []ast.Expr{call.Args[1]}
		}
		for _, subj := range subjects {
			for _, next := range list[i+1 : min(i+3, len(list))] {
				if derefs(info, next, subj) {
					report("T6", call.Pos(), fmt.Sprintf("assert.%s guards %s but the next statements dereference it; use require.%s", name, types.ExprString(subj), name))
					break
				}
			}
		}
	}
}

// valuesAssignedWithError returns the non-blank, non-error values assigned by
// the statement before list[i] when its last assignee is errArg.
func valuesAssignedWithError(list []ast.Stmt, i int, errArg ast.Expr) []ast.Expr {
	if i == 0 {
		return nil
	}
	as, ok := list[i-1].(*ast.AssignStmt)
	errID, isID := errArg.(*ast.Ident)
	if !ok || !isID || len(as.Lhs) < 2 || len(as.Rhs) != 1 {
		return nil
	}
	if last, ok := as.Lhs[len(as.Lhs)-1].(*ast.Ident); !ok || last.Name != errID.Name {
		return nil
	}
	var out []ast.Expr
	for _, lhs := range as.Lhs[:len(as.Lhs)-1] {
		if id, ok := lhs.(*ast.Ident); ok && id.Name != "_" {
			out = append(out, id)
		}
	}
	return out
}

// derefs reports whether stmt indexes, selects through, or dereferences
// subj in a way that panics when subj is nil or empty.
func derefs(info *types.Info, stmt ast.Stmt, subj ast.Expr) bool {
	want := types.ExprString(subj)
	t := info.TypeOf(subj)
	if t == nil {
		return false
	}
	u := t.Underlying()
	_, isPtr := u.(*types.Pointer)
	_, isIface := u.(*types.Interface)
	_, isSlice := u.(*types.Slice)
	isString := types.Identical(u, types.Typ[types.String])
	// `if assert.X(t, subj) {` or `if subj != nil {` guards its own body.
	if ifs, ok := stmt.(*ast.IfStmt); ok && mentions(ifs.Cond, want) {
		return false
	}
	found := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			found = found || (types.ExprString(n.X) == want && (isPtr || isIface))
		case *ast.StarExpr:
			found = found || (types.ExprString(n.X) == want && isPtr)
		case *ast.IndexExpr:
			found = found || (types.ExprString(n.X) == want && (isSlice || isString || isPtr))
		}
		return !found
	})
	return found
}

// mentions reports whether root passes the expression want whole to a call
// or compares it, as in `assert.NotNil(t, want)` or `want != nil`.
func mentions(root ast.Node, want string) bool {
	found := false
	ast.Inspect(root, func(n ast.Node) bool {
		var operands []ast.Expr
		switch n := n.(type) {
		case *ast.CallExpr:
			operands = n.Args
		case *ast.BinaryExpr:
			operands = []ast.Expr{n.X, n.Y}
		}
		for _, e := range operands {
			found = found || types.ExprString(e) == want
		}
		return !found
	})
	return found
}

func testifyAssertName(info *types.Info, call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	pn, ok := info.Uses[id].(*types.PkgName)
	if !ok || pn.Imported().Path() != testifyAssertPath {
		return "", false
	}
	return sel.Sel.Name, true
}

func checkDiscardedError(info *types.Info, uses map[types.Object][]token.Pos, as *ast.AssignStmt, report func(rule string, pos token.Pos, msg string)) {
	if len(as.Lhs) < 2 || len(as.Rhs) != 1 {
		return
	}
	if last, ok := as.Lhs[len(as.Lhs)-1].(*ast.Ident); !ok || last.Name != "_" {
		return
	}
	call, ok := as.Rhs[0].(*ast.CallExpr)
	if !ok {
		return
	}
	tuple, ok := info.TypeOf(call).(*types.Tuple)
	if !ok || tuple.Len() != len(as.Lhs) || !types.Identical(tuple.At(tuple.Len()-1).Type(), types.Universe.Lookup("error").Type()) {
		return
	}
	var callee *ast.Ident
	switch f := call.Fun.(type) {
	case *ast.Ident:
		callee = f
	case *ast.SelectorExpr:
		callee = f.Sel
	default:
		return
	}
	obj := info.Uses[callee]
	if obj == nil || obj.Pkg() == nil || !strings.HasPrefix(obj.Pkg().Path(), backendModulePath) {
		return
	}
	for _, lhs := range as.Lhs[:len(as.Lhs)-1] {
		id, ok := lhs.(*ast.Ident)
		if !ok || id.Name == "_" {
			continue
		}
		v := info.Defs[id]
		if v == nil {
			v = info.Uses[id]
		}
		for _, p := range uses[v] {
			if p > as.End() {
				report("T8", as.Pos(), fmt.Sprintf("error from %s discarded with _ while %s is used later; check it with require.NoError", types.ExprString(call.Fun), id.Name))
				return
			}
		}
	}
}
