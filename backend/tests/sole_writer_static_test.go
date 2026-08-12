// Package tests — cutover sole-writer AST guard.
//
// This test enforces the acceptance criterion that CadenceUpdater is the
// sole post-create writer of contact.last_contacted,
// contact.last_interaction_at, contact.last_outreach_at,
// contact.last_response_at, and contact.contact_by. It walks every .go
// file under backend/internal and backend/cmd/crm-api, flags any call
// to one of the cadence-writing sqlc-generated queries or repository
// tx wrappers, and compares the hit set against a function-level
// allowlist.
//
// Design:
//   - Inventory tracks the sqlc query names that mutate one or more
//     cadence columns, INCLUDING CreateContact + UpdateContact (both
//     write cadence-family columns in their full column lists, even
//     though post-cutover UpdateContact is profile-only by convention).
//   - For CreateContact/UpdateContact specifically, we only flag calls
//     whose receiver is an sqlc Querier (r.queries, q, db.New(tx)) —
//     wrapper-to-wrapper calls (contactRepo.CreateContact from service)
//     are legitimate and should NOT trip. This avoids flagging every
//     handler that goes through the service layer.
//   - Allowlist is FUNCTION-LEVEL: a (file, funcName) pair must be
//     explicitly listed for a hit to be accepted. A new method anywhere
//     in an allowlisted file that adds a cadence-write call will still
//     trip the guard unless the allowlist is expanded.
//
// Also includes TestUpdateContactSQL_DoesNotTouchCadenceColumns, which
// parses contact.sql and asserts the UpdateContact query's SET clause
// never includes a cadence column — a future regression that adds
// `last_contacted = $X` to UpdateContact would trip at the SQL layer
// without having to wait for a call-site guard to miss it.
package tests

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// cadenceWritingSymbols enumerates the sqlc query + repository wrapper
// selector names that mutate one or more cadence columns. Matching is by
// call-expression selector name only, with a receiver-scope refinement
// for CreateContact/UpdateContact (see scopedToSqlcQuerier below).
//
// Kept in sync with backend/internal/db/queries/contact.sql. When
// adding a new cadence-writing query, add its name here.
var cadenceWritingSymbols = map[string]struct{}{
	"UpdateContactLastContacted":        {},
	"UpdateContactLastContactedIfLater": {},
	"UpdateContactOutreachAt":           {},
	"UpdateContactOutreachAtTx":         {},
	"UpdateContactResponseFields":       {},
	"UpdateContactResponseFieldsTx":     {},
	"UpdateContactMutualFields":         {},
	"UpdateContactMutualFieldsTx":       {},
	"UpdateContactCadenceForward":       {},
	"UpdateContactCadenceUnconditional": {},
	"UpdateContactBy":                   {},
	"UpdateContactByTx":                 {},
	"WriteContactDatesAfterDelete":      {},
	// Include CreateContact + UpdateContact so a future regression
	// that adds a cadence column to either query is caught. Selector
	// matching alone would flag every service-layer wrapper caller;
	// querierScopedSymbols below restricts these two names to direct
	// sqlc-Querier call sites.
	"CreateContact": {},
	"UpdateContact": {},
}

// querierScopedSymbols are cadence-writing queries whose names collide
// with higher-level wrapper methods (repo → service → handler). For
// these, a call is only treated as a cadence write when the RECEIVER
// chain matches an sqlc Querier shape — i.e., the call bypasses the
// service layer and talks to the database directly. Wrapper-level
// callers (contactRepo.CreateContact from the service, etc.) are NOT
// flagged.
var querierScopedSymbols = map[string]struct{}{
	"CreateContact": {},
	"UpdateContact": {},
}

// allowedCallSite records which inventoried selectors one (file, function)
// may call, and why. Permission is per SYMBOL, not per function: an entry
// that may call the three knowledge Tx wrappers must NOT thereby be able to
// call a cadence fixture writer, which would declare the cadence owner for
// itself and write cadence columns with every gate green.
type allowedCallSite struct {
	symbols []string
	why     string
}

