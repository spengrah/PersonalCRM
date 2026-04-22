package todoist

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Integration-test harness (same-package so tests can call unexported
// handler functions like handleTaskCompletion / handleSkipTrigger /
// tryRecoverPendingTempID / reconcileExistingTask).
// ============================================================================

// faultyContactTaskWriter wraps a real contactTaskWriter and returns an
// injected error from a named method on its first call, delegating on
// subsequent calls. Safe for concurrent use.
type faultyContactTaskWriter struct {
	contactTaskWriter
	mu           sync.Mutex
	faultyMethod string
	fireLimit    int // 0 = fire once; N = fire up to N times
	fired        int
}

func (f *faultyContactTaskWriter) shouldFire(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.faultyMethod != method {
		return false
	}
	limit := f.fireLimit
	if limit == 0 {
		limit = 1
	}
	if f.fired >= limit {
		return false
	}
	f.fired++
	return true
}

func (f *faultyContactTaskWriter) UpdateContactTaskStateTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, state repository.ContactTaskState) (*repository.ContactTask, error) {
	if f.shouldFire("UpdateContactTaskStateTx") {
		return nil, context.Canceled
	}
	return f.contactTaskWriter.UpdateContactTaskStateTx(ctx, tx, id, state)
}

func (f *faultyContactTaskWriter) UpdateContactTaskMetadataTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, metadata map[string]any) (*repository.ContactTask, error) {
	if f.shouldFire("UpdateContactTaskMetadataTx") {
		return nil, context.Canceled
	}
	return f.contactTaskWriter.UpdateContactTaskMetadataTx(ctx, tx, id, metadata)
}

func (f *faultyContactTaskWriter) UpdateContactTaskExternalIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, externalID string) (*repository.ContactTask, error) {
	if f.shouldFire("UpdateContactTaskExternalIDTx") {
		return nil, context.Canceled
	}
	return f.contactTaskWriter.UpdateContactTaskExternalIDTx(ctx, tx, id, externalID)
}

func (f *faultyContactTaskWriter) UpdateContactTaskMetadata(ctx context.Context, id uuid.UUID, metadata map[string]any) (*repository.ContactTask, error) {
	if f.shouldFire("UpdateContactTaskMetadata") {
		return nil, context.Canceled
	}
	return f.contactTaskWriter.UpdateContactTaskMetadata(ctx, id, metadata)
}

// faultyContactWriter wraps a real contactWriter and injects errors on
// UpdateContactByTx.
type faultyContactWriter struct {
	contactWriter
	mu           sync.Mutex
	faultyMethod string
	fired        int
}

func (f *faultyContactWriter) shouldFire(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.faultyMethod != method {
		return false
	}
	if f.fired >= 1 {
		return false
	}
	f.fired++
	return true
}

func (f *faultyContactWriter) UpdateContactByTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, contactBy time.Time) error {
	if f.shouldFire("UpdateContactByTx") {
		return context.Canceled
	}
	return f.contactWriter.UpdateContactByTx(ctx, tx, id, contactBy)
}

// atomicTxTestEnv bundles the live bus + faulty writers + pool so tests
// can construct a provider with the exact fault-injection behavior they
// need.
type atomicTxTestEnv struct {
	ctx                  context.Context
	database             *db.Database
	pool                 *pgxpool.Pool
	bus                  *events.Bus
	eventRepo            *repository.EventRepository
	contactRepo          *repository.ContactRepository
	contactTaskRepo      *repository.ContactTaskRepository
	faultyTaskWriter     *faultyContactTaskWriter
	faultyContactWriterW *faultyContactWriter
	provider             *CadenceSyncProvider
	settings             Settings
	accountID            string
	riverClient          *river.Client[pgx.Tx]
}

