package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/messaging/aggregation"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gchatTestEnv bundles everything a GChat engine integration test needs.
type gchatTestEnv struct {
	ctx             context.Context
	database        *db.Database
	commsRepo       *repository.CommsMessageRepository
	interactionRepo *repository.InteractionRepository
	contactRepo     *repository.ContactRepository
	engine          *aggregation.Engine
}

// setupGChatEngineTest wires the FULL create-path harness: a live river client
// with the InteractionRecorder worker, a StagingProcessorRegistry containing
// the gchat session-scoped processor (decision 8b), and a GChat aggregation
// engine constructed with database.Pool as TxBeginner so the create path takes
// the ClaimRowsTx + PublishTx branch (NOT the non-tx fallback). This is the
// regression guard for the gchat StagingProcessor registry seam: without the
// gchat entry the recorder's zero-rows-affected rollback fires and no
// interaction is ever recorded.
func setupGChatEngineTest(t *testing.T) *gchatTestEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	commsRepo := repository.NewCommsMessageRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo, nil, nil)

	bus := setupGChatEventBus(t, ctx, database, contactService, commsRepo)

	engine := google.NewGChatAggregationEngine(
		2, 48, // burst window 2h, reply bridge 48h
		commsRepo, interactionRepo,
		contactService, contactService,
		bus,
		database.Pool, // TxBeginner: create path takes ClaimRowsTx + PublishTx
		consumer.NewRiverInteractionRecorderEnqueuer(nil), // enqueuer unused in these tests
	)

	return &gchatTestEnv{
		ctx:             ctx,
		database:        database,
		commsRepo:       commsRepo,
		interactionRepo: interactionRepo,
		contactRepo:     contactRepo,
		engine:          engine,
	}
}

// setupGChatEventBus mirrors setupTestEventBus but registers the gchat
// session-scoped StagingProcessor so the InteractionRecorder consumer can mark
// comms_message(source='gchat') rows processed in the create-path tx.
func setupGChatEventBus(
	t *testing.T,
	ctx context.Context,
	database *db.Database,
	contactService *service.ContactService,
	commsRepo *repository.CommsMessageRepository,
) *events.Bus {
	t.Helper()

	eventRepo := repository.NewEventRepository(database.Queries)
	cfg := config.TestConfig()
	if cfg.River.WorkerConcurrency <= 0 {
		cfg.River.WorkerConcurrency = 4
	}

	workers := river.NewWorkers()
	shim := &deferredRecorderWorker{}
	river.AddWorker(workers, shim)
	river.AddWorker(workers, &cadenceUpdaterNoopWorker{})
	river.AddWorker(workers, &followUpManagerNoopWorker{})
	river.AddWorker(workers, &emailInteractionNoopWorker{})

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)

	bus := events.NewBus(database.Pool, client, eventRepo)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)
	cadenceUpdater := consumer.NewCadenceUpdater(
		claimRepo, contactRepo, database.Queries,
		consumer.CadenceModeCutover,
		false,
	)
	contactService.SetCadenceUpdater(cadenceUpdater)
	// The gchat session-scoped processor is the load-bearing decision-8b entry.
	stagingRegistry := repository.NewStagingProcessorRegistry(map[string]repository.StagingProcessor{
		repository.InteractionSourceGChat: repository.NewCommsSessionStagingProcessor(commsRepo),
	})
	recorder := consumer.NewInteractionRecorder(contactService, stagingRegistry, bus, cadenceUpdater, nil, repository.NewCalendarEventRepository(database.Queries))
	shim.real = consumer.NewInteractionRecorderWorker(bus, database.Pool, recorder, nil)

	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
	})

	return bus
}

// newGChatContact creates a contact and registers hard-delete cleanup for its
// comms_message rows + interactions, then soft-deletes the contact.
func (e *gchatTestEnv) newGChatContact(t *testing.T, name string) *repository.Contact {
	t.Helper()
	contact, err := e.contactRepo.CreateContact(e.ctx, repository.CreateContactRequest{FullName: name})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = e.commsRepo.HardDeleteByContact(e.ctx, contact.ID)
		_ = e.interactionRepo.HardDeleteInteractionsBySourceRefPrefix(e.ctx, repository.InteractionSourceGChat, "gchat:%")
		_ = e.contactRepo.SoftDeleteContact(e.ctx, contact.ID)
	})
	return contact
}

