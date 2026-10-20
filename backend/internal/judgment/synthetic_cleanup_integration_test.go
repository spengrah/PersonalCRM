//go:build integration_testdb

package judgment_test

import (
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/declare"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flagRecords is what recordFlagRecords stored on a thread, to read back.
type flagRecords struct {
	thread    uuid.UUID
	judgments []uuid.UUID
	run       judgment.RunID
}

// recordFlagRecords stores on the thread, through the judgment store, a
// judgment with flag decisions, a later judgment with a clearing decision of
// its commitment, and an unjudged message, all built by the factory from seed.
func (e *env) recordFlagRecords(t *testing.T, th thread, seed uint64) flagRecords {
	t.Helper()
	keys := e.messageKeys(t, th)
	require.NotEmpty(t, keys)
	gen := factory.NewGenerator(seed, "flags")
	kinds := make([]string, len(judgment.FlagKinds))
	for i, k := range judgment.FlagKinds {
		kinds[i] = string(k)
	}
	spec := gen.MessageFlags(keys, kinds)
	flags := make([]judgment.FlagDecision, len(spec.Flags))
	for i, f := range spec.Flags {
		flags[i] = judgment.FlagDecision{MessageKey: f.MessageKey, Kind: judgment.FlagKind(f.Kind), Probability: f.Probability}
	}
	run, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: judgment.RunKindRerun, Method: otherMethod})
	require.NoError(t, err)
	earlier := windowed(otherMethod, th, e.day(1), "flags-1", spec.WindowStart, spec.NewMessages, judgment.Outcomes{})
	earlier.Flags = flags
	_, err = e.store.RecordJudgment(e.ctx, run, earlier)
	require.NoError(t, err)
	first := e.viewOf(t, th, otherMethod, e.day(1))

	clearing := gen.Clearings(spec.NewMessages[:1])[0]
	later := windowed(otherMethod, th, e.day(2), "flags-2", spec.WindowStart, nil, judgment.Outcomes{})
	later.Clearings = []judgment.ClearingDecision{{
		CommitmentMessageKey: clearing.CommitmentMessageKey, CommitmentJudgment: first.ID, Done: clearing.Done, Probability: clearing.Probability,
	}}
	_, err = e.store.RecordJudgment(e.ctx, run, later)
	require.NoError(t, err)
	second := e.viewOf(t, th, otherMethod, e.day(2))

	unjudged := gen.UnjudgedMessages(keys, []string{string(judgment.UnjudgedReasonCallFailed)})[0]
	require.NoError(t, e.store.RecordUnjudged(e.ctx, run, []judgment.UnjudgedMessage{{
		Thread: th.id, CutOff: e.day(2), MessageKey: unjudged.MessageKey, Reason: judgment.UnjudgedReason(unjudged.Reason),
	}}))
	return flagRecords{thread: th.id, judgments: []uuid.UUID{first.ID, second.ID}, run: run}
}

// held counts the records the thread holds: judgments, flag decisions,
// clearing decisions and unjudged messages.
func (e *env) held(t *testing.T, r flagRecords) (judgments, flags, clearings, unjudged int) {
	t.Helper()
	q := e.database.Queries
	rows, err := q.ListJudgmentRows(e.ctx, db.ListJudgmentRowsParams{Ids: []uuid.UUID{}, ThreadID: &r.thread, ExampleIds: []string{}})
	require.NoError(t, err)
	flagRows, err := q.ListJudgmentFlags(e.ctx, r.judgments)
	require.NoError(t, err)
	clearingRows, err := q.ListJudgmentClearings(e.ctx, r.judgments)
	require.NoError(t, err)
	all, err := q.ListJudgmentUnjudged(e.ctx)
	require.NoError(t, err)
	for _, u := range all {
		if u.ThreadID == r.thread {
			unjudged++
		}
	}
	return len(rows), len(flagRows), len(clearingRows), unjudged
}

