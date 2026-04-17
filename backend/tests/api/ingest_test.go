package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/auth"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// ingestTestAPIKey is the fixed key tests send in X-API-Key. TestConfig
// leaves External.APIKey empty; we override to this value and assert
// authentication against it.
const ingestTestAPIKey = "test-ingest-api-key"

// ingestTestNoopArgs satisfies river's "at least one worker" requirement.
// No job is enqueued at this kind — PR 4 only publishes events; consumer
// jobs arrive in PR 5+.
type ingestTestNoopArgs struct{}

func (ingestTestNoopArgs) Kind() string { return "ingest_test_noop" }

type ingestTestNoopWorker struct {
	river.WorkerDefaults[ingestTestNoopArgs]
}

func (*ingestTestNoopWorker) Work(_ context.Context, _ *river.Job[ingestTestNoopArgs]) error {
	return nil
}

// ingestTestSetup bundles the router + deps returned by setupIngestTestRouter.
// Exposed fields let subtests interact with the DB directly (e.g., for
// counting rows persisted for a given source).
type ingestTestSetup struct {
	router     *gin.Engine
	apiKey     string
	eventRepo  *repository.EventRepository
	database   *db.Database
	ctx        context.Context
	busFactory func(repo events.EventRepository) *events.Bus // for mid-tx-rollback test
}

// setupIngestTestRouter builds a full router with real DB, real river, real
// Bus, real IngestService + IngestHandler, and conditional route
// registration based on enableIngest. Returns a cleanup closure registered
// via t.Cleanup so callers don't need to defer anything.
func setupIngestTestRouter(t *testing.T, enableIngest bool) *ingestTestSetup {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	// Migrations are applied once by TestMain.

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	cfg.Database.MigrationsPath = getMigrationsPath()
	cfg.External.APIKey = ingestTestAPIKey
	cfg.Features.EnableEventBusIngest = enableIngest

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	// River client (TestOnly skips leader election / periodic loops).
	workers := river.NewWorkers()
	river.AddWorker(workers, &ingestTestNoopWorker{})
	// PR 7: interaction.recorded events enqueue cadence_updater jobs via
	// consumerJobsForKind. Register a no-op placeholder so river accepts
	// the kind at InsertTx; TestOnly=true means it never runs. Ingest
	// tests that DO exercise CadenceUpdater (see ingest_cadence_test.go)
	// invoke it manually after the HTTP POST.
	river.AddWorker(workers, &apiTestCadenceShim{})
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	startCtx, startCancel := context.WithTimeout(ctx, 10*time.Second)
	defer startCancel()
	require.NoError(t, client.Start(startCtx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
	})

	eventRepo := repository.NewEventRepository(database.Queries)
	busFactory := func(repo events.EventRepository) *events.Bus {
		return events.NewBus(database.Pool, client, repo)
	}
	eventBus := busFactory(eventRepo)
	ingestService := service.NewIngestService(database, eventBus)
	ingestHandler := handlers.NewIngestHandler(ingestService)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	router.Use(api.LoggingMiddleware())
	router.Use(api.CORSMiddleware(cfg.CORS))
	router.Use(api.ErrorHandlerMiddleware())

	v1 := router.Group("/api/v1")
	v1.Use(auth.APIKeyMiddleware(cfg))
	{
		if enableIngest {
			ingest := v1.Group("/ingest")
			{
				ingest.POST("/events", ingestHandler.IngestEvents)
			}
		}
	}

	return &ingestTestSetup{
		router:     router,
		apiKey:     cfg.External.APIKey,
		eventRepo:  eventRepo,
		database:   database,
		ctx:        ctx,
		busFactory: busFactory,
	}
}

