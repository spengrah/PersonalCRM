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
	"go/token"
	"sort"
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
	"internal/api/handlers/telegram.go:StartAuth":         {},
	"internal/api/handlers/telegram.go:VerifyCode":        {},
	"internal/api/handlers/telegram.go:VerifyPassword":    {},
	"internal/api/handlers/telegram.go:GetStatus":         {},
	"internal/api/handlers/telegram.go:ListChats":         {},
	"internal/api/handlers/telegram.go:chatToResponse":    {},
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
