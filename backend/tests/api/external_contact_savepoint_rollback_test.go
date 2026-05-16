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
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// failingMatchExternalContactWriter wraps a real repository and
// intentionally fails UpdateMatchTx. Used to drive the savepoint-
// rollback test: a failure mid-handler must roll back the
// (Bus.PublishTx + UpsertTx + identity rows) writes inside the same
// savepoint so no partial state lands.
type failingMatchExternalContactWriter struct {
	inner *repository.ExternalContactRepository
}

func (f *failingMatchExternalContactWriter) GetBySourceTx(ctx context.Context, tx pgx.Tx, source, sourceID string, accountID *string) (*repository.ExternalContact, error) {
	return f.inner.GetBySourceTx(ctx, tx, source, sourceID, accountID)
}
func (f *failingMatchExternalContactWriter) UpsertTx(ctx context.Context, tx pgx.Tx, req repository.UpsertExternalContactRequest) (*repository.ExternalContact, error) {
	return f.inner.UpsertTx(ctx, tx, req)
}
func (f *failingMatchExternalContactWriter) UpdateMatchTx(_ context.Context, _ pgx.Tx, _ uuid.UUID, _ *uuid.UUID, _ repository.MatchStatus) (*repository.ExternalContact, error) {
	return nil, errors.New("simulated UpdateMatchTx failure")
}
func (f *failingMatchExternalContactWriter) ReviveTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.ExternalContact, error) {
	return f.inner.ReviveTx(ctx, tx, id)
}
func (f *failingMatchExternalContactWriter) SoftDeleteTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	return f.inner.SoftDeleteTx(ctx, tx, id)
}

