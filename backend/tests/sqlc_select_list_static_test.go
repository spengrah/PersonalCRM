// Package tests — sqlc source-SQL projection-duplication lint.
//
// Enforces the convention that full-row reads use SELECT * rather than a
// hand-maintained explicit column list. A repeated multi-column explicit
// projection of the same table is the duplication smell this lints against:
// it means humans are maintaining parallel column lists by hand, and adding a
// column requires editing every copy in lockstep (the exact maintenance cost
// the convention eliminates). SELECT * delegates that expansion to sqlc's
// codegen, so a new column flows to every full-row query for free.
//
// Scope: this analyzer reads ONLY the SOURCE query files in
// backend/internal/db/queries/*.sql — never the generated *.sql.go. sqlc
// expands SELECT * into a flat explicit column list at codegen time, so the
// generated files are full of identical column lists that are harmless
// auto-generated output. Humans edit the .sql source; that is where the
// duplication cost lives and where the lint belongs.
//
// What trips it: an identical explicit (non-*) SELECT projection of N >= 3
// columns of the same table appearing in 2+ queries, unless allowlisted. A
// single explicit projection is fine (a genuine one-off narrow projection).
//
// Limitation (v1): only the outermost/first SELECT projection of each query is
// considered; projections inside subqueries/CTEs are out of scope. The
// companion integration test (telegram_message_all_fields_test.go) catches the
// downstream consequence — a dropped column read back as zero — regardless of
// how the SELECT was written, so the two layers are complementary.
//
// This is the Go counterpart (the authoritative parser) to the grep guard at
// scripts/ci/sqlc-select-list-guard.sh, mirroring the belt-and-suspenders
// convention of the sibling static guards.
package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// allowedDuplicateProjections maps a duplicate-projection fingerprint to its
// justification. A fingerprint is "<table>|<normalized-col-list>". Entries here
// are deliberate narrow projections that legitimately repeat and must NOT be
// converted to SELECT *.
var allowedDuplicateProjections = map[string]string{
	// GetOAuthCredentialStatus + ListOAuthCredentialStatuses deliberately omit
	// the encrypted-token columns (access_token_encrypted,
	// refresh_token_encrypted, encryption_nonce, token_type). Converting to
	// SELECT * would start selecting secret material — a security regression.
	"oauth_credential|account_id, account_name, created_at, expires_at, id, provider, scopes, updated_at": "intentional narrow projection excluding encrypted token columns; SELECT * would leak secrets",
}

// selectProjection holds one query's extracted SELECT projection.
type selectProjection struct {
	queryName string
	file      string
	table     string
	// normalized is the column list with whitespace collapsed, lowercased, and
	// columns sorted, so two lists with the same columns in a different order
	// or spacing fingerprint identically.
	normalized string
	numColumns int
}

var (
	// Splits a query block's leading marker line: "-- name: Foo :one".
	queryNameRe = regexp.MustCompile(`(?m)^--\s*name:\s*(\S+)`)
	// Captures the first SELECT ... FROM <table>. The projection is captured
	// non-greedily up to the first top-level FROM; depth handling below rejects
	// matches where the FROM was inside parentheses.
	selectFromRe = regexp.MustCompile(`(?is)\bSELECT\b(.*?)\bFROM\b\s+([a-z_][a-z0-9_]*)`)
)

