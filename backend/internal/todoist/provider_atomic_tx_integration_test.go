package todoist

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/contacttask"
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
// injected error from a named method. The fault fires on the
// fireOnInvocation-th matching call (1-indexed). Once fired, subsequent
// calls delegate normally unless faultyMethod is reset. fireLimit extends
// this so N sequential firings trigger; default 1. Safe for concurrent use.
type faultyContactTaskWriter struct {
	contactTaskWriter
	mu               sync.Mutex
	faultyMethod     string
	fireOnInvocation int // 0 = first matching call; N = Nth matching call (0-indexed skip count)
	fireLimit        int // 0 = fire once; N = fire up to N times
	invocations      int
	fired            int
}

func (f *faultyContactTaskWriter) shouldFire(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.faultyMethod != method {
		return false
	}
	invIdx := f.invocations
	f.invocations++
	if invIdx < f.fireOnInvocation {
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

// atomicTxTestEnv bundles the live bus + faulty writers + pool so tests
// can construct a provider with the exact fault-injection behavior they
// need.
type atomicTxTestEnv struct {
	ctx              context.Context
	database         *db.Database
	pool             *pgxpool.Pool
	bus              *events.Bus
	eventRepo        *repository.EventRepository
	contactRepo      *repository.ContactRepository
	contactTaskRepo  *repository.ContactTaskRepository
	faultyTaskWriter *faultyContactTaskWriter
	cadenceFake      *fakeCadenceOverrider
	provider         *CadenceSyncProvider
	settings         Settings
	accountID        string
	riverClient      *river.Client[pgx.Tx]
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

	// Migrations are applied once into the template; the per-package clone
	// (testdb.SetupPackage in testmain_integration_test.go) inherits the
	// fully-migrated schema, so no inline db.RunMigrations is needed here.
	ctx := context.Background()

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
	cadenceFake := &fakeCadenceOverrider{contactRepo: contactRepo}
	provider := NewCadenceSyncProvider(
		stubOAuthProvider{},
		faultyTaskWriter,
		contactRepo,
		nil,
		config.TestConfig(),
		bus,
		cadenceFake,
		database.Pool,
		DefaultClientFactory,
		riverClient,
		true,
	)

	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = riverClient.Stop(stopCtx)
		database.Close()
	}

	return &atomicTxTestEnv{
		ctx:              ctx,
		database:         database,
		pool:             database.Pool,
		bus:              bus,
		eventRepo:        eventRepo,
		contactRepo:      contactRepo,
		contactTaskRepo:  contactTaskRepo,
		faultyTaskWriter: faultyTaskWriter,
		cadenceFake:      cadenceFake,
		provider:         provider,
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
// Stub OAuth provider + scripted mock Client for end-to-end Sync tests.
// ============================================================================

// stubOAuthProvider returns a fixed access token and claims an account
// exists. Used so tests can drive the real Sync method without OAuth
// fixtures.
type stubOAuthProvider struct{}

func (stubOAuthProvider) GetAccessToken(_ context.Context, _ string) (string, error) {
	return "stub-token", nil
}

func (stubOAuthProvider) HasAnyAccount(_ context.Context) bool { return true }

// scriptedClient is a Client implementation that replays a queue of
// pre-canned sync responses and records all outbound command batches.
// Matches the Client interface in sync.go. Safe for single-threaded use.
type scriptedClient struct {
	responses []*SyncResponse
	errors    []error
	callIdx   int
	// batches captures the commands passed to each client.Sync call (one
	// entry per call). Tests assert over this to verify the skip-drift
	// branch did not emit a duplicate item_add.
	batches [][]SyncCommand
	// onCall is invoked after computing the response for call callIdx
	// with the outbound commands and the (mutable) response pointer, so
	// tests can patch the response based on what the batch contains
	// (e.g., fill in temp_id_mapping from a temp_id the handler just
	// generated).
	onCall func(callIdx int, commands []SyncCommand, resp *SyncResponse)
}

func (c *scriptedClient) QuickAdd(_ context.Context, _ string, _ string) (*QuickAddTask, error) {
	return &QuickAddTask{ID: "stub-quick-add"}, nil
}

func (c *scriptedClient) Sync(_ context.Context, _ string, _ []string, commands []SyncCommand) (*SyncResponse, error) {
	c.batches = append(c.batches, append([]SyncCommand{}, commands...))
	idx := c.callIdx
	c.callIdx++
	if idx < len(c.errors) && c.errors[idx] != nil {
		return nil, c.errors[idx]
	}
	var resp *SyncResponse
	if idx < len(c.responses) {
		resp = c.responses[idx]
	} else {
		resp = &SyncResponse{SyncToken: "stub-token"}
	}
	if c.onCall != nil {
		c.onCall(idx, commands, resp)
	}
	return resp, nil
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
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleCadenceDue,
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
// that an injected failure on the cadence-override write rolls back the
// event row, contact_by, and metadata all together.
func TestTodoist_HandleSkipTrigger_RollsBackEventAndStateOnDBFailure(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	contact, task := createManagedCadenceTask(t, env, "SkipFail")
	beforeContactBy := contact.ContactBy

	env.cadenceFake.mu.Lock()
	env.cadenceFake.faultyMethod = "ApplyContactByOverride"
	env.cadenceFake.mu.Unlock()

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

	recovered, recoveryFailed := env.provider.tryRecoverPendingTempID(env.ctx, syncItem)
	_ = recovered
	assert.True(t, recoveryFailed, "recoveryFailed must surface so Sync can defer skip-drift")

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
	recovered, recoveryFailed := env.provider.tryRecoverPendingTempID(env.ctx, SyncItem{
		ID:          realID,
		Description: marker,
	})
	require.NotNil(t, recovered, "tryRecoverPendingTempID must match the CRM marker and recover")
	assert.False(t, recoveryFailed, "second-tick recovery must succeed")

	// State after recovery: ExternalTaskID is real, pending_temp_id cleared.
	afterTick2, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, realID, afterTick2.ExternalTaskID)
	_, hasPending := afterTick2.Metadata[MetadataKeyPendingTempID]
	assert.False(t, hasPending, "pending_temp_id must be cleared after recovery")
}

// TestTodoist_ProcessItems_PropagatesRecoveryFailed verifies that a
// tryRecoverPendingTempID rollback in one item propagates through
// processItem's result into processItems's aggregated recoveryFailed
// return — so Sync forces deferSkipDrift=true for this tick.
func TestTodoist_ProcessItems_PropagatesRecoveryFailed(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	contact, task := createManagedCadenceTask(t, env, "RecoverFailProp")

	// Put task in the "needs recovery" shape: ExternalTaskID is a temp,
	// pending_temp_id matches it.
	tempID := "temp-prop-" + uuid.New().String()[:8]
	_, err := env.contactTaskRepo.UpdateContactTaskExternalID(env.ctx, task.ID, tempID)
	require.NoError(t, err)
	_, err = env.contactTaskRepo.UpdateContactTaskMetadata(env.ctx, task.ID, map[string]any{
		MetadataKeyPendingTempID: tempID,
	})
	require.NoError(t, err)

	// Faulty metadata-clear inside the recovery tx.
	env.faultyTaskWriter.mu.Lock()
	env.faultyTaskWriter.faultyMethod = "UpdateContactTaskMetadataTx"
	env.faultyTaskWriter.mu.Unlock()

	// Sync item carries a CRM marker that matches the contact — this is
	// the "real Todoist id just delivered in syncResp.Items" case where
	// GetContactTaskByExternalID misses (the local row still has the temp
	// id) and processItem falls back to tryRecoverPendingTempID.
	realID := "real-prop-" + uuid.New().String()[:8]
	marker := fmt.Sprintf(`{"crm":true,"contact_id":"%s","kind":"cadence","instance":"x"}`, contact.ID.String())

	items := []SyncItem{{
		ID:          realID,
		Description: marker,
		Labels:      []string{env.settings.LabelName},
		Deadline:    &SyncDate{Date: contact.ContactBy.Format(DateFormat)},
		UpdatedAt:   "2026-04-08T10:00:00Z",
	}}

	_, _, recoveryFailed, err := env.provider.processItems(env.ctx, items, env.settings, env.accountID)
	require.NoError(t, err, "processItems should not error — recovery failure is non-fatal")
	assert.True(t, recoveryFailed, "processItems must propagate RecoveryFailed up from processItem")
}

// TestTodoist_ProcessTempIDMappings_RollbackDefersSkipDriftOneTick simulates
// the round-trip where:
//   - handleSkipTrigger commits (state advance + pending_temp_id set).
//   - Todoist HTTP batch succeeds remotely (simulated by calling
//     processTempIDMappings with a temp_id_mapping).
//   - The mapping tx rolls back (faulty metadata-clear).
//   - processTempIDMappings returns rolledBack=true.
//   - Same-tick reconcileContactTasks invoked with deferSkipDrift=true
//     must NOT emit a duplicate item_add, even though the stale local row
//     matches the skip-drift detection predicate.
//
// This is the key correctness test for the rollback-deferral behavior.
func TestTodoist_ProcessTempIDMappings_RollbackDefersSkipDriftOneTick(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	contact, task := createManagedCadenceTask(t, env, "DeferOneTick")

	// Simulate handleSkipTrigger's committed state — pending_temp_id set
	// with a new temp id that differs from ExternalTaskID.
	oldExtID := task.ExternalTaskID
	newTempID := "temp-new-" + uuid.New().String()[:8]
	_, err := env.contactTaskRepo.UpdateContactTaskMetadata(env.ctx, task.ID, map[string]any{
		MetadataKeyPendingTempID:  newTempID,
		MetadataKeySyncedDeadline: contact.ContactBy.Format(DateFormat),
	})
	require.NoError(t, err)

	// Mapping tx rolls back on metadata clear.
	env.faultyTaskWriter.mu.Lock()
	env.faultyTaskWriter.faultyMethod = "UpdateContactTaskMetadataTx"
	env.faultyTaskWriter.mu.Unlock()

	realID := "real-defer-" + uuid.New().String()[:8]
	rolledBack := env.provider.processTempIDMappings(env.ctx, map[string]string{newTempID: realID})
	require.True(t, rolledBack, "mapping tx must have rolled back")

	// State after rollback: ExternalTaskID unchanged (still oldExtID),
	// pending_temp_id unchanged (still newTempID). Detection predicate
	// (pending_temp_id != "" && pending_temp_id != ExternalTaskID) is TRUE.
	afterMapping, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, oldExtID, afterMapping.ExternalTaskID)
	pending, _ := afterMapping.Metadata[MetadataKeyPendingTempID].(string)
	assert.Equal(t, newTempID, pending)

	// Same-tick reconcile with deferSkipDrift=true — skip-drift branch
	// must NOT fire (no item_close + item_add emitted from it).
	syncedDeadline, _ := afterMapping.Metadata[MetadataKeySyncedDeadline].(string)
	cmds := env.provider.reconcileExistingTask(env.ctx, afterMapping, contact, env.settings, syncedDeadline, true /* deferSkipDrift */)

	for _, c := range cmds {
		assert.NotEqual(t, "item_close", c.Type, "deferral must suppress duplicate item_close")
		assert.NotEqual(t, "item_add", c.Type, "deferral must suppress duplicate item_add")
	}

	// Metadata pending_temp_id unchanged (branch did not run).
	afterReconcile, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	stillPending, _ := afterReconcile.Metadata[MetadataKeyPendingTempID].(string)
	assert.Equal(t, newTempID, stillPending, "pending_temp_id must be unchanged under deferral")
}

// TestTodoist_SkipDrift_ReconcileRecovery exercises the end-to-end skip-drift
// self-healing path: when pending_temp_id mismatches ExternalTaskID and
// deferSkipDrift=false, reconcileExistingTask emits item_close + item_add
// so the next HTTP batch retries the replacement. This is the "HTTP batch
// truly failed" case where the remote task was never created.
func TestTodoist_SkipDrift_ReconcileRecovery(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	contact, task := createManagedCadenceTask(t, env, "SkipDriftReconcile")

	oldExtID := task.ExternalTaskID
	staleTempID := "temp-stale-" + uuid.New().String()[:8]
	syncedDeadline := contact.ContactBy.Format(DateFormat)
	_, err := env.contactTaskRepo.UpdateContactTaskMetadata(env.ctx, task.ID, map[string]any{
		MetadataKeyPendingTempID:  staleTempID,
		MetadataKeySyncedDeadline: syncedDeadline,
	})
	require.NoError(t, err)

	afterSkip, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)

	// Reconcile with deferSkipDrift=false — branch must fire.
	cmds := env.provider.reconcileExistingTask(env.ctx, afterSkip, contact, env.settings, syncedDeadline, false)

	require.Len(t, cmds, 2, "skip-drift branch must emit item_close + item_add")
	assert.Equal(t, "item_close", cmds[0].Type)
	assert.Equal(t, oldExtID, cmds[0].Args["id"])
	assert.Equal(t, "item_add", cmds[1].Type)

	// pending_temp_id updated to the new item_add TempID.
	afterRecovery, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	newPending, _ := afterRecovery.Metadata[MetadataKeyPendingTempID].(string)
	assert.Equal(t, cmds[1].TempID, newPending)
	assert.NotEqual(t, staleTempID, newPending)
}

// TestTodoist_Sync_ReconcileDefersSkipDriftAfterMappingRollback drives the
// full provider.Sync(...) method end-to-end. Flow:
//   - Sync pulls items — returns a single SyncItem that is a skip trigger
//     (label removed on a managed cadence task).
//   - processItems dispatches to handleSkipTrigger, which commits state +
//     advances contact_by + emits an item_add command.
//   - Sync's post-items batch sends item_add to the mock; response
//     returns a temp_id_mapping for that task.
//   - processTempIDMappings runs — faulty metadata-clear rolls the tx
//     back; rolledBack=true.
//   - reconcileContactTasks is called with deferSkipDrift=true. Skip-drift
//     branch must NOT emit a duplicate item_add against the already-
//     created remote task.
//
// Assertion: across all mock.Sync calls, exactly ONE item_add for this
// contact is emitted (from handleSkipTrigger). Without the
// mappingRolledBack → deferSkipDrift wiring, reconcileExistingTask's
// skip-drift branch would fire during reconcile and emit a second
// item_add, failing this assertion.
func TestTodoist_Sync_ReconcileDefersSkipDriftAfterMappingRollback(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	contact, task := createManagedCadenceTask(t, env, "SyncE2E")
	// Seed synced_deadline so reconcileExistingTask has a stable baseline.
	syncedDeadline := contact.ContactBy.Format(DateFormat)
	_, err := env.contactTaskRepo.UpdateContactTaskMetadata(env.ctx, task.ID, map[string]any{
		MetadataKeySyncedDeadline: syncedDeadline,
	})
	require.NoError(t, err)

	// The single sync item is the task with its CRM label removed — a
	// skip trigger. handleSkipTrigger will commit + return an item_add.
	syncItem := SyncItem{
		ID:        task.ExternalTaskID,
		ProjectID: env.settings.ProjectID,
		Labels:    []string{}, // CRM label removed
		Deadline:  &SyncDate{Date: syncedDeadline},
		UpdatedAt: "2026-04-09T10:00:00Z",
	}

	// The scripted client must accept at least 3 Sync calls:
	//   call 0: items pull → returns our skip-trigger item.
	//   call 1: post-items batch → item_add executed remotely, returns
	//           temp_id_mapping for the new task.
	//   call 2: reconcile batch (if any). Set empty response.
	// Responses are replayed in order by scriptedClient.Sync.
	mock := &scriptedClient{}
	mock.responses = []*SyncResponse{
		// 0: items pull.
		{SyncToken: "tok1", Items: []SyncItem{syncItem}},
		// 1: post-items batch. TempIDMap maps the item_add's temp_id to
		// a real id. The mock fills this in based on what the batch
		// contains (see below — we pre-seed with a placeholder that the
		// test overrides after building the batch).
		{SyncToken: "tok2", TempIDMap: map[string]string{}},
		// 2: reconcile batch — must return empty command set.
		{SyncToken: "tok3"},
	}

	// Fault the SECOND UpdateContactTaskMetadataTx call. Call sequence:
	//   1. handleSkipTrigger writes pending_temp_id=<new>.
	//   2. processTempIDMappings clears pending_temp_id — this is the
	//      one we want to fail.
	env.faultyTaskWriter.mu.Lock()
	env.faultyTaskWriter.faultyMethod = "UpdateContactTaskMetadataTx"
	env.faultyTaskWriter.fireOnInvocation = 1 // skip first call
	env.faultyTaskWriter.mu.Unlock()

	// Wire a fresh provider that uses our scripted mock via the factory
	// AND uses the env's faulty writers.
	provider := NewCadenceSyncProvider(
		stubOAuthProvider{},
		env.faultyTaskWriter,
		env.contactRepo,
		nil,
		config.TestConfig(),
		env.bus,
		env.cadenceFake,
		env.pool,
		func(_ string) Client { return mock },
		env.riverClient,
		true,
	)

	// Prime the post-items TempIDMap by pre-registering the temp_id.
	// handleSkipTrigger generates its own uuid for the replacement
	// item_add's TempID, so we can't know it upfront. Use a proxy:
	// after the first call, the scripted mock's batches[0] is empty
	// (items pull), batches[1] contains the item_add whose TempID we
	// copy into the second response's TempIDMap. But we can't mutate
	// responses mid-Sync. Workaround: on the second Sync call the mock
	// reads the current batch, and we override the response's TempIDMap
	// to include the actual temp id from the batch.

	// Use a scripted-with-callback: we replace responses[1] with a value
	// computed from batches[1] on the fly. Easiest: wrap Sync to observe
	// batch 1 and patch the response.
	mock.onCall = func(callIdx int, commands []SyncCommand, resp *SyncResponse) {
		if callIdx == 1 {
			for _, c := range commands {
				if c.Type == "item_add" && c.TempID != "" {
					resp.TempIDMap = map[string]string{
						c.TempID: "real-" + uuid.New().String()[:8],
					}
				}
			}
		}
	}

	accountID := env.accountID
	syncCursor := "*"
	state := &repository.SyncState{
		ID:         uuid.New(),
		Source:     SourceName,
		AccountID:  &accountID,
		SyncCursor: &syncCursor,
		Metadata: map[string]any{
			MetadataKeyProjectID: env.settings.ProjectID,
			MetadataKeyLabelID:   env.settings.LabelID,
			MetadataKeyLabelName: env.settings.LabelName,
		},
	}

	result, err := provider.Sync(env.ctx, state, []repository.Contact{*contact})
	require.NoError(t, err)
	assert.Equal(t, 1, result.ItemsProcessed, "handleSkipTrigger should have processed the one skip item")

	// Assert: across all Sync-call batches, exactly ONE item_add was
	// emitted for THIS contact's cadence task. Without the mappingRolledBack
	// → deferSkipDrift wiring, reconcile would emit a second item_add
	// (item_close(old) + item_add(temp-newer)) for the same row.
	//
	// The test DB is shared — other contacts with cadence (from previous
	// tests or the test suite's own fixtures) may also emit item_adds
	// during reconcileContactTasks. Scope the count to our contact by
	// matching the CRM marker's contact_id in the description.
	contactMarker := contact.ID.String()
	itemAddsForContact := 0
	for _, batch := range mock.batches {
		for _, c := range batch {
			if c.Type != "item_add" {
				continue
			}
			desc, _ := c.Args["description"].(string)
			if strings.Contains(desc, contactMarker) {
				itemAddsForContact++
			}
		}
	}
	assert.Equal(t, 1, itemAddsForContact,
		"only handleSkipTrigger's item_add should emit for this contact; skip-drift must be deferred")

	// Also assert: no item_close for this task's old external id was
	// emitted (reconcile's skip-drift branch would emit that alongside
	// item_add).
	itemCloseForTask := 0
	for _, batch := range mock.batches {
		for _, c := range batch {
			if c.Type == "item_close" {
				if id, _ := c.Args["id"].(string); id == task.ExternalTaskID {
					itemCloseForTask++
				}
			}
		}
	}
	assert.Equal(t, 0, itemCloseForTask, "deferral must suppress skip-drift item_close for this task")
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

// makeStaleDeadlineSyncState builds a *repository.SyncState configured the
// way the test harness expects (account ID + cursor + project/label
// metadata), so the gate / reconcile end-to-end tests can drive Sync().
func makeStaleDeadlineSyncState(env *atomicTxTestEnv) *repository.SyncState {
	accountID := env.accountID
	syncCursor := "*"
	return &repository.SyncState{
		ID:         uuid.New(),
		Source:     SourceName,
		AccountID:  &accountID,
		SyncCursor: &syncCursor,
		Metadata: map[string]any{
			MetadataKeyProjectID: env.settings.ProjectID,
			MetadataKeyLabelID:   env.settings.LabelID,
			MetadataKeyLabelName: env.settings.LabelName,
		},
	}
}

// TestSync_StaleTodoistDeadline_OutreachRecovery is sub-case 4a: full Sync()
// end-to-end where the user PATCHed /last-contacted, advancing both
// contact_by and last_outreach_at, and the next Todoist incremental sync
// re-delivers the cadence task at its still-stale deadline. The gate must
// hold (contact_by stays advanced through processItem) and reconcile's
// closeOnOutreach branch must fire to close the old task — mirrors the
// prod recovery path for the affected contacts.
func TestSync_StaleTodoistDeadline_OutreachRecovery(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	cadenceStr := "weekly"
	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "Sync4a " + uuid.New().String()[:8],
		Cadence:  &cadenceStr,
	})
	require.NoError(t, err)

	// Pre-deploy state: synced_deadline + LastOutreachAt are on the OLD
	// values (matching what we last pushed). Then the user PATCHed
	// /last-contacted, which both advanced contact_by AND bumped
	// last_outreach_at via the mutual interaction path.
	staleDeadline := "2026-04-19"
	oldOutreach := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)
	advancedContactBy := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	advancedOutreach := time.Date(2026, 4, 23, 15, 0, 47, 0, time.UTC)

	cadenceExtID := "td-sync4a-" + uuid.New().String()[:8]
	_, err = env.contactTaskRepo.CreateContactTask(env.ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       SourceName,
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleCadenceDue,
		ExternalTaskID: cadenceExtID,
		State:          string(repository.ContactTaskStateManaged),
		Metadata: map[string]any{
			MetadataKeySyncedDeadline:       staleDeadline,
			MetadataKeySyncedLastOutreachAt: oldOutreach.Format(time.RFC3339),
		},
	})
	require.NoError(t, err)

	// Advance contact_by + last_outreach_at to simulate the post-PATCH
	// state. UpdateContactBy is the test fixture's seeding tool (still
	// allowed for fixtures); UpdateContactOutreachAt sets last_outreach_at.
	require.NoError(t, env.contactRepo.UpdateContactBy(env.ctx, contact.ID, advancedContactBy))
	require.NoError(t, env.contactRepo.UpdateContactOutreachAt(env.ctx, contact.ID, advancedOutreach, true))

	// Scripted client returns the cadence task with its stale deadline.
	syncItem := SyncItem{
		ID:        cadenceExtID,
		ProjectID: env.settings.ProjectID,
		Labels:    []string{env.settings.LabelName},
		Deadline:  &SyncDate{Date: staleDeadline},
		UpdatedAt: "2026-04-23T15:00:52Z",
	}
	mock := &scriptedClient{
		responses: []*SyncResponse{
			{SyncToken: "tok1", Items: []SyncItem{syncItem}},
			{SyncToken: "tok2"},
			{SyncToken: "tok3"},
		},
	}

	provider := NewCadenceSyncProvider(
		stubOAuthProvider{},
		env.faultyTaskWriter,
		env.contactRepo,
		nil,
		config.TestConfig(),
		env.bus,
		env.cadenceFake,
		env.pool,
		func(_ string) Client { return mock },
		env.riverClient,
		true,
	)

	state := makeStaleDeadlineSyncState(env)
	_, err = provider.Sync(env.ctx, state, []repository.Contact{*contact})
	require.NoError(t, err)

	// Gate held: contact_by is still the advanced value, NOT clobbered to
	// the stale Todoist deadline.
	reloaded, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, reloaded.ContactBy)
	assert.True(t, reloaded.ContactBy.Equal(advancedContactBy),
		"contact_by must remain advanced after Sync; want=%v got=%v",
		advancedContactBy, *reloaded.ContactBy)

	// closeOnOutreach fired: a NewItemCloseCommand for cadenceExtID
	// appears in some batch.
	closedForExtID := false
	for _, batch := range mock.batches {
		for _, c := range batch {
			if c.Type != "item_close" {
				continue
			}
			if id, _ := c.Args["id"].(string); id == cadenceExtID {
				closedForExtID = true
			}
		}
	}
	assert.True(t, closedForExtID, "closeOnOutreach must emit item_close for cadenceExtID")
}

