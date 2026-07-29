//go:build integration_testdb

package tests

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/declare"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/synthetic/replay"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The foo / foo-bar isolation proof from the CLEANUP side. Sibling namespaces
// where one is a prefix of another are a normal state (a client derives
// per-call suffixes), so cleaning "foo" must never LIKE-sweep across a live
// "foo-bar" world that "foo" itself never created.
func TestCleanup_RefusesLiveDescendant(t *testing.T) {
	database, ctx := declareTestDB(t)

	parent := declareNS(t)
	child := parent + "-bar"
	// Only the CHILD is seeded; the parent token was never used.
	childRes := mustRun(t, ctx, database, "DSH-005", child)
	before := measureResidue(t, ctx, database, childRes.Namespace, factory.DefaultSeed)
	require.Greater(t, before.total(), int64(0))

	res, err := declare.CleanupNamespaces(ctx, database, []string{parent}, factory.DefaultSeed)
	require.NoError(t, err)
	outcome, ok := res.Results[parent]
	require.True(t, ok)
	assert.Equal(t, declare.StatusError, outcome.Status)
	assert.Contains(t, outcome.Descendants, hostnameFor(child),
		"the guard must NAME the descendant that blocked the sweep")

	after := measureResidue(t, ctx, database, childRes.Namespace, factory.DefaultSeed)
	assert.Equal(t, before, after, "the descendant's world must be byte-intact")

	requireCleaned(t, ctx, database, []string{childRes.Namespace}, factory.DefaultSeed)
}

// A test that RENAMES a seeded contact through the ordinary contact API must
// not thereby hide it from cleanup.
//
// The hazard is specific and silent. Cleanup runs in a later request with no
// handle on what the seed created, so it recovers contacts from the namespace
// prefix their generated full_name carries — and full_name is user-editable.
// The update also rewrites node.canonical_label, so the label sweep loses the
// person node too. With only name-derived recovery the whole cleanup then walks
// past the contact AND everything hanging off it, deletes the namespace's
// discovery marker, and reports "cleaned" over live rows that nothing can ever
// find again. This is not hypothetical: the contacts domain's specs edit names.
//
// The residue assertion is deliberately BY ID rather than by prefix: a
// prefix-scoped count cannot see a renamed row, so it would report zero residue
// in exactly the broken case.
func TestCleanup_SweepsRenamedContact(t *testing.T) {
	database, ctx := declareTestDB(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)
	router := newDeclareReadRouter(t, database)

	namespace := declareNS(t)
	seeded := mustRun(t, ctx, database, "DSH-005", namespace)
	target := uuid.MustParse(seeded.Entities["refresh-target"].ID)
	sentinel := uuid.MustParse(seeded.Entities["refresh-sentinel"].ID)

	renameContact(t, router, target.String(), "Renamed Away From The Namespace")

	// Prove the hazard is real before proving the fix: the name-derived lookup
	// the ladder used to rely on no longer finds this contact.
	byName, err := support.SelectContactIDsByFullNamePrefix(ctx,
		factory.NewGenerator(factory.DefaultSeed, seeded.Namespace).Prefix())
	require.NoError(t, err)
	require.NotContains(t, byName, target,
		"the rename must actually have taken the contact out of the prefix sweep")
	require.Contains(t, byName, sentinel, "the untouched sibling must still be found by name")

	requireCleaned(t, ctx, database, []string{namespace}, factory.DefaultSeed)

	surviving, err := support.CountContactsByIds(ctx, []uuid.UUID{target, sentinel})
	require.NoError(t, err)
	assert.Equal(t, int64(0), surviving, "the renamed contact must be swept like any other")
	liveNodes, err := support.CountLiveNodesByIds(ctx, []uuid.UUID{target, sentinel})
	require.NoError(t, err)
	assert.Equal(t, int64(0), liveNodes, "the renamed contact's person node must go with it")
	assert.Equal(t, int64(0), measureResidue(t, ctx, database, seeded.Namespace, factory.DefaultSeed).total())
}

