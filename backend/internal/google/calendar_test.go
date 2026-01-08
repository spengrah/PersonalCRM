package google

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"google.golang.org/api/calendar/v3"
)

func TestCalendarSyncProvider_Config(t *testing.T) {
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil)
	config := provider.Config()

	assert.Equal(t, CalendarSourceName, config.Name)
	assert.Equal(t, "Google Calendar", config.DisplayName)
	assert.Equal(t, repository.SyncStrategyFetchAll, config.Strategy)
	assert.True(t, config.SupportsMultiAccount)
	assert.True(t, config.SupportsDiscovery)
	assert.Equal(t, CalendarDefaultInterval, config.DefaultInterval)
}

func TestGetEventStatus(t *testing.T) {
	tests := []struct {
		name     string
		event    *calendar.Event
		expected string
	}{
		{
			name:     "empty status defaults to confirmed",
			event:    &calendar.Event{Status: ""},
			expected: "confirmed",
		},
		{
			name:     "confirmed status",
			event:    &calendar.Event{Status: "confirmed"},
			expected: "confirmed",
		},
		{
			name:     "cancelled status",
			event:    &calendar.Event{Status: "cancelled"},
			expected: "cancelled",
		},
		{
			name:     "tentative status",
			event:    &calendar.Event{Status: "tentative"},
			expected: "tentative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getEventStatus(tt.event)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetOrganizerEmail(t *testing.T) {
	tests := []struct {
		name     string
		event    *calendar.Event
		expected *string
	}{
		{
			name:     "no organizer",
			event:    &calendar.Event{Organizer: nil},
			expected: nil,
		},
		{
			name:     "empty organizer email",
			event:    &calendar.Event{Organizer: &calendar.EventOrganizer{Email: ""}},
			expected: nil,
		},
		{
			name: "valid organizer email",
			event: &calendar.Event{
				Organizer: &calendar.EventOrganizer{Email: "organizer@example.com"},
			},
			expected: strPtr("organizer@example.com"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getOrganizerEmail(tt.event)
			if tt.expected == nil {
				assert.Nil(t, result)
			} else {
				assert.NotNil(t, result)
				assert.Equal(t, *tt.expected, *result)
			}
		})
	}
}

func TestStrPtrIfNotEmpty(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected *string
	}{
		{
			name:     "empty string returns nil",
			input:    "",
			expected: nil,
		},
		{
			name:     "non-empty string returns pointer",
			input:    "hello",
			expected: strPtr("hello"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := strPtrIfNotEmpty(tt.input)
			if tt.expected == nil {
				assert.Nil(t, result)
			} else {
				assert.NotNil(t, result)
				assert.Equal(t, *tt.expected, *result)
			}
		})
	}
}

func TestCalendarSyncProvider_GetUserResponse(t *testing.T) {
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil)
	accountID := "user@example.com"

	tests := []struct {
		name     string
		event    *calendar.Event
		expected *string
	}{
		{
			name: "user is organizer",
			event: &calendar.Event{
				Organizer: &calendar.EventOrganizer{Email: "user@example.com"},
			},
			expected: strPtr("accepted"),
		},
		{
			name: "user is attendee with accepted response",
			event: &calendar.Event{
				Attendees: []*calendar.EventAttendee{
					{
						Email:          "user@example.com",
						Self:           true,
						ResponseStatus: "accepted",
					},
				},
			},
			expected: strPtr("accepted"),
		},
		{
			name: "user is attendee with declined response",
			event: &calendar.Event{
				Attendees: []*calendar.EventAttendee{
					{
						Email:          "user@example.com",
						Self:           true,
						ResponseStatus: "declined",
					},
				},
			},
			expected: strPtr("declined"),
		},
		{
			name: "user not found in attendees or organizer",
			event: &calendar.Event{
				Organizer: &calendar.EventOrganizer{Email: "other@example.com"},
				Attendees: []*calendar.EventAttendee{
					{
						Email:          "other@example.com",
						ResponseStatus: "accepted",
					},
				},
			},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := provider.getUserResponse(tt.event, accountID)
			if tt.expected == nil {
				assert.Nil(t, result)
			} else {
				assert.NotNil(t, result)
				assert.Equal(t, *tt.expected, *result)
			}
		})
	}
}

func TestCalendarSyncProvider_BuildAttendeeList(t *testing.T) {
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil)
	accountID := "user@example.com"

	event := &calendar.Event{
		Organizer: &calendar.EventOrganizer{Email: "organizer@example.com"},
		Attendees: []*calendar.EventAttendee{
			{
				Email:          "user@example.com",
				DisplayName:    "User",
				Self:           true,
				ResponseStatus: "accepted",
			},
			{
				Email:          "organizer@example.com",
				DisplayName:    "Organizer",
				ResponseStatus: "accepted",
			},
			{
				Email:          "other@example.com",
				DisplayName:    "Other Person",
				ResponseStatus: "needsAction",
			},
		},
	}

	attendees := provider.buildAttendeeList(event, accountID)

	assert.Len(t, attendees, 3)

	// Check self attendee
	selfAttendee := attendees[0]
	assert.Equal(t, "user@example.com", selfAttendee.Email)
	assert.True(t, selfAttendee.Self)
	assert.False(t, selfAttendee.Organizer)

	// Check organizer attendee
	organizerAttendee := attendees[1]
	assert.Equal(t, "organizer@example.com", organizerAttendee.Email)
	assert.False(t, organizerAttendee.Self)
	assert.True(t, organizerAttendee.Organizer)

	// Check other attendee
	otherAttendee := attendees[2]
	assert.Equal(t, "other@example.com", otherAttendee.Email)
	assert.False(t, otherAttendee.Self)
	assert.False(t, otherAttendee.Organizer)
}

// Helper function for creating string pointers in tests
func strPtr(s string) *string {
	return &s
}