// setupAtomicTxTestEnv wires a real DB + real events.Bus (with a TestOnly
// river client + noop workers for all published kinds) + faulty-wrapper
// repositories around a fresh CadenceSyncProvider.
func setupAtomicTxTestEnv(t *testing.T) (*atomicTxTestEnv, func()) {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	require.NoError(t, db.RunMigrations(ctx, databaseURL, migrationsPathForTest()))

	database, err := db.NewDatabase(ctx, config.DatabaseConfig{
		URL:               databaseURL,
		MaxConns:          config.DefaultDBMaxConns,
		MinConns:          config.DefaultDBMinConns,
		MaxConnIdleTime:   config.DefaultDBMaxConnIdleTime,
		MaxConnLifetime:   config.DefaultDBMaxConnLifetime,
		HealthCheckPeriod: config.DefaultDBHealthCheckPeriod,
	})
	require.NoError(t, err)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	eventRepo := repository.NewEventRepository(database.Queries)

	// Build a TestOnly river client with noop workers for every kind the
	// Todoist provider might publish so enqueued jobs drain without side
	// effects. The bus's PublishTx still writes the event row + inserts
	// the river job atomically — exactly what these tests verify.
	workers := river.NewWorkers()
	river.AddWorker(workers, &noopInteractionRecorderWorker{})
	river.AddWorker(workers, &noopCadenceUpdaterWorker{})
	river.AddWorker(workers, &noopFollowUpManagerWorker{})
	river.AddWorker(workers, &noopRematchDispatcherWorker{})

	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 4},
		},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	require.NoError(t, riverClient.Start(ctx))

	bus := events.NewBus(database.Pool, riverClient, eventRepo)

	faultyTaskWriter := &faultyContactTaskWriter{contactTaskWriter: contactTaskRepo}
	faultyCW := &faultyContactWriter{contactWriter: contactRepo}
	provider := NewCadenceSyncProvider(
		nil,
		faultyTaskWriter,
		faultyCW,
		nil,
		config.TestConfig(),
		bus,
		database.Pool,
		DefaultClientFactory,
	)

	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = riverClient.Stop(stopCtx)
		database.Close()
	}

	return &atomicTxTestEnv{
		ctx:                  ctx,
		database:             database,
		pool:                 database.Pool,
		bus:                  bus,
		eventRepo:            eventRepo,
		contactRepo:          contactRepo,
		contactTaskRepo:      contactTaskRepo,
		faultyTaskWriter:     faultyTaskWriter,
		faultyContactWriterW: faultyCW,
		provider:             provider,
		settings: Settings{
			ProjectID:             "test-project",
			ProjectName:           "CRM",
			LabelID:               "test-label-id",
			LabelName:             "crm",
			IntegrationInstanceID: "test-instance",
		},
		accountID:   "test-account",
		riverClient: riverClient,
	}, cleanup
}

// ============================================================================
// Noop river workers — drain jobs without side effects. The event bus
// atomicity contract is about PublishTx's write-and-enqueue pair; what the
// consumer does with the enqueued job is out of scope for these tests.
// ============================================================================

type noopInteractionRecorderWorker struct {
	river.WorkerDefaults[consumerjobs.InteractionRecorderJobArgs]
}

func (*noopInteractionRecorderWorker) Work(_ context.Context, _ *river.Job[consumerjobs.InteractionRecorderJobArgs]) error {
	return nil
}

type noopCadenceUpdaterWorker struct {
	river.WorkerDefaults[consumerjobs.CadenceUpdaterJobArgs]
}

func (*noopCadenceUpdaterWorker) Work(_ context.Context, _ *river.Job[consumerjobs.CadenceUpdaterJobArgs]) error {
	return nil
}

type noopFollowUpManagerWorker struct {
	river.WorkerDefaults[consumerjobs.FollowUpManagerJobArgs]
}

func (*noopFollowUpManagerWorker) Work(_ context.Context, _ *river.Job[consumerjobs.FollowUpManagerJobArgs]) error {
	return nil
}

