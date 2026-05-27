package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/auth"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/todoist"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// call.* ingest integration tests.
//
// These tests exercise the full handleCall hot path against a real DB:
// HTTP -> IngestHandler -> IngestService.IngestBatch -> handleCall ->
// identity match -> phone_call upsert -> ContactService.RecordInteractionTx
// -> bus.PublishTx (interaction.recorded) -> inline CadenceUpdater.HandleEvent
// -> inline FollowUpManager.HandleEvent -> MarkProcessedTx.
//
// Verified observables for the qualified-interaction path:
//   - phone_call row created with the correct fields and matched contact.
//   - interaction row created with source='phone_calls' and the expected
//     direction.
//   - event row landed with kind='interaction.recorded' and a V3 payload
//     carrying prev_cadence_snapshot.
//   - contact's last_contacted (inbound) or last_outreach_at (outbound)
//     bumped by the inline cadence apply.
//
// Failure-rollback tests (T14 / T15) inject failures via the
// failingPhoneCallWriter wrapper so the whole tx rolls back, leaving the
// event log / phone_call / interaction tables untouched for the failed
// envelope.
// ----------------------------------------------------------------------------

// phoneCallIngestEnv bundles the wired stack for the call.* integration
// tests. Pattern mirrors ingestRawTestEnv / extContactIngestEnv but
// wires the four extra IngestService deps (phoneCalls, contactRecorder,
// cadence, followUp) that the call.* inline handler needs.
type phoneCallIngestEnv struct {
	router         *gin.Engine
	apiKey         string
	database       *db.Database
	macService     *service.MacHostService
	phoneCallRepo  *repository.PhoneCallRepository
	contactRepo    *repository.ContactRepository
	cmRepo         *repository.ContactMethodRepository
	eventRepo      *repository.EventRepository
	interactionRpo *repository.InteractionRepository
	claimRepo      *repository.EventConsumerClaimRepository
	pairedHostID   uuid.UUID
	pairedHostKey  string
	seededContact  uuid.UUID
	seededPhone    string
	sourcePrefix   string

	// Optional wrapper used by failure-injection subtests. When non-nil
	// and configured to fail, UpsertCallTx or MarkProcessedTx returns
	// an error; nil-or-unconfigured wrapper behaves identically to the
	// real repo.
	failWriter *failingPhoneCallWriter
}

// failingPhoneCallWriter wraps a real PhoneCallWriter and can be flipped
// to return an error at the configured method. Used to exercise the
// per-event savepoint rollback path on infrastructure-like failures
// (UpsertCallTx / MarkProcessedTx) without needing a flaky DB.
type failingPhoneCallWriter struct {
	inner             service.PhoneCallWriter
	failOnUpsert      error
	failOnMarkProcess error
}

func (f *failingPhoneCallWriter) UpsertCallTx(ctx context.Context, tx pgx.Tx, p repository.UpsertPhoneCallParams) (*repository.PhoneCall, error) {
	if f.failOnUpsert != nil {
		return nil, f.failOnUpsert
	}
	return f.inner.UpsertCallTx(ctx, tx, p)
}

func (f *failingPhoneCallWriter) MarkProcessedTx(ctx context.Context, tx pgx.Tx, p repository.MarkProcessedParams) error {
	if f.failOnMarkProcess != nil {
		return f.failOnMarkProcess
	}
	return f.inner.MarkProcessedTx(ctx, tx, p)
}

