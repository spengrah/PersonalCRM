//go:build integration_testdb

package tests

import (
	"context"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/declare"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/synthetic/replay"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FIRST-ORDER product contracts of the declared seed. Neither is a test of a
// fixture: one is a data-loss guarantee about the live application's job queue,
// the other is the cleanup endpoint's atomicity guarantee. Both are UNGATED so
// they block a merge — a contract that only runs on the nightly cron gates
// nothing.

func declareIsolationNS(t *testing.T) string {
	t.Helper()
	return "q" + uuid.NewString()[:8]
}

func declareIsolationDB(t *testing.T) (*db.Database, context.Context) {
	t.Helper()
	ctx := context.Background()
	// A per-test clone: this test asserts over river_job, and a live River client
	// draining a shared database would steal sibling tests' jobs.
	database, _ := newIsolatedRiverTestDB(t, ctx)
	return database, ctx
}

// TestSyntheticDeclareQueueIsolation_ForeignJobIsNotFetched pins the guarantee
// that a declared seed's River client never touches the live application's work.
//
// The seed starts a real River client. River's fetch selects ANY available job in
// the queues it is configured for — there is no kind or namespace filter — so two
// production halves keep it off `default`: the client is configured for the
// namespace's private queue only, and an insert hook rewrites every job it
// enqueues onto that same queue. If either half regresses, a declared seed can
// finalize or fail unrelated application jobs, which is silent data loss.
//
// The existing queue tests assert only that the queue NAME is well formed and
// per-namespace; nothing else exercises the client's fetch SCOPE.
//
// `attempt` is the load-bearing observable: River increments it on FETCH, so
// attempt = 0 is the positive statement that no worker was ever handed the job.
// "Not finalized" is satisfied by a job nobody looked at, and by a job that was
// fetched and merely failed.
func TestSyntheticDeclareQueueIsolation_ForeignJobIsNotFetched(t *testing.T) {
	database, ctx := declareIsolationDB(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	// A world in a DIFFERENT namespace, so the planted job belongs to nobody the
	// seed under test owns.
	foreignNS := declareIsolationNS(t)
	foreign, err := declare.Run(ctx, database, "DSH-005", foreignNS, factory.DefaultSeed)
	require.NoError(t, err)
	foreignContact := uuid.MustParse(foreign.Entities["refresh-target"].ID)

	// An AVAILABLE (immediately fetchable) job on `default` — the queue the live
	// application works. The fetch has to be POSSIBLE for "never fetched" to mean
	// anything.
	jobID, err := support.InsertAvailableRematchJobForContact(ctx, foreignContact)
	require.NoError(t, err)

	before, err := support.GetRiverJobDisposition(ctx, jobID)
	require.NoError(t, err)
	require.Equal(t, "available", before.State, "the planted job must start fetchable")
	require.Equal(t, int32(0), before.Attempt)

	// Now run a seed whose client is alive and fetching. It drains its own Gate B,
	// so there is real fetch activity for the duration.
	namespace := declareIsolationNS(t)
	seeded, err := declare.Run(ctx, database, "DSH-005", namespace, factory.DefaultSeed)
	require.NoError(t, err, "the seed must COMPLETE — that is what proves its client was fetching at all")

	after, err := support.GetRiverJobDisposition(ctx, jobID)
	require.NoError(t, err)
	assert.Equal(t, int32(0), after.Attempt,
		"the seed's client FETCHED a foreign default-queue job (attempt %d, state %q)", after.Attempt, after.State)
	assert.False(t, after.Finalized, "the seed's client finalized a foreign job")
	assert.Equal(t, "available", after.State, "the foreign job must be exactly as it was planted")

	// And the queue it ran on really was its own — a harness queue that resolved
	// to `default` would isolate nothing, so attempt=0 above would prove nothing.
	require.NotEqual(t, river.QueueDefault, replay.SyntheticQueueName(seeded.Namespace))

	require.NoError(t, support.FinalizeRiverJobByID(ctx, jobID))
	res, err := declare.CleanupNamespaces(ctx, database, []string{foreignNS, namespace}, factory.DefaultSeed)
	require.NoError(t, err)
	for ns, outcome := range res.Results {
		assert.Equal(t, declare.StatusCleaned, outcome.Status, "namespace %s: %s", ns, outcome.Err)
	}
}

// TestSyntheticDeclareCleanup_FailedSweepDeletesNothing pins the cleanup
// endpoint's ALL-OR-NOTHING contract: a sweep that fails partway deletes
// nothing, leaving the namespace discoverable, occupied and fully recoverable by
// a retry.
//
// The contract is not cosmetic. The ladder deletes event_consumer_claims and
// venue_nodes BEFORE contacts, and both are reachable only through the contacts
// the sweep is walking — so a best-effort ladder that deleted the early steps and
// then failed would orphan those rows permanently, with nothing left to find them
// by. The production code states this guarantee in the error string it returns;
// this is what makes the guarantee true rather than advertised.
//
// A mid-ladder failure has to be injected: every step is a delete by tracked id
// against rows the run itself just created, so none can be made to fail on
// demand.
func TestSyntheticDeclareCleanup_FailedSweepDeletesNothing(t *testing.T) {
	database, ctx := declareIsolationDB(t)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	namespace := declareIsolationNS(t)
	// CAD-026's contacts carry replayed history, so the namespace owns events —
	// rows an EARLIER ladder step deletes than the step this test fails at.
	seeded, err := declare.Run(ctx, database, "CAD-026", namespace, factory.DefaultSeed)
	require.NoError(t, err)
	prefix := factory.NewGenerator(factory.DefaultSeed, seeded.Namespace).Prefix()

	contactsBefore, err := support.SelectContactIDsByFullNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.NotEmpty(t, contactsBefore, "the world must exist before it is swept")
	eventsBefore, err := support.ListEventIdsForContacts(ctx, contactsBefore)
	require.NoError(t, err)
	require.NotEmpty(t, eventsBefore,
		"this test needs a namespace whose events are deleted EARLIER than the failing step")

	// Fail at `contacts`, which the ladder reaches AFTER `events`.
	restore := declare.SetCleanupFailStepForTest("contacts")
	failed, err := declare.CleanupNamespaces(ctx, database, []string{seeded.Namespace}, factory.DefaultSeed)
	restore()
	require.NoError(t, err, "an injected step failure is reported per namespace, not as a call error")
	require.Equal(t, declare.StatusError, failed.Results[seeded.Namespace].Status,
		"a failed sweep must report error, not cleaned")

	// THE CONTRACT: the earlier steps' rows survive. If the ladder were
	// best-effort rather than transactional, the events would be gone while the
	// contacts remained, and nothing could ever find them again.
	eventsAfter, err := support.ListEventIdsForContacts(ctx, contactsBefore)
	require.NoError(t, err)
	assert.ElementsMatch(t, eventsBefore, eventsAfter,
		"the failed sweep deleted events that an earlier ladder step had already removed inside the transaction")
	contactsAfter, err := support.SelectContactIDsByFullNamePrefix(ctx, prefix)
	require.NoError(t, err)
	assert.ElementsMatch(t, contactsBefore, contactsAfter, "the failed sweep deleted contacts")

	// And the namespace is genuinely recoverable: an unarmed retry empties it.
	retried, err := declare.CleanupNamespaces(ctx, database, []string{seeded.Namespace}, factory.DefaultSeed)
	require.NoError(t, err)
	require.Equal(t, declare.StatusCleaned, retried.Results[seeded.Namespace].Status,
		"the retry must succeed: %s", retried.Results[seeded.Namespace].Err)
	remaining, err := support.SelectContactIDsByFullNamePrefix(ctx, prefix)
	require.NoError(t, err)
	assert.Empty(t, remaining, "the retry must empty the namespace")
}
