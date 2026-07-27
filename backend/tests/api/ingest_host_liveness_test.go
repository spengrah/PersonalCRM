package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
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

// ----------------------------------------------------------------------------
// IngestBatch tx-internal host-liveness FOR UPDATE coverage
//
// Exercises the SELECT ... FOR UPDATE recheck on mac_host that runs
// at the top of every IngestBatch tx. The recheck closes the race
// window between the auth-middleware host lookup and the batch's
// commit. Auth-middleware separately blocks revoked-host requests
// (covered by mac_host_auth_test.go); this file isolates the in-batch
// recheck so a regression that drops the FOR UPDATE acquire surfaces
// even if the auth-middleware path stays correct.
// ----------------------------------------------------------------------------

// stubFailingHostLiveness always returns db.ErrNotFound, simulating
// "host revoked between auth and the batch's lock acquire."
type stubFailingHostLiveness struct {
	calls int
}

func (s *stubFailingHostLiveness) GetActiveHostByIDForUpdateTx(
	_ context.Context, _ pgx.Tx, _ uuid.UUID,
) (*repository.MacHost, error) {
	s.calls++
	return nil, db.ErrNotFound
}

// spec: ING-006.whole-batch-rolled-back
func TestIngest_HostRevokedMidTx_AbortsBatch(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	t.Parallel()
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	eventRepo := repository.NewEventRepository(database.Queries)
	eventBus := events.NewBus(database.Pool, nil, eventRepo)
	externalRepo := repository.NewExternalContactRepository(database.Queries)

	// Inject the failing liveness checker. The batch must abort with
	// ErrHostRevokedDuringBatch BEFORE any envelope dispatches.
	liveness := &stubFailingHostLiveness{}
	svc := service.NewIngestService(
		database, eventBus,
		nil, // identity unused — we never reach the per-event handler
		nil, // messages unused
		nil, // river unused
		externalRepo,
		liveness,
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

	// Build a structurally-valid envelope so the batch precondition
	// checks pass and the loop is reached. The FOR UPDATE check must
	// fire before any per-event dispatch.
	hostID := uuid.New()
	entityID := "host-revoked-test-" + uuid.NewString()[:8]
	payload, err := events.Marshal(events.KindExternalContactUpserted, events.ExternalContactUpsertedPayload{
		Version:  1,
		HostID:   hostID,
		Source:   "icloud_contacts",
		EntityID: entityID,
	})
	require.NoError(t, err)
	hashHex, err := service.ComputeContentHash(payload)
	require.NoError(t, err)
	env := &events.Envelope{
		Source:     "icloud_contacts",
		SourceID:   entityID + "@" + hashHex,
		Kind:       events.KindExternalContactUpserted,
		Payload:    payload,
		ObservedAt: accelerated.GetCurrentTime(),
	}

	accepted, duplicate, rejections, _, err := svc.IngestBatch(
		ctx, []*events.Envelope{env}, []int{0}, &hostID,
	)
	require.Error(t, err, "batch must abort when liveness check reports revoked host")
	require.True(t, errors.Is(err, service.ErrHostRevokedDuringBatch),
		"abort error must be ErrHostRevokedDuringBatch (got %v)", err)
	require.Equal(t, 0, accepted)
	require.Equal(t, 0, duplicate)
	require.Empty(t, rejections)
	require.Equal(t, 1, liveness.calls, "FOR UPDATE check must fire exactly once per batch")

	// No event-log row may have committed. Cleanup-safe query: source
	// matches our test's synthetic prefix.
	cleanCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	logged, err := eventRepo.FindEventBySource(cleanCtx, "icloud_contacts", env.SourceID)
	if err == nil {
		require.Nil(t, logged, "no event-log row may commit when batch aborts")
	} else {
		require.True(t, errors.Is(err, db.ErrNotFound),
			"event-log row absence expected; got error %v", err)
	}
}

// TestIngest_HostLivenessNil_SkipsCheck documents the test-fixture
// path: NewIngestService(... nil) leaves the recheck disabled so
// existing test wiring without a real mac_host repo continues to work.
// Production wires a real repo so the check always runs.
func TestIngest_HostLivenessNil_SkipsCheck(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	t.Parallel()
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	eventRepo := repository.NewEventRepository(database.Queries)
	eventBus := events.NewBus(database.Pool, nil, eventRepo)

	// nil hostLiveness — recheck is skipped.
	svc := service.NewIngestService(database, eventBus, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	// An empty batch passes the precondition layer immediately and
	// never opens a tx. The test confirms wiring without the recheck
	// does not panic.
	accepted, duplicate, rejections, _, err := svc.IngestBatch(ctx, nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 0, accepted)
	require.Equal(t, 0, duplicate)
	require.Empty(t, rejections)
}

// blockingExternalContactWriter stalls UpsertTx on a release channel
// so a competing goroutine can race against the in-flight ingest
// batch tx. Used by TestIngest_ConcurrentRevokeBlocksUntilBatchCommit
// to prove the FOR UPDATE lock on mac_host serializes the revoke
// against the batch.
type blockingExternalContactWriter struct {
	inner   *repository.ExternalContactRepository
	release <-chan struct{}
	entered chan struct{} // closed exactly once when UpsertTx is reached
	once    sync.Once
}

func (b *blockingExternalContactWriter) GetBySourceTx(ctx context.Context, tx pgx.Tx, source, sourceID string, accountID *string) (*repository.ExternalContact, error) {
	return b.inner.GetBySourceTx(ctx, tx, source, sourceID, accountID)
}
func (b *blockingExternalContactWriter) UpsertTx(ctx context.Context, tx pgx.Tx, req repository.UpsertExternalContactRequest) (*repository.ExternalContact, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release // hold the batch tx open until the test releases us
	return b.inner.UpsertTx(ctx, tx, req)
}
func (b *blockingExternalContactWriter) UpdateMatchTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, crmContactID *uuid.UUID, status repository.MatchStatus) (*repository.ExternalContact, error) {
	return b.inner.UpdateMatchTx(ctx, tx, id, crmContactID, status)
}
func (b *blockingExternalContactWriter) ReviveTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.ExternalContact, error) {
	return b.inner.ReviveTx(ctx, tx, id)
}
func (b *blockingExternalContactWriter) SoftDeleteTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	return b.inner.SoftDeleteTx(ctx, tx, id)
}

