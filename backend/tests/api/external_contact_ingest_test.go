package api

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// external_contact.* ingest integration tests
// ----------------------------------------------------------------------------

// extContactIngestEnv bundles the wired stack for external_contact
// ingest integration tests. Smaller than the raw_message env because we
// don't need a River client (external_contact handlers do not enqueue
// aggregator jobs).
type extContactIngestEnv struct {
	router         *gin.Engine
	apiKey         string
	database       *db.Database
	macService     *service.MacHostService
	externalRepo   *repository.ExternalContactRepository
	contactRepo    *repository.ContactRepository
	cmRepo         *repository.ContactMethodRepository
	identityRepo   *repository.IdentityRepository
	eventRepo      *repository.EventRepository
	pairedHostID   uuid.UUID
	pairedHostKey  string
	seededContact  uuid.UUID // contact with email + phone matching test fixtures
	sourceIDPrefix string    // unique prefix per test so cleanup is targeted
}

func setupExtContactIngestEnv(t *testing.T) *extContactIngestEnv {
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
	macService := service.NewMacHostService(hostRepo, pairingRepo, syncRepo, contactMethodRepo, externalRepo, database.Pool, 4)

	identityRepo := repository.NewIdentityRepository(database.Queries)
	identityService := service.NewIdentityService(identityRepo)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)

	eventRepo := repository.NewEventRepository(database.Queries)
	eventBus := events.NewBus(database.Pool, nil, eventRepo) // no river client needed for external_contact path
	ingestService := service.NewIngestService(
		database,
		eventBus,
		identityService,
		nil, // messagesRepo unused on external_contact path
		nil, // riverClient unused
		externalRepo,
		hostRepo, // host-liveness re-check for the FOR UPDATE lock
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
	pair, err := macService.PairWithToken(ctx, plain, "ext-contact-test", "0.1.0", 1)
	require.NoError(t, err)

	// Seed a contact + email + phone. Tests can match these with their
	// upsert payloads. Per-run randomized name avoids cross-test
	// pollution on a shared DB.
	suffix := uuid.NewString()[:8]
	created, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Ext Contact Test " + suffix,
	})
	require.NoError(t, err)
	contactID := created.ID
	_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contactID,
		Type:      "email",
		Value:     "ext-contact-" + suffix + "@example.com",
	})
	require.NoError(t, err)
	_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contactID,
		Type:      "phone",
		Value:     "+1555" + suffix[:7],
	})
	require.NoError(t, err)

	env := &extContactIngestEnv{
		router:         router,
		apiKey:         cfg.External.APIKey,
		database:       database,
		macService:     macService,
		externalRepo:   externalRepo,
		contactRepo:    contactRepo,
		cmRepo:         contactMethodRepo,
		identityRepo:   identityRepo,
		eventRepo:      eventRepo,
		pairedHostID:   pair.HostID,
		pairedHostKey:  pair.APIKey,
		seededContact:  contactID,
		sourceIDPrefix: "ext-ingest-" + suffix + "-",
	}

	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Drop external_contact rows seeded by this test, including
		// tombstoned ones (uses hard DELETE).
		_ = database.Queries.TestDeleteExternalContactsBySourceIDPrefix(cleanCtx, env.sourceIDPrefix)
		// Drop event-log rows under our source.
		_, _ = database.Queries.DeleteEventsBySource(cleanCtx, "icloud_contacts")
		_, _ = database.Queries.DeleteExternalIdentitiesBySource(cleanCtx, "icloud_contacts")
		_ = contactMethodRepo.DeleteContactMethodsByContact(cleanCtx, env.seededContact)
		_ = contactRepo.HardDeleteContact(cleanCtx, env.seededContact)
		_, _ = database.Queries.DeleteAllMacHosts(cleanCtx)
		_, _ = database.Queries.DeleteAllPairingTokens(cleanCtx)
		database.Close()
	})

	return env
}

// canonicalDeleteSourceID synthesizes a valid envelope source_id for a
// deleted event using the spec's @unknown fallback (line 343). Tests
// that need to exercise the stored-hash validation construct the
// suffix explicitly from the seeded row's last_content_hash.
func canonicalDeleteSourceID(entityID string) string {
	return entityID + "@deleted@unknown"
}

// computeUpsertSourceID returns the envelope source_id whose hash
// suffix matches SHA-256(JCS(payload \ {host_id})) — the contract the
// ingest layer's verifier enforces. Use this in every test that
// constructs an upsert envelope so the verifier accepts the event.
func computeUpsertSourceID(t *testing.T, entityID string, payload []byte) string {
	t.Helper()
	hashHex, err := service.ComputeContentHash(payload)
	require.NoError(t, err)
	return entityID + "@" + hashHex
}

