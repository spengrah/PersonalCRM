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

	"personal-crm/backend/internal/accelerated"
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
	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// meeting_note.* ingest integration tests
//
// Each test seeds its own session UUIDs + per-run sourceIDPrefix so
// parallel runs don't stomp each other. Cleanup hard-deletes by prefix.
// ----------------------------------------------------------------------------

type meetingNoteIngestEnv struct {
	router         *gin.Engine
	apiKey         string
	database       *db.Database
	macService     *service.MacHostService
	externalRepo   *repository.ExternalContactRepository
	meetingRepo    *repository.MeetingNoteRepository
	contactRepo    *repository.ContactRepository
	calendarRepo   *repository.CalendarEventRepository
	interactionRep *repository.InteractionRepository
	identityRepo   *repository.IdentityRepository
	pairedHostID   uuid.UUID
	pairedHostKey  string
	sourceIDPrefix string // included in synthetic data so cleanup is targeted
	// Seeded CRM contacts for tests that need a resolved tagged human.
	contactA uuid.UUID // associated with anarlogHumanA via email match
	contactB uuid.UUID // associated with anarlogHumanB via email match
	contactC uuid.UUID // associated with anarlogHumanC via email match (used for RS2 drift)
}

func setupMeetingNoteIngestEnv(t *testing.T) *meetingNoteIngestEnv {
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
	meetingRepo := repository.NewMeetingNoteRepository(database.Queries)
	macService := service.NewMacHostService(hostRepo, pairingRepo, syncRepo, contactMethodRepo, externalRepo, meetingRepo, database.Pool, 4)

	identityRepo := repository.NewIdentityRepository(database.Queries)
	identityService := service.NewIdentityService(identityRepo)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	calendarRepo := repository.NewCalendarEventRepository(database.Queries)

	eventRepo := repository.NewEventRepository(database.Queries)
	eventBus := events.NewBus(database.Pool, nil, eventRepo)

	// ContactService is constructed without a CadenceUpdater here; the
	// inline meeting_note handler calls RecordInteractionTx with
	// publishesEvent=false which would normally require cadence. Wire a
	// minimal cadence updater for completeness so the tests exercise the
	// full path the production wiring uses.
	contactSvc := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo, eventBus, service.NewRematchService())
	contactSvc.SetCadenceUpdater(wireCadenceUpdaterForAPITest(t, database, contactSvc))

	ingestService := service.NewIngestService(
		database,
		eventBus,
		identityService,
		nil, // messagesRepo unused
		nil, // riverClient unused
		externalRepo,
		hostRepo,
		meetingRepo,
		calendarRepo,
		interactionRepo,
		identityRepo,
		contactSvc,
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

	// Import handler wired so TestMeetingNote_ImportBackfillResolvesOnResync
	// exercises the real /imports/:id/link path (the handler invokes
	// backfillAnarlogIdentity which links the anarlog_human_id identity
	// row to the imported contact).
	enrichmentRepo := repository.NewEnrichmentRepository(database.Queries)
	matchService := service.NewImportMatchService(contactRepo)
	enrichmentSvc := service.NewEnrichmentService(database, contactRepo, contactMethodRepo, enrichmentRepo, nil, nil)
	enrichmentSvc.SetCadenceUpdater(wireCadenceUpdaterForAPITest(t, database, contactSvc))
	importHandler := handlers.NewImportHandler(externalRepo, identityRepo, contactSvc, matchService, enrichmentSvc)
	imports := router.Group("/api/v1/imports")
	imports.POST("/candidates/:id/link", importHandler.LinkContact)

	plain, _, err := macService.CreatePairingToken(ctx)
	require.NoError(t, err)
	pair, err := macService.PairWithToken(ctx, plain, "meeting-note-test", "0.1.0", 1)
	require.NoError(t, err)

	suffix := uuid.NewString()[:8]
	env := &meetingNoteIngestEnv{
		router:         router,
		apiKey:         cfg.External.APIKey,
		database:       database,
		macService:     macService,
		externalRepo:   externalRepo,
		meetingRepo:    meetingRepo,
		contactRepo:    contactRepo,
		calendarRepo:   calendarRepo,
		interactionRep: interactionRepo,
		identityRepo:   identityRepo,
		pairedHostID:   pair.HostID,
		pairedHostKey:  pair.APIKey,
		sourceIDPrefix: "mn-ingest-" + suffix + "-",
	}

	// Seed three CRM contacts each with a synthetic email that the
	// meeting_note tests use to resolve anarlog_humans candidates into
	// CRM contacts via the existing email-match path.
	for i, slot := range []*uuid.UUID{&env.contactA, &env.contactB, &env.contactC} {
		c, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: fmt.Sprintf("MN Test Contact %d %s", i, suffix),
		})
		require.NoError(t, err)
		*slot = c.ID
		_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
			ContactID: c.ID,
			Type:      "email",
			Value:     fmt.Sprintf("mn-test-%d-%s@example.invalid", i, suffix),
		})
		require.NoError(t, err)
	}

	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Drop interactions seeded by these tests (covers anarlog session refs).
		_ = interactionRepo.HardDeleteInteractionsBySourceRefPrefix(cleanCtx, "anarlog_sessions", "anarlog:"+env.sourceIDPrefix+"%")
		_ = interactionRepo.HardDeleteInteractionsBySourceRefPrefix(cleanCtx, "anarlog_sessions", "anarlog:%")
		// Drop meeting_note rows seeded by these tests. Session UUIDs
		// are caller-generated random UUIDs (no exploitable prefix), so
		// scope by the paired mac_host_id this test created.
		_ = meetingRepo.TestHardDeleteByHostID(cleanCtx, env.pairedHostID)
		// Drop synthetic anarlog_humans external_contact rows.
		_ = database.Queries.TestDeleteExternalContactsBySourceIDPrefix(cleanCtx, env.sourceIDPrefix)
		_, _ = database.Queries.DeleteExternalIdentitiesBySource(cleanCtx, "anarlog_humans")
		_, _ = database.Queries.DeleteEventsBySource(cleanCtx, "anarlog_sessions")
		_, _ = database.Queries.DeleteEventsBySource(cleanCtx, "anarlog_humans")
		// Drop synthetic calendar events seeded by these tests.
		_ = database.Queries.TestDeleteCalendarEventsByGcalEventIDPrefix(cleanCtx, env.sourceIDPrefix+"cal-")
		// Drop seeded contacts + their methods.
		for _, id := range []uuid.UUID{env.contactA, env.contactB, env.contactC} {
			_ = contactMethodRepo.DeleteContactMethodsByContact(cleanCtx, id)
			_ = contactRepo.HardDeleteContact(cleanCtx, id)
		}
		_, _ = database.Queries.DeleteAllMacHosts(cleanCtx)
		_, _ = database.Queries.DeleteAllPairingTokens(cleanCtx)
		database.Close()
	})

	return env
}

