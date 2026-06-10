// End-to-end coverage for the cutover decline remove branch driven through
// the REAL CalendarSyncProvider.processEvent (via RunProcessEventForTest)
// with a real *events.Bus + database.Pool. The in-package google tests only
// exercise the off-mode branch (nil bus/pool); these prove the cutover path:
// processEvent gate → removeDeclinedEvent → pool.Begin → publishCalendarDeclinedTx
// (per matched contact) → DeleteByGcalIDTx → Commit, including:
//   - the calendar_event row is deleted;
//   - a declined:-keyed event-log row lands and coexists with the attended row;
//   - the derived gcal interaction is soft-deleted after the decline job
//     drains, and the contact's date columns roll;
//   - publish-before-delete: a failing PublishTx leaves the calendar_event
//     row intact (the delete is rolled back).
package tests

import (
	"context"
	"errors"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	calendarapi "google.golang.org/api/calendar/v3"
)

// declineCutoverEnv bundles a real provider + bus + repos for the cutover
// remove-branch tests. The river client runs live with the
// CalendarDeclineHandlerWorker registered so the declined job drains.
type declineCutoverEnv struct {
	ctx             context.Context
	database        *db.Database
	gen             *factory.Generator
	provider        *google.CalendarSyncProvider
	bus             *events.Bus
	contactRepo     *repository.ContactRepository
	interactionRepo *repository.InteractionRepository
	calendarRepo    *repository.CalendarEventRepository
	eventRepo       *repository.EventRepository
	cadenceUpdater  *consumer.CadenceUpdater
	accountID       string
}

func newDeclineCutoverEnv(t *testing.T, ctx context.Context) *declineCutoverEnv {
	t.Helper()
	// Per-test isolated clone: the live decline worker drains a private
	// river_job.
	database, cfg := newIsolatedRiverTestDB(t, ctx)
	if cfg.River.WorkerConcurrency <= 0 {
		cfg.River.WorkerConcurrency = 4
	}

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	calendarRepo := repository.NewCalendarEventRepository(database.Queries)
	eventRepo := repository.NewEventRepository(database.Queries)
	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)
	cadenceUpdater := consumer.NewCadenceUpdater(claimRepo, contactRepo, database.Queries, consumer.CadenceModeCutover, false)
	declineHandler := consumer.NewCalendarDeclineHandler(interactionRepo, contactRepo)

	// Live river client with the real CalendarDeclineHandlerWorker so the
	// declined job drains after the provider publishes it.
	workers := river.NewWorkers()
	declineShim := &deferredDeclineWorker{}
	river.AddWorker(workers, declineShim)
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)

	bus := events.NewBus(database.Pool, client, eventRepo)
	declineShim.real = consumer.NewCalendarDeclineHandlerWorker(bus, database.Pool, declineHandler)
	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
	})

	provider := google.NewCalendarSyncProvider(nil, calendarRepo, contactRepo, nil, nil, bus, database.Pool)

	gen, _ := migrationGenerator(t)
	accountID := gen.Prefix() + "decline-cutover"
	t.Cleanup(func() { _ = calendarRepo.DeleteEventsByAccount(ctx, accountID) })

	return &declineCutoverEnv{
		ctx:             ctx,
		database:        database,
		gen:             gen,
		provider:        provider,
		bus:             bus,
		contactRepo:     contactRepo,
		interactionRepo: interactionRepo,
		calendarRepo:    calendarRepo,
		eventRepo:       eventRepo,
		cadenceUpdater:  cadenceUpdater,
		accountID:       accountID,
	}
}

// deferredDeclineWorker registers a CalendarDeclineHandler river worker
// before the real worker exists (the bus needs the client, the worker needs
// the bus). Work delegates to the real worker once assigned.
type deferredDeclineWorker struct {
	river.WorkerDefaults[consumerjobs.CalendarDeclineHandlerJobArgs]
	real *consumer.CalendarDeclineHandlerWorker
}

func (w *deferredDeclineWorker) Work(ctx context.Context, j *river.Job[consumerjobs.CalendarDeclineHandlerJobArgs]) error {
	if w.real == nil {
		return errors.New("deferredDeclineWorker invoked before real worker assignment")
	}
	return w.real.Work(ctx, j)
}

func (w *deferredDeclineWorker) Timeout(j *river.Job[consumerjobs.CalendarDeclineHandlerJobArgs]) time.Duration {
	return 30 * time.Second
}

func (e *declineCutoverEnv) newContact(t *testing.T, cadenceStr *string) repository.Contact {
	t.Helper()
	contact, err := e.contactRepo.CreateContact(e.ctx, repository.CreateContactRequest{
		FullName: e.gen.Contact(factory.WithNoMethods()).FullName,
		Cadence:  cadenceStr,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.contactRepo.HardDeleteContact(e.ctx, contact.ID) })
	return *contact
}