func buildExtUpsertEvent(t *testing.T, hostID uuid.UUID, entityID string, mutate func(*events.ExternalContactUpsertedPayload)) map[string]any {
	t.Helper()
	now := accelerated.GetCurrentTime()
	dn := "Sample User"
	p := events.ExternalContactUpsertedPayload{
		Version:     1,
		HostID:      hostID,
		Source:      "icloud_contacts",
		EntityID:    entityID,
		DisplayName: &dn,
		Metadata:    map[string]any{"container_identifier": "test-container"},
	}
	if mutate != nil {
		mutate(&p)
	}
	pBytes, err := events.Marshal(events.KindExternalContactUpserted, p)
	require.NoError(t, err)
	return map[string]any{
		"source":      "icloud_contacts",
		"source_id":   computeUpsertSourceID(t, entityID, pBytes),
		"kind":        string(events.KindExternalContactUpserted),
		"payload":     json.RawMessage(pBytes),
		"observed_at": now,
	}
}

func buildExtDeleteEvent(t *testing.T, hostID uuid.UUID, entityID string) map[string]any {
	t.Helper()
	now := accelerated.GetCurrentTime()
	p := events.ExternalContactDeletedPayload{
		Version:  1,
		HostID:   hostID,
		Source:   "icloud_contacts",
		EntityID: entityID,
	}
	pBytes, err := events.Marshal(events.KindExternalContactDeleted, p)
	require.NoError(t, err)
	return map[string]any{
		"source":      "icloud_contacts",
		"source_id":   canonicalDeleteSourceID(entityID),
		"kind":        string(events.KindExternalContactDeleted),
		"payload":     json.RawMessage(pBytes),
		"observed_at": now,
	}
}

func postIngestExt(t *testing.T, env *extContactIngestEnv, hostID *uuid.UUID, hostKey string, body any) *httptest.ResponseRecorder {
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

// findExtRow looks up an external_contact row by (source=icloud_contacts,
// source_id=entityID). Returns nil/nil-error when the row is absent.
// Tombstone-aware via the existing GetBySource (which deliberately
// returns tombstoned rows so the inline handler can revive them).
func findExtRow(t *testing.T, env *extContactIngestEnv, entityID string) *repository.ExternalContact {
	t.Helper()
	row, err := env.externalRepo.GetBySource(context.Background(), "icloud_contacts", entityID, nil)
	require.NoError(t, err)
	return row
}

func TestIngestExternalContact_Upserted_FirstInsert_NoMatch(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	entityID := env.sourceIDPrefix + "no-match"
	ev := buildExtUpsertEvent(t, env.pairedHostID, entityID, func(p *events.ExternalContactUpsertedPayload) {
		p.Emails = []events.ExternalContactMethodValue{{Value: "nobody-here@example.invalid"}}
	})
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Equal(t, 0, resp.Rejected)

	row := findExtRow(t, env, entityID)
	require.NotNil(t, row)
	require.Nil(t, row.CRMContactID, "no email match → no crm_contact_id")
	require.Equal(t, repository.MatchStatusUnmatched, row.MatchStatus)
	require.Nil(t, row.DeletedAt)
}

func TestIngestExternalContact_Upserted_FirstInsert_EmailMatch(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	entityID := env.sourceIDPrefix + "email-match"

	// Pull the email that was seeded for env.seededContact.
	methods, err := env.cmRepo.ListContactMethodsByContact(context.Background(), env.seededContact)
	require.NoError(t, err)
	var seededEmail string
	for _, m := range methods {
		if m.Type == "email" {
			seededEmail = m.Value
			break
		}
	}
	require.NotEmpty(t, seededEmail)

	ev := buildExtUpsertEvent(t, env.pairedHostID, entityID, func(p *events.ExternalContactUpsertedPayload) {
		p.Emails = []events.ExternalContactMethodValue{{Value: seededEmail}}
	})
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseIngestResp(t, w).Accepted)

	row := findExtRow(t, env, entityID)
	require.NotNil(t, row)
	require.NotNil(t, row.CRMContactID, "email match → crm_contact_id should be set")
	require.Equal(t, env.seededContact, *row.CRMContactID)
	require.Equal(t, repository.MatchStatusMatched, row.MatchStatus)
}

