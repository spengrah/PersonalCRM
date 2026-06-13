package unit

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"
	"personal-crm/backend/internal/service"
	syncpkg "personal-crm/backend/internal/sync"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingProvider is a sync.SyncProvider stub that counts Sync calls so
// tests can verify whether the inline fallback fired.
type countingProvider struct {
	cfg   syncpkg.SourceConfig
	count int
}

func (p *countingProvider) Config() syncpkg.SourceConfig { return p.cfg }
func (p *countingProvider) ValidateCredentials(_ context.Context, _ *string) error {
	return nil
}
func (p *countingProvider) Sync(_ context.Context, _ *repository.SyncState, _ []repository.Contact) (*syncpkg.SyncResult, error) {
	p.count++
	return &syncpkg.SyncResult{ItemsProcessed: 1}, nil
}

func newServiceSuiteDB(t *testing.T) (*db.Database, context.Context) {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration-adjacent service test")
	}
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	ctx := context.Background()
	// Migrations are applied once by TestMain.

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })
	return database, ctx
}

// TestSyncService_TriggerSync_UsesEnqueuerWhenSet verifies that when
// SetRiverEnqueuer has been called, TriggerSync routes through the
// repository's atomic-claim helper and does NOT run the provider inline.
func TestSyncService_TriggerSync_UsesEnqueuerWhenSet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping db-backed service test in short mode")
	}
	t.Parallel()
	database, ctx := newServiceSuiteDB(t)

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)

	// Clean any prior state for this test's source.
	source := "service_test_enqueue_set"
	_ = syncRepo.DeleteRiverJobsBySourceArgForTest(ctx, source)
	_ = syncRepo.DeleteSyncStatesBySourceForTest(ctx, source)
	t.Cleanup(func() {
		_ = syncRepo.DeleteRiverJobsBySourceArgForTest(context.Background(), source)
		_ = syncRepo.DeleteSyncStatesBySourceForTest(context.Background(), source)
	})

	contactRepo := repository.NewContactRepository(database.Queries)
	registry := syncpkg.NewProviderRegistry()
	provider := &countingProvider{cfg: syncpkg.SourceConfig{
		Name:            source,
		DisplayName:     source,
		Strategy:        repository.SyncStrategyFetchAll,
		DefaultInterval: 15 * time.Minute,
	}}
	registry.Register(provider)

	svc := service.NewSyncService(syncRepo, contactRepo, registry)

	// Use a real river client as the enqueuer. Constructing a full fake
	// that matches the generic *river.Client[pgx.Tx] signature is hard
	// because the JobEnqueuer interface accepts pgx.Tx; a real TestOnly
	// client is the simplest path and gives us an end-to-end check that
	// service+repo+river.InsertTx integrates.
	workers := river.NewWorkers()
	river.AddWorker(workers, &syncWorkerNoop{})
	client := mustTestClient(t, database, workers)
	svc.SetRiverEnqueuer(client)

	require.NoError(t, svc.TriggerSync(ctx, source, nil))

	// Enqueue path: provider.Sync was NOT called inline (TriggerSync
	// just inserts a river_job).
	assert.Equal(t, 0, provider.count, "provider.Sync should NOT be called when enqueuer is set")

	// A river_job row should have been inserted for this source.
	cnt, err := syncRepo.CountRiverJobsBySourceArgForTest(ctx, source)
	require.NoError(t, err)
	assert.Equal(t, int64(1), cnt, "exactly one sync_provider_account row expected")
}