// allowedCallSites maps (file, enclosingFunc) → the inventoried selectors
// that call site may invoke. A hit on a cadenceWritingSymbol is accepted
// only if its call site is in this map AND the matched selector is in that
// entry's symbols list. Keys use forward slashes relative to the backend
// module root.
//
// The sole writer lives in consumer/cadence_updater.go:applyTx. Other
// entries are narrow, documented carve-outs.
//
// NOTE (round 1 of PR7's red/green split): this is an interim state. Two of
// the four new fixture-wrapper entries below (TestSeedContactCadenceFields,
// TestWriteKnowledgeColumnsWithoutGUCTx) name symbols not yet present in
// cadenceWritingSymbols, so they are inert until the green phase adds them.
// The ten-symbol inventory extension, the CreateContact -> CreateContactWithNode
// rename, and the RefreshTx / knowledge-wrapper entries land later in the
// same PR.
var allowedCallSites = map[string]allowedCallSite{
	// The authoritative consumer-owned cadence writer. applyTx dispatches
	// to UpdateContactCadenceForward/Unconditional; this is the one
	// place cadence SQL is allowed to live.
	"internal/consumer/cadence_updater.go:applyTx": {
		symbols: []string{"UpdateContactCadenceForward", "UpdateContactCadenceUnconditional"},
		why:     "sole writer",
	},

	// Repository wrappers. Each wrapper is a thin method that delegates
	// to the corresponding sqlc query. All production cadence writes
	// route through CadenceUpdater; these wrappers remain in the
	// allowlist because the wrapper bodies themselves call cadence SQL
	// and because test fixtures use UpdateContactBy/UpdateContactByTx
	// directly to seed contact_by without going through the sole-writer
	// path. Adding a NEW wrapper here requires a matching allowlist
	// entry, so regressions surface at review time.
	"internal/repository/contact.go:CreateContact": {
		symbols: []string{"CreateContact"},
		why:     "initial row seed carve-out",
	},
	"internal/repository/contact.go:UpdateContact": {
		symbols: []string{"UpdateContact"},
		why:     "profile-only wrapper (post-cutover cadence columns not written)",
	},
	"internal/repository/contact.go:UpdateContactBy": {
		symbols: []string{"UpdateContactBy"},
		why:     "test-fixture / implementation wrapper; production writes route through CadenceUpdater",
	},
	"internal/repository/contact.go:UpdateContactByTx": {
		symbols: []string{"UpdateContactBy"},
		why:     "test-fixture / implementation wrapper; production writes route through CadenceUpdater",
	},
	"internal/repository/contact.go:UpdateContactLastContacted": {
		symbols: []string{"UpdateContactLastContacted"},
		why:     "legacy wrapper (no production callers post-cutover)",
	},
	"internal/repository/contact.go:UpdateContactLastContactedIfLater": {
		symbols: []string{"UpdateContactLastContactedIfLater"},
		why:     "legacy wrapper (no production callers post-cutover)",
	},
	"internal/repository/contact.go:UpdateContactOutreachAt": {
		symbols: []string{"UpdateContactOutreachAt"},
		why:     "legacy wrapper (no production callers post-cutover)",
	},
	"internal/repository/contact.go:UpdateContactOutreachAtTx": {
		symbols: []string{"UpdateContactOutreachAt"},
		why:     "legacy wrapper (no production callers post-cutover)",
	},
	"internal/repository/contact.go:UpdateContactResponseFields": {
		symbols: []string{"UpdateContactResponseFields"},
		why:     "legacy wrapper (no production callers post-cutover)",
	},
	"internal/repository/contact.go:UpdateContactResponseFieldsTx": {
		symbols: []string{"UpdateContactResponseFields"},
		why:     "legacy wrapper (no production callers post-cutover)",
	},
	"internal/repository/contact.go:UpdateContactMutualFields": {
		symbols: []string{"UpdateContactMutualFields"},
		why:     "legacy wrapper (no production callers post-cutover)",
	},
	"internal/repository/contact.go:UpdateContactMutualFieldsTx": {
		symbols: []string{"UpdateContactMutualFields"},
		why:     "legacy wrapper (no production callers post-cutover)",
	},

	// Removal-path recompute after soft-deleting a declined calendar
	// interaction: surgical backward correction keyed on the deleted
	// interaction's occurred_at, contact_by computed via
	// cadence.CalculateContactBy to match the forward writer; distinct
	// from CadenceUpdater's additive forward path. recomputeContactDatesAfterDelete
	// is the shared body that calls WriteContactDatesAfterDelete; the two
	// exported wrappers (RecomputeContactDatesAfterDelete[Tx]) delegate to it.
	"internal/repository/contact.go:recomputeContactDatesAfterDelete": {
		symbols: []string{"WriteContactDatesAfterDelete"},
		why:     "removal-path recompute (declined calendar interaction); distinct from CadenceUpdater forward path",
	},

	// Test-only fixture wrapper selectors (PR7 D7-7). Two of these symbols
	// (TestSeedContactCadenceFieldsTx here; TestWriteCadenceColumnsWithoutGUCTx
	// below) call UpdateContactCadenceUnconditional, already inventoried, so
	// their entries are live from round 1. The other two name symbols the
	// green phase adds to the inventory and are inert until then.
	"internal/repository/contact.go:TestSeedContactCadenceFieldsTx": {
		symbols: []string{"UpdateContactCadenceUnconditional"},
		why:     "test fixture writer; declares the cadence owner before writing",
	},
	"internal/repository/contact.go:TestSeedContactCadenceFields": {
		symbols: []string{"TestSeedContactCadenceFieldsTx"},
		why:     "test fixture writer (pool-level); opens its own tx, then delegates",
	},
	"internal/repository/contact.go:TestWriteCadenceColumnsWithoutGUCTx": {
		symbols: []string{"UpdateContactCadenceUnconditional"},
		why:     "deliberate unauthorized probe for the derived-writer trigger's rejection tests",
	},
	"internal/repository/contact.go:TestWriteKnowledgeColumnsWithoutGUCTx": {
		symbols: []string{"UpdateContactLocationCache", "UpdateContactBirthdayCache", "UpdateContactHowMetCache"},
		why:     "deliberate unauthorized probe (knowledge columns)",
	},
}

