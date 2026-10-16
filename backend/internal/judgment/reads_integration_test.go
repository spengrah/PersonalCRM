//go:build integration_testdb

package judgment_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/judgment"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A recorded judgment reads back with its provenance, usage and raw answers;
// two methods that agree stay two records, and the accepted fact is a record
// apart from the judgment behind it.
func TestRecordedJudgmentReadsBack(t *testing.T) {
	t.Run("two methods with the same outcome on one input stay two records", func(t *testing.T) {
		// spec: KNW-043.record-reads-back, KNW-043.agreeing-methods-stay-two
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		outcomes := judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}}
		billed, cost := 512, 0.0042
		prod := judgmentOf(prodMethod, th, e.day(1), "h1", outcomes)
		prod.Usage.BilledTokens, prod.Usage.CostUSD = &billed, &cost
		other := judgmentOf(otherMethod, th, e.day(1), "h1", outcomes)
		prodRun, _ := e.run(t, judgment.RunKindScheduled, prod)
		otherRun, _ := e.run(t, judgment.RunKindRerun, other)

		views, err := e.store.ListJudgments(e.ctx, judgment.JudgmentQuery{Thread: &th.id})
		require.NoError(t, err)
		require.Len(t, views, 2)
		assert.NotEqual(t, views[0].ID, views[1].ID)
		assert.NotEqual(t, views[0].Outcomes[0].ID, views[1].Outcomes[0].ID)
		byMethod := map[judgment.MethodRef]judgment.JudgmentView{views[0].Method: views[0], views[1].Method: views[1]}
		for _, recorded := range []struct {
			run judgment.RunID
			j   judgment.Judgment
		}{{prodRun, prod}, {otherRun, other}} {
			run := recorded.run
			v, ok := byMethod[recorded.j.Method]
			require.True(t, ok, "a judgment of %s", recorded.j.Method.Key)
			assert.Equal(t, "h1", v.InputHash)
			assert.True(t, e.day(1).Equal(v.CutOff))
			require.Len(t, v.Runs, 1)
			assert.Equal(t, run, v.Runs[0].Run)

			records, err := e.store.JudgmentRecords(e.ctx, run)
			require.NoError(t, err)
			require.Len(t, records, 1)
			r := records[0]
			assert.Equal(t, v.Method, r.Method)
			assert.True(t, e.day(1).Add(time.Hour).Equal(r.JudgedAt), "the execution time")
			assert.Equal(t, prod.Questions, r.Questions, "every option's probability and the original answer")
			require.Len(t, r.Outcomes, 1)
			assert.Equal(t, outcomes.BallInCourt[0].QuestionID, r.Outcomes[0].QuestionID)
			assert.Equal(t, 420, r.Usage.ContextTokens)
			assert.Equal(t, 4, r.Usage.Questions)
			assert.Equal(t, 250*time.Millisecond, r.Usage.Latency)
		}
		records, err := e.store.JudgmentRecords(e.ctx, prodRun)
		require.NoError(t, err)
		assert.Equal(t, &billed, records[0].Usage.BilledTokens)
		assert.Equal(t, &cost, records[0].Usage.CostUSD)
	})

	t.Run("an auto-applied addition's accepted fact and its judgment are distinct records", func(t *testing.T) {
		// spec: KNW-043.agreeing-methods-stay-two
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}}))

		outcome := e.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &c)
		obligations := e.openObligations(t, th)
		require.Len(t, obligations, 1)
		assert.Equal(t, judgment.OutcomeStateApplied, outcome.State)
		assert.NotEqual(t, outcome.ID, obligations[0].TaskID)
		assert.Equal(t, string(judgment.BallInCourtIOweReply), outcome.Outcome)
	})

	t.Run("a source that reported no billed tokens and no cost reads back absent, not zero", func(t *testing.T) {
		// spec: KNW-043.unreported-usage-absent
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		run, _ := e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}}))
		require.NoError(t, e.store.FinishRun(e.ctx, run))

		records, err := e.store.JudgmentRecords(e.ctx, run)
		require.NoError(t, err)
		require.Len(t, records, 1)
		assert.Nil(t, records[0].Usage.BilledTokens)
		assert.Nil(t, records[0].Usage.CostUSD)
		finished := finishedRun(t, e, run)
		assert.Nil(t, finished.CostUSD)
	})
}

