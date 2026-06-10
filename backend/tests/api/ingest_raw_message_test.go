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
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/messages"
	"personal-crm/backend/internal/messaging/aggregation"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// ingestRawTestEnv bundles the wired stack for raw_message ingest
// integration tests. The router is built with the production composite
// IngestAuthMiddleware so the auth dispatch is exercised end-to-end.
type ingestRawTestEnv struct {
	router          *gin.Engine
	apiKey          string
	database        *db.Database
	macService      *service.MacHostService
	messagesRepo    *repository.MessagesMessageRepository
	identityRepo    *repository.IdentityRepository
	identityService *service.IdentityService
	contactRepo     *repository.ContactRepository
	cmRepo          *repository.ContactMethodRepository
	riverClient     *river.Client[pgx.Tx]
	pairedHostID    uuid.UUID
	pairedHostKey   string
	pairedContactA  uuid.UUID
}

func setupRawIngestEnv(t *testing.T) *ingestRawTestEnv {
	t.Helper()

	ctx := context.Background()
	// This file asserts DB-wide over river_job (CountRiverJobsByKind) AND
	// pairs the singleton mac_host with a DB-wide DeleteAllMacHosts teardown
	// — both collide on the shared package DB, so it runs on a per-test clone.
	database, cfg := newIsolatedRiverTestDB(t, ctx)
	cfg.External.APIKey = macHostTestKey
	cfg.Features.EnableEventBusIngest = true

	hostRepo := repository.NewMacHostRepository(database.Queries)
	pairingRepo := repository.NewMacHostPairingTokenRepository(database.Queries)
	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	macService := service.NewMacHostService(hostRepo, pairingRepo, syncRepo, nil, nil, nil, database.Pool, 4)

	identityRepo := repository.NewIdentityRepository(database.Queries)
	identityService := service.NewIdentityService(identityRepo)
	messagesRepo := repository.NewMessagesMessageRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	cmRepo := repository.NewContactMethodRepository(database.Queries)

	// Build a River client — the ingest path enqueues
	// MessagingAggregateForContactArgs via InsertTx and the test asserts
	// on river_job row counts directly rather than running workers.
	// Queues must be declared (River errors on a nil/empty Queues map
	// at insert time even if no workers are registered).
	workers := river.NewWorkers()
	// Every kind the test path enqueues must have a registered worker.
	// We register noop workers for both MessagingAggregateForContact
	// (enqueued by the ingest service) and InteractionRecorderJobArgs
	// (enqueued by Bus.PublishTx when the engine publishes a
	// message.received envelope from the e2e test). Workers are noops
	// because the tests assert on staging-row + event-log state, not
	// on Stage 3 execution.
	river.AddWorker(workers, &noopAggregateForContactWorker{})
	river.AddWorker(workers, &noopInteractionRecorderWorker{})
	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		JobTimeout: cfg.River.JobTimeout,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 1},
		},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	// The client is wired into the Bus/IngestService for InsertTx only;
	// these tests enqueue and count river_job rows synchronously and never
	// WORK jobs (workers are no-ops). InsertTx needs only PoolIsSet(), not a
	// running client, so the client is deliberately never Started — avoiding
	// the leadership-Elector teardown cost a per-test clone would multiply.

	eventRepo := repository.NewEventRepository(database.Queries)
	eventBus := events.NewBus(database.Pool, riverClient, eventRepo)
	ingestService := service.NewIngestService(
		database,
		eventBus,
		identityService,
		messagesRepo,
		riverClient,
		nil,
		hostRepo,
		nil, // meetingNotes unused
		nil, // calendar unused
		nil, // interactions unused
		nil, // identityLookup unused
		nil, // contactSvc unused
		nil, // phoneCalls unused
		nil, // contactRecorder unused
		nil, // cadence unused
		nil, // followUp unused
		nil, // titleMatcher unused
		nil, // discovery unused
		nil, // phoneCallLinkage unused
	)
	ingestHandler := handlers.NewIngestHandler(ingestService)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	router.Use(api.LoggingMiddleware())
	router.Use(api.CORSMiddleware(cfg.CORS))

	// Mac host routes (pairing + heartbeat etc).
	limiter := auth.NewPairingIPRateLimiter()
	macHandler := handlers.NewMacHostHandler(macService, limiter)
	handlers.RegisterMacHostRoutes(router, handlers.MacHostRouteDeps{
		HostRepo:    hostRepo,
		Handler:     macHandler,
		AuthLimiter: auth.DefaultMacHostAuthLimiterConfig(),
	})

	// Ingest endpoint behind composite middleware — same wiring as
	// production main.go.
	ingestAuth := auth.IngestAuthMiddleware(
		auth.APIKeyMiddleware(cfg),
		auth.MacHostAuthMiddleware(hostRepo, auth.DefaultPasswordComparator, auth.DefaultMacHostAuthLimiterConfig()),
	)
	ingestGroup := router.Group("/api/v1/ingest")
	ingestGroup.Use(ingestAuth)
	ingestGroup.POST("/events", ingestHandler.IngestEvents)

	// Pair a host so tests can authenticate against the daemon path.
	plain, _, err := macService.CreatePairingToken(ctx)
	require.NoError(t, err)
	pair, err := macService.PairWithToken(ctx, plain, "ingest-test", "0.1.0", 1)
	require.NoError(t, err)

	// Seed a contact + phone contact_method that matches our test peer.
	created, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Ingest Test Contact",
	})
	require.NoError(t, err)
	contactID := created.ID
	_, err = cmRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contactID,
		Type:      "phone",
		Value:     "+15551234567",
	})
	require.NoError(t, err)

	env := &ingestRawTestEnv{
		router:          router,
		apiKey:          cfg.External.APIKey,
		database:        database,
		macService:      macService,
		messagesRepo:    messagesRepo,
		identityRepo:    identityRepo,
		identityService: identityService,
		contactRepo:     contactRepo,
		cmRepo:          cmRepo,
		riverClient:     riverClient,
		pairedHostID:    pair.HostID,
		pairedHostKey:   pair.APIKey,
		pairedContactA:  contactID,
	}

	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = messagesRepo.HardDeleteByMacHost(cleanCtx, env.pairedHostID)
		// Identity rows for the messages source (best-effort wipe
		// across the whole table — tests on a shared DB).
		_, _ = database.Queries.DeleteExternalIdentitiesBySource(cleanCtx, "messages")
		_ = cmRepo.DeleteContactMethodsByContact(cleanCtx, env.pairedContactA)
		_ = contactRepo.HardDeleteContact(cleanCtx, env.pairedContactA)
		states, _ := database.Queries.ListSyncStates(cleanCtx)
		for _, s := range states {
			if s.Strategy == "push" {
				_ = database.Queries.DeleteSyncState(cleanCtx, s.ID)
			}
		}
		_, _ = database.Queries.DeleteAllMacHosts(cleanCtx)
		_, _ = database.Queries.DeleteAllPairingTokens(cleanCtx)
		// Drop river jobs we enqueued.
		_, _ = database.Queries.DeleteRiverJobsByKindAny(cleanCtx, []string{
			"messaging_aggregate_for_contact",
			"messaging_aggregate_sweeper",
		})
		_, _ = database.Queries.DeleteEventsBySource(cleanCtx, "messages")
		// database.Close() is owned by the clone helper's t.Cleanup.
	})

	return env
}