// TestSync_StaleTodoistDeadline_CRMDriftPath is sub-case 4b: same gate
// scenario as 4a but last_outreach_at was NOT advanced (e.g., a
// future-dated cadence change rather than a Mark Contacted). The gate must
// hold; reconcileExistingTask's CRM-drift branch fires instead of
// closeOnOutreach, emitting close+create scoped to the seeded cadenceExtID.
func TestSync_StaleTodoistDeadline_CRMDriftPath(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	cadenceStr := "weekly"
	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "Sync4b " + uuid.New().String()[:8],
		Cadence:  &cadenceStr,
	})
	require.NoError(t, err)

	staleDeadline := "2026-04-19"
	advancedContactBy := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	stableOutreach := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)

	cadenceExtID := "td-sync4b-" + uuid.New().String()[:8]
	_, err = env.contactTaskRepo.CreateContactTask(env.ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       SourceName,
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleCadenceDue,
		ExternalTaskID: cadenceExtID,
		State:          string(repository.ContactTaskStateManaged),
		Metadata: map[string]any{
			MetadataKeySyncedDeadline: staleDeadline,
			// synced_last_outreach_at == contact.LastOutreachAt → no
			// outreach detected.
			MetadataKeySyncedLastOutreachAt: stableOutreach.Format(time.RFC3339),
		},
	})
	require.NoError(t, err)

	require.NoError(t, env.contactRepo.UpdateContactBy(env.ctx, contact.ID, advancedContactBy))
	require.NoError(t, env.contactRepo.UpdateContactOutreachAt(env.ctx, contact.ID, stableOutreach, true))

	syncItem := SyncItem{
		ID:        cadenceExtID,
		ProjectID: env.settings.ProjectID,
		Labels:    []string{env.settings.LabelName},
		Deadline:  &SyncDate{Date: staleDeadline},
		UpdatedAt: "2026-04-23T15:00:52Z",
	}
	mock := &scriptedClient{
		responses: []*SyncResponse{
			{SyncToken: "tok1", Items: []SyncItem{syncItem}},
			{SyncToken: "tok2"},
			{SyncToken: "tok3"},
		},
	}

	provider := NewCadenceSyncProvider(
		stubOAuthProvider{},
		env.faultyTaskWriter,
		env.contactRepo,
		nil,
		config.TestConfig(),
		env.bus,
		env.cadenceFake,
		env.pool,
		func(_ string) Client { return mock },
		env.riverClient,
		true,
	)

	state := makeStaleDeadlineSyncState(env)
	_, err = provider.Sync(env.ctx, state, []repository.Contact{*contact})
	require.NoError(t, err)

	// Gate held.
	reloaded, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, reloaded.ContactBy)
	assert.True(t, reloaded.ContactBy.Equal(advancedContactBy),
		"contact_by must remain advanced after Sync (CRM-drift path)")

	// CRM-drift branch fires: item_close for cadenceExtID + at least one
	// item_add scoped to this contact appear in the batches.
	closedForExtID := 0
	addsForContact := 0
	contactMarker := contact.ID.String()
	for _, batch := range mock.batches {
		for _, c := range batch {
			switch c.Type {
			case "item_close":
				if id, _ := c.Args["id"].(string); id == cadenceExtID {
					closedForExtID++
				}
			case "item_add":
				desc, _ := c.Args["description"].(string)
				if strings.Contains(desc, contactMarker) {
					addsForContact++
				}
			}
		}
	}
	assert.GreaterOrEqual(t, closedForExtID, 1, "CRM-drift branch must emit item_close for cadenceExtID")
	assert.GreaterOrEqual(t, addsForContact, 1, "CRM-drift branch must emit item_add for the contact")
}