type noopRematchDispatcherWorker struct {
	river.WorkerDefaults[consumerjobs.RematchDispatcherJobArgs]
}

func (*noopRematchDispatcherWorker) Work(_ context.Context, _ *river.Job[consumerjobs.RematchDispatcherJobArgs]) error {
	return nil
}

// ============================================================================
// Helpers
// ============================================================================

func createManagedCadenceTask(t *testing.T, env *atomicTxTestEnv, namePrefix string) (*repository.Contact, *repository.ContactTask) {
	t.Helper()

	cadenceStr := "monthly"
	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: namePrefix + " " + uuid.New().String()[:8],
		Cadence:  &cadenceStr,
	})
	require.NoError(t, err)

	// Seed contact_by so skip handler can advance it.
	contactBy := accelerated.GetCurrentTime().UTC().Truncate(24*time.Hour).AddDate(0, 0, 7)
	require.NoError(t, env.contactRepo.UpdateContactBy(env.ctx, contact.ID, contactBy))
	reloaded, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)

	extID := "td-atomic-" + uuid.New().String()[:8]
	task, err := env.contactTaskRepo.CreateContactTask(env.ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       SourceName,
		Kind:           TaskKindCadence,
		ExternalTaskID: extID,
		State:          string(repository.ContactTaskStateManaged),
		Metadata:       map[string]any{},
	})
	require.NoError(t, err)

	return reloaded, task
}

// ============================================================================
// Tests
// ============================================================================

// TestTodoist_HandleTaskCompletion_RollsBackEventAndStateOnDBFailure verifies
// that an injected DB failure in UpdateContactTaskStateTx rolls back both the
// event row and the state transition together.
func TestTodoist_HandleTaskCompletion_RollsBackEventAndStateOnDBFailure(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	contact, task := createManagedCadenceTask(t, env, "CompleteFail")

	env.faultyTaskWriter.mu.Lock()
	env.faultyTaskWriter.faultyMethod = "UpdateContactTaskStateTx"
	env.faultyTaskWriter.mu.Unlock()

	r := env.provider.handleTaskCompletion(env.ctx, SyncItem{
		ID:      task.ExternalTaskID,
		Checked: true,
	}, task, contact, env.settings, env.accountID)

	require.Error(t, r.Err, "handleTaskCompletion must propagate the injected DB error")
	assert.Contains(t, r.Err.Error(), "update task state")

	// No event row should be in the DB.
	_, err := env.eventRepo.FindEventBySource(env.ctx, SourceName, task.ID.String())
	assert.ErrorIs(t, err, db.ErrNotFound, "event row must have rolled back")

	// Task state still 'managed'.
	reloaded, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateManaged, reloaded.State)
}

// TestTodoist_HandleSkipTrigger_RollsBackEventAndStateOnDBFailure verifies
// that an injected failure in UpdateContactByTx rolls back the event row,
// contact_by, and metadata all together.
func TestTodoist_HandleSkipTrigger_RollsBackEventAndStateOnDBFailure(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	contact, task := createManagedCadenceTask(t, env, "SkipFail")
	beforeContactBy := contact.ContactBy

	env.faultyContactWriterW.mu.Lock()
	env.faultyContactWriterW.faultyMethod = "UpdateContactByTx"
	env.faultyContactWriterW.mu.Unlock()

	item := SyncItem{ID: task.ExternalTaskID, UpdatedAt: "2026-04-04T10:00:00Z"}
	r := env.provider.handleSkipTrigger(env.ctx, item, task, contact, env.settings, env.accountID)

	require.Error(t, r.Err)
	assert.Contains(t, r.Err.Error(), "update contact_by")
	assert.Nil(t, r.Commands, "no item_add command must be returned on rollback")

	// Event row not present.
	expectedSourceID := fmt.Sprintf("%s:%s", task.ID.String(), item.UpdatedAt)
	_, err := env.eventRepo.FindEventBySource(env.ctx, SourceName, expectedSourceID)
	assert.ErrorIs(t, err, db.ErrNotFound, "event row must have rolled back")

	// contact_by unchanged.
	reloadedContact, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)
	if beforeContactBy == nil {
		assert.Nil(t, reloadedContact.ContactBy)
	} else {
		require.NotNil(t, reloadedContact.ContactBy)
		assert.True(t, beforeContactBy.Equal(*reloadedContact.ContactBy),
			"contact_by must be unchanged after rollback")
	}

	// Metadata unchanged.
	reloadedTask, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	_, hasPending := reloadedTask.Metadata[MetadataKeyPendingTempID]
	assert.False(t, hasPending, "pending_temp_id must not be set after rollback")
}

