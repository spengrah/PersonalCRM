package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// -----------------------------------------------------------------------------
// Integration test helpers for the PR 6 cutover-mode InteractionRecorder.
// The consumer is now the sole writer; ContactService.RecordInteractionTx
// handles dedup + insert + cadence updates inside the caller's tx.
// -----------------------------------------------------------------------------

type consumerTestEnv struct {
	ctx             context.Context
	database        *db.Database
	cfg             *config.Config
	bus             *events.Bus
	contactRepo     *repository.ContactRepository
	interactionRepo *repository.InteractionRepository
	recorder        *consumer.InteractionRecorder
	contactService  *service.ContactService
	manualHandler   *service.ManualInteractionHandler
}

// newConsumerTestBus builds a river client with both the noop worker and
// the real InteractionRecorderWorker registered. TestOnly=true means the
// dispatcher never picks jobs up; tests manually invoke HandleEvent via
// env.runHandleEvent.
func newConsumerTestBus(
	t *testing.T,
	ctx context.Context,
	database *db.Database,
	cfg *config.Config,
	recorderRef **consumer.InteractionRecorder,
) *events.Bus {
	t.Helper()
	eventRepo := repository.NewEventRepository(database.Queries)

	workers := river.NewWorkers()
	river.AddWorker(workers, &eventBusNoopWorker{})

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)

	bus := events.NewBus(database.Pool, client, eventRepo)

	// recorderRef is a pointer-to-pointer so the worker wrapper can capture
	// a reference that's populated AFTER the bus is constructed. River
	// requires the worker type to be registered before Start.
	river.AddWorker(workers, &lateRecorderWorker{
		bus:         bus,
		pool:        database.Pool,
		recorderRef: recorderRef,
	})
	// consumerJobsForKind enqueues cadence_updater and followup_manager
	// jobs for interaction.recorded events. Register placeholder workers
	// so river accepts those kinds at Start; TestOnly=true means they
	// never run.
	river.AddWorker(workers, &lateCadenceUpdaterWorker{})
	river.AddWorker(workers, &lateFollowUpManagerWorker{})

	startCtx, startCancel := context.WithTimeout(ctx, 10*time.Second)
	defer startCancel()
	require.NoError(t, client.Start(startCtx))

	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
	})

	return bus
}

// lateRecorderWorker is a thin shim that lets us register a worker before
// the concrete recorder is available. Since TestOnly=true, this worker
// never actually runs — it exists only so river's AddWorker validates the
// job kind at Start time.
type lateRecorderWorker struct {
	river.WorkerDefaults[interactionRecorderJobArgsPlaceholder]
	bus         *events.Bus
	pool        any
	recorderRef **consumer.InteractionRecorder
}

// interactionRecorderJobArgsPlaceholder mirrors the real job args kind so
// river can route to lateRecorderWorker.
type interactionRecorderJobArgsPlaceholder struct{}

func (interactionRecorderJobArgsPlaceholder) Kind() string { return "interaction_recorder" }

func (w *lateRecorderWorker) Work(_ context.Context, _ *river.Job[interactionRecorderJobArgsPlaceholder]) error {
	// TestOnly=true means this never runs. Kept as a no-op.
	return nil
}

// lateCadenceUpdaterWorker is the cadence_updater placeholder. TestOnly
// means it never runs; registering it just lets river accept the kind
// when the test bus enqueues jobs via PublishTx.
type lateCadenceUpdaterWorker struct {
	river.WorkerDefaults[cadenceUpdaterJobArgsPlaceholder]
}

type cadenceUpdaterJobArgsPlaceholder struct{}

func (cadenceUpdaterJobArgsPlaceholder) Kind() string { return "cadence_updater" }

func (*lateCadenceUpdaterWorker) Work(_ context.Context, _ *river.Job[cadenceUpdaterJobArgsPlaceholder]) error {
	return nil
}

// lateFollowUpManagerWorker is the followup_manager placeholder.
// TestOnly means it never runs; registering it just lets river accept
// the kind when the test bus enqueues jobs via PublishTx.
type lateFollowUpManagerWorker struct {
	river.WorkerDefaults[followUpManagerJobArgsPlaceholder]
}

type followUpManagerJobArgsPlaceholder struct{}

