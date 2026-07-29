//go:build integration_testdb

package tests

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/declare"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/synthetic/replay"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vocabularyDeclaration exercises every declared PROPERTY, including the
// combinations no PR1-registered fixture uses. Registered fixtures are not
// distorted to reach coverage; this declaration is a VALUE, executed through
// the exported test entry point rather than the registry, precisely so it never
// has to be registered under a spec behavior id.
func vocabularyDeclaration() declare.Declaration {
	return declare.Declaration{
		Behavior: "vocabulary-exercise",
		Entities: []declare.Entity{
			// Overdue through created_at with NO history: last_contacted stays null.
			declare.Contact("stale", declare.Cadence("weekly"), declare.NeverContacted(), declare.CreatedAgo(declare.Periods(2))),
			// The derived NOT-overdue branch: one day back is inside a weekly period.
			declare.Contact("fresh", declare.Cadence("weekly"), declare.NeverContacted(), declare.CreatedAgo(declare.Days(1))),
			// No methods at all.
			declare.Contact("bare", declare.NoMethods()),
			// A multi-method contact with no email.
			declare.Contact("reachable", declare.Methods("phone", "telegram")),
			// A non-weekly cadence with replayed history.
			declare.Contact("monthly", declare.Cadence("monthly"), declare.OverdueBy(declare.Days(9))),
		},
	}
}

// TestDeclareRun_VocabularyExercise proves every vocabulary item lowers to a
// world the API reads back the way the declaration says it should — not just
// that the rows exist.
func TestDeclareRun_VocabularyExercise(t *testing.T) {
	database, ctx := declareTestDB(t)
	router := newDeclareReadRouter(t, database)

	d := vocabularyDeclaration()
	namespace := declareNS(t)
	res, err := declare.RunDeclarationForTest(ctx, database, d, namespace, factory.DefaultSeed)
	require.NoError(t, err)
	require.Len(t, res.Entities, len(d.Entities))

	overdue := listOverdue(t, router)
	for _, pc := range d.Postconditions() {
		seeded, ok := res.Entities[pc.Handle]
		require.True(t, ok, "manifest is missing handle %q", pc.Handle)
		assertPostcondition(t, router, overdue, res, pc, seeded)
	}

	requireCleaned(t, ctx, database, []string{res.Namespace}, factory.DefaultSeed)
	assert.Equal(t, int64(0), measureResidue(t, ctx, database, res.Namespace, factory.DefaultSeed).total())
}

// Two calls must produce two worlds with disjoint identifiers. The generator is
// a pure function of (seed, namespace), so reusing one namespace would mint
// byte-identical names and emails — which is why the client derives a per-call
// namespace rather than reusing the test's prefix.
func TestDeclareRun_TwoCallsTwoNamespaces(t *testing.T) {
	database, ctx := declareTestDB(t)

	base := declareNS(t)
	first := mustRun(t, ctx, database, "CAD-026", fmtNS(base, 1))
	second := mustRun(t, ctx, database, "CAD-026", fmtNS(base, 2))

	require.NotEqual(t, first.Namespace, second.Namespace)
	names := map[string]bool{}
	ids := map[string]bool{}
	for _, res := range []declare.Result{first, second} {
		for _, seeded := range res.Entities {
			require.False(t, names[seeded.Name], "duplicate generated name %q across namespaces", seeded.Name)
			require.False(t, ids[seeded.ID], "duplicate id %q across namespaces", seeded.ID)
			names[seeded.Name] = true
			ids[seeded.ID] = true
		}
	}

	requireCleaned(t, ctx, database, []string{first.Namespace, second.Namespace}, factory.DefaultSeed)
	assert.Equal(t, int64(0), measureResidue(t, ctx, database, first.Namespace, factory.DefaultSeed).total())
	assert.Equal(t, int64(0), measureResidue(t, ctx, database, second.Namespace, factory.DefaultSeed).total())
}