// seedAnarlogHumanResolvingTo upserts an anarlog_humans external_contact
// (and the anarlog_human_id identity row + link to the contact) by
// posting via the ingest endpoint. The email passed in must match a
// contact_method on the target contactID so the existing email-match
// path wires crm_contact_id. Returns the synthetic anarlog UUID used as
// SourceID.
func (e *meetingNoteIngestEnv) seedAnarlogHumanResolvingTo(t *testing.T, email string) string {
	t.Helper()
	anarlogID := uuid.NewString()
	dn := "Tagged Human " + anarlogID[:8]
	p := events.ExternalContactUpsertedPayload{
		Version:     1,
		HostID:      e.pairedHostID,
		Source:      "anarlog_humans",
		EntityID:    anarlogID,
		DisplayName: &dn,
		Emails:      []events.ExternalContactMethodValue{{Value: email}},
	}
	pBytes, err := events.Marshal(events.KindExternalContactUpserted, p)
	require.NoError(t, err)
	srcID := computeUpsertSourceID(t, anarlogID, pBytes)
	body := map[string]any{
		"events": []any{
			map[string]any{
				"source":      "anarlog_humans",
				"source_id":   srcID,
				"kind":        string(events.KindExternalContactUpserted),
				"payload":     json.RawMessage(pBytes),
				"observed_at": accelerated.GetCurrentTime(),
			},
		},
	}
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/events", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mac-Host-ID", e.pairedHostID.String())
	req.Header.Set("Authorization", "Bearer "+e.pairedHostKey)
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted, "anarlog_humans upsert must be accepted")
	return anarlogID
}

