// Package tests — derived-column sole-writer AST guard.
//
// This test enforces the rule that contact's eight derived columns each have
// exactly one owner: CadenceUpdater (last_contacted, last_interaction_at,
// last_outreach_at, last_response_at, contact_by) and
// KnowledgeCacheUpdater.RefreshTx (location, birthday, how_met). It walks
// every .go file under backend/internal and backend/cmd/crm-api, flags any
// call to one of the derived-writing sqlc-generated queries or repository
// tx wrappers, and compares the hit set against a per-(file, function,
// symbol) allowlist. From migration 079 onward this is the SECOND enforcer —
// a BEFORE UPDATE trigger on contact rejects the write at the database layer
// regardless of what Go code attempts it — but this guard still earns its
// keep: it fails at `make test-unit` / CI time with a source-level message,
// before a test ever reaches the database.
//
// Design:
//   - Inventory tracks the sqlc query + wrapper selector names that mutate
//     one or more derived columns, INCLUDING CreateContactWithNode +
//     UpdateContact (both write derived-family columns in their full column
//     lists, even though post-cutover UpdateContact is profile-only by
//     convention and CreateContactWithNode's write is an INSERT the trigger
//     does not reach).
//   - For CreateContactWithNode/UpdateContact specifically, we only flag
//     calls whose receiver is an sqlc Querier (r.queries, q, db.New(tx)) —
//     wrapper-to-wrapper calls (contactRepo.CreateContact from service) are
//     legitimate and should NOT trip. This avoids flagging every handler
//     that goes through the service layer.
//   - Allowlist is per (file, function, SYMBOL): a hit is accepted only when
//     its (file, function) key is present AND the matched selector is in
//     that entry's symbol list. Permission is per symbol, not per function,
//     because a function allowed to call one derived-writing symbol (e.g.
//     RefreshTx calling the three knowledge Tx wrappers) must NOT thereby be
//     able to call a DIFFERENT symbol from a different owner (e.g. a cadence
//     fixture writer) — that would authorize a cross-owner write with every
//     gate green.
//
// Also includes TestUpdateContactSQL_SetClauseIsExactlyProfileColumns, which
// parses contact.sql and asserts UpdateContact's SET clause is EXACTLY
// {cadence, full_name, profile_photo, updated_at} — an exact-set comparison
// rather than a denylist, so a future regression that adds ANY derived
// column (cadence or knowledge) trips at the SQL layer without having to
// wait for a call-site guard to miss it.
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

	"github.com/stretchr/testify/require"
)

// derivedWritingSymbols enumerates the sqlc query + repository wrapper
// selector names that mutate one or more of contact's eight derived columns
// (five cadence + three knowledge-cache). Matching is by call-expression
// selector name only, with a receiver-scope refinement for
// CreateContactWithNode/UpdateContact (see scopedToSqlcQuerier below).
//
// Three symbol classes, because inventorying only the underlying sqlc query
// is not enough — the guard matches on call-expression SELECTOR name and
// skips _test.go, so what it can see is exactly what is in this map:
//   - sqlc query selectors: the direct db.New(tx).X(...) call.
//   - fixture wrapper selectors (Test-prefixed): contactRepo.X(...) from a
//     test; two of these DECLARE their own owner, so an uninventoried caller
//     would be a fully-authorized production back door.
//   - knowledge Tx wrapper selectors: contactRepo.X(...); production calls
//     THESE, not the sqlc queries directly, and each declares the
//     knowledge_cache owner for itself — the same back-door risk.
//
// Kept in sync with backend/internal/db/queries/contact.sql and
// backend/internal/repository/contact.go. When adding a new derived-writing
// query or wrapper, add its name here AND a scoped allowedCallSites entry.
var derivedWritingSymbols = map[string]struct{}{
	// sqlc query selectors.
	"UpdateContactCadenceForward":       {},
	"UpdateContactCadenceUnconditional": {},
	"WriteContactDatesAfterDelete":      {},
	"UpdateContactLocationCache":        {},
	"UpdateContactBirthdayCache":        {},
	"UpdateContactHowMetCache":          {},
	// fixture wrapper selectors.
	"TestSeedContactCadenceFields":          {},
	"TestSeedContactCadenceFieldsTx":        {},
	"TestWriteCadenceColumnsWithoutGUCTx":   {},
	"TestWriteKnowledgeColumnsWithoutGUCTx": {},
	// knowledge Tx wrapper selectors.
	"UpdateContactLocationCacheTx": {},
	"UpdateContactBirthdayCacheTx": {},
	"UpdateContactHowMetCacheTx":   {},
	// Include CreateContactWithNode + UpdateContact so a future regression
	// that adds a derived column to either query is caught. Selector
	// matching alone would flag every service-layer wrapper caller;
	// querierScopedSymbols below restricts these two names to direct
	// sqlc-Querier call sites.
	"CreateContactWithNode": {},
	"UpdateContact":         {},
}

