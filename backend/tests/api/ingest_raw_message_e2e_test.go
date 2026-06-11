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
	"personal-crm/backend/internal/auth"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/messages"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// TestIngestRawMessage_E2E_StagesAggregatesAndCreatesInteraction is
// the end-to-end test that locks the PR's contract:
//
//	POST /api/v1/ingest/events (raw_message.received, known contact)
//	  → staging row inserted with matched_contact_id
//	  → MessagingAggregateForContactArgs job enqueued
//	  → worker runs engine.AggregateForContact per chat
//	  → engine claims rows + publishes message.received envelope
//	  → InteractionRecorder consumer creates the interaction row +
//	    marks staging.processed_at + staging.interaction_id
//
// Wires the FULL stack (live bus, real worker, real
// InteractionRecorder) and waits for the staging row to be marked
// processed. This is the load-bearing assertion for the feature —
// without it we could ship a path that stages rows but never turns
// them into interactions.
// rawMessageE2EEnv bundles the fully-wired e2e stack (live bus, real
// aggregator + InteractionRecorder, started River client) plus a paired
// host and a seeded known contact.
type rawMessageE2EEnv struct {
	ctx             context.Context
	database        *db.Database
	router          *gin.Engine
	messagesRepo    *repository.MessagesMessageRepository
	interactionRepo *repository.InteractionRepository
	contactRepo     *repository.ContactRepository
	pair            *service.PairResult
	contactID       uuid.UUID
}

