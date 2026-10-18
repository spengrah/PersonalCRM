//go:build integration_testdb

package judgment_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A decision's label shows wherever the decision appears: in every proposal
// on the same content, whichever proposal it was made in.
func TestBenchSharedLabels(t *testing.T) {
	t.Run("a label made in another method's proposal on the same content is the item's mark and counts as reviewed", func(t *testing.T) {
		// spec: IXN-055.label-everywhere
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		owe := func() judgment.Outcomes {
			return judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.9)}}
		}
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", owe()))
		b.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, b.day(1), "h1", owe()))
		b.act(t, item(t, b.proposal(t, x, otherMethod), "i_owe_reply", x), "confirm")

		p := b.proposal(t, x, prodMethod)

		assert.Equal(t, ptr("confirmed"), item(t, p, "i_owe_reply", x).Mark)
		assert.Equal(t, 0, p.ToReview)
		assert.Equal(t, "all_reviewed", p.ReviewStatus)
	})

	t.Run("the latest of reviews made in two proposals is the label", func(t *testing.T) {
		// spec: IXN-055.latest-review-is-label
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		owe := func() judgment.Outcomes {
			return judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.9)}}
		}
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", owe()))
		b.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, b.day(1), "h1", owe()))
		marks := func() []*string {
			return []*string{
				item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x).Mark,
				item(t, b.proposal(t, x, otherMethod), "i_owe_reply", x).Mark,
			}
		}

		b.act(t, item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x), "confirm")
		b.act(t, item(t, b.proposal(t, x, otherMethod), "i_owe_reply", x), "remove")
		afterRemove := marks()
		b.act(t, item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x), "confirm")
		afterConfirmAgain := marks()

		assert.Equal(t, []*string{ptr("removed"), ptr("removed")}, afterRemove)
		assert.Equal(t, []*string{ptr("confirmed"), ptr("confirmed")}, afterConfirmAgain, "a review restating an outcome's earlier label is still the latest")
		assert.Equal(t, "i_owe_reply", b.labels(t)[decision(judgment.QuestionBallInCourt, &x)])
	})

	t.Run("a list's nones read reviewed once every negative behind it has a label, whichever proposal labeled it", func(t *testing.T) {
		// spec: IXN-055.nones-reviewed
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", nothingFound(x)))
		b.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, b.day(1), "h1", nothingFound(x)))
		before := b.proposal(t, x, prodMethod)
		require.Equal(t, http.StatusNoContent, b.confirmNones(t, b.proposal(t, x, otherMethod)).Code)

		p := b.proposal(t, x, prodMethod)

		for _, l := range before.Lists {
			assert.False(t, l.NonesReviewed, l.List)
		}
		for _, l := range p.Lists {
			assert.True(t, l.NonesReviewed, l.List)
		}
		assert.Equal(t, "all_reviewed", p.ReviewStatus)
		assert.False(t, p.ConfirmNones)
	})

	t.Run("a list with no negative decision behind it never reads its nones reviewed", func(t *testing.T) {
		// spec: IXN-055.nones-reviewed
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		found := nothingFound(x)
		found.BallInCourt[0].Outcome = judgment.BallInCourtIOweReply
		found.BigMoment = moment(judgment.BigMomentParticipant, 0.9)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", found))
		b.act(t, item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x), "confirm")
		flag := b.proposal(t, x, prodMethod).Items
		require.Len(t, flag, 2)
		for _, it := range flag {
			if it.List == "big_moment" {
				b.act(t, it, "confirm")
			}
		}

		p := b.proposal(t, x, prodMethod)

		require.Equal(t, "all_reviewed", p.ReviewStatus)
		assert.True(t, listView(t, p, "commitments").NonesReviewed)
		assert.False(t, listView(t, p, "i_owe_reply").NonesReviewed)
		assert.False(t, listView(t, p, "owes_me_reply").NonesReviewed)
		assert.False(t, listView(t, p, "big_moment").NonesReviewed)
	})
}