// The namespace cleanup of the replay harness and of a declared world removes
// the judgment, flag, clearing and unjudged records of the namespace's
// threads, and no other namespace's.
func TestSyntheticCleanupRemovesFlagRecords(t *testing.T) {
	t.Run("the replay harness's cleanup", func(t *testing.T) {
		e := newEnv(t, production)
		other := e.thread(t)
		e.message(t, other, e.contact(t), false, e.day(1))
		otherRecords := e.recordFlagRecords(t, other, factory.DefaultSeed)

		h, teardown, err := synthetic.NewHarnessWithDBForNamespace(e.ctx, e.database, "c"+uuid.NewString()[:8], factory.DefaultSeed)
		require.NoError(t, err)
		gen := h.Generator()
		spec := gen.Contact(factory.WithEmail())
		contact, err := h.SeedContact(e.ctx, spec)
		require.NoError(t, err)
		_, err = h.ReplayGmail(e.ctx, contact.ID, gen.GmailMessage(spec, factory.MatchSeeded))
		require.NoError(t, err)
		venues, err := repository.NewSyntheticSupportRepository(e.database.Queries).SelectVenueNodeIDsForContacts(e.ctx, []uuid.UUID{contact.ID})
		require.NoError(t, err)
		require.Len(t, venues, 1)
		records := e.recordFlagRecords(t, thread{id: venues[0]}, factory.DefaultSeed)
		judgments, flags, clearings, unjudged := e.held(t, records)
		require.Equal(t, []int{2, 3, 1, 1}, []int{judgments, flags, clearings, unjudged})

		require.NoError(t, teardown(e.ctx))

		judgments, flags, clearings, unjudged = e.held(t, records)
		assert.Equal(t, []int{0, 0, 0, 0}, []int{judgments, flags, clearings, unjudged}, "the namespace's thread holds none")
		judgments, flags, clearings, unjudged = e.held(t, otherRecords)
		assert.Equal(t, []int{2, 3, 1, 1}, []int{judgments, flags, clearings, unjudged}, "another namespace's records stay")
	})

	t.Run("a declared world's cleanup", func(t *testing.T) {
		e := newEnv(t, production)
		other := e.thread(t)
		e.message(t, other, e.contact(t), false, e.day(1))
		otherRecords := e.recordFlagRecords(t, other, factory.DefaultSeed)

		namespace := "d" + uuid.NewString()[:8]
		world, err := declare.Run(e.ctx, e.database, "IXN-002", namespace, factory.DefaultSeed)
		require.NoError(t, err)
		subject := uuid.MustParse(world.Entities["subject"].ID)
		venues, err := repository.NewSyntheticSupportRepository(e.database.Queries).SelectVenueNodeIDsForContacts(e.ctx, []uuid.UUID{subject})
		require.NoError(t, err)
		var th thread
		for _, venue := range venues {
			if len(e.messageKeys(t, thread{id: venue})) > 0 {
				th = thread{id: venue}
			}
		}
		require.NotEqual(t, uuid.Nil, th.id, "the declared world has a venue holding a message")
		records := e.recordFlagRecords(t, th, factory.DefaultSeed)
		judgments, flags, clearings, unjudged := e.held(t, records)
		require.Equal(t, []int{2, 3, 1, 1}, []int{judgments, flags, clearings, unjudged})

		result, err := declare.CleanupNamespaces(e.ctx, e.database, []string{namespace}, factory.DefaultSeed)
		require.NoError(t, err)
		for ns, r := range result.Results {
			require.Equal(t, declare.StatusCleaned, r.Status, "%s: %s", ns, r.Err)
		}

		judgments, flags, clearings, unjudged = e.held(t, records)
		assert.Equal(t, []int{0, 0, 0, 0}, []int{judgments, flags, clearings, unjudged}, "the namespace's thread holds none")
		judgments, flags, clearings, unjudged = e.held(t, otherRecords)
		assert.Equal(t, []int{2, 3, 1, 1}, []int{judgments, flags, clearings, unjudged}, "another namespace's records stay")
	})
}
