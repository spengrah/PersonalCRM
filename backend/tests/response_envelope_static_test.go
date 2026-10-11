// Package tests — response envelope guard.
//
// Structural rule over non-test Go files under internal/api:
//
//   - A1: a handler must not hand-build the success envelope. A gin.H or
//     map[string]T literal with a "success" key other than literal false, a
//     `m["success"] = v` assignment with v other than false, or an
//     api.APIResponse literal with Success other than false, bypasses
//     api.SendSuccess. The check covers every such literal, not only call
//     arguments, so a body held in a variable or built by a helper is caught.
//     `"success": false` error envelopes are not flagged; internal/api/response.go
//     is exempt because it defines SendSuccess.
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

type envelopeHit struct {
	file string
	line int
	fn   string
	what string
}

func (h envelopeHit) String() string {
	return h.file + ":" + strconv.Itoa(h.line) + " in " + h.fn + " — " + h.what
}

func isGinH(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == "gin" && sel.Sel.Name == "H"
}

// isStringMapLit reports whether lit is a gin.H or map[string]T literal.
func isStringMapLit(lit *ast.CompositeLit) bool {
	if isGinH(lit.Type) {
		return true
	}
	mt, ok := lit.Type.(*ast.MapType)
	if !ok {
		return false
	}
	k, ok := mt.Key.(*ast.Ident)
	return ok && k.Name == "string"
}

// literalKey returns the value expression for a string key of a map literal.
func literalKey(lit *ast.CompositeLit, key string) (ast.Expr, bool) {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if bl, ok := kv.Key.(*ast.BasicLit); ok && bl.Kind == token.STRING {
			if s, err := strconv.Unquote(bl.Value); err == nil && s == key {
				return kv.Value, true
			}
		}
	}
	return nil, false
}

func isFalseIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "false"
}

// isAPIResponseType reports whether e is APIResponse or pkg.APIResponse.
func isAPIResponseType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name == "APIResponse"
	case *ast.SelectorExpr:
		return t.Sel.Name == "APIResponse"
	}
	return false
}

// apiResponseFile is the one file that builds the envelope: SendSuccess and
// SendError construct api.APIResponse there.
const apiResponseFile = "internal/api/response.go"

// findHandRolledSuccess implements A1 for one file. It inspects every
// literal and assignment, not only call arguments, so a body built in a
// variable or a helper, or sent through any gin render method, is caught.
func findHandRolledSuccess(rel string, file *ast.File, fset *token.FileSet) []envelopeHit {
	var hits []envelopeHit
	forEachDecl(file, func(fnName string, decl ast.Node) {
		record := func(pos token.Pos, what string) {
			hits = append(hits, envelopeHit{rel, fset.Position(pos).Line, fnName, what})
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				if x.Type == nil {
					return true
				}
				if isStringMapLit(x) {
					if v, has := literalKey(x, "success"); has && !isFalseIdent(v) {
						record(x.Pos(), "hand-built success envelope (map literal with a \"success\" key)")
					}
				}
				if isAPIResponseType(x.Type) && rel != apiResponseFile {
					for _, el := range x.Elts {
						kv, ok := el.(*ast.KeyValueExpr)
						if id, isID := kv.Key.(*ast.Ident); ok && isID && id.Name == "Success" && !isFalseIdent(kv.Value) {
							record(x.Pos(), "hand-built success envelope (APIResponse literal)")
						}
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range x.Lhs {
					ix, ok := lhs.(*ast.IndexExpr)
					if !ok || i >= len(x.Rhs) {
						continue
					}
					if bl, ok := ix.Index.(*ast.BasicLit); ok && bl.Kind == token.STRING {
						if key, err := strconv.Unquote(bl.Value); err == nil && key == "success" && !isFalseIdent(x.Rhs[i]) {
							record(x.Pos(), "hand-built success envelope (\"success\" key assignment)")
						}
					}
				}
			}
			return true
		})
	})
	return hits
}

// forEachDecl calls fn for every top-level func (named by the func) and every
// package-level GenDecl (named "<package-level>").
func forEachDecl(file *ast.File, fn func(name string, decl ast.Node)) {
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				fn(d.Name.Name, d)
			}
		case *ast.GenDecl:
			fn("<package-level>", d)
		}
	}
}

// scanAPIFiles parses every non-test Go file under dir (relative to the
// backend module root) and returns the hits finder reports, sorted by position.
func scanAPIFiles(t *testing.T, dir string, finder func(rel string, f *ast.File, fset *token.FileSet) []envelopeHit) []envelopeHit {
	t.Helper()
	moduleRoot, err := backendModuleRoot()
	if err != nil {
		t.Fatalf("locate backend module root: %v", err)
	}
	var hits []envelopeHit
	fset := token.NewFileSet()
	root := filepath.Join(moduleRoot, filepath.FromSlash(dir))
	err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
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
		hits = append(hits, finder(filepath.ToSlash(rel), file, fset)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].file != hits[j].file {
			return hits[i].file < hits[j].file
		}
		return hits[i].line < hits[j].line
	})
	return hits
}

func TestAPI_NoHandRolledSuccessEnvelope(t *testing.T) {
	t.Parallel()
	hits := scanAPIFiles(t, "internal/api", findHandRolledSuccess)
	if len(hits) == 0 {
		return
	}
	var msg strings.Builder
	msg.WriteString("hand-built success envelope: use api.SendSuccess with a typed payload.\n")
	for _, h := range hits {
		msg.WriteString("  " + h.String() + "\n")
	}
	t.Fatal(msg.String())
}
