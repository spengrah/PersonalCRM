package google

import (
	"context"
	"time"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
)

// mockCalendarRepo is a mock implementation of calendar repository methods
type mockCalendarRepo struct {
	upsertCalled  bool
	upsertRequest *repository.UpsertCalendarEventRequest
	upsertError   error
	upsertResult  *repository.CalendarEvent

	listPastCalled bool
	listPastResult []repository.CalendarEvent
	listPastError  error

	markUpdatedCalled bool
	markUpdatedIDs    []uuid.UUID
	markUpdatedError  error
}

func (m *mockCalendarRepo) Upsert(ctx context.Context, req repository.UpsertCalendarEventRequest) (*repository.CalendarEvent, error) {
	m.upsertCalled = true
	m.upsertRequest = &req
	if m.upsertError != nil {
		return nil, m.upsertError
	}
	if m.upsertResult != nil {
		return m.upsertResult, nil
	}
	// Return a default result
	return &repository.CalendarEvent{
		ID:                uuid.New(),
		GcalEventID:       req.GcalEventID,
		GcalCalendarID:    req.GcalCalendarID,
		GoogleAccountID:   req.GoogleAccountID,
		Title:             req.Title,
		StartTime:         req.StartTime,
		EndTime:           req.EndTime,
		MatchedContactIDs: req.MatchedContactIDs,
	}, nil
}

func (m *mockCalendarRepo) ListPastEventsNeedingUpdate(ctx context.Context, before time.Time, limit int32) ([]repository.CalendarEvent, error) {
	m.listPastCalled = true
	if m.listPastError != nil {
		return nil, m.listPastError
	}
	return m.listPastResult, nil
}

func (m *mockCalendarRepo) MarkLastContactedUpdated(ctx context.Context, id uuid.UUID) error {
	m.markUpdatedCalled = true
	m.markUpdatedIDs = append(m.markUpdatedIDs, id)
	return m.markUpdatedError
}

// mockContactRepo is a mock implementation of contact repository methods
type mockContactRepo struct {
	updateLastContactedCalled bool
	updateLastContactedIDs    []uuid.UUID
	updateLastContactedTimes  []time.Time
	updateLastContactedError  error
}

func (m *mockContactRepo) UpdateContactLastContacted(ctx context.Context, id uuid.UUID, lastContacted time.Time) error {
	m.updateLastContactedCalled = true
	m.updateLastContactedIDs = append(m.updateLastContactedIDs, id)
	m.updateLastContactedTimes = append(m.updateLastContactedTimes, lastContacted)
	return m.updateLastContactedError
}

// mockIdentityService is a mock implementation of identity service methods
type mockIdentityService struct {
	matchOrCreateCalled   bool
	matchOrCreateRequests []service.MatchRequest
	matchOrCreateResults  map[string]*service.MatchResult // keyed by email
	matchOrCreateError    error
}

func (m *mockIdentityService) MatchOrCreate(ctx context.Context, req service.MatchRequest) (*service.MatchResult, error) {
	m.matchOrCreateCalled = true
	m.matchOrCreateRequests = append(m.matchOrCreateRequests, req)
	if m.matchOrCreateError != nil {
		return nil, m.matchOrCreateError
	}
	if m.matchOrCreateResults != nil {
		if result, ok := m.matchOrCreateResults[req.RawIdentifier]; ok {
			return result, nil
		}
	}
	// Return no match by default
	return nil, nil
}

// CalendarRepoInterface defines the interface for calendar repository operations used by tests
type CalendarRepoInterface interface {
	Upsert(ctx context.Context, req repository.UpsertCalendarEventRequest) (*repository.CalendarEvent, error)
	ListPastEventsNeedingUpdate(ctx context.Context, before time.Time, limit int32) ([]repository.CalendarEvent, error)
	MarkLastContactedUpdated(ctx context.Context, id uuid.UUID) error
}

// ContactRepoInterface defines the interface for contact repository operations used by tests
type ContactRepoInterface interface {
	UpdateContactLastContacted(ctx context.Context, id uuid.UUID, lastContacted time.Time) error
}

// IdentityServiceInterface defines the interface for identity service operations used by tests
type IdentityServiceInterface interface {
	MatchOrCreate(ctx context.Context, req service.MatchRequest) (*service.MatchResult, error)
}

// testableCalendarSyncProvider is a version of CalendarSyncProvider that uses interfaces for testing
type testableCalendarSyncProvider struct {
	calendarRepo    CalendarRepoInterface
	contactRepo     ContactRepoInterface
	identityService IdentityServiceInterface
}

func newTestableProvider(calRepo CalendarRepoInterface, contactRepo ContactRepoInterface, idSvc IdentityServiceInterface) *testableCalendarSyncProvider {
	return &testableCalendarSyncProvider{
		calendarRepo:    calRepo,
		contactRepo:     contactRepo,
		identityService: idSvc,
	}
}

// matchAttendees matches attendee emails to CRM contacts (testable version)
func (p *testableCalendarSyncProvider) matchAttendees(
	ctx context.Context,
	attendees []repository.Attendee,
	accountID string,
) []uuid.UUID {
	matchedIDs := make([]uuid.UUID, 0)
	seen := make(map[uuid.UUID]bool)

	for _, attendee := range attendees {
		// Skip self
		if attendee.Self {
			continue
		}

		// Skip empty emails
		if attendee.Email == "" {
			continue
		}

		// Use identity service to match
		displayName := attendee.DisplayName
		result, err := p.identityService.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier: attendee.Email,
			Type:          "email",
			Source:        CalendarSourceName,
			DisplayName:   &displayName,
		})

		if err != nil {
			continue
		}

		if result != nil && result.ContactID != nil {
			if !seen[*result.ContactID] {
				matchedIDs = append(matchedIDs, *result.ContactID)
				seen[*result.ContactID] = true
			}
		}
	}

	return matchedIDs
}

// updateLastContactedForPastEvents updates last_contacted for contacts in past events (testable version)
func (p *testableCalendarSyncProvider) updateLastContactedForPastEvents(ctx context.Context, now time.Time) error {
	// Fetch past events that need updating
	events, err := p.calendarRepo.ListPastEventsNeedingUpdate(ctx, now, 100)
	if err != nil {
		return err
	}

	for _, event := range events {
		// Update last_contacted for each matched contact
		for _, contactID := range event.MatchedContactIDs {
			if err := p.contactRepo.UpdateContactLastContacted(ctx, contactID, event.EndTime); err != nil {
				continue // Log and continue
			}
		}

		// Mark event as processed
		if err := p.calendarRepo.MarkLastContactedUpdated(ctx, event.ID); err != nil {
			continue // Log and continue
		}
	}

	return nil
}
