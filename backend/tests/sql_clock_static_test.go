// Package tests — SQL clock convention guard.
//
// Domain time comes from the caller: a query takes the timestamp as a
// parameter and the Go caller supplies accelerated.GetCurrentTime(). A query
// may read the database clock (NOW(), CURRENT_TIMESTAMP, ...) only for an
// infrastructure column listed in infraClockColumns, because under time
// acceleration a DB-clock value and an accelerated-clock value are not
// comparable.
//
// The test strips `--` comments from every backend/internal/db/queries/*.sql
// file, finds each clock function call, resolves the column it is assigned to
// or compared against (including INSERT ... VALUES position), and fails on any
// column not in the allowlist.
package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// infraClockColumns are the columns a query may stamp from, or compare to, the
// database clock. Keys are "column" (any table) or "table.column". Each entry
// states why the value is bookkeeping and not domain time.
var infraClockColumns = map[string]string{
	// Row audit stamps: no Go or SQL reader compares them to accelerated time
	// (Go readers only copy them into API structs; SQL only tests IS [NOT] NULL).
	"updated_at":         "row audit stamp; readers only copy it into API responses",
	"deleted_at":         "soft-delete tombstone; readers only test deleted_at IS [NOT] NULL, no age-based purge",
	"computed_at":        "audit stamp on embedding and relationship_signal rows; readers only copy it into structs",
	"enriched_at":        "audit stamp on contact_enrichment rows; readers copy it and ORDER BY it within one clock",
	"processed_at":       "ingest workflow flag; readers test processed_at IS NULL and copy it into structs",
	"consumed_at":        "mac_host_pairing_token single-use flag; services only test consumed_at != nil",
	"api_key_revoked_at": "mac_host revocation flag; readers only test it for nil",
	"api_key_rotated_at": "mac_host rotation audit stamp; readers only copy it into structs",
	// Leases: stamped and compared against the DB clock in the same statement
	// family, so the TTL never mixes clocks. Go readers only copy the column.
	"claimed_at": "ingest and whatsapp claim lease; stamped and TTL-compared on the DB clock inside the claim queries",
	// Recording order: the correction recompute (CAD-047) orders a user's due
	// date set directly against interaction.recorded_at, which a trigger stamps
	// from the DB clock. They are compared only with each other, never with
	// accelerated time.
	"contact.contact_by_direct_set_at": "recording-order stamp; compared only with interaction.recorded_at (DB clock)",
	// River's own table: River runs on the real clock (no TimeGenerator stub in
	// this repo), so fixtures must use the DB clock to match its fetch query.
	"river_job.created_at":   "River fixture row; River fetches on the real clock",
	"river_job.scheduled_at": "River fixture row; River fetches on the real clock",
	"river_job.finalized_at": "River fixture row; River fetches on the real clock",
}

// sqlClockBaseline lists queries that read the DB clock for a domain column
// today, keyed "queryfile:QueryName". The rule is forward-only: a query not
// listed here fails, and a listed query with no remaining violation fails, so
// the baseline only shrinks. Fix a query by taking the timestamp as a
// parameter, then delete its entry.
var sqlClockBaseline = map[string]bool{
	"external_sync.sql:UpdateSyncStateStatus":     true,
	"external_sync.sql:UpdateSyncStateSuccess":    true,
	"external_sync.sql:MarkSyncStateTerminal":     true,
	"mac_host.sql:UpdateMacHostHeartbeat":         true,
	"mac_host.sql:DeleteExpiredPairingTokens":     true,
	"test.sql:TestInsertRelationshipSignalMarker": true,
}

var (
	sqlClockCallRe = regexp.MustCompile(`(?i)\b(?:now\s*\(\s*\)|current_timestamp|clock_timestamp\s*\(\s*\)|statement_timestamp\s*\(\s*\)|transaction_timestamp\s*\(\s*\)|localtimestamp)`)
	sqlNameRe      = regexp.MustCompile(`(?m)^-- name: (\w+)`)
	sqlLineCmtRe   = regexp.MustCompile(`--[^\n]*`)
	sqlStringRe    = regexp.MustCompile(`'(?:[^']|'')*'`)
	sqlCompareRe   = regexp.MustCompile(`(\w+)\s*(?:=|<>|!=|<=|>=|<|>)\s*$`)
	sqlValuesRe    = regexp.MustCompile(`(?i)\bvalues\b`)
	sqlInsertColRe = regexp.MustCompile(`(?is)insert\s+into\s+\w+\s*\(([^)]*)\)\s*$`)
	sqlTableRe     = regexp.MustCompile(`(?i)\b(?:insert\s+into|update|delete\s+from|from)\s+(\w+)`)
)