// Cleanup can never race an in-flight seed: a held reservation is reported as
// busy and NOTHING is deleted under it.
func TestCleanup_BusyWhileSeedInFlight(t *testing.T) {
	database, ctx := declareTestDB(t)

	namespace := declareNS(t)
	seeded := mustRun(t, ctx, database, "DSH-005", namespace)
	before := measureResidue(t, ctx, database, seeded.Namespace, factory.DefaultSeed)

	conn, err := database.Pool.Acquire(ctx)
	require.NoError(t, err)
	holder := repository.NewSyntheticSupportRepository(db.New(conn))
	key := declare.AdvisoryKeyForTest("declare:" + namespace)
	held, err := holder.TryAdvisoryLock(ctx, key)
	require.NoError(t, err)
	require.True(t, held)

	res, err := declare.CleanupNamespaces(ctx, database, []string{namespace}, factory.DefaultSeed)
	require.NoError(t, err)
	assert.Equal(t, declare.StatusBusy, res.Results[namespace].Status)
	assert.Equal(t, before, measureResidue(t, ctx, database, seeded.Namespace, factory.DefaultSeed),
		"a busy namespace must have nothing deleted under it")

	_, err = holder.AdvisoryUnlock(ctx, key)
	require.NoError(t, err)
	conn.Release()

	requireCleaned(t, ctx, database, []string{namespace}, factory.DefaultSeed)
}

// The lost-response case: the client only ever knew the token it REQUESTED, and
// the server re-salted. Cleaning by the requested token alone must still reach
// the salted world, including its salted numeric-band tokens.
func TestCleanup_SweepsSaltedVariants(t *testing.T) {
	database, ctx := declareTestDB(t)

	requested, effective := seedWithForcedResalt(t, ctx, database)
	require.NotEqual(t, requested, effective, "the setup must have forced a re-salt")

	before := measureResidue(t, ctx, database, effective, factory.DefaultSeed)
	require.Greater(t, before.total(), int64(0))

	// Clean by the REQUESTED token only — what a client with a lost response has.
	res := requireCleaned(t, ctx, database, []string{requested}, factory.DefaultSeed)
	assert.Contains(t, res.Expansions[requested], effective,
		"the requested token must expand to the salted world")

	after := measureResidue(t, ctx, database, effective, factory.DefaultSeed)
	assert.Equal(t, int64(0), after.total(), "residue in the salted world: %+v", after)
}

// The ordinary case: cleaning by the EFFECTIVE namespace from the manifest.
func TestDeclareRun_ReSaltedNamespaceCleansUp(t *testing.T) {
	database, ctx := declareTestDB(t)

	_, effective := seedWithForcedResalt(t, ctx, database)
	requireCleaned(t, ctx, database, []string{effective}, factory.DefaultSeed)
	assert.Equal(t, int64(0), measureResidue(t, ctx, database, effective, factory.DefaultSeed).total())
}

// The list the real client sends after a re-salt is [requested, effective].
// Canonicalization must collapse that to ONE world: one lock acquisition, one
// ladder, one result. The requested token re-salted AWAY — it names no world of
// its own — so reporting it as a second (empty, "cleaned") namespace would tell
// the client it swept two worlds when there was one. A reentrant double-acquire
// would also strand a counted session lock and serialize the namespace forever.
func TestCleanup_ClientListRequestedPlusEffective(t *testing.T) {
	database, ctx := declareTestDB(t)

	requested, effective := seedWithForcedResalt(t, ctx, database)

	res, err := declare.CleanupNamespaces(ctx, database, []string{requested, effective}, factory.DefaultSeed)
	require.NoError(t, err)
	require.Len(t, res.Results, 1,
		"the pair names ONE world; canonicalization must not report the re-salted-away token as a second one")
	require.Contains(t, res.Results, effective)
	assert.Equal(t, declare.StatusCleaned, res.Results[effective].Status)
	assert.Equal(t, []string{effective}, res.Expansions[requested],
		"the requested token's expansion IS the effective namespace, not itself plus it")

	assert.Equal(t, int64(0), measureResidue(t, ctx, database, effective, factory.DefaultSeed).total())

	// The reservation is free afterwards — proof no lock was stranded by the
	// duplicate reference to the same world.
	conn, err := database.Pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	holder := repository.NewSyntheticSupportRepository(db.New(conn))
	for _, ns := range []string{requested, effective} {
		key := declare.AdvisoryKeyForTest("declare:" + ns)
		acquired, err := holder.TryAdvisoryLock(ctx, key)
		require.NoError(t, err)
		assert.True(t, acquired, "namespace %s reservation was left held", ns)
		_, _ = holder.AdvisoryUnlock(ctx, key)
	}
}

