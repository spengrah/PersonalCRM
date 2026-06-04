package api

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/anarlog"
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
	"github.com/jackc/pgx/v5/pgtype"
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
	phoneCallRepo  *repository.PhoneCallRepository
	interactionRep *repository.InteractionRepository
	identityRepo   *repository.IdentityRepository
	pairedHostID   uuid.UUID
	pairedHostKey  string
	sourceIDPrefix string // included in synthetic data so cleanup is targeted
	// Seeded CRM contacts for tests that need a resolved tagged human.
	contactA uuid.UUID // associated with anarlogHumanA via email match
	contactB uuid.UUID // associated with anarlogHumanB via email match
	contactC uuid.UUID // associated with anarlogHumanC via email match (used for RS2 drift)
	// sessionUUIDs records every session UUID a test asks the env to
	// remember via trackSession; t.Cleanup hard-deletes interactions
	// scoped to those UUIDs only (no broad sweeps on the shared DB).
	sessionUUIDs   []string
	sessionUUIDsMu sync.Mutex
	// seededAnarlogIDs tracks anarlog_humans external_contact source_ids
	// (UUID strings) seeded by tests. ValidatePayload requires the IDs
	// to parse as UUIDs so we cannot prefix them for the broader
	// prefix-cleanup; track them explicitly instead.
	seededAnarlogIDs   []string
	seededAnarlogIDsMu sync.Mutex
	// titleTokenPrefixes tracks Q-prefixed synthetic name tokens
	// allocated by tests via newTitleToken. Each prefix is the full
	// 7-char token (e.g., "Qzpqxr"); cleanup uses these to scope
	// contact + display_name deletes precisely to the test's own
	// fixtures even when the shared DB has rows from other tests.
	titleTokenPrefixes   []string
	titleTokenPrefixesMu sync.Mutex
	// seededTitleContactIDs tracks contact_ids seeded specifically for
	// title-matching tests so they can be hard-deleted in cleanup.
	seededTitleContactIDs   []uuid.UUID
	seededTitleContactIDsMu sync.Mutex
}

// trackSession records the session UUID for targeted cleanup.
func (e *meetingNoteIngestEnv) trackSession(sessionUUID string) {
	e.sessionUUIDsMu.Lock()
	defer e.sessionUUIDsMu.Unlock()
	e.sessionUUIDs = append(e.sessionUUIDs, sessionUUID)
}

// newSessionUUID generates a fresh session UUID AND registers it for
// targeted cleanup. Tests must use this in place of uuid.NewString()
// for every session they create so t.Cleanup can scope deletes
// precisely (the shared test DB does not tolerate broad sweeps).
func (e *meetingNoteIngestEnv) newSessionUUID() string {
	sid := uuid.NewString()
	e.trackSession(sid)
	return sid
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

	titleMatcher := anarlog.NewTitleMatcher(contactRepo)
	titleDiscoveryWriter := anarlog.NewDiscoveryWriter(externalRepo)
	phoneCallRepo := repository.NewPhoneCallRepository(database.Queries)
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
		nil, // phoneCalls (writer) unused on meeting_note path
		nil, // contactRecorder unused
		nil, // cadence unused
		nil, // followUp unused
		titleMatcher,
		titleDiscoveryWriter,
		phoneCallRepo, // phone_call linkage candidates (read)
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
	suggestionSvc := service.NewSuggestionService(externalRepo, contactRepo, contactMethodRepo, enrichmentSvc, matchService, database)
	importHandler := handlers.NewImportHandler(externalRepo, identityService, contactSvc, matchService, enrichmentSvc, suggestionSvc)
	imports := router.Group("/api/v1/imports")
	imports.POST("/candidates/:id/link", importHandler.LinkContact)
	imports.GET("/candidates", importHandler.ListImportCandidates)

	// Wire the PR 5 user-driven resolve-link + needs-attention routes
	// behind the global API-key middleware so resolve-link tests
	// exercise the same code path as production.
	meetingNoteLinkageTargets := &mnTestLinkageTargetReader{
		calendarRepo:  calendarRepo,
		phoneCallRepo: phoneCallRepo,
	}
	meetingNoteService := service.NewMeetingNoteService(
		database,
		meetingRepo,
		meetingRepo,
		meetingNoteLinkageTargets,
		identityRepo,
		titleMatcher,
		titleDiscoveryWriter,
		contactSvc,
		contactRepo,
	)
	meetingNoteHandler := handlers.NewMeetingNoteHandler(meetingNoteService)
	// Wiring mirrors production main.go: resolve-link stays under
	// the v1 API-key group; needs-attention sits under the composite
	// IngestAuthMiddleware so the Mac daemon's X-Mac-Host-ID +
	// Bearer pair-key auth can reach the recovery endpoint.
	resolveLinkGroup := router.Group("/api/v1/meeting-notes")
	resolveLinkGroup.Use(auth.APIKeyMiddleware(cfg))
	resolveLinkGroup.POST("/:id/resolve-link", meetingNoteHandler.ResolveLink)

	needsAttentionGroup := router.Group("/api/v1/meeting-notes")
	needsAttentionGroup.Use(auth.IngestAuthMiddleware(
		auth.APIKeyMiddleware(cfg),
		auth.MacHostAuthMiddleware(hostRepo, auth.DefaultPasswordComparator, auth.DefaultMacHostAuthLimiterConfig()),
	))
	needsAttentionGroup.GET("/needs-attention", meetingNoteHandler.ListNeedsAttention)

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
		phoneCallRepo:  phoneCallRepo,
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
		// Drop interactions for the session UUIDs this test created
		// (scoped per-session — no broad anarlog:% sweep). Each test
		// uses env.newSessionUUID() so the slice covers every session
		// the test produced.
		env.sessionUUIDsMu.Lock()
		sessions := append([]string(nil), env.sessionUUIDs...)
		env.sessionUUIDsMu.Unlock()
		for _, sid := range sessions {
			_ = interactionRepo.HardDeleteInteractionsBySourceRefPrefix(cleanCtx, "anarlog_sessions", "anarlog:"+sid+":%")
		}
		// Drop meeting_note rows seeded by these tests. Session UUIDs
		// are caller-generated random UUIDs (no exploitable prefix), so
		// scope by the paired mac_host_id this test created.
		_ = meetingRepo.TestHardDeleteByHostID(cleanCtx, env.pairedHostID)
		// Drop anarlog_humans external_contact rows seeded by this test.
		// ValidatePayload forces the source_id to be a UUID so we cannot
		// share a single prefix; track the IDs explicitly instead.
		env.seededAnarlogIDsMu.Lock()
		anarlogIDs := append([]string(nil), env.seededAnarlogIDs...)
		env.seededAnarlogIDsMu.Unlock()
		for _, aid := range anarlogIDs {
			_ = database.Queries.TestDeleteExternalContactsBySourceIDPrefix(cleanCtx, aid)
		}
		// Synthetic icloud_contacts seed cleanup keeps the prefix sweep
		// for non-UUID source_ids (none in this file today, but harmless).
		_ = database.Queries.TestDeleteExternalContactsBySourceIDPrefix(cleanCtx, env.sourceIDPrefix)
		_, _ = database.Queries.DeleteExternalIdentitiesBySource(cleanCtx, "anarlog_humans")
		_, _ = database.Queries.DeleteEventsBySource(cleanCtx, "anarlog_sessions")
		_, _ = database.Queries.DeleteEventsBySource(cleanCtx, "anarlog_humans")
		// Drop anarlog_title rows seeded by title-matching tests.
		// display_name = TitleCase(token) so the Q-prefix scopes
		// precisely to this test's own fixtures.
		env.titleTokenPrefixesMu.Lock()
		titlePrefixes := append([]string(nil), env.titleTokenPrefixes...)
		env.titleTokenPrefixesMu.Unlock()
		for _, prefix := range titlePrefixes {
			_, _ = database.Queries.DeleteExternalContactsByDisplayNamePrefix(cleanCtx, pgtype.Text{String: prefix, Valid: true})
		}
		// Drop synthetic calendar events seeded by these tests.
		_ = database.Queries.TestDeleteCalendarEventsByGcalEventIDPrefix(cleanCtx, env.sourceIDPrefix+"cal-")
		// Drop synthetic phone_call rows seeded by these tests
		// (phone_call has no soft-delete; scope by the paired mac_host_id).
		_ = phoneCallRepo.HardDeleteByMacHost(cleanCtx, env.pairedHostID)
		// Drop seeded contacts + their methods.
		for _, id := range []uuid.UUID{env.contactA, env.contactB, env.contactC} {
			_ = contactMethodRepo.DeleteContactMethodsByContact(cleanCtx, id)
			_ = contactRepo.HardDeleteContact(cleanCtx, id)
		}
		// Drop title-matching contacts seeded with Q-prefix names.
		env.seededTitleContactIDsMu.Lock()
		titleContactIDs := append([]uuid.UUID(nil), env.seededTitleContactIDs...)
		env.seededTitleContactIDsMu.Unlock()
		for _, id := range titleContactIDs {
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
	e.seededAnarlogIDsMu.Lock()
	e.seededAnarlogIDs = append(e.seededAnarlogIDs, anarlogID)
	e.seededAnarlogIDsMu.Unlock()
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
// inserted event ID. Delegates to seedCalendarEventInWindowWithTitle with
// the default per-run title.
func (e *meetingNoteIngestEnv) seedCalendarEventInWindow(t *testing.T, startTime time.Time, matchedContactIDs []uuid.UUID) uuid.UUID {
	t.Helper()
	return e.seedCalendarEventInWindowWithTitle(t, startTime, e.sourceIDPrefix+"Test Event", matchedContactIDs)
}

// seedCalendarEventInWindowWithTitle inserts a calendar_event row with a
// caller-controlled title (used by coalescing tests, which key on the
// normalized title) and a per-call random GcalEventID so each seeded row
// is a distinct DB row. Returns the inserted event ID.
func (e *meetingNoteIngestEnv) seedCalendarEventInWindowWithTitle(t *testing.T, startTime time.Time, title string, matchedContactIDs []uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	upserted, err := e.calendarRepo.Upsert(ctx, repository.UpsertCalendarEventRequest{
		GcalEventID:       e.sourceIDPrefix + "cal-" + suffix,
		GcalCalendarID:    "primary",
		GoogleAccountID:   "test-account",
		Title:             stringPtr(title),
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

// seedPhoneCallInWindow inserts a phone_call row started at the given
// time, scoped to the env's paired mac_host_id for cleanup, with an
// optional matched_contact_id (the call's peer). Returns the inserted
// phone_call ID. Delegates to seedPhoneCallInWindowFull with the default
// voice/inbound/answered/60s shape.
func (e *meetingNoteIngestEnv) seedPhoneCallInWindow(t *testing.T, startedAt time.Time, peerContactID *uuid.UUID) uuid.UUID {
	t.Helper()
	answered := true
	return e.seedPhoneCallInWindowFull(t, startedAt, "+15550000000", repository.PhoneCallServiceVoice, repository.PhoneCallDirectionInbound, &answered, 60, peerContactID)
}

// seedPhoneCallInWindowFull inserts a phone_call row exposing every
// discriminating field the coalescing rules key on — peerNormalized,
// service, direction, answered, durationSeconds — so tests can drive the
// (PeerNormalized, Service, Direction) partition key and representative
// selection. The synthetic call_unique_id embeds the env's source prefix
// and a per-call random suffix so each seeded row is a distinct DB row,
// cleaned up by mac_host_id. Returns the inserted phone_call ID.
func (e *meetingNoteIngestEnv) seedPhoneCallInWindowFull(t *testing.T, startedAt time.Time, peerNormalized, service, direction string, answered *bool, durationSeconds int32, peerContactID *uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	hostID := e.pairedHostID
	call, err := e.phoneCallRepo.UpsertCall(ctx, repository.UpsertPhoneCallParams{
		CallUniqueID:     e.sourceIDPrefix + "call-" + suffix,
		PeerHandle:       peerNormalized,
		PeerNormalized:   peerNormalized,
		Service:          service,
		Direction:        direction,
		Answered:         answered,
		HasVoicemail:     false,
		DurationSeconds:  durationSeconds,
		StartedAt:        startedAt,
		MatchedContactID: peerContactID,
		MacHostID:        &hostID,
	})
	require.NoError(t, err)
	return call.ID
}

// coalesceWindowAnchor is a fixed far-past instant the coalescing tests
// hang their per-run window bases off of. Far enough from the existing
// time.Date(2026,5,...) conflict-test constants that the ±15-min windows
// never overlap.
var coalesceWindowAnchor = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// uniqueWindowBase returns a deterministic-but-run-unique instant for a
// coalescing test's window, derived from the env's per-run random
// sourceIDPrefix plus a per-test subOffset. Different runs land in
// different windows (sourceIDPrefix is per-run random); within a run,
// subOffsets ≥ 1 (hour) apart keep each test's ±15-min window disjoint.
// This prevents an unrelated row from another test/run polluting the
// window and flipping an expected 2→1 auto-link into a conflict.
func (e *meetingNoteIngestEnv) uniqueWindowBase(subOffset int) time.Time {
	sum := sha256.Sum256([]byte(e.sourceIDPrefix))
	// Spread runs across ~100k hours (~11 years) so distinct runs almost
	// never collide; the per-test subOffset (hours) keeps a run's tests
	// well-separated.
	runHours := int(binary.BigEndian.Uint32(sum[:4]) % 100000)
	return coalesceWindowAnchor.Add(time.Duration(runHours+subOffset) * time.Hour)
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
	sessionUUID := env.newSessionUUID()
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

	sessionUUID := env.newSessionUUID()
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

	sessionUUID := env.newSessionUUID()
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

	sessionUUID := env.newSessionUUID()
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
// TC-L7: 2+ calendar candidates with no tagged humans → conflict_pending
// (empty implied set yields no Step 3 winner). The persisted snapshot
// must include both candidates with overlap_count=0.
// ----------------------------------------------------------------------------
func TestMeetingNote_TwoCandidatesConflict(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC)
	env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactB})

	sessionUUID := env.newSessionUUID()
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

	// PR 5 invariant: conflict_candidates snapshot is persisted on the
	// row. The empty implied set → both entries have overlap_count=0.
	require.NotEmpty(t, row.ConflictCandidates, "snapshot must be persisted on conflict_pending")
	var snap []repository.ConflictCandidateSummary
	require.NoError(t, json.Unmarshal(row.ConflictCandidates, &snap))
	require.Len(t, snap, 2)
	for _, s := range snap {
		require.Equal(t, 0, s.OverlapCount, "no tagged humans → overlap 0")
	}
}

// ----------------------------------------------------------------------------
// TC-D-Step3-StrictWinner: 2 candidates with a tagged participant
// matching one of them yields the strict winner via Step 3 → state=linked.
// ----------------------------------------------------------------------------
func TestMeetingNote_Step3_StrictWinnerViaTagged(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-")
	suffix = strings.TrimSuffix(suffix, "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)

	meetingAt := time.Date(2026, 5, 6, 9, 0, 0, 0, time.UTC)
	winningEvent := env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactB})

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Strict Winner Session", []string{anarlogA})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Empty(t, resp.NeedsAttention, "strict winner auto-links → no needs_attention")

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateLinked, row.LinkageState)
	require.NotNil(t, row.LinkedKind)
	require.Equal(t, "event", *row.LinkedKind)
	require.NotNil(t, row.LinkedID)
	require.Equal(t, winningEvent, *row.LinkedID)
	require.Empty(t, row.ConflictCandidates, "snapshot cleared on auto-link")
	// contactA is already in the winning event's attendees → no walk-in.
	require.Empty(t, listSessionInteractions(t, env, sessionUUID))
}

// ----------------------------------------------------------------------------
// TC-D-Step3-TiedConflict: 2 candidates with tagged matching BOTH yields
// no Step 3 winner; state stays conflict_pending and the snapshot
// records both with overlap_count=1.
// ----------------------------------------------------------------------------
func TestMeetingNote_Step3_TiedOverlapYieldsConflict(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-")
	suffix = strings.TrimSuffix(suffix, "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)

	meetingAt := time.Date(2026, 5, 6, 11, 0, 0, 0, time.UTC)
	// Both events have contactA in attendees → tied at overlap=1.
	env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA, env.contactB})
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactA, env.contactC})

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Tied Session", []string{anarlogA})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Len(t, resp.NeedsAttention, 1)
	require.Equal(t, "conflict", resp.NeedsAttention[0].Reason)

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)
	require.NotEmpty(t, row.ConflictCandidates)
	var snap []repository.ConflictCandidateSummary
	require.NoError(t, json.Unmarshal(row.ConflictCandidates, &snap))
	require.Len(t, snap, 2)
	require.Equal(t, 1, snap[0].OverlapCount)
	require.Equal(t, 1, snap[1].OverlapCount)
}

