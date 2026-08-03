//go:build integration_testdb

package tests

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Index-definition coverage for migration 073's two partial indexes on
// comms_message. An index is only an access path: a wrong-but-narrower predicate
// or a wrong column order ships green through the query-correctness suite (the
// planner just declines to use the index). So this test reads Postgres's own
// deterministic indexdef reconstruction and asserts the EXACT key-column list
// (order-sensitive) and the EXACT set of partial-predicate conjuncts
// (order-insensitive but rejecting any extra/missing conjunct). It READS only
// the catalog, so it runs against the shared package DB — no isolated clone.

// usingKeyColumns parses the key-column list out of a pg_indexes.indexdef string
// (the "(col, col, ...)" that follows "USING <method>"). Returns the columns
// trimmed, in index order.
func usingKeyColumns(t *testing.T, def string) []string {
	t.Helper()
	m := regexp.MustCompile(`USING \w+ \(([^)]+)\)`).FindStringSubmatch(def)
	require.Lenf(t, m, 2, "could not parse key columns from indexdef: %s", def)
	parts := strings.Split(m[1], ",")
	cols := make([]string, len(parts))
	for i, p := range parts {
		cols[i] = strings.TrimSpace(p)
	}
	return cols
}

// predicateConjuncts parses the partial WHERE predicate out of an indexdef and
// returns its flat list of conjuncts, normalized. Postgres renders partial
// predicates with per-clause parens, e.g.
//
//	WHERE ((processed_at IS NULL) AND (claimed_at IS NULL) AND (deleted_at IS NULL))
//
// so we lowercase, strip ALL parens (the predicate is a flat conjunction with no
// grouping that changes meaning), collapse whitespace, then split on " and ".
// The caller compares the result as a set, which rejects any extra or missing
// conjunct (e.g. a stray "source = 'gchat'", or a MISSING
// "matched_contact_id IS NOT NULL" — 073 omitted that conjunct because the
// column was NOT NULL, and 076 added it back when the column became nullable,
// so an unmatched WhatsApp row drops out of the eligible indexes instead of
// sitting in them permanently).
func predicateConjuncts(t *testing.T, def string) []string {
	t.Helper()
	parts := strings.SplitN(def, " WHERE ", 2)
	require.Lenf(t, parts, 2, "indexdef has no WHERE clause (expected a partial index): %s", def)
	pred := strings.ToLower(parts[1])
	pred = strings.NewReplacer("(", " ", ")", " ").Replace(pred)
	pred = strings.Join(strings.Fields(pred), " ") // collapse whitespace
	conj := strings.Split(pred, " and ")
	for i := range conj {
		conj[i] = strings.TrimSpace(conj[i])
	}
	return conj
}

func TestCommsMessageEligibleIndexes_Definitions(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	database, ctx := graphTestDB(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	defs, err := support.ListCommsIndexDefs(ctx)
	require.NoError(t, err)

	cases := []struct {
		name string
		cols []string // exact key columns, in order
		pred []string // exact set of normalized predicate conjuncts
	}{
		{
			name: "idx_comms_message_unprocessed_eligible",
			cols: []string{"source", "matched_contact_id", "sent_at"},
			pred: []string{"processed_at is null", "claimed_at is null", "deleted_at is null", "matched_contact_id is not null"},
		},
		{
			name: "idx_comms_message_stale_claim",
			cols: []string{"source", "matched_contact_id", "claimed_at"},
			pred: []string{"processed_at is null", "claimed_at is not null", "deleted_at is null", "matched_contact_id is not null"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			def, ok := defs[tc.name]
			require.Truef(t, ok, "index %s not found on comms_message; have %v", tc.name, indexNames(defs))

			// Key columns: exact list AND order — a wrong/missing/extra/reordered
			// column fails.
			assert.Equal(t, tc.cols, usingKeyColumns(t, def), "key columns (order-sensitive)")

			// Partial predicate: exact set of conjuncts — any extra (e.g. a
			// source restriction), missing (e.g. 076's
			// matched_contact_id IS NOT NULL), or altered conjunct fails.
			// ElementsMatch is exact set membership, not substring.
			assert.ElementsMatch(t, tc.pred, predicateConjuncts(t, def), "partial predicate conjuncts (exact set)")
		})
	}
}

// indexNames returns the sorted index names present, for a readable failure
// message when an expected index is missing.
func indexNames(defs map[string]string) []string {
	names := make([]string, 0, len(defs))
	for name := range defs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