// setupPhoneCallIngestEnv wires the full call.* ingest stack against a
// real DB + live river client. Returns a populated env or t.Skip if no
// DATABASE_URL is set.
func setupPhoneCallIngestEnv(t *testing.T) *phoneCallIngestEnv {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	cfg.External.APIKey = macHostTestKey
	cfg.Features.EnableEventBusIngest = true

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)

	hostRepo := repository.NewMacHostRepository(database.Queries)
	pairingRepo := repository.NewMacHostPairingTokenRepository(database.Queries)
	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	meetingNoteRepo := repository.NewMeetingNoteRepository(database.Queries)
	macService := service.NewMacHostService(hostRepo, pairingRepo, syncRepo, contactMethodRepo, externalRepo, meetingNoteRepo, database.Pool, 4)

	identityRepo := repository.NewIdentityRepository(database.Queries)
	identityService := service.NewIdentityService(identityRepo)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	phoneCallRepo := repository.NewPhoneCallRepository(database.Queries)
	eventRepo := repository.NewEventRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)

	// River client — used by FollowUpManager.HandleEvent (RiverInserter)
	// and by Bus.PublishTx (consumerJobsForKind enqueues
	// cadence_updater + followup_manager jobs alongside the event row).
	// Workers are shims that no-op so the post-publish async path is
	// inert; the inline cadence/follow-up apply done by handleCall is
	// what the test verifies, not the queued re-delivery.
	workers := river.NewWorkers()
	river.AddWorker(workers, &apiTestCadenceShim{})
	river.AddWorker(workers, &apiTestFollowUpShim{})
	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		JobTimeout: cfg.River.JobTimeout,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 1},
		},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	require.NoError(t, riverClient.Start(ctx))
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = riverClient.Stop(stopCtx)
	})

	eventBus := events.NewBus(database.Pool, riverClient, eventRepo)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo, eventBus, nil)

	eventClaimRepo := repository.NewEventConsumerClaimRepository(database.Queries)
	cadenceUpdater := consumer.NewCadenceUpdater(
		eventClaimRepo,
		contactRepo,
		database.Queries,
		consumer.CadenceModeCutover,
		false,
	)
	contactService.SetCadenceUpdater(cadenceUpdater)

	// FollowUpManager wired to ErrTodoistUnconfigured so the post-commit
	// Todoist branch degrades to local-only writes. Phone_calls v1.5 has
	// no Todoist integration; the post-commit closure is expected to be
	// nil on the inline path, so this wiring matters only for
	// defence-in-depth.
	followUpManager := consumer.NewFollowUpManager(
		consumer.FollowUpModeCutover,
		eventClaimRepo,
		contactRepo,
		contactTaskRepo,
		contactTaskRepo,
		interactionRepo,
		riverClient,
		database.Pool,
		func(ctx context.Context) (*todoist.Settings, string, error) {
			return nil, "", consumer.ErrTodoistUnconfigured
		},
		todoist.DefaultClientFactory,
		cfg.CORS.FrontendURL,
		cfg.Watchdog,
	)
	contactService.SetFollowUpConsumer(followUpManager)

	// Optional fail-writer wrapper. failWriter starts disabled (both
	// failOn* fields nil) so it behaves identically to the real repo.
	failWriter := &failingPhoneCallWriter{inner: phoneCallRepo}

	ingestService := service.NewIngestService(
		database,
		eventBus,
		identityService,
		nil,
		riverClient,
		externalRepo,
		hostRepo,
		nil, // meetingNotes unused on phone_call path
		nil, // calendar unused
		nil, // interactions unused
		nil, // identityLookup unused
		nil, // contactSvc unused
		failWriter,
		contactService,
		cadenceUpdater,
		followUpManager,
		nil, // titleMatcher unused on phone_call path
		nil, // discovery unused
	)
	ingestHandler := handlers.NewIngestHandler(ingestService)

	gin.SetMode(gin.TestMode)
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

	// Pair a host.
	plain, _, err := macService.CreatePairingToken(ctx)
	require.NoError(t, err)
	pair, err := macService.PairWithToken(ctx, plain, "phone-calls-test", "0.1.0", 2)
	require.NoError(t, err)

	// Seed a contact + phone contact_method. Phone uses uuid hex digits
	// to keep the normalized value unique across parallel test runs on
	// the shared DB.
	suffix := uuid.NewString()[:8]
	digits := makeDigits(suffix)
	created, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "PhoneCall Test " + suffix,
	})
	require.NoError(t, err)
	contactID := created.ID
	phone := "+1" + digits
	_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contactID,
		Type:      "phone",
		Value:     phone,
	})
	require.NoError(t, err)

	env := &phoneCallIngestEnv{
		router:         router,
		apiKey:         cfg.External.APIKey,
		database:       database,
		macService:     macService,
		phoneCallRepo:  phoneCallRepo,
		contactRepo:    contactRepo,
		cmRepo:         contactMethodRepo,
		eventRepo:      eventRepo,
		interactionRpo: interactionRepo,
		claimRepo:      eventClaimRepo,
		pairedHostID:   pair.HostID,
		pairedHostKey:  pair.APIKey,
		seededContact:  contactID,
		seededPhone:    phone,
		sourcePrefix:   "pcint-" + suffix + "-",
		failWriter:     failWriter,
	}

	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// phone_call rows are scoped to the host's mac_host_id.
		_ = phoneCallRepo.HardDeleteByMacHost(cleanCtx, env.pairedHostID)
		// Event-log rows + identity rows under the phone_calls source.
		_, _ = database.Queries.DeleteEventsBySource(cleanCtx, "phone_calls")
		_, _ = database.Queries.DeleteExternalIdentitiesBySource(cleanCtx, "phone_calls")
		_ = contactMethodRepo.DeleteContactMethodsByContact(cleanCtx, env.seededContact)
		_ = contactRepo.HardDeleteContact(cleanCtx, env.seededContact)
		_, _ = database.Queries.DeleteAllMacHosts(cleanCtx)
		_, _ = database.Queries.DeleteAllPairingTokens(cleanCtx)
		database.Close()
	})

	return env
}