// ----------------------------------------------------------------------------
// TC-RS5: Re-ingest a conflict_pending row with unchanged inputs →
// carry-forward fires; the conflict_candidates snapshot is preserved
// verbatim. Regression for the P1#3 fix.
// ----------------------------------------------------------------------------
func TestMeetingNote_Step3_ResyncCarryForwardPreservesSnapshot(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := time.Date(2026, 5, 6, 13, 0, 0, 0, time.UTC)
	env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactB})

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Carry Forward Session", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	rowFirst := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, rowFirst)
	require.Equal(t, repository.LinkageStateConflictPending, rowFirst.LinkageState)
	require.NotEmpty(t, rowFirst.ConflictCandidates)
	firstSnapshotBytes := append([]byte(nil), rowFirst.ConflictCandidates...)

	// Re-ingest the SAME event payload. The dispatch path runs the
	// inline-on-duplicate probe → handler executes again with identical
	// hashes → carry-forward branch fires; conflict_candidates is
	// preserved.
	w = postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	rowSecond := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, rowSecond)
	require.Equal(t, repository.LinkageStateConflictPending, rowSecond.LinkageState)
	require.JSONEq(t, string(firstSnapshotBytes), string(rowSecond.ConflictCandidates),
		"carry-forward must preserve conflict_candidates verbatim")
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

	sessionUUID := env.newSessionUUID()
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

	sessionUUID := env.newSessionUUID()
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

	sessionUUID := env.newSessionUUID()
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

	sessionUUID := env.newSessionUUID()
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

	sessionUUID := env.newSessionUUID()
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
// TC-IM1: import flow backfills the anarlog identity row;
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
	sessionUUID := env.newSessionUUID()
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
// TC-RV1: delete then re-record identical content →
// row revives (deleted_at cleared), interactions reinserted.
// ----------------------------------------------------------------------------
func TestMeetingNote_DeleteThenReviveIdenticalContent(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-")
	suffix = strings.TrimSuffix(suffix, "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)

	sessionUUID := env.newSessionUUID()
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
	sessions := []string{env.newSessionUUID(), env.newSessionUUID(), env.newSessionUUID()}
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

// ----------------------------------------------------------------------------
// TC-D3: meeting_note.deleted with hash mismatch is rejected with
// MEETING_NOTE_DELETE_HASH_MISMATCH (no cascade fires).
// ----------------------------------------------------------------------------
func TestMeetingNote_DeleteHashMismatchRejected(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC)
	rec := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Will Mismatch", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{rec}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	// Build a delete envelope with a hash that does NOT match the stored
	// last_content_hash and is NOT the @unknown sentinel.
	wrongHash := strings.Repeat("a", 64)
	del := buildMNDeletedEvent(t, env.pairedHostID, sessionUUID, wrongHash)
	w = postMNIngest(t, env, map[string]any{"events": []any{del}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Rejected)
	require.Len(t, resp.Errors, 1)
	require.Equal(t, "MEETING_NOTE_DELETE_HASH_MISMATCH", resp.Errors[0].Code)

	// Row remains live + tracked.
	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Nil(t, row.DeletedAt, "rejected delete must not tombstone the row")
}

// ----------------------------------------------------------------------------
// TC-OW1: a cross-host meeting_note.deleted is a silent no-op (the host
// that owns the row keeps it live).
// ----------------------------------------------------------------------------
func TestMeetingNote_DeleteCrossHostSilentNoOp(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	// Seed a session owned by env.pairedHostID.
	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)
	rec := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Owned by host A", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{rec}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	priorRow := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, priorRow)
	require.NotNil(t, priorRow.LastContentHash)
	priorHash := *priorRow.LastContentHash

	// Pair a second host (B). The mac_host singleton index requires
	// revoking the first one before pairing; production singleton
	// behavior is what the cross-host guard ultimately defends against.
	// For this test we just need a distinct host_id for the second
	// auth path; revoke + re-pair simulates the scenario.
	ctx := context.Background()
	require.NoError(t, env.macService.RevokeHost(ctx, env.pairedHostID))
	plainB, _, err := env.macService.CreatePairingToken(ctx)
	require.NoError(t, err)
	pairB, err := env.macService.PairWithToken(ctx, plainB, "meeting-note-test-B", "0.1.0", 1)
	require.NoError(t, err)
	require.NotEqual(t, env.pairedHostID, pairB.HostID)

	// Host B tries to delete the row owned by host A. The handler logs
	// a warn + silently no-ops; accepted=1, no DB change.
	delBody := buildMNDeletedEvent(t, pairB.HostID, sessionUUID, priorHash)
	bodyBytes, _ := json.Marshal(map[string]any{"events": []any{delBody}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/events", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mac-Host-ID", pairB.HostID.String())
	req.Header.Set("Authorization", "Bearer "+pairB.APIKey)
	wB := httptest.NewRecorder()
	env.router.ServeHTTP(wB, req)
	require.Equal(t, http.StatusOK, wB.Code, "body: %s", wB.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, wB).Accepted)

	// Row still live + still owned by host A.
	stillRow := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, stillRow)
	require.Nil(t, stillRow.DeletedAt, "cross-host delete must NOT tombstone the row")
	require.NotNil(t, stillRow.MacHostID)
	require.Equal(t, env.pairedHostID, *stillRow.MacHostID, "ownership preserved")
}

// ----------------------------------------------------------------------------
// TC-CR1: concurrent first-inserts for the same session UUID converge
// via ON CONFLICT DO NOTHING + the re-read into the update path.
// Uses two goroutines firing in parallel; a sync.WaitGroup serializes
// the start so both batches' tx.Begin run within ~1ms.
// ----------------------------------------------------------------------------
func TestMeetingNote_ConcurrentFirstInsertConverges(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 15, 14, 0, 0, 0, time.UTC)

	// Build two recorded events for the same session with DIFFERENT
	// content (different titles → different content hashes → different
	// envelope source_ids → both pass bus-dedup → both reach the
	// insert path → one wins, the other falls through to update).
	rec1 := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Concurrent A", nil)
	rec2 := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Concurrent B", nil)

	var wg sync.WaitGroup
	start := make(chan struct{})
	type result struct {
		code int
		resp ingestMNResponse
	}
	results := make(chan result, 2)
	for _, ev := range []map[string]any{rec1, rec2} {
		wg.Add(1)
		go func(payload map[string]any) {
			defer wg.Done()
			<-start // release together
			w := postMNIngest(t, env, map[string]any{"events": []any{payload}})
			results <- result{code: w.Code, resp: parseMNIngestResp(t, w)}
		}(ev)
	}
	close(start)
	wg.Wait()
	close(results)
	for r := range results {
		require.Equal(t, http.StatusOK, r.code, "both batches must succeed")
		require.Equal(t, 1, r.resp.Accepted, "each batch must accept its event (one inserts, the other re-reads + updates)")
		require.Equal(t, 0, r.resp.Rejected, "no per-event rejection on conflict path")
		require.Empty(t, r.resp.Errors, "no errors on conflict path")
	}

	// Exactly ONE live meeting_note row exists.
	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Nil(t, row.DeletedAt)
	// Final state is one of the two payloads (last writer wins on the
	// re-read+update path). Both are acceptable — we just assert no
	// duplicates and no rejection.
	require.Contains(t, []string{"Concurrent A", "Concurrent B"}, *row.Title)
}

// ----------------------------------------------------------------------------
// Title-extraction + discovery integration tests.
//
// These tests use Q-prefixed synthetic alphabetic tokens to avoid trigram
// cross-pollination on the shared test DB (per CLAUDE.md gotcha
// "Integration sub-test reuses identifying names across t.Run blocks").
// Each newTitleToken call yields a unique 7-char alphabetic string that
// passes the extractor's keep regex (^[A-Z][a-zA-Z]{1,29}$) and is rare
// enough that no real contact name in the test DB would collide. The
// 6-letter random suffix has 26^6 ≈ 309M entropy, eliminating
// within-suite collisions.
// ----------------------------------------------------------------------------

// newTitleToken returns a unique 7-char Q-prefixed alphabetic string
// suitable for use as a synthetic name token in title-extraction tests.
// The token is registered in env.titleTokenPrefixes so t.Cleanup can
// scope display_name + contact deletes precisely to the test's own
// fixtures.
func newTitleToken(t *testing.T, env *meetingNoteIngestEnv) string {
	t.Helper()
	var buf [6]byte
	if _, err := cryptorand.Read(buf[:]); err != nil {
		t.Fatalf("newTitleToken: rand.Read: %v", err)
	}
	const letters = "abcdefghijklmnopqrstuvwxyz"
	out := []byte{'Q'}
	for _, x := range buf {
		out = append(out, letters[int(x)%26])
	}
	token := string(out)
	env.titleTokenPrefixesMu.Lock()
	env.titleTokenPrefixes = append(env.titleTokenPrefixes, token)
	env.titleTokenPrefixesMu.Unlock()
	return token
}

// seedTitleContact creates a CRM contact with full_name = name and
// registers it for cleanup. Returns the contact UUID.
func seedTitleContact(t *testing.T, env *meetingNoteIngestEnv, name string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	c, err := env.contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: name})
	require.NoError(t, err)
	env.seededTitleContactIDsMu.Lock()
	env.seededTitleContactIDs = append(env.seededTitleContactIDs, c.ID)
	env.seededTitleContactIDsMu.Unlock()
	return c.ID
}

// computeTestAnarlogTitleSourceID mirrors the discovery writer's
// source_id recipe so tests can look up the deterministic row that the
// writer produced for a (token, session) pair. Defined separately here
// so the test asserts the contract independent of the implementation.
func computeTestAnarlogTitleSourceID(normalizedToken string, sessionUUID uuid.UUID) string {
	var buf bytes.Buffer
	buf.WriteString(normalizedToken)
	buf.WriteString(sessionUUID.String())
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:])
}

// getAnarlogTitleRow fetches the unique anarlog_title row for the
// (normalizedToken, sessionUUID) pair. Returns nil when no row exists.
func getAnarlogTitleRow(t *testing.T, env *meetingNoteIngestEnv, normalizedToken, sessionUUID string) *repository.ExternalContact {
	t.Helper()
	sid, err := uuid.Parse(sessionUUID)
	require.NoError(t, err)
	sourceID := computeTestAnarlogTitleSourceID(normalizedToken, sid)
	row, gerr := env.externalRepo.GetBySource(context.Background(), "anarlog_title", sourceID, nil)
	if gerr != nil {
		return nil
	}
	return row
}

// countAnarlogTitleRowsForToken returns the number of anarlog_title
// rows (live, by display_name prefix) for the given token. Used to
// verify the deterministic source_id keeps re-emit idempotent —
// re-emitting the same (token, session) should NOT create new rows.
func countAnarlogTitleRowsForToken(t *testing.T, env *meetingNoteIngestEnv, tokenDisplay string) int64 {
	t.Helper()
	ctx := context.Background()
	n, err := env.database.Queries.CountExternalContactsByDisplayNamePrefix(ctx, pgtype.Text{String: tokenDisplay, Valid: true})
	require.NoError(t, err)
	return n
}

