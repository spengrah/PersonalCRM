// Integration coverage for the FollowUpManager consumer in shadow mode.
// These tests exercise HandleEvent against a real DB and assert the
// observation rows land with the expected action + skip_reason.
//
// Each test seeds its own contact with a deterministic fullName prefix
// so shared-DB row pollution from prior runs cannot influence outcomes,
// and asserts the shadow row invariants scoped to that contact's event.
package tests

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// followUpShadowTestEnv bundles the deps needed for FollowUpManager
// integration tests. The consumer is built in shadow mode; tests opt
// into other modes by constructing their own.
type followUpShadowTestEnv struct {
	database    *db.Database
	contactRepo *repository.ContactRepository
	taskRepo    *repository.ContactTaskRepository
	interRepo   *repository.InteractionRepository
	shadowRepo  *repository.FollowUpShadowObservationRepository
	eventRepo   *repository.EventRepository
	manager     *consumer.FollowUpManager
	watchdog    config.WatchdogConfig
}

func newFollowUpShadowEnv(t *testing.T) (*followUpShadowTestEnv, func()) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = os.Getenv("DATABASE_URL")
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	taskRepo := repository.NewContactTaskRepository(database.Queries)
	interRepo := repository.NewInteractionRepository(database.Queries)
	shadowRepo := repository.NewFollowUpShadowObservationRepository(database.Queries, database.Pool)
	eventRepo := repository.NewEventRepository(database.Queries)

	watchdog := cfg.Watchdog
	manager := consumer.NewFollowUpManager(
		consumer.FollowUpModeShadow,
		taskRepo, interRepo, shadowRepo, watchdog,
	)

	return &followUpShadowTestEnv{
		database:    database,
		contactRepo: contactRepo,
		taskRepo:    taskRepo,
		interRepo:   interRepo,
		shadowRepo:  shadowRepo,
		eventRepo:   eventRepo,
		manager:     manager,
		watchdog:    watchdog,
	}, func() { database.Close() }
}

// seedContact creates a contact with an optional cadence string.
// Returns the ID; the test cleans up via HardDelete.
func (e *followUpShadowTestEnv) seedContact(t *testing.T, cadenceStr string) uuid.UUID {
	t.Helper()
	name := "FollowUpShadow-" + uuid.NewString()[:8]
	req := repository.CreateContactRequest{FullName: name}
	if cadenceStr != "" {
		c := cadenceStr
		req.Cadence = &c
	}
	contact, err := e.contactRepo.CreateContact(context.Background(), req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.contactRepo.HardDeleteContact(context.Background(), contact.ID) })
	return contact.ID
}

// insertRecordedEvent builds an interaction.recorded envelope with a V2
// payload for (contactID, direction, source, occurredAt, cadenceStr)
// and inserts it into the event table via EventRepository.InsertEvent.
// Returns the populated envelope (with ID). Unique source_id per call
// so parallel subtests never collide.
func (e *followUpShadowTestEnv) insertRecordedEvent(
	t *testing.T, ctx context.Context, contactID uuid.UUID,
	direction, source string, occurredAt time.Time, cadenceStr string,
) *events.Envelope {
	t.Helper()
	payload := events.InteractionRecordedPayload{
		Version:       2,
		ContactID:     contactID,
		InteractionID: uuid.New(),
		Direction:     direction,
		OccurredAt:    occurredAt,
		Source:        source,
	}
	if cadenceStr != "" {
		payload.PrevCadenceValue = &cadenceStr
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	env := &events.Envelope{
		Source:     source,
		SourceID:   "followup-shadow-test-" + uuid.NewString(),
		Kind:       events.KindInteractionRecorded,
		Payload:    raw,
		ObservedAt: occurredAt,
	}

	tx, err := e.database.Pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, e.eventRepo.InsertEvent(ctx, tx, env))
	require.NoError(t, tx.Commit(ctx))
	return env
}

// seedPendingFollowUp inserts a managed follow-up contact_task for the
// given contact so guard 3 / complete paths have something to find.
// Returns the task ID.
func (e *followUpShadowTestEnv) seedPendingFollowUp(t *testing.T, contactID uuid.UUID) uuid.UUID {
	t.Helper()
	task, err := e.taskRepo.CreateContactTask(context.Background(), repository.CreateContactTaskRequest{
		ContactID:      contactID,
		Provider:       "todoist",
		Kind:           "follow_up",
		ExternalTaskID: "test-followup-" + uuid.NewString(),
		State:          string(repository.ContactTaskStateManaged),
	})
	require.NoError(t, err)
	return task.ID
}

