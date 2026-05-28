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
// Shape (a) is detected inside function bodies AND in top-level var/const
// initializers (so a package-level `var x = map[string]any{"crm":true}` is not
// a hole); shape (c) is detected inside function bodies (the only place
// statements live).
//
// All must live ONLY in internal/contacttask/marker.go, and within that file
// only inside the sanctioned declarations (allowedConstructionSites). A stray
// marker construction added to an unrelated function — or a top-level var —
// even inside marker.go itself — trips the guard.
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

// markerViolation records one disallowed CRM-marker construction site.
type markerViolation struct {
	file string
	line int
	decl string
	kind string
}

// findCRMMarkerConstructions scans one parsed file for CRM-marker
// construction sites and returns any that fall outside allowedConstructionSites.
// relSlash is the file's path relative to the backend module root (forward
// slashes), used to build the allowlist key. Detection covers:
//   - map composite literals with "crm":true, in function bodies AND top-level
//     var/const initializers
//   - index assignments `m["crm"] = true`, in function bodies
//   - struct type declarations with a json:"crm" field tag
//
// Both the real-tree test and the negative self-test call this, so the two can
// never drift apart.
func findCRMMarkerConstructions(relSlash string, file *ast.File, fset *token.FileSet) []markerViolation {
	var violations []markerViolation
	record := func(declName string, pos token.Pos, kind string) {
		key := relSlash + ":" + declName
		if _, allowed := allowedConstructionSites[key]; allowed {
			return
		}
		violations = append(violations, markerViolation{
			file: relSlash,
			line: fset.Position(pos).Line,
			decl: declName,
			kind: kind,
		})
	}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body == nil {
				continue
			}
			ast.Inspect(d.Body, func(n ast.Node) bool {
				switch {
				case isCRMMarkerMapLiteral(n):
					record(d.Name.Name, n.Pos(), `map literal "crm":true`)
				case isCRMMarkerIndexAssign(n):
					record(d.Name.Name, n.Pos(), `index assignment ["crm"] = true`)
				}
				return true
			})
		case *ast.GenDecl:
			switch d.Tok {
			case token.TYPE:
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
					record(ts.Name.Name, ts.Pos(), `struct field json:"crm"`)
				}
			case token.VAR, token.CONST:
				// Top-level var/const with a composite-literal initializer
				// (e.g. var x = map[string]any{"crm":true}). Walk each value
				// expression; key the violation by the declared name so it
				// lands in the same allowlist as the func/type sites.
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					declName := "<anonymous>"
					if len(vs.Names) > 0 {
						declName = vs.Names[0].Name
					}
					for _, val := range vs.Values {
						ast.Inspect(val, func(n ast.Node) bool {
							if isCRMMarkerMapLiteral(n) {
								record(declName, n.Pos(), `map literal "crm":true`)
							}
							return true
						})
					}
				}
			}
		}
	}
	return violations
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

	var violations []markerViolation

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

			violations = append(violations, findCRMMarkerConstructions(relSlash, file, fset)...)
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
// snippets with a stray marker construction outside the allowlist, runs the
// SAME findCRMMarkerConstructions walk the real test uses, and asserts a
// violation is reported. Covers the map-literal form (in a function and in a
// top-level var), the incremental index-assignment form, and the struct-tag
// form, so a future loosening of any detector fails loudly.
func TestCRMMarkerConstruction_NegativeGuardCatchesStray(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "map literal in func",
			src: `package poc

func strayEncoder() map[string]any {
	return map[string]any{"crm": true, "contact_id": "x"}
}
`,
		},
		{
			name: "index assignment in func",
			src: `package poc

func strayEncoder() map[string]any {
	m := map[string]any{}
	m["crm"] = true
	m["contact_id"] = "x"
	return m
}
`,
		},
		{
			name: "top-level var initializer",
			src: `package poc

var strayMarker = map[string]any{"crm": true, "contact_id": "x"}
`,
		},
		{
			name: "struct field json tag",
			src: `package poc

type strayMarkerStruct struct {
	CRM bool ` + "`json:\"crm\"`" + `
}
`,
		},
	}

	// Use a relSlash NOT present in allowedConstructionSites so any detected
	// construction counts as a violation.
	const unallowlistedRel = "internal/poc/poc.go"

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "poc.go", tc.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse poc: %v", err)
			}
			got := findCRMMarkerConstructions(unallowlistedRel, file, fset)
			if len(got) == 0 {
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