// seedCalendarEventInWindow inserts a calendar_event row centered at
// the given start time with the given matched_contact_ids. Returns the
// inserted event ID.
func (e *meetingNoteIngestEnv) seedCalendarEventInWindow(t *testing.T, startTime time.Time, matchedContactIDs []uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	upserted, err := e.calendarRepo.Upsert(ctx, repository.UpsertCalendarEventRequest{
		GcalEventID:       e.sourceIDPrefix + "cal-" + suffix,
		GcalCalendarID:    "primary",
		GoogleAccountID:   "test-account",
		Title:             stringPtr(e.sourceIDPrefix + "Test Event"),
		StartTime:         startTime,
		EndTime:           startTime.Add(time.Hour),
		Status:            "confirmed",
		Attendees:         []repository.Attendee{},
		MatchedContactIDs: matchedContactIDs,
		SyncedAt:          accelerated.GetCurrentTime(),
	})
	require.NoError(t, err)
	return upserted.ID
}

// buildMNRecordedEvent constructs a meeting_note.recorded envelope ready
// for POST. The session_id, meeting_at, title, and participant_ids are
// caller-controlled.
func buildMNRecordedEvent(t *testing.T, hostID uuid.UUID, sessionUUID string, meetingAt time.Time, title string, participantIDs []string) map[string]any {
	t.Helper()
	tp := title
	p := events.MeetingNoteRecordedPayload{
		Version:        1,
		HostID:         hostID,
		Source:         "anarlog_sessions",
		SourceID:       sessionUUID,
		Title:          &tp,
		MeetingAt:      meetingAt,
		ParticipantIDs: participantIDs,
	}
	pBytes, err := events.Marshal(events.KindMeetingNoteRecorded, p)
	require.NoError(t, err)
	hashHex, err := service.ComputeContentHash(pBytes)
	require.NoError(t, err)
	return map[string]any{
		"source":      "anarlog_sessions",
		"source_id":   sessionUUID + "@" + hashHex,
		"kind":        string(events.KindMeetingNoteRecorded),
		"payload":     json.RawMessage(pBytes),
		"observed_at": accelerated.GetCurrentTime(),
	}
}

// buildMNDeletedEvent constructs a meeting_note.deleted envelope using
// the supplied last_content_hash (or @unknown sentinel when empty).
func buildMNDeletedEvent(t *testing.T, hostID uuid.UUID, sessionUUID, lastContentHash string) map[string]any {
	t.Helper()
	p := events.MeetingNoteDeletedPayload{
		Version:  1,
		HostID:   hostID,
		Source:   "anarlog_sessions",
		SourceID: sessionUUID,
	}
	pBytes, err := events.Marshal(events.KindMeetingNoteDeleted, p)
	require.NoError(t, err)
	suffix := "unknown"
	if lastContentHash != "" {
		suffix = lastContentHash
	}
	return map[string]any{
		"source":      "anarlog_sessions",
		"source_id":   sessionUUID + "@deleted@" + suffix,
		"kind":        string(events.KindMeetingNoteDeleted),
		"payload":     json.RawMessage(pBytes),
		"observed_at": accelerated.GetCurrentTime(),
	}
}

// ingestMNResponse adds the NeedsAttention field to the base ingest
// response shape so tests can assert on it.
type ingestMNResponse struct {
	Accepted       int                           `json:"accepted"`
	Duplicate      int                           `json:"duplicate"`
	Rejected       int                           `json:"rejected"`
	Errors         []handlers.IngestError        `json:"errors"`
	NeedsAttention []handlers.NeedsAttentionItem `json:"needs_attention"`
}

func parseMNIngestResp(t *testing.T, w *httptest.ResponseRecorder) ingestMNResponse {
	t.Helper()
	var r ingestMNResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&r), "body: %s", w.Body.String())
	return r
}