// seedInteraction inserts an interaction row for use by the
// has-response-after guard. Commits before returning so the subsequent
// HandleEvent call can see the row.
func (e *followUpShadowTestEnv) seedInteraction(t *testing.T, contactID uuid.UUID, direction, source string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, pgx.BeginTxFunc(ctx, e.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := e.interRepo.CreateInteractionTx(ctx, tx, repository.CreateInteractionRequest{
			ContactID:  contactID,
			Source:     source,
			OccurredAt: at,
			Direction:  direction,
		})
		return err
	}))
}

// runHandleEventInTx runs manager.HandleEvent inside a fresh tx that is
// committed on success so subsequent reads see the written row.
func runHandleEventInTx(t *testing.T, database *db.Database, manager *consumer.FollowUpManager, env *events.Envelope) error {
	t.Helper()
	ctx := context.Background()
	return pgx.BeginTxFunc(ctx, database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return manager.HandleEvent(ctx, tx, env)
	})
}

// -----------------------------------------------------------------------------
// Outbound paths.
// -----------------------------------------------------------------------------

func TestIntegration_FollowUpManager_Outbound_Fresh_RecordsCreate(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newFollowUpShadowEnv(t)
	defer cleanup()
	ctx := context.Background()

	contactID := env.seedContact(t, "weekly")
	recorded := env.insertRecordedEvent(t, ctx, contactID,
		repository.InteractionDirectionOutbound,
		repository.InteractionSourceTelegram,
		accelerated.GetCurrentTime().Add(-time.Hour),
		"weekly",
	)
	require.NoError(t, runHandleEventInTx(t, env.database, env.manager, recorded))

	obs, err := env.shadowRepo.FindMatchingConsumer(ctx, nil, recorded.ID)
	require.NoError(t, err)
	require.NotNil(t, obs)
	assert.Equal(t, repository.FollowUpActionCreate, obs.Action)
	assert.Empty(t, obs.SkipReason)
	assert.NotNil(t, obs.WouldIdempotencyKey)
	assert.NotNil(t, obs.WouldDeadline)
	assert.False(t, obs.ConsumerCalledTodoist, "shadow must never mark consumer_called_todoist true")
}

func TestIntegration_FollowUpManager_Outbound_NoCadence_SkipNullReason(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newFollowUpShadowEnv(t)
	defer cleanup()
	ctx := context.Background()

	contactID := env.seedContact(t, "")
	recorded := env.insertRecordedEvent(t, ctx, contactID,
		repository.InteractionDirectionOutbound,
		repository.InteractionSourceTelegram,
		accelerated.GetCurrentTime().Add(-time.Hour),
		"",
	)
	require.NoError(t, runHandleEventInTx(t, env.database, env.manager, recorded))

	obs, err := env.shadowRepo.FindMatchingConsumer(ctx, nil, recorded.ID)
	require.NoError(t, err)
	require.NotNil(t, obs)
	assert.Equal(t, repository.FollowUpActionSkip, obs.Action)
	assert.Empty(t, obs.SkipReason, "no-cadence skip is not a guard-class skip")
}

func TestIntegration_FollowUpManager_Outbound_Backdated_NonManual_SkipsBackdated(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newFollowUpShadowEnv(t)
	defer cleanup()
	ctx := context.Background()

	contactID := env.seedContact(t, "weekly")
	// 90 days old, weekly cadence → backdated under the 3-day watchdog.
	old := accelerated.GetCurrentTime().Add(-90 * 24 * time.Hour)
	recorded := env.insertRecordedEvent(t, ctx, contactID,
		repository.InteractionDirectionOutbound,
		repository.InteractionSourceTelegram,
		old, "weekly",
	)
	require.NoError(t, runHandleEventInTx(t, env.database, env.manager, recorded))

	obs, err := env.shadowRepo.FindMatchingConsumer(ctx, nil, recorded.ID)
	require.NoError(t, err)
	require.NotNil(t, obs)
	assert.Equal(t, repository.FollowUpActionSkip, obs.Action)
	assert.Equal(t, repository.FollowUpSkipReasonBackdated, obs.SkipReason)
}

