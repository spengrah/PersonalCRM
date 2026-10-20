//go:build integration_testdb

package judgment_test

import (
	"testing"
	"time"

	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/threadctx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each list of a proposal carries the decision the judgment came closest to
// answering with the list's value among the decisions no item of the list
// represents, so a "None" shows how close the model came to a miss.
func TestBenchClosestDecision(t *testing.T) {
	t.Run("a value's probability sums every option the decision tree maps to it", func(t *testing.T) {
		// spec: IXN-053.closest-by-value, IXN-053.value-sums-options
		b := newBenchEnv(t)
		x, y, th := b.contact(t), b.contact(t), b.thread(t)
		b.message(t, th, x, false, b.day(1))
		b.message(t, th, y, false, b.day(1).Add(time.Minute))
		b.record(t, th, x, repository.InteractionDirectionInbound, b.day(1))
		bx, by := bic(x, judgment.BallInCourtNobodyOwes, 0.64), bic(y, judgment.BallInCourtNobodyOwes, 0.7)
		b.run(t, judgment.RunKindScheduled, asked(prodMethod, th, b.day(1).Add(time.Minute), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bx, by}},
			answered(bx.QuestionID, map[string]float64{"i_owe_reply.question": 0.18, "i_owe_reply.request": 0.18, "nobody_owes.closed": 0.64}),
			answered(by.QuestionID, map[string]float64{"i_owe_reply.question": 0.30, "nobody_owes.closed": 0.70}),
		))

		p := b.proposal(t, x, prodMethod)

		closest := listView(t, p, "i_owe_reply").Closest
		require.NotNil(t, closest)
		require.NotNil(t, closest.Contact)
		assert.Equal(t, x.String(), closest.Contact.ContactID, "two reasons of 18% are one value of 36%, above one option of 30%")
		assert.Equal(t, 36, closest.Percent)
		assert.Nil(t, closest.Debtor)
	})

	t.Run("a contact the model put in one reply list is a candidate for the other", func(t *testing.T) {
		// spec: IXN-053.other-list-candidate
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		bx := bic(x, judgment.BallInCourtIOweReply, 0.6)
		b.run(t, judgment.RunKindScheduled, asked(prodMethod, th, b.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bx}},
			answered(bx.QuestionID, map[string]float64{"i_owe_reply": 0.6, "they_owe_reply": 0.25, "nobody_owes": 0.15}),
		))

		p := b.proposal(t, x, prodMethod)

		require.Equal(t, []string{x.String()}, listContacts(p, "i_owe_reply"))
		closest := listView(t, p, "owes_me_reply").Closest
		require.NotNil(t, closest)
		assert.Equal(t, x.String(), closest.Contact.ContactID)
		assert.Equal(t, 25, closest.Percent)
	})

	t.Run("a list with items takes its closest decision only from decisions not among its items", func(t *testing.T) {
		// spec: IXN-053.items-excluded
		b := newBenchEnv(t)
		x, y, th := b.contact(t), b.contact(t), b.thread(t)
		b.message(t, th, x, false, b.day(1))
		b.message(t, th, y, false, b.day(1).Add(time.Minute))
		b.record(t, th, x, repository.InteractionDirectionInbound, b.day(1))
		bx, by := bic(x, judgment.BallInCourtIOweReply, 0.9), bic(y, judgment.BallInCourtNobodyOwes, 0.8)
		b.run(t, judgment.RunKindScheduled, asked(prodMethod, th, b.day(1).Add(time.Minute), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bx, by}},
			answered(bx.QuestionID, map[string]float64{"i_owe_reply": 0.9, "nobody_owes": 0.1}),
			answered(by.QuestionID, map[string]float64{"i_owe_reply": 0.2, "nobody_owes": 0.8}),
		))

		p := b.proposal(t, x, prodMethod)

		require.Equal(t, []string{x.String()}, listContacts(p, "i_owe_reply"))
		closest := listView(t, p, "i_owe_reply").Closest
		require.NotNil(t, closest)
		assert.Equal(t, y.String(), closest.Contact.ContactID)
		assert.Equal(t, 20, closest.Percent)
	})

	t.Run("the big moment's probability is that of either flag value, and a commitment names its pair", func(t *testing.T) {
		// spec: IXN-053.big-moment-either-flag, IXN-053.closest-by-value
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		found := nothingFound(x)
		mine := found.Commitments[0]
		b.run(t, judgment.RunKindScheduled, asked(prodMethod, th, b.day(1), "h1", found,
			answered("big_moment", map[string]float64{"none": 0.55, "participant_moment": 0.25, "other_person_moment": 0.20}),
			answered(mine.QuestionID, map[string]float64{"open": 0.31, "none": 0.69}),
		))

		p := b.proposal(t, x, prodMethod)

		flag := listView(t, p, "big_moment").Closest
		require.NotNil(t, flag)
		assert.Equal(t, 45, flag.Percent)
		assert.Nil(t, flag.Contact)
		assert.Nil(t, flag.Debtor)
		pair := listView(t, p, "commitments").Closest
		require.NotNil(t, pair)
		assert.Equal(t, x.String(), pair.Contact.ContactID)
		assert.Equal(t, ptr("me"), pair.Debtor)
		assert.Equal(t, 31, pair.Percent)
	})

	t.Run("a big moment that is an item leaves its list with no candidate", func(t *testing.T) {
		// spec: IXN-053.no-candidate
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, asked(prodMethod, th, b.day(1), "h1",
			judgment.Outcomes{BigMoment: moment(judgment.BigMomentParticipant, 0.9)},
			answered("big_moment", map[string]float64{"none": 0.1, "participant_moment": 0.9}),
		))

		p := b.proposal(t, x, prodMethod)

		assert.Nil(t, listView(t, p, "big_moment").Closest)
	})

	t.Run("a commitments list whose every pair is an item has no candidate", func(t *testing.T) {
		// spec: IXN-053.no-candidate
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		mine := commitment(x, judgment.CommitmentDirectionMine, judgment.CommitmentStateOpen, 0.9, nil)
		theirs := commitment(x, judgment.CommitmentDirectionTheirs, judgment.CommitmentStateOpen, 0.8, nil)
		b.run(t, judgment.RunKindScheduled, asked(prodMethod, th, b.day(1), "h1",
			judgment.Outcomes{Commitments: []judgment.CommitmentOutcome{mine, theirs}},
			answered(mine.QuestionID, map[string]float64{"open": 0.9, "none": 0.1}),
			answered(theirs.QuestionID, map[string]float64{"open": 0.8, "none": 0.2}),
		))

		p := b.proposal(t, x, prodMethod)

		require.Len(t, p.Items, 2)
		assert.Nil(t, listView(t, p, "commitments").Closest)
	})

	t.Run("a decision with no stored option probabilities is no candidate", func(t *testing.T) {
		// spec: IXN-053.no-candidate
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, asked(prodMethod, th, b.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtNobodyOwes, 0.8)}},
		))

		p := b.proposal(t, x, prodMethod)

		assert.Nil(t, listView(t, p, "i_owe_reply").Closest)
		assert.Nil(t, listView(t, p, "owes_me_reply").Closest)
	})

	t.Run("a meeting's proposal carries no reply lists", func(t *testing.T) {
		// spec: IXN-053.lists-shown
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		j := judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{BigMoment: moment(judgment.BigMomentNone, 0.9)})
		j.Kind = threadctx.KindMeeting
		b.run(t, judgment.RunKindScheduled, j)
		b.run(t, judgment.RunKindScheduled, judgmentOf(otherMethod, th, b.day(1), "h1", judgment.Outcomes{BigMoment: moment(judgment.BigMomentNone, 0.9)}))

		meeting := b.proposal(t, x, prodMethod)
		conversation := b.proposal(t, x, otherMethod)

		assert.Equal(t, []string{"commitments", "big_moment"}, listNames(meeting))
		assert.Equal(t, []string{"i_owe_reply", "owes_me_reply", "commitments", "big_moment"}, listNames(conversation))
	})
}