func TestIngestExternalContact_Upserted_ReUpsertPreservesMatchState(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	entityID := env.sourceIDPrefix + "preserve"
	methods, err := env.cmRepo.ListContactMethodsByContact(context.Background(), env.seededContact)
	require.NoError(t, err)
	var seededEmail string
	for _, m := range methods {
		if m.Type == "email" {
			seededEmail = m.Value
			break
		}
	}

	// First insert with a match.
	ev1 := buildExtUpsertEvent(t, env.pairedHostID, entityID, func(p *events.ExternalContactUpsertedPayload) {
		p.Emails = []events.ExternalContactMethodValue{{Value: seededEmail}}
	})
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev1},
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, parseIngestResp(t, w).Accepted)

	row1 := findExtRow(t, env, entityID)
	require.NotNil(t, row1)
	require.NotNil(t, row1.CRMContactID)
	originalCRM := *row1.CRMContactID

	// Re-upsert with a different email (no longer matching the
	// original seeded contact). The external_contact row's match state
	// must be preserved — crm_contact_id stays pinned to the seeded
	// contact even though the new payload's emails don't match it.
	// buildExtUpsertEvent computes a fresh JCS hash from the new
	// payload, so the source_id naturally differs from ev1 (different
	// emails → different canonical bytes → different hash) and the
	// event-log dedup doesn't skip the inline handler.
	ev2 := buildExtUpsertEvent(t, env.pairedHostID, entityID, func(p *events.ExternalContactUpsertedPayload) {
		p.Emails = []events.ExternalContactMethodValue{{Value: "unrelated-edit@example.invalid"}}
	})

	w = postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev2},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseIngestResp(t, w).Accepted)

	row2 := findExtRow(t, env, entityID)
	require.NotNil(t, row2)
	require.NotNil(t, row2.CRMContactID, "re-upsert must preserve crm_contact_id")
	require.Equal(t, originalCRM, *row2.CRMContactID)
	require.Equal(t, repository.MatchStatusMatched, row2.MatchStatus)
}

func TestIngestExternalContact_Upserted_RevivesTombstone(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	entityID := env.sourceIDPrefix + "revive"

	// Plant a live row, tombstone it, then upsert and assert revive.
	ctx := context.Background()
	syncedAt := accelerated.GetCurrentTime()
	first, err := env.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:   "icloud_contacts",
		SourceID: entityID,
		Emails:   []repository.EmailEntry{{Value: "tombstone-seed@example.invalid"}},
		SyncedAt: &syncedAt,
	})
	require.NoError(t, err)
	require.NotNil(t, first)

	// Tombstone via a tx. Use a fresh tx so we exercise SoftDeleteTx.
	tx, err := env.database.Pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, env.externalRepo.SoftDeleteTx(ctx, tx, first.ID))
	require.NoError(t, tx.Commit(ctx))

	pre := findExtRow(t, env, entityID)
	require.NotNil(t, pre)
	require.NotNil(t, pre.DeletedAt, "fixture must be tombstoned before the upsert event")

	// Now post an upsert event.
	ev := buildExtUpsertEvent(t, env.pairedHostID, entityID, func(p *events.ExternalContactUpsertedPayload) {
		p.Emails = []events.ExternalContactMethodValue{{Value: "revived@example.invalid"}}
	})
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseIngestResp(t, w).Accepted)

	post := findExtRow(t, env, entityID)
	require.NotNil(t, post)
	require.Nil(t, post.DeletedAt, "tombstoned row should be revived (deleted_at = NULL)")
}

func TestIngestExternalContact_Deleted_SetsDeletedAt(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	entityID := env.sourceIDPrefix + "delete"

	// Seed: upsert first, then delete.
	upEv := buildExtUpsertEvent(t, env.pairedHostID, entityID, nil)
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{upEv},
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, parseIngestResp(t, w).Accepted)
	require.Nil(t, findExtRow(t, env, entityID).DeletedAt)

	delEv := buildExtDeleteEvent(t, env.pairedHostID, entityID)
	w = postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{delEv},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Equal(t, 0, resp.Rejected)

	row := findExtRow(t, env, entityID)
	require.NotNil(t, row, "soft-delete must preserve the row")
	require.NotNil(t, row.DeletedAt, "delete event must set deleted_at")
}

func TestIngestExternalContact_Deleted_NoExistingRow_NoOp(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	entityID := env.sourceIDPrefix + "delete-unknown"
	delEv := buildExtDeleteEvent(t, env.pairedHostID, entityID)
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{delEv},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted, "delete for unknown entity must still be accepted (event-log durable)")
	require.Equal(t, 0, resp.Rejected, "errors: %+v", resp.Errors)

	// No row materializes for the unknown delete.
	require.Nil(t, findExtRow(t, env, entityID))

	// But the event-log row should exist (audit trail).
	logged, err := env.eventRepo.FindEventBySource(
		context.Background(), "icloud_contacts", canonicalDeleteSourceID(entityID))
	require.NoError(t, err)
	require.NotNil(t, logged, "event-log row must persist for the no-op delete (audit)")
}