// TestTodoist_HandleSkipTrigger_ReplayIsNoOpAtEventLayer is the
// spec-mandated "second skip ran twice on replay is now impossible" test.
// Invokes the handler twice with identical (task, UpdatedAt); the second
// call hits the event table's (source, source_id) unique constraint, the
// bus returns env.ID=uuid.Nil, and the handler short-circuits.
func TestTodoist_HandleSkipTrigger_ReplayIsNoOpAtEventLayer(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	contact, task := createManagedCadenceTask(t, env, "SkipReplay")

	item := SyncItem{ID: task.ExternalTaskID, UpdatedAt: "2026-04-05T10:00:00Z"}

	// First call succeeds.
	r1 := env.provider.handleSkipTrigger(env.ctx, item, task, contact, env.settings, env.accountID)
	require.NoError(t, r1.Err)
	require.Len(t, r1.Commands, 1)

	after1, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, after1.ContactBy)

	// Reload task to pick up the committed metadata.
	taskReloaded, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	contactReloaded, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)

	// Second call with same item — real bus returns (source, source_id)
	// duplicate, env.ID=uuid.Nil; handler short-circuits.
	r2 := env.provider.handleSkipTrigger(env.ctx, item, taskReloaded, contactReloaded, env.settings, env.accountID)
	require.NoError(t, r2.Err)
	assert.True(t, r2.Processed)
	assert.Nil(t, r2.Commands, "replay must NOT return a second item_add command")

	// Exactly one event row.
	expectedSourceID := fmt.Sprintf("%s:%s", task.ID.String(), item.UpdatedAt)
	evt, err := env.eventRepo.FindEventBySource(env.ctx, SourceName, expectedSourceID)
	require.NoError(t, err)
	assert.Equal(t, events.KindTaskSkipped, evt.Kind)

	// contact_by did NOT advance a second time.
	after2, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, after2.ContactBy)
	assert.True(t, after1.ContactBy.Equal(*after2.ContactBy),
		"contact_by must not advance on replay at the event-table unique")
}

// TestTodoist_HandleTaskCompletion_DuplicateIsIdempotent verifies the
// completion replay scenario against the real event-table unique index.
func TestTodoist_HandleTaskCompletion_DuplicateIsIdempotent(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	contact, task := createManagedCadenceTask(t, env, "CompleteReplay")

	r1 := env.provider.handleTaskCompletion(env.ctx, SyncItem{
		ID:      task.ExternalTaskID,
		Checked: true,
	}, task, contact, env.settings, env.accountID)
	require.NoError(t, r1.Err)

	// Second call — task state is already 'completed', but the handler
	// runs anyway in this test (we pass the pre-first-call task pointer).
	// The bus dedup via (source, source_id) unique returns env.ID=Nil;
	// handler short-circuits without re-writing state.
	r2 := env.provider.handleTaskCompletion(env.ctx, SyncItem{
		ID:      task.ExternalTaskID,
		Checked: true,
	}, task, contact, env.settings, env.accountID)
	require.NoError(t, r2.Err)
	assert.True(t, r2.Processed)

	// Exactly one event row.
	evt, err := env.eventRepo.FindEventBySource(env.ctx, SourceName, task.ID.String())
	require.NoError(t, err)
	assert.Equal(t, events.KindTaskCompleted, evt.Kind)
}