// ListRuns and ListJudgments serve the bench: runs with their incomplete
// threads, and judgments with every run they appear in and each outcome's
// state.
func TestBenchReads(t *testing.T) {
	t.Run("a judgment reused by a second run is listed under both runs at each one's cut-off", func(t *testing.T) {
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		first, _ := e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}}))
		second, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: judgment.RunKindScheduled, Method: prodMethod})
		require.NoError(t, err)
		_, found, err := e.store.ReuseJudgment(e.ctx, second, th.id, e.day(2), "h1")
		require.NoError(t, err)
		require.True(t, found)

		for _, run := range []judgment.RunID{first, second} {
			views, err := e.store.ListJudgments(e.ctx, judgment.JudgmentQuery{Run: &run})
			require.NoError(t, err)
			require.Len(t, views, 1)
			require.Len(t, views[0].Runs, 2)
			assert.Equal(t, first, views[0].Runs[0].Run)
			assert.True(t, e.day(1).Equal(views[0].Runs[0].CutOff))
			assert.Equal(t, second, views[0].Runs[1].Run)
			assert.True(t, e.day(2).Equal(views[0].Runs[1].CutOff))
			assert.True(t, e.day(1).Equal(views[0].CutOff), "the first appearance's cut-off")
		}
		runs, err := e.store.ListRuns(e.ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, runSummary(t, runs, second).JudgedCount, "reused appearances count")
	})

	t.Run("an evidence judgment beside a production judgment on one thread", func(t *testing.T) {
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		outcomes := judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}}
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1", outcomes))
		e.run(t, judgment.RunKindScheduled, judgmentOf(otherMethod, th, e.day(1), "h1", outcomes))

		views, err := e.store.ListJudgments(e.ctx, judgment.JudgmentQuery{Thread: &th.id})
		require.NoError(t, err)
		require.Len(t, views, 2)
		states := map[string]judgment.OutcomeState{}
		for _, v := range views {
			assert.Equal(t, th.id, v.Thread)
			states[v.Method.Key] = v.Outcomes[0].State
		}
		assert.Equal(t, map[string]judgment.OutcomeState{production: judgment.OutcomeStateApplied, otherMethod.Key: judgment.OutcomeStateEvidence}, states)
	})

	t.Run("an incomplete thread in a run", func(t *testing.T) {
		e := newEnv(t, production)
		th := e.thread(t)
		from, to := e.day(0), e.day(31)
		run, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: judgment.RunKindHistorical, Method: prodMethod, From: &from, To: &to})
		require.NoError(t, err)
		require.NoError(t, e.store.RecordIncomplete(e.ctx, run, th.id, e.day(3), judgment.IncompleteReasonOverLimit))
		require.NoError(t, e.store.FinishRun(e.ctx, run))

		runs, err := e.store.ListRuns(e.ctx)
		require.NoError(t, err)
		s := runSummary(t, runs, run)
		assert.Equal(t, judgment.RunKindHistorical, s.Kind)
		assert.Equal(t, prodMethod, s.Method)
		assert.True(t, from.Equal(*s.From) && to.Equal(*s.To))
		assert.NotNil(t, s.FinishedAt)
		require.Len(t, s.Incomplete, 1)
		assert.Equal(t, th.id, s.Incomplete[0].Thread)
		assert.True(t, e.day(3).Equal(s.Incomplete[0].CutOff))
		assert.Equal(t, judgment.IncompleteReasonOverLimit, s.Incomplete[0].Reason)
	})

	t.Run("a query with neither thread nor run is refused", func(t *testing.T) {
		e := newEnv(t, production)
		_, err := e.store.ListJudgments(e.ctx, judgment.JudgmentQuery{})
		require.ErrorIs(t, err, judgment.ErrUnboundedQuery)
	})
}

