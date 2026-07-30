//go:build integration_testdb

package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/declare"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/tests/testsupport"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// standardWorld seeds the `standard` profile on a fresh clone and leaves the
// rows in place (Quiesce stops the River client without deleting anything), so
// the world can be read back through the production API.
type standardWorld struct {
	database *db.Database
	ctx      context.Context
	router   *gin.Engine
	harness  *synthetic.Harness
	params   synthetic.SeedParams
	world    declare.WorldResult
	profile  synthetic.ProfileResult
	prefix   string
}

// seedStandardWorldIn seeds under a CALLER-CHOSEN namespace. The determinism
// claim is about the pair (namespace, seed): the generator is a pure function of
// both, so a second run under a different namespace legitimately produces
// different names and would prove nothing.
func seedStandardWorldIn(t *testing.T, namespace string) *standardWorld {
	t.Helper()

	database, ctx := declareTestDB(t)
	params, err := synthetic.ProfileParams(synthetic.ProfileStandard)
	require.NoError(t, err)
	params.Namespace = namespace

	h, teardown, err := synthetic.NewHarnessWithDBForNamespaceAt(
		ctx, database, params.Namespace, params.Seed, accelerated.GetCurrentTime())
	require.NoError(t, err)

	world, profile, err := synthetic.StandardWorldForTest(ctx, h, params)
	if err != nil {
		// Tear the partial world down before failing, so a clone drop is not the
		// only thing standing between a failure and a leaked River client.
		_ = teardown(context.Background())
		require.NoError(t, err, "seed the standard world")
	}
	// Seed-and-leave: the world is what the test reads.
	require.NoError(t, h.Quiesce(ctx))

	return &standardWorld{
		database: database,
		ctx:      ctx,
		router:   newDeclareReadRouter(t, database),
		harness:  h,
		params:   params,
		world:    world,
		profile:  profile,
		prefix:   h.Generator().Prefix(),
	}
}