// setupRawMessageE2E wires the full ingest→aggregate→record pipeline on
// an isolated per-test River DB clone and returns the env. Cleanup
// (River stop + FK-ordered hard deletes) is registered via t.Cleanup.
func setupRawMessageE2E(t *testing.T) *rawMessageE2EEnv {
	t.Helper()
	ctx := context.Background()
	// This func genuinely works jobs (real aggregator + recorder, polls for
	// the worked result) AND asserts DB-wide
	// (CountInteractionsByIDContactAndSource) + pairs the singleton mac_host,
	// so it runs on an isolated per-test clone. The clone helper sets
	// WorkerConcurrency=2 (>=1), so the old `<= 0` guard is moot.
	database, cfg := newIsolatedRiverTestDB(t, ctx)
	cfg.External.APIKey = macHostTestKey
	cfg.Features.EnableEventBusIngest = true

	// Repos
	hostRepo := repository.NewMacHostRepository(database.Queries)
	pairingRepo := repository.NewMacHostPairingTokenRepository(database.Queries)
	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	identityRepo := repository.NewIdentityRepository(database.Queries)
	identityService := service.NewIdentityService(identityRepo)
	messagesRepo := repository.NewMessagesMessageRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)
	eventRepo := repository.NewEventRepository(database.Queries)
	rematchSvc := service.NewRematchService()
	contactSvc := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo, nil, rematchSvc)
	cadenceUpdater := consumer.NewCadenceUpdater(claimRepo, contactRepo, database.Queries, consumer.CadenceModeCutover, false)
	contactSvc.SetCadenceUpdater(cadenceUpdater)
	macService := service.NewMacHostService(hostRepo, pairingRepo, syncRepo, nil, nil, nil, database.Pool, 4)

	// River client — wires the real MessagingAggregateForContactWorker
	// (so the e2e test runs through the aggregator), the
	// InteractionRecorderWorker (Stage 3 — creates the interaction
	// row and marks staging processed), and noop workers for the
	// downstream kinds the recorder enqueues (cadence_updater,
	// followup_manager).
	workers := river.NewWorkers()
	// Aggregator worker — registered against the messages engine
	// (constructed below after the bus exists).
	aggregateShim := &deferredAggregateForContactWorker{}
	river.AddWorker(workers, aggregateShim)
	// InteractionRecorder — wired via shim because it needs bus + recorder.
	recorderShim := &deferredInteractionRecorderWorker{}
	river.AddWorker(workers, recorderShim)
	// Stage-3 downstream kinds the recorder enqueues. Noop drains.
	river.AddWorker(workers, &noopCadenceUpdaterWorker{})
	river.AddWorker(workers, &noopFollowUpManagerWorker{})

	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		JobTimeout: cfg.River.JobTimeout,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)

	bus := events.NewBus(database.Pool, riverClient, eventRepo)

	// Messages engine — real, wired against bus + pool so create-path
	// commits claims and publishes events.
	const burstWindowHours = 4
	const replyBridgeHours = 48
	messagesEngine := messages.NewAggregationEngine(
		burstWindowHours,
		replyBridgeHours,
		messagesRepo,
		interactionRepo,
		contactSvc,
		contactSvc,
		bus,
		database.Pool,
		consumer.NewRiverInteractionRecorderEnqueuer(riverClient),
	)

	// Fill the aggregate worker shim now that the engine exists.
	aggregateShim.engine = messagesEngine
	aggregateShim.lister = messagesRepo.ListUnprocessedChatsByContact

	// Build the InteractionRecorder + Stage 3 worker.
	stagingRegistry := repository.NewStagingProcessorRegistry(map[string]repository.StagingProcessor{
		repository.InteractionSourceMessages: repository.NewMessagesStagingProcessor(messagesRepo),
	})
	recorder := consumer.NewInteractionRecorder(contactSvc, stagingRegistry, bus, cadenceUpdater, nil, repository.NewCalendarEventRepository(database.Queries))
	recorderShim.real = consumer.NewInteractionRecorderWorker(bus, database.Pool, recorder, nil)

	// Now wire the ingest service + handler.
	ingestService := service.NewIngestService(database, bus, identityService, messagesRepo, riverClient, nil, hostRepo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	ingestHandler := handlers.NewIngestHandler(ingestService)

	// gin mode is set once for the package in gin_test.go's init(); calling
	// gin.SetMode here would race the global with parallel route registration.
	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	router.Use(api.LoggingMiddleware())
	router.Use(api.CORSMiddleware(cfg.CORS))

	limiter := auth.NewPairingIPRateLimiter()
	macHandler := handlers.NewMacHostHandler(macService, limiter)
	handlers.RegisterMacHostRoutes(router, handlers.MacHostRouteDeps{
		HostRepo:    hostRepo,
		Handler:     macHandler,
		AuthLimiter: auth.DefaultMacHostAuthLimiterConfig(),
	})

	ingestAuth := auth.IngestAuthMiddleware(
		auth.APIKeyMiddleware(cfg),
		auth.MacHostAuthMiddleware(hostRepo, auth.DefaultPasswordComparator, auth.DefaultMacHostAuthLimiterConfig()),
	)
	ingestGroup := router.Group("/api/v1/ingest")
	ingestGroup.Use(ingestAuth)
	ingestGroup.POST("/events", ingestHandler.IngestEvents)

	// Start the River client — workers begin executing jobs.
	// IMPORTANT: pass the OUTER ctx (not a timeout-derived one).
	require.NoError(t, riverClient.Start(ctx))

	// Pair a host + seed a contact.
	plain, _, err := macService.CreatePairingToken(ctx)
	require.NoError(t, err)
	pair, err := macService.PairWithToken(ctx, plain, "ingest-e2e", "0.1.0", 1)
	require.NoError(t, err)
	created, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "E2E Test Contact",
	})
	require.NoError(t, err)
	contactID := created.ID
	_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contactID,
		Type:      "phone",
		Value:     "+15551234567",
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = riverClient.Stop(stopCtx)

		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanCancel()
		// Hard-delete in FK order: interactions reference the contact;
		// contact_methods reference the contact; mac_host references
		// nothing but the staging rows reference it.
		_ = messagesRepo.HardDeleteByMacHost(cleanCtx, pair.HostID)
		_, _ = database.Queries.DeleteExternalIdentitiesBySource(cleanCtx, "messages")
		_, _ = database.Queries.DeleteInteractionsByContactAndSource(cleanCtx, db.DeleteInteractionsByContactAndSourceParams{
			ContactID: pgUUID(contactID),
			Source:    "messages",
		})
		_ = contactMethodRepo.DeleteContactMethodsByContact(cleanCtx, contactID)
		_ = contactRepo.HardDeleteContact(cleanCtx, contactID)
		states, _ := database.Queries.ListSyncStates(cleanCtx)
		for _, s := range states {
			if s.Strategy == "push" {
				_ = database.Queries.DeleteSyncState(cleanCtx, s.ID)
			}
		}
		_, _ = database.Queries.DeleteAllMacHosts(cleanCtx)
		_, _ = database.Queries.DeleteAllPairingTokens(cleanCtx)
		_, _ = database.Queries.DeleteRiverJobsByKindAny(cleanCtx, []string{
			"messaging_aggregate_for_contact",
			"messaging_aggregate_sweeper",
			"interaction_recorder",
			"cadence_updater",
			"followup_manager",
		})
		_, _ = database.Queries.DeleteEventsBySource(cleanCtx, "messages")
		// database.Close() is owned by the clone helper's t.Cleanup. The
		// riverClient.Stop above runs first (registered later, LIFO) so the
		// Elector resigns against a still-live pool.
	})

	return &rawMessageE2EEnv{
		ctx:             ctx,
		database:        database,
		router:          router,
		messagesRepo:    messagesRepo,
		interactionRepo: interactionRepo,
		contactRepo:     contactRepo,
		pair:            pair,
		contactID:       contactID,
	}
}