// TestSyncService_TriggerSync_FallsBackWhenEnqueuerNil verifies that
// when the enqueuer has NOT been set, TriggerSync runs the sync inline
// via runSyncForState.
func TestSyncService_TriggerSync_FallsBackWhenEnqueuerNil(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping db-backed service test in short mode")
	}
	t.Parallel()
	database, ctx := newServiceSuiteDB(t)

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)

	source := "service_test_enqueue_nil"
	_ = syncRepo.DeleteSyncStatesBySourceForTest(ctx, source)
	_ = syncRepo.DeleteSyncLogsBySourceForTest(ctx, source)
	t.Cleanup(func() {
		_ = syncRepo.DeleteSyncStatesBySourceForTest(context.Background(), source)
		_ = syncRepo.DeleteSyncLogsBySourceForTest(context.Background(), source)
	})

	contactRepo := repository.NewContactRepository(database.Queries)
	registry := syncpkg.NewProviderRegistry()
	provider := &countingProvider{cfg: syncpkg.SourceConfig{
		Name:            source,
		DisplayName:     source,
		Strategy:        repository.SyncStrategyFetchAll,
		DefaultInterval: 15 * time.Minute,
	}}
	registry.Register(provider)

	svc := service.NewSyncService(syncRepo, contactRepo, registry)
	// Deliberately do NOT call SetRiverEnqueuer.

	require.NoError(t, svc.TriggerSync(ctx, source, nil))

	// Fallback path: provider.Sync WAS called inline.
	assert.Equal(t, 1, provider.count, "provider.Sync should be called inline when enqueuer is nil")
}

// TestSyncService_TriggerSync_DedupedIsNoError verifies that when the
// atomic-claim helper reports a duplicate (another job is already
// in-flight for this source), TriggerSync returns nil — the call is
// an idempotent no-op. The legacy "sync already in progress"
// hard-block returning an error was retired along with the
// status='syncing' mutex.
func TestSyncService_TriggerSync_DedupedIsNoError(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping db-backed service test in short mode")
	}
	t.Parallel()
	database, ctx := newServiceSuiteDB(t)

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)

	source := "service_test_deduped_isnoerr"
	_ = syncRepo.DeleteRiverJobsBySourceArgForTest(ctx, source)
	_ = syncRepo.DeleteSyncStatesBySourceForTest(ctx, source)
	t.Cleanup(func() {
		_ = syncRepo.DeleteRiverJobsBySourceArgForTest(context.Background(), source)
		_ = syncRepo.DeleteSyncStatesBySourceForTest(context.Background(), source)
	})

	contactRepo := repository.NewContactRepository(database.Queries)
	registry := syncpkg.NewProviderRegistry()
	provider := &countingProvider{cfg: syncpkg.SourceConfig{
		Name:            source,
		DisplayName:     source,
		Strategy:        repository.SyncStrategyFetchAll,
		DefaultInterval: 15 * time.Minute,
	}}
	registry.Register(provider)
	svc := service.NewSyncService(syncRepo, contactRepo, registry)

	workers := river.NewWorkers()
	river.AddWorker(workers, &syncWorkerNoop{})
	client := mustTestClient(t, database, workers)
	svc.SetRiverEnqueuer(client)

	// Seed an in-flight row directly so the atomic-claim helper sees
	// count>0 and returns (enqueued=false, nil).
	require.NoError(t, syncRepo.InsertRiverJobForTest(ctx, []byte(`{"source":"`+source+`"}`)))

	// TriggerSync should observe the dedup and return nil — NOT an error.
	require.NoError(t, svc.TriggerSync(ctx, source, nil))

	// Provider was not called inline (enqueue path), and we didn't add
	// a second row.
	assert.Equal(t, 0, provider.count, "provider.Sync should not run on dedup")
	cnt, err := syncRepo.CountRiverJobsBySourceArgForTest(ctx, source)
	require.NoError(t, err)
	assert.Equal(t, int64(1), cnt, "dedup must not create a second row")
}

