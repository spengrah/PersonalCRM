package google

import (
	"context"
	"time"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
)

// mockCalendarRepo is a mock implementation of calendarRepoInterface
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

// mockContactRepo is a mock implementation of contactRepoInterface
type mockContactRepo struct {
	updateLastContactedCalled bool
	updateLastContactedIDs    []uuid.UUID
	updateLastContactedTimes  []time.Time
	updateLastContactedError  error

	findSimilarCalled  bool
	findSimilarName    string
	findSimilarResults []repository.ContactMatch
	findSimilarError   error
}

func (m *mockContactRepo) UpdateContactLastContacted(ctx context.Context, id uuid.UUID, lastContacted time.Time) error {
	m.updateLastContactedCalled = true
	m.updateLastContactedIDs = append(m.updateLastContactedIDs, id)
	m.updateLastContactedTimes = append(m.updateLastContactedTimes, lastContacted)
	return m.updateLastContactedError
}

func (m *mockContactRepo) FindSimilarContacts(ctx context.Context, name string, threshold float64, limit int32) ([]repository.ContactMatch, error) {
	m.findSimilarCalled = true
	m.findSimilarName = name
	if m.findSimilarError != nil {
		return nil, m.findSimilarError
	}
	return m.findSimilarResults, nil
}

// mockIdentityService is a mock implementation of identityServiceInterface
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

// mockExternalContactRepo is a mock implementation of externalContactRepoInterface
type mockExternalContactRepo struct {
	upsertCalled   bool
	upsertRequests []repository.UpsertExternalContactRequest
	upsertError    error
	upsertResult   *repository.ExternalContact
}

func (m *mockExternalContactRepo) Upsert(ctx context.Context, req repository.UpsertExternalContactRequest) (*repository.ExternalContact, error) {
	m.upsertCalled = true
	m.upsertRequests = append(m.upsertRequests, req)
	if m.upsertError != nil {
		return nil, m.upsertError
	}
	if m.upsertResult != nil {
		return m.upsertResult, nil
	}
	// Return a default result
	return &repository.ExternalContact{
		ID:       uuid.New(),
		Source:   req.Source,
		SourceID: req.SourceID,
	}, nil
}

// newTestProvider creates a CalendarSyncProvider with mocked dependencies for testing
func newTestProvider(calRepo calendarRepoInterface, contactRepo contactRepoInterface, idSvc identityServiceInterface) *CalendarSyncProvider {
	return newTestProviderWithExternal(calRepo, contactRepo, idSvc, nil)
}

// newTestProviderWithExternal creates a CalendarSyncProvider with all mocked dependencies including external contact repo
func newTestProviderWithExternal(calRepo calendarRepoInterface, contactRepo contactRepoInterface, idSvc identityServiceInterface, extRepo externalContactRepoInterface) *CalendarSyncProvider {
	return &CalendarSyncProvider{
		oauthService:        nil, // Not needed for unit tests
		calendarRepo:        calRepo,
		contactRepo:         contactRepo,
		identityService:     idSvc,
		externalContactRepo: extRepo,
	}
}