// Confirm Nones labels a proposal's negative decisions in one act.
func TestBenchConfirmNones(t *testing.T) {
	t.Run("labels every unlabeled negative decision with its judged value, which the scorecard reads", func(t *testing.T) {
		// spec: IXN-056.labels-negatives
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", nothingFound(x)))
		open := b.openObligations(t, th)

		w := b.confirmNones(t, b.proposal(t, x, prodMethod))

		require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
		assert.Equal(t, map[string]string{
			decision(judgment.QuestionBallInCourt, &x):      "nobody_owes",
			decision(judgment.QuestionCommitmentMine, &x):   "none",
			decision(judgment.QuestionCommitmentTheirs, &x): "none",
			decision(judgment.QuestionBigMoment, nil):       "none",
		}, b.labels(t))
		p := b.proposal(t, x, prodMethod)
		assert.Equal(t, "all_reviewed", p.ReviewStatus)
		assert.Equal(t, ptr("all_reviewed"), b.row(t, x).ReviewStatus)
		assert.Equal(t, open, b.openObligations(t, th), "the labels change nothing live")
	})

	t.Run("a negative already labeled from another proposal keeps its label", func(t *testing.T) {
		// spec: IXN-056.keeps-labels
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtNotApplicable, 0.8)},
			BigMoment:   moment(judgment.BigMomentNone, 0.9),
		}))
		b.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.8)},
			BigMoment:   moment(judgment.BigMomentParticipant, 0.9),
		}))
		b.act(t, item(t, b.proposal(t, x, otherMethod), "i_owe_reply", x), "remove")
		require.True(t, b.proposal(t, x, prodMethod).ConfirmNones)

		require.Equal(t, http.StatusNoContent, b.confirmNones(t, b.proposal(t, x, prodMethod)).Code)

		assert.Equal(t, map[string]string{
			decision(judgment.QuestionBallInCourt, &x): "nobody_owes",
			decision(judgment.QuestionBigMoment, nil):  "none",
		}, b.labels(t))
	})

	t.Run("a proposal with an item is never offered Confirm Nones, and is refused with nothing recorded", func(t *testing.T) {
		// spec: IXN-056.refuses-items, IXN-054.confirm-nones-offered
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		found := nothingFound(x)
		found.BallInCourt[0].Outcome = judgment.BallInCourtIOweReply
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", found))
		p := b.proposal(t, x, prodMethod)

		w := b.confirmNones(t, p)

		assert.False(t, p.ConfirmNones)
		assert.Equal(t, http.StatusConflict, w.Code)
		assert.Empty(t, b.labels(t))
	})

	t.Run("the list read offers Confirm Nones only while an item-less proposal holds an unlabeled negative", func(t *testing.T) {
		// spec: IXN-054.confirm-nones-offered
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", nothingFound(x)))
		before := b.proposal(t, x, prodMethod)
		require.Equal(t, http.StatusNoContent, b.confirmNones(t, before).Code)

		after := b.proposal(t, x, prodMethod)

		assert.True(t, before.ConfirmNones)
		assert.False(t, after.ConfirmNones)
	})

	t.Run("a second call records no new label", func(t *testing.T) {
		// spec: IXN-056.refuses-items
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", nothingFound(x)))
		p := b.proposal(t, x, prodMethod)
		require.Equal(t, http.StatusNoContent, b.confirmNones(t, p).Code)
		reviews, err := b.store.ReviewRecords(b.ctx)
		require.NoError(t, err)

		w := b.confirmNones(t, p)

		assert.Equal(t, http.StatusConflict, w.Code)
		again, err := b.store.ReviewRecords(b.ctx)
		require.NoError(t, err)
		assert.Equal(t, reviews, again)
	})

	t.Run("an unknown judgment is not found", func(t *testing.T) {
		// spec: IXN-056.refuses-items
		b := newBenchEnv(t)

		assert.Equal(t, http.StatusNotFound, b.post(t, "/api/v1/judgments/"+uuid.NewString()+"/confirm-nones", nil).Code)
	})
}