// makeDigits derives a 10-digit numeric suffix from an 8-char hex string
// so the seeded phone normalizes to a unique E.164 value across parallel
// test runs on the shared DB.
func makeDigits(seed string) string {
	out := make([]byte, 0, 10)
	for _, c := range seed {
		switch {
		case c >= '0' && c <= '9':
			out = append(out, byte(c))
		case c >= 'a' && c <= 'f':
			out = append(out, byte('0'+(c-'a')%10))
		}
		if len(out) >= 10 {
			break
		}
	}
	for len(out) < 10 {
		out = append(out, '0')
	}
	return string(out)
}

// buildCallEvent constructs an ingest-event request map for a call.* kind.
// The caller can mutate the payload via the supplied closure to test
// invariants / version envelopes.
func buildCallEvent(t *testing.T, kind events.Kind, hostID uuid.UUID, callUniqueID, peer string, mutate func(*events.CallPayload)) map[string]any {
	t.Helper()
	now := accelerated.GetCurrentTime()
	answered := true
	direction := "inbound"
	if kind == events.KindCallSent {
		direction = "outbound"
	}
	p := events.CallPayload{
		Version:         1,
		HostID:          hostID,
		Source:          "phone_calls",
		CallUniqueID:    callUniqueID,
		PeerHandle:      peer,
		PeerNormalized:  peer,
		Service:         "voice",
		Direction:       direction,
		Answered:        &answered,
		HasVoicemail:    false,
		DurationSeconds: 42,
		StartedAt:       now,
	}
	if mutate != nil {
		mutate(&p)
	}
	pBytes, err := events.Marshal(kind, p)
	require.NoError(t, err)
	return map[string]any{
		"source":      "phone_calls",
		"source_id":   callUniqueID,
		"kind":        string(kind),
		"payload":     json.RawMessage(pBytes),
		"observed_at": now,
	}
}

func postIngestCall(t *testing.T, env *phoneCallIngestEnv, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/events", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mac-Host-ID", env.pairedHostID.String())
	req.Header.Set("Authorization", "Bearer "+env.pairedHostKey)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	return w
}