// postRawMessageE2E POSTs one raw_message event of `kind` to the wired
// router via the host-auth path. The sent kind is marshaled via the
// exact RawMessageSentPayload type events.Marshal requires.
func postRawMessageE2E(t *testing.T, env *rawMessageE2EEnv, kind events.Kind, guid string) {
	t.Helper()
	now := accelerated.GetCurrentTime()
	p := events.RawMessageReceivedPayload{
		Version:     1,
		HostID:      env.pair.HostID,
		Source:      "messages",
		Guid:        guid,
		ChatID:      "e2e-chat-1",
		PeerHandle:  "+15551234567",
		MessageType: "text",
		SentAt:      now,
	}
	var pBytes json.RawMessage
	var err error
	if kind == events.KindRawMessageSent {
		pBytes, err = events.Marshal(kind, events.RawMessageSentPayload(p))
	} else {
		pBytes, err = events.Marshal(kind, p)
	}
	require.NoError(t, err)
	ev := map[string]any{
		"source":      "messages",
		"source_id":   guid,
		"kind":        string(kind),
		"payload":     json.RawMessage(pBytes),
		"observed_at": now,
	}
	body, err := json.Marshal(map[string]any{"events": []any{ev}})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mac-Host-ID", env.pair.HostID.String())
	req.Header.Set("Authorization", "Bearer "+env.pair.APIKey)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
}