// postIngestRaw builds + sends an HTTP POST to /api/v1/ingest/events.
// hostID == nil takes the global-API-key path; non-nil takes the
// host-auth path with the supplied bearer key.
func postIngestRaw(t *testing.T, env *ingestRawTestEnv, hostID *uuid.UUID, hostKey string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/events", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if hostID != nil {
		req.Header.Set("X-Mac-Host-ID", hostID.String())
		req.Header.Set("Authorization", "Bearer "+hostKey)
	} else {
		req.Header.Set("X-API-Key", env.apiKey)
	}
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	return w
}

// buildRawMessageEvent constructs an ingest-event request map for a raw_message kind.
func buildRawMessageEvent(t *testing.T, kind events.Kind, hostID uuid.UUID, guid, chatID, peerHandle string) map[string]any {
	t.Helper()
	now := accelerated.GetCurrentTime()
	p := events.RawMessageReceivedPayload{
		Version:     1,
		HostID:      hostID,
		Source:      "messages",
		Guid:        guid,
		ChatID:      chatID,
		PeerHandle:  peerHandle,
		MessageType: "text",
		IsGroup:     false,
		SentAt:      now,
	}
	pBytes, err := events.Marshal(kind, p)
	require.NoError(t, err)
	return map[string]any{
		"source":      "messages",
		"source_id":   guid,
		"kind":        string(kind),
		"payload":     json.RawMessage(pBytes),
		"observed_at": now,
	}
}

func parseIngestResp(t *testing.T, w *httptest.ResponseRecorder) handlers.IngestResponse {
	t.Helper()
	var resp handlers.IngestResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "body: %s", w.Body.String())
	return resp
}