// ----------------------------------------------------------------------------
// TC-T1 — orphan_with_tags_and_title_match: tagged Alice + title
// "Alice / Bob" where Bob is a CRM contact. State =
// orphan_title_augmented; 2 interactions (Alice tagged-anchor, Bob
// title); NO anarlog_title rows (both tokens matched contacts).
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_OrphanWithTagsAndTitleMatch(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)

	// contactA (tagged via anarlogA) — already in env.contactA via
	// email match. Rename it to a Q-token so the extractor + matcher
	// can pick "QtokenA" up unambiguously.
	tokenA := newTitleToken(t, env)
	tokenB := newTitleToken(t, env)
	ctx := context.Background()
	_, err := env.contactRepo.UpdateContact(ctx, env.contactA, repository.UpdateContactRequest{FullName: tokenA + " Smith"})
	require.NoError(t, err)
	contactB := seedTitleContact(t, env, tokenB+" Jones")

	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 7, 14, 0, 0, 0, time.UTC)
	title := tokenA + " / " + tokenB + " 1:1"
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, title, []string{anarlogA})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Empty(t, resp.Errors, "no per-event rejections")

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateOrphanTitleAugmented, row.LinkageState)

	ixs := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, ixs, 2)
	gotRefs := make([]string, 0, 2)
	for _, ix := range ixs {
		require.NotNil(t, ix.SourceRef)
		gotRefs = append(gotRefs, *ix.SourceRef)
	}
	require.Contains(t, gotRefs, "anarlog:"+sessionUUID+":"+env.contactA.String(),
		"tagged anchor interaction for contactA")
	require.Contains(t, gotRefs, "anarlog:"+sessionUUID+":title:"+contactB.String(),
		"title-derived interaction for contactB")

	// Neither token should produce a discovery row (both matched a CRM
	// contact, so they're not weak candidates).
	require.Nil(t, getAnarlogTitleRow(t, env, strings.ToLower(tokenA), sessionUUID))
	require.Nil(t, getAnarlogTitleRow(t, env, strings.ToLower(tokenB), sessionUUID))
}

// ----------------------------------------------------------------------------
// TC-T2 — orphan_with_tags_and_title_no_match: tagged Alice + title
// "Alice / Carol" where Carol is NOT a CRM contact. State stays
// linked_impromptu (tagged-anchor without title-matched contacts);
// 1 interaction (Alice tagged); 1 anarlog_title row for "carol".
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_OrphanWithTagsAndTitleNoMatch(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)

	tokenA := newTitleToken(t, env) // matches contactA after rename
	tokenC := newTitleToken(t, env) // no CRM contact
	ctx := context.Background()
	_, err := env.contactRepo.UpdateContact(ctx, env.contactA, repository.UpdateContactRequest{FullName: tokenA + " Smith"})
	require.NoError(t, err)

	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 7, 15, 0, 0, 0, time.UTC)
	title := tokenA + " / " + tokenC
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, title, []string{anarlogA})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	// tokenA matched contactA but is already tagged → titleMatched empty
	// → state stays linked_impromptu.
	require.Equal(t, repository.LinkageStateLinkedImpromptu, row.LinkageState)
	require.Len(t, listSessionInteractions(t, env, sessionUUID), 1)

	// tokenC has no contact match → anarlog_title row.
	tcRow := getAnarlogTitleRow(t, env, strings.ToLower(tokenC), sessionUUID)
	require.NotNil(t, tcRow, "anarlog_title row exists for unmatched token")
	require.Equal(t, "anarlog_title", tcRow.Source)
	require.NotNil(t, tcRow.DisplayName)
	require.Equal(t, tokenC, *tcRow.DisplayName, "display_name is title-cased token")
}

// ----------------------------------------------------------------------------
// TC-T3 — orphan_no_tags_with_title_matches: 0 tagged + title with
// CRM-matching tokens → state stays orphan_needs_review, NO interactions
// (spec invariant: title matches need a tagged anchor to produce
// interactions). NO anarlog_title rows (tokens matched).
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_OrphanNoTagsWithTitleMatches(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	tokenA := newTitleToken(t, env)
	tokenB := newTitleToken(t, env)
	contactA := seedTitleContact(t, env, tokenA+" Smith")
	contactB := seedTitleContact(t, env, tokenB+" Jones")
	_ = contactA
	_ = contactB

	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 7, 16, 0, 0, 0, time.UTC)
	title := tokenA + " / " + tokenB
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, title, nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateOrphanNeedsReview, row.LinkageState)
	require.Empty(t, listSessionInteractions(t, env, sessionUUID),
		"no anchor → no interactions even with title matches")
	require.Nil(t, getAnarlogTitleRow(t, env, strings.ToLower(tokenA), sessionUUID))
	require.Nil(t, getAnarlogTitleRow(t, env, strings.ToLower(tokenB), sessionUUID))
}

// ----------------------------------------------------------------------------
// TC-T4 — orphan_no_tags_with_title_unmatched: 0 tagged + title with
// no CRM-matching tokens → state orphan_needs_review, NO interactions,
// anarlog_title rows for BOTH unmatched tokens.
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_OrphanNoTagsWithTitleUnmatched(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	tokenC := newTitleToken(t, env)
	tokenD := newTitleToken(t, env)

	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 7, 17, 0, 0, 0, time.UTC)
	title := tokenC + " / " + tokenD
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, title, nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateOrphanNeedsReview, row.LinkageState)
	require.Empty(t, listSessionInteractions(t, env, sessionUUID))

	cRow := getAnarlogTitleRow(t, env, strings.ToLower(tokenC), sessionUUID)
	require.NotNil(t, cRow)
	dRow := getAnarlogTitleRow(t, env, strings.ToLower(tokenD), sessionUUID)
	require.NotNil(t, dRow)
}

// ----------------------------------------------------------------------------
// TC-T5 — linked_with_title_match: 1 calendar candidate + title with
// a CRM-matching token. State stays linked. NO new interactions
// (linked state's walk-in logic only fires for tagged humans, not
// title matches). NO anarlog_title rows for matched tokens.
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_LinkedWithTitleMatch(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	tokenA := newTitleToken(t, env)
	tokenB := newTitleToken(t, env)
	ctx := context.Background()
	_, err := env.contactRepo.UpdateContact(ctx, env.contactA, repository.UpdateContactRequest{FullName: tokenA + " Smith"})
	require.NoError(t, err)
	_ = seedTitleContact(t, env, tokenB+" Jones")

	meetingAt := time.Date(2026, 5, 8, 10, 0, 0, 0, time.UTC)
	eventID := env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})

	sessionUUID := env.newSessionUUID()
	title := tokenA + " / " + tokenB + " 1:1"
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, title, nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateLinked, row.LinkageState)
	require.NotNil(t, row.LinkedID)
	require.Equal(t, eventID, *row.LinkedID)
	require.Empty(t, listSessionInteractions(t, env, sessionUUID),
		"title matches in linked state must not produce interactions")
	require.Nil(t, getAnarlogTitleRow(t, env, strings.ToLower(tokenA), sessionUUID))
	require.Nil(t, getAnarlogTitleRow(t, env, strings.ToLower(tokenB), sessionUUID))
}

// ----------------------------------------------------------------------------
// TC-T6 — linked_with_title_unmatched: linked state + title with
// unmatched token. NO new interactions. 1 anarlog_title row for the
// unmatched token.
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_LinkedWithTitleUnmatched(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	tokenA := newTitleToken(t, env)
	tokenE := newTitleToken(t, env)
	ctx := context.Background()
	_, err := env.contactRepo.UpdateContact(ctx, env.contactA, repository.UpdateContactRequest{FullName: tokenA + " Smith"})
	require.NoError(t, err)

	meetingAt := time.Date(2026, 5, 8, 11, 0, 0, 0, time.UTC)
	env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})

	sessionUUID := env.newSessionUUID()
	title := tokenA + " / " + tokenE
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, title, nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.Equal(t, repository.LinkageStateLinked, row.LinkageState)
	require.Empty(t, listSessionInteractions(t, env, sessionUUID))
	eRow := getAnarlogTitleRow(t, env, strings.ToLower(tokenE), sessionUUID)
	require.NotNil(t, eRow, "unmatched token in linked state still gets a discovery row")
}

// ----------------------------------------------------------------------------
// TC-T7 — title_matches_drive_step3_strict_winner: 2 candidates whose
// only differentiator is a title-token-matched contact. The Step 3
// implied set includes title-matched contacts per spec §Step 3.1, so
// the candidate whose attendees overlap a title-matched contact wins
// and auto-links. NO discovery rows for matched tokens (gated by
// linked state).
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_StrictWinnerViaTitleToken(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	tokenA := newTitleToken(t, env)
	tokenB := newTitleToken(t, env)
	ctx := context.Background()
	// tokenA matches contactA (who is in event 1 attendees), tokenB
	// matches a brand-new contact not in any event → only event 1
	// gains overlap from the title matches → strict winner.
	_, err := env.contactRepo.UpdateContact(ctx, env.contactA, repository.UpdateContactRequest{FullName: tokenA + " Smith"})
	require.NoError(t, err)
	_ = seedTitleContact(t, env, tokenB+" Jones")

	meetingAt := time.Date(2026, 5, 9, 9, 0, 0, 0, time.UTC)
	winningEvent := env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactB})

	sessionUUID := env.newSessionUUID()
	title := tokenA + " / " + tokenB
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, title, nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.Equal(t, repository.LinkageStateLinked, row.LinkageState,
		"title-matched contact in event 1 makes it the strict Step 3 winner")
	require.NotNil(t, row.LinkedKind)
	require.Equal(t, "event", *row.LinkedKind)
	require.NotNil(t, row.LinkedID)
	require.Equal(t, winningEvent, *row.LinkedID)
	// Linked state suppresses title-derived interactions and discovery
	// rows for matched tokens (same invariant as case 1 in decideLinkage).
	require.Empty(t, listSessionInteractions(t, env, sessionUUID))
	require.Nil(t, getAnarlogTitleRow(t, env, strings.ToLower(tokenA), sessionUUID))
	require.Nil(t, getAnarlogTitleRow(t, env, strings.ToLower(tokenB), sessionUUID))
}

// ----------------------------------------------------------------------------
// TC-T7b — conflict_pending_with_title_unmatched: 2 candidates + title
// with unmatched tokens. NO interactions, 2 anarlog_title rows.
// Confirms unmatched-token discovery fires in conflict_pending state.
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_ConflictPendingWithTitleUnmatched(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	tokenC := newTitleToken(t, env)
	tokenD := newTitleToken(t, env)

	meetingAt := time.Date(2026, 5, 9, 10, 0, 0, 0, time.UTC)
	env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactB})

	sessionUUID := env.newSessionUUID()
	title := tokenC + " / " + tokenD
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, title, nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)
	require.Empty(t, listSessionInteractions(t, env, sessionUUID))
	require.NotNil(t, getAnarlogTitleRow(t, env, strings.ToLower(tokenC), sessionUUID))
	require.NotNil(t, getAnarlogTitleRow(t, env, strings.ToLower(tokenD), sessionUUID))
}

// ----------------------------------------------------------------------------
// TC-T8 — title_dedup_against_tagged: Alice is tagged AND title is
// "Alice". Title-matched contact is deduplicated against tagged so
// only the tagged-anchor interaction lands (no `:title:` interaction
// for the same contact).
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_DedupAgainstTagged(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)

	tokenA := newTitleToken(t, env)
	ctx := context.Background()
	_, err := env.contactRepo.UpdateContact(ctx, env.contactA, repository.UpdateContactRequest{FullName: tokenA + " Smith"})
	require.NoError(t, err)

	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 9, 11, 0, 0, 0, time.UTC)
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, tokenA, []string{anarlogA})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.Equal(t, repository.LinkageStateLinkedImpromptu, row.LinkageState,
		"dedup empties titleMatched → state is linked_impromptu, not orphan_title_augmented")
	ixs := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, ixs, 1)
	require.NotNil(t, ixs[0].SourceRef)
	require.Equal(t, "anarlog:"+sessionUUID+":"+env.contactA.String(), *ixs[0].SourceRef,
		"tagged-anchor source_ref wins over title-derived")
}

// ----------------------------------------------------------------------------
// TC-T9 — resync_title_change_drops_old_interaction. Initial title
// "Alice / Bob" + tagged Carol → 3 interactions (Carol tagged, Alice
// title, Bob title). Re-ingest with title "Alice" + tagged Carol
// (Bob removed): Bob's title interaction soft-deleted; Carol+Alice
// preserved.
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_ResyncTitleChangeDropsOldInteraction(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	emailC := fmt.Sprintf("mn-test-2-%s@example.invalid", suffix)
	anarlogC := env.seedAnarlogHumanResolvingTo(t, emailC)

	tokenA := newTitleToken(t, env)
	tokenB := newTitleToken(t, env)
	contactA := seedTitleContact(t, env, tokenA+" Smith")
	contactB := seedTitleContact(t, env, tokenB+" Jones")

	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 10, 14, 0, 0, 0, time.UTC)
	title1 := tokenA + " / " + tokenB
	ev1 := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, title1, []string{anarlogC})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev1}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)
	row := findMeetingNoteRow(t, env, sessionUUID)
	require.Equal(t, repository.LinkageStateOrphanTitleAugmented, row.LinkageState)
	require.Len(t, listSessionInteractions(t, env, sessionUUID), 3)

	// Re-ingest with title containing only tokenA (Bob removed).
	title2 := tokenA
	ev2 := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, title2, []string{anarlogC})
	w = postMNIngest(t, env, map[string]any{"events": []any{ev2}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	after := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, after, 2, "Bob's title interaction soft-deleted; Carol+Alice preserved")
	gotRefs := make([]string, 0, 2)
	for _, ix := range after {
		gotRefs = append(gotRefs, *ix.SourceRef)
	}
	require.Contains(t, gotRefs, "anarlog:"+sessionUUID+":"+env.contactC.String())
	require.Contains(t, gotRefs, "anarlog:"+sessionUUID+":title:"+contactA.String())
	require.NotContains(t, gotRefs, "anarlog:"+sessionUUID+":title:"+contactB.String())

	row = findMeetingNoteRow(t, env, sessionUUID)
	require.Equal(t, repository.LinkageStateOrphanTitleAugmented, row.LinkageState,
		"still augmented because Alice still title-matches")
}