func postMNIngest(t *testing.T, env *meetingNoteIngestEnv, body any) *httptest.ResponseRecorder {
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

// findMeetingNoteRow looks up a meeting_note row by session UUID via
// the repository's tombstone-aware FOR-UPDATE-equivalent path; we use
// the regular GetMeetingNoteBySessionID which only returns live rows.
// For tombstone-aware lookups we open a tx and call the ForUpdate variant.
func findMeetingNoteRow(t *testing.T, env *meetingNoteIngestEnv, sessionUUID string) *repository.MeetingNote {
	t.Helper()
	ctx := context.Background()
	sid, err := uuid.Parse(sessionUUID)
	require.NoError(t, err)
	tx, err := env.database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	row, err := env.meetingRepo.GetMeetingNoteBySessionIDForUpdateTx(ctx, tx, sid)
	if err == db.ErrNotFound {
		return nil
	}
	require.NoError(t, err)
	return row
}

// listSessionInteractions returns the live session-attributed interactions
// (matching anarlog:<sid>:%) for the given session UUID.
func listSessionInteractions(t *testing.T, env *meetingNoteIngestEnv, sessionUUID string) []repository.Interaction {
	t.Helper()
	ctx := context.Background()
	tx, err := env.database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	out, err := env.interactionRep.ListSessionAttributedInteractionsTx(ctx, tx, "anarlog:"+sessionUUID+":%")
	require.NoError(t, err)
	return out
}

// ----------------------------------------------------------------------------
// TC-L1: 0 candidates + 0 resolved tagged humans → orphan_needs_review,
// no interactions, needs_attention contains the session with reason=orphan.
// ----------------------------------------------------------------------------
func TestMeetingNote_OrphanNoTagged(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	sessionUUID := uuid.NewString()
	meetingAt := time.Date(2026, 5, 1, 14, 30, 0, 0, time.UTC)
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Orphan Session", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Equal(t, 0, resp.Rejected)

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateOrphanNeedsReview, row.LinkageState)
	require.Nil(t, row.LinkedKind)
	require.Nil(t, row.LinkedID)
	require.True(t, meetingAt.Equal(row.MeetingAt))
	require.NotEmpty(t, row.InputHash)
	require.NotNil(t, row.LastContentHash)

	require.Empty(t, listSessionInteractions(t, env, sessionUUID))
	require.Len(t, resp.NeedsAttention, 1)
	require.Equal(t, sessionUUID, resp.NeedsAttention[0].SessionID)
	require.Equal(t, "orphan", resp.NeedsAttention[0].Reason)
}

// ----------------------------------------------------------------------------
// TC-L2: 0 candidates + 2 resolved tagged humans → linked_impromptu, 2
// interactions, no needs_attention.
// ----------------------------------------------------------------------------
func TestMeetingNote_LinkedImpromptu(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	// Resolve the seeded contacts via anarlog_humans seeding.
	suffix := strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-")
	suffix = strings.TrimSuffix(suffix, "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	emailB := fmt.Sprintf("mn-test-1-%s@example.invalid", suffix)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)
	anarlogB := env.seedAnarlogHumanResolvingTo(t, emailB)

	sessionUUID := uuid.NewString()
	meetingAt := time.Date(2026, 5, 1, 15, 0, 0, 0, time.UTC)
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Impromptu Session", []string{anarlogA, anarlogB})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateLinkedImpromptu, row.LinkageState)
	require.Nil(t, row.LinkedKind)

	ixs := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, ixs, 2)
	gotRefs := []string{}
	for _, ix := range ixs {
		require.NotNil(t, ix.SourceRef)
		gotRefs = append(gotRefs, *ix.SourceRef)
		require.Equal(t, "mutual", ix.Direction)
		require.Equal(t, "anarlog_sessions", ix.Source)
		require.True(t, meetingAt.Equal(ix.OccurredAt))
	}
	require.Contains(t, gotRefs, "anarlog:"+sessionUUID+":"+env.contactA.String())
	require.Contains(t, gotRefs, "anarlog:"+sessionUUID+":"+env.contactB.String())
	require.Empty(t, resp.NeedsAttention)
}

// ----------------------------------------------------------------------------
// TC-L4: 1 candidate + no tagged humans → linked, no interactions, no
// needs_attention.
// ----------------------------------------------------------------------------
func TestMeetingNote_OneCandidateNoTagged(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)
	eventID := env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA, env.contactB})

	sessionUUID := uuid.NewString()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Linked Session", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateLinked, row.LinkageState)
	require.NotNil(t, row.LinkedKind)
	require.Equal(t, "event", *row.LinkedKind)
	require.NotNil(t, row.LinkedID)
	require.Equal(t, eventID, *row.LinkedID)
	require.Empty(t, listSessionInteractions(t, env, sessionUUID))
	require.Empty(t, resp.NeedsAttention)
}