// sqlClockColumn returns the column a clock call at pos in stmt is assigned
// to or compared against, or "?" when it cannot be resolved.
func sqlClockColumn(stmt string, pos int) string {
	before := stmt[:pos]
	if m := sqlCompareRe.FindStringSubmatch(before); m != nil {
		return m[1]
	}
	locs := sqlValuesRe.FindAllStringIndex(before, -1)
	if len(locs) == 0 {
		return "?"
	}
	valuesAt := locs[len(locs)-1][0]
	cols := sqlInsertColRe.FindStringSubmatch(before[:valuesAt])
	open := strings.Index(before[valuesAt:], "(")
	if cols == nil || open < 0 {
		return "?"
	}
	depth, idx := 0, 0
	for _, r := range before[valuesAt+open:] {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 1 {
				idx++
			}
		}
	}
	names := strings.Split(cols[1], ",")
	if depth < 1 || idx >= len(names) {
		return "?"
	}
	return strings.TrimSpace(names[idx])
}

func TestSQLQueries_ClockOnlyForInfrastructureColumns(t *testing.T) {
	root, err := backendModuleRoot()
	require.NoError(t, err)
	files, err := filepath.Glob(filepath.Join(root, "internal", "db", "queries", "*.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	used := map[string]bool{}
	var violations []string
	baselineUsed := map[string]bool{}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		require.NoError(t, err)
		// Blank comments and string literals in place so offsets and line
		// numbers stay valid and a literal like '%now()%' is not a clock call.
		blank := func(c string) string { return strings.Repeat(" ", len(c)) }
		text := sqlLineCmtRe.ReplaceAllStringFunc(string(raw), blank)
		text = sqlStringRe.ReplaceAllStringFunc(text, blank)
		heads := sqlNameRe.FindAllStringSubmatchIndex(string(raw), -1)
		for i, h := range heads {
			end := len(text)
			if i+1 < len(heads) {
				end = heads[i+1][0]
			}
			name, stmt := string(raw)[h[2]:h[3]], text[h[0]:end]
			table := ""
			if m := sqlTableRe.FindStringSubmatch(stmt); m != nil {
				table = m[1]
			}
			for _, loc := range sqlClockCallRe.FindAllStringIndex(stmt, -1) {
				col := sqlClockColumn(stmt, loc[0])
				key := table + "." + col
				if _, ok := infraClockColumns[key]; ok && col != "?" {
					used[key] = true
					continue
				}
				if _, ok := infraClockColumns[col]; ok && col != "?" {
					used[col] = true
					continue
				}
				baseKey := filepath.Base(file) + ":" + name
				if sqlClockBaseline[baseKey] {
					baselineUsed[baseKey] = true
					continue
				}
				line := 1 + strings.Count(text[:h[0]+loc[0]], "\n")
				violations = append(violations, fmt.Sprintf("%s:%d %s: %s.%s uses the DB clock",
					filepath.ToSlash(strings.TrimPrefix(file, root+string(filepath.Separator))), line, name, table, col))
			}
		}
	}

	var stale []string
	for key := range infraClockColumns {
		if !used[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	require.Empty(t, stale, "infraClockColumns entries no query uses; delete them")
	var staleBaseline []string
	for key := range sqlClockBaseline {
		if !baselineUsed[key] {
			staleBaseline = append(staleBaseline, key)
		}
	}
	sort.Strings(staleBaseline)
	require.Empty(t, staleBaseline, "sqlClockBaseline entries whose query no longer reads the DB clock for a domain column; delete them")
	require.Empty(t, violations, "SQL reads the DB clock for a non-infrastructure column.\n"+
		"Take the timestamp as a query parameter and pass accelerated.GetCurrentTime() from the caller.\n"+
		"If the column is infrastructure bookkeeping, add it to infraClockColumns with a justification.\n%s",
		strings.Join(violations, "\n"))
}