// TestNoDuplicatedFullRowSelectLists walks backend/internal/db/queries/*.sql,
// extracts each query's top-level SELECT projection, and fails if any identical
// explicit (non-*) >=3-column projection of the same table appears in 2+
// queries unless allowlisted. Runs DB-free (pure file parsing) so it executes
// under make test-unit and -short.
func TestNoDuplicatedFullRowSelectLists(t *testing.T) {
	moduleRoot, err := backendModuleRoot()
	if err != nil {
		t.Fatalf("locate backend module root: %v", err)
	}
	queriesDir := filepath.Join(moduleRoot, "internal", "db", "queries")

	files, err := filepath.Glob(filepath.Join(queriesDir, "*.sql"))
	if err != nil {
		t.Fatalf("glob query files: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no .sql query files found under %s", queriesDir)
	}
	sort.Strings(files)

	// fingerprint -> projections that share it.
	byFingerprint := map[string][]selectProjection{}
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		rel := filepath.Base(file)
		for _, proj := range extractSelectProjections(rel, string(content)) {
			if proj.numColumns < 3 {
				continue
			}
			fp := proj.table + "|" + proj.normalized
			byFingerprint[fp] = append(byFingerprint[fp], proj)
		}
	}

	var violations []string
	for fp, projs := range byFingerprint {
		if len(projs) < 2 {
			continue
		}
		if _, ok := allowedDuplicateProjections[fp]; ok {
			continue
		}
		sort.Slice(projs, func(i, j int) bool {
			if projs[i].file != projs[j].file {
				return projs[i].file < projs[j].file
			}
			return projs[i].queryName < projs[j].queryName
		})
		names := make([]string, len(projs))
		for i, p := range projs {
			names[i] = p.file + ":" + p.queryName
		}
		violations = append(violations,
			"  table "+projs[0].table+" — identical "+itoa(projs[0].numColumns)+
				"-column explicit projection in: "+strings.Join(names, ", ")+
				"\n    columns: "+projs[0].normalized)
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("duplicated explicit SELECT column list(s) found in source SQL.\n"+
			"Use SELECT * for full-row reads (sqlc expands it; adding a column needs no query edit),\n"+
			"or, for a deliberate narrow projection, add a justified entry to allowedDuplicateProjections.\n\n%s",
			strings.Join(violations, "\n"))
	}
}

// extractSelectProjections splits the file into query blocks on -- name:
// markers and extracts the top-level SELECT projection from each block whose
// query is a SELECT. Blocks whose projection is *, DISTINCT ON, a bare
// DISTINCT, aggregate-only, or a SELECT 1 existence probe are skipped (they are
// not hand-maintained full-row column lists). Exported for the negative
// self-test so the detector and its test can never drift.
func extractSelectProjections(file, content string) []selectProjection {
	markers := queryNameRe.FindAllStringSubmatchIndex(content, -1)
	if len(markers) == 0 {
		return nil
	}
	var out []selectProjection
	for i, m := range markers {
		name := content[m[2]:m[3]]
		blockStart := m[0]
		blockEnd := len(content)
		if i+1 < len(markers) {
			blockEnd = markers[i+1][0]
		}
		block := content[blockStart:blockEnd]
		proj, ok := parseProjection(block)
		if !ok {
			continue
		}
		proj.queryName = name
		proj.file = file
		out = append(out, proj)
	}
	return out
}

// parseProjection extracts the top-level SELECT projection from a single query
// block. Returns ok=false for blocks that are not a parseable full-row-style
// explicit projection (non-SELECT, SELECT *, DISTINCT, aggregate-only,
// existence probe, or a FROM nested inside parentheses).
func parseProjection(block string) (selectProjection, bool) {
	match := selectFromRe.FindStringSubmatch(block)
	if match == nil {
		return selectProjection{}, false
	}
	projection := strings.TrimSpace(match[1])
	table := strings.ToLower(match[2])

	// Reject when the captured SELECT...FROM straddles a parenthesis boundary
	// (e.g. SELECT ... (SELECT x FROM y) ...): the projection segment must be
	// balanced, otherwise the FROM we matched belongs to a subquery, not the
	// outer SELECT.
	if !parensBalanced(projection) {
		return selectProjection{}, false
	}

	lower := strings.ToLower(projection)
	switch {
	case projection == "*":
		return selectProjection{}, false
	case strings.HasPrefix(lower, "distinct on"):
		// DISTINCT ON (...) col, col — distinct-row reads, not a full-row list.
		return selectProjection{}, false
	case strings.HasPrefix(lower, "distinct"):
		return selectProjection{}, false
	case lower == "1" || lower == "1 ":
		return selectProjection{}, false
	}

	cols := splitTopLevel(projection)
	// An aggregate-only / expression projection (every term contains a call or
	// is the bare * ) is not a hand-maintained column list. We only flag plain
	// bare-identifier column lists; if any column is a bare identifier we still
	// fingerprint, but skip if the projection has zero plain columns.
	if !hasPlainColumn(cols) {
		return selectProjection{}, false
	}

	normalized := normalizeColumns(cols)
	return selectProjection{
		table:      table,
		normalized: normalized,
		numColumns: len(cols),
	}, true
}

