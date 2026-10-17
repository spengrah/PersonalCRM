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
// narrowable to the rows one run's judgments are placed on.
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

	t.Run("a run filter keeps only the row each judgment is placed on, not the thread's other rows", func(t *testing.T) {
		// spec: IXN-051.run-keeps-judgment-rows
		b := newBenchEnv(t)
		c := b.contact(t)
		th := b.thread(t)
		b.record(t, th, c, repository.InteractionDirectionInbound, b.day(1))
		placed := b.record(t, th, c, repository.InteractionDirectionOutbound, b.day(2))
		b.record(t, th, c, repository.InteractionDirectionInbound, b.day(4))
		run, _ := b.run(t, judgment.RunKindHistorical, judgmentOf(prodMethod, th, b.day(3), "h-mid", judgment.Outcomes{}))

		page, meta := b.list(t, "run_id="+run.String())

		assert.Equal(t, []string{nodeOf(placed)}, rowIDs(page.Items), "the row holding the latest content at or before the cut-off, not the earlier or later rows of the thread")
		assert.Equal(t, int64(1), meta.Total)
	})

	t.Run("a run filter excludes a thread judged only in another run", func(t *testing.T) {
		// spec: IXN-051.run-keeps-judgment-rows
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
		// spec: IXN-051.run-keeps-judgment-rows
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

	t.Run("a suggested removal sits under the live obligation it targets, after the user's own reply obligation closed", func(t *testing.T) {
		// spec: IXN-052.removal-keeps-its-target
		b := newBenchEnv(t)
		x, th := b.contact(t), b.thread(t)
		b.message(t, th, x, false, b.day(1))
		b.record(t, th, x, repository.InteractionDirectionInbound, b.day(1))
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.9)},
		}))
		mine := b.openObligations(t, th)
		require.Len(t, mine, 1)
		_, err := b.tasks.UpdateContactTaskState(b.ctx, mine[0].TaskID, repository.ContactTaskStateCompleted)
		require.NoError(t, err)
		b.message(t, th, x, false, b.day(2))
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(2), "h2", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtTheyOweReply, 0.9)},
		}))
		theirs := b.openObligations(t, th)
		require.Len(t, theirs, 1)
		require.Equal(t, repository.ObligationDebtorContact, theirs[0].Debtor)
		b.message(t, th, x, false, b.day(3))
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(3), "h3", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtNobodyOwes, 0.85)},
		}))

		page, _ := b.list(t, "contact_id="+x.String())

		require.Len(t, page.Items, 1)
		var latest *handlers.BenchProposalResponse
		for i, p := range page.Items[0].Proposals {
			if b.day(3).Equal(p.CutOff) {
				latest = &page.Items[0].Proposals[i]
			}
		}
		require.NotNil(t, latest)
		assert.Empty(t, listContacts(*latest, "i_owe_reply"))
		removal := item(t, *latest, "owes_me_reply", x)
		assert.Equal(t, ptr("live_with_change"), removal.State)
		keep := acts(removal)["keep"]
		assert.Equal(t, handlers.BenchActResponse{Act: "keep", RightOutcome: "they_owe_reply"}, keep)
		_, err = b.store.ReviewOutcome(b.ctx, uuid.MustParse(removal.OutcomeID), judgment.ReviewRequest{RightOutcome: keep.RightOutcome})
		require.NoError(t, err)
		after := b.openObligations(t, th)
		require.Len(t, after, 1)
		assert.Equal(t, theirs[0].TaskID, after[0].TaskID, "Keep leaves the participant's obligation open and opens none of the user's")
	})

	t.Run("an item whose obligation closed is not acting, and confirming it reopens nothing", func(t *testing.T) {
		// spec: IXN-052.state-is-live-effect
		b := newBenchEnv(t)
		x, th := b.contact(t), b.thread(t)
		b.message(t, th, x, false, b.day(1))
		b.record(t, th, x, repository.InteractionDirectionInbound, b.day(1))
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{
			Commitments: []judgment.CommitmentOutcome{commitment(x, judgment.CommitmentDirectionMine, judgment.CommitmentStateOpen, 0.9, nil)},
		}))
		open := b.openObligations(t, th)
		require.Len(t, open, 1)
		_, err := b.tasks.UpdateContactTaskState(b.ctx, open[0].TaskID, repository.ContactTaskStateCompleted)
		require.NoError(t, err)

		page, _ := b.list(t, "contact_id="+x.String())

		done := item(t, page.Items[0].Proposals[0], "commitments", x)
		assert.Equal(t, ptr("not_acting"), done.State)
		confirm := acts(done)["confirm"]
		require.Equal(t, "open", confirm.RightOutcome)
		_, err = b.store.ReviewOutcome(b.ctx, uuid.MustParse(done.OutcomeID), judgment.ReviewRequest{RightOutcome: confirm.RightOutcome})
		require.NoError(t, err)
		assert.Empty(t, b.openObligations(t, th), "confirming a completed commitment records the label only")
		page, _ = b.list(t, "contact_id="+x.String())
		reviewed := item(t, page.Items[0].Proposals[0], "commitments", x)
		assert.Equal(t, ptr("not_acting"), reviewed.State)
		assert.Equal(t, ptr("confirmed"), reviewed.Mark)
	})

	t.Run("a confirmed reply stays live once a commitment the user adds supersedes its obligation", func(t *testing.T) {
		// spec: IXN-052.state-is-live-effect
		b := newBenchEnv(t)
		x, th := b.contact(t), b.thread(t)
		b.message(t, th, x, false, b.day(1))
		b.record(t, th, x, repository.InteractionDirectionInbound, b.day(1))
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.9)},
			Commitments: []judgment.CommitmentOutcome{commitment(x, judgment.CommitmentDirectionMine, judgment.CommitmentStateNone, 0.8, nil)},
		}))
		page, _ := b.list(t, "contact_id="+x.String())
		reply := item(t, page.Items[0].Proposals[0], "i_owe_reply", x)
		_, err := b.store.ReviewOutcome(b.ctx, uuid.MustParse(reply.OutcomeID), judgment.ReviewRequest{RightOutcome: acts(reply)["confirm"].RightOutcome})
		require.NoError(t, err)
		add := addition(t, page.Items[0].Proposals[0], "commitments", x)
		_, err = b.store.ReviewOutcome(b.ctx, uuid.MustParse(add.OutcomeID), judgment.ReviewRequest{RightOutcome: add.RightOutcome})
		require.NoError(t, err)
		open := b.openObligations(t, th)
		require.Len(t, open, 1)
		require.Equal(t, repository.ObligationBasisCommitment, open[0].Basis, "the commitment supersedes the reply")

		page, _ = b.list(t, "contact_id="+x.String())

		confirmed := item(t, page.Items[0].Proposals[0], "i_owe_reply", x)
		assert.Equal(t, ptr("confirmed"), confirmed.Mark)
		assert.Equal(t, ptr("live"), confirmed.State, "the commitment continues the reply's obligation")
		assert.Equal(t, ptr("live"), item(t, page.Items[0].Proposals[0], "commitments", x).State)
	})

	// A replacement obligation opens another obligation of the completed
	// commitment's party: through a later judgment, through the reply addition
	// the same proposal offers, or through another version of the production
	// method judging the same content at the same cut-off.
	replacements := []struct {
		name    string
		replace func(t *testing.T, b *benchEnv, th thread, x uuid.UUID, reply judgment.BallInCourt, list string)
	}{
		{"a later judgment's reply obligation", func(t *testing.T, b *benchEnv, th thread, x uuid.UUID, reply judgment.BallInCourt, _ string) {
			b.message(t, th, x, false, b.day(2))
			b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(2), "h2", judgment.Outcomes{
				BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, reply, 0.9)},
			}))
		}},
		{"the reply its own proposal adds", func(t *testing.T, b *benchEnv, th thread, x uuid.UUID, _ judgment.BallInCourt, list string) {
			page, _ := b.list(t, "contact_id="+x.String())
			add := addition(t, page.Items[0].Proposals[0], list, x)
			_, err := b.store.ReviewOutcome(b.ctx, uuid.MustParse(add.OutcomeID), judgment.ReviewRequest{RightOutcome: add.RightOutcome})
			require.NoError(t, err)
		}},
		{"another production version's reply obligation at the same cut-off", func(t *testing.T, b *benchEnv, th thread, x uuid.UUID, reply judgment.BallInCourt, _ string) {
			v2 := prodMethod
			v2.Version = "v2"
			b.run(t, judgment.RunKindHistorical, judgmentOf(v2, th, b.day(1), "h1", judgment.Outcomes{
				BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, reply, 0.9)},
			}))
		}},
	}
	for _, tc := range []struct {
		name      string
		direction judgment.CommitmentDirection
		reply     judgment.BallInCourt
		list      string
		debtor    string
	}{
		{"mine", judgment.CommitmentDirectionMine, judgment.BallInCourtIOweReply, "i_owe_reply", repository.ObligationDebtorUser},
		{"theirs", judgment.CommitmentDirectionTheirs, judgment.BallInCourtTheyOweReply, "owes_me_reply", repository.ObligationDebtorContact},
	} {
		for _, r := range replacements {
			t.Run("a completed "+tc.name+" commitment stays not acting beside "+r.name+", and confirming it keeps that reply", func(t *testing.T) {
				// spec: IXN-052.state-is-live-effect
				b := newBenchEnv(t)
				x, th := b.contact(t), b.thread(t)
				b.message(t, th, x, false, b.day(1))
				b.record(t, th, x, repository.InteractionDirectionInbound, b.day(1))
				b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{
					BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtNobodyOwes, 0.8)},
					Commitments: []judgment.CommitmentOutcome{commitment(x, tc.direction, judgment.CommitmentStateOpen, 0.9, nil)},
				}))
				open := b.openObligations(t, th)
				require.Len(t, open, 1)
				_, err := b.tasks.UpdateContactTaskState(b.ctx, open[0].TaskID, repository.ContactTaskStateCompleted)
				require.NoError(t, err)
				r.replace(t, b, th, x, tc.reply, tc.list)
				reply := b.openObligations(t, th)
				require.Len(t, reply, 1)
				require.Equal(t, tc.debtor, reply[0].Debtor)
				require.Equal(t, repository.ObligationBasisReply, reply[0].Basis)

				page, _ := b.list(t, "contact_id="+x.String())

				var first *handlers.BenchProposalResponse
				for i, p := range page.Items[0].Proposals {
					if b.day(1).Equal(p.CutOff) && p.MethodVersion == prodMethod.Version {
						first = &page.Items[0].Proposals[i]
					}
				}
				require.NotNil(t, first)
				done := item(t, *first, "commitments", x)
				assert.Equal(t, ptr("not_acting"), done.State, "another reply obligation is not the completed commitment's effect")
				confirm := acts(done)["confirm"]
				require.Equal(t, "open", confirm.RightOutcome)
				_, err = b.store.ReviewOutcome(b.ctx, uuid.MustParse(done.OutcomeID), judgment.ReviewRequest{RightOutcome: confirm.RightOutcome})
				require.NoError(t, err)
				after := b.openObligations(t, th)
				require.Len(t, after, 1)
				assert.Equal(t, reply[0].TaskID, after[0].TaskID, "confirming the completed commitment leaves the reply task open")
				assert.Equal(t, repository.ObligationBasisReply, after[0].Basis)
			})
		}
	}
}

