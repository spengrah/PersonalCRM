//go:build integration_testdb

package judgment_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// benchEnv serves GET /api/v1/interactions over an env on a database of its
// own, so the list across every contact holds only what the test seeds.
type benchEnv struct {
	*env
	router *gin.Engine
}

func newBenchEnv(t *testing.T) *benchEnv {
	t.Helper()
	e := newEnv(t, production, withOwnDatabase())
	q := e.database.Queries
	content := service.NewInteractionContentService(
		repository.NewInteractionRepository(q), repository.NewCommsMessageRepository(q),
		repository.NewTelegramMessageRepository(q), repository.NewMessagesMessageRepository(q),
		repository.NewMeetingNoteRepository(q), repository.NewCalendarEventRepository(q),
		repository.NewPhoneCallRepository(q), e.people,
	)
	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	handlers.RegisterBenchRoutes(router.Group("/api/v1"), handlers.NewBenchHandler(judgment.NewBench(e.store, content)))
	return &benchEnv{env: e, router: router}
}

// list reads one page of the view.
func (b *benchEnv) list(t *testing.T, query string) (handlers.AllInteractionsResponse, api.PaginationMeta) {
	t.Helper()
	w := httptest.NewRecorder()
	b.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/interactions?"+query, nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var envelope struct {
		Data handlers.AllInteractionsResponse `json:"data"`
		Meta api.Meta                         `json:"meta"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.NotNil(t, envelope.Meta.Pagination)
	return envelope.Data, *envelope.Meta.Pagination
}

// manual records a manual interaction of the contact.
func (b *benchEnv) manual(t *testing.T, contact uuid.UUID, at time.Time) *repository.Interaction {
	t.Helper()
	ix, err := b.contacts.RecordInteraction(b.ctx, repository.RecordInteractionRequest{
		ContactID: contact, Source: repository.InteractionSourceManual, OccurredAt: at, Direction: repository.InteractionDirectionOutbound,
	})
	require.NoError(t, err)
	return ix
}

func rowIDs(items []handlers.AllInteractionItemResponse) []string {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.Item.Identity.ID
	}
	return ids
}

func participantIDs(item handlers.AllInteractionItemResponse) []string {
	ids := make([]string, len(item.Participants))
	for i, p := range item.Participants {
		ids[i] = p.ContactID
	}
	return ids
}

func nodeOf(ix *repository.Interaction) string { return ix.InteractionNodeID.String() }

// The all-interactions list returns every interaction across contacts, one
// row per interaction, paginated, filterable by contact and by type, and
// narrowable to the rows of threads judged in one run.
func TestListAllInteractions(t *testing.T) {
	t.Run("pages cover every interaction across contacts once, a shared one as one row", func(t *testing.T) {
		// spec: IXN-051.every-interaction-one-row, IXN-051.pages-without-gaps
		b := newBenchEnv(t)
		alice, bea, cy := b.contact(t), b.contact(t), b.contact(t)
		shared := b.thread(t)
		sharedA := b.record(t, shared, alice, repository.InteractionDirectionOutbound, b.day(1))
		sharedB := b.record(t, shared, bea, repository.InteractionDirectionOutbound, b.day(1))
		require.Equal(t, nodeOf(sharedA), nodeOf(sharedB), "one email thread and day is one interaction")
		aliceRow := b.record(t, b.thread(t), alice, repository.InteractionDirectionInbound, b.day(2))
		beaManual := b.manual(t, bea, b.day(3))
		cyRow := b.record(t, b.thread(t), cy, repository.InteractionDirectionInbound, b.day(4))

		first, meta := b.list(t, "limit=3&page=1")
		second, meta2 := b.list(t, "limit=3&page=2")

		assert.Equal(t, api.PaginationMeta{Page: 1, Limit: 3, Total: 4, Pages: 2}, meta)
		assert.Equal(t, int64(4), meta2.Total)
		assert.Equal(t, []string{nodeOf(cyRow), nodeOf(beaManual), nodeOf(aliceRow)}, rowIDs(first.Items))
		require.Equal(t, []string{nodeOf(sharedA)}, rowIDs(second.Items))
		assert.ElementsMatch(t, []string{alice.String(), bea.String()}, participantIDs(second.Items[0]))
		assert.Contains(t, []string{alice.String(), bea.String()}, second.Items[0].ContextContactID)
	})

	t.Run("contact and type filters combine", func(t *testing.T) {
		// spec: IXN-051.filters-combine
		b := newBenchEnv(t)
		alice, bea := b.contact(t), b.contact(t)
		shared := b.thread(t)
		sharedA := b.record(t, shared, alice, repository.InteractionDirectionOutbound, b.day(1))
		b.record(t, shared, bea, repository.InteractionDirectionOutbound, b.day(1))
		aliceEmail := b.record(t, b.thread(t), alice, repository.InteractionDirectionInbound, b.day(2))
		b.manual(t, alice, b.day(3))
		b.record(t, b.thread(t), bea, repository.InteractionDirectionInbound, b.day(4))

		page, meta := b.list(t, "contact_id="+alice.String()+"&type=email")

		assert.Equal(t, []string{nodeOf(aliceEmail), nodeOf(sharedA)}, rowIDs(page.Items))
		assert.Equal(t, int64(2), meta.Total)
		keys := make([]string, len(page.TypeOptions))
		for i, o := range page.TypeOptions {
			keys[i] = o.Key
		}
		assert.Equal(t, []string{"message", "email", "calendar", "call", "other"}, keys)
	})

	t.Run("a run filter excludes a thread judged only in another run", func(t *testing.T) {
		// spec: IXN-051.run-keeps-judged-threads
		b := newBenchEnv(t)
		c := b.contact(t)
		judged, other := b.thread(t), b.thread(t)
		judgedRow := b.record(t, judged, c, repository.InteractionDirectionInbound, b.day(1))
		b.record(t, other, c, repository.InteractionDirectionInbound, b.day(2))
		b.manual(t, c, b.day(3))
		run, _ := b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, judged, b.day(1), "h-judged", judgment.Outcomes{}))
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, other, b.day(2), "h-other", judgment.Outcomes{}))

		page, meta := b.list(t, "run_id="+run.String())

		assert.Equal(t, []string{nodeOf(judgedRow)}, rowIDs(page.Items))
		assert.Equal(t, int64(1), meta.Total)
	})

	t.Run("a run filter includes a thread whose judgment the run reused", func(t *testing.T) {
		// spec: IXN-051.run-keeps-judged-threads
		b := newBenchEnv(t)
		c := b.contact(t)
		th := b.thread(t)
		row := b.record(t, th, c, repository.InteractionDirectionInbound, b.day(1))
		b.record(t, b.thread(t), c, repository.InteractionDirectionInbound, b.day(2))
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{}))
		reusing, err := b.store.StartRun(b.ctx, judgment.RunSpec{Kind: judgment.RunKindScheduled, Method: prodMethod})
		require.NoError(t, err)
		_, found, err := b.store.ReuseJudgment(b.ctx, reusing, th.id, b.day(5), "h1")
		require.NoError(t, err)
		require.True(t, found)

		page, _ := b.list(t, "run_id="+reusing.String())

		require.Equal(t, []string{nodeOf(row)}, rowIDs(page.Items))
		require.Len(t, page.Items[0].Proposals, 1)
		require.NotNil(t, page.Items[0].Proposals[0].ReusedAt)
		assert.True(t, b.day(5).Equal(*page.Items[0].Proposals[0].ReusedAt))
	})
}

// item returns the one item of the list naming the contact.
func item(t *testing.T, p handlers.BenchProposalResponse, list string, contact uuid.UUID) handlers.BenchItemResponse {
	t.Helper()
	var found []handlers.BenchItemResponse
	for _, it := range p.Items {
		if it.List == list && it.Contact != nil && it.Contact.ContactID == contact.String() {
			found = append(found, it)
		}
	}
	require.Len(t, found, 1, "%s items naming %s", list, contact)
	return found[0]
}

func listContacts(p handlers.BenchProposalResponse, list string) []string {
	var ids []string
	for _, it := range p.Items {
		if it.List == list && it.Contact != nil {
			ids = append(ids, it.Contact.ContactID)
		}
	}
	return ids
}

func acts(it handlers.BenchItemResponse) map[string]handlers.BenchActResponse {
	out := map[string]handlers.BenchActResponse{}
	for _, a := range it.Acts {
		out[a.Act] = a
	}
	return out
}

func ptr[T any](v T) *T { return &v }

// The bench read returns each judged thread's proposal compiled into the
// lists the user reviews, each item with its state and the review controls it
// offers, so the client applies no judgment rule.
func TestBenchReadCompilesProposals(t *testing.T) {
	t.Run("a group thread where two participants owe the user a reply", func(t *testing.T) {
		// spec: IXN-052.answers-not-questions, IXN-052.state-and-acts
		b := newBenchEnv(t)
		bea, cy := b.contact(t), b.contact(t)
		th := b.thread(t)
		b.message(t, th, bea, false, b.day(1))
		b.message(t, th, cy, false, b.day(1).Add(time.Minute))
		b.record(t, th, bea, repository.InteractionDirectionInbound, b.day(1))
		b.record(t, th, cy, repository.InteractionDirectionInbound, b.day(1))
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1).Add(time.Minute), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{
				bic(bea, judgment.BallInCourtTheyOweReply, 0.9), bic(cy, judgment.BallInCourtTheyOweReply, 0.8),
			},
		}))

		page, _ := b.list(t, "contact_id="+bea.String())

		require.Len(t, page.Items, 1)
		require.Len(t, page.Items[0].Proposals, 1)
		p := page.Items[0].Proposals[0]
		assert.True(t, p.Production)
		assert.True(t, p.ReplyLists)
		assert.ElementsMatch(t, []string{bea.String(), cy.String()}, listContacts(p, "owes_me_reply"))
		assert.Empty(t, listContacts(p, "i_owe_reply"))
		owed := item(t, p, "owes_me_reply", cy)
		assert.Equal(t, ptr("live"), owed.State)
		assert.Equal(t, ptr(0.8), owed.Confidence)
		assert.Equal(t, handlers.BenchActResponse{Act: "confirm", RightOutcome: "they_owe_reply"}, acts(owed)["confirm"])
		assert.Equal(t, handlers.BenchActResponse{Act: "remove", RightOutcome: "nobody_owes"}, acts(owed)["remove"])
		assert.Equal(t, 2, p.ToReview)
		assert.Equal(t, 2, page.Items[0].ToReview)
	})

	t.Run("a commitment supersedes a reply for the same party and carries its source message", func(t *testing.T) {
		// spec: IXN-052.commitment-absorbs-reply, IXN-052.commitment-source-message
		b := newBenchEnv(t)
		x := b.contact(t)
		th := b.thread(t)
		b.message(t, th, x, false, b.day(1))
		b.record(t, th, x, repository.InteractionDirectionInbound, b.day(1))
		key := b.messageKey(t, th)
		due := b.day(9)
		mine := commitment(x, judgment.CommitmentDirectionMine, judgment.CommitmentStateOpen, 0.9, date(due))
		j := judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.9)},
			Commitments: []judgment.CommitmentOutcome{mine},
		})
		j.Questions = append(j.Questions, judgment.AnsweredQuestion{
			ID: judgment.SourceMessageQuestionID(mine.QuestionID), Probabilities: map[string]float64{key: 0.95}, OriginalAnswer: key,
		})
		b.run(t, judgment.RunKindScheduled, j)

		page, _ := b.list(t, "contact_id="+x.String())

		require.Len(t, page.Items, 1)
		p := page.Items[0].Proposals[0]
		assert.Empty(t, listContacts(p, "i_owe_reply"), "the reply that states a commitment is the commitment's obligation")
		c := item(t, p, "commitments", x)
		assert.Equal(t, ptr("me"), c.Debtor)
		assert.Equal(t, ptr(due.Format("2006-01-02")), c.DueDate)
		require.NotNil(t, c.Source)
		assert.Equal(t, key, c.Source.ContentRef)
		assert.Equal(t, "synthetic message body", c.Source.Text)
		assert.True(t, b.day(1).Equal(c.Source.SentAt))
	})

	t.Run("a non-production method's judgment is not acting", func(t *testing.T) {
		// spec: IXN-052.state-and-acts
		b := newBenchEnv(t)
		x := b.contact(t)
		th := b.thread(t)
		b.message(t, th, x, false, b.day(1))
		b.record(t, th, x, repository.InteractionDirectionInbound, b.day(1))
		b.run(t, judgment.RunKindScheduled, judgmentOf(otherMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.7)},
		}))

		page, _ := b.list(t, "contact_id="+x.String())

		p := page.Items[0].Proposals[0]
		assert.False(t, p.Production)
		assert.Equal(t, "other-method", p.MethodKey)
		assert.Equal(t, ptr("not_acting"), item(t, p, "i_owe_reply", x).State)
		assert.Empty(t, b.openObligations(t, th))
	})

	t.Run("a live item with a suggested removal", func(t *testing.T) {
		// spec: IXN-052.state-and-acts
		b := newBenchEnv(t)
		x, th := b.contact(t, "weekly"), b.thread(t)
		sent := accelerated.GetCurrentTime().UTC().Add(-2 * time.Hour).Truncate(time.Second)
		b.message(t, th, x, true, sent)
		b.record(t, th, x, repository.InteractionDirectionOutbound, sent)
		require.True(t, b.awaiting(t, x), "the outreach starts the wait")
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, sent.Add(time.Hour), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtNobodyOwes, 0.85)},
		}))

		page, _ := b.list(t, "contact_id="+x.String())

		p := page.Items[0].Proposals[0]
		wait := item(t, p, "owes_me_reply", x)
		assert.Equal(t, ptr("live_with_change"), wait.State)
		assert.Nil(t, wait.Confidence, "the wait is the timestamp rule's, not the judgment's")
		require.NotNil(t, wait.Change)
		assert.Equal(t, "remove", wait.Change.Kind)
		assert.Equal(t, ptr(0.85), wait.Change.Confidence)
		assert.Equal(t, handlers.BenchActResponse{Act: "accept", RightOutcome: "nobody_owes"}, acts(wait)["accept"])
		assert.Equal(t, handlers.BenchActResponse{Act: "keep", RightOutcome: "they_owe_reply"}, acts(wait)["keep"])
		assert.True(t, b.awaiting(t, x), "the claim leaves the wait in place")
	})
}

// messageKey is the key of the thread's one message.
func (e *env) messageKey(t *testing.T, th thread) string {
	t.Helper()
	rows, err := repository.NewThreadContentRepository(e.database.Queries).ListThreadContent(e.ctx, th.id, accelerated.GetCurrentTime())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	return rows[0].MessageKey
}