func TestIngestExternalContact_Deleted_AlreadyTombstoned_NoOp(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	entityID := env.sourceIDPrefix + "delete-twice"
	upEv := buildExtUpsertEvent(t, env.pairedHostID, entityID, nil)
	require.Equal(t, 1, parseIngestResp(t, postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{upEv},
	})).Accepted)

	// First delete — content hash A.
	delEvA := buildExtDeleteEvent(t, env.pairedHostID, entityID)
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{delEvA},
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, parseIngestResp(t, w).Accepted)
	deletedAtA := findExtRow(t, env, entityID).DeletedAt
	require.NotNil(t, deletedAtA)

	// Second delete with a different source_id (different prev hash so
	// event-log dedup doesn't swallow it). Should be silently no-op at
	// the row level — deleted_at unchanged.
	delEvB := map[string]any{}
	for k, v := range delEvA {
		delEvB[k] = v
	}
	delEvB["source_id"] = entityID + "@deleted@" + "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	w = postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{delEvB},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, 1, parseIngestResp(t, w).Accepted)

	row := findExtRow(t, env, entityID)
	require.NotNil(t, row)
	require.NotNil(t, row.DeletedAt)
	require.Equal(t, deletedAtA.UnixMilli(), row.DeletedAt.UnixMilli(),
		"second delete must not bump deleted_at on an already-tombstoned row")
}

func TestIngestExternalContact_Deleted_CrossHost_NoOp(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	ctx := context.Background()
	entityID := env.sourceIDPrefix + "cross-host-delete"

	// Host A (the env's paired host) creates the row.
	hostA := env.pairedHostID
	upEv := buildExtUpsertEvent(t, hostA, entityID, nil)
	w := postIngestExt(t, env, &hostA, env.pairedHostKey, map[string]any{
		"events": []any{upEv},
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, parseIngestResp(t, w).Accepted)
	rowBefore := findExtRow(t, env, entityID)
	require.NotNil(t, rowBefore)
	require.Nil(t, rowBefore.DeletedAt)
	require.NotNil(t, rowBefore.HostID)
	require.Equal(t, hostA, *rowBefore.HostID, "row must be owned by host A")

	// Revoke A so B can pair (idx_mac_host_singleton allows one active host).
	require.NoError(t, env.macService.RevokeHost(ctx, hostA))

	// Pair host B.
	plainB, _, err := env.macService.CreatePairingToken(ctx)
	require.NoError(t, err)
	pairB, err := env.macService.PairWithToken(ctx, plainB, "host-b-delete", "0.1.0", 1)
	require.NoError(t, err)

	// Host B emits a delete for host A's row. Uses the @unknown sentinel,
	// which is the realistic post-re-pair shape (B's daemon has no local
	// cache of A's prior content hash).
	delEv := buildExtDeleteEvent(t, pairB.HostID, entityID)
	w = postIngestExt(t, env, &pairB.HostID, pairB.APIKey, map[string]any{
		"events": []any{delEv},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted, "cross-host delete is a silent no-op, counted as accepted")
	require.Equal(t, 0, resp.Rejected, "errors: %+v", resp.Errors)

	// Row must remain live — host B cannot tombstone a row owned by A.
	rowAfter := findExtRow(t, env, entityID)
	require.NotNil(t, rowAfter, "row must still exist")
	require.Nil(t, rowAfter.DeletedAt, "cross-host delete must NOT tombstone the row")
	require.NotNil(t, rowAfter.HostID)
	require.Equal(t, hostA, *rowAfter.HostID, "host ownership unchanged")
}

func TestIngestExternalContact_Deleted_LegacyNullHost_SoftDeletes(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	ctx := context.Background()
	entityID := env.sourceIDPrefix + "legacy-null-host-delete"

	// Seed a row directly with NULL host_id (simulates pre-migration-052
	// rows, or rows from sources that don't set host_id).
	sourceID := entityID
	syncedAt := accelerated.GetCurrentTime()
	tx, err := env.database.Pool.Begin(ctx)
	require.NoError(t, err)
	_, err = env.externalRepo.UpsertTx(ctx, tx, repository.UpsertExternalContactRequest{
		Source:   "icloud_contacts",
		SourceID: sourceID,
		HostID:   nil, // legacy NULL
		SyncedAt: &syncedAt,
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	// The currently-paired host emits a delete for the legacy row.
	delEv := buildExtDeleteEvent(t, env.pairedHostID, entityID)
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{delEv},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Equal(t, 0, resp.Rejected, "errors: %+v", resp.Errors)

	// NULL prior.HostID must pass through the host-scope guard so the
	// existing soft-delete path keeps working. Note: this assertion holds
	// both with AND without the fix (the guard's `prior.HostID != nil`
	// check short-circuits on NULL). The test exists as a forward-looking
	// guard against a future regression that drops NULL through the guard.
	row := findExtRow(t, env, entityID)
	require.NotNil(t, row)
	require.NotNil(t, row.DeletedAt, "NULL-host_id row must still be soft-deletable")
}

func TestIngestExternalContact_Upserted_DuplicateContentHash_DedupSkipsHandler(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	entityID := env.sourceIDPrefix + "dedup"
	ev := buildExtUpsertEvent(t, env.pairedHostID, entityID, nil)

	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, parseIngestResp(t, w).Accepted)
	row1 := findExtRow(t, env, entityID)
	require.NotNil(t, row1)

	// Re-post the exact same event (same source_id, same content hash).
	w = postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code)
	resp := parseIngestResp(t, w)
	require.Equal(t, 0, resp.Accepted)
	require.Equal(t, 1, resp.Duplicate)
	require.Equal(t, 0, resp.Rejected)
}

func TestIngestExternalContact_GlobalKeyPath_RejectedHostOnly(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	entityID := env.sourceIDPrefix + "global-key"
	ev := buildExtUpsertEvent(t, env.pairedHostID, entityID, nil)
	w := postIngestExt(t, env, nil, "", map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 0, resp.Accepted)
	require.Equal(t, 1, resp.Rejected)
	require.Len(t, resp.Errors, 1)
	require.Equal(t, "HOST_ONLY_REQUIRES_HOST_AUTH", resp.Errors[0].Code)
}

func TestIngestExternalContact_HostAuth_UnsupportedSource(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	entityID := env.sourceIDPrefix + "wrong-source"
	ev := buildExtUpsertEvent(t, env.pairedHostID, entityID, nil)
	ev["source"] = "anarlog_humans"
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 0, resp.Accepted)
	require.Equal(t, 1, resp.Rejected)
	require.Len(t, resp.Errors, 1)
	require.Equal(t, "PAYLOAD_INVARIANT", resp.Errors[0].Code)
}

func TestIngestExternalContact_HostMismatch_Rejected(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	entityID := env.sourceIDPrefix + "host-mismatch"
	ev := buildExtUpsertEvent(t, uuid.New() /* random host id */, entityID, nil)
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Rejected)
	require.Equal(t, "PAYLOAD_INVARIANT", resp.Errors[0].Code)
}