// querierScopedSymbols are derived-writing queries whose names collide
// with higher-level wrapper methods (repo → service → handler). For
// these, a call is only treated as a derived-column write when the RECEIVER
// chain matches an sqlc Querier shape — i.e., the call bypasses the
// service layer and talks to the database directly. Wrapper-level
// callers (contactRepo.CreateContact from the service, etc.) are NOT
// flagged.
var querierScopedSymbols = map[string]struct{}{
	"CreateContactWithNode": {},
	"UpdateContact":         {},
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
// that call site may invoke. A hit on a derivedWritingSymbol is accepted
// only if its call site is in this map AND the matched selector is in that
// entry's symbols list. Keys use forward slashes relative to the backend
// module root.
//
// Two owners, twelve entries: CadenceUpdater.applyTx for the five cadence
// columns; KnowledgeCacheUpdater.RefreshTx — the SOLE permitted caller of the
// three knowledge Tx wrappers — for the three knowledge columns. Every other
// entry is a narrow, documented carve-out.
var allowedCallSites = map[string]allowedCallSite{
	// The authoritative consumer-owned cadence writer. applyTx dispatches
	// to UpdateContactCadenceForward/Unconditional; this is the one
	// place cadence SQL is allowed to live.
	"internal/consumer/cadence_updater.go:applyTx": {
		symbols: []string{"UpdateContactCadenceForward", "UpdateContactCadenceUnconditional"},
		why:     "sole writer",
	},
	// The authoritative consumer-owned knowledge-cache writer, and the ONLY
	// permitted caller of the three knowledge Tx wrappers — those wrappers
	// each declare the knowledge_cache owner for themselves, so any OTHER
	// caller would write a knowledge column with full authorization while
	// bypassing KnowledgeCacheUpdater's recompute/supersession/closure logic
	// entirely.
	"internal/consumer/knowledge_cache.go:RefreshTx": {
		symbols: []string{"UpdateContactLocationCacheTx", "UpdateContactBirthdayCacheTx", "UpdateContactHowMetCacheTx"},
		why:     "the ONLY permitted caller of the three knowledge Tx wrappers; KnowledgeCacheUpdater is the column owner",
	},
	"internal/repository/contact.go:CreateContact": {
		symbols: []string{"CreateContactWithNode"}, // PR5 renamed the query; the wrapper method kept its name
		why:     "initial row seed carve-out (INSERT; the trigger is UPDATE-only)",
	},
	"internal/repository/contact.go:UpdateContact": {
		symbols: []string{"UpdateContact"},
		why:     "profile-only wrapper; SET clause pinned by TestUpdateContactSQL_SetClauseIsExactlyProfileColumns",
	},
	// Removal-path recompute after soft-deleting a declined calendar
	// interaction: surgical backward correction keyed on the deleted
	// interaction's occurred_at, contact_by computed via
	// cadence.CalculateContactBy to match the forward writer; distinct
	// from CadenceUpdater's additive forward path. recomputeContactDatesAfterDelete
	// is the shared body that calls WriteContactDatesAfterDelete; the exported
	// wrapper (RecomputeContactDatesAfterDeleteTx) delegates to it.
	"internal/repository/contact.go:recomputeContactDatesAfterDelete": {
		symbols: []string{"WriteContactDatesAfterDelete"},
		why:     "removal-path recompute (declined calendar interaction); distinct from CadenceUpdater forward path",
	},
	// The three knowledge-cache sole-writer wrapper BODIES: each declares the
	// knowledge_cache owner for itself before calling its sqlc query. Not to
	// be confused with the Tx wrappers' own selectors above, which is what
	// RefreshTx is allowlisted to call.
	"internal/repository/contact.go:UpdateContactLocationCacheTx": {
		symbols: []string{"UpdateContactLocationCache"},
		why:     "knowledge-cache sole-writer wrapper body; declares the knowledge_cache owner",
	},
	"internal/repository/contact.go:UpdateContactBirthdayCacheTx": {
		symbols: []string{"UpdateContactBirthdayCache"},
		why:     "knowledge-cache sole-writer wrapper body; declares the knowledge_cache owner",
	},
	"internal/repository/contact.go:UpdateContactHowMetCacheTx": {
		symbols: []string{"UpdateContactHowMetCache"},
		why:     "knowledge-cache sole-writer wrapper body; declares the knowledge_cache owner",
	},
	// Test-only fixture wrapper selectors (PR7 D7-7). No new write SQL: the
	// cadence pair reuses the production UpdateContactCadenceUnconditional
	// query; the deliberate unauthorized probes exist solely so the
	// derived-writer trigger's rejection tests have a legal way to attempt a
	// rejected write (raw SQL in Go is banned).
	"internal/repository/contact.go:TestSeedContactCadenceFieldsTx": {
		symbols: []string{"UpdateContactCadenceUnconditional"},
		why:     "test fixture writer; declares the cadence owner before writing",
	},
	"internal/repository/contact.go:TestSeedContactCadenceFields": {
		symbols: []string{"TestSeedContactCadenceFieldsTx"}, // pool-level variant delegates to the tx form
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
// derived-writing symbol lives in the allowedCallSites map, for the symbol
// actually matched. Generated sqlc files and test files are skipped. Name
// unchanged from the cadence-only era — it is a test identifier, not a
// public interface, and GI-6 governs symbol/query names, not test names.
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
		file              string
		line              int
		fn                string
		call              string
		allowlistedButNot bool // this (file, function) is allowlisted, just not for this symbol
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
					if _, hit := derivedWritingSymbols[name]; !hit {
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
					site, hasKey := allowedCallSites[key]
					if hasKey && slices.Contains(site.symbols, name) {
						return true
					}
					pos := fset.Position(call.Pos())
					violations = append(violations, violation{
						file:              filepath.ToSlash(rel),
						line:              pos.Line,
						fn:                fnName,
						call:              name,
						allowlistedButNot: hasKey, // key hit, but not for THIS symbol — see the message below
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
		msg.WriteString("derived-column sole-writer guard: writes to a derived contact column found outside the per-symbol allowlist.\n")
		msg.WriteString("Cadence columns (last_contacted, last_interaction_at, last_outreach_at, last_response_at, contact_by) route through CadenceUpdater; ")
		msg.WriteString("knowledge-cache columns (location, birthday, how_met) route through KnowledgeCacheUpdater.RefreshTx. ")
		msg.WriteString("Otherwise add a justified allowedCallSites entry naming the specific symbol.\n\n")
		for _, v := range violations {
			// This exact line format — "  <file>:<line> in <fn> — <call>" — is
			// pinned by F4's anchored grep; do not change it independently of
			// the falsification commands in the PR body.
			msg.WriteString("  ")
			msg.WriteString(v.file)
			msg.WriteString(":")
			msg.WriteString(strconv.Itoa(v.line))
			msg.WriteString(" in ")
			msg.WriteString(v.fn)
			msg.WriteString(" — ")
			msg.WriteString(v.call)
			msg.WriteString("\n")
			if v.allowlistedButNot {
				// Distinct from "not allowlisted at all": this (file, function)
				// IS in allowedCallSites, just not permitted to call THIS
				// symbol — the per-symbol scoping F4d falsifies.
				msg.WriteString("    (")
				msg.WriteString(v.file)
				msg.WriteString(":")
				msg.WriteString(v.fn)
				msg.WriteString(" is allowlisted but not for ")
				msg.WriteString(v.call)
				msg.WriteString(")\n")
			}
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

// updateContactSetClauseColumns parses contact.sql's UpdateContact query and
// returns the sorted, deduplicated set of columns its SET clause assigns.
// Shared by TestUpdateContactSQL_SetClauseIsExactlyProfileColumns.
func updateContactSetClauseColumns(t *testing.T) []string {
	t.Helper()
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

	// Strip SQL line comments so a comment mentioning a column doesn't
	// contribute a false assignment.
	lines := strings.Split(queryBody, "\n")
	var strippedLines []string
	for _, ln := range lines {
		if i := strings.Index(ln, "--"); i >= 0 {
			ln = ln[:i]
		}
		strippedLines = append(strippedLines, ln)
	}
	stripped := strings.Join(strippedLines, "\n")

	// Isolate the SET clause itself — between SET and the top-level WHERE —
	// so a WHERE-clause comparison (`WHERE id = $1`) is never mistaken for a
	// SET assignment. Word-boundaried so a future column literally named
	// e.g. "offset" or "asset" can't collide with SET/WHERE as a substring.
	setLoc := regexp.MustCompile(`\bSET\b`).FindStringIndex(stripped)
	if setLoc == nil {
		t.Fatalf("could not locate SET in UpdateContact query")
	}
	rest := stripped[setLoc[1]:]
	whereLoc := regexp.MustCompile(`\bWHERE\b`).FindStringIndex(rest)
	if whereLoc == nil {
		t.Fatalf("could not locate WHERE in UpdateContact query")
	}
	setClause := rest[:whereLoc[0]]

	// Split on TOP-LEVEL commas (paren-depth-aware) rather than lines: SQL
	// permits multiple assignments on one physical line
	// (`profile_photo = $4, location = $5,`), and a line-anchored `^` match
	// would silently miss every assignment after the first on that line —
	// exactly the shape that let a denylist-successor regression hide.
	var assignments []string
	depth := 0
	start := 0
	for i, r := range setClause {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				assignments = append(assignments, setClause[start:i])
				start = i + 1
			}
		}
	}
	assignments = append(assignments, setClause[start:])

	assignRe := regexp.MustCompile(`^\s*(\w+)\s*=`)
	seen := map[string]struct{}{}
	var cols []string
	for _, a := range assignments {
		mm := assignRe.FindStringSubmatch(a)
		if mm == nil {
			continue
		}
		col := mm[1]
		if _, dup := seen[col]; dup {
			continue
		}
		seen[col] = struct{}{}
		cols = append(cols, col)
	}
	sort.Strings(cols)
	return cols
}

// TestUpdateContactSQL_SetClauseIsExactlyProfileColumns parses contact.sql and
// asserts UpdateContact's SET clause is EXACTLY {cadence, full_name,
// profile_photo, updated_at} — an exact-set comparison, not a denylist (GI-4).
// A denylist naming only the five cadence columns would silently permit a
// knowledge column (e.g. `location = $5`) or any other future column; F6
// falsifies this by injecting exactly that and requiring this test to catch
// it where the old denylist did not.
func TestUpdateContactSQL_SetClauseIsExactlyProfileColumns(t *testing.T) {
	t.Parallel()
	require.Equal(t,
		[]string{"cadence", "full_name", "profile_photo", "updated_at"},
		updateContactSetClauseColumns(t),
	)
}

// TestCadenceSoleWriter_NegativeGuardCatchesNewWrite synthesizes a tiny
// Go file that calls r.queries.UpdateContactCadenceForward from an
// unallowlisted function, runs the same AST check against it, and
// asserts a violation is reported. Without this, a future loosening
// of the check (e.g., all-files-allowed) could silently pass.
//
// Synthesizes UpdateContactCadenceForward rather than UpdateContactMutualFields
// (the pre-PR7 choice): the latter left the inventory when the legacy cadence
// queries were deleted, and UpdateContactCadenceForward is a symbol that stays.
func TestCadenceSoleWriter_NegativeGuardCatchesNewWrite(t *testing.T) {
	t.Parallel()
	src := `package poc

type querier struct{}

func (q *querier) UpdateContactCadenceForward() {}

type poc struct {
	queries *querier
}

func (p *poc) FakeNewWriter() {
	p.queries.UpdateContactCadenceForward()
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
			if _, hit := derivedWritingSymbols[sel.Sel.Name]; !hit {
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