// countRiverJobs returns the total number of River jobs of the given
// kind (including finalized rows). Timing-resilient: River workers may
// pick up jobs between insert and assertion under TestOnly mode, so
// counting only `finalized_at IS NULL` is racy. Cross-test pollution
// is bounded by DeleteRiverJobsByKindAny in t.Cleanup.
func countRiverJobs(t *testing.T, env *ingestRawTestEnv, kind string) int {
	t.Helper()
	count, err := env.database.Queries.CountRiverJobsByKind(context.Background(), kind)
	require.NoError(t, err)
	return int(count)
}

// aggregationEngine is the type alias the e2e test uses to keep the
// helper return type in the api package without importing the
// messaging/aggregation package directly.
type aggregationEngine = aggregation.Engine

// noopAggregateForContactWorker is a stub worker that River accepts so
// MessagingAggregateForContactArgs inserts succeed without actually
// running aggregation. The test asserts on river_job row counts, not
// on aggregation side-effects.
type noopAggregateForContactWorker struct {
	river.WorkerDefaults[consumerjobs.MessagingAggregateForContactArgs]
}

func (w *noopAggregateForContactWorker) Work(_ context.Context, _ *river.Job[consumerjobs.MessagingAggregateForContactArgs]) error {
	return nil
}

// noopInteractionRecorderWorker is registered so the e2e test's bus
// can enqueue InteractionRecorderJobArgs after the engine publishes a
// message.received envelope. The body is a noop because the e2e test
// asserts on staging-row claim columns + event-log presence, not on
// Stage 3 (interaction row creation).
type noopInteractionRecorderWorker struct {
	river.WorkerDefaults[consumerjobs.InteractionRecorderJobArgs]
}

func (w *noopInteractionRecorderWorker) Work(_ context.Context, _ *river.Job[consumerjobs.InteractionRecorderJobArgs]) error {
	return nil
}

// adminRiverInserter adapts the river.Client to the
// messages.AdminRiverInserter interface for the integration test
// admin-rematch coverage.
type adminRiverInserter struct {
	client *river.Client[pgx.Tx]
}

func (a adminRiverInserter) Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	return a.client.Insert(ctx, args, opts)
}