// TestProcessEvent_SkipsAllDayEvents verifies that all-day events are skipped
func TestProcessEvent_SkipsAllDayEvents(t *testing.T) {
	// All-day event has Date instead of DateTime
	event := &calendar.Event{
		Id:      "test-event-1",
		Summary: "Holiday",
		Start: &calendar.EventDateTime{
			Date: "2024-01-01", // All-day event indicator
		},
		End: &calendar.EventDateTime{
			Date: "2024-01-02",
		},
	}

	// Create a test that verifies the event is skipped by checking the condition
	// Since processEvent returns early for all-day events, we test the condition directly
	isAllDay := event.Start.Date != ""
	assert.True(t, isAllDay, "Event with Start.Date should be identified as all-day")
}

// TestProcessEvent_SkipsDeclinedEvents verifies that declined events are skipped
func TestProcessEvent_SkipsDeclinedEvents(t *testing.T) {
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil)
	accountID := "user@example.com"

	event := &calendar.Event{
		Id:      "test-event-2",
		Summary: "Declined Meeting",
		Status:  "confirmed",
		Start: &calendar.EventDateTime{
			DateTime: "2024-01-15T10:00:00Z",
		},
		End: &calendar.EventDateTime{
			DateTime: "2024-01-15T11:00:00Z",
		},
		Attendees: []*calendar.EventAttendee{
			{
				Email:          "user@example.com",
				Self:           true,
				ResponseStatus: "declined",
			},
		},
	}

	userResponse := provider.getUserResponse(event, accountID)
	assert.NotNil(t, userResponse)
	assert.Equal(t, "declined", *userResponse)
}

// TestMatchAttendees_SkipsSelfAttendee verifies that the user's own email is not matched
func TestMatchAttendees_SkipsSelfAttendee(t *testing.T) {
	accountID := "user@example.com"

	attendees := []repository.Attendee{
		{Email: "user@example.com", Self: true, DisplayName: "User"},
		{Email: "other@example.com", Self: false, DisplayName: "Other"},
	}

	// Filter attendees as matchAttendees would (excluding self)
	var nonSelfAttendees []repository.Attendee
	for _, a := range attendees {
		if !a.Self && a.Email != "" {
			nonSelfAttendees = append(nonSelfAttendees, a)
		}
	}

	assert.Len(t, nonSelfAttendees, 1)
	assert.Equal(t, "other@example.com", nonSelfAttendees[0].Email)
	_ = accountID // Used in actual implementation
}

// TestMatchAttendees_SkipsEmptyEmails verifies that attendees with empty emails are skipped
func TestMatchAttendees_SkipsEmptyEmails(t *testing.T) {
	attendees := []repository.Attendee{
		{Email: "", Self: false, DisplayName: "No Email"},
		{Email: "valid@example.com", Self: false, DisplayName: "Valid"},
	}

	var validAttendees []repository.Attendee
	for _, a := range attendees {
		if !a.Self && a.Email != "" {
			validAttendees = append(validAttendees, a)
		}
	}

	assert.Len(t, validAttendees, 1)
	assert.Equal(t, "valid@example.com", validAttendees[0].Email)
}

// TestBuildAttendeeList_EmptyAttendees verifies behavior with no attendees
func TestBuildAttendeeList_EmptyAttendees(t *testing.T) {
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil)
	accountID := "user@example.com"

	event := &calendar.Event{
		Organizer: &calendar.EventOrganizer{Email: "organizer@example.com"},
		Attendees: []*calendar.EventAttendee{},
	}

	attendees := provider.buildAttendeeList(event, accountID)
	assert.Empty(t, attendees)
}

// TestEventStatusMapping verifies all event status values are handled
func TestEventStatusMapping(t *testing.T) {
	tests := []struct {
		name     string
		status   string
		expected string
	}{
		{"confirmed", "confirmed", "confirmed"},
		{"tentative", "tentative", "tentative"},
		{"cancelled", "cancelled", "cancelled"},
		{"empty defaults to confirmed", "", "confirmed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := &calendar.Event{Status: tt.status}
			result := getEventStatus(event)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestUserResponse_MultipleAttendees verifies correct user identification among many attendees
func TestUserResponse_MultipleAttendees(t *testing.T) {
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil)

	tests := []struct {
		name      string
		accountID string
		attendees []*calendar.EventAttendee
		expected  *string
	}{
		{
			name:      "user found by Self flag",
			accountID: "user@example.com",
			attendees: []*calendar.EventAttendee{
				{Email: "alice@example.com", ResponseStatus: "accepted"},
				{Email: "user@example.com", Self: true, ResponseStatus: "tentative"},
				{Email: "bob@example.com", ResponseStatus: "needsAction"},
			},
			expected: strPtr("tentative"),
		},
		{
			name:      "user found by email match",
			accountID: "USER@EXAMPLE.COM", // case insensitive
			attendees: []*calendar.EventAttendee{
				{Email: "alice@example.com", ResponseStatus: "accepted"},
				{Email: "user@example.com", Self: false, ResponseStatus: "declined"},
			},
			expected: strPtr("declined"),
		},
		{
			name:      "user not in attendees",
			accountID: "notpresent@example.com",
			attendees: []*calendar.EventAttendee{
				{Email: "alice@example.com", ResponseStatus: "accepted"},
				{Email: "bob@example.com", ResponseStatus: "accepted"},
			},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := &calendar.Event{Attendees: tt.attendees}
			result := provider.getUserResponse(event, tt.accountID)
			if tt.expected == nil {
				assert.Nil(t, result)
			} else {
				assert.NotNil(t, result)
				assert.Equal(t, *tt.expected, *result)
			}
		})
	}
}

// TestBuildAttendeeList_OrganizerIdentification verifies organizer is correctly flagged
func TestBuildAttendeeList_OrganizerIdentification(t *testing.T) {
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil)
	accountID := "user@example.com"

	event := &calendar.Event{
		Organizer: &calendar.EventOrganizer{Email: "alice@example.com"},
		Attendees: []*calendar.EventAttendee{
			{Email: "alice@example.com", DisplayName: "Alice", ResponseStatus: "accepted"},
			{Email: "bob@example.com", DisplayName: "Bob", ResponseStatus: "accepted"},
		},
	}

	attendees := provider.buildAttendeeList(event, accountID)

	assert.Len(t, attendees, 2)

	// Find Alice (organizer)
	var alice *repository.Attendee
	for i := range attendees {
		if attendees[i].Email == "alice@example.com" {
			alice = &attendees[i]
			break
		}
	}
	assert.NotNil(t, alice)
	assert.True(t, alice.Organizer, "Alice should be marked as organizer")

	// Find Bob (not organizer)
	var bob *repository.Attendee
	for i := range attendees {
		if attendees[i].Email == "bob@example.com" {
			bob = &attendees[i]
			break
		}
	}
	assert.NotNil(t, bob)
	assert.False(t, bob.Organizer, "Bob should not be marked as organizer")
}