// TestTodoist_TryRecoverPendingTempID_RollsBackOnDBFailure verifies that an
// injected failure on UpdateContactTaskMetadataTx (after external_id update
// succeeded inside the same tx) rolls back both writes atomically.
func TestTodoist_TryRecoverPendingTempID_RollsBackOnDBFailure(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	contact, task := createManagedCadenceTask(t, env, "Recover")

	// Seed pending_temp_id + put task in pre-recovery state where
	// ExternalTaskID is a temp and pending_temp_id matches (historical
	// shape).
	tempID := "temp-recover-" + uuid.New().String()[:8]
	_, err := env.contactTaskRepo.UpdateContactTaskExternalID(env.ctx, task.ID, tempID)
	require.NoError(t, err)
	_, err = env.contactTaskRepo.UpdateContactTaskMetadata(env.ctx, task.ID, map[string]any{
		MetadataKeyPendingTempID: tempID,
	})
	require.NoError(t, err)

	env.faultyTaskWriter.mu.Lock()
	env.faultyTaskWriter.faultyMethod = "UpdateContactTaskMetadataTx"
	env.faultyTaskWriter.mu.Unlock()

	realID := "real-" + uuid.New().String()[:8]
	// Description carries the CRM marker so tryRecoverPendingTempID
	// matches the contact.
	marker := fmt.Sprintf(`{"crm":true,"contact_id":"%s","kind":"cadence","instance":"x"}`, contact.ID.String())
	syncItem := SyncItem{
		ID:          realID,
		Description: marker,
	}

	_ = env.provider.tryRecoverPendingTempID(env.ctx, syncItem)

	// Both writes rolled back: external_task_id still the temp, metadata
	// still has pending_temp_id.
	reloaded, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, tempID, reloaded.ExternalTaskID, "external_task_id must not advance after rollback")
	pending, _ := reloaded.Metadata[MetadataKeyPendingTempID].(string)
	assert.Equal(t, tempID, pending, "pending_temp_id must remain after rollback")
}

// TestTodoist_ProcessTempIDMappings_AtomicMappingClear verifies that an
// injected failure on the metadata-clear step inside the per-task tx rolls
// both writes back — external_task_id stays at the temp, pending_temp_id
// remains set.
func TestTodoist_ProcessTempIDMappings_AtomicMappingClear(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	_, task := createManagedCadenceTask(t, env, "TempMap")

	tempID := "temp-map-" + uuid.New().String()[:8]
	_, err := env.contactTaskRepo.UpdateContactTaskExternalID(env.ctx, task.ID, tempID)
	require.NoError(t, err)
	_, err = env.contactTaskRepo.UpdateContactTaskMetadata(env.ctx, task.ID, map[string]any{
		MetadataKeyPendingTempID: tempID,
	})
	require.NoError(t, err)

	env.faultyTaskWriter.mu.Lock()
	env.faultyTaskWriter.faultyMethod = "UpdateContactTaskMetadataTx"
	env.faultyTaskWriter.mu.Unlock()

	realID := "real-map-" + uuid.New().String()[:8]
	rolledBack := env.provider.processTempIDMappings(env.ctx, map[string]string{tempID: realID})
	assert.True(t, rolledBack, "processTempIDMappings must report rolledBack=true when a tx fails")

	reloaded, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, tempID, reloaded.ExternalTaskID, "external_task_id must not advance after rollback")
	pending, _ := reloaded.Metadata[MetadataKeyPendingTempID].(string)
	assert.Equal(t, tempID, pending, "pending_temp_id must remain after rollback")
}