// TestIngestRawMessage_HappyPath_StagesRowAndEnqueuesJob asserts a
// well-formed raw_message.received with a known contact's phone number
// produces a staging row with matched_contact_id and one River job.
func TestIngestRawMessage_HappyPath_StagesRowAndEnqueuesJob(t *testing.T) {
	env := setupRawIngestEnv(t)
	t.Parallel()
	guid := "test-guid-" + uuid.NewString()
	ev := buildRawMessageEvent(t, events.KindRawMessageReceived, env.pairedHostID,
		guid, "chat-1", "+15551234567")
	w := postIngestRaw(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Equal(t, 0, resp.Duplicate)
	require.Equal(t, 0, resp.Rejected, "errors: %+v", resp.Errors)

	msg, err := env.messagesRepo.GetMessage(context.Background(), guid)
	require.NoError(t, err)
	require.NotNil(t, msg)
	require.NotNil(t, msg.MatchedContactID, "expected matched_contact_id to be set")
	require.Equal(t, env.pairedContactA, *msg.MatchedContactID)
	require.NotNil(t, msg.MacHostID)
	require.Equal(t, env.pairedHostID, *msg.MacHostID)

	require.Equal(t, 1, countRiverJobs(t, env, "messaging_aggregate_for_contact"))
}

// TestIngestRawMessage_Duplicate_DetectedAndSkipped asserts re-POSTing
// the same guid returns duplicate=1 and produces no new staging row.
func TestIngestRawMessage_Duplicate_DetectedAndSkipped(t *testing.T) {
	env := setupRawIngestEnv(t)
	t.Parallel()
	guid := "test-guid-" + uuid.NewString()
	ev := buildRawMessageEvent(t, events.KindRawMessageReceived, env.pairedHostID,
		guid, "chat-1", "+15551234567")

	w := postIngestRaw(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, parseIngestResp(t, w).Accepted)

	w = postIngestRaw(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code)
	resp := parseIngestResp(t, w)
	require.Equal(t, 0, resp.Accepted)
	require.Equal(t, 1, resp.Duplicate)
	require.Equal(t, 0, resp.Rejected)

	count, err := env.database.Queries.CountMessagesMessageByGuid(context.Background(), guid)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

// TestIngestRawMessage_UnmatchedPeer_StagedWithoutContactNoJob asserts
// that an unmatched peer is staged with matched_contact_id=NULL and
// produces NO aggregator River job for this row.
func TestIngestRawMessage_UnmatchedPeer_StagedWithoutContactNoJob(t *testing.T) {
	env := setupRawIngestEnv(t)
	t.Parallel()
	guid := "test-guid-" + uuid.NewString()
	ev := buildRawMessageEvent(t, events.KindRawMessageReceived, env.pairedHostID,
		guid, "chat-x", "+15559999999") // not in contact_method
	w := postIngestRaw(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, parseIngestResp(t, w).Accepted)

	msg, err := env.messagesRepo.GetMessage(context.Background(), guid)
	require.NoError(t, err)
	require.Nil(t, msg.MatchedContactID, "unmatched peer must produce a NULL matched_contact_id")

	require.Equal(t, 0, countRiverJobs(t, env, "messaging_aggregate_for_contact"))
}

// TestIngestRawMessage_GlobalKeyPath_RejectedWithCode asserts that
// raw_message.* events submitted via the global API key (no
// X-Mac-Host-ID) are REJECTED per-event with HOST_ONLY_REQUIRES_HOST_AUTH.
func TestIngestRawMessage_GlobalKeyPath_RejectedWithCode(t *testing.T) {
	env := setupRawIngestEnv(t)
	t.Parallel()
	guid := "test-guid-" + uuid.NewString()
	ev := buildRawMessageEvent(t, events.KindRawMessageReceived, uuid.New(),
		guid, "chat-1", "+15551234567")
	w := postIngestRaw(t, env, nil, "", map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 0, resp.Accepted)
	require.Equal(t, 1, resp.Rejected)
	require.Len(t, resp.Errors, 1)
	require.Equal(t, "HOST_ONLY_REQUIRES_HOST_AUTH", resp.Errors[0].Code)
}

// TestIngestRawMessage_HostAuthForeignKind_Rejected asserts a non-
// raw_message kind submitted via the host-auth path is REJECTED with
// UNSUPPORTED_HOST_AUTH_KIND.
func TestIngestRawMessage_HostAuthForeignKind_Rejected(t *testing.T) {
	env := setupRawIngestEnv(t)
	t.Parallel()
	now := accelerated.GetCurrentTime()
	payload, err := events.Marshal(events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  env.pairedContactA,
		EventID:    "evt-1",
		OccurredAt: now,
	})
	require.NoError(t, err)
	ev := map[string]any{
		"source":      "calendar",
		"source_id":   "evt-1",
		"kind":        string(events.KindCalendarAttended),
		"payload":     json.RawMessage(payload),
		"observed_at": now,
	}
	w := postIngestRaw(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 0, resp.Accepted)
	require.Equal(t, 1, resp.Rejected)
	require.Len(t, resp.Errors, 1)
	require.Equal(t, "UNSUPPORTED_HOST_AUTH_KIND", resp.Errors[0].Code)
}

// TestIngestRawMessage_PayloadHostMismatch_Rejected asserts an
// envelope whose payload.host_id disagrees with the authenticated host
// is REJECTED with PAYLOAD_INVARIANT.
func TestIngestRawMessage_PayloadHostMismatch_Rejected(t *testing.T) {
	env := setupRawIngestEnv(t)
	t.Parallel()
	guid := "test-guid-" + uuid.NewString()
	otherHost := uuid.New()
	ev := buildRawMessageEvent(t, events.KindRawMessageReceived, otherHost,
		guid, "chat-1", "+15551234567")
	w := postIngestRaw(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 0, resp.Accepted)
	require.Equal(t, 1, resp.Rejected)
	require.Len(t, resp.Errors, 1)
	require.Equal(t, "PAYLOAD_INVARIANT", resp.Errors[0].Code)
	require.Contains(t, resp.Errors[0].Message, "host_id")
}

// TestIngestRawMessage_MissingSourceID_Rejected asserts a raw_message
// event with empty source_id is REJECTED at the handler with
// MISSING_FIELD.
func TestIngestRawMessage_MissingSourceID_Rejected(t *testing.T) {
	env := setupRawIngestEnv(t)
	t.Parallel()
	ev := buildRawMessageEvent(t, events.KindRawMessageReceived, env.pairedHostID,
		"guid-some", "chat-1", "+15551234567")
	ev["source_id"] = ""
	w := postIngestRaw(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 0, resp.Accepted)
	require.Equal(t, 1, resp.Rejected)
	require.Len(t, resp.Errors, 1)
	require.Equal(t, "MISSING_FIELD", resp.Errors[0].Code)
}

// TestIngestRawMessage_InvalidMessageType_Rejected asserts a payload
// with an out-of-set message_type is REJECTED at the handler.
func TestIngestRawMessage_InvalidMessageType_Rejected(t *testing.T) {
	env := setupRawIngestEnv(t)
	t.Parallel()
	guid := "test-guid-" + uuid.NewString()
	now := accelerated.GetCurrentTime()
	p := events.RawMessageReceivedPayload{
		Version:     1,
		HostID:      env.pairedHostID,
		Source:      "messages",
		Guid:        guid,
		ChatID:      "chat-1",
		PeerHandle:  "+15551234567",
		MessageType: "videocall",
		SentAt:      now,
	}
	pBytes, err := events.Marshal(events.KindRawMessageReceived, p)
	require.NoError(t, err)
	ev := map[string]any{
		"source":      "messages",
		"source_id":   guid,
		"kind":        string(events.KindRawMessageReceived),
		"payload":     json.RawMessage(pBytes),
		"observed_at": now,
	}
	w := postIngestRaw(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code)
	resp := parseIngestResp(t, w)
	require.Equal(t, 0, resp.Accepted)
	require.Equal(t, 1, resp.Rejected)
	require.Equal(t, "PAYLOAD_INVALID", resp.Errors[0].Code)
}

// TestIngestRawMessage_BatchDedupesAggregatorJobs asserts that 3
// raw_message events for the same contact in one batch enqueue
// exactly ONE aggregator River job (per-batch dedup).
func TestIngestRawMessage_BatchDedupesAggregatorJobs(t *testing.T) {
	env := setupRawIngestEnv(t)
	t.Parallel()
	mkEv := func() map[string]any {
		return buildRawMessageEvent(t, events.KindRawMessageReceived, env.pairedHostID,
			"test-guid-"+uuid.NewString(), "chat-1", "+15551234567")
	}
	w := postIngestRaw(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{mkEv(), mkEv(), mkEv()},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 3, resp.Accepted)
	require.Equal(t, 1, countRiverJobs(t, env, "messaging_aggregate_for_contact"))
}

// TestIngestRawMessage_AdminRematch_MatchesAfterContactAdded covers
// the operator remediation path: a previously-stranded row gets
// matched + enqueued by the admin handler after a contact_method is
// added that matches it.
func TestIngestRawMessage_AdminRematch_MatchesAfterContactAdded(t *testing.T) {
	env := setupRawIngestEnv(t)
	t.Parallel()
	guid := "test-guid-" + uuid.NewString()
	// Step 1 — POST a raw_message with an unknown peer.
	ev := buildRawMessageEvent(t, events.KindRawMessageReceived, env.pairedHostID,
		guid, "chat-x", "+15558887777")
	w := postIngestRaw(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, parseIngestResp(t, w).Accepted)

	msg, err := env.messagesRepo.GetMessage(context.Background(), guid)
	require.NoError(t, err)
	require.Nil(t, msg.MatchedContactID)

	// Step 2 — operator adds a contact_method that matches the
	// stranded peer.
	created, err := env.contactRepo.CreateContact(context.Background(), repository.CreateContactRequest{
		FullName: "Late Add Contact",
	})
	require.NoError(t, err)
	contactB := created.ID
	_, err = env.cmRepo.CreateContactMethod(context.Background(), repository.CreateContactMethodRequest{
		ContactID: contactB,
		Type:      "phone",
		Value:     "+15558887777",
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = env.cmRepo.DeleteContactMethodsByContact(context.Background(), contactB)
		_ = env.contactRepo.HardDeleteContact(context.Background(), contactB)
	})

	// Step 3 — invoke admin rematch handler.
	res, err := messages.RematchStranded(context.Background(), messages.RematchStrandedDeps{
		Messages:    env.messagesRepo,
		Identity:    env.identityService,
		RiverClient: adminRiverInserter{client: env.riverClient},
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, res.Scanned, 1)
	require.Equal(t, 1, res.Matched)
	require.Equal(t, 1, res.Enqueued)

	// Step 4 — verify the staging row is now matched.
	msg, err = env.messagesRepo.GetMessage(context.Background(), guid)
	require.NoError(t, err)
	require.NotNil(t, msg.MatchedContactID)
	require.Equal(t, contactB, *msg.MatchedContactID)
}
