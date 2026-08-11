//go:build integration_testdb

package tests

import (
	"context"
	"fmt"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// tieBreakTrioIDs are fixed, deliberately reverse-ordered UUIDs: byte order
// ascends aa < bb < cc, but seedTieBreakTrio inserts them in the opposite
// (descending) order. Recovering ascending order from that insertion order
// is only possible under the ORDER BY ladder's `c.id ASC` tiebreak rung —
// random ids would sometimes ascend by luck and make that falsifiable.
var tieBreakTrioIDs = []uuid.UUID{
	uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc"),
	uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"),
	uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"),
}

// seedTieBreakTrio inserts three contacts sharing one full name at the fixed
// ids above, in insertion order (descending). Every contact is expected to
// share its id with a person node: TestInsertContactAtID bypasses
// CreateContact's own node handling (it exists to attach a contact to a
// pre-existing node), so the person node is created first here to honor that
// invariant regardless of how it is presently enforced.
func seedTieBreakTrio(ctx context.Context, t *testing.T, database *db.Database, fullName string) {
	t.Helper()
	nodeRepo := repository.NewNodeRepository(database.Queries)
	supportRepo := repository.NewSyntheticSupportRepository(database.Queries)
	for _, id := range tieBreakTrioIDs {
		_, err := nodeRepo.CreateNode(ctx, id, repository.NodeTypePerson, fullName)
		require.NoError(t, err)
		require.NoError(t, supportRepo.InsertContactAtID(ctx, id, fullName))
	}
}

// TestContactList_DeterministicTieBreak pins CON-026: every contact list
// ordering — every named sort field in both directions, relevance-ranked
// search, and the no-sort default — includes a final unique tie-breaker
// (backend/internal/db/querygen/contact_list.sql.tmpl: `c.full_name ASC,
// c.id ASC`) so equal sort keys never paginate nondeterministically.
//
// Three contacts share one full name at fixed, reverse-ordered ids and are
// otherwise maximally sparse (every other column is NULL). That makes them
// tie under EVERY sort_field the ladder recognises — a NULL location
// coalesces to an empty string for all three, a NULL cadence falls to the same rank-7
// ELSE, NULL date columns tie under NULLS LAST, and identical full_name text
// produces an identical ts_rank score under relevance search — so the
// `c.id ASC` fallback rung is the only thing that can recover ascending
// order from the descending insertion order, under every axis this test
// walks.
func TestContactList_DeterministicTieBreak(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)
	contactRepo := repository.NewContactRepository(database.Queries)

	const tieBreakName = "Tiebreak Shared Name"
	seedTieBreakTrio(ctx, t, database, tieBreakName)

	// Ascending id order is the reverse of the descending insertion order in
	// tieBreakTrioIDs.
	want := []uuid.UUID{tieBreakTrioIDs[2], tieBreakTrioIDs[1], tieBreakTrioIDs[0]}

	// walk pages the clone (which holds only these three rows) at
	// page_limit=1 across three requests and returns the recovered id order.
	walk := func(params repository.ListContactsParams) []uuid.UUID {
		params.Limit = 1
		var got []uuid.UUID
		for offset := int32(0); offset < int32(len(tieBreakTrioIDs))+1; offset++ {
			params.Offset = offset
			page, err := contactRepo.ListContacts(ctx, params)
			require.NoError(t, err)
			if len(page) == 0 {
				break
			}
			require.Len(t, page, 1)
			got = append(got, page[0].ID)
		}
		return got
	}

	assertRecovers := func(t *testing.T, params repository.ListContactsParams) {
		t.Helper()
		got := walk(params)
		require.Equal(t, want, got,
			"id ASC tiebreak must recover ascending order from descending insertion order, with no duplicate and no omission")

		again := walk(params)
		require.Equal(t, got, again, "repeated walk must be byte-identical")
	}

	sortFields := []string{"", "name", "location", "birthday", "last_contacted", "last_response_at", "contact_by", "cadence"}
	sortOrders := []string{"asc", "desc"}
	for _, sortField := range sortFields {
		label := sortField
		if label == "" {
			label = "default"
		}
		for _, sortOrder := range sortOrders {
			t.Run(fmt.Sprintf("sort_field_%s/order_%s", label, sortOrder), func(t *testing.T) {
				assertRecovers(t, repository.ListContactsParams{Sort: sortField, Order: sortOrder})
			})
		}
	}

	t.Run("relevance_ranked_search", func(t *testing.T) {
		// All three rows carry identical full_name text and no contact_method
		// rows, so ts_rank scores identically for all three under any search
		// term the name contains — the relevance CASE ties too, falling
		// through to the same id ASC fallback rung.
		assertRecovers(t, repository.ListContactsParams{Query: "Tiebreak"})
	})
}

