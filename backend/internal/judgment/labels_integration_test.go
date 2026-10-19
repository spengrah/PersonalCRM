//go:build integration_testdb

package judgment_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"personal-crm/backend/internal/api/handlers"
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
		require.Equal(t, http.StatusOK, b.confirmNones(t, b.proposal(t, x, otherMethod)).Code)

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
		require.Equal(t, http.StatusOK, b.closeRow(t, b.row(t, x)).Code)

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

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
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
			BigMoment:   moment(judgment.BigMomentNone, 0.9),
		}))
		b.act(t, item(t, b.proposal(t, x, otherMethod), "i_owe_reply", x), "remove")
		require.True(t, b.proposal(t, x, prodMethod).ConfirmNones)

		require.Equal(t, http.StatusOK, b.confirmNones(t, b.proposal(t, x, prodMethod)).Code)

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
		require.Equal(t, http.StatusOK, b.confirmNones(t, before).Code)

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
		require.Equal(t, http.StatusOK, b.confirmNones(t, p).Code)
		reviews, err := b.store.ReviewRecords(b.ctx)
		require.NoError(t, err)

		w := b.confirmNones(t, p)

		assert.Equal(t, http.StatusConflict, w.Code)
		again, err := b.store.ReviewRecords(b.ctx)
		require.NoError(t, err)
		assert.Equal(t, reviews, again)
	})

	t.Run("a negative another proposal on the same content holds as an unreviewed item gains no label, and is not offered once it is all that is left", func(t *testing.T) {
		// spec: IXN-056.item-awaits-review, IXN-056.refuses-items, IXN-054.confirm-nones-offered
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		found := nothingFound(x)
		found.Commitments[1].Outcome = judgment.CommitmentStateOpen
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", found))
		b.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, b.day(1), "h1", nothingFound(x)))
		require.True(t, b.proposal(t, x, otherMethod).ConfirmNones)

		require.Equal(t, http.StatusOK, b.confirmNones(t, b.proposal(t, x, otherMethod)).Code)

		assert.Equal(t, map[string]string{
			decision(judgment.QuestionBallInCourt, &x):    "nobody_owes",
			decision(judgment.QuestionCommitmentMine, &x): "none",
			decision(judgment.QuestionBigMoment, nil):     "none",
		}, b.labels(t), "the commitment the production proposal holds as an item stays to review")
		prod := b.proposal(t, x, prodMethod)
		assert.Nil(t, listItem(t, prod, "commitments", "contact").Mark)
		assert.Equal(t, 1, prod.ToReview)
		assert.Equal(t, "to_review", prod.ReviewStatus)
		other := b.proposal(t, x, otherMethod)
		assert.Equal(t, "nones_to_review", other.ReviewStatus)
		assert.False(t, other.ConfirmNones, "Confirm Nones would label nothing")
		reviews, err := b.store.ReviewRecords(b.ctx)
		require.NoError(t, err)
		assert.Equal(t, http.StatusConflict, b.confirmNones(t, other).Code)
		again, err := b.store.ReviewRecords(b.ctx)
		require.NoError(t, err)
		assert.Equal(t, reviews, again)
	})

	t.Run("a reply beside another proposal's commitment is an item without a label, so it gains no label and holds back its list's Nones", func(t *testing.T) {
		// spec: IXN-056.item-awaits-review, IXN-054.nones-to-review, IXN-052.reply-beside-commitment
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", owesAndCommits(x)))
		b.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, b.day(1), "h1", nothingFound(x)))
		require.Equal(t, []string{x.String()}, listContacts(b.proposal(t, x, prodMethod), "i_owe_reply"), "the commitment stands in for no reply")

		require.Equal(t, http.StatusOK, b.confirmNones(t, b.proposal(t, x, otherMethod)).Code)

		assert.Equal(t, map[string]string{
			decision(judgment.QuestionCommitmentTheirs, &x): "none",
			decision(judgment.QuestionBigMoment, nil):       "none",
		}, b.labels(t), "the reply and the commitment beside it stay unlabeled")
		prod := b.proposal(t, x, prodMethod)
		assert.Equal(t, []string{x.String()}, listContacts(prod, "i_owe_reply"))
		assert.Nil(t, item(t, prod, "i_owe_reply", x).Mark)
		other := b.proposal(t, x, otherMethod)
		assert.False(t, listView(t, other, "i_owe_reply").NonesReviewed, "the reply the production proposal holds as an item holds back the reply list's Nones")
		assert.False(t, other.ConfirmNones)
	})

	t.Run("an unknown judgment is not found", func(t *testing.T) {
		// spec: IXN-056.refuses-items
		b := newBenchEnv(t)

		assert.Equal(t, http.StatusNotFound, b.post(t, "/api/v1/judgments/"+uuid.NewString()+"/confirm-nones", nil).Code)
	})
}