// TestSyncService_TriggerSync_EnqueueErrorWrapped verifies that an
// infrastructure error from the enqueue path is returned wrapped with a
// "enqueue sync job" prefix rather than silently swallowed.
func TestSyncService_TriggerSync_EnqueueErrorWrapped(t *testing.T) {
	t.Parallel()
	// Pure unit test — no DB needed. Use a fake JobEnqueuer that returns
	// an error from InsertTx. The repo method still opens a tx against
	// a nil pool, so we need a real pool-backed repo, but we can build
	// an in-memory one via the error-returning enqueuer path below.
	// Simplest path: use a fake repo that has SyncRepository pointing at
	// a minimal pool but the enqueuer errors — this is integration-
	// adjacent, so gate on DB.
	if testing.Short() {
		t.Skip("skipping db-backed service test in short mode")
	}
	database, ctx := newServiceSuiteDB(t)

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)

	source := "service_test_enqueue_errwrapped"
	_ = syncRepo.DeleteSyncStatesBySourceForTest(ctx, source)
	t.Cleanup(func() {
		_ = syncRepo.DeleteSyncStatesBySourceForTest(context.Background(), source)
	})

	contactRepo := repository.NewContactRepository(database.Queries)
	registry := syncpkg.NewProviderRegistry()
	registry.Register(&countingProvider{cfg: syncpkg.SourceConfig{
		Name:            source,
		DisplayName:     source,
		Strategy:        repository.SyncStrategyFetchAll,
		DefaultInterval: 15 * time.Minute,
	}})

	svc := service.NewSyncService(syncRepo, contactRepo, registry)

	// Fake enqueuer whose InsertTx always fails.
	fakeErr := errors.New("simulated enqueue failure")
	svc.SetRiverEnqueuer(&failingEnqueuer{err: fakeErr})

	err := svc.TriggerSync(ctx, source, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enqueue sync job",
		"enqueue error must be wrapped with a 'enqueue sync job' prefix")
}

// failingEnqueuer is a repository.JobEnqueuer that always returns the
// configured error from InsertTx. Used to exercise the error-wrapping
// branch of TriggerSync.
type failingEnqueuer struct {
	err error
}

func (f *failingEnqueuer) InsertTx(_ context.Context, _ pgx.Tx, _ river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	return nil, f.err
}

// TestSyncService_TriggerSync_NoLongerReadsSyncingStatus verifies that
// the legacy "already syncing" early-return is gone. Seed a state with
// status='syncing' and assert TriggerSync still dispatches
// successfully — the status-column mutex was retired in favor of
// river_job state as the source of truth for "in-flight".
func TestSyncService_TriggerSync_NoLongerReadsSyncingStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping db-backed service test in short mode")
	}
	t.Parallel()
	database, ctx := newServiceSuiteDB(t)

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)

	source := "service_test_syncing_status_nonblocking"
	_ = syncRepo.DeleteRiverJobsBySourceArgForTest(ctx, source)
	_ = syncRepo.DeleteSyncStatesBySourceForTest(ctx, source)
	t.Cleanup(func() {
		_ = syncRepo.DeleteRiverJobsBySourceArgForTest(context.Background(), source)
		_ = syncRepo.DeleteSyncStatesBySourceForTest(context.Background(), source)
	})

	contactRepo := repository.NewContactRepository(database.Queries)
	registry := syncpkg.NewProviderRegistry()
	provider := &countingProvider{cfg: syncpkg.SourceConfig{
		Name:            source,
		DisplayName:     source,
		Strategy:        repository.SyncStrategyFetchAll,
		DefaultInterval: 15 * time.Minute,
	}}
	registry.Register(provider)

	// Seed state with the legacy 'syncing' value BEFORE wiring the
	// enqueuer. The SyncStatusSyncing constant is retired, but the
	// repository's UpdateSyncStateStatus takes any SyncStatus string;
	// passing the literal exercises the same DB-level invariant the
	// retired constant used to cover without bypassing the repository
	// layer (core rule: never write raw SQL in Go).
	state, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
		Source:   source,
		Enabled:  true,
		Strategy: repository.SyncStrategyFetchAll,
	})
	require.NoError(t, err)
	_, err = syncRepo.UpdateSyncStateStatus(ctx, state.ID, repository.SyncStatus("syncing"), nil)
	require.NoError(t, err)

	svc := service.NewSyncService(syncRepo, contactRepo, registry)
	workers := river.NewWorkers()
	river.AddWorker(workers, &syncWorkerNoop{})
	client := mustTestClient(t, database, workers)
	svc.SetRiverEnqueuer(client)

	// Legacy behavior (when the service read status='syncing' as a
	// mutex) would return "sync already in progress" here. Today's
	// service does not read status for dispatch, so the call succeeds.
	require.NoError(t, svc.TriggerSync(ctx, source, nil))

	// A new river_job row should be enqueued despite status='syncing'.
	cnt, err := syncRepo.CountRiverJobsBySourceArgForTest(ctx, source)
	require.NoError(t, err)
	assert.Equal(t, int64(1), cnt, "TriggerSync should enqueue despite status='syncing' (mutex retired)")
}