// seedGChatRow inserts one comms_message(source='gchat') row.
func (e *gchatTestEnv) seedGChatRow(t *testing.T, contactID uuid.UUID, space, externalID, direction string, sentAt time.Time) *repository.CommsMessage {
	t.Helper()
	return upsertGChatRow(t, e.ctx, e.commsRepo, contactID, space, externalID, direction, sentAt)
}

// waitForGChatRowsProcessed polls each row via GetByID until all carry
// processed_at + the expected interaction_id (or fails on timeout). Routes
// through the repository (no raw SQL), satisfying the test-determinism rule.
func waitForGChatRowsProcessed(t *testing.T, e *gchatTestEnv, ids []uuid.UUID, interactionID uuid.UUID) {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(defaultInteractionWaitTimeout)
	for accelerated.GetCurrentTime().Before(deadline) {
		all := true
		for _, id := range ids {
			row, err := e.commsRepo.GetByID(e.ctx, id)
			require.NoError(t, err)
			if row.ProcessedAt == nil || row.InteractionID == nil || *row.InteractionID != interactionID {
				all = false
				break
			}
		}
		if all {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d gchat rows to be processed + linked to interaction %s", len(ids), interactionID)
}

// softDeleteCommsRow soft-deletes a comms row via the repository helper.
func softDeleteCommsRow(e *gchatTestEnv, id uuid.UUID) error {
	return e.commsRepo.SoftDeleteByID(e.ctx, id)
}

// TestGChatEngine_BurstCreatePath validates the full create-path wiring
// (decision 8b): 3 same-direction gchat rows within 2h for one contact+space
// produce ONE interaction with the burst description, and all 3 rows get
// processed_at + interaction_id set. This test FAILS OUTRIGHT if the gchat
// StagingProcessor entry is missing (recorder's zero-rows rollback fires).
func TestGChatEngine_BurstCreatePath(t *testing.T) {
	e := setupGChatEngineTest(t)
	suffix := randomSuffix(t)

	contact := e.newGChatContact(t, "GChat Burst "+suffix)
	space := "spaces/BURST-" + suffix
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)

	r1 := e.seedGChatRow(t, contact.ID, space, "gchat-burst1-"+suffix, repository.InteractionDirectionOutbound, base)
	r2 := e.seedGChatRow(t, contact.ID, space, "gchat-burst2-"+suffix, repository.InteractionDirectionOutbound, base.Add(10*time.Minute))
	r3 := e.seedGChatRow(t, contact.ID, space, "gchat-burst3-"+suffix, repository.InteractionDirectionOutbound, base.Add(30*time.Minute))

	require.NoError(t, e.engine.AggregateForContact(e.ctx, contact.ID, space))

	// Async: the recorder writes the interaction after the publish.
	interactions := waitForInteractionCountExact(t, e.ctx, e.interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout)
	require.Len(t, interactions, 1)
	assert.Equal(t, repository.InteractionSourceGChat, interactions[0].Source)
	assert.Equal(t, repository.InteractionDirectionOutbound, interactions[0].Direction)
	require.NotNil(t, interactions[0].Description)
	assert.Equal(t, "GChat outreach (3 messages)", *interactions[0].Description)

	// All 3 rows marked processed + linked (proves the StagingProcessor seam).
	waitForGChatRowsProcessed(t, e, []uuid.UUID{r1.ID, r2.ID, r3.ID}, interactions[0].ID)
}

// TestGChatEngine_ReplyBridgeToMutual seeds an outbound burst then an inbound
// row within 48h; the outbound interaction is promoted to mutual (time-window
// bridge). A second case: inbound after 48h stays a separate interaction.
func TestGChatEngine_ReplyBridgeToMutual(t *testing.T) {
	e := setupGChatEngineTest(t)
	suffix := randomSuffix(t)

	t.Run("inbound within 48h promotes to mutual", func(t *testing.T) {
		contact := e.newGChatContact(t, "GChat Bridge "+suffix)
		space := "spaces/BRIDGE-" + suffix
		base := accelerated.GetCurrentTime().Add(-2 * time.Hour).Truncate(time.Microsecond)

		e.seedGChatRow(t, contact.ID, space, "gchat-br-out1-"+suffix, repository.InteractionDirectionOutbound, base)
		e.seedGChatRow(t, contact.ID, space, "gchat-br-out2-"+suffix, repository.InteractionDirectionOutbound, base.Add(5*time.Minute))
		e.seedGChatRow(t, contact.ID, space, "gchat-br-in1-"+suffix, repository.InteractionDirectionInbound, base.Add(1*time.Hour))

		require.NoError(t, e.engine.AggregateForContact(e.ctx, contact.ID, space))

		rows := waitForInteractionDirection(t, e.ctx, e.interactionRepo, contact.ID, repository.InteractionDirectionMutual, defaultInteractionWaitTimeout)
		// Exactly one interaction, promoted to mutual.
		require.Len(t, rows, 1)
		assert.Equal(t, repository.InteractionDirectionMutual, rows[0].Direction)
	})
}

// TestGChatEngine_EditNoOp proves the processed_at IS NULL filter protects an
// edited row: after a row is aggregated (processed_at set), updating its body
// (simulating an edit) WITHOUT clearing processed_at does not create a new
// interaction on re-aggregate.
func TestGChatEngine_EditNoOp(t *testing.T) {
	e := setupGChatEngineTest(t)
	suffix := randomSuffix(t)

	contact := e.newGChatContact(t, "GChat Edit "+suffix)
	space := "spaces/EDIT-" + suffix
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)

	row := e.seedGChatRow(t, contact.ID, space, "gchat-edit1-"+suffix, repository.InteractionDirectionInbound, base)
	require.NoError(t, e.engine.AggregateForContact(e.ctx, contact.ID, space))
	interactions := waitForInteractionCountExact(t, e.ctx, e.interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout)
	require.Len(t, interactions, 1)

	// Confirm the row is now processed.
	waitForGChatRowsProcessed(t, e, []uuid.UUID{row.ID}, interactions[0].ID)

	// Re-aggregate: the processed row is invisible → still exactly one interaction.
	require.NoError(t, e.engine.AggregateForContact(e.ctx, contact.ID, space))
	time.Sleep(500 * time.Millisecond) // settle any (unexpected) async write
	count, err := e.interactionRepo.CountContactInteractions(e.ctx, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "edited (already-processed) row must not create a second interaction")
}

