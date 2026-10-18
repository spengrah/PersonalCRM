//go:build integration_testdb

package judgment_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// post sends a JSON body (none when body is nil) to a route of the bench env.
func (b *benchEnv) post(t *testing.T, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	b.router.ServeHTTP(w, req)
	return w
}

// review posts a review of the outcome through the review route.
func (b *benchEnv) review(t *testing.T, outcomeID, right string, keep bool) {
	t.Helper()
	w := b.post(t, "/api/v1/judgment-outcomes/"+outcomeID+"/review", map[string]any{"right_outcome": right, "keep_current_date": keep})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// act posts the review one of an item's controls sends.
func (b *benchEnv) act(t *testing.T, it handlers.BenchItemResponse, kind string) {
	t.Helper()
	a, ok := acts(it)[kind]
	require.True(t, ok, "item offers %s: %+v", kind, it.Acts)
	b.review(t, it.OutcomeID, a.RightOutcome, a.KeepCurrentDate)
}

// add posts the review an addition sends.
func (b *benchEnv) add(t *testing.T, a handlers.BenchAdditionResponse) {
	t.Helper()
	b.review(t, a.OutcomeID, a.RightOutcome, false)
}

// confirmNones posts Confirm Nones for the proposal and returns the status.
func (b *benchEnv) confirmNones(t *testing.T, p handlers.BenchProposalResponse) *httptest.ResponseRecorder {
	t.Helper()
	return b.post(t, "/api/v1/judgments/"+p.JudgmentID+"/confirm-nones", nil)
}

// row reads the one row the contact takes part in.
func (b *benchEnv) row(t *testing.T, contact uuid.UUID) handlers.AllInteractionItemResponse {
	t.Helper()
	page, _ := b.list(t, "contact_id="+contact.String())
	require.Len(t, page.Items, 1)
	return page.Items[0]
}

// proposal reads the one proposal of method m on the contact's row.
func (b *benchEnv) proposal(t *testing.T, contact uuid.UUID, m judgment.MethodRef) handlers.BenchProposalResponse {
	t.Helper()
	var found []handlers.BenchProposalResponse
	for _, p := range b.row(t, contact).Proposals {
		if p.MethodKey == m.Key && p.MethodVersion == m.Version {
			found = append(found, p)
		}
	}
	require.Len(t, found, 1, "proposals of %s", m.Key)
	return found[0]
}

// labels reads every decision's label as the scorecard reads it, keyed by
// question and contact ("big_moment/" for the flag).
func (b *benchEnv) labels(t *testing.T) map[string]string {
	t.Helper()
	examples, err := b.store.ReviewedExamples(b.ctx)
	require.NoError(t, err)
	out := map[string]string{}
	for _, e := range examples {
		for _, r := range e.Reviews {
			contact := ""
			if r.ContactID != nil {
				contact = r.ContactID.String()
			}
			out[r.Question+"/"+contact] = r.RightOutcome
		}
	}
	return out
}

func decision(question judgment.Question, contact *uuid.UUID) string {
	if contact == nil {
		return string(question) + "/"
	}
	return string(question) + "/" + contact.String()
}

// listView returns the proposal's entry for the list.
func listView(t *testing.T, p handlers.BenchProposalResponse, list string) handlers.BenchListResponse {
	t.Helper()
	for _, l := range p.Lists {
		if l.List == list {
			return l
		}
	}
	require.Failf(t, "no list", "%s in %+v", list, p.Lists)
	return handlers.BenchListResponse{}
}

func listNames(p handlers.BenchProposalResponse) []string {
	names := make([]string, len(p.Lists))
	for i, l := range p.Lists {
		names[i] = l.List
	}
	return names
}

// answered is the question the outcome answers, with option probabilities.
func answered(questionID string, probabilities map[string]float64) judgment.AnsweredQuestion {
	return judgment.AnsweredQuestion{ID: questionID, Probabilities: probabilities}
}

// asked builds a judgment of the thread whose stored questions are exactly
// the given ones.
func asked(m judgment.MethodRef, th thread, cutOff time.Time, inputHash string, outcomes judgment.Outcomes, questions ...judgment.AnsweredQuestion) judgment.Judgment {
	j := judgmentOf(m, th, cutOff, inputHash, outcomes)
	j.Questions = questions
	return j
}

func moment(value judgment.BigMoment, confidence float64) judgment.Scored[judgment.BigMoment] {
	return judgment.Scored[judgment.BigMoment]{QuestionID: "big_moment", Outcome: value, Confidence: confidence}
}

// oneToOne seeds a contact and a one-to-one thread holding one message from
// them on day 1.
func (b *benchEnv) oneToOne(t *testing.T) (uuid.UUID, thread) {
	t.Helper()
	x, th := b.contact(t), b.thread(t)
	b.message(t, th, x, false, b.day(1))
	b.record(t, th, x, repository.InteractionDirectionInbound, b.day(1))
	return x, th
}

// nothingFound is a one-to-one judgment of x whose answers are all negative.
func nothingFound(x uuid.UUID) judgment.Outcomes {
	return judgment.Outcomes{
		BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtNobodyOwes, 0.8)},
		Commitments: []judgment.CommitmentOutcome{
			commitment(x, judgment.CommitmentDirectionMine, judgment.CommitmentStateNone, 0.9, nil),
			commitment(x, judgment.CommitmentDirectionTheirs, judgment.CommitmentStateNone, 0.9, nil),
		},
		BigMoment: moment(judgment.BigMomentNone, 0.95),
	}
}

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

