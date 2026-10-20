package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/threadctx"
	"personal-crm/backend/tests/testsupport"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// judgmentEnv serves the judgment routes over a real judgment store whose
// production method is judgmentProduction. Judgments are seeded through the
// store; River jobs are inserted by a client that never runs them.
type judgmentEnv struct {
	ctx      context.Context
	database *db.Database
	harness  *synthetic.Harness
	store    *judgment.Store
	router   *gin.Engine
	comms    *repository.CommsMessageRepository
	venues   *repository.VenueRepository
	base     time.Time
}

const judgmentProduction = "prod-method"

var judgmentMethod = judgment.MethodRef{Key: judgmentProduction, Version: "v1", WordingVersion: "w1"}

func newJudgmentEnv(t *testing.T) *judgmentEnv {
	t.Helper()
	ctx := t.Context()
	harness := synthetic.NewHarnessForNamespace(t, ctx, "b"+uuid.NewString()[:8], factory.DefaultSeed)
	database := harness.Database()
	jobs, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{SkipUnknownJobCheck: true})
	require.NoError(t, err)
	bus := events.NewBus(database.Pool, jobs, repository.NewEventRepository(database.Queries))
	assertSvc, _ := buildKnowledgeDepsForAPITest(t, database, bus)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	cadence := consumer.NewCadenceUpdater(repository.NewEventConsumerClaimRepository(database.Queries), contactRepo,
		database.Queries, consumer.CadenceModeCutover, false, config.TestConfig().Watchdog)
	venues := repository.NewVenueRepository(database.Queries)
	store := judgment.NewStore(judgment.StoreDeps{
		Pool: database.Pool, Queries: database.Queries, Assert: assertSvc,
		Contexts: threadctx.NewBuilder(repository.NewThreadContentRepository(database.Queries), venues),
		Cadence:  cadence, Jobs: jobs, Watchdog: config.TestConfig().Watchdog, ProductionMethod: judgmentProduction,
	})
	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	handlers.RegisterJudgmentRoutes(router.Group("/api/v1"), handlers.NewJudgmentHandler(store))
	return &judgmentEnv{
		ctx:      ctx,
		database: database,
		harness:  harness,
		store:    store,
		router:   router,
		comms:    repository.NewCommsMessageRepository(database.Queries),
		venues:   venues,
		base:     accelerated.GetCurrentTime().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Second),
	}
}

// thread seeds a contact and an email thread holding one inbound message
// from it at the base time.
func (e *judgmentEnv) thread(t *testing.T) (uuid.UUID, threadctx.VenueID) {
	t.Helper()
	c, err := e.harness.SeedContact(e.ctx, e.harness.Generator().Contact())
	require.NoError(t, err)
	container := "thread-" + uuid.NewString()
	tx, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	thread, err := e.venues.ResolveVenueForInteraction(e.ctx, tx, repository.InteractionSourceEmail, repository.VenueKindEmailThread, container, "")
	require.NoError(t, err)
	require.NoError(t, tx.Commit(e.ctx))
	body, peer := "synthetic message body", "peer@example.test"
	_, err = e.comms.UpsertMessage(e.ctx, repository.UpsertCommsMessageParams{
		Source: repository.InteractionSourceEmail, ExternalID: uuid.NewString(), ThreadID: &container, Body: &body,
		PeerHandle: &peer, PeerNormalized: &peer, Direction: repository.InteractionDirectionInbound, SentAt: e.base,
		SourceMetadata: []byte("{}"), MatchedContactID: c.ID,
	})
	require.NoError(t, err)
	return c.ID, thread
}

// judge records the production method's judgment of the thread in a new
// scheduled run.
func (e *judgmentEnv) judge(t *testing.T, thread threadctx.VenueID, outcomes judgment.Outcomes) judgment.RunID {
	t.Helper()
	run, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: judgment.RunKindScheduled, Method: judgmentMethod})
	require.NoError(t, err)
	_, err = e.store.RecordJudgment(e.ctx, run, judgment.Judgment{
		Method: judgmentMethod, Thread: thread, Kind: threadctx.KindConversation, CutOff: e.base,
		JudgedAt: e.base, InputHash: "h1", Outcomes: outcomes,
	})
	require.NoError(t, err)
	return run
}

func (e *judgmentEnv) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	e.router.ServeHTTP(w, req)
	return w
}

