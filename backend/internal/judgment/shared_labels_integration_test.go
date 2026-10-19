//go:build integration_testdb

package judgment_test

import (
	"net/http"
	"testing"

	"personal-crm/backend/internal/judgment"

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
	})

	t.Run("a list with no negative decision behind it never reads its nones reviewed", func(t *testing.T) {
		// spec: IXN-055.nones-reviewed
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		found := nothingFound(x)
		found.BallInCourt[0].Outcome = judgment.BallInCourtIOweReply
		found.BigMoment = moment(judgment.BigMomentParticipant, 0.9)
		run, _ := b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", found))
		b.act(t, item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x), "confirm")
		flag := b.proposal(t, x, prodMethod).Items
		require.Len(t, flag, 2)
		for _, it := range flag {
			if it.List == "big_moment" {
				b.act(t, it, "confirm")
			}
		}
		require.Equal(t, http.StatusOK, b.closeThread(t, th, run).Code)

		p := b.proposal(t, x, prodMethod)

		assert.True(t, listView(t, p, "commitments").NonesReviewed)
		assert.False(t, listView(t, p, "i_owe_reply").NonesReviewed)
		assert.False(t, listView(t, p, "owes_me_reply").NonesReviewed)
		assert.False(t, listView(t, p, "big_moment").NonesReviewed)
	})
}
