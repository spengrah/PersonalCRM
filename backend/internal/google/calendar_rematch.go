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

// calendarRematchInteractionRecorder is the subset of ContactService the
// calendar rematch handler depends on. Mirrors CalendarSyncProvider's recorder
// dependency — pass the same *service.ContactService instance.
type calendarRematchInteractionRecorder interface {
	RecordInteraction(ctx context.Context, req repository.RecordInteractionRequest) (*repository.Interaction, error)
}

// CalendarRematchHandler implements service.RematchHandler for the "email"
// identifier type. When an email is added to a CRM contact, this handler
// retroactively links any historical calendar events whose attendees include
// that email but where the contact wasn't matched at sync time. For events
// whose end_time has already passed, it records the interaction directly so
// the contact's last_contacted reflects the event without depending on the
// scheduler reading a now-stale matched_contact_ids snapshot (rematch plan
// Design Decision 6).
type CalendarRematchHandler struct {
	calendarRepo *repository.CalendarEventRepository
	externalRepo *repository.ExternalContactRepository
	recorder     calendarRematchInteractionRecorder
	// eventBus is the shadow-mode event bus. Nil when
	// EVENT_BUS_INTERACTION_MODE=off; non-nil triggers calendar.attended
	// publishes alongside the rematch-path RecordInteraction (plan
	// Decision 7.1 — the rematch path is a second calendar write site
	// that MUST publish to keep shadow parity).
	eventBus *events.Bus
}

// NewCalendarRematchHandler constructs a CalendarRematchHandler. The recorder
// must be the same *service.ContactService instance used by CalendarSyncProvider
// so per-source dedup behavior is identical between sync and rematch paths.
// eventBus may be nil; non-nil enables shadow-mode sibling publishes.
func NewCalendarRematchHandler(
	cr *repository.CalendarEventRepository,
	er *repository.ExternalContactRepository,
	rec calendarRematchInteractionRecorder,
	eventBus *events.Bus,
) *CalendarRematchHandler {
	return &CalendarRematchHandler{
		calendarRepo: cr,
		externalRepo: er,
		recorder:     rec,
		eventBus:     eventBus,
	}
}

// IdentifierType returns the contact_method type this handler binds to.
func (h *CalendarRematchHandler) IdentifierType() string { return "email" }

// Rematch finds calendar events whose attendees include the email, appends
// the contact to each event's matched_contact_ids, and records past-event
// interactions directly.
func (h *CalendarRematchHandler) Rematch(ctx context.Context, contactID uuid.UUID, emailNormalized string) (int, error) {
	// Defensive normalization: contact_method.value_normalized should already
	// match matching.NormalizeEmail, but run through it again so the SQL's
	// LOWER comparison stays correct even if the migration 022 trigger drifts.
	email := matching.NormalizeEmail(emailNormalized)
	if email == "" {
		return 0, nil
	}

	events, err := h.calendarRepo.FindEventsByAttendeeEmailUnmatchedForContact(ctx, email, contactID)
	if err != nil {
		return 0, fmt.Errorf("find events: %w", err)
	}

	now := accelerated.GetCurrentTime()
	matched := 0
	for _, e := range events {
		if err := h.calendarRepo.AppendMatchedContact(ctx, e.ID, contactID); err != nil {
			logger.Warn().Err(err).
				Str("event_id", e.ID.String()).
				Str("contact_id", contactID.String()).
				Msg("calendar rematch: append failed")
			continue
		}
		// Past confirmed events: record the interaction now so last_contacted
		// reflects the event without a scheduler race. Match
		// ListPastEventsNeedingUpdate's filter (status = 'confirmed') so
		// rematch doesn't record interactions the scheduler would have
		// skipped for tentative events. RecordInteraction dedupes on
		// (contact_id, source, source_ref) — both at the service layer
		// (ContactService.RecordInteraction → FindBySourceRef) and at the DB
		// layer (idx_interaction_source_ref UNIQUE). Safe to call repeatedly.
		if e.EndTime.Before(now) && e.Status == "confirmed" {
			eventIDStr := e.ID.String()
			title := ""
			if e.Title != nil {
				title = *e.Title
			}
			if _, err := h.recorder.RecordInteraction(ctx, repository.RecordInteractionRequest{
				ContactID:   contactID,
				Source:      repository.InteractionSourceGCal,
				SourceRef:   &eventIDStr,
				OccurredAt:  e.EndTime,
				Description: &title,
			}); err != nil {
				logger.Warn().Err(err).
					Str("event_id", e.ID.String()).
					Str("contact_id", contactID.String()).
					Msg("calendar rematch: record interaction failed")
				// Continue — append already succeeded. If the scheduler picks
				// this event up later it will redo the work safely (idempotent).
			} else if h.eventBus != nil {
				// Shadow-mode sibling publish. Same kind + payload shape as
				// the scheduler-path publish in calendar.go so both code
				// paths emit a calendar.attended the consumer can observe.
				if pubErr := publishCalendarAttended(ctx, h.eventBus, contactID, eventIDStr, e.EndTime); pubErr != nil {
					logger.Warn().Err(pubErr).
						Str("event_id", e.ID.String()).
						Str("contact_id", contactID.String()).
						Msg("calendar rematch: shadow publish failed")
				}
			}
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