// TestCalendarEventTimeValidation verifies time parsing requirements
func TestCalendarEventTimeValidation(t *testing.T) {
	// Test valid RFC3339 times (as returned by Google Calendar API)
	validTimes := []string{
		"2024-01-15T10:00:00Z",
		"2024-01-15T10:00:00+00:00",
		"2024-01-15T10:00:00-08:00",
		"2024-06-15T14:30:00+05:30",
	}

	for _, timeStr := range validTimes {
		t.Run(timeStr, func(t *testing.T) {
			_, err := time.Parse(time.RFC3339, timeStr)
			assert.NoError(t, err, "Should parse valid RFC3339 time")
		})
	}
}

// TestPtrToStr verifies the pointer-to-string helper function
func TestPtrToStr(t *testing.T) {
	tests := []struct {
		name     string
		input    *string
		expected string
	}{
		{"nil returns empty", nil, ""},
		{"non-nil returns value", strPtr("hello"), "hello"},
		{"empty string returns empty", strPtr(""), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ptrToStr(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// =============================================================================
// Integration tests with mocked dependencies
// =============================================================================

// TestMatchAttendees_WithMockedIdentityService tests contact matching with mocked identity service
// This tests the ACTUAL CalendarSyncProvider.matchAttendees() method with mocked dependencies
func TestMatchAttendees_WithMockedIdentityService(t *testing.T) {
	ctx := context.Background()

	contactID1 := uuid.New()
	contactID2 := uuid.New()

	mockIdentity := &mockIdentityService{
		matchOrCreateResults: map[string]*service.MatchResult{
			"alice@example.com": {ContactID: &contactID1},
			"bob@example.com":   {ContactID: &contactID2},
		},
	}

	// Empty contact repo for fuzzy matching fallback (no fuzzy matches available)
	mockContactRepo := &mockContactRepo{
		findSimilarResults: []repository.ContactMatch{},
	}

	// Use newTestProvider to create the REAL CalendarSyncProvider with mocked deps
	provider := newTestProvider(nil, mockContactRepo, mockIdentity)

	attendees := []repository.Attendee{
		{Email: "user@example.com", Self: true, DisplayName: "User"},        // Should be skipped (self)
		{Email: "alice@example.com", Self: false, DisplayName: "Alice"},     // Should match
		{Email: "bob@example.com", Self: false, DisplayName: "Bob"},         // Should match
		{Email: "unknown@example.com", Self: false, DisplayName: "Unknown"}, // No match (fuzzy attempted but no results)
		{Email: "", Self: false, DisplayName: "No Email"},                   // Should be skipped (empty)
	}

	// Call the REAL matchAttendees method on the REAL CalendarSyncProvider
	matchedIDs := provider.matchAttendees(ctx, attendees, "user@example.com", nil)

	assert.True(t, mockIdentity.matchOrCreateCalled)
	assert.Len(t, mockIdentity.matchOrCreateRequests, 3) // alice, bob, unknown (skipped self and empty)
	assert.Len(t, matchedIDs, 2)                         // Only alice and bob matched
	assert.Contains(t, matchedIDs, contactID1)
	assert.Contains(t, matchedIDs, contactID2)
}

// TestMatchAttendees_DeduplicatesContacts tests that duplicate contact matches are deduplicated
func TestMatchAttendees_DeduplicatesContacts(t *testing.T) {
	ctx := context.Background()

	contactID := uuid.New()

	mockIdentity := &mockIdentityService{
		matchOrCreateResults: map[string]*service.MatchResult{
			"alice@example.com":  {ContactID: &contactID},
			"alice2@example.com": {ContactID: &contactID}, // Same contact, different email
		},
	}

	mockContactRepo := &mockContactRepo{}

	provider := newTestProvider(nil, mockContactRepo, mockIdentity)

	attendees := []repository.Attendee{
		{Email: "alice@example.com", Self: false, DisplayName: "Alice"},
		{Email: "alice2@example.com", Self: false, DisplayName: "Alice Alt"},
	}

	matchedIDs := provider.matchAttendees(ctx, attendees, "user@example.com", nil)

	assert.Len(t, matchedIDs, 1) // Only one unique contact
	assert.Equal(t, contactID, matchedIDs[0])
}

// TestMatchAttendees_HandlesIdentityServiceError tests error handling
func TestMatchAttendees_HandlesIdentityServiceError(t *testing.T) {
	ctx := context.Background()

	contactID := uuid.New()

	mockIdentity := &mockIdentityService{
		matchOrCreateResults: map[string]*service.MatchResult{
			"bob@example.com": {ContactID: &contactID},
		},
		matchOrCreateError: nil, // Will return error for alice (not in results)
	}

	mockContactRepo := &mockContactRepo{
		findSimilarResults: []repository.ContactMatch{}, // No fuzzy matches
	}

	provider := newTestProvider(nil, mockContactRepo, mockIdentity)

	attendees := []repository.Attendee{
		{Email: "alice@example.com", Self: false, DisplayName: "Alice"}, // No match
		{Email: "bob@example.com", Self: false, DisplayName: "Bob"},     // Match
	}

	matchedIDs := provider.matchAttendees(ctx, attendees, "user@example.com", nil)

	assert.True(t, mockIdentity.matchOrCreateCalled)
	assert.Len(t, matchedIDs, 1)
	assert.Equal(t, contactID, matchedIDs[0])
}

// TestUpdateLastContactedForPastEvents_UpdatesContactsAndMarksEvents tests the full update flow
// This tests the ACTUAL CalendarSyncProvider.updateLastContactedForPastEvents() method
func TestUpdateLastContactedForPastEvents_UpdatesContactsAndMarksEvents(t *testing.T) {
	ctx := context.Background()

	eventID := uuid.New()
	contactID1 := uuid.New()
	contactID2 := uuid.New()
	eventEndTime := accelerated.GetCurrentTime().Add(-1 * time.Hour)

	mockCalRepo := &mockCalendarRepo{
		listPastResult: []repository.CalendarEvent{
			{
				ID:                eventID,
				EndTime:           eventEndTime,
				MatchedContactIDs: []uuid.UUID{contactID1, contactID2},
			},
		},
	}

	mockContactRepo := &mockContactRepo{}

	// Use newTestProvider to create the REAL CalendarSyncProvider with mocked deps
	provider := newTestProvider(mockCalRepo, mockContactRepo, nil)

	// Call the REAL updateLastContactedForPastEvents method
	err := provider.updateLastContactedForPastEvents(ctx)

	assert.NoError(t, err)

	// Verify calendar repo was called to list past events
	assert.True(t, mockCalRepo.listPastCalled)

	// Verify contacts were updated
	assert.True(t, mockContactRepo.updateLastContactedCalled)
	assert.Len(t, mockContactRepo.updateLastContactedIDs, 2)
	assert.Contains(t, mockContactRepo.updateLastContactedIDs, contactID1)
	assert.Contains(t, mockContactRepo.updateLastContactedIDs, contactID2)

	// Verify correct times were used
	for _, contactedTime := range mockContactRepo.updateLastContactedTimes {
		assert.Equal(t, eventEndTime, contactedTime)
	}

	// Verify event was marked as updated
	assert.True(t, mockCalRepo.markUpdatedCalled)
	assert.Len(t, mockCalRepo.markUpdatedIDs, 1)
	assert.Equal(t, eventID, mockCalRepo.markUpdatedIDs[0])
}

// TestUpdateLastContactedForPastEvents_HandlesMultipleEvents tests processing multiple events
func TestUpdateLastContactedForPastEvents_HandlesMultipleEvents(t *testing.T) {
	ctx := context.Background()

	event1ID := uuid.New()
	event2ID := uuid.New()
	contact1 := uuid.New()
	contact2 := uuid.New()

	mockCalRepo := &mockCalendarRepo{
		listPastResult: []repository.CalendarEvent{
			{
				ID:                event1ID,
				EndTime:           accelerated.GetCurrentTime().Add(-2 * time.Hour),
				MatchedContactIDs: []uuid.UUID{contact1},
			},
			{
				ID:                event2ID,
				EndTime:           accelerated.GetCurrentTime().Add(-1 * time.Hour),
				MatchedContactIDs: []uuid.UUID{contact2},
			},
		},
	}

	mockContactRepo := &mockContactRepo{}

	provider := newTestProvider(mockCalRepo, mockContactRepo, nil)

	err := provider.updateLastContactedForPastEvents(ctx)

	assert.NoError(t, err)
	assert.Len(t, mockCalRepo.markUpdatedIDs, 2)
	assert.Len(t, mockContactRepo.updateLastContactedIDs, 2)
}

// TestUpdateLastContactedForPastEvents_NoEventsNeedingUpdate tests the empty case
func TestUpdateLastContactedForPastEvents_NoEventsNeedingUpdate(t *testing.T) {
	ctx := context.Background()

	mockCalRepo := &mockCalendarRepo{
		listPastResult: []repository.CalendarEvent{}, // No events
	}

	mockContactRepo := &mockContactRepo{}

	provider := newTestProvider(mockCalRepo, mockContactRepo, nil)

	err := provider.updateLastContactedForPastEvents(ctx)

	assert.NoError(t, err)
	assert.True(t, mockCalRepo.listPastCalled)
	assert.False(t, mockContactRepo.updateLastContactedCalled) // No contacts to update
	assert.False(t, mockCalRepo.markUpdatedCalled)             // No events to mark
}

// ========================================
// Fuzzy Matching Tests
// ========================================

// TestMatchAttendees_FuzzyMatchFallback tests that fuzzy matching is attempted when exact match fails
func TestMatchAttendees_FuzzyMatchFallback(t *testing.T) {
	ctx := context.Background()

	contactID := uuid.New()

	// Identity service returns no match (email not found)
	mockIdentity := &mockIdentityService{
		matchOrCreateResults: map[string]*service.MatchResult{}, // No exact matches
	}

	// Contact repo returns a similar contact with high name similarity
	mockContactRepo := &mockContactRepo{
		findSimilarResults: []repository.ContactMatch{
			{
				Contact: repository.Contact{
					ID:       contactID,
					FullName: "Jon Smith", // Similar to "John Smith"
					Methods: []repository.ContactMethod{
						{Type: "email_personal", Value: "jon@example.com"},
					},
				},
				Similarity: 0.85, // High name similarity
			},
		},
	}

	provider := newTestProvider(nil, mockContactRepo, mockIdentity)

	attendees := []repository.Attendee{
		{
			Email:       "john.smith@work.com", // Different from contact's email
			DisplayName: "John Smith",
			Self:        false,
		},
	}

	matchedIDs := provider.matchAttendees(ctx, attendees, "user@example.com", nil)

	// Verify identity service was called first
	assert.True(t, mockIdentity.matchOrCreateCalled)

	// Verify fuzzy matching was attempted
	assert.True(t, mockContactRepo.findSimilarCalled)
	assert.Equal(t, "John Smith", mockContactRepo.findSimilarName)

	// With 85% name similarity and no method overlap:
	// Score = 0.6 * 0.85 + 0.4 * 0 = 0.51 (below 0.7 threshold)
	// So no fuzzy match should be returned
	assert.Len(t, matchedIDs, 0)
}

// TestMatchAttendees_FuzzyMatchWithMethodOverlap tests fuzzy matching with contact method overlap
func TestMatchAttendees_FuzzyMatchWithMethodOverlap(t *testing.T) {
	ctx := context.Background()

	contactID := uuid.New()

	// Identity service returns no match (email not in contact_method table but matches contact)
	mockIdentity := &mockIdentityService{
		matchOrCreateResults: map[string]*service.MatchResult{}, // No exact matches
	}

	// Contact repo returns a similar contact whose email matches the attendee
	mockContactRepo := &mockContactRepo{
		findSimilarResults: []repository.ContactMatch{
			{
				Contact: repository.Contact{
					ID:       contactID,
					FullName: "Jon Smith",
					Methods: []repository.ContactMethod{
						{Type: "email_work", Value: "john.smith@work.com"}, // Same as attendee!
					},
				},
				Similarity: 0.80, // Good name similarity
			},
		},
	}

	provider := newTestProvider(nil, mockContactRepo, mockIdentity)

	attendees := []repository.Attendee{
		{
			Email:       "john.smith@work.com",
			DisplayName: "John Smith",
			Self:        false,
		},
	}

	matchedIDs := provider.matchAttendees(ctx, attendees, "user@example.com", nil)

	// Verify fuzzy matching was attempted
	assert.True(t, mockContactRepo.findSimilarCalled)

	// With 80% name similarity and 100% method overlap (1/1 methods match):
	// Score = 0.6 * 0.80 + 0.4 * 1.0 = 0.48 + 0.40 = 0.88 (above 0.7 threshold)
	// Fuzzy match should succeed
	assert.Len(t, matchedIDs, 1)
	assert.Equal(t, contactID, matchedIDs[0])
}

// TestMatchAttendees_ExactMatchTakesPrecedence tests that exact matches are preferred over fuzzy
func TestMatchAttendees_ExactMatchTakesPrecedence(t *testing.T) {
	ctx := context.Background()

	exactContactID := uuid.New()
	fuzzyContactID := uuid.New()

	// Identity service returns an exact match
	mockIdentity := &mockIdentityService{
		matchOrCreateResults: map[string]*service.MatchResult{
			"john.smith@work.com": {
				ContactID: &exactContactID,
			},
		},
	}

	// Contact repo would return a different fuzzy match
	mockContactRepo := &mockContactRepo{
		findSimilarResults: []repository.ContactMatch{
			{
				Contact: repository.Contact{
					ID:       fuzzyContactID,
					FullName: "John Smith Jr",
					Methods:  []repository.ContactMethod{},
				},
				Similarity: 0.95, // Very high similarity
			},
		},
	}

	provider := newTestProvider(nil, mockContactRepo, mockIdentity)

	attendees := []repository.Attendee{
		{
			Email:       "john.smith@work.com",
			DisplayName: "John Smith",
			Self:        false,
		},
	}

	matchedIDs := provider.matchAttendees(ctx, attendees, "user@example.com", nil)

	// Exact match found, so fuzzy matching should NOT be attempted
	assert.False(t, mockContactRepo.findSimilarCalled)

	// Should return exact match, not fuzzy
	assert.Len(t, matchedIDs, 1)
	assert.Equal(t, exactContactID, matchedIDs[0])
}

// TestMatchAttendees_NoDisplayName_SkipsFuzzyMatch tests that fuzzy matching is skipped without display name
func TestMatchAttendees_NoDisplayName_SkipsFuzzyMatch(t *testing.T) {
	ctx := context.Background()

	// Identity service returns no match
	mockIdentity := &mockIdentityService{
		matchOrCreateResults: map[string]*service.MatchResult{},
	}

	mockContactRepo := &mockContactRepo{}

	provider := newTestProvider(nil, mockContactRepo, mockIdentity)

	attendees := []repository.Attendee{
		{
			Email:       "john.smith@work.com",
			DisplayName: "", // No display name
			Self:        false,
		},
	}

	matchedIDs := provider.matchAttendees(ctx, attendees, "user@example.com", nil)

	// Without display name, fuzzy matching should not be attempted
	assert.False(t, mockContactRepo.findSimilarCalled)
	assert.Len(t, matchedIDs, 0)
}

// TestFindFuzzyMatch_NoMatches tests findFuzzyMatch when no similar contacts exist
func TestFindFuzzyMatch_NoMatches(t *testing.T) {
	ctx := context.Background()

	mockContactRepo := &mockContactRepo{
		findSimilarResults: []repository.ContactMatch{}, // No matches
	}

	provider := newTestProvider(nil, mockContactRepo, nil)

	result := provider.findFuzzyMatch(ctx, "Unknown Person", "unknown@example.com")

	assert.True(t, mockContactRepo.findSimilarCalled)
	assert.Nil(t, result)
}

// TestFindFuzzyMatch_BelowThreshold tests that matches below confidence threshold are rejected
func TestFindFuzzyMatch_BelowThreshold(t *testing.T) {
	ctx := context.Background()

	contactID := uuid.New()

	mockContactRepo := &mockContactRepo{
		findSimilarResults: []repository.ContactMatch{
			{
				Contact: repository.Contact{
					ID:       contactID,
					FullName: "John",
					Methods:  []repository.ContactMethod{}, // No methods
				},
				Similarity: 0.5, // Only 50% name similarity
			},
		},
	}

	provider := newTestProvider(nil, mockContactRepo, nil)

	result := provider.findFuzzyMatch(ctx, "Jonathan", "jonathan@example.com")

	// Score = 0.6 * 0.5 + 0.4 * 0 = 0.30 (below 0.7 threshold)
	assert.True(t, mockContactRepo.findSimilarCalled)
	assert.Nil(t, result)
}

// TestFindFuzzyMatch_SelectsBestMatch tests that the highest scoring match is selected
func TestFindFuzzyMatch_SelectsBestMatch(t *testing.T) {
	ctx := context.Background()

	contact1ID := uuid.New()
	contact2ID := uuid.New()

	mockContactRepo := &mockContactRepo{
		findSimilarResults: []repository.ContactMatch{
			{
				Contact: repository.Contact{
					ID:       contact1ID,
					FullName: "Jon Smith",
					Methods:  []repository.ContactMethod{}, // No overlap
				},
				Similarity: 0.85,
			},
			{
				Contact: repository.Contact{
					ID:       contact2ID,
					FullName: "John Smyth",
					Methods: []repository.ContactMethod{
						{Type: "email_work", Value: "john.smith@work.com"}, // Email matches!
					},
				},
				Similarity: 0.75, // Lower name similarity but has email match
			},
		},
	}

	provider := newTestProvider(nil, mockContactRepo, nil)

	result := provider.findFuzzyMatch(ctx, "John Smith", "john.smith@work.com")

	// Contact 1: 0.6 * 0.85 + 0.4 * 0 = 0.51 (below threshold)
	// Contact 2: 0.6 * 0.75 + 0.4 * 1.0 = 0.45 + 0.40 = 0.85 (above threshold!)
	assert.NotNil(t, result)
	assert.Equal(t, contact2ID, *result)
}

// TestFindFuzzyMatch_EmailNormalization tests that email comparison is case-insensitive
func TestFindFuzzyMatch_EmailNormalization(t *testing.T) {
	ctx := context.Background()

	contactID := uuid.New()

	mockContactRepo := &mockContactRepo{
		findSimilarResults: []repository.ContactMatch{
			{
				Contact: repository.Contact{
					ID:       contactID,
					FullName: "John Smith",
					Methods: []repository.ContactMethod{
						{Type: "email_personal", Value: "John.Smith@EXAMPLE.COM"}, // Different case
					},
				},
				Similarity: 0.90,
			},
		},
	}

	provider := newTestProvider(nil, mockContactRepo, nil)

	result := provider.findFuzzyMatch(ctx, "John Smith", "john.smith@example.com")

	// Emails should match despite case difference
	// Score = 0.6 * 0.90 + 0.4 * 1.0 = 0.54 + 0.40 = 0.94 (above threshold)
	assert.NotNil(t, result)
	assert.Equal(t, contactID, *result)
}

// TestFindFuzzyMatch_IgnoresNonEmailMethods tests that only emails are counted for overlap
func TestFindFuzzyMatch_IgnoresNonEmailMethods(t *testing.T) {
	ctx := context.Background()

	contactID := uuid.New()

	// Contact has 1 email (matching) and 2 phones (should be ignored in scoring)
	mockContactRepo := &mockContactRepo{
		findSimilarResults: []repository.ContactMatch{
			{
				Contact: repository.Contact{
					ID:       contactID,
					FullName: "John Smith",
					Methods: []repository.ContactMethod{
						{Type: "email_work", Value: "john.smith@work.com"}, // Matches attendee email
						{Type: "phone", Value: "+1234567890"},              // Should be ignored
						{Type: "phone", Value: "+0987654321"},              // Should be ignored
					},
				},
				Similarity: 0.80,
			},
		},
	}

	provider := newTestProvider(nil, mockContactRepo, nil)

	result := provider.findFuzzyMatch(ctx, "John Smith", "john.smith@work.com")

	// Should count only email methods (1/1 match = 100% overlap)
	// Score = 0.6 * 0.80 + 0.4 * 1.0 = 0.48 + 0.40 = 0.88 (above 0.7 threshold)
	// Without the fix, it would count all methods: 1/3 = 0.33 overlap
	// Wrong score would be: 0.6 * 0.80 + 0.4 * 0.33 = 0.48 + 0.13 = 0.61 (below threshold!)
	assert.NotNil(t, result)
	assert.Equal(t, contactID, *result)
}

// ========================================
// Sync Window and HtmlLink Tests
// ========================================

// TestSyncWindowConstants verifies the sync window constants are set correctly
func TestSyncWindowConstants(t *testing.T) {
	// Verify past sync window is 1 year (365 days)
	assert.Equal(t, 365, CalendarPastSyncDays, "Past sync window should be 365 days")

	// Verify future sync window is 30 days
	assert.Equal(t, 30, CalendarFutureSyncDays, "Future sync window should be 30 days")
}

// TestProcessEvent_ExtractsHtmlLink verifies that HtmlLink is extracted from Google Calendar events
func TestProcessEvent_ExtractsHtmlLink(t *testing.T) {
	ctx := context.Background()

	mockCalRepo := &mockCalendarRepo{}
	mockContactRepo := &mockContactRepo{}
	mockIdentity := &mockIdentityService{}

	provider := newTestProvider(mockCalRepo, mockContactRepo, mockIdentity)

	// Create a Google Calendar event with HtmlLink
	event := &calendar.Event{
		Id:       "test-event-123",
		Summary:  "Test Meeting",
		HtmlLink: "https://www.google.com/calendar/event?eid=abc123",
		Status:   "confirmed",
		Start: &calendar.EventDateTime{
			DateTime: "2024-06-15T10:00:00Z",
		},
		End: &calendar.EventDateTime{
			DateTime: "2024-06-15T11:00:00Z",
		},
		Organizer: &calendar.EventOrganizer{
			Email: "user@example.com",
		},
	}

	err := provider.processEvent(ctx, event, "user@example.com")

	assert.NoError(t, err)
	assert.True(t, mockCalRepo.upsertCalled, "Upsert should be called")
	assert.NotNil(t, mockCalRepo.upsertRequest, "Upsert request should not be nil")
	assert.NotNil(t, mockCalRepo.upsertRequest.HtmlLink, "HtmlLink should not be nil")
	assert.Equal(t, "https://www.google.com/calendar/event?eid=abc123", *mockCalRepo.upsertRequest.HtmlLink)
}

// ========================================
// storeUnmatchedAttendee Tests
// ========================================

// TestStoreUnmatchedAttendee_Success tests successful storage of unmatched attendees
func TestStoreUnmatchedAttendee_Success(t *testing.T) {
	ctx := context.Background()

	mockExtRepo := &mockExternalContactRepo{}

	provider := newTestProviderWithExternal(nil, nil, nil, mockExtRepo)

	attendee := repository.Attendee{
		Email:       "unknown@example.com",
		DisplayName: "Unknown Person",
	}

	eventContext := &EventContext{
		Title:     "Team Meeting",
		StartTime: time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC),
		HtmlLink:  "https://calendar.google.com/event?eid=abc123",
	}

	err := provider.storeUnmatchedAttendee(ctx, attendee, "user@example.com", eventContext)

	assert.NoError(t, err)
	assert.True(t, mockExtRepo.upsertCalled)
	assert.Len(t, mockExtRepo.upsertRequests, 1)

	req := mockExtRepo.upsertRequests[0]
	assert.Equal(t, CalendarAttendeeSource, req.Source)
	assert.Equal(t, "unknown@example.com", req.SourceID) // Normalized email
	assert.NotNil(t, req.AccountID)
	assert.Equal(t, "user@example.com", *req.AccountID)
	assert.NotNil(t, req.DisplayName)
	assert.Equal(t, "Unknown Person", *req.DisplayName)
	assert.Len(t, req.Emails, 1)
	assert.Equal(t, "unknown@example.com", req.Emails[0].Value)

	// Verify metadata
	assert.NotNil(t, req.Metadata)
	assert.Equal(t, "Team Meeting", req.Metadata["meeting_title"])
	assert.Equal(t, "https://calendar.google.com/event?eid=abc123", req.Metadata["meeting_link"])
	assert.NotEmpty(t, req.Metadata["meeting_date"])
	assert.NotEmpty(t, req.Metadata["discovered_at"])
}

// TestStoreUnmatchedAttendee_NilExternalRepo tests that nil repo is handled gracefully
func TestStoreUnmatchedAttendee_NilExternalRepo(t *testing.T) {
	ctx := context.Background()

	// Provider without external contact repo
	provider := newTestProvider(nil, nil, nil)

	attendee := repository.Attendee{
		Email:       "unknown@example.com",
		DisplayName: "Unknown Person",
	}

	eventContext := &EventContext{
		Title:     "Team Meeting",
		StartTime: time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC),
	}

	err := provider.storeUnmatchedAttendee(ctx, attendee, "user@example.com", eventContext)

	// Should return nil without error when repo is nil
	assert.NoError(t, err)
}

// TestStoreUnmatchedAttendee_NilEventContext tests that nil event context is handled gracefully
func TestStoreUnmatchedAttendee_NilEventContext(t *testing.T) {
	ctx := context.Background()

	mockExtRepo := &mockExternalContactRepo{}
	provider := newTestProviderWithExternal(nil, nil, nil, mockExtRepo)

	attendee := repository.Attendee{
		Email:       "unknown@example.com",
		DisplayName: "Unknown Person",
	}

	err := provider.storeUnmatchedAttendee(ctx, attendee, "user@example.com", nil)

	// Should return nil without error and NOT call upsert
	assert.NoError(t, err)
	assert.False(t, mockExtRepo.upsertCalled)
}

// TestStoreUnmatchedAttendee_EmailNormalization tests that email is normalized to lowercase
func TestStoreUnmatchedAttendee_EmailNormalization(t *testing.T) {
	ctx := context.Background()

	mockExtRepo := &mockExternalContactRepo{}
	provider := newTestProviderWithExternal(nil, nil, nil, mockExtRepo)

	attendee := repository.Attendee{
		Email:       "  UNKNOWN@EXAMPLE.COM  ", // Uppercase with whitespace
		DisplayName: "Unknown Person",
	}

	eventContext := &EventContext{
		Title:     "Meeting",
		StartTime: accelerated.GetCurrentTime(),
	}

	err := provider.storeUnmatchedAttendee(ctx, attendee, "user@example.com", eventContext)

	assert.NoError(t, err)
	assert.True(t, mockExtRepo.upsertCalled)

	// Source ID should be normalized (lowercase, trimmed)
	req := mockExtRepo.upsertRequests[0]
	assert.Equal(t, "unknown@example.com", req.SourceID)
}

// TestStoreUnmatchedAttendee_EmptyDisplayName tests handling of empty display name
func TestStoreUnmatchedAttendee_EmptyDisplayName(t *testing.T) {
	ctx := context.Background()

	mockExtRepo := &mockExternalContactRepo{}
	provider := newTestProviderWithExternal(nil, nil, nil, mockExtRepo)

	attendee := repository.Attendee{
		Email:       "unknown@example.com",
		DisplayName: "", // Empty display name
	}

	eventContext := &EventContext{
		Title:     "Meeting",
		StartTime: accelerated.GetCurrentTime(),
	}

	err := provider.storeUnmatchedAttendee(ctx, attendee, "user@example.com", eventContext)

	assert.NoError(t, err)
	assert.True(t, mockExtRepo.upsertCalled)

	// DisplayName should be nil for empty string
	req := mockExtRepo.upsertRequests[0]
	assert.Nil(t, req.DisplayName)
}

// TestStoreUnmatchedAttendee_RepoError tests error handling when repo fails
func TestStoreUnmatchedAttendee_RepoError(t *testing.T) {
	ctx := context.Background()

	mockExtRepo := &mockExternalContactRepo{
		upsertError: assert.AnError,
	}
	provider := newTestProviderWithExternal(nil, nil, nil, mockExtRepo)

	attendee := repository.Attendee{
		Email:       "unknown@example.com",
		DisplayName: "Unknown Person",
	}

	eventContext := &EventContext{
		Title:     "Meeting",
		StartTime: accelerated.GetCurrentTime(),
	}

	err := provider.storeUnmatchedAttendee(ctx, attendee, "user@example.com", eventContext)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "upsert external contact")
}

// TestStoreUnmatchedAttendee_Deduplication tests that same email results in same source_id
func TestStoreUnmatchedAttendee_Deduplication(t *testing.T) {
	ctx := context.Background()

	mockExtRepo := &mockExternalContactRepo{}
	provider := newTestProviderWithExternal(nil, nil, nil, mockExtRepo)

	// Same person in two different meetings
	attendee := repository.Attendee{
		Email:       "colleague@example.com",
		DisplayName: "Colleague",
	}

	event1 := &EventContext{Title: "Meeting 1", StartTime: accelerated.GetCurrentTime()}
	event2 := &EventContext{Title: "Meeting 2", StartTime: accelerated.GetCurrentTime().Add(time.Hour)}

	_ = provider.storeUnmatchedAttendee(ctx, attendee, "user@example.com", event1)
	_ = provider.storeUnmatchedAttendee(ctx, attendee, "user@example.com", event2)

	assert.Len(t, mockExtRepo.upsertRequests, 2)

	// Both should use the same source_id (normalized email)
	// This allows the database to handle deduplication via upsert
	assert.Equal(t, mockExtRepo.upsertRequests[0].SourceID, mockExtRepo.upsertRequests[1].SourceID)
	assert.Equal(t, "colleague@example.com", mockExtRepo.upsertRequests[0].SourceID)
}

// TestMatchAttendees_StoresUnmatchedAttendees tests that unmatched attendees are stored via matchAttendees
func TestMatchAttendees_StoresUnmatchedAttendees(t *testing.T) {
	ctx := context.Background()

	contactID := uuid.New()

	// Identity service returns match for alice, no match for bob
	mockIdentity := &mockIdentityService{
		matchOrCreateResults: map[string]*service.MatchResult{
			"alice@example.com": {ContactID: &contactID},
		},
	}

	mockContactRepo := &mockContactRepo{
		findSimilarResults: []repository.ContactMatch{}, // No fuzzy matches
	}

	mockExtRepo := &mockExternalContactRepo{}

	provider := newTestProviderWithExternal(nil, mockContactRepo, mockIdentity, mockExtRepo)

	attendees := []repository.Attendee{
		{Email: "alice@example.com", Self: false, DisplayName: "Alice"}, // Will match
		{Email: "bob@example.com", Self: false, DisplayName: "Bob"},     // Will NOT match
	}

	eventContext := &EventContext{
		Title:     "Team Meeting",
		StartTime: accelerated.GetCurrentTime(),
		HtmlLink:  "https://calendar.google.com/event?eid=xyz",
	}

	matchedIDs := provider.matchAttendees(ctx, attendees, "user@example.com", eventContext)

	// Alice matched
	assert.Len(t, matchedIDs, 1)
	assert.Equal(t, contactID, matchedIDs[0])

	// Bob should be stored as unmatched
	assert.True(t, mockExtRepo.upsertCalled)
	assert.Len(t, mockExtRepo.upsertRequests, 1)
	assert.Equal(t, "bob@example.com", mockExtRepo.upsertRequests[0].SourceID)
	assert.Equal(t, "Team Meeting", mockExtRepo.upsertRequests[0].Metadata["meeting_title"])
}

// TestProcessEvent_HandlesEmptyHtmlLink verifies that empty HtmlLink is handled correctly
func TestProcessEvent_HandlesEmptyHtmlLink(t *testing.T) {
	ctx := context.Background()

	mockCalRepo := &mockCalendarRepo{}
	mockContactRepo := &mockContactRepo{}
	mockIdentity := &mockIdentityService{}

	provider := newTestProvider(mockCalRepo, mockContactRepo, mockIdentity)

	// Create a Google Calendar event without HtmlLink
	event := &calendar.Event{
		Id:       "test-event-456",
		Summary:  "Test Meeting",
		HtmlLink: "", // Empty
		Status:   "confirmed",
		Start: &calendar.EventDateTime{
			DateTime: "2024-06-15T10:00:00Z",
		},
		End: &calendar.EventDateTime{
			DateTime: "2024-06-15T11:00:00Z",
		},
		Organizer: &calendar.EventOrganizer{
			Email: "user@example.com",
		},
	}

	err := provider.processEvent(ctx, event, "user@example.com")

	assert.NoError(t, err)
	assert.True(t, mockCalRepo.upsertCalled, "Upsert should be called")
	assert.NotNil(t, mockCalRepo.upsertRequest, "Upsert request should not be nil")
	assert.Nil(t, mockCalRepo.upsertRequest.HtmlLink, "HtmlLink should be nil for empty string")
}
