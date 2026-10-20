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
	"personal-crm/backend/internal/config"
	decisions "personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// benchEnv serves GET /api/v1/interactions, the Threads tab's routes and the
// judgment routes over an env on a database of its own, so the lists across
// every contact hold only what the test seeds. methods is the method
// registry, whose keys are the compared methods.
type benchEnv struct {
	*env
	router  *gin.Engine
	methods *decisions.Registry
}

func newBenchEnv(t *testing.T, opts ...envOption) *benchEnv {
	t.Helper()
	return newBenchEnvFor(t, production, opts...)
}

// newBenchEnvFor is the env whose store designates productionMethod.
func newBenchEnvFor(t *testing.T, productionMethod string, opts ...envOption) *benchEnv {
	t.Helper()
	e := newEnv(t, productionMethod, append([]envOption{withOwnDatabase()}, opts...)...)
	q := e.database.Queries
	content := service.NewInteractionContentService(
		repository.NewInteractionRepository(q), repository.NewCommsMessageRepository(q),
		repository.NewTelegramMessageRepository(q), repository.NewMessagesMessageRepository(q),
		repository.NewMeetingNoteRepository(q), repository.NewCalendarEventRepository(q),
		repository.NewPhoneCallRepository(q), e.people,
	)
	methods := decisions.NewRegistry(config.DecisionConfig{}, nil)
	threads := judgment.NewThreads(e.store, methods)
	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	handlers.RegisterAllInteractionsRoutes(router.Group("/api/v1"), handlers.NewAllInteractionsHandler(content))
	handlers.RegisterThreadRoutes(router.Group("/api/v1"), handlers.NewThreadHandler(threads))
	handlers.RegisterJudgmentRoutes(router.Group("/api/v1"), handlers.NewJudgmentHandler(e.store))
	return &benchEnv{env: e, router: router, methods: methods}
}

// get reads a route of the env.
func (b *benchEnv) get(path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	b.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// list reads one page of the all-interactions list.
func (b *benchEnv) list(t *testing.T, query string) (handlers.AllInteractionsResponse, api.PaginationMeta) {
	t.Helper()
	w := b.get("/api/v1/interactions?" + query)
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
// row per interaction, paginated, filterable by contact and by type. A row
// carries no judgment, and the list offers no run filter.
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
		assert.Equal(t, []string{"message", "email", "event", "call", "other"}, keys)
	})

	t.Run("a calendar meeting and an anarlog session are events, and a manual interaction is other", func(t *testing.T) {
		// spec: IXN-051.filters-combine
		b := newBenchEnv(t)
		c := b.contact(t)
		var events []string
		for i, source := range []string{repository.InteractionSourceGCal, repository.InteractionSourceAnarlogSessions} {
			ref := "event-" + uuid.NewString()
			ix, err := b.contacts.RecordInteraction(b.ctx, repository.RecordInteractionRequest{
				ContactID: c, Source: source, SourceRef: &ref, OccurredAt: b.day(i + 1), Direction: repository.InteractionDirectionMutual,
			})
			require.NoError(t, err)
			events = append(events, nodeOf(ix))
		}
		manual := b.manual(t, c, b.day(3))

		event, _ := b.list(t, "type=event")
		other, _ := b.list(t, "type=other")

		assert.ElementsMatch(t, events, rowIDs(event.Items))
		assert.Equal(t, []string{nodeOf(manual)}, rowIDs(other.Items))
	})

	t.Run("a row whose thread a run judged carries no judgment, and a request naming the run answers as the same request without it", func(t *testing.T) {
		// spec: IXN-051.carries-no-judgment
		b := newBenchEnv(t)
		c := b.contact(t)
		th := b.thread(t)
		judged := b.record(t, th, c, repository.InteractionDirectionInbound, b.day(1))
		b.message(t, th, c, false, b.day(1))
		other := b.record(t, b.thread(t), c, repository.InteractionDirectionInbound, b.day(2))
		run, _ := b.run(t, judgment.RunKindScheduled, judgmentOf(prodMethod, th, b.day(1), "h1", judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(c, judgment.BallInCourtIOweReply, 0.9)},
		}))

		plain := b.get("/api/v1/interactions")
		named := b.get("/api/v1/interactions?run_id=" + run.String())

		require.Equal(t, http.StatusOK, plain.Code, plain.Body.String())
		assert.JSONEq(t, plain.Body.String(), named.Body.String())
		var envelope struct {
			Data struct {
				Items []map[string]json.RawMessage `json:"items"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(plain.Body.Bytes(), &envelope))
		require.Len(t, envelope.Data.Items, 2)
		for _, item := range envelope.Data.Items {
			for _, field := range []string{"proposals", "review_status", "to_review"} {
				assert.NotContains(t, item, field)
			}
		}
		ids := rowIDs(decodeData[handlers.AllInteractionsResponse](t, plain).Items)
		assert.ElementsMatch(t, []string{nodeOf(judged), nodeOf(other)}, ids, "the judged thread's row is listed like any other")
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
