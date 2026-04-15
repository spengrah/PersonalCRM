package google

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"
)

// CalendarRematchHandler implements service.RematchHandler for the "email"
// identifier type. When an email is added to a CRM contact, this handler
// retroactively links any historical calendar events whose attendees include
// that email but where the contact wasn't matched at sync time. For events
// whose end_time has already passed, it publishes calendar.attended so the
// async consumer writes the interaction.
//
// Post-PR-6 (cutover): the direct-path recorder has been removed.
// Past-confirmed events publish to the event bus BEFORE appending to
// matched_contact_ids — a publish failure leaves matched_contact_ids
// unchanged so the next rematch re-selects the pair. Append-then-publish
// would strand the interaction if publish failed (plan Decision 11a).
type CalendarRematchHandler struct {
	calendarRepo *repository.CalendarEventRepository
	externalRepo *repository.ExternalContactRepository
	// eventBus is required in cutover mode. Nil disables past-event
	// rematch writes entirely (spec §3.9).
	eventBus *events.Bus
}

// NewCalendarRematchHandler constructs a CalendarRematchHandler. eventBus
// is required in cutover (default post-PR-6).
func NewCalendarRematchHandler(
	cr *repository.CalendarEventRepository,
	er *repository.ExternalContactRepository,
	eventBus *events.Bus,
) *CalendarRematchHandler {
	return &CalendarRematchHandler{
		calendarRepo: cr,
		externalRepo: er,
		eventBus:     eventBus,
	}
}

// IdentifierType returns the contact_method type this handler binds to.
func (h *CalendarRematchHandler) IdentifierType() string { return "email" }

// Rematch finds calendar events whose attendees include the email, appends
// the contact to each event's matched_contact_ids, and publishes
// calendar.attended for past-confirmed events so the async consumer writes
// the interaction.
func (h *CalendarRematchHandler) Rematch(ctx context.Context, contactID uuid.UUID, emailNormalized string) (int, error) {
	// Defensive normalization: contact_method.value_normalized should already
	// match matching.NormalizeEmail, but run through it again so the SQL's
	// LOWER comparison stays correct even if the migration 022 trigger drifts.
	email := matching.NormalizeEmail(emailNormalized)
	if email == "" {
		return 0, nil
	}

	foundEvents, err := h.calendarRepo.FindEventsByAttendeeEmailUnmatchedForContact(ctx, email, contactID)
	if err != nil {
		return 0, fmt.Errorf("find events: %w", err)
	}

	now := accelerated.GetCurrentTime()
	matched := 0
	for _, e := range foundEvents {
		isPastConfirmed := e.EndTime.Before(now) && e.Status == "confirmed"

		// Past confirmed events: publish FIRST, then append. If publish
		// fails we skip append so the event stays in
		// FindEventsByAttendeeEmailUnmatchedForContact's result set on
		// the next rematch call (plan Decision 11a).
		if isPastConfirmed {
			if h.eventBus == nil {
				logger.Warn().
					Str("event_id", e.ID.String()).
					Str("contact_id", contactID.String()).
					Msg("calendar rematch: eventBus not configured; skipping past-confirmed event")
				continue
			}
			eventIDStr := e.ID.String()
			if pubErr := publishCalendarAttended(ctx, h.eventBus, contactID, eventIDStr, e.EndTime); pubErr != nil {
				logger.Warn().Err(pubErr).
					Str("event_id", e.ID.String()).
					Str("contact_id", contactID.String()).
					Msg("calendar rematch: publish failed; leaving matched_contact_ids unchanged to allow retry")
				continue
			}
		}

		if err := h.calendarRepo.AppendMatchedContact(ctx, e.ID, contactID); err != nil {
			logger.Warn().Err(err).
				Str("event_id", e.ID.String()).
				Str("contact_id", contactID.String()).
				Msg("calendar rematch: append failed")
			// For past-confirmed events we already published; the consumer
			// will write the interaction via the durable river queue.
			// matched_contact_ids is unchanged — the next rematch re-selects
			// this event and hits the (source, source_id) unique index →
			// publish no-ops; append retries.
			continue
		}
		matched++
	}

	// Mark any matching gcal_attendee external_contact rows as matched so they
	// disappear from the import candidate list. Errors are logged but never
	// returned — this is housekeeping, not correctness.
	candidates, err := h.externalRepo.FindBySourceAndSourceID(ctx, "gcal_attendee", email)
	if err != nil {
		logger.Warn().Err(err).Str("email", email).Msg("calendar rematch: external candidate lookup failed")
	}
	for _, ec := range candidates {
		if _, err := h.externalRepo.UpdateMatch(ctx, ec.ID, &contactID, repository.MatchStatusMatched); err != nil {
			logger.Warn().Err(err).
				Str("external_id", ec.ID.String()).
				Str("contact_id", contactID.String()).
				Msg("calendar rematch: external candidate update failed")
		}
	}

	return matched, nil
}