// closeRow posts a close of the row: the judgments of its proposals.
func (b *benchEnv) closeRow(t *testing.T, row handlers.AllInteractionItemResponse) *httptest.ResponseRecorder {
	t.Helper()
	ids := make([]string, len(row.Proposals))
	for i, p := range row.Proposals {
		ids[i] = p.JudgmentID
	}
	return b.post(t, "/api/v1/judgments/close", map[string]any{"judgment_ids": ids})
}

// A close of a row labels the negative decisions of each of its proposals
// that holds a label; reviewing items labels none.
func TestBenchInference(t *testing.T) {
	t.Run("a close labels the negatives of a proposal holding a label, leaving its unlabeled item and a proposal with no label as they were", func(t *testing.T) {
		// spec: IXN-057.close-labels-negatives, IXN-057.items-stay, IXN-057.no-label-gains-none, IXN-068.labels-row
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		found := nothingFound(x)
		found.BallInCourt[0].Outcome = judgment.BallInCourtIOweReply
		found.Commitments[1].Outcome = judgment.CommitmentStateOpen
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", found))
		b.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, b.day(1), "h2", nothingFound(x)))
		b.act(t, item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x), "confirm")
		open := b.openObligations(t, th)

		w := b.closeRow(t, b.row(t, x))

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, open, b.openObligations(t, th), "the labels change nothing live")
		assert.Equal(t, map[string]string{
			decision(judgment.QuestionBallInCourt, &x):    "i_owe_reply",
			decision(judgment.QuestionCommitmentMine, &x): "none",
			decision(judgment.QuestionBigMoment, nil):     "none",
		}, b.labels(t), "the unlabeled commitment stays to review, and the other content's proposal gains nothing")
		prod := b.proposal(t, x, prodMethod)
		assert.Nil(t, listItem(t, prod, "commitments", "contact").Mark)
		assert.Equal(t, "to_review", prod.ReviewStatus)
		other := b.proposal(t, x, otherMethod)
		assert.Equal(t, "nones_to_review", other.ReviewStatus)
		assert.True(t, other.ConfirmNones)
	})

	t.Run("a label made in another proposal on the same content counts, and a close never replaces a label", func(t *testing.T) {
		// spec: IXN-057.across-proposals, IXN-057.never-replaces
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		nothing := nothingFound(x)
		nothing.BallInCourt[0].Outcome = judgment.BallInCourtNotApplicable
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", nothing))
		b.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.8)},
		}))
		b.act(t, item(t, b.proposal(t, x, otherMethod), "i_owe_reply", x), "remove")

		require.Equal(t, http.StatusOK, b.closeRow(t, b.row(t, x)).Code)

		assert.Equal(t, map[string]string{
			decision(judgment.QuestionBallInCourt, &x):      "nobody_owes",
			decision(judgment.QuestionCommitmentMine, &x):   "none",
			decision(judgment.QuestionCommitmentTheirs, &x): "none",
			decision(judgment.QuestionBigMoment, nil):       "none",
		}, b.labels(t), "the judged not_applicable does not replace the label")
		assert.Equal(t, "all_reviewed", b.proposal(t, x, prodMethod).ReviewStatus)
	})

	// closeLeavesUnreviewedItem seeds content judged by two methods: the
	// production proposal holds a reply and a commitment of theirs as items,
	// the other proposal holds every decision as a negative. The user confirms
	// the reply, then closes the judgments closed names.
	closeLeavesUnreviewedItem := func(t *testing.T, closed func(b *benchEnv, x uuid.UUID) []string) {
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		found := nothingFound(x)
		found.BallInCourt[0].Outcome = judgment.BallInCourtIOweReply
		found.Commitments[1].Outcome = judgment.CommitmentStateOpen
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", found))
		b.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, b.day(1), "h1", nothingFound(x)))
		b.act(t, item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x), "confirm")

		w := b.post(t, "/api/v1/judgments/close", map[string]any{"judgment_ids": closed(b, x)})

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, map[string]string{
			decision(judgment.QuestionBallInCourt, &x):    "i_owe_reply",
			decision(judgment.QuestionCommitmentMine, &x): "none",
			decision(judgment.QuestionBigMoment, nil):     "none",
		}, b.labels(t), "the other proposal's negatives are labeled but for the commitment the production proposal holds as an item")
		prod := b.proposal(t, x, prodMethod)
		assert.Nil(t, listItem(t, prod, "commitments", "contact").Mark)
		assert.Equal(t, 1, prod.ToReview)
		assert.Equal(t, "to_review", prod.ReviewStatus)
		assert.Equal(t, "nones_to_review", b.proposal(t, x, otherMethod).ReviewStatus)
		assert.Equal(t, 1, b.row(t, x).ToReview)
	}

	t.Run("a close leaves a negative that another proposal on the row holds as an unreviewed item to review in both", func(t *testing.T) {
		// spec: IXN-057.item-awaits-review, IXN-057.items-stay
		closeLeavesUnreviewedItem(t, func(b *benchEnv, x uuid.UUID) []string {
			row := b.row(t, x)
			require.Len(t, row.Proposals, 2)
			return []string{row.Proposals[0].JudgmentID, row.Proposals[1].JudgmentID}
		})
	})

	t.Run("a close leaves a negative that a proposal off the row holds as an unreviewed item to review", func(t *testing.T) {
		// spec: IXN-057.item-awaits-review
		closeLeavesUnreviewedItem(t, func(b *benchEnv, x uuid.UUID) []string {
			return []string{b.proposal(t, x, otherMethod).JudgmentID}
		})
	})

	t.Run("a close leaves a reply beside another proposal's commitment without a label, and both proposals hold it to review", func(t *testing.T) {
		// spec: IXN-057.item-awaits-review, IXN-054.nones-to-review, IXN-054.proposal-status, IXN-055.nones-reviewed, IXN-051.row-review-status, IXN-052.reply-beside-commitment
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", owesAndCommits(x)))
		b.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, b.day(1), "h1", nothingFound(x)))
		require.Equal(t, []string{x.String()}, listContacts(b.proposal(t, x, prodMethod), "i_owe_reply"), "the commitment stands in for no reply")
		b.act(t, listItem(t, b.proposal(t, x, prodMethod), "commitments", "me"), "confirm")

		require.Equal(t, http.StatusOK, b.closeRow(t, b.row(t, x)).Code)

		assert.Equal(t, map[string]string{
			decision(judgment.QuestionCommitmentMine, &x):   "open",
			decision(judgment.QuestionCommitmentTheirs, &x): "none",
			decision(judgment.QuestionBigMoment, nil):       "none",
		}, b.labels(t), "the other proposal's negatives are labeled but for the reply the production proposal holds as an item")
		row := b.row(t, x)
		prod, other := b.proposal(t, x, prodMethod), b.proposal(t, x, otherMethod)
		assert.Equal(t, []string{x.String()}, listContacts(prod, "i_owe_reply"))
		assert.Nil(t, item(t, prod, "i_owe_reply", x).Mark)
		assert.Equal(t, "to_review", prod.ReviewStatus)
		assert.Equal(t, 1, prod.ToReview)
		assert.Equal(t, "nones_to_review", other.ReviewStatus, "the reply's negative in this proposal is a None to review")
		assert.False(t, other.ConfirmNones)
		assert.False(t, listView(t, other, "i_owe_reply").NonesReviewed, "the reply the production proposal holds as an item holds back the reply list's Nones")
		assert.Equal(t, 1, row.ToReview)
	})

	t.Run("a close naming an unknown judgment is not found and records nothing", func(t *testing.T) {
		// spec: IXN-068.unknown-judgment
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		found := nothingFound(x)
		found.BallInCourt[0].Outcome = judgment.BallInCourtIOweReply
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", found))
		b.act(t, item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x), "confirm")
		labels := b.labels(t)

		w := b.post(t, "/api/v1/judgments/close", map[string]any{
			"judgment_ids": []string{b.proposal(t, x, prodMethod).JudgmentID, uuid.NewString()},
		})

		assert.Equal(t, http.StatusNotFound, w.Code)
		assert.Equal(t, labels, b.labels(t))
	})

	t.Run("reviewing a proposal's last unlabeled item labels no negative decision", func(t *testing.T) {
		// spec: IXN-057.review-labels-no-negative
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		found := nothingFound(x)
		found.BallInCourt[0].Outcome = judgment.BallInCourtIOweReply
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", found))

		b.act(t, item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x), "confirm")

		assert.Equal(t, map[string]string{decision(judgment.QuestionBallInCourt, &x): "i_owe_reply"}, b.labels(t))
		assert.Equal(t, "nones_to_review", b.proposal(t, x, prodMethod).ReviewStatus)
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

// Confirm Nones, and a close that triggers inference, record all of their
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

	t.Run("a close that fails after recording some labels records none", func(t *testing.T) {
		// spec: IXN-068.all-or-nothing
		b, f := newFailing(t)
		x, th := b.oneToOne(t)
		found := nothingFound(x)
		found.BallInCourt[0].Outcome = judgment.BallInCourtIOweReply
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", found))
		b.act(t, item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x), "confirm")
		before, labels := b.row(t, x), b.labels(t)
		f.failOn(2)

		w := b.closeRow(t, before)

		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Equal(t, labels, b.labels(t))
		assert.Equal(t, before, b.row(t, x))
	})
}