// ReviewedExamples returns each input the user reviewed, a review labelling it
// for every method, and OutcomesFor reads one method's outcomes on them.
// spec: KNW-048.reviewed-examples-label-every-method, KNW-048.one-opaque-example-id
func TestScorecardReads(t *testing.T) {
	t.Run("a review of one method's evidence outcome labels the example another method is read for", func(t *testing.T) {
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtNobodyOwes, 0.6)}}))
		prodOutcomes := judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.8)}}
		e.run(t, judgment.RunKindRerun, judgmentOf(prodMethod, th, e.day(1), "h1", prodOutcomes))
		_, err := e.store.ReviewOutcome(e.ctx, e.outcome(t, th, otherMethod, judgment.QuestionBallInCourt, &c).ID, judgment.ReviewRequest{RightOutcome: string(judgment.BallInCourtIOweReply)})
		require.NoError(t, err)

		examples, err := e.store.ReviewedExamples(e.ctx)
		require.NoError(t, err)
		example := exampleOf(t, examples, th.id)
		assert.Equal(t, "h1", example.InputHash)
		assert.True(t, e.day(1).Equal(example.CutOff))
		assert.Equal(t, []judgment.Review{{Question: string(judgment.QuestionBallInCourt), ContactID: &c, RightOutcome: string(judgment.BallInCourtIOweReply)}}, example.Reviews)

		outcomes, err := e.store.OutcomesFor(e.ctx, prodMethod, []judgment.ExampleID{example.ID})
		require.NoError(t, err)
		require.Contains(t, outcomes, example.ID)
		got := outcomes[example.ID].BallInCourt
		require.Len(t, got, 1)
		assert.Equal(t, judgment.BallInCourtIOweReply, got[0].Outcome)
		assert.Equal(t, c, got[0].ContactID)
		assert.Equal(t, prodOutcomes.BallInCourt[0].QuestionID, got[0].QuestionID)
		assert.InDelta(t, 0.8, got[0].Confidence, 0.01)
	})

	t.Run("two reviews on one example", func(t *testing.T) {
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, e.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtNobodyOwes, 0.6)},
			BigMoment:   judgment.Scored[judgment.BigMoment]{QuestionID: "big_moment", Outcome: judgment.BigMomentNone, Confidence: 0.7},
		}))
		_, err := e.store.ReviewOutcome(e.ctx, e.outcome(t, th, otherMethod, judgment.QuestionBallInCourt, &c).ID, judgment.ReviewRequest{RightOutcome: string(judgment.BallInCourtNobodyOwes)})
		require.NoError(t, err)
		_, err = e.store.ReviewOutcome(e.ctx, e.outcome(t, th, otherMethod, judgment.QuestionBigMoment, nil).ID, judgment.ReviewRequest{RightOutcome: string(judgment.BigMomentParticipant)})
		require.NoError(t, err)

		examples, err := e.store.ReviewedExamples(e.ctx)
		require.NoError(t, err)
		reviews := exampleOf(t, examples, th.id).Reviews
		assert.ElementsMatch(t, []judgment.Review{
			{Question: string(judgment.QuestionBallInCourt), ContactID: &c, RightOutcome: string(judgment.BallInCourtNobodyOwes)},
			{Question: string(judgment.QuestionBigMoment), RightOutcome: string(judgment.BigMomentParticipant)},
		}, reviews)
	})

	t.Run("a method with no judgment on an example is absent from its result", func(t *testing.T) {
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtNobodyOwes, 0.6)}}))
		_, err := e.store.ReviewOutcome(e.ctx, e.outcome(t, th, otherMethod, judgment.QuestionBallInCourt, &c).ID, judgment.ReviewRequest{RightOutcome: string(judgment.BallInCourtIOweReply)})
		require.NoError(t, err)
		examples, err := e.store.ReviewedExamples(e.ctx)
		require.NoError(t, err)
		example := exampleOf(t, examples, th.id)

		outcomes, err := e.store.OutcomesFor(e.ctx, prodMethod, []judgment.ExampleID{example.ID})

		require.NoError(t, err)
		assert.NotContains(t, outcomes, example.ID)
	})
}

