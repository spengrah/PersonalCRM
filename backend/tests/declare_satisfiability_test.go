//go:build integration_testdb

package tests

import (
	"net/url"
	"testing"
	"time"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/declare"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createdAtTolerance absorbs the gap between the generator anchor (stamped when
// the harness is built) and the row write. It is far tighter than any declared
// amount, so a mis-lowered created_at cannot hide inside it.
const createdAtTolerance = 30 * time.Second

// TestSyntheticDeclareSatisfiability executes EVERY registered declaration and
// asserts the postconditions its own properties imply, through the production
// API read path. The subtests are minted from the registry itself
// (TestSyntheticDeclareSatisfiability/<behavior-id>), so a registered
// declaration cannot lack coverage — there is no parallel list to drift from.
//
// Each declaration then gets its namespace cleaned and its residue asserted to
// zero, TOMBSTONES INCLUDED: one seeded contact is soft-deleted through the
// real service first, because a cleanup that only found live rows would leave
// the tombstone's identifiers permanently claiming the namespace.
func TestSyntheticDeclareSatisfiability(t *testing.T) {
	testsupport.RequireLongTests(t)

	database, ctx := declareTestDB(t)
	router := newDeclareReadRouter(t, database)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)

	declarations := declare.Registered()
	require.NotEmpty(t, declarations, "the registry must not be empty")

	for _, d := range declarations {
		t.Run(d.Behavior, func(t *testing.T) {
			namespace := declareNS(t)
			res := mustRun(t, ctx, database, d.Behavior, namespace)

			require.Equal(t, namespace, res.Namespace,
				"a fresh clone has free bands, so no re-salt is expected here")
			require.Len(t, res.Entities, len(d.Entities))
			require.False(t, res.Anchor.IsZero(), "the manifest must carry the generator anchor")

			overdue := listOverdue(t, router)

			// PostconditionsAt, not Postconditions: the anchor-dependent facts
			// (a birthday) are only populated when the run's own anchor is
			// supplied, so the anchor-free call would silently skip them.
			for _, pc := range d.PostconditionsAt(res.Anchor) {
				seeded, ok := res.Entities[pc.Handle]
				require.True(t, ok, "manifest is missing handle %q", pc.Handle)
				assertPostcondition(t, router, overdue, res, pc, seeded)
			}

			// Soft-delete one seeded contact through the REAL service so the
			// namespace carries a tombstone, then prove cleanup still empties it.
			var tombstoned string
			for _, seeded := range res.Entities {
				tombstoned = seeded.ID
				break
			}
			require.NoError(t, contactRepo.SoftDeleteContact(ctx, uuidMust(t, tombstoned)))

			before := measureResidue(t, ctx, database, res.Namespace, factory.DefaultSeed)
			require.Greater(t, before.total(), int64(0), "the world must exist before it is cleaned")
			require.Equal(t, len(d.Entities), before.Contacts,
				"the tombstoned contact must still be COUNTED — a soft-delete-blind sweep would strand it")

			requireCleaned(t, ctx, database, []string{res.Namespace}, factory.DefaultSeed)
			after := measureResidue(t, ctx, database, res.Namespace, factory.DefaultSeed)
			assert.Equal(t, int64(0), after.total(), "residue after cleanup: %+v", after)
		})
	}
}

// assertPostcondition checks one derived fact set against the API reads. Every
// non-nil field is asserted; a nil field is a declaration that says nothing
// about that property, and asserting it anyway would invent a requirement.
func assertPostcondition(
	t *testing.T,
	router *gin.Engine,
	overdue []handlers.OverdueContactResponse,
	res declare.Result,
	pc declare.Postcondition,
	seeded declare.Seeded,
) {
	t.Helper()

	if pc.Listed {
		// Searched by the MANIFEST NAME, which is how a spec locates a seeded
		// contact: a fixture that exists in the database but cannot be found
		// through the search users actually use is not a usable fixture.
		assert.True(t, containsContactID(listContacts(t, router, url.QueryEscape(seeded.Name)), seeded.ID),
			"handle %q (%s) is not reachable through the contact search read", pc.Handle, seeded.Name)
	}

	detail := getContact(t, router, seeded.ID)
	assert.Equal(t, seeded.Name, detail.FullName, "manifest name must match the stored full_name")

	if pc.Cadence != nil {
		require.NotNil(t, detail.Cadence, "handle %q should carry a cadence", pc.Handle)
		assert.Equal(t, *pc.Cadence, *detail.Cadence)
	}

	if pc.OverdueMember != nil {
		inOverdue := containsOverdueID(overdue, seeded.ID)
		assert.Equal(t, *pc.OverdueMember, inOverdue,
			"handle %q overdue membership", pc.Handle)
	}

	if pc.LastContacted != nil {
		if *pc.LastContacted {
			require.NotNil(t, detail.LastContacted,
				"handle %q declared replayed history, so last_contacted must be set", pc.Handle)
			// Replayed history means a real interaction moved it — a column
			// write with no interaction behind it is exactly what the toolkit's
			// honesty rule forbids.
			assert.GreaterOrEqual(t, countInteractions(t, router, seeded.ID), 1,
				"handle %q must have an interaction backing its last_contacted", pc.Handle)
		} else {
			assert.Nil(t, detail.LastContacted,
				"handle %q declared no history, so last_contacted must be null", pc.Handle)
			assert.Equal(t, 0, countInteractions(t, router, seeded.ID))
		}
	}

	if pc.CreatedAgo != nil {
		// TWO-SIDED against the manifest anchor: an upper bound alone would pass
		// a contact created arbitrarily early.
		want := res.Anchor.Add(-*pc.CreatedAgo)
		delta := detail.CreatedAt.Sub(want)
		assert.LessOrEqual(t, absDuration(delta), createdAtTolerance,
			"handle %q created_at %s is not within %s of anchor−%s (%s)",
			pc.Handle, detail.CreatedAt, createdAtTolerance, *pc.CreatedAgo, want)
	}

	if pc.MethodKinds != nil {
		assert.ElementsMatch(t, pc.MethodKinds, methodKindsOf(detail),
			"handle %q method set", pc.Handle)
	}

	if pc.Birthday != nil {
		require.NotNil(t, detail.Birthday, "handle %q declared a birthday", pc.Handle)
		assert.Equal(t, pc.Birthday.UTC().Format("2006-01-02"), detail.Birthday.UTC().Format("2006-01-02"),
			"handle %q birthday must survive the read byte-identically", pc.Handle)
	}

	if pc.Location != nil {
		require.NotNil(t, detail.Location, "handle %q declared a location", pc.Handle)
		expected := factory.SyntheticSourcePrefix + res.Namespace + "-" + *pc.Location
		assert.Equal(t, expected, *detail.Location,
			"handle %q location must survive the read with its namespace prefix", pc.Handle)
	}
}

func containsContactID(list []handlers.ContactResponse, id string) bool {
	for _, c := range list {
		if c.ID == id {
			return true
		}
	}
	return false
}

func containsOverdueID(list []handlers.OverdueContactResponse, id string) bool {
	for _, c := range list {
		if c.ID == id {
			return true
		}
	}
	return false
}

func methodKindsOf(c handlers.ContactResponse) []string {
	kinds := make([]string, 0, len(c.Methods))
	for _, m := range c.Methods {
		kinds = append(kinds, m.Type)
	}
	return kinds
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func uuidMust(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	require.NoError(t, err)
	return id
}