// ----------------------------------------------------------------------------
// TC-T10 — resync_title_change_keeps_old_discovery_rows. Accept
// leak-through per D6 — stale discovery rows from a renamed title
// persist alongside new rows.
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_ResyncTitleChangeKeepsOldDiscoveryRows(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	emailC := fmt.Sprintf("mn-test-2-%s@example.invalid", suffix)
	anarlogC := env.seedAnarlogHumanResolvingTo(t, emailC)

	tokenF := newTitleToken(t, env) // unmatched
	tokenS := newTitleToken(t, env) // unmatched

	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 10, 15, 0, 0, 0, time.UTC)
	title1 := tokenF // a single unmatched token; extractor produces ["tokenF"]
	ev1 := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, title1, []string{anarlogC})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev1}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)
	require.NotNil(t, getAnarlogTitleRow(t, env, strings.ToLower(tokenF), sessionUUID))

	// Re-ingest with title that has only tokenS (different token).
	title2 := tokenS
	ev2 := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, title2, []string{anarlogC})
	w = postMNIngest(t, env, map[string]any{"events": []any{ev2}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	// BOTH rows exist (stale tokenF + new tokenS).
	require.NotNil(t, getAnarlogTitleRow(t, env, strings.ToLower(tokenF), sessionUUID),
		"stale token row persists per accept-leak-through")
	require.NotNil(t, getAnarlogTitleRow(t, env, strings.ToLower(tokenS), sessionUUID))
}

// ----------------------------------------------------------------------------
// TC-T11 — carry_forward_skips_discovery_writes_only. Re-ingest of
// identical payload hits carry-forward (no NEW anarlog_title rows
// inserted because source_id is deterministic). updated_at MAY be
// bumped on existing rows (Block B runs unconditionally so legacy
// sessions backfill); interaction-diff is skipped.
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_CarryForwardKeepsRows(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	tokenF := newTitleToken(t, env)

	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 10, 16, 0, 0, 0, time.UTC)
	title := tokenF
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, title, nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)
	first := getAnarlogTitleRow(t, env, strings.ToLower(tokenF), sessionUUID)
	require.NotNil(t, first)
	firstID := first.ID

	// Re-send identical payload — bus-dedup skips the inline handler.
	w = postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Duplicate)

	// Exactly ONE row for the (token, session) pair — deterministic
	// source_id makes re-emit an idempotent UPDATE.
	again := getAnarlogTitleRow(t, env, strings.ToLower(tokenF), sessionUUID)
	require.NotNil(t, again)
	require.Equal(t, firstID, again.ID, "same row, not a new one")
}

// ----------------------------------------------------------------------------
// TC-T12 — revive_with_title_re_extracts. Record session with title
// containing tokenA + tagged Carol → orphan_title_augmented. Delete
// the session. Revive with a new title containing tokenB + tagged
// Carol → new orphan_title_augmented state, tokenB's title interaction
// created, tokenA's interaction stays soft-deleted.
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_ReviveReExtracts(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	emailC := fmt.Sprintf("mn-test-2-%s@example.invalid", suffix)
	anarlogC := env.seedAnarlogHumanResolvingTo(t, emailC)

	tokenA := newTitleToken(t, env)
	tokenB := newTitleToken(t, env)
	contactA := seedTitleContact(t, env, tokenA+" Smith")
	contactB := seedTitleContact(t, env, tokenB+" Jones")

	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 11, 14, 0, 0, 0, time.UTC)
	ev1 := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, tokenA, []string{anarlogC})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev1}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)
	require.Len(t, listSessionInteractions(t, env, sessionUUID), 2,
		"Carol tagged + Alice title")

	// Delete.
	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row.LastContentHash)
	del := buildMNDeletedEvent(t, env.pairedHostID, sessionUUID, *row.LastContentHash)
	w = postMNIngest(t, env, map[string]any{"events": []any{del}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)
	require.Empty(t, listSessionInteractions(t, env, sessionUUID))

	// Revive with new title.
	ev2 := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, tokenB, []string{anarlogC})
	w = postMNIngest(t, env, map[string]any{"events": []any{ev2}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	row = findMeetingNoteRow(t, env, sessionUUID)
	require.Equal(t, repository.LinkageStateOrphanTitleAugmented, row.LinkageState)
	after := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, after, 2, "Carol tagged + tokenB title")
	gotRefs := make([]string, 0, 2)
	for _, ix := range after {
		gotRefs = append(gotRefs, *ix.SourceRef)
	}
	require.Contains(t, gotRefs, "anarlog:"+sessionUUID+":"+env.contactC.String())
	require.Contains(t, gotRefs, "anarlog:"+sessionUUID+":title:"+contactB.String())
	require.NotContains(t, gotRefs, "anarlog:"+sessionUUID+":title:"+contactA.String(),
		"prior tokenA title interaction stays soft-deleted")
}

// ----------------------------------------------------------------------------
// TC-T13 — host_ownership_skip_skips_title_work. A second mac host
// pushes a meeting_note.recorded for a session already owned by host
// A. The host-ownership guard returns silent no-op BEFORE title work
// runs, so NO anarlog_title rows are inserted.
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_HostOwnershipSkipsTitleWork(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)

	tokenF := newTitleToken(t, env)

	// Host A creates the session WITHOUT the unmatched token in title.
	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 11, 15, 0, 0, 0, time.UTC)
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Initial", []string{anarlogA})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	// Host B pairs and pushes the SAME session UUID with a title that
	// contains an unmatched token. The host-ownership guard must
	// silently drop the event before any title work runs. The
	// mac_host singleton index requires revoking host A first.
	ctx := context.Background()
	require.NoError(t, env.macService.RevokeHost(ctx, env.pairedHostID))
	plain, _, err := env.macService.CreatePairingToken(ctx)
	require.NoError(t, err)
	pairB, err := env.macService.PairWithToken(ctx, plain, "host-b", "0.1.0", 1)
	require.NoError(t, err)

	titleWithUnmatched := tokenF + " review"
	evB := buildMNRecordedEvent(t, pairB.HostID, sessionUUID, meetingAt, titleWithUnmatched, []string{anarlogA})
	b, err := json.Marshal(map[string]any{"events": []any{evB}})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/events", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mac-Host-ID", pairB.HostID.String())
	req.Header.Set("Authorization", "Bearer "+pairB.APIKey)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// NO anarlog_title row for tokenF — title block didn't run.
	require.Nil(t, getAnarlogTitleRow(t, env, strings.ToLower(tokenF), sessionUUID),
		"host-ownership guard skipped title work")
}

// ----------------------------------------------------------------------------
// TC-T14 — source_id_deterministic. Two separate batches emit the
// same (token, session) → single anarlog_title row.
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_SourceIDDeterministic(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	tokenG := newTitleToken(t, env)

	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 11, 16, 0, 0, 0, time.UTC)
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, tokenG, nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)
	first := getAnarlogTitleRow(t, env, strings.ToLower(tokenG), sessionUUID)
	require.NotNil(t, first)

	// Re-emit same payload (a 2nd batch — bus dedups envelope, but we
	// can also force a second inline run via revive).
	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row.LastContentHash)
	del := buildMNDeletedEvent(t, env.pairedHostID, sessionUUID, *row.LastContentHash)
	w = postMNIngest(t, env, map[string]any{"events": []any{del}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	w = postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Still only ONE row.
	require.Equal(t, int64(1), countAnarlogTitleRowsForToken(t, env, tokenG),
		"deterministic source_id keeps re-emit idempotent")
	again := getAnarlogTitleRow(t, env, strings.ToLower(tokenG), sessionUUID)
	require.NotNil(t, again)
	require.Equal(t, first.ID, again.ID)
}

// ----------------------------------------------------------------------------
// TC-T15 — metadata_shape: every anarlog_title row has the four
// metadata keys the downstream UI depends on.
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_MetadataShape(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	tokenH := newTitleToken(t, env)

	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 11, 17, 0, 0, 0, time.UTC)
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, tokenH, nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	row := getAnarlogTitleRow(t, env, strings.ToLower(tokenH), sessionUUID)
	require.NotNil(t, row)
	for _, key := range []string{"session_uuid", "token_normalized", "token_display", "extracted_at"} {
		_, ok := row.Metadata[key]
		require.True(t, ok, "metadata missing %q: %+v", key, row.Metadata)
	}
	require.Equal(t, sessionUUID, row.Metadata["session_uuid"])
	require.Equal(t, strings.ToLower(tokenH), row.Metadata["token_normalized"])
	require.Equal(t, tokenH, row.Metadata["token_display"])
}

// ----------------------------------------------------------------------------
// TC-T16 — regression_unchanged_for_empty_title: a session with no
// extractable tokens in the title behaves identically to the pre-
// title-parsing path (one tagged interaction, linked_impromptu, no
// anarlog_title rows).
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_RegressionUnchangedForEmptyTitle(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)

	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 11, 18, 0, 0, 0, time.UTC)
	// Title is non-empty but has no extractable tokens (lowercased, no
	// real names) — the extractor returns []string{}.
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "intro sync", []string{anarlogA})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.Equal(t, repository.LinkageStateLinkedImpromptu, row.LinkageState)
	require.Len(t, listSessionInteractions(t, env, sessionUUID), 1)
}

// ----------------------------------------------------------------------------
// TC-T17 — regression_external_contact_ingest: an icloud_contacts
// external_contact.upserted envelope still ingests cleanly.
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_RegressionExternalContactIngest(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	// seedAnarlogHumanResolvingTo exercises the external_contact path
	// (anarlog_humans). If it succeeds, the constructor signature
	// extension didn't break that path.
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	id := env.seedAnarlogHumanResolvingTo(t, emailA)
	require.NotEmpty(t, id)
}

// ----------------------------------------------------------------------------
// TC-T18 — imports_ui_excludes_anarlog_title_rows. After seeding an
// anarlog_title row via meeting_note.recorded, the GET
// /imports/candidates response must NOT include it.
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_ImportsUIExcludesAnarlogTitle(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	tokenI := newTitleToken(t, env)

	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 12, 9, 0, 0, 0, time.UTC)
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, tokenI, nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)
	// Confirm the row IS in the DB (so the test isn't trivially passing).
	require.NotNil(t, getAnarlogTitleRow(t, env, strings.ToLower(tokenI), sessionUUID))

	// GET /api/v1/imports/candidates — the filter must hide it.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/imports/candidates", nil)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	body := rec.Body.String()
	require.NotContains(t, body, tokenI,
		"anarlog_title row leaked into imports candidates response")
	require.NotContains(t, body, "anarlog_title",
		"anarlog_title source string leaked into imports candidates response")
}

// ----------------------------------------------------------------------------
// TC-T19 — count_query_excludes_anarlog_title_rows. The CountUnmatched
// and CountAllUnmatched repository methods must not include
// anarlog_title rows.
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_CountQueryExcludesAnarlogTitle(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	ctx := context.Background()

	tokenJ := newTitleToken(t, env)
	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, tokenJ, nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)
	// Sanity: the row was actually persisted (so the test isn't trivially
	// passing on an empty DB).
	require.NotNil(t, getAnarlogTitleRow(t, env, strings.ToLower(tokenJ), sessionUUID))

	// CountUnmatched("anarlog_title") MUST return 0 — the defense-in-
	// depth `source != 'anarlog_title'` filter zeroes out every row
	// regardless of what other tests in the shared DB are doing. True
	// invariant assertion, resistant to parallel test pollution.
	srcCount, err := env.externalRepo.CountUnmatched(ctx, "anarlog_title", false)
	require.NoError(t, err)
	require.Equal(t, int64(0), srcCount,
		"CountUnmatched(anarlog_title) MUST be 0 — filter excludes all rows")

	// Sanity: the raw row exists in external_contact (so the test
	// isn't trivially passing on an empty DB).
	titlePrefixCount := countAnarlogTitleRowsForToken(t, env, tokenJ)
	require.Equal(t, int64(1), titlePrefixCount,
		"the raw row exists in external_contact (sanity check)")

	// Per-source list MUST be empty (filter excludes all anarlog_title rows).
	imports, err := env.externalRepo.ListUnmatched(ctx, "anarlog_title", 100, 0, false)
	require.NoError(t, err)
	require.Empty(t, imports,
		"ListUnmatched(anarlog_title) MUST be empty — filter excludes all rows")

	// CountAllUnmatched + ListAllUnmatched MUST also exclude the row.
	// We assert by iterating the full unmatched-across-sources list and
	// checking the deterministic source_id we just wrote is absent.
	sid, err := uuid.Parse(sessionUUID)
	require.NoError(t, err)
	wroteSourceID := computeTestAnarlogTitleSourceID(strings.ToLower(tokenJ), sid)
	allImports, err := env.externalRepo.ListAllUnmatched(ctx, 1000, 0, false)
	require.NoError(t, err)
	for _, row := range allImports {
		require.NotEqual(t, "anarlog_title", row.Source,
			"ListAllUnmatched MUST NOT include any anarlog_title rows: %+v", row)
		require.NotEqual(t, wroteSourceID, row.SourceID,
			"ListAllUnmatched MUST NOT include our specific anarlog_title row")
	}
	allCount, err := env.externalRepo.CountAllUnmatched(ctx, false)
	require.NoError(t, err)
	require.GreaterOrEqual(t, allCount, int64(0))
	require.LessOrEqual(t, allCount, int64(len(allImports))+50,
		"CountAllUnmatched must agree with ListAllUnmatched within shared-DB drift bounds")
}

// ----------------------------------------------------------------------------
// TC-T20 — daemon_cannot_push_anarlog_title_source. An
// external_contact.upserted envelope with source='anarlog_title' is
// rejected with PAYLOAD_INVARIANT.
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_DaemonCannotPushAnarlogTitleSource(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	entityID := uuid.NewString()
	dn := "Title Source Spoof"
	p := events.ExternalContactUpsertedPayload{
		Version:     1,
		HostID:      env.pairedHostID,
		Source:      "anarlog_title",
		EntityID:    entityID,
		DisplayName: &dn,
	}
	pBytes, err := events.Marshal(events.KindExternalContactUpserted, p)
	require.NoError(t, err)
	srcID := computeUpsertSourceID(t, entityID, pBytes)
	body := map[string]any{
		"events": []any{
			map[string]any{
				"source":      "anarlog_title",
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
	req.Header.Set("X-Mac-Host-ID", env.pairedHostID.String())
	req.Header.Set("Authorization", "Bearer "+env.pairedHostKey)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	resp := parseMNIngestResp(t, rec)
	require.Equal(t, 0, resp.Accepted, "rejection counts as not-accepted")
	require.GreaterOrEqual(t, resp.Rejected, 1)
	require.NotEmpty(t, resp.Errors)
	require.Equal(t, "PAYLOAD_INVARIANT", resp.Errors[0].Code)
}

// ----------------------------------------------------------------------------
// TC-T21 — contact_rename_invalidates_carry_forward. Tagged Carol +
// title-matched Alice → 2 interactions. Rename Alice's contact so the
// title no longer matches → re-ingest forces re-link → Alice's title
// interaction soft-deleted, Carol's tagged interaction preserved.
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_ContactRenameInvalidatesCarryForward(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	emailC := fmt.Sprintf("mn-test-2-%s@example.invalid", suffix)
	anarlogC := env.seedAnarlogHumanResolvingTo(t, emailC)

	tokenA := newTitleToken(t, env)
	contactA := seedTitleContact(t, env, tokenA+" Smith")

	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 12, 11, 0, 0, 0, time.UTC)
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, tokenA, []string{anarlogC})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)
	require.Len(t, listSessionInteractions(t, env, sessionUUID), 2,
		"Carol tagged + Alice title")

	// Rename contact so the trigram no longer matches the title token.
	// Use a totally unrelated synthetic prefix.
	renameToken := newTitleToken(t, env)
	ctx := context.Background()
	_, err := env.contactRepo.UpdateContact(ctx, contactA, repository.UpdateContactRequest{FullName: renameToken + " Renamed"})
	require.NoError(t, err)

	// Re-ingest identical payload — input_hash unchanged but
	// resolved_set_hash differs (title now matches nothing) → re-link.
	w = postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Duplicate,
		"envelope is bus-dedup'd but the shouldRunInlineOnDuplicate probe runs the handler again")

	after := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, after, 1, "Alice title interaction soft-deleted; Carol preserved")
	require.Equal(t, "anarlog:"+sessionUUID+":"+env.contactC.String(), *after[0].SourceRef)
}