// seedStoredEvent upserts an accepted/confirmed calendar_event matched to the
// contact and returns it. gcalID is the upstream Google id; the returned
// internal UUID is what derived interactions reference as source_ref.
func (e *declineCutoverEnv) seedStoredEvent(t *testing.T, gcalID string, contactID uuid.UUID, endTime time.Time) repository.CalendarEvent {
	t.Helper()
	title := "decline-cutover-event"
	accepted := "accepted"
	stored, err := e.calendarRepo.Upsert(e.ctx, repository.UpsertCalendarEventRequest{
		GcalEventID:       gcalID,
		GcalCalendarID:    "primary",
		GoogleAccountID:   e.accountID,
		Title:             &title,
		StartTime:         endTime.Add(-time.Hour),
		EndTime:           endTime,
		Status:            "confirmed",
		UserResponse:      &accepted,
		Attendees:         []repository.Attendee{},
		MatchedContactIDs: []uuid.UUID{contactID},
		SyncedAt:          endTime,
	})
	require.NoError(t, err)
	return *stored
}

// declinedSelfEvent builds a minimal cancelled/declined delta for the gcal id
// (organizer is the user → getUserResponse synthesizes "accepted", so the
// status="cancelled" clause drives keep=false). No Start/End DateTime, proving
// the remove branch runs without parsed times.
func cancelledSelfEvent(gcalID, accountID string) *calendarapi.Event {
	return &calendarapi.Event{
		Id:        gcalID,
		Status:    "cancelled",
		Start:     &calendarapi.EventDateTime{},
		End:       &calendarapi.EventDateTime{},
		Organizer: &calendarapi.EventOrganizer{Email: accountID},
	}
}

// waitForInteractionGone polls until the interaction for (contact, source_ref)
// is soft-deleted (FindBySourceRef returns ErrNotFound) or the timeout fires.
func (e *declineCutoverEnv) waitForInteractionGone(t *testing.T, contactID uuid.UUID, sourceRef string, timeout time.Duration) {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(timeout)
	for accelerated.GetCurrentTime().Before(deadline) {
		_, err := e.interactionRepo.FindBySourceRef(e.ctx, contactID, repository.InteractionSourceGCal, sourceRef)
		if errors.Is(err, db.ErrNotFound) {
			return
		}
		require.NoError(t, err)
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for interaction (contact=%s ref=%s) to be soft-deleted", contactID, sourceRef)
}

// TestIntegration_DeclineCutover_RemovesEventAndInteractionThroughProcessEvent
// drives the real cutover remove branch through processEvent with a real bus +
// pool and the live decline consumer, asserting the full chain.
func TestIntegration_DeclineCutover_RemovesEventAndInteractionThroughProcessEvent(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newDeclineCutoverEnv(t, ctx)

	cad := "weekly"
	contact := e.newContact(t, &cad)
	gcalID := "evt-" + uuid.NewString()
	// Truncate to microseconds so the Go anchor survives the timestamptz
	// round-trip exactly (Postgres stores microsecond precision).
	endTime := accelerated.GetCurrentTime().Truncate(time.Microsecond).AddDate(0, 0, -1) // past

	stored := e.seedStoredEvent(t, gcalID, contact.ID, endTime)
	internalRef := stored.ID.String()

	// Record the derived gcal interaction the production way (source_ref =
	// internal calendar_event.ID) and apply cadence so the four contact date
	// columns land. Also seed an attended event-log row so the coexistence
	// assertion has the attended counterpart of the declined: key.
	ref := internalRef
	_, err := e.interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID: contact.ID, Source: repository.InteractionSourceGCal, SourceRef: &ref,
		OccurredAt: endTime, Direction: repository.InteractionDirectionMutual,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = e.interactionRepo.HardDeleteInteractionsBySourceRefPrefix(ctx, repository.InteractionSourceGCal, internalRef)
	})
	require.NoError(t, pgx.BeginTxFunc(ctx, e.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return e.cadenceUpdater.ApplyInteraction(ctx, tx, repository.ApplyInteractionRequest{
			ContactID: contact.ID, Direction: repository.InteractionDirectionMutual,
			Source: repository.InteractionSourceGCal, OccurredAt: endTime,
		})
	}))
	attendedSourceID := internalRef + ":" + contact.ID.String()
	e.seedAttendedEventRow(t, attendedSourceID, contact.ID, internalRef, endTime)

	// Drive the REAL cutover remove branch through processEvent.
	require.NoError(t, e.provider.RunProcessEventForTest(ctx, cancelledSelfEvent(gcalID, e.accountID), e.accountID))

	// calendar_event row deleted.
	_, err = e.calendarRepo.GetByGcalID(ctx, gcalID, "primary", e.accountID)
	require.ErrorIs(t, err, db.ErrNotFound, "cutover remove branch deletes the stored calendar_event")

	// declined: event-log row landed and coexists with the attended row.
	declinedSourceID := "declined:" + internalRef + ":" + contact.ID.String()
	declined, err := e.eventRepo.FindEventBySource(ctx, repository.InteractionSourceGCal, declinedSourceID)
	require.NoError(t, err, "declined: event row published by the cutover branch")
	require.Equal(t, events.KindCalendarDeclined, declined.Kind)
	attended, err := e.eventRepo.FindEventBySource(ctx, repository.InteractionSourceGCal, attendedSourceID)
	require.NoError(t, err, "attended event row still present (declined: prefix keeps them disjoint)")
	require.NotEqual(t, attended.ID, declined.ID, "two distinct event-log rows coexist")
	t.Cleanup(func() {
		_ = e.eventRepo.HardDeleteEventsBySourceAndSourceIDPrefix(ctx, repository.InteractionSourceGCal, declinedSourceID)
		_ = e.eventRepo.HardDeleteEventsBySourceAndSourceIDPrefix(ctx, repository.InteractionSourceGCal, attendedSourceID)
	})

	// The decline job drains → interaction soft-deleted + contact dates roll.
	e.waitForInteractionGone(t, contact.ID, internalRef, 30*time.Second)

	got, err := e.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Nil(t, got.LastContacted, "contact date columns roll back after the decline job drains")
	assert.Nil(t, got.LastOutreachAt)
	require.NotNil(t, got.ContactBy, "contact_by falls back to created_at + cadence")
}