func TestIntegration_FollowUpManager_Outbound_Backdated_Manual_BypassesGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newFollowUpShadowEnv(t)
	defer cleanup()
	ctx := context.Background()

	contactID := env.seedContact(t, "weekly")
	old := accelerated.GetCurrentTime().Add(-90 * 24 * time.Hour)
	recorded := env.insertRecordedEvent(t, ctx, contactID,
		repository.InteractionDirectionOutbound,
		repository.InteractionSourceManual,
		old, "weekly",
	)
	require.NoError(t, runHandleEventInTx(t, env.database, env.manager, recorded))

	obs, err := env.shadowRepo.FindMatchingConsumer(ctx, nil, recorded.ID)
	require.NoError(t, err)
	require.NotNil(t, obs)
	assert.Equal(t, repository.FollowUpActionCreate, obs.Action,
		"manual source bypasses the backdated guard")
}

func TestIntegration_FollowUpManager_Outbound_OutOfOrder_Skip(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newFollowUpShadowEnv(t)
	defer cleanup()
	ctx := context.Background()

	contactID := env.seedContact(t, "weekly")
	// Seed a response AFTER the outbound occurred_at so guard 2 fires.
	responseAt := accelerated.GetCurrentTime()
	outboundAt := responseAt.Add(-2 * time.Hour)
	env.seedInteraction(t, contactID, repository.InteractionDirectionInbound, repository.InteractionSourceTelegram, responseAt)

	recorded := env.insertRecordedEvent(t, ctx, contactID,
		repository.InteractionDirectionOutbound,
		repository.InteractionSourceTelegram,
		outboundAt, "weekly",
	)
	require.NoError(t, runHandleEventInTx(t, env.database, env.manager, recorded))

	obs, err := env.shadowRepo.FindMatchingConsumer(ctx, nil, recorded.ID)
	require.NoError(t, err)
	require.NotNil(t, obs)
	assert.Equal(t, repository.FollowUpActionSkip, obs.Action)
	assert.Equal(t, repository.FollowUpSkipReasonOutOfOrder, obs.SkipReason)
}

func TestIntegration_FollowUpManager_Outbound_PendingExists_Refresh(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newFollowUpShadowEnv(t)
	defer cleanup()
	ctx := context.Background()

	contactID := env.seedContact(t, "weekly")
	_ = env.seedPendingFollowUp(t, contactID)

	recorded := env.insertRecordedEvent(t, ctx, contactID,
		repository.InteractionDirectionOutbound,
		repository.InteractionSourceTelegram,
		accelerated.GetCurrentTime().Add(-time.Hour), "weekly",
	)
	require.NoError(t, runHandleEventInTx(t, env.database, env.manager, recorded))

	obs, err := env.shadowRepo.FindMatchingConsumer(ctx, nil, recorded.ID)
	require.NoError(t, err)
	require.NotNil(t, obs)
	assert.Equal(t, repository.FollowUpActionRefresh, obs.Action,
		"outbound with pending refreshes (mirrors direct path), not skip")
	assert.Empty(t, obs.SkipReason)
	assert.NotNil(t, obs.WouldDeadline)
}

// -----------------------------------------------------------------------------
// Inbound / mutual paths.
// -----------------------------------------------------------------------------

func TestIntegration_FollowUpManager_Inbound_HasPending_RecordsComplete(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newFollowUpShadowEnv(t)
	defer cleanup()
	ctx := context.Background()

	contactID := env.seedContact(t, "weekly")
	_ = env.seedPendingFollowUp(t, contactID)

	recorded := env.insertRecordedEvent(t, ctx, contactID,
		repository.InteractionDirectionInbound,
		repository.InteractionSourceTelegram,
		accelerated.GetCurrentTime(), "weekly",
	)
	require.NoError(t, runHandleEventInTx(t, env.database, env.manager, recorded))

	obs, err := env.shadowRepo.FindMatchingConsumer(ctx, nil, recorded.ID)
	require.NoError(t, err)
	require.NotNil(t, obs)
	assert.Equal(t, repository.FollowUpActionComplete, obs.Action)
}