// ----------------------------------------------------------------------------
// TC-T22 — new_same_name_contact_invalidates_carry_forward. Tagged
// Carol + uniquely-matched Alice → 2 interactions. Add a 2nd same-name
// contact → collision-gap drops the title-match → re-ingest forces
// re-link → Alice's title interaction soft-deleted.
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_NewSameNameContactInvalidatesCarryForward(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	emailC := fmt.Sprintf("mn-test-2-%s@example.invalid", suffix)
	anarlogC := env.seedAnarlogHumanResolvingTo(t, emailC)

	tokenA := newTitleToken(t, env)
	_ = seedTitleContact(t, env, tokenA+" Smith")

	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, tokenA, []string{anarlogC})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)
	require.Len(t, listSessionInteractions(t, env, sessionUUID), 2)

	// Add a 2nd contact with the same first-name token → collision-gap
	// drops the match.
	_ = seedTitleContact(t, env, tokenA+" Cooper")

	// Re-ingest.
	w = postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	after := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, after, 1, "Alice title interaction soft-deleted; Carol preserved")
	require.Equal(t, "anarlog:"+sessionUUID+":"+env.contactC.String(), *after[0].SourceRef)
}

// ----------------------------------------------------------------------------
// TC-T23 — previously_ambiguous_now_disambiguated. Two Alices exist;
// tagged Carol + ambiguous title → 1 interaction (Carol only).
// Soft-delete one Alice → re-ingest → unique match → 2 interactions
// (Carol + Alice title).
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_PreviouslyAmbiguousNowDisambiguated(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	emailC := fmt.Sprintf("mn-test-2-%s@example.invalid", suffix)
	anarlogC := env.seedAnarlogHumanResolvingTo(t, emailC)

	tokenA := newTitleToken(t, env)
	contactA1 := seedTitleContact(t, env, tokenA+" Smith")
	contactA2 := seedTitleContact(t, env, tokenA+" Cooper")
	_ = contactA1

	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 12, 13, 0, 0, 0, time.UTC)
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, tokenA, []string{anarlogC})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)
	require.Len(t, listSessionInteractions(t, env, sessionUUID), 1,
		"ambiguous title → only Carol-tagged")

	// Soft-delete contactA2 → match becomes unique.
	ctx := context.Background()
	require.NoError(t, env.contactRepo.SoftDeleteContact(ctx, contactA2))

	// Re-ingest.
	w = postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	after := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, after, 2, "Carol tagged + Alice title (now unique)")
	gotRefs := make([]string, 0, 2)
	for _, ix := range after {
		gotRefs = append(gotRefs, *ix.SourceRef)
	}
	require.Contains(t, gotRefs, "anarlog:"+sessionUUID+":"+env.contactC.String())
	require.Contains(t, gotRefs, "anarlog:"+sessionUUID+":title:"+contactA1.String())
}

// ----------------------------------------------------------------------------
// TC-T24 — legacy_session_backfills_discovery_on_carry_forward.
// Simulates a pre-title-parsing ingest where a meeting_note row exists for a
// title with unmatched tokens but NO companion anarlog_title rows
// (because the prior code didn't write them). Re-ingesting the IDENTICAL
// payload hits the bus-dedup path; the meeting_note inline-on-duplicate
// probe re-enters the handler; hashes are unchanged so carry-forward
// fires; Block B MUST still run unconditionally and backfill the
// discovery row. Regression for the Block-B-runs-unconditionally invariant —
// gating Block B on !carryForward would break this case.
// ----------------------------------------------------------------------------
func TestMeetingNote_TitleMatch_LegacySessionBackfillsDiscoveryOnCarryForward(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	tokenK := newTitleToken(t, env)

	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 12, 14, 0, 0, 0, time.UTC)
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, tokenK, nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted)
	firstRow := getAnarlogTitleRow(t, env, strings.ToLower(tokenK), sessionUUID)
	require.NotNil(t, firstRow, "first ingest creates the discovery row")
	firstRowID := firstRow.ID

	// Simulate the pre-title-parsing state by hard-deleting the discovery row.
	// The meeting_note row's input_hash + resolved_set_hash are
	// unchanged from the original ingest.
	ctx := context.Background()
	rowsDeleted, err := env.database.Queries.DeleteExternalContactsByDisplayNamePrefix(ctx, pgtype.Text{String: tokenK, Valid: true})
	require.NoError(t, err)
	require.GreaterOrEqual(t, rowsDeleted, int64(1), "delete the pre-existing discovery row")
	require.Nil(t, getAnarlogTitleRow(t, env, strings.ToLower(tokenK), sessionUUID),
		"discovery row truly gone before the carry-forward re-ingest")

	// Re-ingest IDENTICAL payload. Bus-dedup hits; the meeting_note
	// shouldRunInlineOnDuplicate probe runs the handler again; carry-
	// forward fires (input_hash + resolved_set_hash both match prior).
	// Block B is gated on UNCONDITIONAL execution per the
	// regression-target invariant, so the discovery row must reappear.
	w = postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Duplicate, "envelope dedups, but the handler still runs on the carry-forward path")
	require.Empty(t, resp.Errors)

	backfilled := getAnarlogTitleRow(t, env, strings.ToLower(tokenK), sessionUUID)
	require.NotNil(t, backfilled,
		"Block B must run on the carry-forward path so legacy sessions backfill discovery rows")
	require.NotEqual(t, firstRowID, backfilled.ID,
		"this is a NEW row (hard-deleted prior) — proves Block B re-emitted via UPSERT")
}

// mnTestLinkageTargetReader adapts the calendar + phone_call repositories
// into the polymorphic service.LinkageTargetReader interface used by
// MeetingNoteService. Mirrors the production adapter in cmd/crm-api/main.go.
type mnTestLinkageTargetReader struct {
	calendarRepo  *repository.CalendarEventRepository
	phoneCallRepo *repository.PhoneCallRepository
}

func (r *mnTestLinkageTargetReader) GetEventByID(ctx context.Context, id uuid.UUID) (*repository.CalendarEvent, error) {
	return r.calendarRepo.GetByID(ctx, id)
}

func (r *mnTestLinkageTargetReader) GetPhoneCallByID(ctx context.Context, id uuid.UUID) (*repository.PhoneCall, error) {
	return r.phoneCallRepo.GetCallByID(ctx, id)
}

func (r *mnTestLinkageTargetReader) GetEventByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.CalendarEvent, error) {
	return r.calendarRepo.GetByIDTx(ctx, tx, id)
}

func (r *mnTestLinkageTargetReader) GetPhoneCallByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.PhoneCall, error) {
	return r.phoneCallRepo.GetCallByIDTx(ctx, tx, id)
}

// postResolveLink posts to the resolve-link endpoint with the given
// body and returns the response recorder.
func postResolveLink(t *testing.T, env *meetingNoteIngestEnv, mnID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/meeting-notes/"+mnID+"/resolve-link", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", env.apiKey)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	return w
}

// getNeedsAttention GETs the needs-attention list, returning the
// response recorder.
func getNeedsAttention(t *testing.T, env *meetingNoteIngestEnv, hostID string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/v1/meeting-notes/needs-attention"
	if hostID != "" {
		path += "?host_id=" + hostID
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-API-Key", env.apiKey)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	return w
}

// TestResolveLink_LinkToEventSuccess — seed a conflict_pending row,
// post action=link with the winning candidate, verify the row transitions
// to linked and the snapshot is cleared.
func TestResolveLink_LinkToEventSuccess(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := time.Date(2026, 5, 7, 9, 0, 0, 0, time.UTC)
	eventA := env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})
	eventB := env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactB})
	_ = eventB

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Resolve Link Session", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)

	w = postResolveLink(t, env, row.ID.String(), map[string]any{
		"action": "link",
		"kind":   "event",
		"id":     eventA.String(),
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	updated := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, updated)
	require.Equal(t, repository.LinkageStateLinked, updated.LinkageState)
	require.NotNil(t, updated.LinkedKind)
	require.Equal(t, "event", *updated.LinkedKind)
	require.NotNil(t, updated.LinkedID)
	require.Equal(t, eventA, *updated.LinkedID)
	require.Empty(t, updated.ConflictCandidates, "snapshot cleared on link")
}

// TestResolveLink_AlreadyLinkedReturns409 — calling resolve-link on a
// row that's not in conflict_pending returns 409.
func TestResolveLink_AlreadyLinkedReturns409(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Already Linked", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateLinked, row.LinkageState, "single candidate auto-links")

	w = postResolveLink(t, env, row.ID.String(), map[string]any{"action": "none_of_these"})
	require.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
}

// TestResolveLink_IDNotInSnapshotReturns400 — picking an event UUID that
// wasn't in the persisted snapshot returns 400.
func TestResolveLink_IDNotInSnapshotReturns400(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := time.Date(2026, 5, 7, 11, 0, 0, 0, time.UTC)
	env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactB})

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Not In Snapshot", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)

	w = postResolveLink(t, env, row.ID.String(), map[string]any{
		"action": "link",
		"kind":   "event",
		"id":     uuid.NewString(), // random UUID not in snapshot
	})
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	require.Contains(t, w.Body.String(), "not one of the recorded candidates")
}

// TestResolveLink_UnknownMeetingNoteReturns404 — resolve on a random UUID
// returns 404.
func TestResolveLink_UnknownMeetingNoteReturns404(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	w := postResolveLink(t, env, uuid.NewString(), map[string]any{"action": "none_of_these"})
	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
}

// TestResolveLink_NoneOfTheseToLinkedImpromptu — conflict_pending row
// with a tagged participant, "none of these" promotes to
// linked_impromptu with one interaction per resolved tagged contact.
func TestResolveLink_NoneOfTheseToLinkedImpromptu(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-")
	suffix = strings.TrimSuffix(suffix, "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)

	meetingAt := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	// Two candidates with no overlap with anarlogA → conflict_pending.
	env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactB})
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactC})

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Impromptu via NoneOfThese", []string{anarlogA})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)
	prevHash := row.ResolvedSetHash

	w = postResolveLink(t, env, row.ID.String(), map[string]any{"action": "none_of_these"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	updated := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, updated)
	require.Equal(t, repository.LinkageStateLinkedImpromptu, updated.LinkageState)
	require.Nil(t, updated.LinkedKind)
	require.Nil(t, updated.LinkedID)
	require.Empty(t, updated.ConflictCandidates)
	// Resolved-set-hash recomputed; new value reflects the implied set.
	require.NotEmpty(t, updated.ResolvedSetHash)
	_ = prevHash // hashes here are equal because the recipe is the same set; covered by D11 matrix.

	ixs := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, ixs, 1, "one interaction per resolved tagged contact")
	require.NotNil(t, ixs[0].SourceRef)
	require.Equal(t, "anarlog:"+sessionUUID+":"+env.contactA.String(), *ixs[0].SourceRef)
}

// TestListNeedsAttention_AcceptsDaemonHostAuth — regression for the
// auth-path bug where needs-attention was mounted under straight
// APIKeyMiddleware and 401-ed the Mac daemon's pair-key auth. The
// route now lives under IngestAuthMiddleware (same composite that
// fronts /ingest/events); this test asserts the daemon's headers
// (X-Mac-Host-ID + Authorization: Bearer <pair-key>) reach the
// handler successfully. If a future edit moves the route back under
// straight APIKeyMiddleware this test will 401 and catch the
// regression.
func TestListNeedsAttention_AcceptsDaemonHostAuth(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	path := "/api/v1/meeting-notes/needs-attention?host_id=" + env.pairedHostID.String()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Mac-Host-ID", env.pairedHostID.String())
	req.Header.Set("Authorization", "Bearer "+env.pairedHostKey)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code,
		"daemon's host-auth headers must reach the needs-attention handler — body: %s",
		w.Body.String())
}