// TestIntegration_DeclineCutover_PublishFailureLeavesRowIntact proves
// publish-before-delete: when PublishTx fails, the whole tx rolls back and the
// calendar_event row survives (the delete never commits).
func TestIntegration_DeclineCutover_PublishFailureLeavesRowIntact(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newDeclineCutoverEnv(t, ctx)

	contact := e.newContact(t, nil)
	gcalID := "evt-" + uuid.NewString()
	endTime := accelerated.GetCurrentTime().Truncate(time.Microsecond).AddDate(0, 0, -1)
	stored := e.seedStoredEvent(t, gcalID, contact.ID, endTime)

	// Substitute a failing decline bus so the per-contact PublishTx errors
	// BEFORE the delete. publish-before-delete means the delete must roll back.
	e.provider.SetDeclineBusForTest(failingDeclineBus{})

	err := e.provider.RunProcessEventForTest(ctx, cancelledSelfEvent(gcalID, e.accountID), e.accountID)
	require.Error(t, err, "a failing PublishTx must surface as an error")

	// The calendar_event row must still exist (delete rolled back with publish).
	got, err := e.calendarRepo.GetByGcalID(ctx, gcalID, "primary", e.accountID)
	require.NoError(t, err, "publish-before-delete: a publish failure leaves the calendar_event intact")
	require.Equal(t, stored.ID, got.ID)

	// And no declined: event row leaked.
	declinedSourceID := "declined:" + stored.ID.String() + ":" + contact.ID.String()
	_, err = e.eventRepo.FindEventBySource(ctx, repository.InteractionSourceGCal, declinedSourceID)
	require.ErrorIs(t, err, db.ErrNotFound, "the rolled-back tx leaves no declined event-log row")
}

// failingDeclineBus is a busTx whose PublishTx always errors, used to assert
// publish-before-delete ordering in the cutover remove branch.
type failingDeclineBus struct{}

func (failingDeclineBus) PublishTx(_ context.Context, _ pgx.Tx, _ *events.Envelope) error {
	return errors.New("simulated publish failure")
}

// seedAttendedEventRow inserts a calendar.attended event-log row directly
// (keyed <internalRef>:<contactID>) so the coexistence assertion has the
// attended counterpart of the declined: key without driving the attended
// consumer.
func (e *declineCutoverEnv) seedAttendedEventRow(t *testing.T, sourceID string, contactID uuid.UUID, internalRef string, occurredAt time.Time) {
	t.Helper()
	raw, err := events.Marshal(events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version: 1, ContactID: contactID, EventID: internalRef, OccurredAt: occurredAt,
	})
	require.NoError(t, err)
	env := &events.Envelope{
		ID:         uuid.New(),
		Source:     repository.InteractionSourceGCal,
		SourceID:   sourceID,
		Kind:       events.KindCalendarAttended,
		Payload:    raw,
		ObservedAt: occurredAt,
	}
	require.NoError(t, pgx.BeginTxFunc(e.ctx, e.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return e.eventRepo.InsertEvent(e.ctx, tx, env)
	}))
}