// postIngest POSTs a JSON body to /api/v1/ingest/events with the given
// API key header. Returns the recorder for assertion.
func postIngest(t *testing.T, router *gin.Engine, apiKey string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func jsonBody(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// buildManualEventReq builds a valid interaction.manual ingest request body.
// source/sourceID are caller-supplied to ensure per-test uniqueness under
// the shared DB.
func buildManualEventReq(t *testing.T, source, sourceID string) handlers.IngestEventRequest {
	t.Helper()
	observed := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	payload, err := events.Marshal(events.KindInteractionManual, events.InteractionManualPayload{
		Version:    1,
		ContactID:  uuid.New(),
		Direction:  "mutual",
		OccurredAt: observed,
	})
	require.NoError(t, err)
	return handlers.IngestEventRequest{
		Source:     source,
		SourceID:   sourceID,
		Kind:       string(events.KindInteractionManual),
		Payload:    payload,
		ObservedAt: &observed,
	}
}

// uniqueIngestSource builds a per-subtest unique source name so tests on
// the shared DB don't observe each other's rows via CountBySource.
func uniqueIngestSource(prefix string) string {
	return prefix + "-" + uuid.NewString()
}

func TestIngest_ValidBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, true)

	source := uniqueIngestSource("valid-batch")
	batch := handlers.IngestBatchRequest{
		Events: []handlers.IngestEventRequest{
			buildManualEventReq(t, source, uuid.NewString()),
			buildManualEventReq(t, source, uuid.NewString()),
			buildManualEventReq(t, source, uuid.NewString()),
		},
	}

	w := postIngest(t, setup.router, setup.apiKey, jsonBody(t, batch))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp handlers.IngestResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, 3, resp.Accepted)
	require.Equal(t, 0, resp.Duplicate)
	require.Equal(t, 0, resp.Rejected)
	require.NotNil(t, resp.Errors)
	require.Len(t, resp.Errors, 0)

	count, err := setup.eventRepo.CountBySource(setup.ctx, source)
	require.NoError(t, err)
	require.Equal(t, int64(3), count)
}

func TestIngest_AuthFailure_MissingKey(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, true)
	batch := handlers.IngestBatchRequest{
		Events: []handlers.IngestEventRequest{
			buildManualEventReq(t, uniqueIngestSource("auth-missing"), uuid.NewString()),
		},
	}
	w := postIngest(t, setup.router, "", jsonBody(t, batch))
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "MISSING_API_KEY")
}

func TestIngest_AuthFailure_InvalidKey(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, true)
	batch := handlers.IngestBatchRequest{
		Events: []handlers.IngestEventRequest{
			buildManualEventReq(t, uniqueIngestSource("auth-invalid"), uuid.NewString()),
		},
	}
	w := postIngest(t, setup.router, "wrong-key", jsonBody(t, batch))
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), "INVALID_API_KEY")
}

func TestIngest_MalformedJSON(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, true)
	w := postIngest(t, setup.router, setup.apiKey, []byte("{not json"))
	require.Equal(t, http.StatusBadRequest, w.Code)
}