// TestListNeedsAttention_BasicProjection — seed a conflict_pending row
// and verify the needs-attention list includes it with a populated
// candidates array.
func TestListNeedsAttention_BasicProjection(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := time.Date(2026, 5, 7, 13, 0, 0, 0, time.UTC)
	env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactB})

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Needs Attention Session", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Scope the list to this test's mac_host so other parallel tests'
	// rows don't pollute the assertion.
	w = getNeedsAttention(t, env, env.pairedHostID.String())
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp struct {
		Success bool                                 `json:"success"`
		Data    []service.NeedsAttentionItemResponse `json:"data"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	require.True(t, resp.Success)
	require.NotEmpty(t, resp.Data)
	var found *service.NeedsAttentionItemResponse
	for i := range resp.Data {
		if resp.Data[i].AnarlogSessionID.String() == sessionUUID {
			found = &resp.Data[i]
			break
		}
	}
	require.NotNil(t, found, "needs-attention list must contain our seeded row")
	require.Equal(t, repository.LinkageStateConflictPending, found.LinkageState)
	require.Len(t, found.Candidates, 2, "both event candidates projected")
	for _, c := range found.Candidates {
		require.False(t, c.TargetMissing)
		require.NotNil(t, c.Preview)
	}
}

// TestResolveLink_NoneOfTheseFiresDiscoveryUpsert — none_of_these
// re-runs Step 4 and upserts anarlog_title discovery rows for
// unmatched name tokens (P1#8 contract).
func TestResolveLink_NoneOfTheseFiresDiscoveryUpsert(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	tokenC := newTitleToken(t, env) // unmatched → should generate a discovery row

	meetingAt := time.Date(2026, 5, 8, 9, 0, 0, 0, time.UTC)
	env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactB})

	sessionUUID := env.newSessionUUID()
	title := tokenC + " sync"
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, title, nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)

	// The daemon-side ingest path already discovered the token on first
	// ingest (Block B runs unconditionally). Hard-delete it so the
	// resolve-link re-upsert is observable.
	existing := getAnarlogTitleRow(t, env, strings.ToLower(tokenC), sessionUUID)
	require.NotNil(t, existing)
	require.NoError(t, env.database.Queries.TestDeleteExternalContactsBySourceIDPrefix(context.Background(), existing.SourceID))
	require.Nil(t, getAnarlogTitleRow(t, env, strings.ToLower(tokenC), sessionUUID))

	w = postResolveLink(t, env, row.ID.String(), map[string]any{"action": "none_of_these"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	refilled := getAnarlogTitleRow(t, env, strings.ToLower(tokenC), sessionUUID)
	require.NotNil(t, refilled, "none_of_these must re-upsert discovery rows for unmatched title tokens")
}

// TestResolveLink_NoneOfTheseToOrphanNeedsReview — conflict_pending row
// with empty participants and no title matches transitions to
// orphan_needs_review with no interactions.
func TestResolveLink_NoneOfTheseToOrphanNeedsReview(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := time.Date(2026, 5, 8, 10, 0, 0, 0, time.UTC)
	env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactB})

	sessionUUID := env.newSessionUUID()
	// No title, no participants → on none_of_these, Step 4 hits
	// orphan_needs_review.
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)

	w = postResolveLink(t, env, row.ID.String(), map[string]any{"action": "none_of_these"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	updated := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, updated)
	require.Equal(t, repository.LinkageStateOrphanNeedsReview, updated.LinkageState)
	require.Empty(t, updated.ConflictCandidates)
	require.Empty(t, listSessionInteractions(t, env, sessionUUID))
}

// TestResolveLink_TargetMissingReturns404 — pick an event from the
// snapshot whose row has since been hard-deleted; resolve-link returns
// 404 (and the tx rolls back so the meeting_note stays
// conflict_pending).
func TestResolveLink_TargetMissingReturns404(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := time.Date(2026, 5, 8, 11, 0, 0, 0, time.UTC)
	eventA := env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactB})

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Target Missing", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)

	// Hard-delete eventA via the test-only repository helper
	// (calendar_event has no soft-delete column; raw SQL in Go is
	// banned by the CLAUDE.md absolute rule).
	ctx := context.Background()
	require.NoError(t, env.calendarRepo.TestHardDeleteByID(ctx, eventA))

	w = postResolveLink(t, env, row.ID.String(), map[string]any{
		"action": "link",
		"kind":   "event",
		"id":     eventA.String(),
	})
	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	require.Contains(t, w.Body.String(), "linked target no longer exists")

	// Row stays in conflict_pending — tx rolled back.
	after := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, after)
	require.Equal(t, repository.LinkageStateConflictPending, after.LinkageState)
	require.NotEmpty(t, after.ConflictCandidates, "snapshot preserved on rollback")
}

// TestListNeedsAttention_HostFilter — the optional host_id query
// parameter scopes the response.
func TestListNeedsAttention_HostFilter(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactB})

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Host Filter", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// host_id = random uuid → 0 entries from this test's session
	w = getNeedsAttention(t, env, uuid.NewString())
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Success bool                                 `json:"success"`
		Data    []service.NeedsAttentionItemResponse `json:"data"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	for _, item := range resp.Data {
		require.NotEqual(t, sessionUUID, item.AnarlogSessionID.String(),
			"unrelated host filter must exclude this test's row")
	}

	// host_id = this test's mac_host → row is present.
	w = getNeedsAttention(t, env, env.pairedHostID.String())
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp2 struct {
		Success bool                                 `json:"success"`
		Data    []service.NeedsAttentionItemResponse `json:"data"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp2))
	found := false
	for _, item := range resp2.Data {
		if item.AnarlogSessionID.String() == sessionUUID {
			found = true
			break
		}
	}
	require.True(t, found, "matching host filter must include this test's row")
}

// TestListNeedsAttention_TargetMissingProjection — when a candidate's
// target has been hard-deleted, the entry stays in the response with
// target_missing=true and preview=nil.
func TestListNeedsAttention_TargetMissingProjection(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := time.Date(2026, 5, 8, 13, 0, 0, 0, time.UTC)
	eventA := env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactB})

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Stale Target", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Hard-delete eventA so its snapshot entry becomes a stale pointer.
	// Uses the test-only repository helper per CLAUDE.md "no raw SQL
	// in Go" absolute rule.
	ctx := context.Background()
	require.NoError(t, env.calendarRepo.TestHardDeleteByID(ctx, eventA))

	w = getNeedsAttention(t, env, env.pairedHostID.String())
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp struct {
		Success bool                                 `json:"success"`
		Data    []service.NeedsAttentionItemResponse `json:"data"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	var found *service.NeedsAttentionItemResponse
	for i := range resp.Data {
		if resp.Data[i].AnarlogSessionID.String() == sessionUUID {
			found = &resp.Data[i]
			break
		}
	}
	require.NotNil(t, found)
	var missingEntry *service.NeedsAttentionCandidate
	var presentEntry *service.NeedsAttentionCandidate
	for i := range found.Candidates {
		c := &found.Candidates[i]
		if c.TargetMissing {
			missingEntry = c
		} else {
			presentEntry = c
		}
	}
	require.NotNil(t, missingEntry, "stale snapshot entry stays in response")
	require.Nil(t, missingEntry.Preview)
	require.NotNil(t, presentEntry, "non-stale entry still rendered")
	require.NotNil(t, presentEntry.Preview)
}

// TestResolveLink_LinkToPhoneCallSuccess (TC-RL2) — seed a
// conflict_pending row with at least one phone_call candidate in the
// linkage window, resolve via action=link kind=phone_call. The row
// transitions to linked with linked_kind="phone_call" and the snapshot
// is cleared. Exercises the LinkedKindPhoneCall branch of
// resolveToLinked + fetchCandidateAsLinkageTx that previously had ZERO
// integration coverage.
func TestResolveLink_LinkToPhoneCallSuccess(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := time.Date(2026, 5, 9, 9, 0, 0, 0, time.UTC)

	// Two candidates so the daemon-side flow lands in conflict_pending:
	// one calendar event + one phone_call, both inside the ±15-min
	// linkage window. No tagged humans → no implied set → Step 3
	// finds no strict winner.
	env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})
	callID := env.seedPhoneCallInWindow(t, meetingAt.Add(2*time.Minute), &env.contactB)

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Phone Call Resolve", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)
	require.NotEmpty(t, row.ConflictCandidates, "snapshot persisted on conflict_pending")

	w = postResolveLink(t, env, row.ID.String(), map[string]any{
		"action": "link",
		"kind":   "phone_call",
		"id":     callID.String(),
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	updated := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, updated)
	require.Equal(t, repository.LinkageStateLinked, updated.LinkageState)
	require.NotNil(t, updated.LinkedKind)
	require.Equal(t, repository.LinkedKindPhoneCall, *updated.LinkedKind)
	require.NotNil(t, updated.LinkedID)
	require.Equal(t, callID, *updated.LinkedID)
	require.Empty(t, updated.ConflictCandidates, "snapshot cleared on link")
}

// TestResolveLink_PhoneCallTargetMissingReturns404 (TC-RL22) — pick a
// phone_call from the snapshot whose row has since been hard-deleted;
// resolve-link returns 404 and the tx rolls back so the meeting_note
// stays conflict_pending.
func TestResolveLink_PhoneCallTargetMissingReturns404(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := time.Date(2026, 5, 9, 10, 0, 0, 0, time.UTC)
	env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})
	callID := env.seedPhoneCallInWindow(t, meetingAt.Add(2*time.Minute), &env.contactB)

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Phone Call Missing", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)

	// Hard-delete the phone_call so the snapshot entry becomes a stale
	// pointer. Uses the test-only repository helper per the CLAUDE.md
	// "no raw SQL in Go" absolute rule.
	ctx := context.Background()
	pc, err := env.phoneCallRepo.GetCallByID(ctx, callID)
	require.NoError(t, err)
	require.NoError(t, env.phoneCallRepo.HardDeleteByUniqueID(ctx, pc.CallUniqueID))

	w = postResolveLink(t, env, row.ID.String(), map[string]any{
		"action": "link",
		"kind":   "phone_call",
		"id":     callID.String(),
	})
	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	require.Contains(t, w.Body.String(), "linked target no longer exists")

	// Row stays in conflict_pending — tx rolled back.
	after := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, after)
	require.Equal(t, repository.LinkageStateConflictPending, after.LinkageState)
	require.NotEmpty(t, after.ConflictCandidates, "snapshot preserved on rollback")
}

// TestResolveLink_PhoneCallPeerWalkinCorrect (TC-RL24) — regression
// for the P1#5 false-walk-in bug. Setup a conflict_pending row with
// two phone_call candidates (one matching contactA, one matching
// contactB) and tagged participants resolving to [contactA, contactB].
// Resolve to the phone_call whose peer is contactA. Assert exactly ONE
// walk-in interaction for contactB (the non-peer), and NO walk-in for
// contactA (who IS the call's peer). The fix lives in
// LinkageCandidate.ImpliedAttendeeSet(); without it, contactA would
// have been added as a walk-in via the calendar-only attendee path.
func TestResolveLink_PhoneCallPeerWalkinCorrect(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	emailB := fmt.Sprintf("mn-test-1-%s@example.invalid", suffix)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)
	anarlogB := env.seedAnarlogHumanResolvingTo(t, emailB)

	meetingAt := time.Date(2026, 5, 9, 11, 0, 0, 0, time.UTC)
	// Two phone_call candidates so daemon-side lands in conflict_pending
	// (case 2+ in decideLinkage). Each peer matches a different tagged
	// contact, producing overlap=1 for both → no strict winner. The two
	// calls use DISTINCT peer numbers (a different real number resolves to
	// each contact) so the dropped-redial coalescing pass does not collapse
	// them — they are genuinely-different interactions, not a redial pair.
	answered := true
	callA := env.seedPhoneCallInWindowFull(t, meetingAt.Add(1*time.Minute), randomTestPhoneNumber(t), repository.PhoneCallServiceVoice, repository.PhoneCallDirectionInbound, &answered, 60, &env.contactA)
	env.seedPhoneCallInWindowFull(t, meetingAt.Add(2*time.Minute), randomTestPhoneNumber(t), repository.PhoneCallServiceVoice, repository.PhoneCallDirectionInbound, &answered, 60, &env.contactB)

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Peer Walk-In Regression", []string{anarlogA, anarlogB})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState,
		"two equal-overlap phone_call candidates must land in conflict_pending")

	// Resolve to phone_call whose peer = contactA. Step 5 should emit a
	// walk-in for contactB ONLY (contactA is the peer).
	w = postResolveLink(t, env, row.ID.String(), map[string]any{
		"action": "link",
		"kind":   "phone_call",
		"id":     callA.String(),
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	updated := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, updated)
	require.Equal(t, repository.LinkageStateLinked, updated.LinkageState)
	require.NotNil(t, updated.LinkedKind)
	require.Equal(t, repository.LinkedKindPhoneCall, *updated.LinkedKind)

	ixs := listSessionInteractions(t, env, sessionUUID)
	walkinARef := "anarlog:" + sessionUUID + ":walkin:" + env.contactA.String()
	walkinBRef := "anarlog:" + sessionUUID + ":walkin:" + env.contactB.String()
	var walkinASeen, walkinBSeen bool
	for _, ix := range ixs {
		require.NotNil(t, ix.SourceRef)
		switch *ix.SourceRef {
		case walkinARef:
			walkinASeen = true
		case walkinBRef:
			walkinBSeen = true
		}
	}
	require.False(t, walkinASeen, "contactA IS the phone_call peer; must NOT receive a walk-in")
	require.True(t, walkinBSeen, "contactB is NOT the peer; must receive exactly one walk-in")
}

// TestResolveLink_NoneOfTheseToOrphanTitleAugmented (TC-SV9 / TC-RL4)
// — none_of_these on a conflict_pending row with resolved tagged
// participants AND a matched title token transitions to
// orphan_title_augmented with interactions for both the tagged anchor
// and the title-matched contact. The orphan_title_augmented branch on
// the resolve-link path was previously untested.
func TestResolveLink_NoneOfTheseToOrphanTitleAugmented(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)

	// Tagged contact (contactA) + a separately-seeded title-matched
	// contact (contactT) reachable via a Q-token in the title.
	tokenT := newTitleToken(t, env)
	contactT := seedTitleContact(t, env, tokenT+" Brown")

	meetingAt := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	// Two candidates with no overlap with the implied set → conflict_pending.
	env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactB})
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactC})

	sessionUUID := env.newSessionUUID()
	title := tokenT + " sync"
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, title, []string{anarlogA})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)

	w = postResolveLink(t, env, row.ID.String(), map[string]any{"action": "none_of_these"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	updated := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, updated)
	require.Equal(t, repository.LinkageStateOrphanTitleAugmented, updated.LinkageState,
		"resolved tagged + matched title → orphan_title_augmented")
	require.Nil(t, updated.LinkedKind)
	require.Nil(t, updated.LinkedID)
	require.Empty(t, updated.ConflictCandidates)

	ixs := listSessionInteractions(t, env, sessionUUID)
	taggedRef := "anarlog:" + sessionUUID + ":" + env.contactA.String()
	titleRef := "anarlog:" + sessionUUID + ":title:" + contactT.String()
	gotRefs := make([]string, 0, len(ixs))
	for _, ix := range ixs {
		require.NotNil(t, ix.SourceRef)
		gotRefs = append(gotRefs, *ix.SourceRef)
	}
	require.Contains(t, gotRefs, taggedRef, "tagged anchor interaction created")
	require.Contains(t, gotRefs, titleRef, "title-derived interaction created")
}

