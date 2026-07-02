package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestContactListUnified_Integration locks the unified contact-list contract:
// one WHERE + ORDER BY shape shared by the rows, IDs, and count queries, with
// a deterministic default order (name asc, id asc) when no sort is requested.
func TestContactListUnified_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Parallel()
	// Migrations are applied once by TestMain.
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	repo := repository.NewContactRepository(database.Queries)
	gen, ns := migrationGenerator(t)

	sharedBirthday := time.Date(1990, time.March, 14, 0, 0, 0, 0, time.UTC)

	// Six namespaced contacts covering the filter/sort dimensions: cadence
	// present/absent, shared birthday (tiebreaker), and email methods (FTS
	// corpus includes method values).
	seeded := make(map[uuid.UUID]*repository.Contact)
	for _, opts := range [][]factory.ContactOption{
		{factory.WithCadence("weekly"), factory.WithEmail()},
		{factory.WithCadence("monthly")},
		{factory.WithBirthday(sharedBirthday)},
		{factory.WithBirthday(sharedBirthday)},
		{factory.WithEmail()},
		{},
	} {
		contact, cleanup := seedMigrationContact(ctx, t, database, gen, opts...)
		defer cleanup()
		seeded[contact.ID] = contact
	}

	// scopedIDs filters a rows result down to this namespace's contacts,
	// preserving order.
	scopedIDs := func(contacts []repository.Contact) []uuid.UUID {
		var ids []uuid.UUID
		for _, c := range contacts {
			if _, ok := seeded[c.ID]; ok {
				ids = append(ids, c.ID)
			}
		}
		return ids
	}
	scopedIDList := func(all []uuid.UUID) []uuid.UUID {
		var ids []uuid.UUID
		for _, id := range all {
			if _, ok := seeded[id]; ok {
				ids = append(ids, id)
			}
		}
		return ids
	}

	listAll := func(params repository.ListContactsParams) []repository.Contact {
		params.Limit = 100000
		contacts, err := repo.ListContacts(ctx, params)
		require.NoError(t, err)
		return contacts
	}

	t.Run("DefaultOrderIsNameAscending", func(t *testing.T) {
		// The unsorted list must be deterministic: identical to an explicit
		// name-asc sort (Postgres computes both orderings, so this assertion
		// is collation-proof).
		unsorted := scopedIDs(listAll(repository.ListContactsParams{}))
		byName := scopedIDs(listAll(repository.ListContactsParams{Sort: "name", Order: "asc"}))
		require.Len(t, unsorted, len(seeded))
		assert.Equal(t, byName, unsorted, "unsorted list must default to name-asc order")
	})

	t.Run("DefaultOrderPaginatesConsistently", func(t *testing.T) {
		// Page through with no sort using a page size of 1 against the full
		// table; the concatenation must contain this namespace's contacts in
		// the same order as the one-shot read, with no duplicates or gaps.
		oneShot := scopedIDs(listAll(repository.ListContactsParams{}))

		var paged []uuid.UUID
		pageSize := int32(7)
		for offset := int32(0); ; offset += pageSize {
			contacts, err := repo.ListContacts(ctx, repository.ListContactsParams{Limit: pageSize, Offset: offset})
			require.NoError(t, err)
			paged = append(paged, scopedIDs(contacts)...)
			if int32(len(contacts)) < pageSize {
				break
			}
		}
		assert.Equal(t, oneShot, paged, "paging with no sort must partition the same deterministic order")
	})

	t.Run("SortTiebreakerIsName", func(t *testing.T) {
		// Two contacts share a birthday; a birthday sort must fall back to
		// name order between them (expected relative order computed by
		// Postgres via the name sort).
		byName := scopedIDs(listAll(repository.ListContactsParams{Sort: "name", Order: "asc"}))
		nameRank := make(map[uuid.UUID]int, len(byName))
		for i, id := range byName {
			nameRank[id] = i
		}

		byBirthday := scopedIDs(listAll(repository.ListContactsParams{Sort: "birthday", Order: "asc"}))
		var shared []uuid.UUID
		for _, id := range byBirthday {
			if b := seeded[id].Birthday; b != nil && b.Equal(sharedBirthday) {
				shared = append(shared, id)
			}
		}
		require.Len(t, shared, 2)
		assert.Less(t, nameRank[shared[0]], nameRank[shared[1]],
			"equal sort keys must tiebreak by name")
	})

	t.Run("IDsQueryMatchesRowsQuery", func(t *testing.T) {
		cases := []struct {
			name   string
			params repository.ListContactsParams
		}{
			{"unsorted", repository.ListContactsParams{}},
			{"name desc", repository.ListContactsParams{Sort: "name", Order: "desc"}},
			{"birthday asc", repository.ListContactsParams{Sort: "birthday", Order: "asc"}},
			{"cadence desc + has_cadence", repository.ListContactsParams{Sort: "cadence", Order: "desc", CadenceFilter: "has_cadence"}},
			{"search unsorted", repository.ListContactsParams{Query: ns}},
			{"search + name asc", repository.ListContactsParams{Query: ns, Sort: "name", Order: "asc"}},
			{"search + no_cadence", repository.ListContactsParams{Query: ns, CadenceFilter: "no_cadence"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				rows := scopedIDs(listAll(tc.params))
				ids, err := repo.ListContactIDs(ctx, repository.ListContactIDsParams{
					Sort:           tc.params.Sort,
					Order:          tc.params.Order,
					Search:         tc.params.Query,
					CadenceFilter:  tc.params.CadenceFilter,
					FollowupFilter: tc.params.FollowupFilter,
				})
				require.NoError(t, err)
				assert.Equal(t, rows, scopedIDList(ids),
					"IDs query must return the same contacts in the same order as the rows query")
			})
		}
	})

	t.Run("SearchScopesToNamespaceAndCounts", func(t *testing.T) {
		// The namespace token appears in every seeded full_name, so an FTS
		// search on it returns exactly this namespace's contacts — and the
		// count query must agree with the rows query.
		rows := listAll(repository.ListContactsParams{Query: ns})
		require.Len(t, scopedIDs(rows), len(seeded), "namespace search should find all seeded contacts")

		total, err := repo.CountContacts(ctx, repository.ListContactsParams{Query: ns})
		require.NoError(t, err)
		assert.Equal(t, int64(len(rows)), total, "count must agree with the rows query")

		withCadence, err := repo.CountContacts(ctx, repository.ListContactsParams{Query: ns, CadenceFilter: "has_cadence"})
		require.NoError(t, err)
		assert.Equal(t, int64(2), withCadence)

		withoutCadence, err := repo.CountContacts(ctx, repository.ListContactsParams{Query: ns, CadenceFilter: "no_cadence"})
		require.NoError(t, err)
		assert.Equal(t, int64(len(seeded)-2), withoutCadence)
	})

	t.Run("SearchRankOrderIsStable", func(t *testing.T) {
		// Search without a sort orders by relevance; repeated calls must
		// return the identical order (rank ties broken deterministically).
		first := scopedIDs(listAll(repository.ListContactsParams{Query: ns}))
		require.Len(t, first, len(seeded))
		for i := 0; i < 3; i++ {
			again := scopedIDs(listAll(repository.ListContactsParams{Query: ns}))
			require.Equal(t, first, again, "search order must be stable across calls")
		}
	})
}