// TestSyntheticDeclareStandardWorld asserts the composed world is TOURABLE and
// stays inside the budgets the tours impose.
//
// The claims are deliberately two-sided where a bound exists on both ends: an
// overdue population that is too SMALL fails the adversarial requirement, and
// one that is too LARGE silently truncates the tours' captured evidence. A
// one-sided assertion would let the world drift into either failure.
func TestSyntheticDeclareStandardWorld(t *testing.T) {
	testsupport.RequireLongTests(t)
	t.Parallel()
	namespace := declareNS(t)
	w := seedStandardWorldIn(t, namespace)

	t.Run("every marker resolves to exactly one contact", func(t *testing.T) {
		// The Go twin of the tours' own resolveFixture contract: search the
		// contacts list for the marker, keep only rows whose full_name contains
		// it, require exactly one. A later declaration that happened to mint a
		// name carrying a marker token fails HERE rather than twenty minutes into
		// a staging tour run.
		for _, marker := range synthetic.PinnedFixtureMarkers {
			matches := 0
			for _, c := range listContacts(t, w.router, marker) {
				if strings.Contains(c.FullName, marker) {
					matches++
				}
			}
			assert.Equal(t, 1, matches, "marker %q must resolve to exactly one contact", marker)
		}
	})

	t.Run("the three rider fixtures carry the states the tours check", func(t *testing.T) {
		ids := w.harness.PinnedFixtureIDs()

		outreach := getContact(t, w.router, requireMarkerID(t, ids, synthetic.FixtureMarkerOutreach))
		assert.NotNil(t, outreach.LastOutreachAt, "the outreach fixture must carry last_outreach_at")
		assert.Nil(t, outreach.LastContacted, "an outbound touches neither last_contacted nor last_interaction_at")

		response := getContact(t, w.router, requireMarkerID(t, ids, synthetic.FixtureMarkerResponse))
		assert.NotNil(t, response.LastResponseAt, "the response fixture must carry last_response_at")

		pending := getContact(t, w.router, requireMarkerID(t, ids, synthetic.FixtureMarkerPending))
		assert.True(t, pending.HasPendingFollowup, "the pending fixture must carry a live follow-up")
		assert.NotNil(t, pending.LastOutreachAt,
			"a follow-up loop is opened BY an outbound — awaiting a reply to nothing is a state production cannot reach")

		noActivity := getContact(t, w.router, requireMarkerID(t, ids, synthetic.FixtureMarkerNoActivity))
		assert.Nil(t, noActivity.LastContacted)
		assert.Nil(t, noActivity.LastOutreachAt)
		assert.Nil(t, noActivity.LastResponseAt)
		assert.False(t, noActivity.HasPendingFollowup)
	})

	t.Run("the tours' fixture preconditions hold", func(t *testing.T) {
		ids := w.harness.PinnedFixtureIDs()

		target := getContact(t, w.router, requireMarkerID(t, ids, synthetic.FixtureMarkerMergeTarget))
		source := getContact(t, w.router, requireMarkerID(t, ids, synthetic.FixtureMarkerMergeSource))
		require.NotNil(t, target.Cadence)
		require.NotNil(t, source.Cadence)
		assert.NotEqual(t, *target.Cadence, *source.Cadence,
			"the merge preview needs a genuine cadence conflict")

		search := getContact(t, w.router, requireMarkerID(t, ids, synthetic.FixtureMarkerSearch))
		assert.NotNil(t, search.Cadence, "the search subject is walked through the has_cadence filter")

		birthday := getContact(t, w.router, requireMarkerID(t, ids, synthetic.FixtureMarkerBirthday))
		assert.NotNil(t, birthday.Birthday)

		// The delete victim is consumed by the contacts tour, so it only has to
		// exist and be reachable.
		assert.NotEmpty(t, requireMarkerID(t, ids, synthetic.FixtureMarkerDelete))
	})

	t.Run("budgets", func(t *testing.T) {
		contacts := w.namespaceContacts(t)

		// The tours' own floors and windows.
		assert.GreaterOrEqual(t, len(contacts), 5, "the contacts tour throws below five contacts")
		assert.Greater(t, len(contacts), 50, "the arc's page-overflow bar")
		assert.LessOrEqual(t, len(contacts), 500,
			"the tours resolve their fixtures inside a limit=500 window — past it a fixture becomes unreachable")

		overdue := 0
		for _, c := range listOverdue(t, w.router) {
			if strings.HasPrefix(c.FullName, w.prefix) {
				overdue++
			}
		}
		assert.Greater(t, overdue, 50, "the adversarial requirement: an overdue population past fifty")
		assert.LessOrEqual(t, overdue, synthetic.TourOverdueCaptureCap,
			"the overdue-bearing tour captures are sliced at this cap, so a larger population reaches the judge as an unnamed subset — "+
				"raise the cap deliberately (both sides), split the world, or exclude a declaration class")

		// Phones panic on exhaustion at 100 per namespace, and the panic would
		// land on a staging reseed rather than here. Asserting the drawn count
		// makes a future phone-heavy declaration trip a named test instead.
		support := repository.NewSyntheticSupportRepository(w.database.Queries)
		phones, err := support.CountContactMethodsByValueNormalizedPrefix(w.ctx,
			w.harness.Generator().SyntheticPhonePrefix())
		require.NoError(t, err)
		assert.Less(t, phones, int64(100), "the namespace's phone block is exhausted at 100 and exhaustion PANICS")

		// The doc asks a PR that grows the world to report its occupancy, so the
		// subtest prints it rather than making every author instrument the test.
		t.Logf("standard-world occupancy: contacts=%d/500 overdue=%d/%d phones=%d/100",
			len(contacts), overdue, synthetic.TourOverdueCaptureCap, phones)
	})

	t.Run("the composed import queue holds both keying shapes", func(t *testing.T) {
		// Exact, not bounded: the deep-import-queue edge contributes 12 per
		// source and the same-name-pair edge adds ONE more gcontacts collider, so
		// any later edge or declaration that adds a candidate has to update this
		// number — which is the point of asserting it exactly.
		assert.Equal(t, int64(13), w.candidateTotal(t, declare.SourceGContacts))
		assert.Equal(t, int64(12), w.candidateTotal(t, declare.SourceCorrespondence))
		assert.Equal(t, int64(25), w.candidateTotal(t, ""))
	})

	t.Run("the standard tail is last", func(t *testing.T) {
		assertStandardTailIsLast(t, w.world)
	})

	t.Run("the world is deterministic from (namespace, seed)", func(t *testing.T) {
		// The SAME namespace on a SECOND clone: same pair in, same world out.
		second := seedStandardWorldIn(t, namespace)

		require.Equal(t, len(w.world.Steps), len(second.world.Steps), "step count")
		for i := range w.world.Steps {
			assert.Equal(t, w.world.Steps[i].Kind, second.world.Steps[i].Kind, "step %d kind", i)
			assert.Equal(t, w.world.Steps[i].Key, second.world.Steps[i].Key, "step %d key", i)
			assert.Equal(t, w.world.Steps[i].Entities, second.world.Steps[i].Entities, "step %d entity count", i)
		}

		// Names, not ids: the ids are fresh UUIDs by construction. The names are
		// the PRNG's output, so equality here is the determinism claim.
		firstNames := renamedToNamespace(w.world.Order, w.prefix)
		secondNames := renamedToNamespace(second.world.Order, second.prefix)
		assert.Equal(t, firstNames, secondNames, "the same (namespace, seed) must regenerate the same world")

		// The profile accounting must match too — a world that produced the same
		// names by a different route is not the same world.
		assert.Equal(t, w.profile.Contacts, second.profile.Contacts)
		assert.Equal(t, w.profile.SettledInteractions, second.profile.SettledInteractions)
		assert.Equal(t, w.profile.OutboundOnlyContacts, second.profile.OutboundOnlyContacts)
		assert.Equal(t, w.profile.MutualMessageContacts, second.profile.MutualMessageContacts)
		assert.Equal(t, w.profile.SeededTasks, second.profile.SeededTasks)
		assert.Equal(t, w.profile.SeededPendingFollowUps, second.profile.SeededPendingFollowUps)
	})
}