// TestIngestCall_Inbound_RecordsInteractionAndBumpsLastContacted runs
// the full ingest -> handleCall -> publish -> inline-cadence -> mark-
// processed path for a fresh inbound answered call and verifies the
// four observable side-effects: phone_call row, interaction row, event
// row with V3 payload + prev_cadence_snapshot, and contact's
// last_contacted bump (inbound interactions bump last_contacted but
// NOT last_outreach_at — phone_calls follow the standard direction
// semantics).
func TestIngestCall_Inbound_RecordsInteractionAndBumpsLastContacted(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env := setupPhoneCallIngestEnv(t)
	ctx := context.Background()

	callID := env.sourcePrefix + "inbound"
	ev := buildCallEvent(t, events.KindCallReceived, env.pairedHostID, callID, env.seededPhone, nil)
	w := postIngestCall(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted, "errors=%+v", resp.Errors)
	require.Equal(t, 0, resp.Rejected, "errors=%+v", resp.Errors)

	// (a) phone_call row created and marked processed with an interaction_id.
	pc, err := env.phoneCallRepo.GetCallByUniqueID(ctx, callID)
	require.NoError(t, err)
	require.Equal(t, env.seededPhone, pc.PeerNormalized)
	require.Equal(t, "inbound", pc.Direction)
	require.NotNil(t, pc.MatchedContactID)
	require.Equal(t, env.seededContact, *pc.MatchedContactID)
	require.NotNil(t, pc.ProcessedAt)
	require.NotNil(t, pc.InteractionID, "qualified inbound must link to an interaction")

	// (b) interaction row exists with source='phone_calls' and inbound direction.
	interaction, err := env.interactionRpo.FindBySourceRef(ctx, env.seededContact, "phone_calls", callID)
	require.NoError(t, err)
	require.Equal(t, "phone_calls", interaction.Source)
	require.Equal(t, "inbound", interaction.Direction)
	require.Equal(t, *pc.InteractionID, interaction.ID)

	// (c) event row with kind='interaction.recorded' and V3 payload.
	recordedEv, err := env.eventRepo.FindEventBySource(ctx, "phone_calls", interaction.ID.String())
	require.NoError(t, err)
	require.Equal(t, events.KindInteractionRecorded, recordedEv.Kind)
	var recorded events.InteractionRecordedPayload
	require.NoError(t, json.Unmarshal(recordedEv.Payload, &recorded))
	require.Equal(t, 3, recorded.Version)
	require.Equal(t, env.seededContact, recorded.ContactID)
	require.Equal(t, "inbound", recorded.Direction)
	require.NotNil(t, recorded.PrevCadenceSnapshot, "V3 payload must carry prev_cadence_snapshot")

	// (d) contact's last_contacted bumped; last_outreach_at NOT touched
	// (inbound never bumps last_outreach_at).
	contact, err := env.contactRepo.GetContact(ctx, env.seededContact)
	require.NoError(t, err)
	require.NotNil(t, contact.LastContacted, "inbound interaction must bump last_contacted")
	require.Nil(t, contact.LastOutreachAt, "inbound must NOT bump last_outreach_at")

	// (e) inline cadence + follow-up legs ran. Both consumers write an
	// event_consumer_claim row under their consumer name; a missing
	// claim would mean the inline HandleEvent never fired (and the
	// queued re-delivery is no-op too, since this test uses
	// TestOnly river workers). Asserting on the claims proves both
	// inline legs of handleCall executed against the published event.
	cadenceClaimed, err := env.claimRepo.ExistsTx(ctx, nil, recordedEv.ID, repository.EventConsumerCadenceUpdater)
	require.NoError(t, err)
	require.True(t, cadenceClaimed, "inline cadence apply must have claimed the interaction.recorded event")
	followUpClaimed, err := env.claimRepo.ExistsTx(ctx, nil, recordedEv.ID, repository.EventConsumerFollowUpManager)
	require.NoError(t, err)
	require.True(t, followUpClaimed, "inline follow-up apply must have claimed the interaction.recorded event")
}