// The export reads return content-free records keyed by example ID and
// question ID: no thread content, thread ID or contact ID.
// spec: KNW-048.export-records-content-free
func TestExportReads(t *testing.T) {
	// contentFree fails when a record names the thread or a contact, or carries
	// the thread's message body.
	contentFree := func(t *testing.T, record any, ids ...uuid.UUID) {
		t.Helper()
		raw, err := json.Marshal(record)
		require.NoError(t, err)
		for _, id := range ids {
			assert.NotContains(t, string(raw), id.String())
		}
		assert.NotContains(t, string(raw), "synthetic message body")
	}

	t.Run("a reused judgment is listed under its second run", func(t *testing.T) {
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}}))
		second, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: judgment.RunKindScheduled, Method: prodMethod})
		require.NoError(t, err)
		_, _, err = e.store.ReuseJudgment(e.ctx, second, th.id, e.day(2), "h1")
		require.NoError(t, err)
		require.NoError(t, e.store.FinishRun(e.ctx, second))

		records, err := e.store.JudgmentRecords(e.ctx, second)
		require.NoError(t, err)
		require.Len(t, records, 1)
		assert.True(t, e.day(2).Equal(records[0].CutOff), "the second run's cut-off")
		assert.NotEmpty(t, records[0].Example)
		contentFree(t, records, th.id, c)
		finished := finishedRun(t, e, second)
		assert.Equal(t, 1, finished.Judged)
		assert.Nil(t, finished.CostUSD, "a reused appearance made no call")
		contentFree(t, finished, th.id, c)
	})

	t.Run("a review of an auto-applied addition", func(t *testing.T) {
		e := newEnv(t, production)
		c, th := e.contact(t), e.thread(t)
		e.message(t, th, c, false, e.day(1))
		outcomes := judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)}}
		run, _ := e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1", outcomes))
		_, err := e.store.ReviewOutcome(e.ctx, e.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &c).ID, judgment.ReviewRequest{RightOutcome: string(judgment.BallInCourtNobodyOwes)})
		require.NoError(t, err)
		judgments, err := e.store.JudgmentRecords(e.ctx, run)
		require.NoError(t, err)

		reviews, err := e.store.ReviewRecords(e.ctx)
		require.NoError(t, err)
		var mine []judgment.ReviewRecord
		for _, r := range reviews {
			if r.Example == judgments[0].Example {
				mine = append(mine, r)
			}
		}
		require.Len(t, mine, 1)
		assert.Equal(t, prodMethod, mine[0].Method)
		assert.Equal(t, outcomes.BallInCourt[0].QuestionID, mine[0].QuestionID)
		assert.Equal(t, judgment.VerdictRejected, mine[0].Verdict)
		assert.Equal(t, string(judgment.BallInCourtNobodyOwes), mine[0].RightOutcome)
		assert.False(t, mine[0].ReviewedAt.IsZero())
		contentFree(t, mine, th.id, c)
	})

	t.Run("a run with an incomplete thread", func(t *testing.T) {
		e := newEnv(t, production)
		th := e.thread(t)
		run, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: judgment.RunKindScheduled, Method: prodMethod})
		require.NoError(t, err)
		require.NoError(t, e.store.RecordIncomplete(e.ctx, run, th.id, e.day(1), judgment.IncompleteReasonCallFailed))
		require.NoError(t, e.store.FinishRun(e.ctx, run))

		finished := finishedRun(t, e, run)
		assert.Equal(t, map[judgment.IncompleteReason]int{judgment.IncompleteReasonCallFailed: 1}, finished.Incomplete)
		assert.Equal(t, 0, finished.Judged)
		contentFree(t, finished, th.id)
	})

	t.Run("per-participant outcomes in a group thread are keyed by question ID", func(t *testing.T) {
		e := newEnv(t, production)
		c1, c2, th := e.contact(t), e.contact(t), e.thread(t)
		e.message(t, th, c1, false, e.day(1))
		e.message(t, th, c2, false, e.day(1))
		outcomes := judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{
			bic(c1, judgment.BallInCourtIOweReply, 0.9), bic(c2, judgment.BallInCourtNobodyOwes, 0.7),
		}}
		run, _ := e.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, e.day(1), "h1", outcomes))

		records, err := e.store.JudgmentRecords(e.ctx, run)
		require.NoError(t, err)
		require.Len(t, records, 1)
		got := map[string]string{}
		for _, o := range records[0].Outcomes {
			got[o.QuestionID] = o.Outcome
		}
		assert.Equal(t, map[string]string{
			outcomes.BallInCourt[0].QuestionID: string(judgment.BallInCourtIOweReply),
			outcomes.BallInCourt[1].QuestionID: string(judgment.BallInCourtNobodyOwes),
		}, got)
		contentFree(t, records, th.id, c1, c2)
	})
}

func finishedRun(t *testing.T, e *env, run judgment.RunID) judgment.RunRecord {
	t.Helper()
	runs, err := e.store.FinishedRuns(e.ctx)
	require.NoError(t, err)
	for _, r := range runs {
		if r.Run == run {
			return r
		}
	}
	t.Fatalf("run %s is not among the finished runs", run)
	return judgment.RunRecord{}
}

func runSummary(t *testing.T, runs []judgment.RunSummary, run judgment.RunID) judgment.RunSummary {
	t.Helper()
	for _, r := range runs {
		if r.ID == run {
			return r
		}
	}
	t.Fatalf("run %s is not listed", run)
	return judgment.RunSummary{}
}

func exampleOf(t *testing.T, examples []judgment.Example, thread uuid.UUID) judgment.Example {
	t.Helper()
	for _, e := range examples {
		if e.Thread == thread {
			assert.False(t, strings.Contains(string(e.ID), thread.String()), "the example ID carries no thread ID")
			return e
		}
	}
	t.Fatalf("no reviewed example of thread %s", thread)
	return judgment.Example{}
}