// Each proposal, and each row, says what remains to review: items without a
// label, else negative decisions without a label, else nothing.
func TestBenchReviewStatus(t *testing.T) {
	t.Run("a proposal whose answers are all negative and unlabeled awaits its nones", func(t *testing.T) {
		// spec: IXN-054.proposal-status, IXN-051.row-review-status
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", nothingFound(x)))

		row := b.row(t, x)

		require.Len(t, row.Proposals, 1)
		p := row.Proposals[0]
		assert.Empty(t, p.Items)
		assert.Equal(t, 0, p.ToReview)
		assert.Equal(t, "nones_to_review", p.ReviewStatus)
		assert.Equal(t, ptr("nones_to_review"), row.ReviewStatus)
	})

	t.Run("a proposal with an unlabeled item awaits its items, with their count", func(t *testing.T) {
		// spec: IXN-054.proposal-status
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		found := nothingFound(x)
		found.BallInCourt[0].Outcome = judgment.BallInCourtIOweReply
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", found))

		p := b.proposal(t, x, prodMethod)

		assert.Equal(t, 1, p.ToReview)
		assert.Equal(t, "to_review", p.ReviewStatus)
	})

	t.Run("a proposal whose items and negatives all have labels is all reviewed", func(t *testing.T) {
		// spec: IXN-054.proposal-status
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		found := nothingFound(x)
		found.BallInCourt[0].Outcome = judgment.BallInCourtIOweReply
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", found))
		b.act(t, item(t, b.proposal(t, x, prodMethod), "i_owe_reply", x), "confirm")

		row := b.row(t, x)

		assert.Equal(t, "all_reviewed", row.Proposals[0].ReviewStatus)
		assert.Equal(t, ptr("all_reviewed"), row.ReviewStatus)
	})

	t.Run("an outcome with a no-effect value that suggests removing a live item is an item, not a negative", func(t *testing.T) {
		// spec: IXN-054.negative-decisions
		b := newBenchEnv(t)
		x, th := b.contact(t, "weekly"), b.thread(t)
		sent := accelerated.GetCurrentTime().UTC().Add(-2 * time.Hour).Truncate(time.Second)
		b.message(t, th, x, true, sent)
		b.record(t, th, x, repository.InteractionDirectionOutbound, sent)
		require.True(t, b.awaiting(t, x), "the outreach starts the wait")
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, sent.Add(time.Hour), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtNobodyOwes, 0.85)},
		}))

		p := b.proposal(t, x, prodMethod)

		require.Equal(t, ptr("live_with_change"), item(t, p, "owes_me_reply", x).State)
		assert.Equal(t, "to_review", p.ReviewStatus)
		assert.Equal(t, 1, p.ToReview)
		assert.False(t, p.ConfirmNones)
	})

	t.Run("a reply its party's commitment absorbs is not a negative decision", func(t *testing.T) {
		// spec: IXN-054.negative-decisions
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.9)},
			Commitments: []judgment.CommitmentOutcome{commitment(x, judgment.CommitmentDirectionMine, judgment.CommitmentStateOpen, 0.9, nil)},
		}))
		b.act(t, item(t, b.proposal(t, x, prodMethod), "commitments", x), "confirm")

		p := b.proposal(t, x, prodMethod)

		assert.Equal(t, "all_reviewed", p.ReviewStatus, "the absorbed reply awaits no review")
		assert.NotContains(t, b.labels(t), decision(judgment.QuestionBallInCourt, &x), "no rule labels the absorbed reply")
	})

	t.Run("a probability stored for a sub-question that yields no outcome adds no negative", func(t *testing.T) {
		// spec: IXN-054.negative-decisions
		b := newBenchEnv(t)
		x, th := b.oneToOne(t)
		bx := bic(x, judgment.BallInCourtNotApplicable, 0.9)
		b.run(t, judgment.RunKindScheduled, asked(prodMethod, th, b.day(1), "h1",
			judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bx}},
			answered(bx.QuestionID, map[string]float64{"not_applicable": 0.9, "nobody_owes": 0.1}),
			answered("commitment_mine/THEM", map[string]float64{"open": 0.4, "none": 0.6}),
		))
		require.Equal(t, http.StatusNoContent, b.confirmNones(t, b.proposal(t, x, prodMethod)).Code)

		p := b.proposal(t, x, prodMethod)

		assert.Equal(t, "all_reviewed", p.ReviewStatus)
		assert.Equal(t, map[string]string{decision(judgment.QuestionBallInCourt, &x): "not_applicable"}, b.labels(t))
	})

	// Two judgments of one thread at two inputs sit on the same row as two
	// proposals of different content.
	twoContents := func(t *testing.T, b *benchEnv, first, second func(x uuid.UUID) judgment.Outcomes) uuid.UUID {
		t.Helper()
		x, th := b.oneToOne(t)
		b.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, b.day(1), "h1", first(x)))
		b.run(t, judgment.RunKindRerun, judgmentOf(otherMethod, th, b.day(1).Add(time.Minute), "h2", second(x)))
		require.Len(t, b.row(t, x).Proposals, 2)
		return x
	}
	withItem := func(x uuid.UUID) judgment.Outcomes {
		o := nothingFound(x)
		o.BallInCourt[0].Outcome = judgment.BallInCourtIOweReply
		return o
	}

	t.Run("a row with one proposal awaiting items and another awaiting only nones awaits its items", func(t *testing.T) {
		// spec: IXN-051.row-review-status, IXN-054.proposal-status
		b := newBenchEnv(t)
		x := twoContents(t, b, withItem, nothingFound)

		row := b.row(t, x)

		statuses := map[string]int{}
		for _, p := range row.Proposals {
			statuses[p.ReviewStatus] = p.ToReview
		}
		assert.Equal(t, map[string]int{"to_review": 1, "nones_to_review": 0}, statuses, "each header describes only its proposal")
		assert.Equal(t, ptr("to_review"), row.ReviewStatus)
		assert.Equal(t, 1, row.ToReview)
	})

	t.Run("a row whose proposals await only nones awaits its nones, until every proposal is reviewed", func(t *testing.T) {
		// spec: IXN-051.row-review-status
		b := newBenchEnv(t)
		x := twoContents(t, b, nothingFound, nothingFound)
		require.Equal(t, ptr("nones_to_review"), b.row(t, x).ReviewStatus)

		require.Equal(t, http.StatusNoContent, b.confirmNones(t, b.row(t, x).Proposals[0]).Code)
		between := b.row(t, x)
		var rest handlers.BenchProposalResponse
		for _, p := range between.Proposals {
			if p.ReviewStatus != "all_reviewed" {
				rest = p
			}
		}
		require.Equal(t, http.StatusNoContent, b.confirmNones(t, rest).Code)
		after := b.row(t, x)

		assert.Equal(t, ptr("nones_to_review"), between.ReviewStatus, "one proposal still awaits its nones")
		assert.Equal(t, ptr("all_reviewed"), after.ReviewStatus)
	})

	t.Run("a row carrying no proposal carries no review status", func(t *testing.T) {
		// spec: IXN-051.row-review-status
		b := newBenchEnv(t)
		x, _ := b.oneToOne(t)

		assert.Nil(t, b.row(t, x).ReviewStatus)
	})
}