// TestIngestCall_Outbound_BumpsLastOutreachAt mirrors the inbound test
// for the outbound (call.sent) path: the inline cadence apply must bump
// last_outreach_at and NOT last_contacted (the user reaching out is
// recorded as outreach, not as contact).
func TestIngestCall_Outbound_BumpsLastOutreachAt(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env := setupPhoneCallIngestEnv(t)
	ctx := context.Background()

	callID := env.sourcePrefix + "outbound"
	ev := buildCallEvent(t, events.KindCallSent, env.pairedHostID, callID, env.seededPhone, func(p *events.CallPayload) {
		// Outbound: daemon should send answered=nil and has_voicemail=false.
		p.Answered = nil
		p.HasVoicemail = false
		p.DurationSeconds = 60
	})
	w := postIngestCall(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted, "errors=%+v", resp.Errors)

	pc, err := env.phoneCallRepo.GetCallByUniqueID(ctx, callID)
	require.NoError(t, err)
	require.Equal(t, "outbound", pc.Direction)
	require.Nil(t, pc.Answered, "outbound must force answered=NULL on staging")
	require.False(t, pc.HasVoicemail, "outbound must force has_voicemail=FALSE")

	interaction, err := env.interactionRpo.FindBySourceRef(ctx, env.seededContact, "phone_calls", callID)
	require.NoError(t, err)
	require.Equal(t, "outbound", interaction.Direction)

	contact, err := env.contactRepo.GetContact(ctx, env.seededContact)
	require.NoError(t, err)
	require.NotNil(t, contact.LastOutreachAt, "outbound interaction must bump last_outreach_at")
	require.Nil(t, contact.LastContacted, "outbound must NOT bump last_contacted")

	// Inline cadence + follow-up legs both ran for the outbound path
	// (mirrors the inbound assertion). FollowUpManager's outbound
	// branch is the path that returns the post-commit refresh closure
	// for contacts with a pending follow-up task; here no task exists
	// so the closure is nil, but the claim row lands either way.
	recordedEv, err := env.eventRepo.FindEventBySource(ctx, "phone_calls", interaction.ID.String())
	require.NoError(t, err)
	cadenceClaimed, err := env.claimRepo.ExistsTx(ctx, nil, recordedEv.ID, repository.EventConsumerCadenceUpdater)
	require.NoError(t, err)
	require.True(t, cadenceClaimed, "outbound: inline cadence apply must have claimed the event")
	followUpClaimed, err := env.claimRepo.ExistsTx(ctx, nil, recordedEv.ID, repository.EventConsumerFollowUpManager)
	require.NoError(t, err)
	require.True(t, followUpClaimed, "outbound: inline follow-up apply must have claimed the event")
}

// TestIngestCall_MissedInboundNoVoicemail_NoInteractionWritten covers
// the content-delivered decision-table NO-interaction branch end-to-end:
// the phone_call row IS still created and marked processed with
// interaction_id=NULL, but no interaction row, no interaction.recorded
// event, and no cadence bump.
func TestIngestCall_MissedInboundNoVoicemail_NoInteractionWritten(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env := setupPhoneCallIngestEnv(t)
	ctx := context.Background()

	callID := env.sourcePrefix + "missed"
	ev := buildCallEvent(t, events.KindCallReceived, env.pairedHostID, callID, env.seededPhone, func(p *events.CallPayload) {
		answered := false
		p.Answered = &answered
		p.HasVoicemail = false
		p.DurationSeconds = 0
	})
	w := postIngestCall(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted, "errors=%+v", resp.Errors)

	pc, err := env.phoneCallRepo.GetCallByUniqueID(ctx, callID)
	require.NoError(t, err)
	require.NotNil(t, pc.ProcessedAt, "missed-no-voicemail must still be marked processed")
	require.Nil(t, pc.InteractionID, "missed-no-voicemail must NOT link an interaction")

	_, err = env.interactionRpo.FindBySourceRef(ctx, env.seededContact, "phone_calls", callID)
	require.ErrorIs(t, err, db.ErrNotFound, "no interaction must be written for missed-no-voicemail")

	contact, err := env.contactRepo.GetContact(ctx, env.seededContact)
	require.NoError(t, err)
	require.Nil(t, contact.LastContacted, "missed-no-voicemail must NOT bump last_contacted")
	require.Nil(t, contact.LastOutreachAt, "missed-no-voicemail must NOT bump last_outreach_at")
}

