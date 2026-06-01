// Integration coverage for the calendar decline-removal path (issue #366):
// when a stored calendar event is declined / cancelled / user-removed
// upstream, the derived gcal interaction is soft-deleted and the contact's
// date columns are surgically recomputed.
//
// These tests hit a real DB. They cover:
//   - CalendarDeclineHandler consumer (soft-delete + recompute) end-to-end
//   - the surgical RecomputeContactDatesAfterDelete query, per column +
//     provenance-safety guards (creation value preserved, contact_by
//     override preserved, NULL-out, per-direction, forward-writer parity)
//   - decline replay idempotency
//   - Decision-3a attended-after-delete guard (skip-when-deleted +
//     FOR SHARE / FOR UPDATE lock serialization)
//   - the declined: SourceID namespace coexisting with the attended row
package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
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

// declineTestEnv bundles the repositories + consumer used by the
// decline-removal integration tests.
type declineTestEnv struct {
	ctx             context.Context
	database        *db.Database
	contactRepo     *repository.ContactRepository
	interactionRepo *repository.InteractionRepository
	calendarRepo    *repository.CalendarEventRepository
	cadenceUpdater  *consumer.CadenceUpdater
	declineHandler  *consumer.CalendarDeclineHandler
}

func newDeclineTestEnv(t *testing.T, ctx context.Context) *declineTestEnv {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	cfg := config.TestConfig()
	cfg.Database.URL = os.Getenv("DATABASE_URL")
	cfg.Database.MigrationsPath = getMigrationsPath()
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	calendarRepo := repository.NewCalendarEventRepository(database.Queries)
	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)
	cadenceUpdater := consumer.NewCadenceUpdater(
		claimRepo, contactRepo, database.Queries, consumer.CadenceModeCutover, false,
	)
	declineHandler := consumer.NewCalendarDeclineHandler(interactionRepo, contactRepo)

	return &declineTestEnv{
		ctx:             ctx,
		database:        database,
		contactRepo:     contactRepo,
		interactionRepo: interactionRepo,
		calendarRepo:    calendarRepo,
		cadenceUpdater:  cadenceUpdater,
		declineHandler:  declineHandler,
	}
}

func (e *declineTestEnv) newContact(t *testing.T, cadenceStr *string, lastContacted *time.Time) repository.Contact {
	t.Helper()
	contact, err := e.contactRepo.CreateContact(e.ctx, repository.CreateContactRequest{
		FullName:      "decline-removal-" + uuid.NewString()[:8],
		Cadence:       cadenceStr,
		LastContacted: lastContacted,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.contactRepo.HardDeleteContact(e.ctx, contact.ID) })
	return *contact
}

// seedGcalInteraction creates a gcal interaction with the given source_ref
// (the internal calendar_event.ID string) + direction + occurred_at, then
// applies it through the real CadenceUpdater so the contact's date columns
// land exactly as the forward path would set them.
func (e *declineTestEnv) seedGcalInteraction(t *testing.T, contactID uuid.UUID, sourceRef, direction string, occurredAt time.Time) *repository.Interaction {
	t.Helper()
	ref := sourceRef
	interaction, err := e.interactionRepo.CreateInteraction(e.ctx, repository.CreateInteractionRequest{
		ContactID:  contactID,
		Source:     repository.InteractionSourceGCal,
		SourceRef:  &ref,
		OccurredAt: occurredAt,
		Direction:  direction,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = e.interactionRepo.HardDeleteInteractionsBySourceRefPrefix(e.ctx, repository.InteractionSourceGCal, sourceRef)
	})

	applyInTx(t, e.database, func(tx pgx.Tx) error {
		return e.cadenceUpdater.ApplyInteraction(e.ctx, tx, repository.ApplyInteractionRequest{
			ContactID:  contactID,
			Direction:  direction,
			Source:     repository.InteractionSourceGCal,
			OccurredAt: occurredAt,
		})
	})
	return interaction
}