// Seeding into an occupied namespace is refused, and two concurrent runs on the
// same namespace resolve to exactly one winner — by the reservation lock or by
// the occupancy read, both of which are correct refusals.
func TestDeclareRun_NamespaceOccupiedConflict(t *testing.T) {
	database, ctx := declareTestDB(t)

	namespace := declareNS(t)
	first := mustRun(t, ctx, database, "DSH-005", namespace)

	_, err := declare.Run(ctx, database, "DSH-005", namespace, factory.DefaultSeed)
	require.Error(t, err)
	assert.ErrorIs(t, err, declare.ErrNamespaceOccupied)

	requireCleaned(t, ctx, database, []string{first.Namespace}, factory.DefaultSeed)

	// Concurrent: exactly one may succeed.
	concurrent := declareNS(t)
	var wg sync.WaitGroup
	results := make([]error, 2)
	wg.Add(2)
	for i := range results {
		go func(i int) {
			defer wg.Done()
			_, err := declare.Run(ctx, database, "DSH-005", concurrent, factory.DefaultSeed)
			results[i] = err
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
			continue
		}
		assert.True(t,
			errors.Is(err, declare.ErrNamespaceBusy) || errors.Is(err, declare.ErrNamespaceOccupied),
			"the loser must be refused by the reservation or the occupancy read, got: %v", err)
	}
	assert.Equal(t, 1, succeeded, "exactly one concurrent run on one namespace may succeed")
	requireCleaned(t, ctx, database, []string{concurrent}, factory.DefaultSeed)
}

// A held reservation is answered IMMEDIATELY, not queued behind. A caller
// blocked on a same-namespace lock is by definition a duplicate.
func TestDeclareRun_HeldLockConflicts(t *testing.T) {
	database, ctx := declareTestDB(t)
	namespace := declareNS(t)

	conn, err := database.Pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	holder := repository.NewSyntheticSupportRepository(db.New(conn))
	key := declare.AdvisoryKeyForTest("declare:" + namespace)
	held, err := holder.TryAdvisoryLock(ctx, key)
	require.NoError(t, err)
	require.True(t, held)
	defer func() { _, _ = holder.AdvisoryUnlock(context.Background(), key) }()

	// replay.NewStopwatch is the toolkit's single audited real-clock site (the
	// repo bans direct time.Now()); elapsed wall time is infrastructure timing,
	// not domain time, so it must not be read off the accelerated clock either.
	sw := replay.NewStopwatch()
	_, err = declare.Run(ctx, database, "DSH-005", namespace, factory.DefaultSeed)
	elapsed := sw.Elapsed()

	require.ErrorIs(t, err, declare.ErrNamespaceBusy)
	assert.Less(t, elapsed, 5*time.Second, "a held reservation must be reported, not waited on")
}

// A failed run must RELEASE its reservation. A stranded session lock would
// serialize that namespace for the lifetime of the process, and the symptom
// (every later seed reporting busy) looks nothing like its cause.
func TestDeclareRun_LockReleasedOnFailure(t *testing.T) {
	database, ctx := declareTestDB(t)
	namespace := declareNS(t)

	restore := declare.SetFailpointForTest(declare.FailpointAfterFirstEntity)
	_, err := declare.Run(ctx, database, "CAD-026", namespace, factory.DefaultSeed)
	restore()
	require.Error(t, err)

	// The partial world still occupies the namespace, so clean it first; the
	// point of this test is that the LOCK is free, which the cleanup itself
	// would also report as busy if it were not.
	requireCleaned(t, ctx, database, []string{namespace}, factory.DefaultSeed)

	res, err := declare.Run(ctx, database, "CAD-026", namespace, factory.DefaultSeed)
	require.NoError(t, err, "the namespace must be re-seedable after a failed run released its lock")
	requireCleaned(t, ctx, database, []string{res.Namespace}, factory.DefaultSeed)
}

