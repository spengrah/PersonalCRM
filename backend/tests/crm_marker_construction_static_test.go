// Package tests — CRM-marker construction AST guard.
//
// Enforces that the Todoist CRM-marker wire format is constructed in exactly
// one place: contacttask.EncodeMarker (and its CRMMarker type), in
// backend/internal/contacttask/marker.go. This is the function-level
// companion (the suspenders) to the grep guard at
// scripts/ci/crm-marker-construction-guard.sh (the belt).
//
// Two construction shapes are detected:
//
//	(a) a map composite literal containing a "crm" string-literal key whose
//	    value is the bool literal true — the form every inline encoder used.
//	(b) a struct type declaration with a field carrying a `json:"crm"` tag —
//	    a struct encoder reintroduced outside the primitive.
//
// Both must live ONLY in internal/contacttask/marker.go, and within that file
// only inside the sanctioned declarations (allowedConstructionSites). A stray
// marker construction added to an unrelated function — even inside marker.go
// itself — trips the guard.
package tests

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// allowedConstructionSites maps (file, enclosingDecl) -> justification. A
// marker construction is accepted only when its enclosing top-level
// declaration name is listed here. Keys use forward slashes relative to the
// backend module root. enclosingDecl is the func name (for the map literal in
// EncodeMarker) or the type name (for the CRMMarker / crmMarkerJSON struct).
var allowedConstructionSites = map[string]string{
	"internal/contacttask/marker.go:EncodeMarker":  "sole sanctioned encoder",
	"internal/contacttask/marker.go:crmMarkerJSON": "decode-only wire struct",
}

// TestCRMMarkerConstruction_OnlyAllowedSites walks the Go AST of
// backend/internal + backend/cmd/crm-api and asserts every CRM-marker
// construction (map literal with "crm":true, or struct with a json:"crm" tag)
// lives in allowedConstructionSites. Generated sqlc files and test files are
// skipped.
func TestCRMMarkerConstruction_OnlyAllowedSites(t *testing.T) {
	moduleRoot, err := backendModuleRoot()
	if err != nil {
		t.Fatalf("locate backend module root: %v", err)
	}

	roots := []string{
		filepath.Join(moduleRoot, "internal"),
		filepath.Join(moduleRoot, "cmd", "crm-api"),
	}

	type violation struct {
		file string
		line int
		decl string
		kind string
	}
	var violations []violation

	fset := token.NewFileSet()
	for _, root := range roots {
		if err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if strings.HasSuffix(info.Name(), ".sql.go") ||
				info.Name() == "querier.go" ||
				info.Name() == "models.go" ||
				info.Name() == "db.go" {
				return nil
			}
			rel, err := filepath.Rel(moduleRoot, path)
			if err != nil {
				return err
			}
			relSlash := filepath.ToSlash(rel)

			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}

			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if d.Body == nil {
						continue
					}
					ast.Inspect(d.Body, func(n ast.Node) bool {
						if !isCRMMarkerMapLiteral(n) {
							return true
						}
						key := relSlash + ":" + d.Name.Name
						if _, allowed := allowedConstructionSites[key]; allowed {
							return true
						}
						violations = append(violations, violation{
							file: relSlash,
							line: fset.Position(n.Pos()).Line,
							decl: d.Name.Name,
							kind: `map literal "crm":true`,
						})
						return true
					})
				case *ast.GenDecl:
					if d.Tok != token.TYPE {
						continue
					}
					for _, spec := range d.Specs {
						ts, ok := spec.(*ast.TypeSpec)
						if !ok {
							continue
						}
						st, ok := ts.Type.(*ast.StructType)
						if !ok {
							continue
						}
						if !structHasCRMJSONTag(st) {
							continue
						}
						key := relSlash + ":" + ts.Name.Name
						if _, allowed := allowedConstructionSites[key]; allowed {
							continue
						}
						violations = append(violations, violation{
							file: relSlash,
							line: fset.Position(ts.Pos()).Line,
							decl: ts.Name.Name,
							kind: `struct field json:"crm"`,
						})
					}
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if len(violations) > 0 {
		sort.Slice(violations, func(i, j int) bool {
			if violations[i].file != violations[j].file {
				return violations[i].file < violations[j].file
			}
			return violations[i].line < violations[j].line
		})
		var msg strings.Builder
		msg.WriteString("CRM-marker construction guard: marker construction found outside the allowlist.\n")
		msg.WriteString("Build the marker via contacttask.EncodeMarker, or add a justified entry to allowedConstructionSites.\n\n")
		for _, v := range violations {
			msg.WriteString("  ")
			msg.WriteString(v.file)
			msg.WriteString(":")
			msg.WriteString(strconv.Itoa(v.line))
			msg.WriteString(" in ")
			msg.WriteString(v.decl)
			msg.WriteString(" — ")
			msg.WriteString(v.kind)
			msg.WriteString("\n")
		}
		t.Fatal(msg.String())
	}
}

// isCRMMarkerMapLiteral reports whether n is a composite literal containing a
// "crm" string-literal key whose value is the bool literal true.
func isCRMMarkerMapLiteral(n ast.Node) bool {
	cl, ok := n.(*ast.CompositeLit)
	if !ok {
		return false
	}
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.BasicLit)
		if !ok || key.Kind != token.STRING {
			continue
		}
		unq, err := strconv.Unquote(key.Value)
		if err != nil || unq != "crm" {
			continue
		}
		if val, ok := kv.Value.(*ast.Ident); ok && val.Name == "true" {
			return true
		}
	}
	return false
}

// structHasCRMJSONTag reports whether the struct has any field tagged
// `json:"crm"` (with or without options like `json:"crm,omitempty"`).
func structHasCRMJSONTag(st *ast.StructType) bool {
	if st.Fields == nil {
		return false
	}
	for _, f := range st.Fields.List {
		if f.Tag == nil {
			continue
		}
		raw, err := strconv.Unquote(f.Tag.Value)
		if err != nil {
			continue
		}
		jsonTag := reflect.StructTag(raw).Get("json")
		name := jsonTag
		if i := strings.IndexByte(name, ','); i >= 0 {
			name = name[:i]
		}
		if name == "crm" {
			return true
		}
	}
	return false
}

// TestCRMMarkerConstruction_NegativeGuardCatchesStray synthesizes a tiny Go
// file with a stray "crm":true map literal in an unallowlisted function, runs
// the same detection against it, and asserts a violation is reported. Without
// this, a future loosening of the detector could silently pass.
func TestCRMMarkerConstruction_NegativeGuardCatchesStray(t *testing.T) {
	src := `package poc

func strayEncoder() map[string]any {
	return map[string]any{"crm": true, "contact_id": "x"}
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "poc.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse poc: %v", err)
	}

	var found bool
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if isCRMMarkerMapLiteral(n) {
				found = true
			}
			return true
		})
	}
	if !found {
		t.Fatal("negative guard did not detect the stray marker literal — the real guard would let a new construction through")
	}
}