// TestIngest_ConcurrentRevokeBlocksUntilBatchCommit proves the SELECT
// ... FOR UPDATE lock the batch acquires on mac_host actually
// serializes a concurrent revoke. The test runs two goroutines:
//
//  1. Goroutine A starts an IngestBatch that acquires the row lock
//     on mac_host, then stalls inside the per-event handler so the
//     batch tx stays open.
//  2. Goroutine B issues RevokeHost (UPDATE mac_host) and must block
//     on the row lock until A commits.
//
// The test asserts B does NOT return until A's batch commits. If the
// FOR UPDATE clause were removed (or the repo method ran without
// FOR UPDATE), B would update concurrently and finish well before A.
//
// Skipped unless LONG_TESTS=1 to keep the standard suite quick.
func TestIngest_ConcurrentRevokeBlocksUntilBatchCommit(t *testing.T) {
	if os.Getenv("LONG_TESTS") == "" {
		t.Skip("LONG_TESTS not set; skipping concurrency test")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	hostRepo := repository.NewMacHostRepository(database.Queries)
	pairingRepo := repository.NewMacHostPairingTokenRepository(database.Queries)
	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	macService := service.NewMacHostService(hostRepo, pairingRepo, syncRepo, nil, externalRepo, nil, database.Pool, 4)
	identityRepo := repository.NewIdentityRepository(database.Queries)
	identityService := service.NewIdentityService(identityRepo)

	// Pair a host so the FOR UPDATE acquire actually finds a row.
	plain, _, err := macService.CreatePairingToken(ctx)
	require.NoError(t, err)
	pair, err := macService.PairWithToken(ctx, plain, "concurrent-revoke-test", "0.1.0", 1)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = database.Queries.DeleteAllMacHosts(cleanCtx)
		_, _ = database.Queries.DeleteAllPairingTokens(cleanCtx)
	})

	release := make(chan struct{})
	releaseOnce := sync.Once{}
	releaseBlocker := func() { releaseOnce.Do(func() { close(release) }) }
	// Always release the blocker so the parked batch goroutine and
	// its open transaction unwind even if a t.Fatal aborts mid-test.
	defer releaseBlocker()
	blocker := &blockingExternalContactWriter{
		inner:   externalRepo,
		release: release,
		entered: make(chan struct{}),
	}

	eventRepo := repository.NewEventRepository(database.Queries)
	eventBus := events.NewBus(database.Pool, nil, eventRepo)
	svc := service.NewIngestService(database, eventBus, identityService, nil, nil, blocker, hostRepo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	entityID := "concurrent-revoke-" + uuid.NewString()[:8]
	payload, err := events.Marshal(events.KindExternalContactUpserted, events.ExternalContactUpsertedPayload{
		Version:  1,
		HostID:   pair.HostID,
		Source:   "icloud_contacts",
		EntityID: entityID,
	})
	require.NoError(t, err)
	hashHex, err := service.ComputeContentHash(payload)
	require.NoError(t, err)
	env := &events.Envelope{
		Source:     "icloud_contacts",
		SourceID:   entityID + "@" + hashHex,
		Kind:       events.KindExternalContactUpserted,
		Payload:    payload,
		ObservedAt: accelerated.GetCurrentTime(),
	}

	// Goroutine A: run the batch. It will acquire FOR UPDATE on the
	// mac_host row, then stall inside UpsertTx until we release it.
	type batchResult struct {
		accepted int
		err      error
	}
	batchDone := make(chan batchResult, 1)
	go func() {
		acc, _, _, _, err := svc.IngestBatch(
			ctx, []*events.Envelope{env}, []int{0}, &pair.HostID,
		)
		batchDone <- batchResult{accepted: acc, err: err}
	}()

	// Wait until goroutine A has reached UpsertTx — at that point the
	// FOR UPDATE lock is held inside the tx. The deferred
	// releaseBlocker below guarantees we never leak the parked
	// goroutine + open tx, even on a t.Fatal abort.
	select {
	case <-blocker.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("batch never entered UpsertTx; FOR UPDATE acquire may have failed")
	}

	// Goroutine B: try to revoke. UPDATE mac_host will block on the
	// row lock held by goroutine A. We measure how long the revoke
	// blocks using wall-clock time (not accelerated.GetCurrentTime
	// which can be sped up by the time-acceleration knob) because
	// the test is asserting on real elapsed concurrency behavior.
	revokeDone := make(chan struct{})
	revokeStart := time.Now() //nolint:forbidigo // Wall-clock time for concurrency-latency assertion
	var revokeErr error
	go func() {
		revokeErr = macService.RevokeHost(ctx, pair.HostID)
		close(revokeDone)
	}()

	// Confirm the revoke is actually waiting. Give it 300ms — well
	// beyond a normal UPDATE.
	select {
	case <-revokeDone:
		t.Fatal("revoke completed while batch tx held the FOR UPDATE lock; lock is not serializing")
	case <-time.After(300 * time.Millisecond):
		// expected: revoke blocked on the lock
	}

	// Release goroutine A. The batch commits, drops the lock, and B
	// proceeds.
	releaseBlocker()

	select {
	case res := <-batchDone:
		require.NoError(t, res.err)
		require.Equal(t, 1, res.accepted)
	case <-time.After(5 * time.Second):
		t.Fatal("batch did not finish after release")
	}

	select {
	case <-revokeDone:
		require.NoError(t, revokeErr)
		elapsed := time.Since(revokeStart) //nolint:forbidigo // Wall-clock concurrency latency
		require.GreaterOrEqual(t, elapsed, 250*time.Millisecond,
			"revoke must have waited for the batch tx to release the row lock")
	case <-time.After(5 * time.Second):
		t.Fatal("revoke never completed after batch released the lock")
	}

	// Cleanup: drop the row the batch upserted (UpsertTx ran on the
	// real repo via the blocker's inner).
	cleanCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = database.Queries.DeleteEventsBySource(cleanCtx, "icloud_contacts")
	_ = database.Queries.TestDeleteExternalContactsBySourceIDPrefix(cleanCtx, "concurrent-revoke-")
}