// A descendant of a LIVE namespace may never seed: its rows sit inside the
// ancestor's prefix sweep, so a later cleanup of the ancestor would destroy them.
func TestDeclareRun_DescendantNamespaceRejected(t *testing.T) {
	database, ctx := declareTestDB(t)

	parent := declareNS(t)
	res := mustRun(t, ctx, database, "DSH-005", parent)

	_, err := declare.Run(ctx, database, "DSH-005", parent+"-child", factory.DefaultSeed)
	require.ErrorIs(t, err, declare.ErrNamespaceNested)

	requireCleaned(t, ctx, database, []string{res.Namespace}, factory.DefaultSeed)

	// Once the ancestor is gone the descendant is free to seed.
	child, err := declare.Run(ctx, database, "DSH-005", parent+"-child", factory.DefaultSeed)
	require.NoError(t, err)
	requireCleaned(t, ctx, database, []string{child.Namespace}, factory.DefaultSeed)
}

// A HOST-ONLY residue under a SALTED variant occupies the requested namespace,
// and the refusal happens BEFORE construction.
//
// The residue shape is real: a constructor failure whose best-effort host
// removal also failed leaves exactly this — a marker and nothing else, under a
// token the client never asked for. The occupancy check has to probe every
// salted variant by EXACT hostname to see it, because the contact sweep finds no
// contacts and the requested token's own host does not exist. Without that probe
// this run would re-salt onto the residue's variant (its own band being taken)
// and materialize a second world on top of it — two worlds sharing one
// namespace, one of which the next cleanup would half-delete.
func TestDeclareRun_SaltedHostResidueOccupiesNamespace(t *testing.T) {
	database, ctx := declareTestDB(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	requested := declareNS(t)
	firstSalt := requested + "-s1"

	_, err := support.SeedRevokedMacHost(ctx, hostnameFor(firstSalt))
	require.NoError(t, err)

	// Occupy the REQUESTED namespace's band, which is what would push
	// construction onto the residue's variant.
	requestedGen := factory.NewGenerator(factory.DefaultSeed, requested)
	require.NoError(t, support.InsertTelegramChatConfigInBand(ctx, requestedGen.PeerBandStart()))
	t.Cleanup(func() {
		_, _ = support.DeleteTelegramChatConfigsByChatIds(context.Background(), []int64{requestedGen.PeerBandStart()})
	})

	_, err = declare.Run(ctx, database, "DSH-005", requested, factory.DefaultSeed)
	require.ErrorIs(t, err, declare.ErrNamespaceOccupied)

	// Before construction: the prefix covers the salted variants too, so an
	// empty result means no world was built under ANY token of this family.
	contactIDs, err := support.SelectContactIDsByFullNamePrefix(ctx, requestedGen.Prefix())
	require.NoError(t, err)
	assert.Empty(t, contactIDs, "the run must be refused before it constructs anything")

	// The refusal is actionable: the residue is reachable and reclaimable by the
	// REQUESTED token, which is the only token the client ever knew.
	requireCleaned(t, ctx, database, []string{requested}, factory.DefaultSeed)
	_, exists, err := support.SelectMacHostIDByHostname(ctx, hostnameFor(firstSalt))
	require.NoError(t, err)
	assert.False(t, exists, "cleanup must reclaim the salted host-only residue")
}

// A run failing mid-world reports honest recovery metadata, and the partial
// world it left is fully reachable by the requested token afterwards.
func TestDeclareRun_MidReplayFailureRecovery(t *testing.T) {
	database, ctx := declareTestDB(t)
	namespace := declareNS(t)

	restore := declare.SetFailpointForTest(declare.FailpointAfterFirstEntity)
	_, err := declare.Run(ctx, database, "CAD-026", namespace, factory.DefaultSeed)
	restore()

	require.Error(t, err)
	var runErr *declare.RunError
	require.ErrorAs(t, err, &runErr)
	assert.Equal(t, namespace, runErr.Namespace)

	if runErr.Cleaned {
		assert.Equal(t, int64(0), measureResidue(t, ctx, database, namespace, factory.DefaultSeed).total(),
			"a run reporting cleaned=true must have left nothing behind")
	} else {
		assert.Greater(t, measureResidue(t, ctx, database, namespace, factory.DefaultSeed).total(), int64(0),
			"a run reporting cleaned=false must actually have left something")
	}

	// Either way the namespace is reachable and reclaimable by its token.
	requireRetriableCleanup(t, ctx, database, namespace)
	assert.Equal(t, int64(0), measureResidue(t, ctx, database, namespace, factory.DefaultSeed).total())
}

// The post-host constructor failure path: the toolkit removes the marker it
// inserted, and the metadata says so. Reporting a blanket cleaned=false here
// would send a client cleaning up a namespace that is already empty; reporting
// a blanket true would strand real residue.
func TestDeclareRun_ConstructorFailureCleanedTruthful(t *testing.T) {
	database, ctx := declareTestDB(t)
	namespace := declareNS(t)

	restore := replay.SetConstructorFailpointForTest(replay.ConstructorFailpointAfterHost)
	_, err := declare.Run(ctx, database, "DSH-005", namespace, factory.DefaultSeed)
	restore()

	require.Error(t, err)
	var runErr *declare.RunError
	require.ErrorAs(t, err, &runErr)
	assert.True(t, runErr.Cleaned, "the constructor removed its own host marker, so cleaned must be true")
	assert.False(t, errors.Is(err, replay.ErrConstructorResidue))

	support := repository.NewSyntheticSupportRepository(database.Queries)
	_, exists, err := support.SelectMacHostIDByHostname(ctx, hostnameFor(namespace))
	require.NoError(t, err)
	assert.False(t, exists, "the namespaced host row must be gone")

	// Companion: a host-only world (the shape a FAILED removal would leave) is
	// still reachable and reclaimable by the requested token.
	_, err = support.SeedRevokedMacHost(ctx, hostnameFor(namespace))
	require.NoError(t, err)
	requireCleaned(t, ctx, database, []string{namespace}, factory.DefaultSeed)
	_, exists, err = support.SelectMacHostIDByHostname(ctx, hostnameFor(namespace))
	require.NoError(t, err)
	assert.False(t, exists, "cleanup must reclaim a host-only residue world")
}

// The constructor-residue direction: when the best-effort host removal ITSELF
// fails, rows genuinely remain, and the metadata must say cleaned=false. Proving
// only the true direction would leave a blanket `true` indistinguishable from an
// honest one.
func TestDeclareRun_ConstructorResidueReportsUncleaned(t *testing.T) {
	database, ctx := declareTestDB(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)
	namespace := declareNS(t)

	restore := replay.SetConstructorFailpointForTest(replay.ConstructorFailpointAfterHostResidue)
	_, err := declare.Run(ctx, database, "DSH-005", namespace, factory.DefaultSeed)
	restore()

	require.Error(t, err)
	require.ErrorIs(t, err, replay.ErrConstructorResidue)
	var runErr *declare.RunError
	require.ErrorAs(t, err, &runErr)
	assert.Equal(t, namespace, runErr.Namespace)
	assert.False(t, runErr.Cleaned, "the removal failed, so residue remains and cleaned must report it")

	// The claim is checkable: the marker the removal failed to delete is there.
	_, exists, err := support.SelectMacHostIDByHostname(ctx, hostnameFor(namespace))
	require.NoError(t, err)
	require.True(t, exists, "cleaned=false must correspond to real residue, not a pessimistic default")

	// And the residue is reclaimable by the requested token, which is what makes
	// the honest report actionable.
	requireCleaned(t, ctx, database, []string{namespace}, factory.DefaultSeed)
	_, exists, err = support.SelectMacHostIDByHostname(ctx, hostnameFor(namespace))
	require.NoError(t, err)
	assert.False(t, exists)
}

// A failed lock RELEASE is not something to log and continue past. The re-salt
// swap releases the requested namespace's bands and then BLOCKS on the effective
// namespace's; continuing with a hold it can no longer account for is
// hold-and-wait, the deadlock shape the release-before-acquire ordering exists
// to prevent. The run must abort — and the locks must not survive it, because
// destroying the session's connection is the only thing that can free a session
// lock nobody can account for.
//
// BOTH of pg_advisory_unlock's failure shapes are exercised. The `not-held`
// return is the one worth spelling out: it carries no error, so a caller that
// inspected only `err` would walk straight past it — while the state it reports
// (the session did not hold what it thought it held) is precisely the state the
// abort rule exists to refuse to continue from.
//
// The freed set is the COMPLETE reservation family — the requested token plus
// every salted variant — because the run took all of them. Sampling two of the
// eight would leave a release path that stranded any of the rest looking green,
// and a stranded session lock serializes that token for the life of the process.
func TestDeclareRun_UnlockFailureAbortsReSaltSwap(t *testing.T) {
	database, cfg, ctx := declareTestDBWithConfig(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)
	// Off the run's own pool: see newForeignLockObserver — a shared pool can
	// hand back the stranded connection, on which every lock looks free.
	holder := newForeignLockObserver(t, ctx, cfg)

	for _, mode := range []string{declare.UnlockFailpointError, declare.UnlockFailpointNotHeld} {
		t.Run(mode, func(t *testing.T) {
			requested := declareNS(t)
			gen := factory.NewGenerator(factory.DefaultSeed, requested)
			require.NoError(t, support.InsertTelegramChatConfigInBand(ctx, gen.PeerBandStart()))
			t.Cleanup(func() {
				_, _ = support.DeleteTelegramChatConfigsByChatIds(context.Background(), []int64{gen.PeerBandStart()})
			})

			restore := declare.SetUnlockFailpointForTest(mode)
			_, err := declare.Run(ctx, database, "DSH-005", requested, factory.DefaultSeed)
			restore()
			require.Error(t, err, "a release that failed must abort the run, not be swallowed")
			// Non-vacuous: the run must have failed AT the swap's release, not
			// somewhere earlier that never exercised the rule.
			require.ErrorContains(t, err, "before the re-salt swap")

			// Every reservation the run took is free again: the broken session's
			// connection was destroyed rather than returned to the pool still holding.
			for _, token := range declare.NamespaceFamilyForTest(requested) {
				key := declare.AdvisoryKeyForTest("declare:" + token)
				acquired, lockErr := holder.TryAdvisoryLock(ctx, key)
				require.NoError(t, lockErr)
				assert.True(t, acquired, "the reservation for %s was stranded by the failed release", token)
				if acquired {
					_, _ = holder.AdvisoryUnlock(ctx, key)
				}
			}

			requireRetriableCleanup(t, ctx, database, requested)
		})
	}
}

// The other half of the complete-family guarantee: the reservation ACQUIRES the
// whole family, so holding any single salted variant stands a run down.
//
// This matters because construction picks the effective namespace INTERNALLY.
// A variant left unreserved could be seeded by a second run — or cleaned out
// from under this one — in the window between "the run started" and "the run
// discovered it had re-salted onto that variant".
func TestDeclareRun_ReservationCoversSaltVariants(t *testing.T) {
	database, ctx := declareTestDB(t)
	namespace := declareNS(t)

	// The run's own pool is sound HERE (unlike the release test): the holder
	// keeps its connection checked out for the whole test, so the pool cannot
	// hand that same session to the run and let it re-enter its own lock.
	conn, err := database.Pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	holder := repository.NewSyntheticSupportRepository(db.New(conn))

	family := declare.NamespaceFamilyForTest(namespace)
	require.Equal(t, namespace, family[0], "the requested token leads the family")
	require.Contains(t, family, namespace+"-s7", "the family must span the whole salt budget")

	// One variant at a time: a reservation that covered only some of them would
	// let the run straight through on the ones it missed.
	for _, variant := range family[1:] {
		key := declare.AdvisoryKeyForTest("declare:" + variant)
		held, lockErr := holder.TryAdvisoryLock(ctx, key)
		require.NoError(t, lockErr)
		require.True(t, held, "the test could not take the variant lock for %s", variant)

		_, runErr := declare.Run(ctx, database, "DSH-005", namespace, factory.DefaultSeed)
		assert.ErrorIs(t, runErr, declare.ErrNamespaceBusy,
			"a held reservation on variant %s must stand the run down", variant)

		released, unlockErr := holder.AdvisoryUnlock(ctx, key)
		require.NoError(t, unlockErr)
		require.True(t, released)
	}
}

// The harness runs a live River client on the shared `default` queue, and River
// fetches ANY available job there — no kind or namespace filter — while a nil
// Work result FINALIZES the job. The two kinds the harness registers no-op
// workers for would therefore swallow another process's work silently. A foreign
// job must come back un-finalized, and the snooze counter is what makes that a
// POSITIVE observation: a job nobody ever fetched is also un-finalized.
//
// BOTH no-op kinds are planted, because they resolve ownership by DIFFERENT
// routes and a regression in one is invisible to the other:
//   - rematch_dispatcher's args carry the contact id, so ownership is a direct
//     contact lookup;
//   - knowledge_cache_updater's args carry only an event id, so ownership has to
//     resolve the event's SUBJECT contact first. A broken event-subject
//     predicate (or a kind that quietly stopped being ownership-checked at all)
//     would finalize the cache job while the rematch job still looked correct.
func TestDeclareRun_ForeignRiverJobIsNotStolen(t *testing.T) {
	database, ctx := declareTestDB(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	// A contact in a DIFFERENT namespace than the one about to seed.
	foreignNS := declareNS(t)
	foreign := mustRun(t, ctx, database, "DSH-005", foreignNS)
	foreignContactID := uuid.MustParse(foreign.Entities["refresh-target"].ID)

	rematchJobID, err := support.InsertAvailableRematchJobForContact(ctx, foreignContactID)
	require.NoError(t, err)

	// The cache job must point at a REAL foreign event: an unresolvable event id
	// is refused by the "not provably ours" fallback and would prove nothing
	// about the subject resolution itself.
	foreignEventIDs, err := support.ListEventIdsForContacts(ctx, []uuid.UUID{foreignContactID})
	require.NoError(t, err)
	require.NotEmpty(t, foreignEventIDs, "the foreign world must own an event for the cache job to reference")
	foreignPrefix := factory.NewGenerator(factory.DefaultSeed, foreign.Namespace).Prefix()
	ownedByForeign, err := support.NamespaceOwnsEventSubject(ctx, foreignEventIDs[0], foreignPrefix)
	require.NoError(t, err)
	require.True(t, ownedByForeign,
		"the planted event must resolve to the FOREIGN namespace's subject — otherwise the seeding harness would be refusing an id it simply could not resolve")
	cacheJobID, err := support.InsertAvailableKnowledgeCacheJobForEvent(ctx, foreignEventIDs[0])
	require.NoError(t, err)

	// Observe while the SECOND namespace's client is alive and fetching.
	var observedRematch, observedCache repository.RiverJobDisposition
	restore := declare.SetTestHookForTest(declare.HookAfterReplayBeforeDrain,
		func(hookCtx context.Context, _ *replay.Harness) error {
			observedRematch = pollJobDisposition(hookCtx, support, rematchJobID, 30*time.Second)
			observedCache = pollJobDisposition(hookCtx, support, cacheJobID, 30*time.Second)
			return nil
		})
	namespace := declareNS(t)
	seeded, err := declare.Run(ctx, database, "DSH-005", namespace, factory.DefaultSeed)
	restore()
	require.NoError(t, err)

	for _, observed := range []struct {
		kind string
		got  repository.RiverJobDisposition
	}{
		{"rematch_dispatcher", observedRematch},
		{"knowledge_cache_updater", observedCache},
	} {
		assert.Greater(t, observed.got.Snoozes, int64(0),
			"the harness must have FETCHED the foreign %s job — without that, un-finalized proves nothing", observed.kind)
		assert.False(t, observed.got.Finalized,
			"the harness finalized a foreign %s job", observed.kind)
		assert.NotEqual(t, "completed", observed.got.State, "foreign %s job", observed.kind)
	}

	require.NoError(t, support.FinalizeRiverJobByID(ctx, rematchJobID))
	require.NoError(t, support.FinalizeRiverJobByID(ctx, cacheJobID))
	requireCleaned(t, ctx, database, []string{foreignNS, namespace}, factory.DefaultSeed)
	assert.Equal(t, int64(0), measureResidue(t, ctx, database, seeded.Namespace, factory.DefaultSeed).total())
}

// A client disconnect must not cancel the run: River silently stops fetching
// when its start context dies, so a request-scoped context would strand the
// seed mid-settle. The detach is asserted, not assumed.
func TestDeclareRun_ClientDisconnectDoesNotCancel(t *testing.T) {
	database, ctx := declareTestDB(t)
	namespace := declareNS(t)

	requestCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := declare.Run(requestCtx, database, "DSH-005", namespace, factory.DefaultSeed)
		done <- err
	}()
	// Cancel while the run is in flight.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "a cancelled request context must not cancel the detached run")
	case <-time.After(declare.WorstCaseRunResidence()):
		t.Fatal("the detached run did not complete within its own bound")
	}
	requireCleaned(t, ctx, database, []string{namespace}, factory.DefaultSeed)
}