// TestProcessItem_DeadlineEditTxFailure_RollsBackAndSurfacesErr is the
// critical tx-failure regression test. Setup is a legitimate Todoist-wins
// scenario (synced_deadline != item.Deadline AND item.Deadline !=
// contact.ContactBy). Failure injection on the metadata-write step inside
// the new pgx.BeginTxFunc proves that:
//   - The contact_by write rolls back even though ApplyContactByOverride
//     itself succeeded — this pins the tx atomicity fix.
//   - synced_deadline metadata is unchanged.
//   - processItemResult.Err is non-nil so processItems aborts the batch.
//   - Sync() returns with result.NewCursor == "" so the next tick replays
//     the same batch (cursor preserved at the SyncService layer).
func TestProcessItem_DeadlineEditTxFailure_RollsBackAndSurfacesErr(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	cadenceStr := "weekly"
	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "TxFail " + uuid.New().String()[:8],
		Cadence:  &cadenceStr,
	})
	require.NoError(t, err)

	// Legitimate-edit precondition: synced_deadline ≠ item.Deadline AND
	// item.Deadline ≠ contact.ContactBy.
	originalContactBy := time.Date(2027, 2, 3, 0, 0, 0, 0, time.UTC)
	require.NoError(t, env.contactRepo.UpdateContactBy(env.ctx, contact.ID, originalContactBy))

	cadenceExtID := "td-txfail-" + uuid.New().String()[:8]
	_, err = env.contactTaskRepo.CreateContactTask(env.ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       SourceName,
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleCadenceDue,
		ExternalTaskID: cadenceExtID,
		State:          string(repository.ContactTaskStateManaged),
		Metadata: map[string]any{
			MetadataKeySyncedDeadline: "2027-02-03",
		},
	})
	require.NoError(t, err)

	// Inject failure on the metadata-write step inside the new tx —
	// reproduces the original non-tx bug shape (first write succeeds,
	// second write fails) but inside the tx so the rollback is observable.
	env.faultyTaskWriter.mu.Lock()
	env.faultyTaskWriter.faultyMethod = "UpdateContactTaskMetadataTx"
	env.faultyTaskWriter.fired = 0
	env.faultyTaskWriter.invocations = 0
	env.faultyTaskWriter.mu.Unlock()

	syncItem := SyncItem{
		ID:        cadenceExtID,
		ProjectID: env.settings.ProjectID,
		Labels:    []string{env.settings.LabelName},
		Deadline:  &SyncDate{Date: "2026-02-24"},
		UpdatedAt: "2026-04-15T10:00:00Z",
	}
	mock := &scriptedClient{
		responses: []*SyncResponse{
			{SyncToken: "tok1", Items: []SyncItem{syncItem}},
			{SyncToken: "tok2"},
			{SyncToken: "tok3"},
		},
	}

	provider := NewCadenceSyncProvider(
		stubOAuthProvider{},
		env.faultyTaskWriter,
		env.contactRepo,
		nil,
		config.TestConfig(),
		env.bus,
		env.cadenceFake,
		env.pool,
		func(_ string) Client { return mock },
		env.riverClient,
		true,
	)

	state := makeStaleDeadlineSyncState(env)
	result, err := provider.Sync(env.ctx, state, []repository.Contact{*contact})
	require.Error(t, err, "Sync must surface the deadline-edit tx failure")
	assert.Contains(t, err.Error(), "deadline-edit tx")
	require.NotNil(t, result)
	assert.Equal(t, "", result.NewCursor,
		"result.NewCursor must remain empty so SyncService preserves the pre-batch cursor")

	// contact_by rolled back to the original CRM value despite
	// ApplyContactByOverride succeeding inside the tx.
	reloaded, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, reloaded.ContactBy)
	assert.True(t, reloaded.ContactBy.Equal(originalContactBy),
		"contact_by must roll back to its pre-call value; want=%v got=%v",
		originalContactBy, *reloaded.ContactBy)

	// synced_deadline still on its pre-call value — second write failed.
	reloadedTask, err := env.contactTaskRepo.GetContactTaskByExternalID(env.ctx, SourceName, cadenceExtID)
	require.NoError(t, err)
	assert.Equal(t, "2027-02-03", reloadedTask.Metadata[MetadataKeySyncedDeadline],
		"synced_deadline must remain unchanged after rollback")
}