func decodeData[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var envelope struct {
		Success bool `json:"success"`
		Data    T    `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope), w.Body.String())
	require.True(t, envelope.Success, w.Body.String())
	return envelope.Data
}

func iOweReply(c uuid.UUID, confidence float64) judgment.Outcomes {
	return judgment.Outcomes{BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{
		{QuestionID: "ball_in_court:THEM", ContactID: c, Outcome: judgment.BallInCourtIOweReply, Confidence: confidence},
	}}
}

// GET /api/v1/judgment-runs and GET /api/v1/judgments serve the store's runs
// and judgments inside the API envelope; a judgments request names a thread,
// a run or both.
func TestJudgmentReadRoutes(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()

	t.Run("run_id naming a run that reused a judgment lists it under both runs at each one's cut-off", func(t *testing.T) {
		// spec: KNW-050.judgments-by-thread-or-run, KNW-050.runs-listed
		e := newJudgmentEnv(t)
		c, thread := e.thread(t)
		first := e.judge(t, thread, iOweReply(c, 0.9))
		second, err := e.store.StartRun(e.ctx, judgment.RunSpec{Kind: judgment.RunKindScheduled, Method: judgmentMethod})
		require.NoError(t, err)
		_, found, err := e.store.ReuseJudgment(e.ctx, second, thread, e.base.Add(time.Hour), "h1")
		require.NoError(t, err)
		require.True(t, found)
		_, overLimit := e.thread(t)
		require.NoError(t, e.store.RecordIncomplete(e.ctx, second, overLimit, e.base, judgment.IncompleteReasonOverLimit))

		w := e.do(t, http.MethodGet, "/api/v1/judgments?run_id="+second.String(), nil)

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		data := decodeData[handlers.JudgmentsResponse](t, w)
		require.Len(t, data.Judgments, 1)
		j := data.Judgments[0]
		assert.Equal(t, thread.String(), j.ThreadID)
		assert.Equal(t, judgmentProduction, j.MethodKey)
		assert.Equal(t, "v1", j.MethodVersion)
		assert.Equal(t, string(threadctx.KindConversation), j.ContextKind)
		assert.True(t, e.base.Equal(j.CutOff))
		require.Len(t, j.Runs, 2)
		assert.Equal(t, first.String(), j.Runs[0].RunID)
		assert.True(t, e.base.Equal(j.Runs[0].CutOff))
		assert.Equal(t, second.String(), j.Runs[1].RunID)
		assert.True(t, e.base.Add(time.Hour).Equal(j.Runs[1].CutOff))

		runs := decodeData[handlers.JudgmentRunsResponse](t, e.do(t, http.MethodGet, "/api/v1/judgment-runs", nil))
		var listed *handlers.JudgmentRunResponse
		for i := range runs.Runs {
			if runs.Runs[i].ID == second.String() {
				listed = &runs.Runs[i]
			}
		}
		require.NotNil(t, listed)
		assert.Equal(t, string(judgment.RunKindScheduled), listed.Kind)
		assert.Equal(t, judgmentProduction, listed.MethodKey)
		assert.Equal(t, 1, listed.JudgedCount)
		assert.Nil(t, listed.FinishedAt)
		require.Len(t, listed.Incomplete, 1)
		assert.Equal(t, overLimit.String(), listed.Incomplete[0].ThreadID)
		assert.True(t, e.base.Equal(listed.Incomplete[0].CutOff))
		assert.Equal(t, string(judgment.IncompleteReasonOverLimit), listed.Incomplete[0].Reason)
	})

	t.Run("thread_id and run_id combined narrow to the thread's judgments in the run", func(t *testing.T) {
		// spec: KNW-050.judgments-by-thread-or-run
		e := newJudgmentEnv(t)
		c1, thread1 := e.thread(t)
		run := e.judge(t, thread1, iOweReply(c1, 0.9))
		c2, thread2 := e.thread(t)
		e.judge(t, thread2, iOweReply(c2, 0.9))

		w := e.do(t, http.MethodGet, "/api/v1/judgments?thread_id="+thread2.String()+"&run_id="+run.String(), nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Empty(t, decodeData[handlers.JudgmentsResponse](t, w).Judgments)

		w = e.do(t, http.MethodGet, "/api/v1/judgments?thread_id="+thread1.String()+"&run_id="+run.String(), nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		judgments := decodeData[handlers.JudgmentsResponse](t, w).Judgments
		require.Len(t, judgments, 1)
		assert.Equal(t, thread1.String(), judgments[0].ThreadID)
	})

	t.Run("a request with neither parameter is refused", func(t *testing.T) {
		// spec: KNW-050.unbounded-or-malformed-refused
		e := newJudgmentEnv(t)
		w := e.do(t, http.MethodGet, "/api/v1/judgments", nil)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("a malformed thread_id is refused", func(t *testing.T) {
		// spec: KNW-050.unbounded-or-malformed-refused
		e := newJudgmentEnv(t)
		w := e.do(t, http.MethodGet, "/api/v1/judgments?thread_id=not-a-uuid", nil)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("an accepted big-moment flag outcome reads in state applied", func(t *testing.T) {
		// spec: KNW-050.outcome-state-and-review
		e := newJudgmentEnv(t)
		_, thread := e.thread(t)
		e.judge(t, thread, judgment.Outcomes{BigMoment: judgment.Scored[judgment.BigMoment]{
			QuestionID: "big_moment", Outcome: judgment.BigMomentParticipant, Confidence: 0.8,
		}})

		w := e.do(t, http.MethodGet, "/api/v1/judgments?thread_id="+thread.String(), nil)

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		judgments := decodeData[handlers.JudgmentsResponse](t, w).Judgments
		require.Len(t, judgments, 1)
		require.Len(t, judgments[0].Outcomes, 1)
		o := judgments[0].Outcomes[0]
		assert.Equal(t, string(judgment.QuestionBigMoment), o.Question)
		assert.Equal(t, string(judgment.BigMomentParticipant), o.Outcome)
		assert.Equal(t, string(judgment.OutcomeStateApplied), o.State)
		assert.Nil(t, o.ContactID)
		assert.Nil(t, o.Review)
	})
}

// POST /api/v1/judgment-outcomes/:id/review records the right outcome the
// user states and returns the outcome with the verdict the server derived.
func TestJudgmentReviewRoute(t *testing.T) {
	testsupport.RequireDatabase(t)
	t.Parallel()

	outcomeOf := func(t *testing.T, e *judgmentEnv, thread threadctx.VenueID) handlers.JudgmentOutcomeResponse {
		t.Helper()
		judgments := decodeData[handlers.JudgmentsResponse](t, e.do(t, http.MethodGet, "/api/v1/judgments?thread_id="+thread.String(), nil)).Judgments
		require.Len(t, judgments, 1)
		require.Len(t, judgments[0].Outcomes, 1)
		return judgments[0].Outcomes[0]
	}
	claimOn := func(t *testing.T, e *judgmentEnv) (threadctx.VenueID, handlers.JudgmentOutcomeResponse) {
		t.Helper()
		c, thread := e.thread(t)
		e.judge(t, thread, iOweReply(c, 0.3))
		o := outcomeOf(t, e, thread)
		require.Equal(t, string(judgment.OutcomeStateClaim), o.State)
		return thread, o
	}
	claim := func(t *testing.T, e *judgmentEnv) handlers.JudgmentOutcomeResponse {
		t.Helper()
		_, o := claimOn(t, e)
		return o
	}
	review := func(id string) string { return "/api/v1/judgment-outcomes/" + id + "/review" }

	t.Run("confirming a claim returns verdict confirmed", func(t *testing.T) {
		// spec: KNW-049.review-returns-derived-verdict
		e := newJudgmentEnv(t)
		o := claim(t, e)

		w := e.do(t, http.MethodPost, review(o.ID), map[string]string{"right_outcome": string(judgment.BallInCourtIOweReply)})

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		got := decodeData[handlers.JudgmentOutcomeResponse](t, w)
		assert.Equal(t, o.ID, got.ID)
		require.NotNil(t, got.Review)
		assert.Equal(t, string(judgment.VerdictConfirmed), got.Review.Verdict)
		assert.Equal(t, string(judgment.BallInCourtIOweReply), got.Review.RightOutcome)
	})

	t.Run("a body without right_outcome is refused", func(t *testing.T) {
		// spec: KNW-049.invalid-right-outcome-refused
		e := newJudgmentEnv(t)
		o := claim(t, e)
		w := e.do(t, http.MethodPost, review(o.ID), map[string]string{"verdict": string(judgment.VerdictConfirmed)})
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("right_outcome open on a ball_in_court outcome is refused", func(t *testing.T) {
		// spec: KNW-049.invalid-right-outcome-refused
		e := newJudgmentEnv(t)
		thread, o := claimOn(t, e)
		w := e.do(t, http.MethodPost, review(o.ID), map[string]string{"right_outcome": string(judgment.CommitmentStateOpen)})
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Nil(t, outcomeOf(t, e, thread).Review, "nothing is recorded")
	})

	t.Run("an unknown outcome ID is not found", func(t *testing.T) {
		// spec: KNW-049.unknown-outcome-not-found
		e := newJudgmentEnv(t)
		w := e.do(t, http.MethodPost, review(uuid.NewString()), map[string]string{"right_outcome": string(judgment.BallInCourtIOweReply)})
		assert.Equal(t, http.StatusNotFound, w.Code)
	})

	t.Run("a verdict the client sends is ignored and the server derives rejected", func(t *testing.T) {
		// spec: KNW-049.client-verdict-ignored
		e := newJudgmentEnv(t)
		o := claim(t, e)

		w := e.do(t, http.MethodPost, review(o.ID), map[string]string{
			"verdict": string(judgment.VerdictConfirmed), "right_outcome": string(judgment.BallInCourtNobodyOwes),
		})

		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		got := decodeData[handlers.JudgmentOutcomeResponse](t, w)
		require.NotNil(t, got.Review)
		assert.Equal(t, string(judgment.VerdictRejected), got.Review.Verdict)
		assert.Equal(t, string(judgment.BallInCourtNobodyOwes), got.Review.RightOutcome)
	})
}