// ----------------------------------------------------------------------------
// TC-L6: 1 candidate + tagged human NOT in attendees → linked + walk-in
// interaction.
// ----------------------------------------------------------------------------
func TestMeetingNote_OneCandidateWalkin(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := time.Date(2026, 5, 2, 11, 0, 0, 0, time.UTC)
	// Calendar event has contactA as attendee.
	eventID := env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})

	// Tag contactB (NOT in attendees) → walk-in expected.
	suffix := strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-")
	suffix = strings.TrimSuffix(suffix, "-")
	emailB := fmt.Sprintf("mn-test-1-%s@example.invalid", suffix)
	anarlogB := env.seedAnarlogHumanResolvingTo(t, emailB)

	sessionUUID := uuid.NewString()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Linked + Walkin", []string{anarlogB})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateLinked, row.LinkageState)
	require.NotNil(t, row.LinkedID)
	require.Equal(t, eventID, *row.LinkedID)

	ixs := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, ixs, 1, "exactly one walk-in interaction for contactB")
	require.NotNil(t, ixs[0].SourceRef)
	expectedRef := "anarlog:" + sessionUUID + ":walkin:" + env.contactB.String()
	require.Equal(t, expectedRef, *ixs[0].SourceRef)
}

// ----------------------------------------------------------------------------
// TC-L7: 2+ calendar candidates → conflict_pending, no interactions,
// needs_attention with reason=conflict.
// ----------------------------------------------------------------------------
func TestMeetingNote_TwoCandidatesConflict(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC)
	env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactB})

	sessionUUID := uuid.NewString()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Conflict Session", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)
	require.Nil(t, row.LinkedKind)
	require.Nil(t, row.LinkedID)
	require.Empty(t, listSessionInteractions(t, env, sessionUUID))
	require.Len(t, resp.NeedsAttention, 1)
	require.Equal(t, "conflict", resp.NeedsAttention[0].Reason)
}

