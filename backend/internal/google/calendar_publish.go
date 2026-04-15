package google

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// calendarEventPublisher is the subset of *events.Bus used by
// publishCalendarAttended. Interface lets tests stub Publish without a
// real bus or river client.
type calendarEventPublisher interface {
	Publish(ctx context.Context, env *events.Envelope) error
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
) error {
	payload := events.CalendarAttendedPayload{
		Version:    1,
		ContactID:  contactID,
		EventID:    eventIDStr,
		OccurredAt: occurredAt,
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