func TestIngestExternalContact_BadSourceIDFormat_Rejected(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	entityID := env.sourceIDPrefix + "bad-source-id"
	ev := buildExtUpsertEvent(t, env.pairedHostID, entityID, nil)
	ev["source_id"] = entityID + "@not-a-hash"
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Rejected)
	require.Equal(t, "PAYLOAD_INVARIANT", resp.Errors[0].Code)
}

func TestIngestExternalContact_VersionZero_Rejected(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	entityID := env.sourceIDPrefix + "v0"
	ev := buildExtUpsertEvent(t, env.pairedHostID, entityID, func(p *events.ExternalContactUpsertedPayload) {
		p.Version = 0 // wire-shape forbidden — Version must be >= 1
	})
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Rejected)
	require.Equal(t, "PAYLOAD_INVALID", resp.Errors[0].Code)
	require.Contains(t, resp.Errors[0].Message, "version")
}

func TestIngestExternalContact_VersionTooHigh_Rejected(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	entityID := env.sourceIDPrefix + "vhigh"
	ev := buildExtUpsertEvent(t, env.pairedHostID, entityID, func(p *events.ExternalContactUpsertedPayload) {
		p.Version = 999 // future daemon — operator must upgrade Pi
	})
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Rejected)
	require.Equal(t, "PAYLOAD_INVALID", resp.Errors[0].Code)
	require.Contains(t, resp.Errors[0].Message, "upgrade Pi")
}

// ----------------------------------------------------------------------------
// Un-normalizable identifier tests (issue #320)
//
// Regression coverage for the daemon cursor stall caused by per-event
// rejections when a phone/email field normalized to the empty string
// (e.g. "+", "   ", "\t"). All tests assert the envelope is accepted
// and other (valid) identifiers in the same envelope still produce
// identity rows / match the seeded contact.
// ----------------------------------------------------------------------------

