package google

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"personal-crm/backend/internal/accelerated"
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
}

// NewCalendarRematchHandler constructs a CalendarRematchHandler. The recorder
// must be the same *service.ContactService instance used by CalendarSyncProvider
// so per-source dedup behavior is identical between sync and rematch paths.
func NewCalendarRematchHandler(
	cr *repository.CalendarEventRepository,
	er *repository.ExternalContactRepository,
	rec calendarRematchInteractionRecorder,
) *CalendarRematchHandler {
	return &CalendarRematchHandler{
		calendarRepo: cr,
		externalRepo: er,
		recorder:     rec,
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
		// Past events: record the interaction now so last_contacted reflects
		// the event without a scheduler race. RecordInteraction dedupes on
		// (contact_id, source, source_ref) — both at the service layer
		// (ContactService.RecordInteraction → FindBySourceRef) and at the DB
		// layer (idx_interaction_source_ref UNIQUE). Safe to call repeatedly.
		if e.EndTime.Before(now) {
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