// A re-salted run publishes the EFFECTIVE namespace's host marker during
// CONSTRUCTION — before any declaration executes and before the caller learns
// the token. That marker is exactly what cleanup's expansion discovers, so a
// reservation covering only the REQUESTED token would leave the effective world
// unlocked: a lost-response cleanup arriving mid-run would try-lock the
// effective token, find it free, and delete a still-running harness's world out
// from under it. Reserving the whole salt family before anything is
// materialized is what makes this cleanup report busy and delete nothing.
func TestCleanup_CannotDeleteInFlightResaltedRun(t *testing.T) {
	database, ctx := declareTestDB(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	requested := declareNS(t)
	gen := factory.NewGenerator(factory.DefaultSeed, requested)
	require.NoError(t, support.InsertTelegramChatConfigInBand(ctx, gen.PeerBandStart()))
	t.Cleanup(func() {
		_, _ = support.DeleteTelegramChatConfigsByChatIds(context.Background(), []int64{gen.PeerBandStart()})
	})

	// The hook fires with the world fully seeded and the run still holding its
	// reservation — the exact instant a lost-response cleanup would arrive.
	var midRun declare.CleanupResult
	var midRunErr error
	restore := declare.SetTestHookForTest(declare.HookAfterReplayBeforeDrain,
		func(hookCtx context.Context, _ *replay.Harness) error {
			midRun, midRunErr = declare.CleanupNamespaces(hookCtx, database, []string{requested}, factory.DefaultSeed)
			return nil
		})
	res, err := declare.Run(ctx, database, "DSH-005", requested, factory.DefaultSeed)
	restore()
	require.NoError(t, err)
	require.NotEqual(t, requested, res.Namespace, "the pre-occupied band must have forced a re-salt")

	require.NoError(t, midRunErr)
	// Prove the cleanup actually REACHED the effective world first: a cleanup
	// that never discovered it would refuse nothing and look identical to one
	// that was correctly blocked.
	require.Contains(t, midRun.Expansions[requested], res.Namespace,
		"the mid-run cleanup must have discovered the effective namespace")
	require.NotEmpty(t, midRun.Results)
	for ns, outcome := range midRun.Results {
		assert.Equal(t, declare.StatusBusy, outcome.Status,
			"namespace %s must be refused while its run is in flight: %s (%s)", ns, outcome.Status, outcome.Err)
	}

	// The world survived: the manifest's contacts are all still there.
	contactIDs, err := support.SelectContactIDsByFullNamePrefix(ctx, factory.SyntheticSourcePrefix+res.Namespace+"-")
	require.NoError(t, err)
	assert.Len(t, contactIDs, len(res.Entities), "the in-flight world lost rows to the racing cleanup")

	requireCleaned(t, ctx, database, []string{requested}, factory.DefaultSeed)
	assert.Equal(t, int64(0), measureResidue(t, ctx, database, res.Namespace, factory.DefaultSeed).total())
}

// Cleanup refuses while an EVENT-LINKED unfinalized job still dereferences the
// namespace's rows, and completes once it is finalized.
func TestCleanup_PendingThenRetrySucceeds(t *testing.T) {
	database, ctx := declareTestDB(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	namespace := declareNS(t)
	seeded := mustRun(t, ctx, database, "DSH-005", namespace)

	prefix := factory.SyntheticSourcePrefix + seeded.Namespace + "-"
	contactIDs, err := support.SelectContactIDsByFullNamePrefix(ctx, prefix)
	require.NoError(t, err)
	eventIDs, err := support.ListEventIdsForContacts(ctx, contactIDs)
	require.NoError(t, err)
	require.NotEmpty(t, eventIDs, "the seeded world must have produced events to link a job to")

	jobID, err := support.InsertUnfinalizedRecorderJobForEvent(ctx, eventIDs[0])
	require.NoError(t, err)

	// Prove the injection landed before asserting the refusal — a gate that
	// cannot fail is indistinguishable from one that always passes.
	planted, err := support.CountPendingJobsForNamespaceCleanup(ctx, eventIDs, contactIDs)
	require.NoError(t, err)
	require.Greater(t, planted, int64(0))

	before := measureResidue(t, ctx, database, seeded.Namespace, factory.DefaultSeed)
	res, err := declare.CleanupNamespaces(ctx, database, []string{namespace}, factory.DefaultSeed)
	require.NoError(t, err)
	assert.Equal(t, declare.StatusPending, res.Results[seeded.Namespace].Status)
	assert.Equal(t, before, measureResidue(t, ctx, database, seeded.Namespace, factory.DefaultSeed),
		"a pending namespace must have nothing deleted under it")

	require.NoError(t, support.FinalizeRiverJobByID(ctx, jobID))
	requireCleaned(t, ctx, database, []string{namespace}, factory.DefaultSeed)
	assert.Equal(t, int64(0), measureResidue(t, ctx, database, seeded.Namespace, factory.DefaultSeed).total())
}

// The SECOND Gate-B linkage class, separately: an aggregate job keys on
// {contact_id, source} and carries NO event id, so an event-only predicate
// would happily delete rows it still dereferences.
func TestCleanup_AggregateJobRefusal(t *testing.T) {
	database, ctx := declareTestDB(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	namespace := declareNS(t)
	seeded := mustRun(t, ctx, database, "DSH-005", namespace)

	prefix := factory.SyntheticSourcePrefix + seeded.Namespace + "-"
	contactIDs, err := support.SelectContactIDsByFullNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.NotEmpty(t, contactIDs)

	jobID, err := support.InsertUnfinalizedAggregateJobForContact(ctx, contactIDs[0], repository.InteractionSourceMessages)
	require.NoError(t, err)

	// The injection is invisible to an event-only predicate, which is the whole
	// point: assert it against the real gate, with an EMPTY event set.
	planted, err := support.CountPendingJobsForNamespaceCleanup(ctx, nil, contactIDs)
	require.NoError(t, err)
	require.Greater(t, planted, int64(0), "the aggregate job must be visible to the cleanup gate")

	res, err := declare.CleanupNamespaces(ctx, database, []string{namespace}, factory.DefaultSeed)
	require.NoError(t, err)
	assert.Equal(t, declare.StatusPending, res.Results[seeded.Namespace].Status)

	require.NoError(t, support.FinalizeRiverJobByID(ctx, jobID))
	requireCleaned(t, ctx, database, []string{namespace}, factory.DefaultSeed)
}

// A partially failed ladder KEEPS both discovery mechanisms — the host marker
// and the namespace's ownership records — so the namespace stays discoverable
// (expansion finds it), occupied (a re-seed is refused) and RECOVERABLE (a
// renamed contact is still reachable by id), and a later un-failed retry
// finishes the job. Dropping either on a failed sweep would make the residue
// unreachable forever.
func TestCleanup_LadderFailureKeepsMarker(t *testing.T) {
	database, ctx := declareTestDB(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	namespace := declareNS(t)
	seeded := mustRun(t, ctx, database, "DSH-005", namespace)

	restore := declare.SetCleanupFailStepForTest("contacts")
	res, err := declare.CleanupNamespaces(ctx, database, []string{namespace}, factory.DefaultSeed)
	restore()
	require.NoError(t, err)
	require.Equal(t, declare.StatusError, res.Results[seeded.Namespace].Status)

	_, exists, err := support.SelectMacHostIDByHostname(ctx, hostnameFor(seeded.Namespace))
	require.NoError(t, err)
	assert.True(t, exists, "the discovery marker must SURVIVE a partially failed sweep")

	owned, err := support.SelectNamespaceEntityIDs(ctx, seeded.Namespace, repository.EntityKindContact)
	require.NoError(t, err)
	assert.Len(t, owned, len(seeded.Entities),
		"the ownership records must SURVIVE too — they are how a renamed contact stays reachable")

	// Still occupied: a re-seed of the same namespace is refused.
	_, err = declare.Run(ctx, database, "DSH-005", namespace, factory.DefaultSeed)
	assert.ErrorIs(t, err, declare.ErrNamespaceOccupied)

	// And the retry finishes it.
	requireCleaned(t, ctx, database, []string{namespace}, factory.DefaultSeed)
	assert.Equal(t, int64(0), measureResidue(t, ctx, database, seeded.Namespace, factory.DefaultSeed).total())
}

// D13's forced collision: two DIFFERENT namespaces whose phone bands hash to
// the same area code, run concurrently with phone-bearing declarations. The
// band claim serializes them, so the second sees the first's committed rows and
// re-salts — the outcome being that no phone value is ever shared between two
// worlds, which identity matching resolves DB-wide with no namespace scoping.
func TestDeclareRun_ConcurrentBandCollisionSerializes(t *testing.T) {
	database, ctx := declareTestDB(t)

	first, second := collidingNamespaces(t)
	d := declare.Declaration{Behavior: "band-collision", Entities: []declare.Entity{
		declare.Contact("phone-bearer", declare.Methods("phone")),
	}}

	var wg sync.WaitGroup
	results := make([]declare.Result, 2)
	errs := make([]error, 2)
	for i, ns := range []string{first, second} {
		wg.Add(1)
		go func(i int, ns string) {
			defer wg.Done()
			results[i], errs[i] = declare.RunDeclarationForTest(ctx, database, d, ns, factory.DefaultSeed)
		}(i, ns)
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])

	// Exactly one re-salted away from the shared band, so the two worlds' phone
	// prefixes are disjoint.
	prefixA := factory.NewGenerator(factory.DefaultSeed, results[0].Namespace).SyntheticPhonePrefix()
	prefixB := factory.NewGenerator(factory.DefaultSeed, results[1].Namespace).SyntheticPhonePrefix()
	assert.NotEqual(t, prefixA, prefixB,
		"two concurrent runs colliding on a phone band must not both keep it")

	// Each cleanup removes exactly its own rows.
	requireCleaned(t, ctx, database, []string{results[0].Namespace}, factory.DefaultSeed)
	assert.Equal(t, int64(0), measureResidue(t, ctx, database, results[0].Namespace, factory.DefaultSeed).total())
	assert.Greater(t, measureResidue(t, ctx, database, results[1].Namespace, factory.DefaultSeed).total(), int64(0),
		"cleaning one world must not touch the other")

	requireCleaned(t, ctx, database, []string{results[1].Namespace}, factory.DefaultSeed)
	assert.Equal(t, int64(0), measureResidue(t, ctx, database, results[1].Namespace, factory.DefaultSeed).total())
}

// --- setup helpers ----------------------------------------------------------

// seedWithForcedResalt pre-occupies a namespace's telegram peer band so
// construction re-salts, then seeds it. Returns (requested, effective).
func seedWithForcedResalt(t *testing.T, ctx context.Context, database *db.Database) (string, string) {
	t.Helper()
	support := repository.NewSyntheticSupportRepository(database.Queries)

	requested := declareNS(t)
	gen := factory.NewGenerator(factory.DefaultSeed, requested)

	// A chat-config row inside the peer band is one of the occupancy signals
	// the toolkit checks, and it belongs to no contact — so it forces a re-salt
	// without seeding a world that would then need cleaning.
	require.NoError(t, support.InsertTelegramChatConfigInBand(ctx, gen.PeerBandStart()))
	t.Cleanup(func() {
		_, _ = support.DeleteTelegramChatConfigsByChatIds(context.Background(), []int64{gen.PeerBandStart()})
	})

	res := mustRun(t, ctx, database, "DSH-005", requested)
	require.NotEqual(t, requested, res.Namespace, "the pre-occupied band must have forced a re-salt")
	return requested, res.Namespace
}

// collidingNamespaces brute-forces two distinct namespace tokens whose phone
// area codes collide. The generator buckets namespaces into a few hundred
// areas, so a short search always finds a pair — which is exactly why globally
// unique namespaces do NOT give unique bands.
func collidingNamespaces(t *testing.T) (string, string) {
	t.Helper()
	base := declareNS(t)
	seen := map[int64]string{}
	for i := 0; i < 5000; i++ {
		candidate := fmt.Sprintf("%s-b%d", base, i)
		area := factory.NewGenerator(factory.DefaultSeed, candidate).PhoneAreaCode()
		if prior, ok := seen[area]; ok {
			return prior, candidate
		}
		seen[area] = candidate
	}
	t.Fatal("no colliding phone-area pair found; the band space is far smaller than the search")
	return "", ""
}