// TestIngest_MissingFields exercises spec §3.5's "batch continues" rule:
// a mixed batch where events are missing source / kind / payload /
// observed_at must return HTTP 200 with those events surfaced in
// errors[] — NOT 400'd wholesale at gin's bind step.
func TestIngest_MissingFields(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, true)

	source := uniqueIngestSource("missing-fields")
	valid := buildManualEventReq(t, source, uuid.NewString())
	rawValid := jsonBody(t, valid)

	// Construct raw JSON events that each violate a single required field.
	// index=0: valid.
	// index=1: observed_at is the zero-time (serializable as a literal RFC3339).
	// index=2: source is absent.
	// index=3: kind is absent.
	// index=4: payload is absent.
	// index=5: observed_at key absent entirely.
	zeroTimeEv := fmt.Sprintf(`{"source":%q,"source_id":%q,"kind":%q,"payload":%s,"observed_at":"0001-01-01T00:00:00Z"}`,
		source, uuid.NewString(), string(events.KindInteractionManual), string(valid.Payload))
	missingSource := fmt.Sprintf(`{"source_id":%q,"kind":%q,"payload":%s,"observed_at":"2026-04-10T12:00:00Z"}`,
		uuid.NewString(), string(events.KindInteractionManual), string(valid.Payload))
	missingKind := fmt.Sprintf(`{"source":%q,"source_id":%q,"payload":%s,"observed_at":"2026-04-10T12:00:00Z"}`,
		source, uuid.NewString(), string(valid.Payload))
	missingPayload := fmt.Sprintf(`{"source":%q,"source_id":%q,"kind":%q,"observed_at":"2026-04-10T12:00:00Z"}`,
		source, uuid.NewString(), string(events.KindInteractionManual))
	missingObservedAt := fmt.Sprintf(`{"source":%q,"source_id":%q,"kind":%q,"payload":%s}`,
		source, uuid.NewString(), string(events.KindInteractionManual), string(valid.Payload))

	body := []byte(`{"events":[` + string(rawValid) + `,` +
		zeroTimeEv + `,` +
		missingSource + `,` +
		missingKind + `,` +
		missingPayload + `,` +
		missingObservedAt + `]}`)

	w := postIngest(t, setup.router, setup.apiKey, body)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp handlers.IngestResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, 1, resp.Accepted)
	require.Equal(t, 5, resp.Rejected)
	require.Len(t, resp.Errors, 5)

	// Every per-event rejection should be coded MISSING_FIELD.
	indexes := map[int]string{}
	for _, e := range resp.Errors {
		require.Equal(t, "MISSING_FIELD", e.Code)
		indexes[e.Index] = e.Message
	}
	require.Contains(t, indexes, 1)
	require.Contains(t, indexes[1], "observed_at")
	require.Contains(t, indexes, 2)
	require.Contains(t, indexes[2], "source")
	require.Contains(t, indexes, 3)
	require.Contains(t, indexes[3], "kind")
	require.Contains(t, indexes, 4)
	require.Contains(t, indexes[4], "payload")
	require.Contains(t, indexes, 5)
	require.Contains(t, indexes[5], "observed_at")

	// The valid event at index 0 persisted.
	count, err := setup.eventRepo.CountBySource(setup.ctx, source)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

// TestIngest_NullPayload covers the `payload: null` case: stdlib json
// would silently decode null into a zero-value struct, so the ingest
// boundary must reject it as PAYLOAD_INVALID rather than persist a row
// with all-zero payload fields.
func TestIngest_NullPayload(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, true)
	source := uniqueIngestSource("null-payload")
	nullEv := fmt.Sprintf(`{"source":%q,"source_id":%q,"kind":%q,"payload":null,"observed_at":"2026-04-10T12:00:00Z"}`,
		source, uuid.NewString(), string(events.KindInteractionManual))
	body := []byte(`{"events":[` + nullEv + `]}`)

	w := postIngest(t, setup.router, setup.apiKey, body)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp handlers.IngestResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Accepted)
	require.Equal(t, 1, resp.Rejected)
	// Null payload is caught by the per-event validator — either via
	// the "payload is required" path (RawMessage is nil on `null`) or
	// via ValidatePayload's null-rejection.
	require.Len(t, resp.Errors, 1)
	require.Contains(t, []string{"MISSING_FIELD", "PAYLOAD_INVALID"}, resp.Errors[0].Code)

	// Confirm nothing was persisted.
	count, err := setup.eventRepo.CountBySource(setup.ctx, source)
	require.NoError(t, err)
	require.Equal(t, int64(0), count)
}

func TestIngest_UnknownKind(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, true)
	source := uniqueIngestSource("unknown-kind")

	valid := buildManualEventReq(t, source, uuid.NewString())
	unknown := buildManualEventReq(t, source, uuid.NewString())
	unknown.Kind = "nope.kind"

	batch := handlers.IngestBatchRequest{
		Events: []handlers.IngestEventRequest{valid, unknown},
	}
	w := postIngest(t, setup.router, setup.apiKey, jsonBody(t, batch))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp handlers.IngestResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, 1, resp.Accepted)
	require.Equal(t, 1, resp.Rejected)
	require.Len(t, resp.Errors, 1)
	require.Equal(t, 1, resp.Errors[0].Index)
	require.Equal(t, "UNKNOWN_KIND", resp.Errors[0].Code)
}

