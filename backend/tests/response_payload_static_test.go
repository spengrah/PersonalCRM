// Package tests — success payload type guard.
//
// A success payload in internal/api/handlers must be a typed DTO. A gin.H or
// map[string]string/any literal that is not a trivial ack (at most one key,
// constant value), a []gin.H type, or a func returning gin.H is a violation
// unless the (file, function) is in untypedPayloadAllowlist. The rule is
// forward-only: the allowlist only shrinks, because an entry with no remaining
// hit fails the test. Literals nested in a "success": false envelope are
// error details and are not flagged.
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

// untypedPayloadAllowlist is the backlog: each entry needs a typed DTO.
// Delete the entry when the function is fixed.
var untypedPayloadAllowlist = map[string]struct{}{
	"internal/api/handlers/mac_host.go:KnownIDs":          {},
	"internal/api/handlers/meeting_note.go:ResolveLink":   {},
	"internal/api/handlers/sync.go:TriggerSync":           {},
	"internal/api/handlers/system.go:SetTimeAcceleration": {},
	"internal/api/handlers/system.go:ExportData":          {},
	"internal/api/handlers/system.go:ImportData":          {},
	"internal/api/handlers/telegram.go:StartAuth":         {},
	"internal/api/handlers/telegram.go:VerifyCode":        {},
	"internal/api/handlers/telegram.go:VerifyPassword":    {},
	"internal/api/handlers/telegram.go:GetStatus":         {},
	"internal/api/handlers/telegram.go:ListChats":         {},
	"internal/api/handlers/telegram.go:chatToResponse":    {},
}

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

// isUntypedMapLit reports whether lit is a gin.H or map[string]string/any literal.
func isUntypedMapLit(lit *ast.CompositeLit) bool {
	if isGinH(lit.Type) {
		return true
	}
	mt, ok := lit.Type.(*ast.MapType)
	if !ok {
		return false
	}
	k, ok := mt.Key.(*ast.Ident)
	if !ok || k.Name != "string" {
		return false
	}
	switch v := mt.Value.(type) {
	case *ast.Ident:
		return v.Name == "string" || v.Name == "any"
	case *ast.InterfaceType:
		return len(v.Methods.List) == 0
	}
	return false
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

// isErrorEnvelope reports whether lit has `"success": false`.
func isErrorEnvelope(lit *ast.CompositeLit) bool {
	v, ok := literalKey(lit, "success")
	return ok && isFalseIdent(v)
}

// isTrivialAck reports whether lit has at most one key and every value is a
// constant (string/number literal or true/false), e.g. {"ok": true}.
func isTrivialAck(lit *ast.CompositeLit) bool {
	if len(lit.Elts) > 1 {
		return false
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			return false
		}
		switch v := kv.Value.(type) {
		case *ast.BasicLit:
		case *ast.Ident:
			if v.Name != "true" && v.Name != "false" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// findUntypedPayloads implements A2 for one file.
func findUntypedPayloads(rel string, file *ast.File, fset *token.FileSet) []envelopeHit {
	var hits []envelopeHit
	forEachDecl(file, func(fnName string, decl ast.Node) {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Type.Results != nil {
			for _, r := range fd.Type.Results.List {
				if isGinH(r.Type) {
					hits = append(hits, envelopeHit{rel, fset.Position(r.Pos()).Line, fnName, "returns gin.H"})
				}
			}
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				if isErrorEnvelope(x) {
					return false
				}
				if isUntypedMapLit(x) && !isTrivialAck(x) {
					hits = append(hits, envelopeHit{rel, fset.Position(x.Pos()).Line, fnName, "untyped map literal"})
				}
			case *ast.ArrayType:
				if isGinH(x.Elt) {
					hits = append(hits, envelopeHit{rel, fset.Position(x.Pos()).Line, fnName, "[]gin.H"})
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

func TestAPI_NoUntypedSuccessPayloads(t *testing.T) {
	t.Parallel()
	hits := scanAPIFiles(t, "internal/api/handlers", findUntypedPayloads)
	seen := map[string]struct{}{}
	var msg strings.Builder
	for _, h := range hits {
		key := h.file + ":" + h.fn
		seen[key] = struct{}{}
		if _, ok := untypedPayloadAllowlist[key]; !ok {
			msg.WriteString("  " + h.String() + "\n")
		}
	}
	if msg.Len() > 0 {
		t.Errorf("untyped payload (gin.H / map literal) outside untypedPayloadAllowlist: define a typed DTO.\n%s", msg.String())
	}
	var stale []string
	for key := range untypedPayloadAllowlist {
		if _, ok := seen[key]; !ok {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("untypedPayloadAllowlist entries with no remaining hit (delete them):\n  %s", strings.Join(stale, "\n  "))
	}
}