// findSeededMethod returns the seeded contact_method value for the
// given type ("email" or "phone"). Mirrors the inline lookups in
// existing tests but in one place.
func findSeededMethod(t *testing.T, env *extContactIngestEnv, methodType string) string {
	t.Helper()
	methods, err := env.cmRepo.ListContactMethodsByContact(context.Background(), env.seededContact)
	require.NoError(t, err)
	for _, m := range methods {
		if m.Type == methodType {
			return m.Value
		}
	}
	t.Fatalf("no seeded %s on env.seededContact", methodType)
	return ""
}

// addSeededPhoneWithUniqueDigits attaches a new phone contact_method
// to env.seededContact whose normalized value embeds a high-entropy
// digit sequence derived from sourceIDPrefix. The env's default
// seeded phone uses uuid hex which can normalize to a short digit
// string (e.g. all-letters suffix yields "+1555"), causing
// ambiguity with other tests that share the same DB. Tests that
// match via phone use this helper instead to avoid cross-test
// collisions on value_normalized.
func addSeededPhoneWithUniqueDigits(t *testing.T, env *extContactIngestEnv) string {
	t.Helper()
	// sourceIDPrefix is "ext-ingest-<uuid8>-"; hash to 10 digits.
	h := sha256.Sum256([]byte(env.sourceIDPrefix))
	var b strings.Builder
	for _, by := range h {
		if b.Len() >= 10 {
			break
		}
		fmt.Fprintf(&b, "%d", int(by)%10)
	}
	phone := "+1" + b.String()
	_, err := env.cmRepo.CreateContactMethod(context.Background(), repository.CreateContactMethodRequest{
		ContactID: env.seededContact,
		Type:      "phone",
		Value:     phone,
	})
	require.NoError(t, err)
	return phone
}

func TestIngestExternalContact_Upserted_PhoneJunkPlusValidMatching_MatchesContact(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	ctx := context.Background()
	entityID := env.sourceIDPrefix + "phone-junk-plus-valid"
	seededPhone := addSeededPhoneWithUniqueDigits(t, env)

	countBefore, err := env.identityRepo.CountBySource(ctx, "icloud_contacts")
	require.NoError(t, err)

	ev := buildExtUpsertEvent(t, env.pairedHostID, entityID, func(p *events.ExternalContactUpsertedPayload) {
		p.Phones = []events.ExternalContactMethodValue{
			{Value: "+"}, // normalizes to empty — must be silently skipped
			{Value: seededPhone},
		}
	})
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Equal(t, 0, resp.Rejected, "errors: %+v", resp.Errors)

	row := findExtRow(t, env, entityID)
	require.NotNil(t, row)
	require.NotNil(t, row.CRMContactID, "valid phone must produce a match")
	require.Equal(t, env.seededContact, *row.CRMContactID)
	require.Equal(t, repository.MatchStatusMatched, row.MatchStatus)

	countAfter, err := env.identityRepo.CountBySource(ctx, "icloud_contacts")
	require.NoError(t, err)
	require.Equal(t, int64(1), countAfter-countBefore,
		"exactly one identity row must be created (for the valid phone, not for \"+\")")

	// Confirm the row that bumped the count is the valid phone, not "+".
	normalized := identity.Normalize(seededPhone, identity.IdentifierTypePhone)
	got, err := env.identityRepo.GetByIdentifier(ctx, identity.IdentifierTypePhone, normalized, "icloud_contacts")
	require.NoError(t, err)
	require.NotNil(t, got)
}

func TestIngestExternalContact_Upserted_AllPhonesNormalizeToEmpty_StillAccepts(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	ctx := context.Background()
	entityID := env.sourceIDPrefix + "phones-all-junk"

	countBefore, err := env.identityRepo.CountBySource(ctx, "icloud_contacts")
	require.NoError(t, err)

	ev := buildExtUpsertEvent(t, env.pairedHostID, entityID, func(p *events.ExternalContactUpsertedPayload) {
		p.Phones = []events.ExternalContactMethodValue{{Value: "+"}, {Value: "   "}}
	})
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Equal(t, 0, resp.Rejected, "errors: %+v", resp.Errors)

	row := findExtRow(t, env, entityID)
	require.NotNil(t, row)
	require.Nil(t, row.CRMContactID, "no valid phone → no match")
	require.Equal(t, repository.MatchStatusUnmatched, row.MatchStatus)

	countAfter, err := env.identityRepo.CountBySource(ctx, "icloud_contacts")
	require.NoError(t, err)
	require.Equal(t, int64(0), countAfter-countBefore,
		"no identity rows must be created when all phones normalize to empty")
}