// TestTodoist_ProcessTempIDMappings_RollbackRecoversViaTryRecoverPendingTempID
// verifies the end-to-end recovery chain: first tick's mapping tx rolls
// back; next tick's syncResp.Items delivers the real item, and
// tryRecoverPendingTempID finalizes the mapping atomically.
func TestTodoist_ProcessTempIDMappings_RollbackRecoversViaTryRecoverPendingTempID(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	contact, task := createManagedCadenceTask(t, env, "RecoverChain")

	tempID := "temp-chain-" + uuid.New().String()[:8]
	_, err := env.contactTaskRepo.UpdateContactTaskExternalID(env.ctx, task.ID, tempID)
	require.NoError(t, err)
	_, err = env.contactTaskRepo.UpdateContactTaskMetadata(env.ctx, task.ID, map[string]any{
		MetadataKeyPendingTempID: tempID,
	})
	require.NoError(t, err)

	// First tick: mapping tx fails on metadata clear.
	env.faultyTaskWriter.mu.Lock()
	env.faultyTaskWriter.faultyMethod = "UpdateContactTaskMetadataTx"
	env.faultyTaskWriter.fired = 0
	env.faultyTaskWriter.mu.Unlock()

	realID := "real-chain-" + uuid.New().String()[:8]
	rolledBack := env.provider.processTempIDMappings(env.ctx, map[string]string{tempID: realID})
	require.True(t, rolledBack)

	// State after first tick: ExternalTaskID still temp, pending_temp_id
	// still set.
	afterTick1, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	require.Equal(t, tempID, afterTick1.ExternalTaskID)

	// Second tick: clear the fault (fireLimit is 1, and we've already
	// triggered once). tryRecoverPendingTempID runs against an item with
	// the CRM marker pointing at this contact.
	marker := fmt.Sprintf(`{"crm":true,"contact_id":"%s","kind":"cadence","instance":"x"}`, contact.ID.String())
	recovered := env.provider.tryRecoverPendingTempID(env.ctx, SyncItem{
		ID:          realID,
		Description: marker,
	})
	require.NotNil(t, recovered, "tryRecoverPendingTempID must match the CRM marker and recover")

	// State after recovery: ExternalTaskID is real, pending_temp_id cleared.
	afterTick2, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, realID, afterTick2.ExternalTaskID)
	_, hasPending := afterTick2.Metadata[MetadataKeyPendingTempID]
	assert.False(t, hasPending, "pending_temp_id must be cleared after recovery")
}

// TestReconcileExistingTask_SkipDriftRecovery_MetadataWriteFailure verifies
// that when the pre-emit metadata write fails, the reconcile branch does
// NOT emit commands (holding the new TempID back so the next tick retries
// from a consistent state).
func TestReconcileExistingTask_SkipDriftRecovery_MetadataWriteFailure(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	contact, task := createManagedCadenceTask(t, env, "SkipDriftMetaFail")

	// Seed the "skip-drift detected" state.
	origTempID := "temp-metafail-" + uuid.New().String()[:8]
	_, err := env.contactTaskRepo.UpdateContactTaskMetadata(env.ctx, task.ID, map[string]any{
		MetadataKeyPendingTempID:  origTempID,
		MetadataKeySyncedDeadline: "2027-03-15",
	})
	require.NoError(t, err)
	reloaded, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)

	env.faultyTaskWriter.mu.Lock()
	env.faultyTaskWriter.faultyMethod = "UpdateContactTaskMetadata"
	env.faultyTaskWriter.mu.Unlock()

	cmds := env.provider.reconcileExistingTask(env.ctx, reloaded, contact, env.settings, "2027-04-15", false)

	// No commands emitted because the pre-emit metadata write failed.
	assert.Empty(t, cmds, "skip-drift branch must not emit commands when metadata write fails")

	// pending_temp_id unchanged (still the stale one — new one was never
	// persisted).
	afterFail, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	pending, _ := afterFail.Metadata[MetadataKeyPendingTempID].(string)
	assert.Equal(t, origTempID, pending)
}
