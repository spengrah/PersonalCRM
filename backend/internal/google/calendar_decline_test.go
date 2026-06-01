package google

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/calendar/v3"
)

// These mock-based tests exercise the OFF-mode remove branch (the
// mock-backed newTestProvider* factories leave eventBus + pool nil). The
// cutover publish-before-delete + coexistence path is DB-backed and runs in
// the CI integration suite, so it lives in
// backend/tests/calendar_decline_removal_integration_test.go (driven through
// RunProcessEventForTest with a real bus + pool).

// TestProcessEvent_DeclinedEvent_OffMode_MarksCancelled asserts that a
// declined event with a previously-stored row marks the row cancelled
// (off-mode deferral) rather than publishing or deleting.
func TestProcessEvent_DeclinedEvent_OffMode_MarksCancelled(t *testing.T) {
	ctx := context.Background()
	storedID := uuid.New()
	mockCalRepo := &mockCalendarRepo{
		getByGcalIDResult: &repository.CalendarEvent{
			ID:                storedID,
			GcalEventID:       "evt-declined",
			MatchedContactIDs: []uuid.UUID{uuid.New()},
			EndTime:           time.Date(2026, 4, 1, 11, 0, 0, 0, time.UTC),
		},
	}
	provider := newTestProvider(mockCalRepo, &mockContactRepo{}, &mockIdentityService{})

	event := &calendar.Event{
		Id:     "evt-declined",
		Status: "confirmed",
		Start:  &calendar.EventDateTime{DateTime: "2026-04-01T10:00:00Z"},
		End:    &calendar.EventDateTime{DateTime: "2026-04-01T11:00:00Z"},
		Attendees: []*calendar.EventAttendee{
			{Email: "user@example.com", Self: true, ResponseStatus: "declined"},
		},
	}

	require.NoError(t, provider.processEvent(ctx, event, "user@example.com"))
	require.True(t, mockCalRepo.getByGcalIDCalled, "remove branch looks up the stored row")
	require.True(t, mockCalRepo.markCancelledCalled, "off mode marks the stored row cancelled")
	require.False(t, mockCalRepo.deleteByGcalIDCalled, "off mode does not delete")
	require.False(t, mockCalRepo.upsertCalled, "declined event is not upserted")
}

// TestProcessEvent_DeclinedEvent_NeverStored_NoOp asserts that a declined
// event with no stored row is a clean no-op: no mark-cancelled, no delete,
// no upsert. We must NOT emit a decline for events never in the CRM.
func TestProcessEvent_DeclinedEvent_NeverStored_NoOp(t *testing.T) {
	ctx := context.Background()
	mockCalRepo := &mockCalendarRepo{getByGcalIDError: db.ErrNotFound}
	provider := newTestProvider(mockCalRepo, &mockContactRepo{}, &mockIdentityService{})

	event := &calendar.Event{
		Id:     "evt-never-stored",
		Status: "confirmed",
		Start:  &calendar.EventDateTime{DateTime: "2026-04-01T10:00:00Z"},
		End:    &calendar.EventDateTime{DateTime: "2026-04-01T11:00:00Z"},
		Attendees: []*calendar.EventAttendee{
			{Email: "user@example.com", Self: true, ResponseStatus: "declined"},
		},
	}

	require.NoError(t, provider.processEvent(ctx, event, "user@example.com"))
	require.True(t, mockCalRepo.getByGcalIDCalled)
	require.False(t, mockCalRepo.markCancelledCalled, "no-op when the event was never stored")
	require.False(t, mockCalRepo.deleteByGcalIDCalled)
	require.False(t, mockCalRepo.upsertCalled)
}

// TestProcessEvent_CancelledWithoutDateTime_ReachesRemoveBranch proves the
// gate-before-parse fix (Decision 1): a cancelled delta arriving WITHOUT
// Start/End DateTime still reaches the remove branch instead of erroring at
// the time parse. The organizer is the user (synthesized "accepted"), so it
// is the status='cancelled' clause that drives keep=false.
func TestProcessEvent_CancelledWithoutDateTime_ReachesRemoveBranch(t *testing.T) {
	ctx := context.Background()
	storedID := uuid.New()
	mockCalRepo := &mockCalendarRepo{
		getByGcalIDResult: &repository.CalendarEvent{
			ID:                storedID,
			GcalEventID:       "evt-cancelled",
			MatchedContactIDs: []uuid.UUID{uuid.New()},
			EndTime:           time.Date(2026, 4, 1, 11, 0, 0, 0, time.UTC),
		},
	}
	provider := newTestProvider(mockCalRepo, &mockContactRepo{}, &mockIdentityService{})

	// Cancelled stub: no Start/End DateTime (Google sends a minimal cancelled
	// delta). User is the organizer → getUserResponse synthesizes "accepted",
	// so keep=false comes from status="cancelled".
	event := &calendar.Event{
		Id:        "evt-cancelled",
		Status:    "cancelled",
		Start:     &calendar.EventDateTime{},
		End:       &calendar.EventDateTime{},
		Organizer: &calendar.EventOrganizer{Email: "user@example.com"},
	}

	require.NoError(t, provider.processEvent(ctx, event, "user@example.com"),
		"remove branch must run without parsed Start/End DateTime")
	require.True(t, mockCalRepo.getByGcalIDCalled, "remove branch reached despite missing DateTime")
	require.True(t, mockCalRepo.markCancelledCalled, "off mode marks the stored row cancelled")
	require.False(t, mockCalRepo.upsertCalled)
}

// TestProcessEvent_TentativeEvent_OffMode_MarksCancelled asserts the gate
// also fires for tentative (non-accepted) responses, exercising the
// userResponse != "accepted" clause.
func TestProcessEvent_TentativeEvent_OffMode_MarksCancelled(t *testing.T) {
	ctx := context.Background()
	mockCalRepo := &mockCalendarRepo{
		getByGcalIDResult: &repository.CalendarEvent{
			ID:                uuid.New(),
			GcalEventID:       "evt-tentative",
			MatchedContactIDs: []uuid.UUID{uuid.New()},
			EndTime:           time.Date(2026, 4, 1, 11, 0, 0, 0, time.UTC),
		},
	}
	provider := newTestProvider(mockCalRepo, &mockContactRepo{}, &mockIdentityService{})

	event := &calendar.Event{
		Id:     "evt-tentative",
		Status: "confirmed",
		Start:  &calendar.EventDateTime{DateTime: "2026-04-01T10:00:00Z"},
		End:    &calendar.EventDateTime{DateTime: "2026-04-01T11:00:00Z"},
		Attendees: []*calendar.EventAttendee{
			{Email: "user@example.com", Self: true, ResponseStatus: "tentative"},
		},
	}

	require.NoError(t, provider.processEvent(ctx, event, "user@example.com"))
	require.True(t, mockCalRepo.markCancelledCalled)
	require.False(t, mockCalRepo.upsertCalled)
}