// PhoneLiteralPlus is the exact value surfaced by the PR #318
// hypothesis-validation run that originally exposed issue #320.
func TestIngestExternalContact_Upserted_PhoneLiteralPlus_RegressionForSurfacedData(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	ctx := context.Background()
	entityID := env.sourceIDPrefix + "phone-literal-plus"

	countBefore, err := env.identityRepo.CountBySource(ctx, "icloud_contacts")
	require.NoError(t, err)

	ev := buildExtUpsertEvent(t, env.pairedHostID, entityID, func(p *events.ExternalContactUpsertedPayload) {
		p.Phones = []events.ExternalContactMethodValue{{Value: "+"}}
	})
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Equal(t, 0, resp.Rejected, "errors: %+v", resp.Errors)

	row := findExtRow(t, env, entityID)
	require.NotNil(t, row)
	require.Nil(t, row.CRMContactID)
	require.Equal(t, repository.MatchStatusUnmatched, row.MatchStatus)

	countAfter, err := env.identityRepo.CountBySource(ctx, "icloud_contacts")
	require.NoError(t, err)
	require.Equal(t, int64(0), countAfter-countBefore)
}

func TestIngestExternalContact_Upserted_EmailJunkPlusValidMatching_MatchesContact(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	ctx := context.Background()
	entityID := env.sourceIDPrefix + "email-junk-plus-valid"
	seededEmail := findSeededMethod(t, env, "email")

	countBefore, err := env.identityRepo.CountBySource(ctx, "icloud_contacts")
	require.NoError(t, err)

	ev := buildExtUpsertEvent(t, env.pairedHostID, entityID, func(p *events.ExternalContactUpsertedPayload) {
		p.Emails = []events.ExternalContactMethodValue{
			{Value: "   "}, // normalizes to empty — must be silently skipped
			{Value: seededEmail},
		}
	})
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Equal(t, 0, resp.Rejected, "errors: %+v", resp.Errors)

	row := findExtRow(t, env, entityID)
	require.NotNil(t, row)
	require.NotNil(t, row.CRMContactID, "valid email must produce a match")
	require.Equal(t, env.seededContact, *row.CRMContactID)
	require.Equal(t, repository.MatchStatusMatched, row.MatchStatus)

	countAfter, err := env.identityRepo.CountBySource(ctx, "icloud_contacts")
	require.NoError(t, err)
	require.Equal(t, int64(1), countAfter-countBefore,
		"exactly one identity row must be created (for the valid email)")

	normalized := identity.Normalize(seededEmail, identity.IdentifierTypeEmail)
	got, err := env.identityRepo.GetByIdentifier(ctx, identity.IdentifierTypeEmail, normalized, "icloud_contacts")
	require.NoError(t, err)
	require.NotNil(t, got)
}

func TestIngestExternalContact_Upserted_AllEmailsNormalizeToEmpty_StillAccepts(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	ctx := context.Background()
	entityID := env.sourceIDPrefix + "emails-all-junk"

	countBefore, err := env.identityRepo.CountBySource(ctx, "icloud_contacts")
	require.NoError(t, err)

	ev := buildExtUpsertEvent(t, env.pairedHostID, entityID, func(p *events.ExternalContactUpsertedPayload) {
		p.Emails = []events.ExternalContactMethodValue{{Value: "   "}, {Value: "\t"}}
	})
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Equal(t, 0, resp.Rejected, "errors: %+v", resp.Errors)

	row := findExtRow(t, env, entityID)
	require.NotNil(t, row)
	require.Nil(t, row.CRMContactID)
	require.Equal(t, repository.MatchStatusUnmatched, row.MatchStatus)

	countAfter, err := env.identityRepo.CountBySource(ctx, "icloud_contacts")
	require.NoError(t, err)
	require.Equal(t, int64(0), countAfter-countBefore)
}

// EmailWhitespaceOnly is the email-side regression equivalent of
// PhoneLiteralPlus. Note: do NOT use "@" — that survives normalization
// and would exercise a different code path.
func TestIngestExternalContact_Upserted_EmailWhitespaceOnly_RegressionForJunkData(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	ctx := context.Background()
	entityID := env.sourceIDPrefix + "email-whitespace-only"

	countBefore, err := env.identityRepo.CountBySource(ctx, "icloud_contacts")
	require.NoError(t, err)

	ev := buildExtUpsertEvent(t, env.pairedHostID, entityID, func(p *events.ExternalContactUpsertedPayload) {
		p.Emails = []events.ExternalContactMethodValue{{Value: "   "}}
	})
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Equal(t, 0, resp.Rejected, "errors: %+v", resp.Errors)

	row := findExtRow(t, env, entityID)
	require.NotNil(t, row)
	require.Nil(t, row.CRMContactID)
	require.Equal(t, repository.MatchStatusUnmatched, row.MatchStatus)

	countAfter, err := env.identityRepo.CountBySource(ctx, "icloud_contacts")
	require.NoError(t, err)
	require.Equal(t, int64(0), countAfter-countBefore)
}