// TestSyncService_EnqueueRequiresEnqueuer verifies that directly calling
// EnqueueAccountSyncIfNotInFlight without setting an enqueuer returns
// a clear error rather than panicking.
func TestSyncService_EnqueueRequiresEnqueuer(t *testing.T) {
	t.Parallel()
	syncRepo := repository.NewSyncRepository(nil)
	svc := service.NewSyncService(syncRepo, nil, syncpkg.NewProviderRegistry())
	_, err := svc.EnqueueAccountSyncIfNotInFlight(context.Background(), "gmail", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enqueuer")
}

// TestSyncService_ListDueAccounts_FiltersUnregisteredProviders asserts
// that stale sync_state rows for an unconfigured provider are filtered
// out of the tick-worker's enumeration — the Codex round-1 finding
// about poison jobs.
func TestSyncService_ListDueAccounts_FiltersUnregisteredProviders(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping db-backed service test in short mode")
	}
	t.Parallel()
	database, ctx := newServiceSuiteDB(t)

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)

	liveSource := "service_test_live_source"
	deadSource := "service_test_dead_source"
	cleanupSources := func(c context.Context) {
		_ = syncRepo.DeleteSyncStatesBySourceForTest(c, liveSource)
		_ = syncRepo.DeleteSyncStatesBySourceForTest(c, deadSource)
	}
	cleanupSources(ctx)
	t.Cleanup(func() { cleanupSources(context.Background()) })

	contactRepo := repository.NewContactRepository(database.Queries)
	registry := syncpkg.NewProviderRegistry()
	// Only register the "live" source — the "dead" source simulates a
	// sync_state row left over after OAuth was revoked for the
	// corresponding provider.
	registry.Register(&countingProvider{cfg: syncpkg.SourceConfig{
		Name:            liveSource,
		DisplayName:     liveSource,
		Strategy:        repository.SyncStrategyFetchAll,
		DefaultInterval: 15 * time.Minute,
	}})

	past := accelerated.GetCurrentTime().Add(-1 * time.Minute)
	_, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
		Source:     liveSource,
		Enabled:    true,
		Strategy:   repository.SyncStrategyFetchAll,
		NextSyncAt: &past,
	})
	require.NoError(t, err)
	_, err = syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
		Source:     deadSource,
		Enabled:    true,
		Strategy:   repository.SyncStrategyFetchAll,
		NextSyncAt: &past,
	})
	require.NoError(t, err)

	svc := service.NewSyncService(syncRepo, contactRepo, registry)
	accounts, err := svc.ListDueAccounts(ctx)
	require.NoError(t, err)

	foundLive, foundDead := false, false
	for _, a := range accounts {
		if a.Source == liveSource {
			foundLive = true
		}
		if a.Source == deadSource {
			foundDead = true
		}
	}
	assert.True(t, foundLive, "live source should be listed")
	assert.False(t, foundDead, "unregistered provider's source should be filtered out")
}