func (followUpManagerJobArgsPlaceholder) Kind() string { return "followup_manager" }

func (*lateFollowUpManagerWorker) Work(_ context.Context, _ *river.Job[followUpManagerJobArgsPlaceholder]) error {
	return nil
}

func newConsumerTestEnv(t *testing.T, ctx context.Context) *consumerTestEnv {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	database, cfg := newEventBusTestDB(t, ctx)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	telegramMessageRepo := repository.NewTelegramMessageRepository(database.Queries)

	var recorder *consumer.InteractionRecorder
	bus := newConsumerTestBus(t, ctx, database, cfg, &recorder)

	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo, nil, nil)
	// Wire a real CadenceUpdater so the recorder's inline-apply seam
	// writes cadence columns against a live DB in these integration
	// tests.
	contactRepo.SetPool(database.Pool)
	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)
	cadenceUpdater := consumer.NewCadenceUpdater(
		claimRepo, contactRepo, database.Queries,
		consumer.CadenceModeCutover,
		false,
	)
	contactService.SetCadenceUpdater(cadenceUpdater)
	recorder = consumer.NewInteractionRecorder(contactService, telegramMessageRepo, bus, cadenceUpdater, nil)

	manualHandler := service.NewManualInteractionHandler(database.Pool, bus, recorder)

	return &consumerTestEnv{
		ctx:             ctx,
		database:        database,
		cfg:             cfg,
		bus:             bus,
		contactRepo:     contactRepo,
		interactionRepo: interactionRepo,
		recorder:        recorder,
		contactService:  contactService,
		manualHandler:   manualHandler,
	}
}

func (e *consumerTestEnv) newContact(t *testing.T, name string) uuid.UUID {
	t.Helper()
	contact, err := e.contactRepo.CreateContact(e.ctx, repository.CreateContactRequest{
		FullName: name + "-" + uuid.NewString()[:8],
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.contactRepo.HardDeleteContact(e.ctx, contact.ID) })
	return contact.ID
}

// runHandleEvent wraps InteractionRecorder.HandleEvent in a fresh tx
// (matching the river worker's behavior). Commits on success, rolls back
// on error. Ignores the returned interaction / postCommit — tests assert
// via direct DB queries.
func (e *consumerTestEnv) runHandleEvent(t *testing.T, env *events.Envelope) error {
	t.Helper()
	var postCommit func(context.Context)
	err := pgx.BeginTxFunc(e.ctx, e.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, pc, handleErr := e.recorder.HandleEvent(e.ctx, tx, env)
		if handleErr != nil {
			return handleErr
		}
		postCommit = pc
		return nil
	})
	if err != nil {
		return err
	}
	if postCommit != nil {
		postCommit(e.ctx)
	}
	return nil
}

// mustPublish publishes the envelope and asserts the ID is populated.
func (e *consumerTestEnv) mustPublish(t *testing.T, env *events.Envelope) {
	t.Helper()
	require.NoError(t, e.bus.Publish(e.ctx, env))
	require.NotEqual(t, uuid.Nil, env.ID)
}

// -----------------------------------------------------------------------------
// Cutover happy-path tests.
// -----------------------------------------------------------------------------

// TestIntegration_CalendarAttended_CutoverWritesInteraction asserts the
// consumer writes the interaction row and emits interaction.recorded in
// the same tx (spec §3.4.1 atomicity contract).
func TestIntegration_CalendarAttended_CutoverWritesInteraction(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	env := newConsumerTestEnv(t, ctx)

	contactID := env.newContact(t, "calendar-attended-cutover")
	eventIDStr := uuid.NewString()
	// Per plan Decision 11, the publisher-built SourceID is per-(event, contact).
	sourceID := eventIDStr + ":" + contactID.String()
	occurredAt := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	payload, err := events.Marshal(events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  contactID,
		EventID:    eventIDStr,
		OccurredAt: occurredAt,
	})
	require.NoError(t, err)

	envelope := &events.Envelope{
		Source:     repository.InteractionSourceGCal,
		SourceID:   sourceID,
		Kind:       events.KindCalendarAttended,
		Payload:    payload,
		ObservedAt: occurredAt,
	}
	env.mustPublish(t, envelope)

	require.NoError(t, env.runHandleEvent(t, envelope))

	got, err := env.interactionRepo.FindBySourceRef(ctx, contactID, repository.InteractionSourceGCal, eventIDStr)
	require.NoError(t, err)
	require.Equal(t, contactID, got.ContactID)
	require.Equal(t, repository.InteractionDirectionMutual, got.Direction)

	eventRepo := repository.NewEventRepository(env.database.Queries)
	recorded, err := eventRepo.FindEventBySource(ctx, repository.InteractionSourceGCal, got.ID.String())
	require.NoError(t, err)
	require.Equal(t, events.KindInteractionRecorded, recorded.Kind)
}