// JunkEmailValidMatchingPhone proves the junk-email skip in the first
// loop does NOT prevent the phone loop from running and matching.
func TestIngestExternalContact_Upserted_JunkEmailValidMatchingPhone_MatchesViaPhone(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	entityID := env.sourceIDPrefix + "junk-email-valid-phone"
	seededPhone := addSeededPhoneWithUniqueDigits(t, env)

	ev := buildExtUpsertEvent(t, env.pairedHostID, entityID, func(p *events.ExternalContactUpsertedPayload) {
		p.Emails = []events.ExternalContactMethodValue{{Value: "   "}}
		p.Phones = []events.ExternalContactMethodValue{{Value: seededPhone}}
	})
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Equal(t, 0, resp.Rejected, "errors: %+v", resp.Errors)

	row := findExtRow(t, env, entityID)
	require.NotNil(t, row)
	require.NotNil(t, row.CRMContactID, "junk email must not block phone-based match")
	require.Equal(t, env.seededContact, *row.CRMContactID)
	require.Equal(t, repository.MatchStatusMatched, row.MatchStatus)
}

// ValidEmailMatchingThenJunkPhone proves the rollback path is gone:
// pre-fix, a valid email match followed by a "+" phone would have
// rolled back the whole savepoint (the inner empty-check tripped after
// state was already written).
func TestIngestExternalContact_Upserted_ValidEmailMatchingThenJunkPhone_StillAccepted(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	entityID := env.sourceIDPrefix + "valid-email-junk-phone"
	seededEmail := findSeededMethod(t, env, "email")

	ev := buildExtUpsertEvent(t, env.pairedHostID, entityID, func(p *events.ExternalContactUpsertedPayload) {
		p.Emails = []events.ExternalContactMethodValue{{Value: seededEmail}}
		p.Phones = []events.ExternalContactMethodValue{{Value: "+"}}
	})
	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{ev},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 1, resp.Accepted)
	require.Equal(t, 0, resp.Rejected, "errors: %+v", resp.Errors)

	row := findExtRow(t, env, entityID)
	require.NotNil(t, row)
	require.NotNil(t, row.CRMContactID)
	require.Equal(t, env.seededContact, *row.CRMContactID)
	require.Equal(t, repository.MatchStatusMatched, row.MatchStatus)
}

// BatchWithJunkAndValidContacts is the direct regression for issue
// #320's cursor-stall failure mode: one junk-only event and one
// matching event in the same batch. Pre-fix, the junk event would
// reject and the daemon would hold the cursor. Both must now accept.
func TestIngestExternalContact_Upserted_BatchWithJunkAndValidContacts_BothLand(t *testing.T) {
	env := setupExtContactIngestEnv(t)
	entityIDJunk := env.sourceIDPrefix + "batch-junk"
	entityIDValid := env.sourceIDPrefix + "batch-valid"
	seededPhone := addSeededPhoneWithUniqueDigits(t, env)

	evJunk := buildExtUpsertEvent(t, env.pairedHostID, entityIDJunk, func(p *events.ExternalContactUpsertedPayload) {
		p.Phones = []events.ExternalContactMethodValue{{Value: "+"}}
	})
	evValid := buildExtUpsertEvent(t, env.pairedHostID, entityIDValid, func(p *events.ExternalContactUpsertedPayload) {
		p.Phones = []events.ExternalContactMethodValue{{Value: seededPhone}}
	})

	w := postIngestExt(t, env, &env.pairedHostID, env.pairedHostKey, map[string]any{
		"events": []any{evJunk, evValid},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := parseIngestResp(t, w)
	require.Equal(t, 2, resp.Accepted, "both events must be accepted; errors: %+v", resp.Errors)
	require.Equal(t, 0, resp.Rejected, "errors: %+v", resp.Errors)

	rowJunk := findExtRow(t, env, entityIDJunk)
	require.NotNil(t, rowJunk, "junk-only event must still materialize an external_contact row")
	require.Nil(t, rowJunk.CRMContactID)
	require.Equal(t, repository.MatchStatusUnmatched, rowJunk.MatchStatus)

	rowValid := findExtRow(t, env, entityIDValid)
	require.NotNil(t, rowValid)
	require.NotNil(t, rowValid.CRMContactID, "valid event must match the seeded contact")
	require.Equal(t, env.seededContact, *rowValid.CRMContactID)
	require.Equal(t, repository.MatchStatusMatched, rowValid.MatchStatus)
}
