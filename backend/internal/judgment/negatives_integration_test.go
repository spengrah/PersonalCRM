//go:build integration_testdb

package judgment_test

import (
	"bytes"
	"cmp"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
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

// proposals reads, through the thread read of every run, the reply proposal of
// each judgment of the threads the contact takes part in, each once: the
// production method's first, then by method, version and cut-off.
func (b *benchEnv) proposals(t *testing.T, contact uuid.UUID) []handlers.BenchProposalResponse {
	t.Helper()
	runs, err := b.store.ListRuns(b.ctx)
	require.NoError(t, err)
	var out []handlers.BenchProposalResponse
	seen := map[string]bool{}
	list, _ := b.threadList(t, "contact_id="+contact.String())
	for _, row := range list.Items {
		for _, run := range runs {
			for _, n := range b.readThread(t, row.ThreadID, "run_id="+run.ID.String()).Review.Nights {
				if !seen[n.JudgmentID] {
					seen[n.JudgmentID] = true
					out = append(out, n.Proposal)
				}
			}
		}
	}
	slices.SortStableFunc(out, func(a, b handlers.BenchProposalResponse) int {
		return cmp.Or(
			-cmp.Compare(boolRank(a.Production), boolRank(b.Production)),
			cmp.Compare(a.MethodKey, b.MethodKey), cmp.Compare(a.MethodVersion, b.MethodVersion), a.CutOff.Compare(b.CutOff))
	})
	return out
}

func boolRank(v bool) int {
	if v {
		return 1
	}
	return 0
}

// proposal reads the one proposal of method m on the contact's threads.
func (b *benchEnv) proposal(t *testing.T, contact uuid.UUID, m judgment.MethodRef) handlers.BenchProposalResponse {
	t.Helper()
	var found []handlers.BenchProposalResponse
	for _, p := range b.proposals(t, contact) {
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