func TestIngest_PayloadStructurallyInvalid(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, true)
	source := uniqueIngestSource("payload-invalid")

	// Use KindCalendarAttended: ContactID is uuid.UUID. Send a raw payload
	// with contact_id="not-a-uuid" — JSON decoder rejects with
	// UnmarshalTypeError, which ValidatePayload wraps.
	badPayload := json.RawMessage(`{"version":1,"contact_id":"not-a-uuid","event_id":"e","occurred_at":"2026-04-10T12:00:00Z"}`)
	observed := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	bad := handlers.IngestEventRequest{
		Source:     source,
		SourceID:   uuid.NewString(),
		Kind:       string(events.KindCalendarAttended),
		Payload:    badPayload,
		ObservedAt: &observed,
	}
	batch := handlers.IngestBatchRequest{Events: []handlers.IngestEventRequest{bad}}
	w := postIngest(t, setup.router, setup.apiKey, jsonBody(t, batch))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp handlers.IngestResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Accepted)
	require.Equal(t, 1, resp.Rejected)
	require.Len(t, resp.Errors, 1)
	require.Equal(t, "PAYLOAD_INVALID", resp.Errors[0].Code)
}

func TestIngest_BatchTooLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, true)

	// 501 events, each tiny. The batch-size check runs pre-validation so
	// payloads don't need to be valid for this specific test.
	source := uniqueIngestSource("batch-too-large")
	evs := make([]handlers.IngestEventRequest, 501)
	for i := range evs {
		evs[i] = buildManualEventReq(t, source, uuid.NewString())
	}
	batch := handlers.IngestBatchRequest{Events: evs}
	w := postIngest(t, setup.router, setup.apiKey, jsonBody(t, batch))
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "VALIDATION_ERROR")
}

func TestIngest_EmptyBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, true)

	// binding:"required" on Events []IngestEventRequest flags nil/missing.
	// We're testing the explicit empty-array case, which passes bind.
	body := []byte(`{"events":[]}`)
	w := postIngest(t, setup.router, setup.apiKey, body)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "events must be non-empty")
}

func TestIngest_DuplicateInBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, true)

	source := uniqueIngestSource("dup-in-batch")
	sourceID := uuid.NewString()
	first := buildManualEventReq(t, source, sourceID)
	second := buildManualEventReq(t, source, sourceID) // same (source, source_id)
	batch := handlers.IngestBatchRequest{
		Events: []handlers.IngestEventRequest{first, second},
	}
	w := postIngest(t, setup.router, setup.apiKey, jsonBody(t, batch))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp handlers.IngestResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, 1, resp.Accepted)
	require.Equal(t, 1, resp.Duplicate)
	require.Equal(t, 0, resp.Rejected)

	count, err := setup.eventRepo.CountBySource(setup.ctx, source)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

func TestIngest_DuplicateAcrossBatches(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, true)

	source := uniqueIngestSource("dup-across")
	sourceID := uuid.NewString()
	ev := buildManualEventReq(t, source, sourceID)

	// First request: accepted.
	w1 := postIngest(t, setup.router, setup.apiKey, jsonBody(t, handlers.IngestBatchRequest{
		Events: []handlers.IngestEventRequest{ev},
	}))
	require.Equal(t, http.StatusOK, w1.Code)
	var r1 handlers.IngestResponse
	require.NoError(t, json.Unmarshal(w1.Body.Bytes(), &r1))
	require.Equal(t, 1, r1.Accepted)
	require.Equal(t, 0, r1.Duplicate)

	// Second request: duplicate.
	w2 := postIngest(t, setup.router, setup.apiKey, jsonBody(t, handlers.IngestBatchRequest{
		Events: []handlers.IngestEventRequest{ev},
	}))
	require.Equal(t, http.StatusOK, w2.Code)
	var r2 handlers.IngestResponse
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &r2))
	require.Equal(t, 0, r2.Accepted)
	require.Equal(t, 1, r2.Duplicate)

	count, err := setup.eventRepo.CountBySource(setup.ctx, source)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
}