// owesAndCommits is content where the user owes the contact a reply and has
// committed to them, so the reply and the commitment are both items.
func owesAndCommits(x uuid.UUID) judgment.Outcomes {
	o := nothingFound(x)
	o.BallInCourt[0].Outcome = judgment.BallInCourtIOweReply
	o.Commitments[0].Outcome = judgment.CommitmentStateOpen
	return o
}

// sameContentTwoRows seeds one thread whose content two rows hold, a message
// from x on each of days 1 and 2, judged on each day's row by its own method
// on the same content, each proposal owing x a reply. It returns the rows as
// the list reads them, newest first.
func (b *benchEnv) sameContentTwoRows(t *testing.T) (uuid.UUID, []handlers.AllInteractionItemResponse) {
	t.Helper()
	x, th := b.contact(t), b.thread(t)
	for _, day := range []int{1, 2} {
		b.message(t, th, x, false, b.day(day))
		b.record(t, th, x, repository.InteractionDirectionInbound, b.day(day))
	}
	owes := nothingFound(x)
	owes.BallInCourt[0].Outcome = judgment.BallInCourtIOweReply
	b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", owes))
	b.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, b.day(2), "h1", owes))
	page, _ := b.list(t, "contact_id="+x.String())
	require.Len(t, page.Items, 2)
	for _, row := range page.Items {
		require.Len(t, row.Proposals, 1, "each day's judgment sits on its own row")
	}
	return x, page.Items
}