// resolveLinkResponse is the resolve-link success envelope, sufficient
// to assert on interactions_created.
type resolveLinkResponse struct {
	Success bool `json:"success"`
	Data    struct {
		MeetingNote         json.RawMessage `json:"meeting_note"`
		InteractionsCreated []struct {
			ContactID string `json:"contact_id"`
			SourceRef string `json:"source_ref"`
		} `json:"interactions_created"`
	} `json:"data"`
}

func parseResolveLinkResp(t *testing.T, w *httptest.ResponseRecorder) resolveLinkResponse {
	t.Helper()
	var r resolveLinkResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&r), "body: %s", w.Body.String())
	return r
}

// buildMNRecordedEventWithSummary is buildMNRecordedEvent plus a Summary
// field. Used to drive a content-changing re-sync (different
// last_content_hash) while keeping the matching inputs (title,
// participants, meeting_at → input_hash + resolved_set_hash) unchanged.
func buildMNRecordedEventWithSummary(t *testing.T, hostID uuid.UUID, sessionUUID string, meetingAt time.Time, title, summary string, participantIDs []string) map[string]any {
	t.Helper()
	tp := title
	sm := summary
	p := events.MeetingNoteRecordedPayload{
		Version:        1,
		HostID:         hostID,
		Source:         "anarlog_sessions",
		SourceID:       sessionUUID,
		Title:          &tp,
		Summary:        &sm,
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

// TestResolveLink_OrphanToImpromptu_Success — an orphan_needs_review row
// (no candidates, no resolvable tagged humans) resolves to
// linked_impromptu via "Log as impromptu" (none_of_these). State flips,
// pointers stay nil, and no interaction is created (no resolved
// participant to attach one to). The row also leaves the
// needs-attention list.
func TestResolveLink_OrphanToImpromptu_Success(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 11, 9, 0, 0, 0, time.UTC)
	// No candidate, no participants → orphan_needs_review.
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Orphan To Impromptu", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateOrphanNeedsReview, row.LinkageState, "sanity: lands orphan")

	w = postResolveLink(t, env, row.ID.String(), map[string]any{"action": "none_of_these"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseResolveLinkResp(t, w)
	require.Empty(t, resp.Data.InteractionsCreated, "no resolved participant → no interaction")

	updated := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, updated)
	require.Equal(t, repository.LinkageStateLinkedImpromptu, updated.LinkageState)
	require.Nil(t, updated.LinkedKind)
	require.Nil(t, updated.LinkedID)
	require.Empty(t, updated.ConflictCandidates)
	require.Empty(t, listSessionInteractions(t, env, sessionUUID))

	// The resolved row left the needs-attention set.
	naW := getNeedsAttention(t, env, env.pairedHostID.String())
	require.Equal(t, http.StatusOK, naW.Code, "body: %s", naW.Body.String())
	require.NotContains(t, naW.Body.String(), sessionUUID,
		"resolved orphan must leave the needs-attention list")
}

// TestResolveLink_TerminalStatesReturn409 — the widened IN(...) guard
// must still reject every terminal state. Table-driven over linked,
// linked_impromptu, and orphan_title_augmented (the last shares the
// orphan_ prefix and is the trap a sloppy LIKE 'orphan_%' guard would
// admit). Each → 409.
func TestResolveLink_TerminalStatesReturn409(t *testing.T) {
	cases := []struct {
		name  string
		drive func(t *testing.T, env *meetingNoteIngestEnv) *repository.MeetingNote
	}{
		{
			name: "linked",
			drive: func(t *testing.T, env *meetingNoteIngestEnv) *repository.MeetingNote {
				meetingAt := time.Date(2026, 5, 11, 10, 0, 0, 0, time.UTC)
				env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})
				sessionUUID := env.newSessionUUID()
				ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Terminal Linked", nil)
				w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
				require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
				row := findMeetingNoteRow(t, env, sessionUUID)
				require.NotNil(t, row)
				require.Equal(t, repository.LinkageStateLinked, row.LinkageState, "single candidate auto-links")
				return row
			},
		},
		{
			name: "linked_impromptu",
			drive: func(t *testing.T, env *meetingNoteIngestEnv) *repository.MeetingNote {
				suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
				anarlogA := env.seedAnarlogHumanResolvingTo(t, fmt.Sprintf("mn-test-0-%s@example.invalid", suffix))
				meetingAt := time.Date(2026, 5, 11, 11, 0, 0, 0, time.UTC)
				sessionUUID := env.newSessionUUID()
				ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Terminal Impromptu", []string{anarlogA})
				w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
				require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
				row := findMeetingNoteRow(t, env, sessionUUID)
				require.NotNil(t, row)
				require.Equal(t, repository.LinkageStateLinkedImpromptu, row.LinkageState)
				return row
			},
		},
		{
			name: "orphan_title_augmented",
			drive: func(t *testing.T, env *meetingNoteIngestEnv) *repository.MeetingNote {
				suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
				anarlogA := env.seedAnarlogHumanResolvingTo(t, fmt.Sprintf("mn-test-0-%s@example.invalid", suffix))
				tokenT := newTitleToken(t, env)
				seedTitleContact(t, env, tokenT+" Brown")
				meetingAt := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
				sessionUUID := env.newSessionUUID()
				ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, tokenT+" sync", []string{anarlogA})
				w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
				require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
				row := findMeetingNoteRow(t, env, sessionUUID)
				require.NotNil(t, row)
				require.Equal(t, repository.LinkageStateOrphanTitleAugmented, row.LinkageState,
					"tagged + matched title → orphan_title_augmented")
				return row
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := setupMeetingNoteIngestEnv(t)
			row := tc.drive(t, env)
			w := postResolveLink(t, env, row.ID.String(), map[string]any{"action": "none_of_these"})
			require.Equal(t, http.StatusConflict, w.Code,
				"terminal state %s must 409 — body: %s", tc.name, w.Body.String())
		})
	}
}

// TestResolveLink_OrphanLinkActionRejected — action="link" on an orphan
// (which has no candidate snapshot) is rejected as 400, not 500.
func TestResolveLink_OrphanLinkActionRejected(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 11, 13, 0, 0, 0, time.UTC)
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Orphan Link Rejected", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateOrphanNeedsReview, row.LinkageState)

	w = postResolveLink(t, env, row.ID.String(), map[string]any{
		"action": "link",
		"kind":   "event",
		"id":     uuid.NewString(),
	})
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())

	// State unchanged — the rejected link is a no-op.
	after := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, after)
	require.Equal(t, repository.LinkageStateOrphanNeedsReview, after.LinkageState)
}

// TestResolveLink_OrphanToImpromptu_WithNewlyResolvableParticipant —
// exercises the state↔interaction invariant AND the union-hash
// carry-forward fix. Two DISTINCT contacts: T (the tagged human, made
// resolvable AFTER ingest via import) and M (a title-matched contact,
// resolvable from the title throughout). At ingest no tags resolve → the
// row is orphan_needs_review even though M title-matches. After T is
// imported, "Log as impromptu" creates exactly one interaction (T's
// tagged anchor) and NO :title: interaction for M, and persists the
// union hash so a content-changing re-sync carries linked_impromptu
// forward instead of flipping to orphan_title_augmented.
func TestResolveLink_OrphanToImpromptu_WithNewlyResolvableParticipant(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)

	// M — title-matched contact, distinct from T, resolvable from the
	// title throughout.
	tokenM := newTitleToken(t, env)
	contactM := seedTitleContact(t, env, tokenM+" Vance")

	// T — tagged human that is NOT resolvable at ingest (unique email, no
	// matching contact yet).
	unmatchedEmail := "unmatched-" + uuid.NewString()[:8] + "@example.invalid"
	anarlogT := env.seedAnarlogHumanResolvingTo(t, unmatchedEmail)

	sessionUUID := env.newSessionUUID()
	meetingAt := time.Date(2026, 5, 11, 14, 0, 0, 0, time.UTC)
	title := tokenM + " sync"
	rec := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, title, []string{anarlogT})
	w := postMNIngest(t, env, map[string]any{"events": []any{rec}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// At ingest: T unresolved, title alone doesn't lift the orphan.
	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateOrphanNeedsReview, row.LinkageState,
		"title match alone does not promote a no-tag orphan")
	require.Empty(t, listSessionInteractions(t, env, sessionUUID))

	// Make T resolvable via the real import/link handler (links T's
	// anarlog_human identity to env.contactA).
	ctx := context.Background()
	external, err := env.externalRepo.GetBySource(ctx, "anarlog_humans", anarlogT, nil)
	require.NoError(t, err)
	require.NotNil(t, external)
	linkBody, _ := json.Marshal(map[string]any{"crm_contact_id": env.contactA.String()})
	linkReq := httptest.NewRequest(http.MethodPost,
		"/api/v1/imports/candidates/"+external.ID.String()+"/link",
		bytes.NewReader(linkBody))
	linkReq.Header.Set("Content-Type", "application/json")
	linkW := httptest.NewRecorder()
	env.router.ServeHTTP(linkW, linkReq)
	require.Equal(t, http.StatusOK, linkW.Code, "body: %s", linkW.Body.String())

	// "Log as impromptu" on the orphan.
	w = postResolveLink(t, env, row.ID.String(), map[string]any{"action": "none_of_these"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseResolveLinkResp(t, w)
	require.Len(t, resp.Data.InteractionsCreated, 1, "exactly one interaction — T's tagged anchor")
	require.Equal(t, "anarlog:"+sessionUUID+":"+env.contactA.String(), resp.Data.InteractionsCreated[0].SourceRef)

	updated := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, updated)
	require.Equal(t, repository.LinkageStateLinkedImpromptu, updated.LinkageState)

	// The invariant: no :title: interaction for M (tagged-only rule).
	ixs := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, ixs, 1, "tagged interaction only; no title-derived interaction")
	titleRefM := "anarlog:" + sessionUUID + ":title:" + contactM.String()
	for _, ix := range ixs {
		require.NotNil(t, ix.SourceRef)
		require.NotContains(t, *ix.SourceRef, ":title:", "no title-derived interaction in a forced-impromptu row")
		require.NotEqual(t, titleRefM, *ix.SourceRef)
	}

	// Carry-forward guard (asserted behaviorally): re-ingest with the SAME
	// matching inputs (title, participants, meeting_at → input_hash +
	// union resolved_set_hash unchanged) but a CHANGED summary so
	// last_content_hash differs and a genuine re-ingest runs the
	// carry-forward branch. The row must STAY linked_impromptu and gain no
	// :title: interaction for M. Had the resolve persisted hash(T) only,
	// the re-ingest's hash(T ∪ M) would mismatch, fail carry-forward,
	// re-run decideLinkage, and flip to orphan_title_augmented + create
	// M's :title: interaction.
	rec2 := buildMNRecordedEventWithSummary(t, env.pairedHostID, sessionUUID, meetingAt, title, "content changed for re-sync", []string{anarlogT})
	w = postMNIngest(t, env, map[string]any{"events": []any{rec2}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseMNIngestResp(t, w).Accepted, "changed content → genuine re-ingest, not bus-dedup")

	afterResync := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, afterResync)
	require.Equal(t, repository.LinkageStateLinkedImpromptu, afterResync.LinkageState,
		"union-hash carry-forward preserves the user's impromptu decision")
	resyncIxs := listSessionInteractions(t, env, sessionUUID)
	require.Len(t, resyncIxs, 1, "carry-forward creates no new interaction")
	for _, ix := range resyncIxs {
		require.NotNil(t, ix.SourceRef)
		require.NotContains(t, *ix.SourceRef, ":title:", "no title interaction appears on re-sync")
	}
}