// runDecline soft-deletes the interaction + recomputes the contact's date
// columns via CalendarDeclineHandler.HandleEvent in a fresh tx (mirroring
// the river worker). The envelope carries the internal-UUID source_ref.
func (e *declineTestEnv) runDecline(t *testing.T, contactID uuid.UUID, sourceRef string, occurredAt time.Time) {
	t.Helper()
	payload := events.CalendarDeclinedPayload{
		Version: 1, ContactID: contactID, EventID: sourceRef, OccurredAt: occurredAt,
	}
	raw, err := events.Marshal(events.KindCalendarDeclined, payload)
	require.NoError(t, err)
	env := &events.Envelope{
		ID:         uuid.New(),
		Source:     repository.InteractionSourceGCal,
		SourceID:   "declined:" + sourceRef + ":" + contactID.String(),
		Kind:       events.KindCalendarDeclined,
		Payload:    raw,
		ObservedAt: occurredAt,
	}
	require.NoError(t, pgx.BeginTxFunc(e.ctx, e.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return e.declineHandler.HandleEvent(e.ctx, tx, env)
	}))
}

// Test 5: consumer-direct soft-delete + recompute.
func TestIntegration_CalendarDecline_SoftDeletesAndRecomputes(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newDeclineTestEnv(t, ctx)

	occurredAt := time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)
	contact := e.newContact(t, nil, nil)
	sourceRef := uuid.NewString()
	e.seedGcalInteraction(t, contact.ID, sourceRef, repository.InteractionDirectionMutual, occurredAt)

	// Sanity: the forward path set the four columns to occurredAt.
	pre, err := e.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, pre.LastContacted)
	assert.Equal(t, occurredAt.UTC(), pre.LastContacted.UTC())

	e.runDecline(t, contact.ID, sourceRef, occurredAt)

	// Interaction soft-deleted.
	_, err = e.interactionRepo.FindBySourceRef(ctx, contact.ID, repository.InteractionSourceGCal, sourceRef)
	require.ErrorIs(t, err, db.ErrNotFound, "decline soft-deletes the derived interaction")

	// Contact dates rolled to NULL (no remaining interactions, no cadence).
	got, err := e.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Nil(t, got.LastContacted, "last_contacted NULLs when the sourcing interaction is removed and none remain")
	assert.Nil(t, got.LastInteractionAt)
	assert.Nil(t, got.LastResponseAt)
	assert.Nil(t, got.LastOutreachAt)
}

// Test 6(a): NULL-out (sourced + none remain) + contact_by from created_at.
func TestIntegration_CalendarDecline_NullOutWithCadenceFallback(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newDeclineTestEnv(t, ctx)

	cad := "weekly"
	// Anchor the interaction AFTER creation so it wins forward-max and truly
	// sources last_contacted + contact_by (a past occurredAt would be blocked
	// by the created_at-based contact_by floor, masking the rollback path).
	occurredAt := accelerated.GetCurrentTime().AddDate(0, 0, 30)
	// Contact created WITHOUT last_contacted so its only date source is the
	// gcal interaction; contact_by initially derives from created_at.
	contact := e.newContact(t, &cad, nil)
	sourceRef := uuid.NewString()
	e.seedGcalInteraction(t, contact.ID, sourceRef, repository.InteractionDirectionMutual, occurredAt)

	require.NoError(t, e.interactionRepo.SoftDeleteInteraction(ctx, mustFindInteraction(t, e, contact.ID, sourceRef).ID))
	require.NoError(t, e.contactRepo.RecomputeContactDatesAfterDelete(ctx, contact.ID, occurredAt))

	got, err := e.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Nil(t, got.LastContacted, "sourced + none remain → NULL")
	assert.Nil(t, got.LastOutreachAt)
	require.NotNil(t, got.ContactBy, "contact_by falls back to created_at + cadence, NOT NULL")
	assertSameCadenceDate(t, cadence.CalculateContactBy(got.CreatedAt, cadence.CadenceWeekly), *got.ContactBy,
		"contact_by re-derived from created_at after the sourcing interaction is removed")
}