// A review acts on the obligation its item's live effect is, followed through
// the tasks continuing it, and on the contact the item's contact now is: it
// never takes away another obligation of the same party, and a contact merge
// does not detach the item from its obligation.
func TestBenchReviewActsOnItsOwnObligation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		direction judgment.CommitmentDirection
		debtor    string
	}{
		{"mine", judgment.CommitmentDirectionMine, repository.ObligationDebtorUser},
		{"theirs", judgment.CommitmentDirectionTheirs, repository.ObligationDebtorContact},
	} {
		t.Run("removing a completed "+tc.name+" commitment leaves a later judgment's commitment open", func(t *testing.T) {
			// spec: IXN-052.state-is-live-effect
			b := newBenchEnv(t)
			x, th := b.contact(t), b.thread(t)
			b.message(t, th, x, false, b.day(1))
			b.record(t, th, x, repository.InteractionDirectionInbound, b.day(1))
			b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{
				Commitments: []judgment.CommitmentOutcome{commitment(x, tc.direction, judgment.CommitmentStateOpen, 0.9, nil)},
			}))
			first := b.openObligations(t, th)
			require.Len(t, first, 1)
			_, err := b.tasks.UpdateContactTaskState(b.ctx, first[0].TaskID, repository.ContactTaskStateCompleted)
			require.NoError(t, err)
			b.message(t, th, x, false, b.day(2))
			b.record(t, th, x, repository.InteractionDirectionInbound, b.day(2))
			b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(2), "h2", judgment.Outcomes{
				Commitments: []judgment.CommitmentOutcome{commitment(x, tc.direction, judgment.CommitmentStateOpen, 0.9, nil)},
			}))
			replacement := b.openObligations(t, th)
			require.Len(t, replacement, 1)
			require.Equal(t, tc.debtor, replacement[0].Debtor)
			require.Equal(t, repository.ObligationBasisCommitment, replacement[0].Basis)

			page, _ := b.list(t, "contact_id="+x.String())
			var old *handlers.BenchProposalResponse
			for _, row := range page.Items {
				for i, p := range row.Proposals {
					if b.day(1).Equal(p.CutOff) {
						old = &row.Proposals[i]
					}
				}
			}
			require.NotNil(t, old)
			done := item(t, *old, "commitments", x)
			require.Equal(t, ptr("not_acting"), done.State)
			remove := acts(done)["remove"]
			require.Equal(t, "none", remove.RightOutcome)
			_, err = b.store.ReviewOutcome(b.ctx, uuid.MustParse(done.OutcomeID), judgment.ReviewRequest{RightOutcome: remove.RightOutcome})
			require.NoError(t, err)

			assert.Equal(t, replacement, b.openObligations(t, th), "removing the completed commitment records the label only")
		})
	}

	t.Run("rejecting an earlier suggested reply leaves a later judgment's reply obligation open", func(t *testing.T) {
		// spec: IXN-052.state-is-live-effect, KNW-046.live-review-gives-right-effect
		b := newBenchEnv(t)
		x, th := b.contact(t), b.thread(t)
		b.message(t, th, x, false, b.day(1))
		b.record(t, th, x, repository.InteractionDirectionInbound, b.day(1))
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.49)},
		}))
		suggested := b.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &x)
		require.Equal(t, judgment.OutcomeStateClaim, suggested.State)
		b.message(t, th, x, false, b.day(2))
		b.record(t, th, x, repository.InteractionDirectionInbound, b.day(2))
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(2), "h2", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.9)},
		}))
		later := b.openObligations(t, th)
		require.Len(t, later, 1)

		_, err := b.store.ReviewOutcome(b.ctx, suggested.ID, judgment.ReviewRequest{RightOutcome: "nobody_owes"})
		require.NoError(t, err)

		assert.Equal(t, later, b.openObligations(t, th), "rejecting the suggestion takes away nothing it did not add")
	})

	t.Run("moving a completed reply item to the other reply list records the label only", func(t *testing.T) {
		// spec: IXN-052.state-is-live-effect
		b := newBenchEnv(t)
		x, th := b.contact(t), b.thread(t)
		b.message(t, th, x, false, b.day(1))
		b.record(t, th, x, repository.InteractionDirectionInbound, b.day(1))
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.9)},
		}))
		open := b.openObligations(t, th)
		require.Len(t, open, 1)
		_, err := b.tasks.UpdateContactTaskState(b.ctx, open[0].TaskID, repository.ContactTaskStateCompleted)
		require.NoError(t, err)

		_, err = b.store.ReviewOutcome(b.ctx, b.outcome(t, th, prodMethod, judgment.QuestionBallInCourt, &x).ID,
			judgment.ReviewRequest{RightOutcome: "they_owe_reply"})
		require.NoError(t, err)

		assert.Empty(t, b.openObligations(t, th), "a review of a not-acting item opens nothing")
	})

	merged := func(t *testing.T, b *benchEnv, th thread, outcomes func(retired uuid.UUID) judgment.Outcomes) (retired, survivor uuid.UUID) {
		t.Helper()
		retired, survivor = b.contact(t), b.contact(t)
		b.message(t, th, retired, false, b.day(1))
		b.record(t, th, retired, repository.InteractionDirectionInbound, b.day(1))
		b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", outcomes(retired)))
		_, err := b.contacts.MergeContacts(b.ctx, service.MergeContactsRequest{SourceContactID: retired, TargetContactID: survivor})
		require.NoError(t, err)
		return retired, survivor
	}
	outcomeOf := func(t *testing.T, b *benchEnv, th thread, question judgment.Question) uuid.UUID {
		t.Helper()
		outcomes := b.outcomes(t, th)
		require.Len(t, outcomes, 1)
		require.Equal(t, question, outcomes[0].Question)
		return outcomes[0].ID
	}

	t.Run("removing a participant's commitment after a contact merge closes the obligation the merge moved", func(t *testing.T) {
		// spec: IXN-052.state-is-live-effect, KNW-046.live-review-gives-right-effect
		b := newBenchEnv(t)
		th := b.thread(t)
		_, survivor := merged(t, b, th, func(retired uuid.UUID) judgment.Outcomes {
			return judgment.Outcomes{Commitments: []judgment.CommitmentOutcome{commitment(retired, judgment.CommitmentDirectionTheirs, judgment.CommitmentStateOpen, 0.9, nil)}}
		})
		moved := b.openObligations(t, th)
		require.Len(t, moved, 1)
		require.Equal(t, survivor, moved[0].ContactID, "the merge moves the obligation to the surviving contact")

		_, err := b.store.ReviewOutcome(b.ctx, outcomeOf(t, b, th, judgment.QuestionCommitmentTheirs), judgment.ReviewRequest{RightOutcome: "none"})
		require.NoError(t, err)

		assert.Empty(t, b.openObligations(t, th), "the review removes the commitment the merge moved")
	})

	t.Run("confirming a suggested reply after a contact merge opens it for the surviving contact", func(t *testing.T) {
		// spec: KNW-046.live-review-gives-right-effect
		b := newBenchEnv(t)
		th := b.thread(t)
		_, survivor := merged(t, b, th, func(retired uuid.UUID) judgment.Outcomes {
			return judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(retired, judgment.BallInCourtIOweReply, 0.49)}}
		})
		require.Empty(t, b.openObligations(t, th))

		_, err := b.store.ReviewOutcome(b.ctx, outcomeOf(t, b, th, judgment.QuestionBallInCourt), judgment.ReviewRequest{RightOutcome: "i_owe_reply"})
		require.NoError(t, err)

		open := b.openObligations(t, th)
		require.Len(t, open, 1)
		assert.Equal(t, survivor, open[0].ContactID)
		assert.Equal(t, repository.ObligationDebtorUser, open[0].Debtor)
	})
}

// addition returns the one choice of the proposal's pickers adding the
// contact to the list.
func addition(t *testing.T, p handlers.BenchProposalResponse, list string, contact uuid.UUID) handlers.BenchAdditionResponse {
	t.Helper()
	var found []handlers.BenchAdditionResponse
	for _, a := range p.Additions {
		if a.List == list && a.Contact != nil && a.Contact.ContactID == contact.String() {
			found = append(found, a)
		}
	}
	require.Len(t, found, 1, "%s additions naming %s", list, contact)
	return found[0]
}

// messageKey is the key of the thread's one message.
func (e *env) messageKey(t *testing.T, th thread) string {
	t.Helper()
	rows, err := repository.NewThreadContentRepository(e.database.Queries).ListThreadContent(e.ctx, th.id, accelerated.GetCurrentTime())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	return rows[0].MessageKey
}