// TestSyncService_TriggerSync_RejectsNilAccountForAccountScopedProvider is the
// primary #530 regression: an account-scoped provider (Config().RequiresAccount)
// triggered with a nil account must be rejected with ErrAccountRequired and must
// NOT bootstrap an external_sync_state row (which would error on every dispatch
// forever). The enqueuer is wired so we can also assert no river_job was queued.
func TestSyncService_TriggerSync_RejectsNilAccountForAccountScopedProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping db-backed service test in short mode")
	}
	t.Parallel()
	database, ctx := newServiceSuiteDB(t)

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)

	source := "service_test_requires_account_nil"
	_ = syncRepo.DeleteRiverJobsBySourceArgForTest(ctx, source)
	_ = syncRepo.DeleteSyncStatesBySourceForTest(ctx, source)
	t.Cleanup(func() {
		_ = syncRepo.DeleteRiverJobsBySourceArgForTest(context.Background(), source)
		_ = syncRepo.DeleteSyncStatesBySourceForTest(context.Background(), source)
	})

	contactRepo := repository.NewContactRepository(database.Queries)
	registry := syncpkg.NewProviderRegistry()
	provider := &countingProvider{cfg: syncpkg.SourceConfig{
		Name:            source,
		DisplayName:     source,
		Strategy:        repository.SyncStrategyFetchAll,
		DefaultInterval: 15 * time.Minute,
		RequiresAccount: true,
	}}
	registry.Register(provider)

	svc := service.NewSyncService(syncRepo, contactRepo, registry)
	// Wire the enqueuer so the "no river_job enqueued" assertion is meaningful
	// (the guard returns before the enqueue branch).
	workers := river.NewWorkers()
	river.AddWorker(workers, &syncWorkerNoop{})
	client := mustTestClient(t, database, workers)
	svc.SetRiverEnqueuer(client)

	err := svc.TriggerSync(ctx, source, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, service.ErrAccountRequired),
		"expected ErrAccountRequired, got %v", err)

	// No external_sync_state row was created. GetSyncStateBySource uses
	// COALESCE(account_id,'') = COALESCE($2,'') so a nil lookup matches both a
	// NULL and an empty-string row — this single check covers both variants.
	_, getErr := syncRepo.GetSyncStateBySource(ctx, source, nil)
	assert.True(t, errors.Is(getErr, db.ErrNotFound),
		"no sync_state row should exist, got %v", getErr)

	// Provider.Sync was never called and nothing was enqueued.
	assert.Equal(t, 0, provider.count, "provider.Sync must not run when account is missing")
	cnt, err := syncRepo.CountRiverJobsBySourceArgForTest(ctx, source)
	require.NoError(t, err)
	assert.Equal(t, int64(0), cnt, "no river_job should be enqueued for a rejected trigger")
}

// TestSyncService_TriggerSync_RejectsMissingAccountVariants covers the empty and
// whitespace-only account paths in addition to nil. AccountIDMissing trims, so
// "", " ", and "\t" are all rejected — without this a {"account_id":""} body
// would bootstrap a non-NULL empty-string row that still errors at OAuth lookup.
func TestSyncService_TriggerSync_RejectsMissingAccountVariants(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping db-backed service test in short mode")
	}
	t.Parallel()
	database, ctx := newServiceSuiteDB(t)

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	contactRepo := repository.NewContactRepository(database.Queries)

	cases := []struct {
		name      string
		accountID *string
	}{
		{name: "nil", accountID: nil},
		{name: "empty", accountID: strPtr("")},
		{name: "whitespace", accountID: strPtr("  ")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			source := "service_test_requires_account_" + tc.name
			_ = syncRepo.DeleteRiverJobsBySourceArgForTest(ctx, source)
			_ = syncRepo.DeleteSyncStatesBySourceForTest(ctx, source)
			t.Cleanup(func() {
				_ = syncRepo.DeleteRiverJobsBySourceArgForTest(context.Background(), source)
				_ = syncRepo.DeleteSyncStatesBySourceForTest(context.Background(), source)
			})

			registry := syncpkg.NewProviderRegistry()
			provider := &countingProvider{cfg: syncpkg.SourceConfig{
				Name:            source,
				DisplayName:     source,
				Strategy:        repository.SyncStrategyFetchAll,
				DefaultInterval: 15 * time.Minute,
				RequiresAccount: true,
			}}
			registry.Register(provider)
			svc := service.NewSyncService(syncRepo, contactRepo, registry)

			err := svc.TriggerSync(ctx, source, tc.accountID)
			require.Error(t, err)
			assert.True(t, errors.Is(err, service.ErrAccountRequired),
				"expected ErrAccountRequired for %q, got %v", tc.name, err)

			// No row created for the exact value passed. The COALESCE lookup
			// collapses NULL/'' (covering nil + empty); for whitespace we query
			// with the same whitespace value, which the guard never wrote.
			_, getErr := syncRepo.GetSyncStateBySource(ctx, source, tc.accountID)
			assert.True(t, errors.Is(getErr, db.ErrNotFound),
				"no sync_state row should exist for %q, got %v", tc.name, getErr)
			assert.Equal(t, 0, provider.count, "provider.Sync must not run for %q", tc.name)
		})
	}
}