// TestIntegration_CalendarAttended_TitlePreservedInInteraction regression-
// tests the Codex P2 fix: calendar.attended events carry the calendar
// event's title in the payload so the consumer populates
// interaction.description. Pre-cutover this came from the direct-path
// RecordInteraction arg; post-cutover it must flow through the payload.
func TestIntegration_CalendarAttended_TitlePreservedInInteraction(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	env := newConsumerTestEnv(t, ctx)

	contactID := env.newContact(t, "calendar-attended-title")
	eventIDStr := uuid.NewString()
	sourceID := eventIDStr + ":" + contactID.String()
	occurredAt := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	title := "Quarterly sync with Alice"

	payload, err := events.Marshal(events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  contactID,
		EventID:    eventIDStr,
		OccurredAt: occurredAt,
		Title:      &title,
	})
	require.NoError(t, err)

	envelope := &events.Envelope{
		Source: repository.InteractionSourceGCal, SourceID: sourceID,
		Kind: events.KindCalendarAttended, Payload: payload, ObservedAt: occurredAt,
	}
	env.mustPublish(t, envelope)
	require.NoError(t, env.runHandleEvent(t, envelope))

	got, err := env.interactionRepo.FindBySourceRef(ctx, contactID, repository.InteractionSourceGCal, eventIDStr)
	require.NoError(t, err)
	require.NotNil(t, got.Description, "calendar interaction must carry the event title as description")
	require.Equal(t, title, *got.Description)
}

// TestIntegration_CalendarAttended_Replay exercises dedup: second HandleEvent
// finds the existing row and early-returns without emitting a second
// interaction.recorded.
func TestIntegration_CalendarAttended_Replay(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	env := newConsumerTestEnv(t, ctx)

	contactID := env.newContact(t, "calendar-attended-replay")
	eventIDStr := uuid.NewString()
	sourceID := eventIDStr + ":" + contactID.String()
	occurredAt := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	payload, err := events.Marshal(events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version: 1, ContactID: contactID, EventID: eventIDStr, OccurredAt: occurredAt,
	})
	require.NoError(t, err)

	first := &events.Envelope{
		Source: repository.InteractionSourceGCal, SourceID: sourceID,
		Kind: events.KindCalendarAttended, Payload: payload, ObservedAt: occurredAt,
	}
	env.mustPublish(t, first)
	require.NoError(t, env.runHandleEvent(t, first))

	// Second envelope with same (source, source_id) — bus.Publish is idempotent
	// at the event-table level. Re-run HandleEvent manually on a fresh envelope
	// for the same SourceRef to exercise replay at the interaction layer.
	second := &events.Envelope{
		Source: repository.InteractionSourceGCal, SourceID: sourceID,
		Kind: events.KindCalendarAttended, Payload: payload, ObservedAt: occurredAt,
	}
	require.NoError(t, env.bus.Publish(ctx, second))
	require.NoError(t, env.runHandleEvent(t, second))

	// Still only one interaction row.
	rows, err := env.interactionRepo.ListContactInteractions(ctx, contactID, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1, "replay must not create a second interaction row")

	// Only one interaction.recorded event row keyed by interaction.ID.
	var recordedCount int
	err = env.database.Pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM event WHERE source = $1 AND source_id = $2 AND kind = 'interaction.recorded'",
		repository.InteractionSourceGCal, rows[0].ID.String(),
	).Scan(&recordedCount)
	require.NoError(t, err)
	require.Equal(t, 1, recordedCount, "replay must not emit a second interaction.recorded")
}