// Test 6(b): cadence-unset variant → contact_by left as-is (NULL).
func TestIntegration_CalendarDecline_NoCadence_ContactByStaysNil(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newDeclineTestEnv(t, ctx)

	occurredAt := time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)
	contact := e.newContact(t, nil, nil)
	sourceRef := uuid.NewString()
	e.seedGcalInteraction(t, contact.ID, sourceRef, repository.InteractionDirectionMutual, occurredAt)

	require.NoError(t, e.interactionRepo.SoftDeleteInteraction(ctx, mustFindInteraction(t, e, contact.ID, sourceRef).ID))
	require.NoError(t, e.contactRepo.RecomputeContactDatesAfterDelete(ctx, contact.ID, occurredAt))

	got, err := e.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Nil(t, got.ContactBy, "no cadence → contact_by untouched (stays nil)")
}

// Test 6(c): per-direction — inbound(earlier) + outbound(latest) + mutual
// (middle) live; remove the mutual; last_outreach_at rolls to the outbound
// time, the non-outbound columns roll to the inbound time.
func TestIntegration_CalendarDecline_PerDirectionRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newDeclineTestEnv(t, ctx)

	cad := "weekly"
	// Anchor after creation so the mutual sources contact_by via forward-max.
	base := accelerated.GetCurrentTime()
	inbound := base.AddDate(0, 0, 10)
	mutual := base.AddDate(0, 0, 20)
	outbound := base.AddDate(0, 0, 30)
	contact := e.newContact(t, &cad, nil)

	inboundRef := uuid.NewString()
	mutualRef := uuid.NewString()
	outboundRef := uuid.NewString()
	// Seed all three. Forward-max over the non-outbound subset {inbound,
	// mutual} → last_contacted = mutual (Mar 10 > Mar 1). Over the
	// outbound/mutual subset {mutual, outbound} → last_outreach_at = outbound
	// (Mar 20 > Mar 10).
	e.seedGcalInteraction(t, contact.ID, inboundRef, repository.InteractionDirectionInbound, inbound)
	e.seedGcalInteraction(t, contact.ID, mutualRef, repository.InteractionDirectionMutual, mutual)
	e.seedGcalInteraction(t, contact.ID, outboundRef, repository.InteractionDirectionOutbound, outbound)

	// At this point: last_contacted = mutual (max non-outbound), last_outreach_at = outbound (max).
	pre, err := e.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, pre.LastContacted)
	assert.Equal(t, mutual.UTC(), pre.LastContacted.UTC())
	require.NotNil(t, pre.LastOutreachAt)
	assert.Equal(t, outbound.UTC(), pre.LastOutreachAt.UTC())

	// Remove the mutual (it sourced last_contacted = mutual).
	require.NoError(t, e.interactionRepo.SoftDeleteInteraction(ctx, mustFindInteraction(t, e, contact.ID, mutualRef).ID))
	require.NoError(t, e.contactRepo.RecomputeContactDatesAfterDelete(ctx, contact.ID, mutual))

	got, err := e.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastContacted)
	assert.Equal(t, inbound.UTC(), got.LastContacted.UTC(), "non-outbound rolls to remaining inbound")
	require.NotNil(t, got.LastResponseAt)
	assert.Equal(t, inbound.UTC(), got.LastResponseAt.UTC())
	require.NotNil(t, got.LastOutreachAt)
	assert.Equal(t, outbound.UTC(), got.LastOutreachAt.UTC(), "outbound subset unchanged (outbound interaction still live)")
	require.NotNil(t, got.ContactBy)
	assertSameCadenceDate(t, cadence.CalculateContactBy(inbound, cadence.CadenceWeekly), *got.ContactBy,
		"contact_by re-derived from the remaining inbound after the mutual is removed")
}