// assertStandardTailIsLast proves the pinned tour fixtures are the LAST rows the
// world creates.
//
// It asserts against the world's own EXECUTION-order creation log rather than
// against created_at, and the distinction is load-bearing: three of the pinned
// fixtures are deliberately backdated (one by three hundred days), so ordering
// by created_at would put most of the world after them and the assertion could
// never pass. What this catches is anything appended after the fixture block
// that CREATES A ROW — which is every case that could shift a numeric identifier
// onto one another contact already owns, the hazard the ordering rule exists for.
func assertStandardTailIsLast(t *testing.T, world declare.WorldResult) {
	t.Helper()
	markers := synthetic.PinnedFixtureMarkers
	order := world.Order
	require.Greater(t, len(order), len(markers), "the world must hold more than its tail")

	tail := order[len(order)-len(markers):]
	got := make([]string, 0, len(tail))
	for _, seeded := range tail {
		assert.Equal(t, "contact", seeded.Kind, "every tail row is a pinned fixture contact")
		got = append(got, markerOf(t, seeded.Name, markers))
	}

	// The tail's own emission order, stated: the three marker-bearing riders the
	// catalog profile used to seed inside its phase blocks, then the replay-free
	// pinned block — which stays genuinely last.
	want := []string{
		synthetic.FixtureMarkerPending,
		synthetic.FixtureMarkerOutreach,
		synthetic.FixtureMarkerResponse,
		synthetic.FixtureMarkerNoActivity,
		synthetic.FixtureMarkerMergeTarget,
		synthetic.FixtureMarkerMergeSource,
		synthetic.FixtureMarkerSearch,
		synthetic.FixtureMarkerDelete,
		synthetic.FixtureMarkerBirthday,
		synthetic.FixtureMarkerOverdueA,
		synthetic.FixtureMarkerOverdueB,
	}
	assert.Equal(t, want, got,
		"the final %d rows the world creates must be exactly the pinned fixtures, in the tail's emission order — "+
			"anything created after them can shift a numeric identifier onto a contact that already owns it", len(markers))

	// And nothing earlier carries a marker, so the tail is the ONLY place they
	// come from.
	for _, seeded := range order[:len(order)-len(markers)] {
		for _, marker := range markers {
			assert.NotContains(t, seeded.Name, marker,
				"a non-tail row carries marker %q — the marker set must be minted once, by the tail", marker)
		}
	}
}