// TestIngest_NullSourceID_NotDeduped covers the partial-index semantics:
// migration 036 creates the unique index WHERE source_id IS NOT NULL, so
// NULL source_ids never collide. Two events with empty source_id submitted
// as two separate requests both insert.
func TestIngest_NullSourceID_NotDeduped(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, true)

	source := uniqueIngestSource("null-sid")
	ev1 := buildManualEventReq(t, source, "") // empty → NULL at DB
	ev2 := buildManualEventReq(t, source, "")

	w1 := postIngest(t, setup.router, setup.apiKey, jsonBody(t, handlers.IngestBatchRequest{
		Events: []handlers.IngestEventRequest{ev1},
	}))
	require.Equal(t, http.StatusOK, w1.Code)
	w2 := postIngest(t, setup.router, setup.apiKey, jsonBody(t, handlers.IngestBatchRequest{
		Events: []handlers.IngestEventRequest{ev2},
	}))
	require.Equal(t, http.StatusOK, w2.Code)

	var r2 handlers.IngestResponse
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &r2))
	require.Equal(t, 1, r2.Accepted, "NULL source_id must not trip the unique index")

	count, err := setup.eventRepo.CountBySource(setup.ctx, source)
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
}

// TestIngest_GateDisabled_Returns404 exercises the spec acceptance
// criterion: EVENT_BUS_INGEST_ENABLED=false → 404. The route is not
// registered; gin's default NoRoute handler emits 404 without running the
// v1 group's API-key middleware.
func TestIngest_GateDisabled_Returns404(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, false)
	batch := handlers.IngestBatchRequest{
		Events: []handlers.IngestEventRequest{
			buildManualEventReq(t, uniqueIngestSource("gate-off"), uuid.NewString()),
		},
	}
	w := postIngest(t, setup.router, setup.apiKey, jsonBody(t, batch))
	require.Equal(t, http.StatusNotFound, w.Code)
}

// TestIngest_GateDisabled_NoKey_StillReturns404 pins down plan Decision 1:
// the flag-off + no-key case returns 404, NOT 401. An unregistered route
// bypasses the v1 group's middleware chain entirely.
func TestIngest_GateDisabled_NoKey_StillReturns404(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, false)
	body := []byte(`{"events":[]}`)
	w := postIngest(t, setup.router, "", body)
	require.Equal(t, http.StatusNotFound, w.Code)
	// Ensure the auth-middleware's MISSING_API_KEY error envelope is NOT
	// present — proves no middleware ran.
	require.NotContains(t, w.Body.String(), "MISSING_API_KEY")
}

func TestIngest_MixedBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, true)

	source := uniqueIngestSource("mixed")
	dupSourceID := uuid.NewString()

	// 4 valid events (distinct source_ids) + 1 unknown-kind + 1
	// duplicate-of-first-valid. Expected: accepted=4, duplicate=1,
	// rejected=1.
	ev0 := buildManualEventReq(t, source, dupSourceID)
	ev1 := buildManualEventReq(t, source, uuid.NewString())
	ev2 := buildManualEventReq(t, source, uuid.NewString())
	ev3 := buildManualEventReq(t, source, uuid.NewString())
	evBad := buildManualEventReq(t, source, uuid.NewString())
	evBad.Kind = "nope.kind"                             // rejected pre-tx
	evDup := buildManualEventReq(t, source, dupSourceID) // duplicates ev0

	batch := handlers.IngestBatchRequest{
		Events: []handlers.IngestEventRequest{ev0, ev1, ev2, ev3, evBad, evDup},
	}
	w := postIngest(t, setup.router, setup.apiKey, jsonBody(t, batch))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp handlers.IngestResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, 4, resp.Accepted)
	require.Equal(t, 1, resp.Duplicate)
	require.Equal(t, 1, resp.Rejected)
	require.Len(t, resp.Errors, 1)
	require.Equal(t, 4, resp.Errors[0].Index) // index of evBad in the original batch
	require.Equal(t, 6, resp.Accepted+resp.Duplicate+resp.Rejected)

	count, err := setup.eventRepo.CountBySource(setup.ctx, source)
	require.NoError(t, err)
	require.Equal(t, int64(4), count)
}