// Test 6(d): PROVENANCE GUARD — a creation-set last_contacted (no backing
// interaction, distinct instant from the gcal occurred_at) is NOT erased.
func TestIntegration_CalendarDecline_PreservesCreationValue(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newDeclineTestEnv(t, ctx)

	// Creation-set last_contacted distinct from the gcal occurred_at.
	creationLC := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	gcalOccurredAt := time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)
	contact := e.newContact(t, nil, &creationLC)

	// Seed the gcal interaction but do NOT apply it (so last_contacted stays
	// at the creation value, not the gcal time). The interaction exists with
	// occurred_at = gcalOccurredAt; last_contacted = creationLC != gcalOccurredAt.
	sourceRef := uuid.NewString()
	ref := sourceRef
	created, err := e.interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID: contact.ID, Source: repository.InteractionSourceGCal, SourceRef: &ref,
		OccurredAt: gcalOccurredAt, Direction: repository.InteractionDirectionMutual,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = e.interactionRepo.HardDeleteInteractionsBySourceRefPrefix(ctx, repository.InteractionSourceGCal, sourceRef)
	})

	require.NoError(t, e.interactionRepo.SoftDeleteInteraction(ctx, created.ID))
	require.NoError(t, e.contactRepo.RecomputeContactDatesAfterDelete(ctx, contact.ID, gcalOccurredAt))

	got, err := e.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastContacted)
	assert.Equal(t, creationLC.UTC(), got.LastContacted.UTC(),
		"creation-set last_contacted (!= deleted ts) must be preserved")
}

// Test 6(e): PROVENANCE GUARD — a Todoist/user contact_by override is
// preserved even when the gcal interaction sourced last_contacted.
func TestIntegration_CalendarDecline_PreservesContactByOverride(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newDeclineTestEnv(t, ctx)

	cad := "weekly"
	// Anchor after creation so the interaction sources last_contacted.
	occurredAt := accelerated.GetCurrentTime().AddDate(0, 0, 30)
	contact := e.newContact(t, &cad, nil)
	sourceRef := uuid.NewString()
	e.seedGcalInteraction(t, contact.ID, sourceRef, repository.InteractionDirectionMutual, occurredAt)

	// Apply a Todoist/user override AFTER the interaction: a contact_by that
	// differs from CalculateContactBy(last_contacted=occurredAt, weekly).
	override := time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC)
	applyInTx(t, e.database, func(tx pgx.Tx) error {
		return e.cadenceUpdater.ApplyContactByOverride(ctx, tx, contact.ID, &override)
	})

	require.NoError(t, e.interactionRepo.SoftDeleteInteraction(ctx, mustFindInteraction(t, e, contact.ID, sourceRef).ID))
	require.NoError(t, e.contactRepo.RecomputeContactDatesAfterDelete(ctx, contact.ID, occurredAt))

	got, err := e.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Nil(t, got.LastContacted, "last_contacted rolls back (it was sourced by the removed interaction)")
	require.NotNil(t, got.ContactBy)
	assertSameRawDate(t, override, *got.ContactBy, "the contact_by override survives the decline recompute")
}

// Test 6(f): contact_by correctly rolled back when it still reflects the
// removed interaction (no override since).
func TestIntegration_CalendarDecline_RollsBackContactByWhenNoOverride(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newDeclineTestEnv(t, ctx)

	cad := "weekly"
	// Anchor after creation so the interaction sources contact_by.
	occurredAt := accelerated.GetCurrentTime().AddDate(0, 0, 30)
	contact := e.newContact(t, &cad, nil)
	sourceRef := uuid.NewString()
	e.seedGcalInteraction(t, contact.ID, sourceRef, repository.InteractionDirectionMutual, occurredAt)

	// Sanity: the interaction set contact_by = CalculateContactBy(occurredAt, weekly).
	pre, err := e.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, pre.ContactBy)
	assertSameCadenceDate(t, cadence.CalculateContactBy(occurredAt, cadence.CadenceWeekly), *pre.ContactBy,
		"forward path set contact_by from the interaction's occurred_at")

	require.NoError(t, e.interactionRepo.SoftDeleteInteraction(ctx, mustFindInteraction(t, e, contact.ID, sourceRef).ID))
	require.NoError(t, e.contactRepo.RecomputeContactDatesAfterDelete(ctx, contact.ID, occurredAt))

	got, err := e.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Nil(t, got.LastContacted, "last_contacted NULLs (sourced + none remain)")
	require.NotNil(t, got.ContactBy)
	assertSameCadenceDate(t, cadence.CalculateContactBy(got.CreatedAt, cadence.CadenceWeekly), *got.ContactBy,
		"contact_by re-derived from created_at (the removed interaction's own contribution undone)")
}