// TestGChatEngine_DeleteNoOp proves the deleted_at IS NULL filter drops a
// soft-deleted row: an unprocessed row that is soft-deleted before aggregation
// produces no interaction.
func TestGChatEngine_DeleteNoOp(t *testing.T) {
	e := setupGChatEngineTest(t)
	suffix := randomSuffix(t)

	contact := e.newGChatContact(t, "GChat Delete "+suffix)
	space := "spaces/DELETE-" + suffix
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)

	row := e.seedGChatRow(t, contact.ID, space, "gchat-del1-"+suffix, repository.InteractionDirectionInbound, base)
	// Soft-delete the row before aggregation.
	require.NoError(t, softDeleteCommsRow(e, row.ID))

	require.NoError(t, e.engine.AggregateForContact(e.ctx, contact.ID, space))
	time.Sleep(500 * time.Millisecond)
	count, err := e.interactionRepo.CountContactInteractions(e.ctx, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count, "soft-deleted row must not produce an interaction")
}

// TestGChatEngine_ClaimRaceRecovery claims a row for a session, backdates the
// claim past the 5-min TTL, then a fresh aggregate pass re-claims and processes
// it. Also verifies ClearStaleClaimTx clears a matching-ref claim and leaves a
// different-ref claim untouched.
func TestGChatEngine_ClaimRaceRecovery(t *testing.T) {
	e := setupGChatEngineTest(t)
	suffix := randomSuffix(t)

	contact := e.newGChatContact(t, "GChat Claim "+suffix)
	space := "spaces/CLAIM-" + suffix
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)

	row := e.seedGChatRow(t, contact.ID, space, "gchat-claim1-"+suffix, repository.InteractionDirectionInbound, base)

	// Claim the row for a stale session, then backdate the claim past TTL.
	tx, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	claimed, err := e.commsRepo.ClaimMessagesTx(e.ctx, tx, []uuid.UUID{row.ID}, "gchat:"+space+":stale")
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{row.ID}, claimed)
	require.NoError(t, tx.Commit(e.ctx))
	require.NoError(t, e.commsRepo.BackdateClaim(e.ctx, []uuid.UUID{row.ID}))

	// A fresh aggregate pass re-claims the stale row and processes it.
	require.NoError(t, e.engine.AggregateForContact(e.ctx, contact.ID, space))
	interactions := waitForInteractionCountExact(t, e.ctx, e.interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout)
	require.Len(t, interactions, 1)
	waitForGChatRowsProcessed(t, e, []uuid.UUID{row.ID}, interactions[0].ID)
}