// Once every item of a proposal has a label, its negative decisions are
// labeled with their judged values, with no further act.
func TestBenchInference(t *testing.T) {
	t.Run("the review giving a proposal's last unlabeled item its label labels its negative decisions", func(t *testing.T) {
		// spec: IXN-057.last-item-labels-negatives
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		found := nothingFound(x)
		found.BallInCourt[0].Outcome = judgment.BallInCourtIOweReply
		found.BigMoment = moment(judgment.BigMomentParticipant, 0.9)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", found))
		p := b.proposal(t, x, prodMethod)
		for _, it := range p.Items {
			if it.List == "big_moment" {
				b.act(t, it, "confirm")
			}
		}
		afterFirst := b.labels(t)
		b.act(t, item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x), "confirm")

		assert.Equal(t, map[string]string{decision(judgment.QuestionBigMoment, nil): "participant_moment"}, afterFirst, "an item awaits review, so no negative is labeled")
		assert.Equal(t, map[string]string{
			decision(judgment.QuestionBallInCourt, &x):      "i_owe_reply",
			decision(judgment.QuestionBigMoment, nil):       "participant_moment",
			decision(judgment.QuestionCommitmentMine, &x):   "none",
			decision(judgment.QuestionCommitmentTheirs, &x): "none",
		}, b.labels(t))
		assert.Equal(t, "all_reviewed", b.proposal(t, x, prodMethod).ReviewStatus)
	})

	t.Run("a review in one proposal that labels another proposal's last item labels that proposal's negatives", func(t *testing.T) {
		// spec: IXN-057.across-proposals
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.9)},
			Commitments: []judgment.CommitmentOutcome{commitment(x, judgment.CommitmentDirectionTheirs, judgment.CommitmentStateNone, 0.9, nil)},
		}))
		b.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.9)},
			BigMoment:   moment(judgment.BigMomentParticipant, 0.9),
		}))

		b.act(t, item(t, b.proposal(t, x, otherMethod), "i_owe_reply", x), "confirm")

		assert.Equal(t, "none", b.labels(t)[decision(judgment.QuestionCommitmentTheirs, &x)])
		assert.Equal(t, "all_reviewed", b.proposal(t, x, prodMethod).ReviewStatus)
		assert.Equal(t, "to_review", b.proposal(t, x, otherMethod).ReviewStatus)
	})

	t.Run("a Confirm Nones that labels another proposal's last item labels that proposal's negatives", func(t *testing.T) {
		// spec: IXN-057.across-proposals
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtNotApplicable, 0.8)},
			BigMoment:   moment(judgment.BigMomentNone, 0.9),
		}))
		b.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.8)},
			Commitments: []judgment.CommitmentOutcome{commitment(x, judgment.CommitmentDirectionMine, judgment.CommitmentStateNone, 0.9, nil)},
			BigMoment:   moment(judgment.BigMomentParticipant, 0.9),
		}))

		require.Equal(t, http.StatusNoContent, b.confirmNones(t, b.proposal(t, x, prodMethod)).Code)

		assert.Equal(t, "none", b.labels(t)[decision(judgment.QuestionCommitmentMine, &x)])
		other := b.proposal(t, x, otherMethod)
		assert.Equal(t, ptr("removed"), item(t, other, "i_owe_reply", x).Mark)
		assert.Equal(t, "all_reviewed", other.ReviewStatus)
	})

	t.Run("inference never replaces a label", func(t *testing.T) {
		// spec: IXN-057.never-replaces
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtNotApplicable, 0.8)},
			BigMoment:   moment(judgment.BigMomentParticipant, 0.9),
		}))
		b.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.8)},
			BigMoment:   moment(judgment.BigMomentParticipant, 0.9),
		}))
		b.act(t, item(t, b.proposal(t, x, otherMethod), "i_owe_reply", x), "remove")

		for _, it := range b.proposal(t, x, prodMethod).Items {
			if it.List == "big_moment" {
				b.act(t, it, "confirm")
			}
		}

		assert.Equal(t, "nobody_owes", b.labels(t)[decision(judgment.QuestionBallInCourt, &x)], "the judged not_applicable does not replace the label")
		assert.Equal(t, "all_reviewed", b.proposal(t, x, prodMethod).ReviewStatus)
	})

	t.Run("a proposal with no items never gains a label by inference", func(t *testing.T) {
		// spec: IXN-057.item-less-never
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtNobodyOwes, 0.8)},
			BigMoment:   moment(judgment.BigMomentNone, 0.9),
		}))
		b.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.8)},
		}))

		b.act(t, item(t, b.proposal(t, x, otherMethod), "i_owe_reply", x), "remove")

		assert.Equal(t, map[string]string{decision(judgment.QuestionBallInCourt, &x): "nobody_owes"}, b.labels(t))
		p := b.proposal(t, x, prodMethod)
		assert.Empty(t, p.Items)
		assert.Equal(t, "nones_to_review", p.ReviewStatus)
	})
}

// failingReviews fails one review assertion once armed: the one a count of
// review writes reaches, so a write fails after recording earlier labels.
type failingReviews struct {
	judgment.Asserter
	mu   sync.Mutex
	left int
}

func (f *failingReviews) failOn(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.left = n
}

func (f *failingReviews) AssertTx(ctx context.Context, tx pgx.Tx, req service.AssertRequest) (*repository.Assertion, error) {
	if req.PredicateKey == "judgment_review" {
		f.mu.Lock()
		fail := false
		if f.left > 0 {
			f.left--
			fail = f.left == 0
		}
		f.mu.Unlock()
		if fail {
			return nil, errors.New("injected review failure")
		}
	}
	return f.Asserter.AssertTx(ctx, tx, req)
}

// Confirm Nones, and a review that triggers inference, record all of their
// labels or none.
func TestBenchLabelsAllOrNothing(t *testing.T) {
	newFailing := func(t *testing.T) (*benchEnv, *failingReviews) {
		t.Helper()
		f := &failingReviews{}
		b := newBenchEnv(t, withAsserter(func(a judgment.Asserter) judgment.Asserter {
			f.Asserter = a
			return f
		}))
		return b, f
	}

	t.Run("a Confirm Nones that fails after recording some labels records none", func(t *testing.T) {
		// spec: IXN-056.all-or-nothing
		b, f := newFailing(t)
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", nothingFound(x)))
		before := b.row(t, x)
		f.failOn(3)

		w := b.confirmNones(t, before.Proposals[0])

		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Empty(t, b.labels(t))
		assert.Equal(t, before, b.row(t, x))
	})

	t.Run("a review whose inference fails records nothing, its own label included", func(t *testing.T) {
		// spec: IXN-057.all-or-nothing
		b, f := newFailing(t)
		x, th := b.oneToOne(t)
		found := nothingFound(x)
		found.BallInCourt[0].Outcome = judgment.BallInCourtIOweReply
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", found))
		before := b.row(t, x)
		owed := item(t, before.Proposals[0], "i_owe_reply", x)
		f.failOn(2)

		w := b.post(t, "/api/v1/judgment-outcomes/"+owed.OutcomeID+"/review", map[string]any{"right_outcome": "i_owe_reply"})

		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Empty(t, b.labels(t))
		assert.Equal(t, before, b.row(t, x))
	})
}