// The run budget is a real bound, not a hope: with a tiny budget the run fails
// LOUDLY and returns inside the honest worst case (budget + one in-flight
// toolkit settle timer + teardown's own gate timer + the teardown budget).
func TestDeclareRun_BudgetBoundsRun(t *testing.T) {
	database, ctx := declareTestDB(t)
	namespace := declareNS(t)

	restore := declare.SetBudgetsForTest(1*time.Millisecond, 20*time.Second)
	bound := declare.WorstCaseRunResidence()
	sw := replay.NewStopwatch()
	_, err := declare.Run(ctx, database, "CAD-026", namespace, factory.DefaultSeed)
	elapsed := sw.Elapsed()
	restore()

	require.Error(t, err, "an expired budget must fail loudly rather than hang")
	assert.LessOrEqual(t, elapsed, bound+15*time.Second,
		"run took %s, outside the stated worst case %s", elapsed, bound)

	// The reservation was released on the failure path, so the namespace is
	// immediately reclaimable.
	requireRetriableCleanup(t, ctx, database, namespace)
}

// requireRetriableCleanup cleans a namespace, tolerating the retriable states
// a just-failed run can legitimately leave (a job still finalizing).
func requireRetriableCleanup(t *testing.T, ctx context.Context, database *db.Database, namespace string) {
	t.Helper()
	waitFor(t, 60*time.Second, "namespace "+namespace+" to clean", func() bool {
		res, err := declare.CleanupNamespaces(ctx, database, []string{namespace}, factory.DefaultSeed)
		if err != nil {
			return false
		}
		for _, outcome := range res.Results {
			if outcome.Status != declare.StatusCleaned {
				return false
			}
		}
		return true
	})
}

