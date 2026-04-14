package google

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// publishCalendarAttended emits a calendar.attended event for a
// newly-recorded calendar interaction. Shared by the scheduler path
// (CalendarSyncProvider.updateLastContactedForPastEvents) and the rematch
// path (CalendarRematchHandler.Rematch) so both produce matching shadow
// observations (plan Decisions 6 / 7.1).
//
// SourceID = eventIDStr (the calendar_event.ID as a string) — the same
// string the direct path uses for FindBySourceRef dedup. Consumer
// FindBySourceRefTx on (contact_id, "gcal", eventIDStr) finds the direct-
// path row written by the same call chain → replay early-return →
// writer='consumer' replay=true observation.
func publishCalendarAttended(
	ctx context.Context,
	bus *events.Bus,
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
		SourceID:   eventIDStr,
		Kind:       events.KindCalendarAttended,
		Payload:    raw,
		ObservedAt: occurredAt,
	}
	return bus.Publish(ctx, env)
}
