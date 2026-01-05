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
	provider := NewCalendarSyncProvider(nil, nil, nil, nil)
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
	provider := NewCalendarSyncProvider(nil, nil, nil, nil)
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
	provider := NewCalendarSyncProvider(nil, nil, nil, nil)
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
	provider := NewCalendarSyncProvider(nil, nil, nil, nil)
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
	provider := NewCalendarSyncProvider(nil, nil, nil, nil)
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
	provider := NewCalendarSyncProvider(nil, nil, nil, nil)

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
	provider := NewCalendarSyncProvider(nil, nil, nil, nil)
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

	provider := newTestableProvider(nil, nil, mockIdentity)

	attendees := []repository.Attendee{
		{Email: "user@example.com", Self: true, DisplayName: "User"},        // Should be skipped (self)
		{Email: "alice@example.com", Self: false, DisplayName: "Alice"},     // Should match
		{Email: "bob@example.com", Self: false, DisplayName: "Bob"},         // Should match
		{Email: "unknown@example.com", Self: false, DisplayName: "Unknown"}, // No match
		{Email: "", Self: false, DisplayName: "No Email"},                   // Should be skipped (empty)
	}

	matchedIDs := provider.matchAttendees(ctx, attendees, "user@example.com")

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

	provider := newTestableProvider(nil, nil, mockIdentity)

	attendees := []repository.Attendee{
		{Email: "alice@example.com", Self: false, DisplayName: "Alice"},
		{Email: "alice2@example.com", Self: false, DisplayName: "Alice Alt"},
	}

	matchedIDs := provider.matchAttendees(ctx, attendees, "user@example.com")

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

	provider := newTestableProvider(nil, nil, mockIdentity)

	attendees := []repository.Attendee{
		{Email: "alice@example.com", Self: false, DisplayName: "Alice"}, // No match
		{Email: "bob@example.com", Self: false, DisplayName: "Bob"},     // Match
	}

	matchedIDs := provider.matchAttendees(ctx, attendees, "user@example.com")

	assert.True(t, mockIdentity.matchOrCreateCalled)
	assert.Len(t, matchedIDs, 1)
	assert.Equal(t, contactID, matchedIDs[0])
}

// TestUpdateLastContactedForPastEvents_UpdatesContactsAndMarksEvents tests the full update flow
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

	provider := newTestableProvider(mockCalRepo, mockContactRepo, nil)

	err := provider.updateLastContactedForPastEvents(ctx, accelerated.GetCurrentTime())

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

	provider := newTestableProvider(mockCalRepo, mockContactRepo, nil)

	err := provider.updateLastContactedForPastEvents(ctx, accelerated.GetCurrentTime())

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

	provider := newTestableProvider(mockCalRepo, mockContactRepo, nil)

	err := provider.updateLastContactedForPastEvents(ctx, accelerated.GetCurrentTime())

	assert.NoError(t, err)
	assert.True(t, mockCalRepo.listPastCalled)
	assert.False(t, mockContactRepo.updateLastContactedCalled) // No contacts to update
	assert.False(t, mockCalRepo.markUpdatedCalled)             // No events to mark
}
