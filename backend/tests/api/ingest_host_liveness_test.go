package api

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

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

func TestIngest_HostRevokedMidTx_AbortsBatch(t *testing.T) {
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

	accepted, duplicate, rejections, err := svc.IngestBatch(
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
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	eventRepo := repository.NewEventRepository(database.Queries)
	eventBus := events.NewBus(database.Pool, nil, eventRepo)

	// nil hostLiveness — recheck is skipped.
	svc := service.NewIngestService(database, eventBus, nil, nil, nil, nil, nil)

	// An empty batch passes the precondition layer immediately and
	// never opens a tx. The test confirms wiring without the recheck
	// does not panic.
	accepted, duplicate, rejections, err := svc.IngestBatch(ctx, nil, nil, nil)
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
	macService := service.NewMacHostService(hostRepo, pairingRepo, syncRepo, nil, externalRepo, database.Pool, 4)
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
	blocker := &blockingExternalContactWriter{
		inner:   externalRepo,
		release: release,
		entered: make(chan struct{}),
	}

	eventRepo := repository.NewEventRepository(database.Queries)
	eventBus := events.NewBus(database.Pool, nil, eventRepo)
	svc := service.NewIngestService(database, eventBus, identityService, nil, nil, blocker, hostRepo)

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
		acc, _, _, err := svc.IngestBatch(
			ctx, []*events.Envelope{env}, []int{0}, &pair.HostID,
		)
		batchDone <- batchResult{accepted: acc, err: err}
	}()

	// Wait until goroutine A has reached UpsertTx — at that point the
	// FOR UPDATE lock is held inside the tx.
	select {
	case <-blocker.entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("batch never entered UpsertTx; FOR UPDATE acquire may have failed")
	}

	// Goroutine B: try to revoke. UPDATE mac_host will block on the
	// row lock held by goroutine A.
	revokeDone := make(chan struct{})
	revokeStart := time.Now()
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
	close(release)

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
		require.GreaterOrEqual(t, time.Since(revokeStart), 250*time.Millisecond,
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