// TestIngest_BodyTooLarge_Returns413 posts a body exceeding the 8 MiB cap.
// The cheapest way is one event with an oversized payload. The
// per-payload size check would also reject this but the MaxBytesReader
// wrap at the handler boundary fires first because it's enforced during
// JSON decode, before any per-field validation runs.
func TestIngest_BodyTooLarge_Returns413(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, true)

	// Build a body > 8 MiB: an events array containing a single event with
	// a ~9 MiB string payload. The raw body doesn't need to be valid JSON
	// for a real event — the 413 path fires before validation.
	big := strings.Repeat("x", 9<<20)
	raw := fmt.Sprintf(`{"events":[{"source":"x","kind":"interaction.manual","payload":{"v":%q},"observed_at":"2026-04-10T12:00:00Z"}]}`, big)
	w := postIngest(t, setup.router, setup.apiKey, []byte(raw))
	require.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "body: %s", w.Body.String())
}

// failingRepo is a test double that wraps a real EventRepository and fails
// InsertEvent on the Nth call. Used to exercise the mid-tx rollback
// contract: a service-level error must roll back the WHOLE batch, not
// just the failing event.
type failingRepo struct {
	inner   events.EventRepository
	failOn  int
	callCnt int
	fireCnt int // how many times the failure actually triggered
}

func (f *failingRepo) InsertEvent(ctx context.Context, tx pgx.Tx, env *events.Envelope) error {
	f.callCnt++
	if f.callCnt == f.failOn {
		f.fireCnt++
		return fmt.Errorf("injected failure at call %d", f.callCnt)
	}
	return f.inner.InsertEvent(ctx, tx, env)
}

func (f *failingRepo) GetEvent(ctx context.Context, id uuid.UUID) (*events.Envelope, error) {
	return f.inner.GetEvent(ctx, id)
}

func (f *failingRepo) FindEventBySource(ctx context.Context, source, sourceID string) (*events.Envelope, error) {
	return f.inner.FindEventBySource(ctx, source, sourceID)
}

// TestIngest_MidTxRollback_RollsBack_And_Returns500 exercises the spec §3.5
// atomicity contract: on an unexpected mid-batch failure, the whole tx
// rolls back and NO rows are persisted. A fake EventRepository wraps the
// real one and fails on its 3rd InsertEvent; a 5-event batch is posted
// and we assert zero rows landed in the event table for this batch's
// source prefix.
func TestIngest_MidTxRollback_RollsBack_And_Returns500(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	setup := setupIngestTestRouter(t, true)

	// Wire a new handler over a failing repo, mounted on a fresh router
	// so the rest of the suite isn't affected.
	fake := &failingRepo{inner: setup.eventRepo, failOn: 3}
	bus := setup.busFactory(fake)
	ingestService := service.NewIngestService(setup.database, bus)
	handler := handlers.NewIngestHandler(ingestService)

	cfg := config.TestConfig()
	cfg.External.APIKey = setup.apiKey
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	router.Use(api.LoggingMiddleware())
	router.Use(api.CORSMiddleware(cfg.CORS))
	router.Use(api.ErrorHandlerMiddleware())
	v1 := router.Group("/api/v1")
	v1.Use(auth.APIKeyMiddleware(cfg))
	{
		v1.POST("/ingest/events", handler.IngestEvents)
	}

	source := uniqueIngestSource("midtx-rollback")
	evs := make([]handlers.IngestEventRequest, 5)
	for i := range evs {
		evs[i] = buildManualEventReq(t, source, uuid.NewString())
	}
	batch := handlers.IngestBatchRequest{Events: evs}
	w := postIngest(t, router, setup.apiKey, jsonBody(t, batch))
	require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	require.Contains(t, w.Body.String(), "INTERNAL_ERROR")

	// Sanity: the failure actually fired.
	require.Equal(t, 1, fake.fireCnt, "injected failure should have triggered once")

	// Atomicity: zero rows persisted for this batch's source prefix.
	count, err := setup.eventRepo.CountBySource(setup.ctx, source)
	require.NoError(t, err)
	require.Equal(t, int64(0), count,
		"spec §3.5: all-or-nothing on unexpected errors — no rows must persist")
}