// ----------------------------------------------------------------------------
// TC-D1: meeting_note.deleted soft-deletes meeting_note + all
// session-attributed interactions.
// ----------------------------------------------------------------------------
func TestMeetingNote_DeleteCascadesToInteractions(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-")
	suffix = strings.TrimSuffix(suffix, "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)

	sessionUUID := uuid.NewString()
	meetingAt := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	rec := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Will Delete", []string{anarlogA})
	w := postMNIngest(t, env, map[string]any{"events": []any{rec}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)
	require.Len(t, listSessionInteractions(t, env, sessionUUID), 1)

	// Get the content hash to construct a valid delete source_id.
	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.NotNil(t, row.LastContentHash)

	del := buildMNDeletedEvent(t, env.pairedHostID, sessionUUID, *row.LastContentHash)
	w = postMNIngest(t, env, map[string]any{"events": []any{del}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	// Live lookup returns nil; tombstoned row still exists.
	ctx := context.Background()
	sid, _ := uuid.Parse(sessionUUID)
	tx, err := env.database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	liveRow, _ := env.meetingRepo.GetMeetingNoteBySessionIDTx(ctx, tx, sid)
	require.Nil(t, liveRow, "live row must be gone after delete")
	tomb, err := env.meetingRepo.GetTombstonedMeetingNoteBySessionIDTx(ctx, tx, sid)
	require.NoError(t, err)
	require.NotNil(t, tomb, "tombstoned row preserved for audit")
	require.NotNil(t, tomb.DeletedAt)

	// All session interactions tombstoned.
	require.Empty(t, listSessionInteractions(t, env, sessionUUID))
}

// ----------------------------------------------------------------------------
// TC-D2: meeting_note.deleted for unknown session → silent no-op
// (accepted=1, no DB changes).
// ----------------------------------------------------------------------------
func TestMeetingNote_DeleteUnknownSilentNoOp(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	unknownSession := uuid.NewString()
	del := buildMNDeletedEvent(t, env.pairedHostID, unknownSession, "")
	w := postMNIngest(t, env, map[string]any{"events": []any{del}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Equal(t, 0, resp.Rejected)
	require.Nil(t, findMeetingNoteRow(t, env, unknownSession))
}

// ----------------------------------------------------------------------------
// TC-RS1: re-sync with unchanged inputs carries forward; interaction set
// unchanged.
// ----------------------------------------------------------------------------
func TestMeetingNote_ResyncCarryForward(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-")
	suffix = strings.TrimSuffix(suffix, "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)

	sessionUUID := uuid.NewString()
	meetingAt := time.Date(2026, 5, 5, 14, 0, 0, 0, time.UTC)
	rec := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Same Title", []string{anarlogA})
	w := postMNIngest(t, env, map[string]any{"events": []any{rec}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)
	firstIxs := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, firstIxs, 1)
	firstID := firstIxs[0].ID

	// Re-send the SAME payload — bus-dedup will skip the inline handler
	// (live row, content hash unchanged, so not a revive case). Tests
	// the dispatch loop's duplicate guard.
	w = postMNIngest(t, env, map[string]any{"events": []any{rec}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Duplicate)

	// Interaction set unchanged.
	afterIxs := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, afterIxs, 1)
	require.Equal(t, firstID, afterIxs[0].ID)
}

// ----------------------------------------------------------------------------
// TC-RS2: re-sync with changed participant_ids re-runs linkage and
// diffs interactions (existing dropped, new inserted).
// ----------------------------------------------------------------------------
func TestMeetingNote_ResyncDiffsInteractions(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-")
	suffix = strings.TrimSuffix(suffix, "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	emailB := fmt.Sprintf("mn-test-1-%s@example.invalid", suffix)
	emailC := fmt.Sprintf("mn-test-2-%s@example.invalid", suffix)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)
	anarlogB := env.seedAnarlogHumanResolvingTo(t, emailB)
	anarlogC := env.seedAnarlogHumanResolvingTo(t, emailC)

	sessionUUID := uuid.NewString()
	meetingAt := time.Date(2026, 5, 6, 14, 0, 0, 0, time.UTC)

	rec1 := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Initial", []string{anarlogA, anarlogB})
	w := postMNIngest(t, env, map[string]any{"events": []any{rec1}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)
	require.Len(t, listSessionInteractions(t, env, sessionUUID), 2)

	// Swap B for C — input_hash changes, linkage re-runs, diff fires.
	rec2 := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Initial", []string{anarlogA, anarlogC})
	w = postMNIngest(t, env, map[string]any{"events": []any{rec2}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	ixs := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, ixs, 2)
	gotRefs := []string{}
	for _, ix := range ixs {
		gotRefs = append(gotRefs, *ix.SourceRef)
	}
	require.Contains(t, gotRefs, "anarlog:"+sessionUUID+":"+env.contactA.String())
	require.Contains(t, gotRefs, "anarlog:"+sessionUUID+":"+env.contactC.String())
	require.NotContains(t, gotRefs, "anarlog:"+sessionUUID+":"+env.contactB.String())
}

// ----------------------------------------------------------------------------
// Re-sync that changes meeting_at + title (without changing participants)
// refreshes the existing session interactions' occurred_at + description
// instead of leaving them stale.
// ----------------------------------------------------------------------------
func TestMeetingNote_ResyncRefreshesInteractionContent(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-")
	suffix = strings.TrimSuffix(suffix, "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)

	sessionUUID := uuid.NewString()
	firstMeetingAt := time.Date(2026, 5, 20, 14, 0, 0, 0, time.UTC)
	rec1 := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, firstMeetingAt, "First Title", []string{anarlogA})
	w := postMNIngest(t, env, map[string]any{"events": []any{rec1}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	before := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, before, 1)
	require.True(t, firstMeetingAt.Equal(before[0].OccurredAt))
	require.NotNil(t, before[0].Description)
	require.Equal(t, "First Title", *before[0].Description)
	beforeID := before[0].ID

	// Re-sync with a different meeting_at + title. Same participant,
	// same source_ref → the diff loop hits the in-both branch and
	// refreshes occurred_at + description.
	updatedMeetingAt := firstMeetingAt.Add(30 * time.Minute)
	rec2 := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, updatedMeetingAt, "Updated Title", []string{anarlogA})
	w = postMNIngest(t, env, map[string]any{"events": []any{rec2}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	after := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, after, 1, "in-both diff must update, not drop+insert")
	require.Equal(t, beforeID, after[0].ID, "same row updated in place")
	require.True(t, updatedMeetingAt.Equal(after[0].OccurredAt), "occurred_at refreshed")
	require.NotNil(t, after[0].Description)
	require.Equal(t, "Updated Title", *after[0].Description)
}

// ----------------------------------------------------------------------------
// TC-DD1: two anarlog_human_ids mapping to the same CRM
// contact produce exactly ONE interaction.
// ----------------------------------------------------------------------------
func TestMeetingNote_DedupesByContactID(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-")
	suffix = strings.TrimSuffix(suffix, "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	// Two anarlog humans both carrying the SAME email → both resolve to
	// contactA via the existing email-match path.
	anarlog1 := env.seedAnarlogHumanResolvingTo(t, emailA)
	anarlog2 := env.seedAnarlogHumanResolvingTo(t, emailA)
	require.NotEqual(t, anarlog1, anarlog2)

	sessionUUID := uuid.NewString()
	meetingAt := time.Date(2026, 5, 7, 14, 0, 0, 0, time.UTC)
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Dedup Test", []string{anarlog1, anarlog2})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	ixs := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, ixs, 1, "two anarlog humans resolving to the same contact must produce ONE interaction")
	require.Equal(t, "anarlog:"+sessionUUID+":"+env.contactA.String(), *ixs[0].SourceRef)
}

// ----------------------------------------------------------------------------
// TC-IM1 (round-1 P1#4): import flow backfills the anarlog identity row;
// subsequent meeting_note re-sync resolves the human.
// ----------------------------------------------------------------------------
func TestMeetingNote_ImportBackfillResolvesOnResync(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)

	// Seed an anarlog_humans candidate that does NOT match any seeded
	// contact (unique email) so it lands with crm_contact_id=NULL +
	// identity row unmatched.
	unmatchedEmail := "unmatched-" + uuid.NewString()[:8] + "@example.invalid"
	anarlogX := env.seedAnarlogHumanResolvingTo(t, unmatchedEmail)

	// First meeting_note.recorded → orphan_needs_review (unresolved tag,
	// 0 candidates).
	sessionUUID := uuid.NewString()
	meetingAt := time.Date(2026, 5, 8, 14, 0, 0, 0, time.UTC)
	rec1 := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Pre-Import", []string{anarlogX})
	w := postMNIngest(t, env, map[string]any{"events": []any{rec1}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Len(t, resp.NeedsAttention, 1)
	require.Equal(t, "orphan", resp.NeedsAttention[0].Reason)
	require.Empty(t, listSessionInteractions(t, env, sessionUUID))

	// Simulate user import via the actual /imports/:id/link handler
	// endpoint. That handler calls backfillAnarlogIdentity which links
	// the anarlog_human_id identity row to the imported contact —
	// the exact path the production flow takes.
	ctx := context.Background()
	idents, err := env.identityRepo.FindIdentitiesByAnarlogHumanID(ctx, anarlogX)
	require.NoError(t, err)
	require.Len(t, idents, 1, "identity row planted at ingest time")
	require.Nil(t, idents[0].ContactID, "unmatched before import")

	// Find the external_contact row for our anarlog candidate so we can
	// POST to /imports/candidates/<id>/link.
	external, err := env.externalRepo.GetBySource(ctx, "anarlog_humans", anarlogX, nil)
	require.NoError(t, err)
	require.NotNil(t, external)
	linkReqBody, _ := json.Marshal(map[string]any{"crm_contact_id": env.contactA.String()})
	linkReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/imports/candidates/"+external.ID.String()+"/link",
		bytes.NewReader(linkReqBody))
	linkReq.Header.Set("Content-Type", "application/json")
	linkW := httptest.NewRecorder()
	env.router.ServeHTTP(linkW, linkReq)
	require.Equal(t, http.StatusOK, linkW.Code, "body: %s", linkW.Body.String())

	// Verify backfill actually linked the identity row to contactA.
	postIdents, err := env.identityRepo.FindIdentitiesByAnarlogHumanID(ctx, anarlogX)
	require.NoError(t, err)
	require.Len(t, postIdents, 1)
	require.NotNil(t, postIdents[0].ContactID)
	require.Equal(t, env.contactA, *postIdents[0].ContactID,
		"import handler must backfill anarlog_human_id identity")

	// Re-send the SAME payload (identical source_id). The dispatch
	// loop's inline-on-duplicate probe must detect the live row and
	// run the handler so the resolved-set-hash drift drives a re-link.
	// Without this, an import after the first ingest would only take
	// effect on a future content-changing event.
	w = postMNIngest(t, env, map[string]any{"events": []any{rec1}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp2 := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp2.Duplicate, "identical source_id → bus dedup")

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateLinkedImpromptu, row.LinkageState,
		"after import + re-sync, resolved tagged human → linked_impromptu")
	ixs := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, ixs, 1)
	require.Equal(t, "anarlog:"+sessionUUID+":"+env.contactA.String(), *ixs[0].SourceRef)
}

// ----------------------------------------------------------------------------
// TC-RV1 (round-1 P0#1): delete then re-record identical content →
// row revives (deleted_at cleared), interactions reinserted.
// ----------------------------------------------------------------------------
func TestMeetingNote_DeleteThenReviveIdenticalContent(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-")
	suffix = strings.TrimSuffix(suffix, "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)

	sessionUUID := uuid.NewString()
	meetingAt := time.Date(2026, 5, 9, 14, 0, 0, 0, time.UTC)
	rec := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Revive Me", []string{anarlogA})

	// Initial insert.
	w := postMNIngest(t, env, map[string]any{"events": []any{rec}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)
	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.NotNil(t, row.LastContentHash)
	priorHash := *row.LastContentHash

	// Delete.
	del := buildMNDeletedEvent(t, env.pairedHostID, sessionUUID, priorHash)
	w = postMNIngest(t, env, map[string]any{"events": []any{del}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)
	require.Empty(t, listSessionInteractions(t, env, sessionUUID))

	// Re-send the SAME recorded payload — source_id is identical, so
	// bus-dedup hits, but the revive-bypass probe should detect the
	// tombstone and run the handler anyway.
	w = postMNIngest(t, env, map[string]any{"events": []any{rec}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Duplicate, "source_id matched → bus-dedup")
	// But the row revived.
	row = findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Nil(t, row.DeletedAt, "revive cleared deleted_at")
	require.Equal(t, repository.LinkageStateLinkedImpromptu, row.LinkageState)

	// And one fresh interaction.
	ixs := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, ixs, 1)
}

// ----------------------------------------------------------------------------
// TC-A1: external_contact.upserted (source='anarlog_humans') creates
// the external_contact row AND the external_identity row keyed by
// anarlog_human_id.
// ----------------------------------------------------------------------------
func TestMeetingNote_AnarlogHumanIdentityRegistered(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-")
	suffix = strings.TrimSuffix(suffix, "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	anarlogID := env.seedAnarlogHumanResolvingTo(t, emailA)

	// Identity row exists and is linked to contactA via the email-match
	// + anarlog-link path in handleExternalContactUpserted.
	ctx := context.Background()
	idents, err := env.identityRepo.FindIdentitiesByAnarlogHumanID(ctx, anarlogID)
	require.NoError(t, err)
	require.Len(t, idents, 1)
	require.NotNil(t, idents[0].ContactID)
	require.Equal(t, env.contactA, *idents[0].ContactID)
}

// ----------------------------------------------------------------------------
// TC-KI1: /known-ids?source=anarlog_sessions returns session UUIDs from
// meeting_note.
// ----------------------------------------------------------------------------
func TestMeetingNote_KnownIDsForAnarlogSessions(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := time.Date(2026, 5, 10, 14, 0, 0, 0, time.UTC)
	sessions := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	for _, sid := range sessions {
		ev := buildMNRecordedEvent(t, env.pairedHostID, sid, meetingAt, "Known IDs Test", nil)
		w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)
	}

	// GET /known-ids
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/host/"+env.pairedHostID.String()+"/sync/anarlog_sessions/known-ids", nil)
	req.Header.Set("X-Mac-Host-ID", env.pairedHostID.String())
	req.Header.Set("Authorization", "Bearer "+env.pairedHostKey)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	type entry struct {
		SourceID        string  `json:"source_id"`
		LastContentHash *string `json:"last_content_hash"`
	}
	type resp struct {
		Success bool `json:"success"`
		Data    struct {
			IDs []entry `json:"ids"`
		} `json:"data"`
	}
	var r resp
	require.NoError(t, json.NewDecoder(w.Body).Decode(&r), "body: %s", w.Body.String())
	gotIDs := make(map[string]bool)
	for _, e := range r.Data.IDs {
		gotIDs[e.SourceID] = true
	}
	for _, sid := range sessions {
		require.True(t, gotIDs[sid], "expected session %s in known-ids response", sid)
	}
}