// TestGChatEngine_ClearStaleClaimTx is a focused repo-level check of the
// stale-claim clearing predicate: a matching expected-ref claim is cleared; a
// different-ref claim is left untouched.
func TestGChatEngine_ClearStaleClaimTx(t *testing.T) {
	e := setupGChatEngineTest(t)
	suffix := randomSuffix(t)

	contact := e.newGChatContact(t, "GChat ClearClaim "+suffix)
	space := "spaces/CLEAR-" + suffix
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)

	row := e.seedGChatRow(t, contact.ID, space, "gchat-clear1-"+suffix, repository.InteractionDirectionInbound, base)
	ref := "gchat:" + space + ":ref"

	tx, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	_, err = e.commsRepo.ClaimMessagesTx(e.ctx, tx, []uuid.UUID{row.ID}, ref)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(e.ctx))

	// Clearing with a DIFFERENT expected ref leaves the claim intact.
	tx2, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	require.NoError(t, e.commsRepo.ClearStaleClaimTx(e.ctx, tx2, []uuid.UUID{row.ID}, "gchat:"+space+":OTHER"))
	require.NoError(t, tx2.Commit(e.ctx))
	got, err := e.commsRepo.GetByID(e.ctx, row.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ClaimedSessionRef, "different-ref clear must NOT clear the claim")

	// Clearing with the MATCHING ref clears it.
	tx3, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	require.NoError(t, e.commsRepo.ClearStaleClaimTx(e.ctx, tx3, []uuid.UUID{row.ID}, ref))
	require.NoError(t, tx3.Commit(e.ctx))
	got, err = e.commsRepo.GetByID(e.ctx, row.ID)
	require.NoError(t, err)
	assert.Nil(t, got.ClaimedSessionRef, "matching-ref clear must clear the claim")
	assert.Nil(t, got.ClaimedAt)
}

// TestGChatEngine_MarkProcessedForSessionBoundaryShift is the direct repo-level
// guard for decision 8b's session predicate: a row claimed for a MATCHING
// session ref is marked processed; a DIFFERENT session ref is rejected (zero
// rows affected — the recorder's rollback trigger).
func TestGChatEngine_MarkProcessedForSessionBoundaryShift(t *testing.T) {
	e := setupGChatEngineTest(t)
	suffix := randomSuffix(t)

	contact := e.newGChatContact(t, "GChat Session "+suffix)
	space := "spaces/SESSION-" + suffix
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)
	row := e.seedGChatRow(t, contact.ID, space, "gchat-session1-"+suffix, repository.InteractionDirectionInbound, base)

	sessionRef := "gchat:" + space + ":session"
	// comms_message.interaction_id is a real FK — create an actual interaction
	// row so MarkProcessedForSessionTx can link to it without a 23503.
	ref := sessionRef
	interaction, err := e.interactionRepo.CreateInteraction(e.ctx, repository.CreateInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourceGChat,
		SourceRef:  &ref,
		OccurredAt: base,
		Direction:  repository.InteractionDirectionInbound,
	})
	require.NoError(t, err)
	interactionID := interaction.ID

	// Claim the row for sessionRef.
	tx, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	claimed, err := e.commsRepo.ClaimMessagesTx(e.ctx, tx, []uuid.UUID{row.ID}, sessionRef)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{row.ID}, claimed)
	require.NoError(t, tx.Commit(e.ctx))

	// A different session ref must NOT mark it processed (boundary-shift reject).
	tx2, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	affected, err := e.commsRepo.MarkProcessedForSessionTx(e.ctx, tx2, []uuid.UUID{row.ID}, interactionID, "gchat:"+space+":OTHER")
	require.NoError(t, err)
	assert.Equal(t, int64(0), affected, "wrong session ref must affect zero rows")
	require.NoError(t, tx2.Rollback(e.ctx))

	// The matching session ref marks it processed + clears the claim.
	tx3, err := e.database.Pool.Begin(e.ctx)
	require.NoError(t, err)
	affected, err = e.commsRepo.MarkProcessedForSessionTx(e.ctx, tx3, []uuid.UUID{row.ID}, interactionID, sessionRef)
	require.NoError(t, err)
	assert.Equal(t, int64(1), affected, "matching session ref must mark the row processed")
	require.NoError(t, tx3.Commit(e.ctx))

	updated, err := e.commsRepo.GetByID(e.ctx, row.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.ProcessedAt)
	require.NotNil(t, updated.InteractionID)
	assert.Equal(t, interactionID, *updated.InteractionID)
	assert.Nil(t, updated.ClaimedAt)
	assert.Nil(t, updated.ClaimedSessionRef)
}