func TestIntegration_FollowUpManager_Mutual_NoPending_RecordsSkipNoReason(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newFollowUpShadowEnv(t)
	defer cleanup()
	ctx := context.Background()

	contactID := env.seedContact(t, "weekly")

	recorded := env.insertRecordedEvent(t, ctx, contactID,
		repository.InteractionDirectionMutual,
		repository.InteractionSourceGCal,
		accelerated.GetCurrentTime(), "weekly",
	)
	require.NoError(t, runHandleEventInTx(t, env.database, env.manager, recorded))

	obs, err := env.shadowRepo.FindMatchingConsumer(ctx, nil, recorded.ID)
	require.NoError(t, err)
	require.NotNil(t, obs)
	assert.Equal(t, repository.FollowUpActionSkip, obs.Action)
	assert.Empty(t, obs.SkipReason)
}

// -----------------------------------------------------------------------------
// Mode / idempotency invariants.
// -----------------------------------------------------------------------------

func TestIntegration_FollowUpManager_ModeOff_NoRows(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newFollowUpShadowEnv(t)
	defer cleanup()
	ctx := context.Background()

	contactID := env.seedContact(t, "weekly")

	offManager := consumer.NewFollowUpManager(
		consumer.FollowUpModeOff,
		env.taskRepo, env.interRepo, env.shadowRepo, env.watchdog,
	)
	recorded := env.insertRecordedEvent(t, ctx, contactID,
		repository.InteractionDirectionOutbound,
		repository.InteractionSourceTelegram,
		accelerated.GetCurrentTime().Add(-time.Hour), "weekly",
	)
	require.NoError(t, runHandleEventInTx(t, env.database, offManager, recorded))

	count, err := env.shadowRepo.CountByContact(ctx, contactID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count, "mode=off must not write shadow rows")
}

// TestIntegration_FollowUpManager_WorkerRetry_OneConsumerRow asserts the
// shadow-table dedupe: invoking HandleEvent twice for the same event_id
// (simulating a river retry) leaves exactly one consumer row thanks to
// the UNIQUE (event_id, writer) constraint + ON CONFLICT DO NOTHING.
func TestIntegration_FollowUpManager_WorkerRetry_OneConsumerRow(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newFollowUpShadowEnv(t)
	defer cleanup()
	ctx := context.Background()

	contactID := env.seedContact(t, "weekly")
	recorded := env.insertRecordedEvent(t, ctx, contactID,
		repository.InteractionDirectionOutbound,
		repository.InteractionSourceTelegram,
		accelerated.GetCurrentTime().Add(-time.Hour), "weekly",
	)

	// First invocation — writes the consumer row.
	require.NoError(t, runHandleEventInTx(t, env.database, env.manager, recorded))
	// Second invocation — ON CONFLICT DO NOTHING on (event_id, writer).
	require.NoError(t, runHandleEventInTx(t, env.database, env.manager, recorded))

	count, err := env.shadowRepo.CountByContact(ctx, contactID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "retrying the same event must not duplicate observation rows")
}

func TestIntegration_FollowUpManager_V1Payload_RejectedNoRows(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newFollowUpShadowEnv(t)
	defer cleanup()
	ctx := context.Background()

	contactID := env.seedContact(t, "weekly")

	// Build a V1 payload (no PrevCadenceSnapshot / PrevCadenceValue).
	payload := events.InteractionRecordedPayload{
		Version:       1,
		ContactID:     contactID,
		InteractionID: uuid.New(),
		Direction:     repository.InteractionDirectionOutbound,
		OccurredAt:    accelerated.GetCurrentTime().Add(-time.Hour),
		Source:        repository.InteractionSourceTelegram,
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	eventEnv := &events.Envelope{
		Source:     repository.InteractionSourceTelegram,
		SourceID:   "followup-shadow-v1-" + uuid.NewString(),
		Kind:       events.KindInteractionRecorded,
		Payload:    raw,
		ObservedAt: payload.OccurredAt,
	}
	tx, err := env.database.Pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, env.eventRepo.InsertEvent(ctx, tx, eventEnv))
	require.NoError(t, tx.Commit(ctx))

	// Consumer must log + return nil without writing.
	require.NoError(t, runHandleEventInTx(t, env.database, env.manager, eventEnv))

	count, err := env.shadowRepo.CountByContact(ctx, contactID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count, "V1 payloads must be rejected without writing shadow rows")
}