// TestResolveLink_SnapshotMissingReturns422 (TC-SV10) — defensive
// branch: a row in conflict_pending with conflict_candidates NULL must
// fail with ErrResolveLinkSnapshotMissing → 422. The production ingest
// path always atomically writes the snapshot when entering
// conflict_pending, so this scenario only arises from corrupted state
// (or a future migration footgun). Seed the row directly via the
// repository to bypass the normal-flow invariant.
func TestResolveLink_SnapshotMissingReturns422(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	ctx := context.Background()
	sessionUUID := env.newSessionUUID()
	sid, err := uuid.Parse(sessionUUID)
	require.NoError(t, err)

	// Insert a meeting_note row directly into conflict_pending with a
	// nil ConflictCandidates snapshot — the defensive case.
	tx, err := env.database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	hostID := env.pairedHostID
	inserted, err := env.meetingRepo.InsertMeetingNoteTx(ctx, tx, repository.InsertMeetingNoteParams{
		AnarlogSessionID: sid,
		Title:            stringPtr("Snapshot Missing Defensive"),
		MeetingAt:        time.Date(2026, 5, 9, 13, 0, 0, 0, time.UTC),
		Participants:     []string{},
		MacHostID:        &hostID,
		LinkageState:     repository.LinkageStateConflictPending,
		// Hashes must match ^[a-f0-9]{64}$ (sha256 hex) per migration
		// 054's CHECK constraint; the literal values are arbitrary.
		InputHash:          "0000000000000000000000000000000000000000000000000000000000000001",
		ResolvedSetHash:    "0000000000000000000000000000000000000000000000000000000000000002",
		ConflictCandidates: nil, // <-- the invariant violation
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	w := postResolveLink(t, env, inserted.ID.String(), map[string]any{
		"action": "link",
		"kind":   "event",
		"id":     uuid.NewString(),
	})
	require.Equal(t, http.StatusUnprocessableEntity, w.Code, "body: %s", w.Body.String())
	require.Contains(t, w.Body.String(), "snapshot missing")

	// Row stays in conflict_pending — tx rolled back.
	after := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, after)
	require.Equal(t, repository.LinkageStateConflictPending, after.LinkageState)
}

// TestResolveLink_NoneOfTheseThenResyncCarryForwardPreservesChoice
// (TC-RL25) — regression for the P1#2 resolved_set_hash fix.
//  1. Daemon ingest produces a conflict_pending row.
//  2. User picks "none of these" — service recomputes
//     resolved_set_hash from the resolved tagged + title-matched IDs
//     AND persists it via ClearMeetingNoteConflictTx.
//  3. Daemon re-syncs the SAME payload — input_hash unchanged,
//     resolved_set_hash now matches the recomputed value, so the
//     carry-forward branch fires and the user's decision is preserved.
//
// Without the P1#2 fix, the resolved_set_hash on the row would still
// reflect the pre-resolve daemon computation, daemon-side recompute
// would mismatch, and the row would re-enter conflict_pending →
// user's "none of these" decision lost.
func TestResolveLink_NoneOfTheseThenResyncCarryForwardPreservesChoice(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	suffix := strings.TrimSuffix(strings.TrimPrefix(env.sourceIDPrefix, "mn-ingest-"), "-")
	emailA := fmt.Sprintf("mn-test-0-%s@example.invalid", suffix)
	anarlogA := env.seedAnarlogHumanResolvingTo(t, emailA)

	meetingAt := time.Date(2026, 5, 9, 14, 0, 0, 0, time.UTC)
	// Two candidates with no overlap with anarlogA → conflict_pending.
	env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactB})
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactC})

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "None Then Resync", []string{anarlogA})
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)

	// User picks none_of_these → state becomes linked_impromptu and
	// resolved_set_hash is recomputed + persisted.
	w = postResolveLink(t, env, row.ID.String(), map[string]any{"action": "none_of_these"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	afterResolve := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, afterResolve)
	require.Equal(t, repository.LinkageStateLinkedImpromptu, afterResolve.LinkageState)
	resolvedHashAfterResolve := afterResolve.ResolvedSetHash
	require.NotEmpty(t, resolvedHashAfterResolve, "resolved_set_hash recomputed on resolve")
	ixCountAfterResolve := len(listSessionInteractions(t, env, sessionUUID))
	require.Equal(t, 1, ixCountAfterResolve, "one interaction for the tagged contact")

	// Daemon re-syncs the SAME payload — input_hash unchanged, AND
	// resolved_set_hash now matches the recomputed value → carry-forward
	// branch must fire and preserve the user's linked_impromptu state.
	w = postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	afterResync := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, afterResync)
	require.Equal(t, repository.LinkageStateLinkedImpromptu, afterResync.LinkageState,
		"P1#2 regression: re-sync must preserve user's none_of_these decision")
	require.Equal(t, resolvedHashAfterResolve, afterResync.ResolvedSetHash,
		"carry-forward branch preserves resolved_set_hash verbatim")
	require.Empty(t, afterResync.ConflictCandidates,
		"snapshot stays cleared across the carry-forward re-sync")
	// Interaction count unchanged — carry-forward does not re-emit.
	require.Len(t, listSessionInteractions(t, env, sessionUUID), ixCountAfterResolve,
		"carry-forward preserves the prior interaction set; no new rows")
}

// TestResolveLink_LinkThenResyncPreservesUserChoice (TC-RL17) — the
// action="link" mirror of TC-RL25. After a user resolves a
// conflict_pending row by picking one of the candidates, a subsequent
// daemon re-sync with identical inputs must NOT undo the resolution.
// The carry-forward branch fires because input_hash + resolved_set_hash
// are both unchanged (resolved_set_hash never changed — both candidates
// were already in the original snapshot).
func TestResolveLink_LinkThenResyncPreservesUserChoice(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := time.Date(2026, 5, 9, 15, 0, 0, 0, time.UTC)
	eventA := env.seedCalendarEventInWindow(t, meetingAt, []uuid.UUID{env.contactA})
	env.seedCalendarEventInWindow(t, meetingAt.Add(5*time.Minute), []uuid.UUID{env.contactB})

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, "Link Then Resync", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)
	resolvedHashBefore := row.ResolvedSetHash

	// User picks eventA.
	w = postResolveLink(t, env, row.ID.String(), map[string]any{
		"action": "link",
		"kind":   "event",
		"id":     eventA.String(),
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	afterResolve := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, afterResolve)
	require.Equal(t, repository.LinkageStateLinked, afterResolve.LinkageState)
	require.NotNil(t, afterResolve.LinkedID)
	require.Equal(t, eventA, *afterResolve.LinkedID)
	require.Empty(t, afterResolve.ConflictCandidates)
	require.Equal(t, resolvedHashBefore, afterResolve.ResolvedSetHash,
		"action=link preserves resolved_set_hash; ResolveMeetingNoteToLinkedTx does not touch it")

	// Daemon re-syncs the SAME payload. input_hash + resolved_set_hash
	// unchanged → carry-forward branch must fire and keep the user's
	// linked decision pointing at eventA.
	w = postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	afterResync := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, afterResync)
	require.Equal(t, repository.LinkageStateLinked, afterResync.LinkageState,
		"re-sync with unchanged hashes must preserve user's linked choice")
	require.NotNil(t, afterResync.LinkedID)
	require.Equal(t, eventA, *afterResync.LinkedID,
		"linked_id stays pinned to the user's pick across re-sync")
	require.Empty(t, afterResync.ConflictCandidates)
}

// ----------------------------------------------------------------------------
// Candidate coalescing (#370 calendar mirror, #371 phone dropped-redial)
// ----------------------------------------------------------------------------

// randomTestPhoneNumber returns a per-call-unique +1555 number so phone
// coalescing tests do not collide in the shared test DB's global phone
// window query.
func randomTestPhoneNumber(t *testing.T) string {
	t.Helper()
	var buf [4]byte
	_, err := cryptorand.Read(buf[:])
	require.NoError(t, err)
	return fmt.Sprintf("+1555%07d", binary.BigEndian.Uint32(buf[:])%10000000)
}

// assertCalendarWindowExactly opens a read-only tx and asserts the
// calendar linkage finder returns exactly the expected event IDs for the
// ±linkageWindow around meetingAt — a loud backstop against an unrelated
// row polluting the auto-link cases (where ConflictCandidates is empty by
// design so membership cannot be asserted on the persisted row).
func assertCalendarWindowExactly(t *testing.T, env *meetingNoteIngestEnv, meetingAt time.Time, expected ...uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tx, err := env.database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	cands, err := env.calendarRepo.FindLinkageCandidatesTx(ctx, tx, meetingAt.Add(-15*time.Minute), meetingAt.Add(15*time.Minute))
	require.NoError(t, err)
	got := make([]uuid.UUID, 0, len(cands))
	for _, c := range cands {
		got = append(got, c.ID)
	}
	require.ElementsMatch(t, expected, got, "calendar window must contain exactly the seeded rows (no pollutant)")
}

// assertPhoneWindowExactly is the phone analogue of
// assertCalendarWindowExactly.
func assertPhoneWindowExactly(t *testing.T, env *meetingNoteIngestEnv, meetingAt time.Time, expected ...uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tx, err := env.database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	cands, err := env.phoneCallRepo.FindLinkageCandidatesTx(ctx, tx, meetingAt.Add(-15*time.Minute), meetingAt.Add(15*time.Minute))
	require.NoError(t, err)
	got := make([]uuid.UUID, 0, len(cands))
	for _, c := range cands {
		got = append(got, c.ID)
	}
	require.ElementsMatch(t, expected, got, "phone window must contain exactly the seeded rows (no pollutant)")
}

// TestMeetingNote_CalendarCoalesce_SameMeetingMirrored: the same meeting
// mirrored across two calendars/series (same title + start, distinct rows,
// different matched-contact counts) coalesces to the more-attendees
// representative and auto-links — no spurious conflict (#370).
func TestMeetingNote_CalendarCoalesce_SameMeetingMirrored(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := env.uniqueWindowBase(1)
	title := env.sourceIDPrefix + "Mirrored Standup"
	rep := env.seedCalendarEventInWindowWithTitle(t, meetingAt, title, []uuid.UUID{env.contactA, env.contactB})
	dup := env.seedCalendarEventInWindowWithTitle(t, meetingAt, title, []uuid.UUID{env.contactC})

	assertCalendarWindowExactly(t, env, meetingAt, rep, dup)

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, title, nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Empty(t, resp.NeedsAttention, "mirrored meeting coalesces and auto-links → no needs_attention")

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateLinked, row.LinkageState)
	require.NotNil(t, row.LinkedKind)
	require.Equal(t, repository.LinkedKindEvent, *row.LinkedKind)
	require.NotNil(t, row.LinkedID)
	require.Equal(t, rep, *row.LinkedID, "representative is the more-attendees mirror")
	require.Empty(t, row.ConflictCandidates)
}

// TestMeetingNote_CalendarNoCoalesce_DifferentStart_StillConflict: two
// same-title events 1 min apart are NOT coalesced (start differs) and
// stay a conflict — guards against over-coalescing (#370 negative case).
func TestMeetingNote_CalendarNoCoalesce_DifferentStart_StillConflict(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := env.uniqueWindowBase(2)
	title := env.sourceIDPrefix + "Recurring Sync"
	eventA := env.seedCalendarEventInWindowWithTitle(t, meetingAt, title, []uuid.UUID{env.contactA})
	eventB := env.seedCalendarEventInWindowWithTitle(t, meetingAt.Add(time.Minute), title, []uuid.UUID{env.contactB})

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, title, nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Len(t, resp.NeedsAttention, 1)
	require.Equal(t, "conflict", resp.NeedsAttention[0].Reason)

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)
	var snap []repository.ConflictCandidateSummary
	require.NoError(t, json.Unmarshal(row.ConflictCandidates, &snap))
	require.Len(t, snap, 2)
	snapIDs := map[uuid.UUID]struct{}{}
	for _, s := range snap {
		snapIDs[s.ID] = struct{}{}
	}
	require.Contains(t, snapIDs, eventA)
	require.Contains(t, snapIDs, eventB)
}

// TestMeetingNote_PhoneCoalesce_DroppedThenRedial: a dropped attempt
// followed 30s later by the connected redial to the same number coalesces
// to the connected call and auto-links — no spurious conflict (#371).
func TestMeetingNote_PhoneCoalesce_DroppedThenRedial(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := env.uniqueWindowBase(3)
	peer := randomTestPhoneNumber(t)
	dropped := false
	connected := true
	droppedID := env.seedPhoneCallInWindowFull(t, meetingAt, peer, repository.PhoneCallServiceVoice, repository.PhoneCallDirectionInbound, &dropped, 0, nil)
	connectedID := env.seedPhoneCallInWindowFull(t, meetingAt.Add(30*time.Second), peer, repository.PhoneCallServiceVoice, repository.PhoneCallDirectionInbound, &connected, 120, nil)

	assertPhoneWindowExactly(t, env, meetingAt, droppedID, connectedID)

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, env.sourceIDPrefix+"Call Session", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Empty(t, resp.NeedsAttention, "dropped+redial coalesces and auto-links → no needs_attention")

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateLinked, row.LinkageState)
	require.NotNil(t, row.LinkedKind)
	require.Equal(t, repository.LinkedKindPhoneCall, *row.LinkedKind)
	require.NotNil(t, row.LinkedID)
	require.Equal(t, connectedID, *row.LinkedID, "representative is the connected call")
	require.Empty(t, row.ConflictCandidates)
}

// TestMeetingNote_PhoneNoCoalesce_OutsideWindow_StillConflict: two
// same-number calls 6 min apart (> phoneCoalesceWindow) are NOT coalesced
// and stay a conflict (#371 negative case).
func TestMeetingNote_PhoneNoCoalesce_OutsideWindow_StillConflict(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := env.uniqueWindowBase(4)
	peer := randomTestPhoneNumber(t)
	answered := true
	callA := env.seedPhoneCallInWindowFull(t, meetingAt, peer, repository.PhoneCallServiceVoice, repository.PhoneCallDirectionInbound, &answered, 60, nil)
	callB := env.seedPhoneCallInWindowFull(t, meetingAt.Add(6*time.Minute), peer, repository.PhoneCallServiceVoice, repository.PhoneCallDirectionInbound, &answered, 60, nil)

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, env.sourceIDPrefix+"Two Calls Session", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Len(t, resp.NeedsAttention, 1)
	require.Equal(t, "conflict", resp.NeedsAttention[0].Reason)

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)
	var snap []repository.ConflictCandidateSummary
	require.NoError(t, json.Unmarshal(row.ConflictCandidates, &snap))
	require.Len(t, snap, 2)
	snapIDs := map[uuid.UUID]struct{}{}
	for _, s := range snap {
		snapIDs[s.ID] = struct{}{}
	}
	require.Contains(t, snapIDs, callA)
	require.Contains(t, snapIDs, callB)
}

// TestMeetingNote_CrossKind_EventPlusCall_StillConflict: an event and a
// phone_call in the same window are never coalesced across kinds and stay
// a conflict (end-to-end cross-kind guard).
func TestMeetingNote_CrossKind_EventPlusCall_StillConflict(t *testing.T) {
	env := setupMeetingNoteIngestEnv(t)
	meetingAt := env.uniqueWindowBase(5)
	eventID := env.seedCalendarEventInWindowWithTitle(t, meetingAt, env.sourceIDPrefix+"Cross Kind Event", []uuid.UUID{env.contactA})
	peer := randomTestPhoneNumber(t)
	answered := true
	callID := env.seedPhoneCallInWindowFull(t, meetingAt, peer, repository.PhoneCallServiceVoice, repository.PhoneCallDirectionInbound, &answered, 60, nil)

	sessionUUID := env.newSessionUUID()
	ev := buildMNRecordedEvent(t, env.pairedHostID, sessionUUID, meetingAt, env.sourceIDPrefix+"Cross Kind Session", nil)
	w := postMNIngest(t, env, map[string]any{"events": []any{ev}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseMNIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Len(t, resp.NeedsAttention, 1)
	require.Equal(t, "conflict", resp.NeedsAttention[0].Reason)

	row := findMeetingNoteRow(t, env, sessionUUID)
	require.NotNil(t, row)
	require.Equal(t, repository.LinkageStateConflictPending, row.LinkageState)
	var snap []repository.ConflictCandidateSummary
	require.NoError(t, json.Unmarshal(row.ConflictCandidates, &snap))
	require.Len(t, snap, 2)
	snapIDs := map[uuid.UUID]struct{}{}
	for _, s := range snap {
		snapIDs[s.ID] = struct{}{}
	}
	require.Contains(t, snapIDs, eventID)
	require.Contains(t, snapIDs, callID)
}
