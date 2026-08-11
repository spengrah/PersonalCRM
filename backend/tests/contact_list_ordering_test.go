//go:build integration_testdb

package tests

import (
	"context"
	"fmt"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

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
// ids above, in insertion order (descending). PR5 adds contact_id_node_fk,
// after which a contact insert with no node fails; TestInsertContactAtID
// bypasses CreateContact's own node handling (it exists to attach a contact
// to a pre-existing node), so the person node is created first here — this
// keeps the fixture green when PR5 lands in wave C.
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

// TestContactList_DeterministicTieBreak pins CON-026's default fallback +
// tiebreaker rung (contact.sql: "name asc then id, so ... equal sort keys
// never paginate nondeterministically"). Three contacts share one full name
// at fixed, reverse-ordered ids; only the `c.id ASC` tiebreak rung can
// recover ascending order from descending insertion order.
func TestContactList_DeterministicTieBreak(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

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

	for _, tc := range []struct {
		name   string
		params repository.ListContactsParams
	}{
		{"no_sort_default", repository.ListContactsParams{}},
		{"sort_field_name", repository.ListContactsParams{Sort: "name", Order: "asc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := walk(tc.params)
			require.Equal(t, want, got,
				"id ASC tiebreak must recover ascending order from descending insertion order, with no duplicate and no omission")

			again := walk(tc.params)
			require.Equal(t, got, again, "repeated walk must be byte-identical")
		})
	}
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

	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)
	contactRepo := repository.NewContactRepository(database.Queries)
	taskRepo := repository.NewContactTaskRepository(database.Queries)

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

	for i, c := range cells {
		name := fmt.Sprintf("Factorial Contact %02d", i)
		if c.search {
			name += " " + searchTerm
		}
		var cadencePtr *string
		if c.cadence {
			weekly := "weekly"
			cadencePtr = &weekly
		}
		contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: name,
			Cadence:  cadencePtr,
		})
		require.NoError(t, err)

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
	// sort_field, read off the ORDER BY ladder at contact.sql:35-64 (the
	// authority). No last_interaction_at/last_outreach_at rung: those columns
	// exist on contact but the list queries do not sort on them, and a
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