// TestContactList_TripleParity locks ListContacts/CountContacts/ListContactIDs
// as one generated shape (CON-026): for every combination of filters and
// sort, the three queries must agree on which rows match and in what order.
//
// The fixture is a full factorial over three binary axes (cadence set,
// live followup_loop task, full_name matches the search term) rather than
// axes satisfied independently: an independently-satisfied fixture lets a
// filter intersection return empty from all three queries, which is a
// vacuous pass, not a real one.
func TestContactList_TripleParity(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)
	contactRepo := repository.NewContactRepository(database.Queries)
	taskRepo := repository.NewContactTaskRepository(database.Queries)
	gen, _ := migrationGenerator(t)

	const searchTerm = "zqxparity"
	const paddingName = "Padding Shared Name"

	type factorialCell struct {
		cadence  bool
		followup bool
		search   bool
	}
	var cells []factorialCell
	for _, cadence := range []bool{false, true} {
		for _, followup := range []bool{false, true} {
			for _, search := range []bool{false, true} {
				cells = append(cells, factorialCell{cadence, followup, search})
			}
		}
	}
	require.Len(t, cells, 8, "the fixture must be a full factorial over the three binary axes")

	for _, c := range cells {
		// The cadence and search axes are ordinary ContactSpec shape, so they
		// go through the synthetic factory (migrationGenerator/factory.Contact)
		// like every other repo-test contact fixture. The followup axis does
		// not: no ContactOption creates a contact_task — a followup_loop row is
		// a downstream FollowUpManager side effect of a real outbound
		// interaction in production, and replaying that here would trade a
		// direct, deterministic CreateContactTask call for an async
		// settle-gated pipeline, for a test whose subject is SQL query-shape
		// parity, not follow-up creation. That axis stays hand-rolled through
		// ContactTaskRepository.
		var opts []factory.ContactOption
		if c.cadence {
			opts = append(opts, factory.WithCadence("weekly"))
		}
		if c.search {
			opts = append(opts, factory.WithNameMarker(searchTerm))
		}
		contact, _ := seedMigrationContact(ctx, t, database, gen, opts...)

		if c.followup {
			_, err := taskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
				ContactID: contact.ID,
				Provider:  "todoist",
				Kind:      "reach_out",
				Lifecycle: "followup_loop",
				State:     "managed",
			})
			require.NoError(t, err)
		}
	}

	// The tie-break trio's own fixture, folded into this matrix too: a
	// genuine full_name collision must not break parity between the three
	// queries at any cell. Named distinctly from searchTerm so it never
	// perturbs the search axis.
	seedTieBreakTrio(ctx, t, database, paddingName)

	cadenceFilters := []string{"", "has_cadence", "no_cadence"}
	followupFilters := []string{"", "has_followup", "no_followup"}
	// search_query: the seeded term, or "" which repository.ListContactsParams
	// maps to NULL (no search).
	searchQueries := []string{searchTerm, ""}
	// sort_field, read off the ORDER BY ladder in
	// backend/internal/db/querygen/contact_list.sql.tmpl (the authority — the
	// three list queries, and their shared ORDER BY ladder, no longer live in
	// contact.sql). No last_interaction_at/last_outreach_at rung: those
	// columns exist on contact but the list queries do not sort on them, and a
	// sort_field the SQL does not recognise falls through to the default rung
	// — adding them here would silently duplicate cells.
	sortFields := []string{"", "name", "location", "birthday", "last_contacted", "last_response_at", "contact_by", "cadence"}
	sortOrders := []string{"asc", "desc"}

	// Every (cadence_filter, followup_filter, search_query) combination must
	// be satisfiable against this fixture — the full factorial guarantees it.
	// Assert non-emptiness once per filter triple, before the parity walk
	// below, so a three-way implementation that vacuously agrees on an empty
	// result set cannot pass.
	for _, cadenceFilter := range cadenceFilters {
		for _, followupFilter := range followupFilters {
			for _, searchQuery := range searchQueries {
				total, err := contactRepo.CountContacts(ctx, repository.ListContactsParams{
					CadenceFilter:  cadenceFilter,
					FollowupFilter: followupFilter,
					Query:          searchQuery,
				})
				require.NoError(t, err)
				require.Greater(t, total, int64(0),
					"filter combo cadence=%q followup=%q search=%q must be satisfiable by the fixture, or the fixture is not a real factorial",
					cadenceFilter, followupFilter, searchQuery)
			}
		}
	}

	// pageAll pages ListContacts to exhaustion at a page size smaller than the
	// fixture (11 rows), returning the concatenated id sequence.
	pageAll := func(params repository.ListContactsParams) []uuid.UUID {
		params.Limit = 3
		var ids []uuid.UUID
		for offset := int32(0); ; offset += params.Limit {
			params.Offset = offset
			page, err := contactRepo.ListContacts(ctx, params)
			require.NoError(t, err)
			for _, c := range page {
				ids = append(ids, c.ID)
			}
			if int32(len(page)) < params.Limit {
				break
			}
		}
		return ids
	}

	cellsWalked := 0
	for _, cadenceFilter := range cadenceFilters {
		for _, followupFilter := range followupFilters {
			for _, searchQuery := range searchQueries {
				for _, sortField := range sortFields {
					for _, sortOrder := range sortOrders {
						cellsWalked++

						total, err := contactRepo.CountContacts(ctx, repository.ListContactsParams{
							CadenceFilter:  cadenceFilter,
							FollowupFilter: followupFilter,
							Query:          searchQuery,
						})
						require.NoError(t, err)

						ids, err := contactRepo.ListContactIDs(ctx, repository.ListContactIDsParams{
							Sort:           sortField,
							Order:          sortOrder,
							Search:         searchQuery,
							CadenceFilter:  cadenceFilter,
							FollowupFilter: followupFilter,
						})
						require.NoError(t, err)
						require.Equal(t, total, int64(len(ids)),
							"CountContacts must equal len(ListContactIDs) at cadence=%q followup=%q search=%q sort=%q order=%q",
							cadenceFilter, followupFilter, searchQuery, sortField, sortOrder)

						rowsIDs := pageAll(repository.ListContactsParams{
							CadenceFilter:  cadenceFilter,
							FollowupFilter: followupFilter,
							Query:          searchQuery,
							Sort:           sortField,
							Order:          sortOrder,
						})
						require.Equal(t, ids, rowsIDs,
							"ListContacts paged to exhaustion must return exactly the ListContactIDs sequence at cadence=%q followup=%q search=%q sort=%q order=%q",
							cadenceFilter, followupFilter, searchQuery, sortField, sortOrder)

						if sortField == "" {
							// The default rung ignores sort_order entirely:
							// both values must return the identical sequence.
							otherOrder := "desc"
							if sortOrder == "desc" {
								otherOrder = "asc"
							}
							otherIDs, err := contactRepo.ListContactIDs(ctx, repository.ListContactIDsParams{
								Sort:           sortField,
								Order:          otherOrder,
								Search:         searchQuery,
								CadenceFilter:  cadenceFilter,
								FollowupFilter: followupFilter,
							})
							require.NoError(t, err)
							require.Equal(t, ids, otherIDs,
								"sort_field='' must ignore sort_order entirely, at cadence=%q followup=%q search=%q",
								cadenceFilter, followupFilter, searchQuery)
						}
					}
				}
			}
		}
	}
	require.Equal(t, len(cadenceFilters)*len(followupFilters)*len(searchQueries)*len(sortFields)*len(sortOrders), cellsWalked)
}
