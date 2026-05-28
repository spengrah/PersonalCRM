// Package tests — CRM-marker construction AST guard.
//
// Enforces that the Todoist CRM-marker wire format is constructed in exactly
// one place: contacttask.EncodeMarker (and its CRMMarker type), in
// backend/internal/contacttask/marker.go. This is the function-level
// companion (the suspenders) to the grep guard at
// scripts/ci/crm-marker-construction-guard.sh (the belt).
//
// Three construction shapes are detected:
//
//	(a) a map composite literal containing a "crm" string-literal key whose
//	    value is the bool literal true — the form every inline encoder used.
//	(b) a struct type declaration with a field carrying a `json:"crm"` tag —
//	    a struct encoder reintroduced outside the primitive.
//	(c) an index-assignment statement of the form `<map>["crm"] = true`
//	    (*ast.AssignStmt whose LHS is an *ast.IndexExpr with a "crm"
//	    string-literal index and a bool-true RHS) — an incrementally-built
//	    marker map.
//
// All must live ONLY in internal/contacttask/marker.go, and within that file
// only inside the sanctioned declarations (allowedConstructionSites). A stray
// marker construction added to an unrelated function — even inside marker.go
// itself — trips the guard.
package tests

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
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
// construction (map literal with "crm":true, an index assignment
// `m["crm"] = true`, or a struct with a json:"crm" tag) lives in
// allowedConstructionSites. Generated sqlc files and test files are skipped.
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
						var kind string
						switch {
						case isCRMMarkerMapLiteral(n):
							kind = `map literal "crm":true`
						case isCRMMarkerIndexAssign(n):
							kind = `index assignment ["crm"] = true`
						default:
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
							kind: kind,
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

// isCRMMarkerIndexAssign reports whether n is an assignment statement of the
// form `<map>["crm"] = true` — an incrementally-built marker map. Matches both
// `=` and `:=` assignments whose LHS is an index expression with a "crm"
// string-literal index and whose corresponding RHS is the bool literal true.
func isCRMMarkerIndexAssign(n ast.Node) bool {
	assign, ok := n.(*ast.AssignStmt)
	if !ok {
		return false
	}
	for i, lhs := range assign.Lhs {
		idx, ok := lhs.(*ast.IndexExpr)
		if !ok {
			continue
		}
		key, ok := idx.Index.(*ast.BasicLit)
		if !ok || key.Kind != token.STRING {
			continue
		}
		unq, err := strconv.Unquote(key.Value)
		if err != nil || unq != "crm" {
			continue
		}
		// Parallel-assignment safe: match the RHS at the same position when
		// the counts line up; otherwise (e.g. multi-value call RHS) treat the
		// "crm" index target as a hit conservatively.
		if len(assign.Rhs) == len(assign.Lhs) {
			if val, ok := assign.Rhs[i].(*ast.Ident); ok && val.Name == "true" {
				return true
			}
			continue
		}
		return true
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

// TestCRMMarkerConstruction_NegativeGuardCatchesStray synthesizes tiny Go
// snippets with a stray marker construction in an unallowlisted function, runs
// the AST detection against each, and asserts a violation is reported. Covers
// both the map-literal form and the incremental index-assignment form so a
// future loosening of either detector fails loudly.
func TestCRMMarkerConstruction_NegativeGuardCatchesStray(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "map literal",
			src: `package poc

func strayEncoder() map[string]any {
	return map[string]any{"crm": true, "contact_id": "x"}
}
`,
		},
		{
			name: "index assignment",
			src: `package poc

func strayEncoder() map[string]any {
	m := map[string]any{}
	m["crm"] = true
	m["contact_id"] = "x"
	return m
}
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "poc.go", tc.src, parser.SkipObjectResolution)
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
					if isCRMMarkerMapLiteral(n) || isCRMMarkerIndexAssign(n) {
						found = true
					}
					return true
				})
			}
			if !found {
				t.Fatalf("negative guard did not detect the stray %s — the real guard would let a new construction through", tc.name)
			}
		})
	}
}

// TestCRMMarkerGrepGuard_CatchesIndexAssignment runs the actual grep guard
// script against a temporary stray file built with the incremental
// `m["crm"] = true` form, and asserts the script exits non-zero. This proves
// the belt (grep) layer covers pattern (d), complementing the AST suspenders
// above. The stray file is written under backend/internal and removed after.
func TestCRMMarkerGrepGuard_CatchesIndexAssignment(t *testing.T) {
	moduleRoot, err := backendModuleRoot()
	if err != nil {
		t.Fatalf("locate backend module root: %v", err)
	}
	repoRoot := filepath.Dir(moduleRoot)
	guard := filepath.Join(repoRoot, "scripts", "ci", "crm-marker-construction-guard.sh")
	if _, statErr := os.Stat(guard); statErr != nil {
		t.Fatalf("guard script not found at %s: %v", guard, statErr)
	}

	// Sanity: the guard must currently pass on the real tree (no false
	// positives) before we inject the stray.
	if out, runErr := exec.Command(guard).CombinedOutput(); runErr != nil {
		t.Fatalf("guard unexpectedly failed on clean tree: %v\n%s", runErr, out)
	}

	strayDir := filepath.Join(moduleRoot, "internal", "todoist")
	strayPath := filepath.Join(strayDir, "zz_crm_marker_grep_probe.go")
	content := "package todoist\n\nfunc crmMarkerGrepProbe() map[string]any {\n\tm := map[string]any{}\n\tm[\"crm\"] = true\n\treturn m\n}\n"
	if writeErr := os.WriteFile(strayPath, []byte(content), 0o644); writeErr != nil {
		t.Fatalf("write stray probe: %v", writeErr)
	}
	defer func() {
		if rmErr := os.Remove(strayPath); rmErr != nil {
			t.Errorf("remove stray probe %s: %v", strayPath, rmErr)
		}
	}()

	out, runErr := exec.Command(guard).CombinedOutput()
	if runErr == nil {
		t.Fatalf("grep guard did NOT flag the stray index-assignment marker; output:\n%s", out)
	}
	if !strings.Contains(string(out), "zz_crm_marker_grep_probe.go") {
		t.Errorf("grep guard failed but did not name the stray file; output:\n%s", out)
	}
}