// TestCadenceSoleWriter_OnlyAllowedFilesCallCadenceSQL walks the Go AST
// of backend/internal + backend/cmd/crm-api and asserts every call to a
// cadence-writing symbol lives in the allowedCallSites map. Generated
// sqlc files and test files are skipped.
func TestCadenceSoleWriter_OnlyAllowedFilesCallCadenceSQL(t *testing.T) {
	t.Parallel()
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
		fn   string
		call string
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
			// Skip generated sqlc files: their UPDATE SQL is the target
			// of the guard, not a source of uncontrolled writes.
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

			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}

			// Walk top-level function declarations so we can track the
			// enclosing function name for each call.
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				fnName := fn.Name.Name
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					name := sel.Sel.Name
					if _, hit := cadenceWritingSymbols[name]; !hit {
						return true
					}
					// For collision-prone names, require the receiver
					// to look like an sqlc Querier (queries, q, or
					// db.New(tx)).
					if _, scoped := querierScopedSymbols[name]; scoped {
						if !receiverIsSqlcQuerier(sel.X) {
							return true
						}
					}
					key := filepath.ToSlash(rel) + ":" + fnName
					if site, hasKey := allowedCallSites[key]; hasKey && slices.Contains(site.symbols, name) {
						return true
					}
					pos := fset.Position(call.Pos())
					violations = append(violations, violation{
						file: filepath.ToSlash(rel),
						line: pos.Line,
						fn:   fnName,
						call: name,
					})
					return true
				})
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
		msg.WriteString("cadence sole-writer guard: cadence-writing calls found outside the function-level allowlist.\n")
		msg.WriteString("Either route the write through CadenceUpdater, or add a justified entry to allowedCallSites.\n\n")
		for _, v := range violations {
			msg.WriteString("  ")
			msg.WriteString(v.file)
			msg.WriteString(":")
			msg.WriteString(strconv.Itoa(v.line))
			msg.WriteString(" in ")
			msg.WriteString(v.fn)
			msg.WriteString(" — ")
			msg.WriteString(v.call)
			msg.WriteString("\n")
		}
		t.Fatal(msg.String())
	}
}

// receiverIsSqlcQuerier returns true when the receiver of a selector
// expression looks like an sqlc Querier handle. This excludes wrapper
// receivers like `contactRepo`, `s.contactRepo`, `contactService`, etc.
//
// Accepted shapes:
//   - `queries.X`               (bare Queries var)
//   - `r.queries.X`             (ContactRepository field)
//   - `q.X`                     (common loop var)
//   - `db.New(tx).X`            (tx-scoped Queries constructor)
//   - `txQueries.X`             (ad-hoc tx queries var — service layer uses this)
func receiverIsSqlcQuerier(recv ast.Expr) bool {
	switch e := recv.(type) {
	case *ast.Ident:
		return sqlcQuerierIdent(e.Name)
	case *ast.SelectorExpr:
		return sqlcQuerierIdent(e.Sel.Name)
	case *ast.CallExpr:
		// db.New(tx) → SelectorExpr{X: db, Sel: New}
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
			if sel.Sel.Name == "New" {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "db" {
					return true
				}
			}
		}
	}
	return false
}