// TestIngestCall_UpsertFailure_RollsBackBatch (T14) injects a phone_call
// upsert failure via the failingPhoneCallWriter. The per-event savepoint
// rolls back; the response surfaces a rejection. No phone_call row,
// no interaction row, no event row land for this envelope.
func TestIngestCall_UpsertFailure_RollsBackBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env := setupPhoneCallIngestEnv(t)
	ctx := context.Background()
	env.failWriter.failOnUpsert = errors.New("injected upsert failure")
	t.Cleanup(func() { env.failWriter.failOnUpsert = nil })

	callID := env.sourcePrefix + "upsert-fail"
	ev := buildCallEvent(t, events.KindCallReceived, env.pairedHostID, callID, env.seededPhone, nil)
	w := postIngestCall(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 0, resp.Accepted)
	require.Equal(t, 1, resp.Rejected)
	require.Contains(t, resp.Errors[0].Message, "upsert phone_call")

	_, err := env.phoneCallRepo.GetCallByUniqueID(ctx, callID)
	require.ErrorIs(t, err, db.ErrNotFound, "phone_call row must NOT exist after upsert rollback")
	_, err = env.interactionRpo.FindBySourceRef(ctx, env.seededContact, "phone_calls", callID)
	require.ErrorIs(t, err, db.ErrNotFound, "interaction row must NOT exist after upsert rollback")
}

// TestIngestCall_MarkProcessedFailure_RollsBackBatch (T15) injects a
// MarkProcessedTx failure. The interaction is created in the same tx
// as the staging upsert, so a mark-processed failure must roll back BOTH
// the staging row AND the interaction. The event-log row must also be
// rolled back.
func TestIngestCall_MarkProcessedFailure_RollsBackBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env := setupPhoneCallIngestEnv(t)
	ctx := context.Background()
	env.failWriter.failOnMarkProcess = errors.New("injected mark-processed failure")
	t.Cleanup(func() { env.failWriter.failOnMarkProcess = nil })

	callID := env.sourcePrefix + "mark-fail"
	ev := buildCallEvent(t, events.KindCallReceived, env.pairedHostID, callID, env.seededPhone, nil)
	w := postIngestCall(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 0, resp.Accepted)
	require.Equal(t, 1, resp.Rejected)
	require.Contains(t, resp.Errors[0].Message, "mark phone_call processed")

	_, err := env.phoneCallRepo.GetCallByUniqueID(ctx, callID)
	require.ErrorIs(t, err, db.ErrNotFound, "phone_call row must NOT exist after mark-processed rollback")
	_, err = env.interactionRpo.FindBySourceRef(ctx, env.seededContact, "phone_calls", callID)
	require.ErrorIs(t, err, db.ErrNotFound, "interaction row must NOT exist after mark-processed rollback")
	_, err = env.eventRepo.FindEventBySource(ctx, "phone_calls", callID)
	require.ErrorIs(t, err, db.ErrNotFound, "call.received event row must NOT exist after rollback")
}

// TestIngestCall_VersionTooHigh_Rejected (T22) covers the handler-side
// max-version rejection for call.* kinds. A payload with version=2
// (> eventsCallMaxKnownVersion=1) must be rejected with PAYLOAD_INVALID
// at the handler boundary BEFORE the batch tx opens; mirrors
// TestIngestExternalContact_VersionTooHigh_Rejected and the equivalent
// raw_message guard.
func TestIngestCall_VersionTooHigh_Rejected(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env := setupPhoneCallIngestEnv(t)

	callID := env.sourcePrefix + "vhigh"
	ev := buildCallEvent(t, events.KindCallReceived, env.pairedHostID, callID, env.seededPhone, func(p *events.CallPayload) {
		p.Version = 2 // exceeds eventsCallMaxKnownVersion=1
	})
	w := postIngestCall(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Rejected)
	require.Equal(t, "PAYLOAD_INVALID", resp.Errors[0].Code)
	require.Contains(t, resp.Errors[0].Message, "upgrade Pi")

	// No row should have landed.
	_, err := env.phoneCallRepo.GetCallByUniqueID(context.Background(), callID)
	require.ErrorIs(t, err, db.ErrNotFound)
}