// TestIntegration_ManualHandler_SingleTxFlow asserts the cutover
// ManualInteractionHandler writes the interaction and emits
// interaction.recorded inside a single tx.
func TestIntegration_ManualHandler_SingleTxFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	env := newConsumerTestEnv(t, ctx)

	contactID := env.newContact(t, "manual-single-tx")
	occurredAt := time.Date(2026, 4, 10, 12, 30, 0, 0, time.UTC)

	interaction, err := env.manualHandler.Run(ctx, contactID, repository.InteractionDirectionMutual, occurredAt, "manual test")
	require.NoError(t, err)
	require.NotNil(t, interaction)
	require.Equal(t, contactID, interaction.ContactID)
	require.Equal(t, repository.InteractionSourceManual, interaction.Source)

	rows, err := env.interactionRepo.ListContactInteractions(ctx, contactID, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	// Exactly one interaction.manual event row for this contact.
	var manualEventCount int
	err = env.database.Pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM event WHERE kind = 'interaction.manual' AND (payload->>'contact_id')::uuid = $1",
		contactID,
	).Scan(&manualEventCount)
	require.NoError(t, err)
	require.Equal(t, 1, manualEventCount)

	// Exactly one interaction.recorded event row keyed by interaction.ID.
	var recordedCount int
	err = env.database.Pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM event WHERE kind = 'interaction.recorded' AND source_id = $1",
		rows[0].ID.String(),
	).Scan(&recordedCount)
	require.NoError(t, err)
	require.Equal(t, 1, recordedCount)
}

// TestIntegration_ManualHandler_DedupWithinWindow asserts two manual
// writes within the 30-min dedup window return the same row (replay).
func TestIntegration_ManualHandler_DedupWithinWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	env := newConsumerTestEnv(t, ctx)

	contactID := env.newContact(t, "manual-dedup")
	occurredAt := time.Date(2026, 4, 10, 12, 30, 0, 0, time.UTC)

	first, err := env.manualHandler.Run(ctx, contactID, repository.InteractionDirectionMutual, occurredAt, "first")
	require.NoError(t, err)

	second, err := env.manualHandler.Run(ctx, contactID, repository.InteractionDirectionMutual, occurredAt.Add(5*time.Minute), "second")
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID, "second call within 30-min window returns the first interaction (dedup)")

	rows, err := env.interactionRepo.ListContactInteractions(ctx, contactID, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

// TestIntegration_MissingContact_ConsumerReturnsNotFound asserts unresolved
// contact_ids surface as db.ErrNotFound (wrapped), per spec §3.4.1.
func TestIntegration_MissingContact_ConsumerReturnsNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	env := newConsumerTestEnv(t, ctx)

	missingID := uuid.New()
	eventIDStr := uuid.NewString()
	sourceID := eventIDStr + ":" + missingID.String()
	occurredAt := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	payload, err := events.Marshal(events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version: 1, ContactID: missingID, EventID: eventIDStr, OccurredAt: occurredAt,
	})
	require.NoError(t, err)

	envelope := &events.Envelope{
		Source: repository.InteractionSourceGCal, SourceID: sourceID,
		Kind: events.KindCalendarAttended, Payload: payload, ObservedAt: occurredAt,
	}
	env.mustPublish(t, envelope)

	err = env.runHandleEvent(t, envelope)
	require.Error(t, err)
	require.ErrorIs(t, err, db.ErrNotFound)
}

// TestIntegration_CutoverMode_NoShadowObservationRowsWritten asserts the
// shadow-observation side-effects are gone in cutover. PR 7 will re-add
// observations for CadenceUpdater shadow, but in PR 6 the table stays
// empty during InteractionRecorder flows.
func TestIntegration_CutoverMode_NoShadowObservationRowsWritten(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	env := newConsumerTestEnv(t, ctx)

	contactID := env.newContact(t, "no-shadow-obs")
	occurredAt := time.Date(2026, 4, 10, 12, 30, 0, 0, time.UTC)
	_, err := env.manualHandler.Run(ctx, contactID, repository.InteractionDirectionMutual, occurredAt, "cutover")
	require.NoError(t, err)

	var obsCount int
	err = env.database.Pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM event_shadow_observation WHERE contact_id = $1",
		contactID,
	).Scan(&obsCount)
	require.NoError(t, err)
	require.Zero(t, obsCount, "cutover mode must not write any shadow observation rows for InteractionRecorder")
}