// TestIngest_HostRevokedMidBatch_Returns401UnknownHost exercises the
// HTTP half of the revoked-mid-batch contract: when IngestBatch aborts
// with ErrHostRevokedDuringBatch, the handler must answer 401 with the
// literal UNKNOWN_HOST code so the daemon stops retrying (a 5xx would
// keep it hammering a revoked pairing).
//
// The request rides the PRODUCTION auth path (IngestAuthMiddleware →
// MacHostAuthMiddleware with a real paired host), so the middleware's
// own host lookup PASSES — the 401 asserted here can only come from
// the handler's ErrHostRevokedDuringBatch mapping, fed by a stub
// liveness checker that reports the host revoked at the tx-internal
// re-check. liveness.calls is asserted to pin that provenance.
//
// Deliberately serial (no t.Parallel): pairs the singleton mac_host on
// the shared DB and hard-deletes all hosts in cleanup, same as the
// mac_host_* test files.
//
// spec: ING-006.unauthorized-401-stops-retries
func TestIngest_HostRevokedMidBatch_Returns401UnknownHost(t *testing.T) {
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
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	// bcrypt cost 4 keeps the real-bcrypt auth path tolerably fast.
	macService := service.NewMacHostService(hostRepo, pairingRepo, syncRepo, nil, externalRepo, nil, database.Pool, 4)

	plain, _, err := macService.CreatePairingToken(ctx)
	require.NoError(t, err)
	pair, err := macService.PairWithToken(ctx, plain, "ing006-revoked-mid-batch", "0.1.0", 1)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Same sweep as setupMacHostEnv: hosts + tokens + any push
		// sync-state rows pairing created.
		states, _ := database.Queries.ListSyncStates(cleanCtx)
		for _, s := range states {
			if s.Strategy == "push" {
				_ = database.Queries.DeleteSyncState(cleanCtx, s.ID)
			}
		}
		_, _ = database.Queries.DeleteAllMacHosts(cleanCtx)
		_, _ = database.Queries.DeleteAllPairingTokens(cleanCtx)
	})

	eventRepo := repository.NewEventRepository(database.Queries)
	eventBus := events.NewBus(database.Pool, nil, eventRepo)
	// Stub liveness: the host row IS active in the DB (auth middleware
	// passes), but the tx-internal FOR UPDATE re-check reports it
	// revoked — the mid-batch revocation race this behavior is about.
	liveness := &stubFailingHostLiveness{}
	svc := service.NewIngestService(database, eventBus, nil, nil, nil, externalRepo, liveness, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	ingestHandler := handlers.NewIngestHandler(svc)

	// gin mode is set once for the package in gin_test.go's init().
	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	router.Use(api.LoggingMiddleware())
	router.Use(api.CORSMiddleware(cfg.CORS))
	ingestAuth := auth.IngestAuthMiddleware(
		auth.APIKeyMiddleware(cfg),
		auth.MacHostAuthMiddleware(hostRepo, auth.DefaultPasswordComparator, auth.DefaultMacHostAuthLimiterConfig()),
	)
	ingestGroup := router.Group("/api/v1/ingest")
	ingestGroup.Use(ingestAuth)
	ingestGroup.POST("/events", ingestHandler.IngestEvents)

	// A fully-valid external_contact.upserted event whose payload claims
	// the AUTHENTICATED host — nothing about the event itself is wrong,
	// so the only failure in play is the revocation.
	entityID := "ing006-revoked-" + uuid.NewString()[:8]
	payload, err := events.Marshal(events.KindExternalContactUpserted, events.ExternalContactUpsertedPayload{
		Version:  1,
		HostID:   pair.HostID,
		Source:   "icloud_contacts",
		EntityID: entityID,
	})
	require.NoError(t, err)
	hashHex, err := service.ComputeContentHash(payload)
	require.NoError(t, err)
	sourceID := entityID + "@" + hashHex

	body, err := json.Marshal(map[string]any{
		"events": []any{map[string]any{
			"source":      "icloud_contacts",
			"source_id":   sourceID,
			"kind":        string(events.KindExternalContactUpserted),
			"payload":     payload,
			"observed_at": accelerated.GetCurrentTime(),
		}},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mac-Host-ID", pair.HostID.String())
	req.Header.Set("Authorization", "Bearer "+pair.APIKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
	// Assert the LITERAL wire error code, not a round-tripped DTO.
	var respBody map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &respBody))
	errObj, ok := respBody["error"].(map[string]any)
	require.True(t, ok, "response must carry an error object; body: %s", w.Body.String())
	require.Equal(t, "UNKNOWN_HOST", errObj["code"], "body: %s", w.Body.String())

	// Provenance: the auth middleware passed (host is active in the DB)
	// and the 401 came from the batch's tx-internal re-check.
	require.Equal(t, 1, liveness.calls,
		"the in-tx liveness re-check must be what fired — a 0 here means the 401 came from the auth middleware instead")

	// The aborted batch persisted nothing.
	logged, err := eventRepo.FindEventBySource(ctx, "icloud_contacts", sourceID)
	if err == nil {
		require.Nil(t, logged, "no event-log row may commit when the batch 401s")
	} else {
		require.True(t, errors.Is(err, db.ErrNotFound),
			"event-log row absence expected; got error %v", err)
	}
}

// TestIngest_PayloadHostIDMismatch_RejectedForEveryDaemonFamily pins
// the cross-family half of the daemon-claims invariant: for EVERY
// registered daemon family, a payload whose claimed host_id differs
// from the authenticated host is rejected (PAYLOAD_INVARIANT) instead
// of trusted. The family list is enumerated from the production ingest
// kind registry (service.DaemonFamilyViews), so registering a new
// family without a host-id-mismatch case here fails loudly.
//
// Every current family's payload carries a host_id claim (raw_message,
// external_contact, meeting_note, call) — there is no family that
// "cannot carry" the claim today; the require on the builders map is
// what forces a future family to either add a case or document an
// exemption.
//
// Each envelope is fully valid EXCEPT the host claim (source in the
// family allow-list, source_id matching the family's dedup-key shape
// incl. server-recomputed content hashes, peer_normalized matching the
// production re-canonicalization), so the asserted rejection can only
// come from the host-id re-check. The service is wired with nil family
// deps: on a regression that skips the check, dispatch degrades to a
// "not configured" rejection whose message fails the assertions below
// cleanly rather than panicking.
//
// spec: ING-036
func TestIngest_PayloadHostIDMismatch_RejectedForEveryDaemonFamily(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	t.Parallel()
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	eventRepo := repository.NewEventRepository(database.Queries)
	eventBus := events.NewBus(database.Pool, nil, eventRepo)
	// nil hostLiveness (that re-check is ING-006's concern) and nil
	// family deps — a host-mismatch rejection fires in the pre-savepoint
	// verify step, before any dep is touched.
	svc := service.NewIngestService(database, eventBus, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	authHostID := uuid.New()    // the host the request authenticated as
	claimedHostID := uuid.New() // the differing claim inside the payload
	require.NotEqual(t, authHostID, claimedHostID)
	ns := uuid.NewString()[:8] // per-run namespace for dedup keys on the shared DB
	observed := accelerated.GetCurrentTime()

	// One representative envelope builder per family, keyed by the
	// registry's family name. Both kinds of a family share one verify
	// function, so one kind per family exercises the family's check.
	builders := map[string]func(t *testing.T) *events.Envelope{
		"raw_message": func(t *testing.T) *events.Envelope {
			t.Helper()
			guid := "ing036-" + ns + "-" + uuid.NewString()
			payload, err := events.Marshal(events.KindRawMessageReceived, events.RawMessageReceivedPayload{
				Version:     1,
				HostID:      claimedHostID,
				Source:      "messages",
				Guid:        guid,
				ChatID:      "ing036-chat-" + ns,
				PeerHandle:  "+15550001111",
				MessageType: "text",
				SentAt:      observed,
			})
			require.NoError(t, err)
			return &events.Envelope{
				Source: "messages", SourceID: guid,
				Kind: events.KindRawMessageReceived, Payload: payload, ObservedAt: observed,
			}
		},
		"external_contact": func(t *testing.T) *events.Envelope {
			t.Helper()
			entityID := "ing036-" + ns + "-" + uuid.NewString()[:8]
			payload, err := events.Marshal(events.KindExternalContactUpserted, events.ExternalContactUpsertedPayload{
				Version:  1,
				HostID:   claimedHostID,
				Source:   "icloud_contacts",
				EntityID: entityID,
			})
			require.NoError(t, err)
			hashHex, err := service.ComputeContentHash(payload)
			require.NoError(t, err)
			return &events.Envelope{
				Source: "icloud_contacts", SourceID: entityID + "@" + hashHex,
				Kind: events.KindExternalContactUpserted, Payload: payload, ObservedAt: observed,
			}
		},
		"meeting_note": func(t *testing.T) *events.Envelope {
			t.Helper()
			sessionUUID := uuid.NewString()
			payload, err := events.Marshal(events.KindMeetingNoteRecorded, events.MeetingNoteRecordedPayload{
				Version:   1,
				HostID:    claimedHostID,
				Source:    "anarlog_sessions",
				SourceID:  sessionUUID,
				MeetingAt: observed,
			})
			require.NoError(t, err)
			hashHex, err := service.ComputeContentHash(payload)
			require.NoError(t, err)
			return &events.Envelope{
				Source: "anarlog_sessions", SourceID: sessionUUID + "@" + hashHex,
				Kind: events.KindMeetingNoteRecorded, Payload: payload, ObservedAt: observed,
			}
		},
		"call": func(t *testing.T) *events.Envelope {
			t.Helper()
			callID := "ing036-" + ns + "-" + uuid.NewString()
			const peer = "+15550002222"
			normalized := identity.Normalize(peer, identity.DetectIdentifierType(peer))
			require.NotEmpty(t, normalized)
			payload, err := events.Marshal(events.KindCallReceived, events.CallPayload{
				Version:         1,
				HostID:          claimedHostID,
				Source:          repository.InteractionSourcePhoneCalls,
				CallUniqueID:    callID,
				PeerHandle:      peer,
				PeerNormalized:  normalized,
				Service:         "voice",
				Direction:       "inbound",
				DurationSeconds: 30,
				StartedAt:       observed,
			})
			require.NoError(t, err)
			return &events.Envelope{
				Source: repository.InteractionSourcePhoneCalls, SourceID: callID,
				Kind: events.KindCallReceived, Payload: payload, ObservedAt: observed,
			}
		},
	}

	families := service.DaemonFamilyViews()
	require.NotEmpty(t, families)
	for _, fam := range families {
		build, ok := builders[fam.Name]
		require.True(t, ok,
			"daemon family %q has no host-id-mismatch case in this table — every family must re-check the daemon's host claim (add a builder, or document why the family cannot carry one)", fam.Name)
		t.Run(fam.Name, func(t *testing.T) {
			env := build(t)
			accepted, duplicate, rejections, needsAttention, err := svc.IngestBatch(
				ctx, []*events.Envelope{env}, []int{0}, &authHostID,
			)
			require.NoError(t, err)
			require.Equal(t, 0, accepted, "a mismatched host claim must never be accepted")
			require.Equal(t, 0, duplicate)
			require.Empty(t, needsAttention)
			require.Len(t, rejections, 1)
			require.Equal(t, 0, rejections[0].Index)
			require.Equal(t, "PAYLOAD_INVARIANT", rejections[0].Code)
			require.Contains(t, rejections[0].Message, "host_id does not match authenticated host",
				"rejection must come from the host-claim re-check, got: %s", rejections[0].Message)

			// The rejected event left no durable event-log row.
			logged, ferr := eventRepo.FindEventBySource(ctx, env.Source, env.SourceID)
			if ferr == nil {
				require.Nil(t, logged, "no event-log row may persist for a host-mismatch rejection")
			} else {
				require.True(t, errors.Is(ferr, db.ErrNotFound),
					"event-log row absence expected; got error %v", ferr)
			}
		})
	}
}