// TestSync_LegitimateTodoistEdit_SameTickReconcileIsNotSpuriousCloseCreate
// pins the same-tick ordering edge: after a legitimate Todoist-wins
// clobber commits in processItem, reconcileExistingTask runs in the same
// Sync() invocation with the new contact_by + new synced_deadline visible
// (committed reads). It must NOT emit a spurious close+create.
func TestSync_LegitimateTodoistEdit_SameTickReconcileIsNotSpuriousCloseCreate(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	cadenceStr := "weekly"
	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "SameTick " + uuid.New().String()[:8],
		Cadence:  &cadenceStr,
	})
	require.NoError(t, err)

	originalContactBy := time.Date(2027, 2, 3, 0, 0, 0, 0, time.UTC)
	require.NoError(t, env.contactRepo.UpdateContactBy(env.ctx, contact.ID, originalContactBy))

	cadenceExtID := "td-sametick-" + uuid.New().String()[:8]
	_, err = env.contactTaskRepo.CreateContactTask(env.ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       SourceName,
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleCadenceDue,
		ExternalTaskID: cadenceExtID,
		State:          string(repository.ContactTaskStateManaged),
		Metadata: map[string]any{
			MetadataKeySyncedDeadline: "2027-02-03",
		},
	})
	require.NoError(t, err)

	editedDeadline := "2026-02-24"
	syncItem := SyncItem{
		ID:        cadenceExtID,
		ProjectID: env.settings.ProjectID,
		Labels:    []string{env.settings.LabelName},
		Deadline:  &SyncDate{Date: editedDeadline},
		UpdatedAt: "2026-04-15T10:00:00Z",
	}
	mock := &scriptedClient{
		responses: []*SyncResponse{
			{SyncToken: "tok1", Items: []SyncItem{syncItem}},
			{SyncToken: "tok2"},
			{SyncToken: "tok3"},
		},
	}

	provider := NewCadenceSyncProvider(
		stubOAuthProvider{},
		env.faultyTaskWriter,
		env.contactRepo,
		nil,
		config.TestConfig(),
		env.bus,
		env.cadenceFake,
		env.pool,
		func(_ string) Client { return mock },
		env.riverClient,
		true,
	)

	state := makeStaleDeadlineSyncState(env)
	_, err = provider.Sync(env.ctx, state, []repository.Contact{*contact})
	require.NoError(t, err)

	// processItem's clobber committed: contact_by + synced_deadline both
	// equal the new Todoist value.
	reloaded, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, reloaded.ContactBy)
	expectedContactBy := time.Date(2026, 2, 24, 0, 0, 0, 0, time.UTC)
	assert.True(t, reloaded.ContactBy.Equal(expectedContactBy),
		"contact_by must equal the Todoist edit; want=%v got=%v", expectedContactBy, *reloaded.ContactBy)

	reloadedTask, err := env.contactTaskRepo.GetContactTaskByExternalID(env.ctx, SourceName, cadenceExtID)
	require.NoError(t, err)
	assert.Equal(t, editedDeadline, reloadedTask.Metadata[MetadataKeySyncedDeadline],
		"synced_deadline must be updated to the new Todoist deadline")

	// Same-tick reconcile must NOT emit a NewItemCloseCommand for
	// cadenceExtID — the drift branch emits close+create together, so
	// close-absence is sufficient proof the drift branch did not fire.
	closeForExtID := 0
	for _, batch := range mock.batches {
		for _, c := range batch {
			if c.Type == "item_close" {
				if id, _ := c.Args["id"].(string); id == cadenceExtID {
					closeForExtID++
				}
			}
		}
	}
	assert.Equal(t, 0, closeForExtID,
		"same-tick reconcile must not emit a spurious item_close for the seeded task")
}