// hasPlainColumn reports whether at least one column is a bare identifier
// (no function call, no AS alias, no *). A projection consisting entirely of
// aggregates/expressions (COUNT(*), MAX(x), ...) is not a column list.
func hasPlainColumn(cols []string) bool {
	for _, c := range cols {
		c = strings.TrimSpace(strings.ToLower(c))
		if c == "" || c == "*" {
			continue
		}
		if strings.ContainsAny(c, "(") {
			continue
		}
		if strings.Contains(c, " as ") {
			continue
		}
		// A bare identifier (possibly schema-qualified) with no spaces.
		if !strings.ContainsAny(c, " \t") {
			return true
		}
	}
	return false
}

// normalizeColumns lowercases, trims, and sorts the columns so two lists with
// the same members fingerprint identically regardless of order or spacing.
func normalizeColumns(cols []string) string {
	norm := make([]string, 0, len(cols))
	for _, c := range cols {
		c = strings.TrimSpace(strings.ToLower(c))
		c = strings.Join(strings.Fields(c), " ")
		if c == "" {
			continue
		}
		norm = append(norm, c)
	}
	sort.Strings(norm)
	return strings.Join(norm, ", ")
}

// splitTopLevel splits a projection on commas that are not inside parentheses.
func splitTopLevel(s string) []string {
	var parts []string
	depth := 0
	start := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	// Drop trailing empties.
	out := parts[:0]
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// parensBalanced reports whether s has matched parentheses.
func parensBalanced(s string) bool {
	depth := 0
	for _, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// itoa is a tiny strconv.Itoa to keep the import list minimal.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestSelectListDetector_FlagsSyntheticDuplicate feeds the SAME analyzer the
// real test uses a synthetic file containing an identical 3-column explicit
// projection in two queries, and asserts the detector reports both — proving
// the detector is live (not a no-op). A second case proves SELECT * is NOT
// flagged.
func TestSelectListDetector_FlagsSyntheticDuplicate(t *testing.T) {
	const dupSrc = `-- name: GetWidgetA :one
SELECT alpha, beta, gamma FROM widget WHERE id = $1;

-- name: GetWidgetB :one
SELECT alpha, beta, gamma FROM widget WHERE name = $1;
`
	projs := extractSelectProjections("synthetic.sql", dupSrc)
	if len(projs) != 2 {
		t.Fatalf("expected 2 projections from synthetic duplicate, got %d: %+v", len(projs), projs)
	}
	fp := func(p selectProjection) string { return p.table + "|" + p.normalized }
	if projs[0].numColumns != 3 {
		t.Errorf("expected 3 columns, got %d", projs[0].numColumns)
	}
	if fp(projs[0]) != fp(projs[1]) {
		t.Errorf("expected identical fingerprints, got %q vs %q", fp(projs[0]), fp(projs[1]))
	}

	const starSrc = `-- name: GetWidgetStarA :one
SELECT * FROM widget WHERE id = $1;

-- name: GetWidgetStarB :one
SELECT * FROM widget WHERE name = $1;
`
	starProjs := extractSelectProjections("synthetic_star.sql", starSrc)
	if len(starProjs) != 0 {
		t.Errorf("SELECT * must not be extracted as an explicit projection, got %d: %+v", len(starProjs), starProjs)
	}
}

// TestSelectListDetector_SkipsNonFullRowShapes asserts the shapes that must NOT
// be flagged are correctly skipped: DISTINCT ON, aggregate-only, SELECT 1,
// and single-occurrence narrow projections (a lone explicit projection is
// fine; only 2+ identical ones are the smell).
func TestSelectListDetector_SkipsNonFullRowShapes(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "distinct on",
			src: `-- name: GetDistinct :many
SELECT DISTINCT ON (peer_id) peer_id, a, b, c FROM widget;`,
		},
		{
			name: "aggregate only",
			src: `-- name: CountThings :one
SELECT COUNT(*) as total, MAX(sent_at) as last, MIN(sent_at) as first FROM widget;`,
		},
		{
			name: "existence probe",
			src: `-- name: Probe :one
SELECT 1 FROM widget WHERE id = $1;`,
		},
		{
			name: "star",
			src: `-- name: GetAll :one
SELECT * FROM widget WHERE id = $1;`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			projs := extractSelectProjections("synthetic.sql", tc.src)
			if len(projs) != 0 {
				t.Errorf("%s must be skipped, but detector extracted: %+v", tc.name, projs)
			}
		})
	}
}