// Test 6(g): forward-writer parity — the recomputed contact_by equals what
// the forward path would store for the equivalent base+cadence.
func TestIntegration_CalendarDecline_ContactByForwardWriterParity(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newDeclineTestEnv(t, ctx)

	cad := "monthly"
	occurredAt := accelerated.GetCurrentTime().AddDate(0, 0, 30)
	contact := e.newContact(t, &cad, nil)
	sourceRef := uuid.NewString()
	e.seedGcalInteraction(t, contact.ID, sourceRef, repository.InteractionDirectionMutual, occurredAt)

	require.NoError(t, e.interactionRepo.SoftDeleteInteraction(ctx, mustFindInteraction(t, e, contact.ID, sourceRef).ID))
	require.NoError(t, e.contactRepo.RecomputeContactDatesAfterDelete(ctx, contact.ID, occurredAt))

	got, err := e.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ContactBy)

	// Forward-writer reference: the recompute re-derives contact_by from the
	// contact's own created_at; assert it matches CalculateContactBy(created_at,
	// monthly) — the exact value the forward writer would store for that base.
	assertSameCadenceDate(t, cadence.CalculateContactBy(got.CreatedAt, cadence.CadenceMonthly), *got.ContactBy,
		"recompute contact_by matches the forward writer for the created_at + monthly base")
}

// Test 7: replay — delivering the same decline twice is a safe no-op.
func TestIntegration_CalendarDecline_ReplayIsNoOp(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newDeclineTestEnv(t, ctx)

	occurredAt := time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)
	contact := e.newContact(t, nil, nil)
	sourceRef := uuid.NewString()
	e.seedGcalInteraction(t, contact.ID, sourceRef, repository.InteractionDirectionMutual, occurredAt)

	e.runDecline(t, contact.ID, sourceRef, occurredAt)
	// Second delivery: interaction already soft-deleted → FindBySourceRefTx
	// returns ErrNotFound → no-op, no error.
	e.runDecline(t, contact.ID, sourceRef, occurredAt)

	got, err := e.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Nil(t, got.LastContacted)
}

// Test 8(a): Decision-3a skip-when-deleted — calendar.attended whose backing
// calendar_event was deleted → no interaction written.
func TestIntegration_AttendedAfterDelete_SkipsInsert(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newDeclineTestEnv(t, ctx)
	contact := e.newContact(t, nil, nil)

	// EventID points at a calendar_event that does NOT exist.
	missingEventID := uuid.New()

	var exists bool
	require.NoError(t, pgx.BeginTxFunc(ctx, e.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		exists, err = e.calendarRepo.LockExistsByIDTx(ctx, tx, missingEventID)
		return err
	}))
	assert.False(t, exists, "LockExistsByIDTx reports a missing calendar_event as not present → attended insert is skipped")

	// And no interaction exists for that source_ref.
	_, err := e.interactionRepo.FindBySourceRef(ctx, contact.ID, repository.InteractionSourceGCal, missingEventID.String())
	require.ErrorIs(t, err, db.ErrNotFound)
}