// namedRows names rows as a save's request does: by their proposals' judgments.
func namedRows(rows ...handlers.AllInteractionItemResponse) []handlers.BenchRowRequest {
	out := make([]handlers.BenchRowRequest, len(rows))
	for i, row := range rows {
		for _, p := range row.Proposals {
			out[i].JudgmentIDs = append(out[i].JudgmentIDs, p.JudgmentID)
		}
	}
	return out
}

// listedRows reads the contact's rows from the list as a save returns rows.
func (b *benchEnv) listedRows(t *testing.T, contact uuid.UUID) []handlers.BenchRowStateResponse {
	t.Helper()
	page, _ := b.list(t, "contact_id="+contact.String())
	out := make([]handlers.BenchRowStateResponse, len(page.Items))
	for i, row := range page.Items {
		out[i] = handlers.BenchRowStateResponse{Proposals: row.Proposals, ToReview: row.ToReview, ReviewStatus: row.ReviewStatus}
	}
	return out
}

func decodeData[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var envelope struct {
		Data T `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	return envelope.Data
}

// A save returns the rows it names as a read of the list made after it shows
// them, so the bench shows each row's proposals, count and status together.
func TestBenchSaveRows(t *testing.T) {
	t.Run("a review returns each named row as the list then reads it, a row its label reached included", func(t *testing.T) {
		// spec: IXN-073.rows-as-listed
		b := newBenchEnv(t)
		x, rows := b.sameContentTwoRows(t)
		it := item(t, rows[1].Proposals[0], "i_owe_reply", x)

		got := decodeData[handlers.ReviewJudgmentOutcomeResponse](t, b.post(t, "/api/v1/judgment-outcomes/"+it.OutcomeID+"/review", map[string]any{
			"right_outcome": acts(it)["confirm"].RightOutcome, "rows": namedRows(rows...),
		}))

		assert.Equal(t, b.listedRows(t, x), got.Rows)
		require.Len(t, got.Rows, 2)
		assert.Equal(t, ptr("nones_to_review"), got.Rows[0].ReviewStatus, "the label reached the other row's proposal on the same content")
		assert.Equal(t, 0, got.Rows[0].ToReview)
	})

	t.Run("Confirm Nones and a close return each named row as the list then reads it", func(t *testing.T) {
		// spec: IXN-073.rows-as-listed
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", nothingFound(x)))
		row := b.row(t, x)

		confirmed := decodeData[handlers.BenchRowsResponse](t, b.post(t, "/api/v1/judgments/"+row.Proposals[0].JudgmentID+"/confirm-nones", map[string]any{"rows": namedRows(row)}))

		assert.Equal(t, b.listedRows(t, x), confirmed.Rows)
		require.Len(t, confirmed.Rows, 1)
		assert.Equal(t, ptr("all_reviewed"), confirmed.Rows[0].ReviewStatus)

		closed := decodeData[handlers.BenchRowsResponse](t, b.post(t, "/api/v1/judgments/close", map[string]any{
			"judgment_ids": []string{row.Proposals[0].JudgmentID}, "rows": namedRows(row),
		}))

		assert.Equal(t, b.listedRows(t, x), closed.Rows)
	})

	t.Run("a save naming a row with an unknown judgment is not found and records nothing", func(t *testing.T) {
		// spec: IXN-073.unknown-judgment
		b := newBenchEnv(t)
		x, rows := b.sameContentTwoRows(t)
		it := item(t, rows[1].Proposals[0], "i_owe_reply", x)

		w := b.post(t, "/api/v1/judgment-outcomes/"+it.OutcomeID+"/review", map[string]any{
			"right_outcome": acts(it)["confirm"].RightOutcome,
			"rows":          []handlers.BenchRowRequest{{JudgmentIDs: []string{uuid.NewString()}}},
		})

		assert.Equal(t, http.StatusNotFound, w.Code)
		assert.Empty(t, b.labels(t))
	})
}
