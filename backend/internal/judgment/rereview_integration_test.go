//go:build integration_testdb

package judgment_test

import (
	"net/http"
	"testing"
	"time"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hasAddition reports whether the proposal's pickers offer adding the contact
// to the list.
func hasAddition(p handlers.BenchProposalResponse, list string, contact uuid.UUID) bool {
	for _, a := range p.Additions {
		if a.List == list && a.Contact != nil && a.Contact.ContactID == contact.String() {
			return true
		}
	}
	return false
}

// A review of a decision that already has a label records the new label and
// changes nothing live; the item then shows its live state beside its mark.
func TestBenchReReview(t *testing.T) {
	owedReply := func(t *testing.T, b *benchEnv, confidence float64) (uuid.UUID, thread) {
		t.Helper()
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, confidence)},
		}))
		return x, th
	}

	t.Run("a confirmed live item changed to removed leaves its task open, stays live beside its mark and offers its controls again", func(t *testing.T) {
		// spec: IXN-058.label-only, IXN-058.controls-again, IXN-060.removal-not-taken
		b := newBenchEnv(t)
		x, th := owedReply(t, b, 0.9)
		b.act(t, item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x), "confirm")
		confirmed := item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x)
		open := b.openObligations(t, th)
		require.Len(t, open, 1)

		b.act(t, confirmed, "remove")

		assert.Equal(t, open, b.openObligations(t, th), "the re-review closes no task")
		removed := item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x)
		assert.Equal(t, ptr("removed"), removed.Mark)
		assert.Equal(t, ptr("live"), removed.State, "the removal did not take effect")
		assert.Equal(t, handlers.BenchActResponse{Act: "confirm", RightOutcome: "i_owe_reply"}, acts(confirmed)["confirm"], "a reviewed item offers its controls")
		assert.Equal(t, handlers.BenchActResponse{Act: "remove", RightOutcome: "nobody_owes"}, acts(removed)["remove"])
	})

	t.Run("a removal that took effect changed to confirmed reopens nothing and reads not acting", func(t *testing.T) {
		// spec: IXN-058.label-only, IXN-060.closed-not-acting, IXN-060.agrees-as-before
		b := newBenchEnv(t)
		x, th := owedReply(t, b, 0.9)
		b.act(t, item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x), "remove")
		require.Empty(t, b.openObligations(t, th), "a first review takes its live effect")
		removed := item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x)

		b.act(t, removed, "confirm")

		assert.Empty(t, b.openObligations(t, th), "the re-review reopens nothing")
		assert.Nil(t, removed.State, "a removal that took effect has no live state")
		confirmed := item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x)
		assert.Equal(t, ptr("confirmed"), confirmed.Mark)
		assert.Equal(t, ptr("not_acting"), confirmed.State)
	})

	t.Run("an accepted later date changed to Keep original leaves the obligation's moved date, shown beside the kept one", func(t *testing.T) {
		// spec: IXN-058.label-only, IXN-060.kept-date-moved
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		first, later := date(b.day(20)), date(b.day(30))
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{
			Commitments: []judgment.CommitmentOutcome{commitment(x, judgment.CommitmentDirectionMine, judgment.CommitmentStateOpen, 0.9, first)},
		}))
		b.message(t, th, x, false, b.day(2))
		b.record(t, th, x, repository.InteractionDirectionInbound, b.day(2))
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(2), "h2", judgment.Outcomes{
			Commitments: []judgment.CommitmentOutcome{commitment(x, judgment.CommitmentDirectionMine, judgment.CommitmentStateOpen, 0.9, later)},
		}))
		moving := func() handlers.BenchItemResponse {
			page, _ := b.list(t, "contact_id="+x.String())
			for _, row := range page.Items {
				for _, p := range row.Proposals {
					if b.day(2).Equal(p.CutOff) {
						return item(t, p, "commitments", x)
					}
				}
			}
			require.FailNow(t, "no proposal at day 2")
			return handlers.BenchItemResponse{}
		}
		require.NotNil(t, moving().Change)
		require.Equal(t, "later_date", moving().Change.Kind)
		b.act(t, moving(), "accept")
		moved := b.openObligations(t, th)
		require.Len(t, moved, 1)
		require.True(t, later.Equal(moved[0].DueDate), "accepting moves the obligation")
		accepted := moving()

		b.act(t, accepted, "keep_original")

		assert.Equal(t, moved, b.openObligations(t, th), "the re-review moves no date back")
		assert.Nil(t, accepted.LiveDueDate, "an accepted date the obligation holds shows no other date")
		kept := moving()
		assert.Equal(t, ptr("confirmed"), kept.Mark)
		assert.Equal(t, ptr("live"), kept.State)
		assert.Equal(t, ptr(first.Format("2006-01-02")), kept.DueDate, "the date the label kept")
		assert.Equal(t, ptr(later.Format("2006-01-02")), kept.LiveDueDate, "the obligation's live date")
	})

	t.Run("a settled claim is not re-settled", func(t *testing.T) {
		// spec: IXN-058.label-only
		b := newBenchEnv(t)
		x, th := owedReply(t, b, 0.49)
		suggested := item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x)
		require.Equal(t, ptr("suggested"), suggested.State)
		b.act(t, suggested, "confirm")
		open := b.openObligations(t, th)
		require.Len(t, open, 1, "confirming the claim applies it")

		b.act(t, item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x), "remove")

		assert.Equal(t, open, b.openObligations(t, th), "the claim keeps its effect")
		assert.Equal(t, judgment.OutcomeStateApplied, b.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &x).State)
		assert.Equal(t, ptr("live"), item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x).State)
	})

	t.Run("a raised flag changed to removed stays raised", func(t *testing.T) {
		// spec: IXN-058.label-only, IXN-060.removal-not-taken
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{BigMoment: moment(judgment.BigMomentParticipant, 0.9)}))
		flag := func() handlers.BenchItemResponse {
			items := b.proposal(t, x, prodMethod).Items
			require.Len(t, items, 1)
			return items[0]
		}
		b.act(t, flag(), "confirm")

		b.act(t, flag(), "remove")

		assert.Equal(t, ptr("removed"), flag().Mark)
		assert.Equal(t, ptr("live"), flag().State, "the flag is still raised")
	})

	t.Run("a label that agrees with the live state reads as before", func(t *testing.T) {
		// spec: IXN-060.agrees-as-before
		b := newBenchEnv(t)
		x, _ := owedReply(t, b, 0.9)
		b.act(t, item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x), "confirm")

		confirmed := item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x)

		assert.Equal(t, ptr("confirmed"), confirmed.Mark)
		assert.Equal(t, ptr("live"), confirmed.State)
		assert.Nil(t, confirmed.LiveDueDate)
	})
}