// Test 8(b): Decision-3a lock-serialization — the attended FOR SHARE
// conflicts with a concurrent FOR UPDATE NOWAIT (proving the lock is held),
// without sleeps. Then the decline DELETE soft-deletes the just-written
// interaction; and the reverse order (decline-first) skips the attended insert.
func TestIntegration_AttendedAfterDelete_LockSerialization(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newDeclineTestEnv(t, ctx)
	contact := e.newContact(t, nil, nil)

	accountID := "decline-lock-" + uuid.NewString()
	stored := e.seedCalendarEvent(t, accountID, contact.ID)

	// Open attended tx A and acquire the FOR SHARE lock (do NOT commit yet).
	txA, err := e.database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = txA.Rollback(ctx) }()
	existsA, err := e.calendarRepo.LockExistsByIDTx(ctx, txA, stored.ID)
	require.NoError(t, err)
	require.True(t, existsA, "attended FOR SHARE acquired on the present row")

	// From tx B, FOR UPDATE NOWAIT must fail immediately (A holds FOR SHARE).
	txB, err := e.database.Pool.Begin(ctx)
	require.NoError(t, err)
	_, probeErr := e.calendarRepo.TestGetByIDForUpdateNoWaitTx(ctx, txB, stored.ID)
	require.Error(t, probeErr, "FOR UPDATE NOWAIT must conflict with the attended FOR SHARE held by tx A")
	_ = txB.Rollback(ctx)

	// Commit A (in production this is where the attended interaction insert
	// would commit). Release the lock.
	require.NoError(t, txA.Commit(ctx))

	// Reverse order: decline DELETE commits first → attended FOR SHARE read
	// returns no row → skip. Delete the event, then assert LockExistsByIDTx
	// reports gone.
	require.NoError(t, e.calendarRepo.DeleteByGcalID(ctx, stored.GcalEventID, "primary", accountID))
	var existsAfterDelete bool
	require.NoError(t, pgx.BeginTxFunc(ctx, e.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		existsAfterDelete, err = e.calendarRepo.LockExistsByIDTx(ctx, tx, stored.ID)
		return err
	}))
	assert.False(t, existsAfterDelete, "after the decline DELETE, the attended branch sees no row → skips the insert")
}

// Test: declined: SourceID coexists with the attended row (P0-collision
// regression). Publishing both an attended and a declined event for the same
// internal UUID + contact yields TWO distinct event-log rows.
func TestIntegration_DeclineSourceID_CoexistsWithAttended(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	ctx := context.Background()
	e := newDeclineTestEnv(t, ctx)

	eventRepo := repository.NewEventRepository(e.database.Queries)
	internalUUID := uuid.NewString()
	contactID := uuid.New()
	occurredAt := time.Date(2026, 3, 15, 11, 0, 0, 0, time.UTC)

	attendedSourceID := internalUUID + ":" + contactID.String()
	declinedSourceID := "declined:" + internalUUID + ":" + contactID.String()

	insertEvent(t, ctx, e, eventRepo, attendedSourceID, events.KindCalendarAttended, attendedPayload(t, contactID, internalUUID, occurredAt))
	insertEvent(t, ctx, e, eventRepo, declinedSourceID, events.KindCalendarDeclined, declinedPayload(t, contactID, internalUUID, occurredAt))
	t.Cleanup(func() { hardDeleteEventBySource(t, ctx, e, attendedSourceID, declinedSourceID) })

	attended, err := eventRepo.FindEventBySource(ctx, repository.InteractionSourceGCal, attendedSourceID)
	require.NoError(t, err, "attended event row present")
	declined, err := eventRepo.FindEventBySource(ctx, repository.InteractionSourceGCal, declinedSourceID)
	require.NoError(t, err, "declined event row present (declined: prefix keeps it disjoint from attended)")
	require.NotEqual(t, attended.ID, declined.ID, "two distinct event-log rows coexist")
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func mustFindInteraction(t *testing.T, e *declineTestEnv, contactID uuid.UUID, sourceRef string) *repository.Interaction {
	t.Helper()
	got, err := e.interactionRepo.FindBySourceRef(e.ctx, contactID, repository.InteractionSourceGCal, sourceRef)
	require.NoError(t, err)
	return got
}

// assertSameCadenceDate compares a stored contact_by DATE against a
// cadence-derived expected value (cadence.CalculateContactBy, local-midnight
// in production). The forward writer stores DateOnly(expected) so the DB DATE
// is the LOCAL calendar day of expected; reading it back yields UTC-midnight
// of that day. So compare expected's LOCAL date against the stored value's
// UTC date. Mirrors the contact_by parity assertions in
// contact_by_integration_test.go.
func assertSameCadenceDate(t *testing.T, expected, stored time.Time, msg string) {
	t.Helper()
	eY, eM, eD := cadence.DateOnly(expected).Date()
	gY, gM, gD := stored.UTC().Date()
	assert.Equalf(t, [3]int{eY, int(eM), eD}, [3]int{gY, int(gM), gD},
		"expected DATE %04d-%02d-%02d, got %04d-%02d-%02d: %s", eY, eM, eD, gY, gM, gD, msg)
}

// assertSameRawDate compares a stored contact_by DATE against a raw
// (override) date value. The override is written verbatim (UTC midnight), so
// both reduce to a UTC calendar date.
func assertSameRawDate(t *testing.T, expected, stored time.Time, msg string) {
	t.Helper()
	eY, eM, eD := expected.UTC().Date()
	gY, gM, gD := stored.UTC().Date()
	assert.Equalf(t, [3]int{eY, int(eM), eD}, [3]int{gY, int(gM), gD},
		"expected DATE %04d-%02d-%02d, got %04d-%02d-%02d: %s", eY, eM, eD, gY, gM, gD, msg)
}

// seedCalendarEvent upserts a stored calendar_event with one matched contact.
func (e *declineTestEnv) seedCalendarEvent(t *testing.T, accountID string, contactID uuid.UUID) repository.CalendarEvent {
	t.Helper()
	title := "decline-lock-event"
	stored, err := e.calendarRepo.Upsert(e.ctx, repository.UpsertCalendarEventRequest{
		GcalEventID:       "evt-" + uuid.NewString(),
		GcalCalendarID:    "primary",
		GoogleAccountID:   accountID,
		Title:             &title,
		StartTime:         time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		EndTime:           time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC),
		Status:            "confirmed",
		Attendees:         []repository.Attendee{},
		MatchedContactIDs: []uuid.UUID{contactID},
		SyncedAt:          time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.calendarRepo.DeleteEventsByAccount(e.ctx, accountID) })
	return *stored
}