// A seed may not report success while a job it created still references its
// rows: the whole point of the drained Gate B is that a 2xx guarantees a
// QUIESCENT namespace, which is what makes the later stateless, id-set based
// cleanup safe to delete under.
func TestDeclareRun_GateBUnclearedFailsSeed(t *testing.T) {
	database, ctx := declareTestDB(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)
	namespace := declareNS(t)

	var plantedJobID int64
	var observedPending int64
	restore := declare.SetTestHookForTest(declare.HookAfterReplayBeforeDrain,
		func(hookCtx context.Context, h *replay.Harness) error {
			// Plant an AGGREGATE-class job: it keys on {contact_id, source} and
			// needs no event id, so the hook needs nothing but the namespace.
			contactIDs, err := support.SelectContactIDsByFullNamePrefix(hookCtx, h.Generator().Prefix())
			if err != nil {
				return err
			}
			if len(contactIDs) == 0 {
				return fmt.Errorf("hook: no seeded contacts to attach a job to")
			}
			plantedJobID, err = support.InsertUnfinalizedAggregateJobForContact(
				hookCtx, contactIDs[0], repository.InteractionSourceMessages)
			if err != nil {
				return err
			}
			observedPending, err = support.CountPendingJobsForNamespaceCleanup(hookCtx, nil, contactIDs)
			return err
		})
	_, err := declare.Run(ctx, database, "DSH-005", namespace, factory.DefaultSeed)
	restore()

	// The injection landed — assert that BEFORE reading anything into the
	// failure, or a gate that never fired would look identical to one that did.
	require.Greater(t, observedPending, int64(0), "the planted job must be visible to the gate")

	require.Error(t, err, "an uncleared Gate B must fail the seed")
	var runErr *declare.RunError
	require.ErrorAs(t, err, &runErr)
	assert.False(t, runErr.Cleaned,
		"teardown leaves the rows when Gate B is uncleared, so cleaned must report false")

	require.NoError(t, support.FinalizeRiverJobByID(ctx, plantedJobID))
	requireCleaned(t, ctx, database, []string{namespace}, factory.DefaultSeed)
}