// The Add pickers offer every negative decision, labeled or not, and an Add
// on a negative decision has the live effect it has on an unlabeled one.
func TestBenchAddAfterLabels(t *testing.T) {
	confirmed := func(t *testing.T, b *benchEnv, m judgment.MethodRef, kind judgment.RunKind) (uuid.UUID, thread) {
		t.Helper()
		x, th := b.oneToOne(t)
		b.run(t, kind, judgmentOf(m, th, b.day(1), "h1", nothingFound(x)))
		require.Equal(t, http.StatusNoContent, b.confirmNones(t, b.proposal(t, x, m)).Code)
		return x, th
	}

	t.Run("an Add on a confirmed negative in an acting proposal opens the obligation and reads live and added", func(t *testing.T) {
		// spec: IXN-059.offers-negatives, IXN-059.add-effect
		b := newBenchEnv(t)
		x, th := confirmed(t, b, prodMethod, judgment.RunKindScheduled)

		b.add(t, addition(t, b.proposal(t, x, prodMethod), "i_owe_reply", x))

		open := b.openObligations(t, th)
		require.Len(t, open, 1)
		assert.Equal(t, repository.ObligationDebtorUser, open[0].Debtor)
		added := item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x)
		assert.Equal(t, ptr("added"), added.Mark)
		assert.Equal(t, ptr("live"), added.State)
	})

	t.Run("an Add on a labeled negative in a non-acting proposal records the label only", func(t *testing.T) {
		// spec: IXN-059.add-effect
		b := newBenchEnv(t)
		x, th := confirmed(t, b, otherMethod, judgment.RunKindRerun)

		b.add(t, addition(t, b.proposal(t, x, otherMethod), "i_owe_reply", x))

		assert.Empty(t, b.openObligations(t, th))
		added := item(t, b.proposal(t, x, otherMethod), "i_owe_reply", x)
		assert.Equal(t, ptr("added"), added.Mark)
		assert.Equal(t, ptr("not_acting"), added.State)
	})

	t.Run("the scorecard reads an Add after a Confirm Nones as the decision's label", func(t *testing.T) {
		// spec: IXN-059.add-effect
		b := newBenchEnv(t)
		x, _ := confirmed(t, b, prodMethod, judgment.RunKindScheduled)

		b.add(t, addition(t, b.proposal(t, x, prodMethod), "owes_me_reply", x))

		assert.Equal(t, "they_owe_reply", b.labels(t)[decision(judgment.QuestionBallInCourt, &x)])
	})

	t.Run("an unlabeled positive decision of the other reply list stays offered, a labeled one is offered nowhere", func(t *testing.T) {
		// spec: IXN-059.offers-negatives
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1).Add(time.Minute), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.9)},
		}))
		unlabeled := b.proposal(t, x, prodMethod)
		b.act(t, item(t, unlabeled, "i_owe_reply", x), "confirm")

		labeled := b.proposal(t, x, prodMethod)

		assert.True(t, hasAddition(unlabeled, "owes_me_reply", x))
		assert.False(t, hasAddition(labeled, "owes_me_reply", x))
		assert.False(t, hasAddition(labeled, "i_owe_reply", x))
	})
}