// pollProcessedRawMessage waits (bounded, count-based) for the staging
// row to be marked processed by Stage 3.
func pollProcessedRawMessage(t *testing.T, env *rawMessageE2EEnv, guid string) *repository.MessagesMessage {
	t.Helper()
	var msg *repository.MessagesMessage
	for range 200 {
		m, err := env.messagesRepo.GetMessage(context.Background(), guid)
		require.NoError(t, err)
		if m.ProcessedAt != nil && m.InteractionID != nil {
			msg = m
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.NotNil(t, msg, "staging row was never marked processed within 10s")
	require.NotNil(t, msg.ProcessedAt, "staging.processed_at must be set after Stage 3")
	require.NotNil(t, msg.InteractionID, "staging.interaction_id must be set after Stage 3")
	return msg
}

func TestIngestRawMessage_E2E_StagesAggregatesAndCreatesInteraction(t *testing.T) {
	t.Parallel()
	env := setupRawMessageE2E(t)

	guid := "test-e2e-guid-" + uuid.NewString()
	postRawMessageE2E(t, env, events.KindRawMessageReceived, guid)

	msg := pollProcessedRawMessage(t, env, guid)
	require.Equal(t, env.contactID, *msg.MatchedContactID)

	// And the interaction row exists with source=messages.
	interactionCount, err := env.database.Queries.CountInteractionsByIDContactAndSource(
		context.Background(),
		db.CountInteractionsByIDContactAndSourceParams{
			ID:        pgUUID(*msg.InteractionID),
			ContactID: pgUUID(env.contactID),
			Source:    "messages",
		},
	)
	require.NoError(t, err)
	require.Equal(t, int64(1), interactionCount, "exactly one messages-source interaction expected")
}

// TestIngestRawMessage_E2E_Sent_OutboundInteractionBumpsOutreach is the
// outbound mirror: POST raw_message.sent for a known contact → staging
// row aggregates → interaction created with direction=outbound,
// source=messages → contact.last_outreach_at bumped AND
// last_contacted unchanged (outbound-only outreach semantics).
func TestIngestRawMessage_E2E_Sent_OutboundInteractionBumpsOutreach(t *testing.T) {
	t.Parallel()
	env := setupRawMessageE2E(t)

	// Baseline: a fresh contact has no last_contacted / last_outreach_at.
	before, err := env.contactRepo.GetContact(env.ctx, env.contactID)
	require.NoError(t, err)
	require.Nil(t, before.LastContacted, "fresh contact has no last_contacted")
	require.Nil(t, before.LastOutreachAt, "fresh contact has no last_outreach_at")

	guid := "test-e2e-sent-" + uuid.NewString()
	postRawMessageE2E(t, env, events.KindRawMessageSent, guid)

	msg := pollProcessedRawMessage(t, env, guid)
	require.True(t, msg.IsOutgoing, "sent kind stages is_outgoing=true")
	require.Equal(t, env.contactID, *msg.MatchedContactID)

	// The interaction is outbound, source=messages.
	interaction, err := env.interactionRepo.GetInteraction(env.ctx, *msg.InteractionID)
	require.NoError(t, err)
	require.Equal(t, "outbound", interaction.Direction,
		"outbound message produces an outbound interaction")
	require.Equal(t, "messages", interaction.Source)

	// Outbound bumps last_outreach_at only; last_contacted stays nil.
	after, err := env.contactRepo.GetContact(env.ctx, env.contactID)
	require.NoError(t, err)
	require.NotNil(t, after.LastOutreachAt, "outbound message bumps last_outreach_at")
	require.Nil(t, after.LastContacted,
		"outbound-only message must NOT bump last_contacted")
}

// deferredAggregateForContactWorker is filled after the engine and
// lister are constructed. Implements river.Worker for
// MessagingAggregateForContactArgs.
type deferredAggregateForContactWorker struct {
	river.WorkerDefaults[consumerjobs.MessagingAggregateForContactArgs]
	engine *aggregationEngine
	lister func(ctx context.Context, contactID uuid.UUID) ([]string, error)
}

func (w *deferredAggregateForContactWorker) Work(ctx context.Context, job *river.Job[consumerjobs.MessagingAggregateForContactArgs]) error {
	if w.engine == nil || w.lister == nil {
		return nil
	}
	inner := scheduler.NewMessagingAggregateForContactWorker(
		map[string]scheduler.ChatAwareAggregator{"messages": w.engine},
		scheduler.NewPerSourceChatListerRegistry(map[string]func(ctx context.Context, contactID uuid.UUID) ([]string, error){
			"messages": w.lister,
		}),
	)
	return inner.Work(ctx, job)
}

// deferredInteractionRecorderWorker proxies to the real consumer's
// worker once it's filled in. Mirrors the test_event_bus_harness
// pattern.
type deferredInteractionRecorderWorker struct {
	river.WorkerDefaults[consumerjobs.InteractionRecorderJobArgs]
	real *consumer.InteractionRecorderWorker
}

func (w *deferredInteractionRecorderWorker) Work(ctx context.Context, job *river.Job[consumerjobs.InteractionRecorderJobArgs]) error {
	if w.real == nil {
		return nil
	}
	return w.real.Work(ctx, job)
}

// Noop workers for kinds the recorder enqueues but this test doesn't
// exercise.
type noopCadenceUpdaterWorker struct {
	river.WorkerDefaults[consumerjobs.CadenceUpdaterJobArgs]
}

func (w *noopCadenceUpdaterWorker) Work(_ context.Context, _ *river.Job[consumerjobs.CadenceUpdaterJobArgs]) error {
	return nil
}

type noopFollowUpManagerWorker struct {
	river.WorkerDefaults[consumerjobs.FollowUpManagerJobArgs]
}

func (w *noopFollowUpManagerWorker) Work(_ context.Context, _ *river.Job[consumerjobs.FollowUpManagerJobArgs]) error {
	return nil
}

// pgUUID is a thin pgtype.UUID constructor for sqlc-generated query
// params that take pgtype.UUID. Kept local to this file rather than
// importing the (private) repository helper.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}