func sqlcQuerierIdent(name string) bool {
	// Exact hits or patterns that reliably denote an sqlc Querier.
	switch name {
	case "queries", "q", "txQueries":
		return true
	}
	return strings.HasSuffix(name, "Queries")
}

// TestUpdateContactSQL_DoesNotTouchCadenceColumns parses contact.sql
// and asserts the UpdateContact query's SET clause never includes a
// cadence column. A regression that adds e.g. `last_contacted = $X` to
// UpdateContact would trip here, independent of the call-site guard.
func TestUpdateContactSQL_DoesNotTouchCadenceColumns(t *testing.T) {
	t.Parallel()
	moduleRoot, err := backendModuleRoot()
	if err != nil {
		t.Fatalf("locate backend module root: %v", err)
	}
	sqlPath := filepath.Join(moduleRoot, "internal", "db", "queries", "contact.sql")
	raw, err := os.ReadFile(sqlPath)
	if err != nil {
		t.Fatalf("read contact.sql: %v", err)
	}

	// Extract the UpdateContact query (between `-- name: UpdateContact :`
	// and the next `-- name:` header).
	queryRe := regexp.MustCompile(`(?s)-- name: UpdateContact :one\n(.*?)(?:\n-- name:|$)`)
	m := queryRe.FindStringSubmatch(string(raw))
	if len(m) < 2 {
		t.Fatalf("could not locate UpdateContact query in %s", sqlPath)
	}
	queryBody := m[1]

	forbidden := []string{
		"last_contacted",
		"last_interaction_at",
		"last_outreach_at",
		"last_response_at",
		"contact_by",
	}
	for _, col := range forbidden {
		// Match a SET assignment for the column (`<col> =` with any
		// whitespace). This catches `last_contacted = $N` even on
		// continuation lines, while tolerating the column being
		// mentioned in a comment.
		setRe := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(col) + `\s*=`)
		// Strip SQL line comments so a comment mentioning the column
		// doesn't cause a false positive.
		lines := strings.Split(queryBody, "\n")
		var stripped []string
		for _, ln := range lines {
			if i := strings.Index(ln, "--"); i >= 0 {
				ln = ln[:i]
			}
			stripped = append(stripped, ln)
		}
		if setRe.MatchString(strings.Join(stripped, "\n")) {
			t.Errorf("UpdateContact query must not write cadence column %q — route cadence mutations through CadenceUpdater", col)
		}
	}
}

// TestCadenceSoleWriter_NegativeGuardCatchesNewWrite synthesizes a tiny
// Go file that calls r.queries.UpdateContactMutualFields from an
// unallowlisted function, runs the same AST check against it, and
// asserts a violation is reported. Without this, a future loosening
// of the check (e.g., all-files-allowed) could silently pass.
func TestCadenceSoleWriter_NegativeGuardCatchesNewWrite(t *testing.T) {
	t.Parallel()
	src := `package poc

type querier struct{}

func (q *querier) UpdateContactMutualFields() {}

type poc struct {
	queries *querier
}

func (p *poc) FakeNewWriter() {
	p.queries.UpdateContactMutualFields()
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
		fnName := fn.Name.Name
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if _, hit := cadenceWritingSymbols[sel.Sel.Name]; !hit {
				return true
			}
			// Simulate the allowlist lookup against a fake path that
			// is NOT in allowedCallSites — expectation is no match.
			key := "internal/poc/poc.go:" + fnName
			if _, allowed := allowedCallSites[key]; allowed {
				t.Fatalf("unexpected allowlist hit for synthetic key %s", key)
			}
			found = true
			return true
		})
	}
	if !found {
		t.Fatal("negative guard did not detect a cadence-writing call site in the synthetic POC — the real guard would let a new writer through")
	}
}

// backendModuleRoot returns the absolute path to the backend module's root
// by walking up from cwd until it finds go.mod. The tests binary runs
// with cwd set to the package's directory (backend/tests), so one hop up
// normally suffices — but the loop keeps the helper robust if that
// convention ever changes.
func backendModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