func attendedPayload(t *testing.T, contactID uuid.UUID, eventID string, occurredAt time.Time) []byte {
	t.Helper()
	raw, err := events.Marshal(events.KindCalendarAttended, events.CalendarAttendedPayload{
		Version: 1, ContactID: contactID, EventID: eventID, OccurredAt: occurredAt,
	})
	require.NoError(t, err)
	return raw
}

func declinedPayload(t *testing.T, contactID uuid.UUID, eventID string, occurredAt time.Time) []byte {
	t.Helper()
	raw, err := events.Marshal(events.KindCalendarDeclined, events.CalendarDeclinedPayload{
		Version: 1, ContactID: contactID, EventID: eventID, OccurredAt: occurredAt,
	})
	require.NoError(t, err)
	return raw
}

func insertEvent(t *testing.T, ctx context.Context, e *declineTestEnv, eventRepo *repository.EventRepository, sourceID string, kind events.Kind, payload []byte) {
	t.Helper()
	env := &events.Envelope{
		ID:         uuid.New(),
		Source:     repository.InteractionSourceGCal,
		SourceID:   sourceID,
		Kind:       kind,
		Payload:    payload,
		ObservedAt: time.Date(2026, 3, 15, 11, 0, 0, 0, time.UTC),
	}
	require.NoError(t, pgx.BeginTxFunc(ctx, e.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return eventRepo.InsertEvent(ctx, tx, env)
	}))
}

func hardDeleteEventBySource(t *testing.T, ctx context.Context, e *declineTestEnv, sourceIDs ...string) {
	t.Helper()
	eventRepo := repository.NewEventRepository(e.database.Queries)
	for _, sid := range sourceIDs {
		// Exact sourceID works as its own prefix.
		_ = eventRepo.HardDeleteEventsBySourceAndSourceIDPrefix(ctx, repository.InteractionSourceGCal, sid)
	}
}