// TestIngestExternalContact_SavepointRollback_OnMatchFailure asserts
// that when the inline handler fails partway through (UpdateMatchTx
// errors), the savepoint rolls back EVERY write inside it:
//
//  1. external_contact row was NOT inserted.
//  2. external_identity rows from in-handler MatchOrCreateTx calls
//     were NOT inserted.
//  3. The event-log row was NOT persisted either — Bus.PublishTx is
//     called INSIDE the same savepoint, so rollback undoes that too.
//
// Documents the intentional tradeoff: a daemon retry on the same
// event will re-execute the handler (no event-log dedup suppresses
// it), which is the same contract raw_message.* follows.
func TestIngestExternalContact_SavepointRollback_OnMatchFailure(t *testing.T) {
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
	t.Cleanup(func() { database.Close() })

	hostRepo := repository.NewMacHostRepository(database.Queries)
	pairingRepo := repository.NewMacHostPairingTokenRepository(database.Queries)
	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	macService := service.NewMacHostService(hostRepo, pairingRepo, syncRepo, contactMethodRepo, nil, database.Pool, 4)

	identityRepo := repository.NewIdentityRepository(database.Queries)
	identityService := service.NewIdentityService(identityRepo)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	externalRepo := repository.NewExternalContactRepository(database.Queries)

	eventRepo := repository.NewEventRepository(database.Queries)
	eventBus := events.NewBus(database.Pool, nil, eventRepo)

	// Wire the failing stub for UpdateMatchTx so the inline handler's
	// match-flip path errors after Bus.PublishTx + UpsertTx +
	// MatchOrCreateTx have already written rows inside the savepoint.
	failingWriter := &failingMatchExternalContactWriter{inner: externalRepo}
	ingestService := service.NewIngestService(database, eventBus, identityService, nil, nil, failingWriter, hostRepo)
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
	g := router.Group("/api/v1/ingest")
	g.Use(ingestAuth)
	g.POST("/events", ingestHandler.IngestEvents)

	// Pair a host.
	plain, _, err := macService.CreatePairingToken(ctx)
	require.NoError(t, err)
	pair, err := macService.PairWithToken(ctx, plain, "rollback-test", "0.1.0", 1)
	require.NoError(t, err)

	// Seed a CRM contact with an email; that match triggers the
	// failing UpdateMatchTx.
	suffix := uuid.NewString()[:8]
	created, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Rollback Test " + suffix,
	})
	require.NoError(t, err)
	seededEmail := "rollback-" + suffix + "@example.invalid"
	_, err = contactMethodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: created.ID,
		Type:      "email",
		Value:     seededEmail,
	})
	require.NoError(t, err)

	sourceIDPrefix := "rollback-" + suffix + "-"
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = database.Queries.TestDeleteExternalContactsBySourceIDPrefix(cleanCtx, sourceIDPrefix)
		_, _ = database.Queries.DeleteEventsBySource(cleanCtx, "icloud_contacts")
		_, _ = database.Queries.DeleteExternalIdentitiesBySource(cleanCtx, "icloud_contacts")
		_ = contactMethodRepo.DeleteContactMethodsByContact(cleanCtx, created.ID)
		_ = contactRepo.HardDeleteContact(cleanCtx, created.ID)
		_, _ = database.Queries.DeleteAllMacHosts(cleanCtx)
		_, _ = database.Queries.DeleteAllPairingTokens(cleanCtx)
	})

	// Build an upsert event whose email matches the seeded contact —
	// the inline handler will get past UpsertTx + MatchOrCreateTx and
	// then fail at UpdateMatchTx (the stub).
	entityID := sourceIDPrefix + "match-fail"
	hashHex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	now := accelerated.GetCurrentTime()
	dn := "Sample Rollback"
	p := events.ExternalContactUpsertedPayload{
		Version:     1,
		HostID:      pair.HostID,
		Source:      "icloud_contacts",
		EntityID:    entityID,
		DisplayName: &dn,
		Emails:      []events.ExternalContactMethodValue{{Value: seededEmail}},
		Metadata:    map[string]any{"container_identifier": "rollback-test"},
	}
	pBytes, err := events.Marshal(events.KindExternalContactUpserted, p)
	require.NoError(t, err)
	ev := map[string]any{
		"source":      "icloud_contacts",
		"source_id":   entityID + "@" + hashHex,
		"kind":        string(events.KindExternalContactUpserted),
		"payload":     json.RawMessage(pBytes),
		"observed_at": now,
	}

	body, err := json.Marshal(map[string]any{"events": []any{ev}})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mac-Host-ID", pair.HostID.String())
	req.Header.Set("Authorization", "Bearer "+pair.APIKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp handlers.IngestResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	require.Equal(t, 0, resp.Accepted, "match-failure must NOT count as accepted")
	require.Equal(t, 1, resp.Rejected)
	require.Len(t, resp.Errors, 1)
	require.Equal(t, "EXTERNAL_CONTACT_UPDATE_MATCH_FAILED", resp.Errors[0].Code)

	// 1. No external_contact row was committed.
	row, err := externalRepo.GetBySource(ctx, "icloud_contacts", entityID, nil)
	require.NoError(t, err)
	require.Nil(t, row, "external_contact row must not be committed after savepoint rollback")

	// 2. No event-log row was committed (Bus.PublishTx ran inside the
	//    savepoint, so rollback undoes it too).
	logged, err := eventRepo.FindEventBySource(ctx, "icloud_contacts", entityID+"@"+hashHex)
	require.Error(t, err, "event-log row must not be committed after savepoint rollback")
	require.True(t, errors.Is(err, db.ErrNotFound), "expected ErrNotFound, got %v", err)
	require.Nil(t, logged)

	// 3. No external_identity row was committed. The handler called
	//    MatchOrCreateTx on the seeded email BEFORE UpdateMatchTx
	//    failed; that call writes an external_identity row inside the
	//    same savepoint. Rollback must undo it.
	ident, err := identityRepo.GetByIdentifier(ctx,
		identity.IdentifierTypeEmail, seededEmail, "icloud_contacts")
	require.Error(t, err, "external_identity row must not be committed after savepoint rollback")
	require.True(t, errors.Is(err, db.ErrNotFound), "expected ErrNotFound, got %v", err)
	require.Nil(t, ident)
}
