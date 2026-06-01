package google

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// calendarEventPublisher is the subset of *events.Bus used by
// publishCalendarAttended. Interface lets tests stub Publish without a
// real bus or river client.
type calendarEventPublisher interface {
	Publish(ctx context.Context, env *events.Envelope) error
}

// busTx is the subset of *events.Bus used by publishCalendarDeclinedTx.
// The decline remove branch needs N publishes + one DELETE in a single
// atomic tx, so it publishes via PublishTx on the caller's tx rather than
// Publish (which opens its own tx).
type busTx interface {
	PublishTx(ctx context.Context, tx pgx.Tx, env *events.Envelope) error
}

// publishCalendarAttended emits a calendar.attended event for a past
// calendar event attended by a specific contact. Shared by the scheduler
// path (CalendarSyncProvider.updateLastContactedForPastEvents) and the
// rematch path (CalendarRematchHandler.Rematch).
//
// SourceID is per-(event, contact): "<eventIDStr>:<contactID>". The event
// table has a partial unique index on (source, source_id) WHERE source_id
// IS NOT NULL (migration 036), so a calendar event with N CRM-tracked
// attendees would collide on contacts 2..N if SourceID were just the
// event UUID. Per-entity SourceID lets each contact's row survive ingest;
// see .ai/rules/core.md "Multi-entity events" gotcha. Consumer dedup on
// (contact_id, "gcal", eventIDStr) uses the payload's EventID field, not
// the envelope's SourceID, so this format change is consumer-transparent.
func publishCalendarAttended(
	ctx context.Context,
	bus calendarEventPublisher,
	contactID uuid.UUID,
	eventIDStr string,
	occurredAt time.Time,
	title *string,
) error {
	payload := events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  contactID,
		EventID:    eventIDStr,
		OccurredAt: occurredAt,
		Title:      title,
	}
	raw, err := events.Marshal(events.KindCalendarAttended, payload)
	if err != nil {
		return fmt.Errorf("marshal calendar.attended: %w", err)
	}
	env := &events.Envelope{
		Source:     repository.InteractionSourceGCal,
		SourceID:   eventIDStr + ":" + contactID.String(),
		Kind:       events.KindCalendarAttended,
		Payload:    raw,
		ObservedAt: occurredAt,
	}
	return bus.Publish(ctx, env)
}

// publishCalendarDeclinedTx emits a calendar.declined event for a
// previously-stored calendar event that was declined / cancelled /
// user-removed upstream, for a specific matched contact. Published on the
// caller's tx (via PublishTx) so the N per-contact publishes + the
// calendar_event DELETE commit atomically (publish-before-delete; see the
// remove branch in CalendarSyncProvider.processEvent).
//
// eventIDStr is the INTERNAL calendar_event.ID UUID string (passed
// stored.ID.String() by the remove branch), so the payload's EventID matches
// the attended interaction's source_ref for source='gcal' — the decline
// consumer's FindBySourceRefTx(contactID, "gcal", eventIDStr) finds the right
// interaction.
//
// SourceID carries a "declined:" prefix so the event-log (source, source_id)
// unique index does NOT collide with the calendar.attended row
// ("<internal-uuid>:<contactID>"). Without the prefix, PublishTx would see
// the attended row as a duplicate, return nil WITHOUT enqueuing the decline
// handler, and the remove branch would delete the calendar_event — stranding
// the false interaction with no consumer to clean it up. The "declined:"
// namespace keeps both event rows + both consumers alive, while a second
// decline for the same (event, contact) still dedups against the first.
func publishCalendarDeclinedTx(
	ctx context.Context,
	bus busTx,
	tx pgx.Tx,
	contactID uuid.UUID,
	eventIDStr string,
	occurredAt time.Time,
) error {
	payload := events.CalendarDeclinedPayload{
		Version:    1,
		ContactID:  contactID,
		EventID:    eventIDStr,
		OccurredAt: occurredAt,
	}
	raw, err := events.Marshal(events.KindCalendarDeclined, payload)
	if err != nil {
		return fmt.Errorf("marshal calendar.declined: %w", err)
	}
	env := &events.Envelope{
		Source:     repository.InteractionSourceGCal,
		SourceID:   "declined:" + eventIDStr + ":" + contactID.String(),
		Kind:       events.KindCalendarDeclined,
		Payload:    raw,
		ObservedAt: occurredAt,
	}
	return bus.PublishTx(ctx, tx, env)
}