// The release-acquire gap D13's revalidation closes. A third run can claim the
// effective namespace's band between releasing the requested band locks and
// acquiring the effective ones; executing on the pre-swap read would recreate
// exactly the collision the locks exist to prevent. The run must instead tear
// down, retry construction, and land somewhere genuinely free.
func TestDeclareRun_ReSaltSwapRevalidates(t *testing.T) {
	database, ctx := declareTestDB(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	requested := declareNS(t)
	firstSalt := requested + "-s1"

	// Force the first re-salt by occupying the requested namespace's band.
	requestedGen := factory.NewGenerator(factory.DefaultSeed, requested)
	require.NoError(t, support.InsertTelegramChatConfigInBand(ctx, requestedGen.PeerBandStart()))

	// ...then, inside the swap gap, occupy the band the run just re-salted onto.
	var once sync.Once
	var hookErr error
	restore := declare.SetTestHookForTest(declare.HookAfterBandSwapBeforeRevalidate,
		func(hookCtx context.Context, h *replay.Harness) error {
			once.Do(func() {
				saltGen := factory.NewGenerator(factory.DefaultSeed, firstSalt)
				hookErr = support.InsertTelegramChatConfigInBand(hookCtx, saltGen.PeerBandStart())
			})
			return hookErr
		})
	res, err := declare.Run(ctx, database, "DSH-005", requested, factory.DefaultSeed)
	restore()
	require.NoError(t, err)

	assert.NotEqual(t, requested, res.Namespace)
	assert.NotEqual(t, firstSalt, res.Namespace,
		"the run must NOT have executed against the stale pre-swap read")

	// The world it did land on owns a band nobody else claimed.
	landedGen := factory.NewGenerator(factory.DefaultSeed, res.Namespace)
	assert.NotEqual(t, requestedGen.PeerBandStart(), landedGen.PeerBandStart())
	assert.NotEqual(t, factory.NewGenerator(factory.DefaultSeed, firstSalt).PeerBandStart(), landedGen.PeerBandStart())

	requireCleaned(t, ctx, database, []string{requested}, factory.DefaultSeed)
	assert.Equal(t, int64(0), measureResidue(t, ctx, database, res.Namespace, factory.DefaultSeed).total())

	_, _ = support.DeleteTelegramChatConfigsByChatIds(ctx, []int64{
		requestedGen.PeerBandStart(),
		factory.NewGenerator(factory.DefaultSeed, firstSalt).PeerBandStart(),
	})
}