// TestSyntheticDeclareWorldStepFailure proves a step failure stays VISIBLE after
// the composition moved the Gate-B drain from once-per-declaration to
// once-per-world. That is a genuine change to the error surface, so it gets a
// test rather than an argument.
func TestSyntheticDeclareWorldStepFailure(t *testing.T) {
	testsupport.RequireLongTests(t)

	// The earliest interior step keeps both a completed prefix and an unreached
	// suffix to assert about without paying to build unrelated later edges before
	// deliberately failing.
	plan := declare.WorldPlan("test-tail")
	require.Greater(t, len(plan), 3)
	failAt := plan[1]
	require.NotEqual(t, declare.WorldStepTail, failAt.Kind)

	t.Run("an interior world step failure names the step", func(t *testing.T) {
		database, ctx := declareTestDB(t)
		restore := declare.SetWorldStepFailpointForTest(failAt.Key)
		defer restore()

		namespace := declareNS(t)
		h, teardown, err := synthetic.NewHarnessWithDBForNamespaceAt(
			ctx, database, namespace, factory.DefaultSeed, accelerated.GetCurrentTime())
		require.NoError(t, err)

		support := repository.NewSyntheticSupportRepository(database.Queries)
		tailRan := false
		res, err := declare.World(ctx, h, support, declare.WorldTail{
			Name: "test-tail",
			Run: func(context.Context, *synthetic.Harness) ([]declare.Seeded, error) {
				tailRan = true
				return nil, nil
			},
		})

		require.Error(t, err, "the failpoint must surface, not be swallowed by the single drain")
		assert.Contains(t, err.Error(), failAt.Key, "the error must NAME the failing step")
		assert.False(t, tailRan, "no later step may run after a step failed")

		// The partial run is diagnosable: Steps is the log of what COMPLETED, so
		// it holds every step BEFORE the failure and not the failing one — the
		// error is what names where the run stopped.
		wantCompleted := []string{}
		for _, step := range plan {
			if step.Key == failAt.Key {
				break
			}
			wantCompleted = append(wantCompleted, step.Key)
		}
		gotCompleted := make([]string, 0, len(res.Steps))
		for _, step := range res.Steps {
			gotCompleted = append(gotCompleted, step.Key)
		}
		assert.Equal(t, wantCompleted, gotCompleted,
			"the completed-step log must be the plan's prefix up to the failure")

		// And the partial world is recoverable: teardown, then a cleanup, empties
		// the namespace.
		require.NoError(t, teardown(context.Background()))
		after := measureResidue(t, ctx, database, namespace, factory.DefaultSeed)
		assert.Equal(t, int64(0), after.total(), "residue after teardown: %+v", after)
	})

}

// --- helpers -----------------------------------------------------------------

// namespaceContacts is the world's contacts as the tours' own selector window
// sees them: one unscoped list read at limit=500.
//
// Unscoped, deliberately: the contact search is Postgres FULL TEXT search, so a
// namespace PREFIX is not a term it can match. The test runs on a per-test clone
// that holds only this world, so the unscoped read IS the namespace's — and the
// prefix filter keeps that assumption visible rather than implicit.
func (w *standardWorld) namespaceContacts(t *testing.T) []handlers.ContactResponse {
	t.Helper()
	env := getEnvelope(t, w.router, "/api/v1/contacts?limit=500&page=1")
	require.Equal(t, http.StatusOK, env.Status)
	var rows []handlers.ContactResponse
	require.NoError(t, json.Unmarshal(env.Data, &rows))

	out := make([]handlers.ContactResponse, 0, len(rows))
	for _, c := range rows {
		if strings.HasPrefix(c.FullName, w.prefix) {
			out = append(out, c)
		}
	}
	require.Len(t, out, len(rows), "the clone must hold only this world")
	return out
}

// candidateTotal is the import queue's reported total for one source (or all
// sources when source is empty). The clone holds only this world, so the total
// IS the namespace's.
func (w *standardWorld) candidateTotal(t *testing.T, source string) int64 {
	t.Helper()
	path := "/api/v1/imports/candidates?limit=10&page=1"
	if source != "" {
		path += "&source=" + source
	}
	env := getEnvelope(t, w.router, path)
	require.Equal(t, http.StatusOK, env.Status)
	require.NotNil(t, env.Meta)
	require.NotNil(t, env.Meta.Pagination)
	return env.Meta.Pagination.Total
}

func requireMarkerID(t *testing.T, ids map[string]uuid.UUID, marker string) string {
	t.Helper()
	id, ok := ids[marker]
	require.True(t, ok, "the world did not record a contact for marker %q", marker)
	return id.String()
}

// renamedToNamespace strips the namespace prefix so two worlds seeded under
// DIFFERENT namespaces can be compared on the part the PRNG actually decides.
func renamedToNamespace(order []declare.Seeded, prefix string) []string {
	out := make([]string, 0, len(order))
	for _, seeded := range order {
		out = append(out, seeded.Kind+"/"+strings.TrimPrefix(seeded.Name, prefix))
	}
	return out
}

func markerOf(t *testing.T, name string, markers []string) string {
	t.Helper()
	for _, marker := range markers {
		if strings.Contains(name, marker) {
			return marker
		}
	}
	t.Fatalf("tail row %q carries no pinned marker", name)
	return ""
}
