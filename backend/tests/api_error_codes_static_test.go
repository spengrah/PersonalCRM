// Package tests — API error-code literal AST guard.
//
// Every error code on the wire must be a named constant (api.ErrCode*), so
// the set of codes is greppable and cannot drift by typo. Typing SendError's
// parameter as `type ErrorCode string` would NOT enforce this: an untyped
// string literal converts implicitly to a named string type, so
// SendError(c, 400, "FOO", ...) still compiles. This test flags the literals
// instead.
//
// A "forged" code is a string literal (interpreted or raw), a concatenation, a
// call (fmt.Sprintf and the like), or an identifier naming a string constant
// declared in the same package directory whose name does not start with
// ErrCode. A selector (api.ErrCode*) and a variable or parameter are not
// forged: a pass-through wrapper stays legal.
//
// Flagged shapes (non-test .go files under backend/internal and backend/cmd):
//   - SendError / SendErrorWithDetails / abortAuth calls whose code argument
//     (third argument) is forged.
//   - gin.H / map composite literals with a "code" key, and
//     `m["code"] = ...` assignments, whose value is forged (hand-built error
//     bodies that bypass SendError).
//   - APIError struct literals whose Code field is forged.
//
// allowedCodeLiteralSites is keyed by "path:line" relative to the backend
// module root and must stay empty; new entries need a written reason.
package tests

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var allowedCodeLiteralSites = map[string]string{}

// errorCodeCallees are functions whose third argument is the wire error code.
var errorCodeCallees = map[string]struct{}{
	"SendError":            {},
	"SendErrorWithDetails": {},
	"abortAuth":            {},
}

func isStringLit(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING
}

func isCodeKey(e ast.Expr) bool {
	if !isStringLit(e) {
		return false
	}
	s, err := strconv.Unquote(e.(*ast.BasicLit).Value)
	return err == nil && s == "code"
}

func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// stringConstNames returns the names of every const in file whose value is a
// string literal, at any scope.
func stringConstNames(file *ast.File, into map[string]struct{}) {
	ast.Inspect(file, func(n ast.Node) bool {
		gd, ok := n.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			return true
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if i < len(vs.Values) && isStringLit(vs.Values[i]) {
					into[name.Name] = struct{}{}
				}
			}
		}
		return true
	})
}

// isForgedCode reports whether e is a wire error code that is not a named
// api.ErrCode* constant. consts holds the string constants of e's package.
func isForgedCode(e ast.Expr, consts map[string]struct{}) bool {
	switch x := e.(type) {
	case *ast.BasicLit:
		return x.Kind == token.STRING
	case *ast.BinaryExpr, *ast.CallExpr:
		return true
	case *ast.ParenExpr:
		return isForgedCode(x.X, consts)
	case *ast.Ident:
		_, isConst := consts[x.Name]
		return isConst && !strings.HasPrefix(x.Name, "ErrCode")
	}
	return false
}

func isAPIErrorType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == "APIError"
	case *ast.SelectorExpr:
		return t.Sel.Name == "APIError"
	}
	return false
}

func TestAPIErrorCodesAreNotStringLiterals(t *testing.T) {
	fset := token.NewFileSet()
	type parsed struct {
		rel  string
		dir  string
		file *ast.File
	}
	var files []parsed
	constsByDir := map[string]map[string]struct{}{}
	for _, root := range []string{"../internal", "../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			dir := filepath.Dir(path)
			if constsByDir[dir] == nil {
				constsByDir[dir] = map[string]struct{}{}
			}
			stringConstNames(file, constsByDir[dir])
			files = append(files, parsed{strings.TrimPrefix(filepath.ToSlash(path), "../"), dir, file})
			return nil
		})
		require.NoError(t, err)
	}

	var hits []string
	for _, pf := range files {
		consts := constsByDir[pf.dir]
		record := func(pos token.Pos, what string) {
			key := pf.rel + ":" + strconv.Itoa(fset.Position(pos).Line)
			if _, ok := allowedCodeLiteralSites[key]; !ok {
				hits = append(hits, key+" "+what)
			}
		}
		ast.Inspect(pf.file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				name := calleeName(x.Fun)
				if _, ok := errorCodeCallees[name]; ok && len(x.Args) > 2 && isForgedCode(x.Args[2], consts) {
					record(x.Args[2].Pos(), name+" with forged code")
				}
			case *ast.CompositeLit:
				for _, el := range x.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if isCodeKey(kv.Key) && isForgedCode(kv.Value, consts) {
						record(kv.Value.Pos(), `"code" key with forged value`)
					}
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Code" && x.Type != nil && isAPIErrorType(x.Type) && isForgedCode(kv.Value, consts) {
						record(kv.Value.Pos(), "APIError.Code with forged value")
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range x.Lhs {
					ix, ok := lhs.(*ast.IndexExpr)
					if ok && isCodeKey(ix.Index) && i < len(x.Rhs) && isForgedCode(x.Rhs[i], consts) {
						record(x.Rhs[i].Pos(), `["code"] assignment with forged value`)
					}
				}
			}
			return true
		})
	}
	sort.Strings(hits)
	require.Empty(t, hits, "error codes must be api.ErrCode* constants, not string literals:\n%s", strings.Join(hits, "\n"))
}