// TestSyncService_TriggerSync_AccountOptionalProviderStillBootstraps guards the
// second acceptance criterion: a provider that does NOT require an account
// (RequiresAccount defaults false) keeps its create-on-first-trigger behavior
// when called with a nil account. Prevents an over-broad rejection regression.
func TestSyncService_TriggerSync_AccountOptionalProviderStillBootstraps(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping db-backed service test in short mode")
	}
	t.Parallel()
	database, ctx := newServiceSuiteDB(t)

	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)

	source := "service_test_account_optional_bootstrap"
	_ = syncRepo.DeleteSyncStatesBySourceForTest(ctx, source)
	_ = syncRepo.DeleteSyncLogsBySourceForTest(ctx, source)
	t.Cleanup(func() {
		_ = syncRepo.DeleteSyncStatesBySourceForTest(context.Background(), source)
		_ = syncRepo.DeleteSyncLogsBySourceForTest(context.Background(), source)
	})

	contactRepo := repository.NewContactRepository(database.Queries)
	registry := syncpkg.NewProviderRegistry()
	provider := &countingProvider{cfg: syncpkg.SourceConfig{
		Name:            source,
		DisplayName:     source,
		Strategy:        repository.SyncStrategyFetchAll,
		DefaultInterval: 15 * time.Minute,
		// RequiresAccount left false (the push/account-optional default).
	}}
	registry.Register(provider)

	svc := service.NewSyncService(syncRepo, contactRepo, registry)
	// No enqueuer: TriggerSync falls back to inline runSyncForState after
	// bootstrapping the row.
	require.NoError(t, svc.TriggerSync(ctx, source, nil))

	// The create-on-first-trigger row exists and the inline sync ran.
	state, err := syncRepo.GetSyncStateBySource(ctx, source, nil)
	require.NoError(t, err)
	assert.Equal(t, source, state.Source)
	assert.Equal(t, 1, provider.count, "account-optional provider should still sync inline")
}

// --- test helpers ---

// mustTestClient builds a test-only *river.Client[pgx.Tx] so callers can
// set it on the service via SetRiverEnqueuer. TestOnly=true suppresses
// leader election and periodic loops.
func mustTestClient(t *testing.T, database *db.Database, workers *river.Workers) *river.Client[pgx.Tx] {
	t.Helper()
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 1},
		},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	return client
}

// syncWorkerNoop is a no-op river.Worker for SyncProviderAccountArgs.
// It exists so the test-only river client has the kind registered and
// InsertTx (called by the service via the repo's atomic-claim helper)
// doesn't reject the args with "job kind is not registered". The worker
// never runs in these tests — we assert on the inserted row, not on
// sync side-effects.
type syncWorkerNoop struct {
	river.WorkerDefaults[scheduler.SyncProviderAccountArgs]
}

func (*syncWorkerNoop) Work(_ context.Context, _ *river.Job[scheduler.SyncProviderAccountArgs]) error {
	return nil
}
