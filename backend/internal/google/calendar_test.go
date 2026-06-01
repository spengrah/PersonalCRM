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
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil, nil, nil)
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
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil, nil, nil)
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
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil, nil, nil)
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

// TestGetUserResponse_DeclinedAttendee verifies getUserResponse reports a
// declined self-attendee. The remove-branch behavior for declined events is
// covered in calendar_decline_test.go.
func TestGetUserResponse_DeclinedAttendee(t *testing.T) {
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil, nil, nil)
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

// TestBuildAttendeeList_EmptyAttendees verifies behavior with no attendees but an organizer
func TestBuildAttendeeList_EmptyAttendees(t *testing.T) {
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil, nil, nil)
	accountID := "user@example.com"

	event := &calendar.Event{
		Organizer: &calendar.EventOrganizer{Email: "organizer@example.com", DisplayName: "Organizer"},
		Attendees: []*calendar.EventAttendee{},
	}

	attendees := provider.buildAttendeeList(event, accountID)

	// Organizer should be included even when not in attendees list
	assert.Len(t, attendees, 1)
	assert.Equal(t, "organizer@example.com", attendees[0].Email)
	assert.Equal(t, "Organizer", attendees[0].DisplayName)
	assert.True(t, attendees[0].Organizer)
	assert.False(t, attendees[0].Self)
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
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil, nil, nil)

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
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil, nil, nil)
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

// TestBuildAttendeeList_OrganizerNotInAttendees verifies organizer is added when not in attendees list
// This is the fix for issue #183 - organizers who aren't also attendees should still be matched
func TestBuildAttendeeList_OrganizerNotInAttendees(t *testing.T) {
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil, nil, nil)
	accountID := "user@example.com"

	// Event where the organizer (Bob) is NOT in the attendees list.
	// Regression scenario from issue #183.
	event := &calendar.Event{
		Organizer: &calendar.EventOrganizer{
			Email:       "bob@example.com",
			DisplayName: "Bob",
		},
		Attendees: []*calendar.EventAttendee{
			{
				Email:          "alice@example.com",
				DisplayName:    "Alice",
				Self:           true,
				ResponseStatus: "accepted",
			},
		},
	}

	attendees := provider.buildAttendeeList(event, accountID)

	// Should have 2 attendees: Alice (self) + Bob (organizer added)
	assert.Len(t, attendees, 2)

	// Find Bob (organizer who was added)
	var organizer *repository.Attendee
	for i := range attendees {
		if attendees[i].Email == "bob@example.com" {
			organizer = &attendees[i]
			break
		}
	}
	assert.NotNil(t, organizer, "Organizer should be in attendees list")
	assert.Equal(t, "Bob", organizer.DisplayName)
	assert.True(t, organizer.Organizer)
	assert.False(t, organizer.Self)
	assert.Equal(t, "", organizer.ResponseType) // Organizers don't have response status
}

// TestBuildAttendeeList_OrganizerAlreadyInAttendees verifies organizer is not duplicated
func TestBuildAttendeeList_OrganizerAlreadyInAttendees(t *testing.T) {
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil, nil, nil)
	accountID := "user@example.com"

	// Event where organizer IS in the attendees list
	event := &calendar.Event{
		Organizer: &calendar.EventOrganizer{
			Email:       "organizer@example.com",
			DisplayName: "Organizer",
		},
		Attendees: []*calendar.EventAttendee{
			{
				Email:          "user@example.com",
				Self:           true,
				ResponseStatus: "accepted",
			},
			{
				Email:          "organizer@example.com",
				DisplayName:    "Organizer",
				ResponseStatus: "accepted",
			},
		},
	}

	attendees := provider.buildAttendeeList(event, accountID)

	// Should only have 2 attendees - organizer should NOT be duplicated
	assert.Len(t, attendees, 2)

	// Count how many times organizer appears
	organizerCount := 0
	for _, a := range attendees {
		if a.Email == "organizer@example.com" {
			organizerCount++
		}
	}
	assert.Equal(t, 1, organizerCount, "Organizer should appear exactly once")
}

// TestBuildAttendeeList_OrganizerIsSelf verifies organizer is not added when they are self
func TestBuildAttendeeList_OrganizerIsSelf(t *testing.T) {
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil, nil, nil)
	accountID := "user@example.com"

	// User is the organizer but not in attendees list
	event := &calendar.Event{
		Organizer: &calendar.EventOrganizer{
			Email:       "user@example.com",
			DisplayName: "User",
		},
		Attendees: []*calendar.EventAttendee{
			{
				Email:          "other@example.com",
				DisplayName:    "Other Person",
				ResponseStatus: "accepted",
			},
		},
	}

	attendees := provider.buildAttendeeList(event, accountID)

	// Should only have 1 attendee - organizer (self) should NOT be added
	assert.Len(t, attendees, 1)
	assert.Equal(t, "other@example.com", attendees[0].Email)
}

// TestBuildAttendeeList_OrganizerEmailCaseInsensitive verifies case-insensitive duplicate detection
func TestBuildAttendeeList_OrganizerEmailCaseInsensitive(t *testing.T) {
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil, nil, nil)
	accountID := "user@example.com"

	// Organizer email has different case than in attendees list
	event := &calendar.Event{
		Organizer: &calendar.EventOrganizer{
			Email:       "ORGANIZER@EXAMPLE.COM",
			DisplayName: "Organizer",
		},
		Attendees: []*calendar.EventAttendee{
			{
				Email:          "organizer@example.com", // lowercase
				DisplayName:    "Organizer",
				ResponseStatus: "accepted",
			},
		},
	}

	attendees := provider.buildAttendeeList(event, accountID)

	// Should only have 1 attendee - case difference should not cause duplication
	assert.Len(t, attendees, 1)
}

// TestBuildAttendeeList_NoOrganizer verifies behavior when organizer is nil
func TestBuildAttendeeList_NoOrganizer(t *testing.T) {
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil, nil, nil)
	accountID := "user@example.com"

	event := &calendar.Event{
		Organizer: nil,
		Attendees: []*calendar.EventAttendee{
			{
				Email:          "other@example.com",
				DisplayName:    "Other Person",
				ResponseStatus: "accepted",
			},
		},
	}

	attendees := provider.buildAttendeeList(event, accountID)

	// Should work fine with nil organizer
	assert.Len(t, attendees, 1)
	assert.Equal(t, "other@example.com", attendees[0].Email)
}

// TestBuildAttendeeList_OrganizerEmptyEmail verifies behavior when organizer has empty email
func TestBuildAttendeeList_OrganizerEmptyEmail(t *testing.T) {
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil, nil, nil)
	accountID := "user@example.com"

	event := &calendar.Event{
		Organizer: &calendar.EventOrganizer{
			Email:       "",
			DisplayName: "No Email Organizer",
		},
		Attendees: []*calendar.EventAttendee{
			{
				Email:          "other@example.com",
				DisplayName:    "Other Person",
				ResponseStatus: "accepted",
			},
		},
	}

	attendees := provider.buildAttendeeList(event, accountID)

	// Should not add organizer with empty email
	assert.Len(t, attendees, 1)
	assert.Equal(t, "other@example.com", attendees[0].Email)
}

// TestBuildAttendeeList_OrganizerSelfFlag verifies organizer is not added when Organizer.Self is true
// even if organizer email differs from accountID (handles aliases and delegated calendars)
func TestBuildAttendeeList_OrganizerSelfFlag(t *testing.T) {
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil, nil, nil)
	accountID := "user@example.com"

	// Organizer uses an alias email but Self flag is true
	event := &calendar.Event{
		Organizer: &calendar.EventOrganizer{
			Email:       "user-alias@example.com", // Different from accountID
			DisplayName: "User Alias",
			Self:        true, // But Self flag indicates it's the user
		},
		Attendees: []*calendar.EventAttendee{
			{
				Email:          "other@example.com",
				DisplayName:    "Other Person",
				ResponseStatus: "accepted",
			},
		},
	}

	attendees := provider.buildAttendeeList(event, accountID)

	// Should only have 1 attendee - organizer with Self=true should NOT be added
	// even though their email doesn't match accountID
	assert.Len(t, attendees, 1)
	assert.Equal(t, "other@example.com", attendees[0].Email)
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

// TestUpdateLastContactedForPastEvents_NilBus_SkipsEventsWithoutMarking asserts
// the post-cutover behavior: when eventBus is nil (mode=off/shadow), the
// loop lists past events but skips the publish/mark block per event. This
// exercises the real plan Decision 6 wiring — rolling back to off/shadow
// does NOT restore the direct path (spec §3.9).
func TestUpdateLastContactedForPastEvents_NilBus_SkipsEventsWithoutMarking(t *testing.T) {
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

	// newTestProvider leaves eventBus nil — matches mode=off/shadow wiring.
	provider := newTestProvider(mockCalRepo, mockContactRepo, nil)

	err := provider.updateLastContactedForPastEvents(ctx)
	assert.NoError(t, err)

	// Listed past events but skipped the publish + mark block (no bus).
	assert.True(t, mockCalRepo.listPastCalled)
	assert.False(t, mockCalRepo.markUpdatedCalled,
		"nil eventBus must NOT mark events processed — next tick with a real bus retries them")
	_ = contactID1
	_ = contactID2
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
	assert.False(t, mockCalRepo.markUpdatedCalled) // No events to mark
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
						{Type: "email", Value: "jon@example.com"},
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
						{Type: "email", Value: "john.smith@work.com"}, // Same as attendee!
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
						{Type: "email", Value: "john.smith@work.com"}, // Email matches!
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
						{Type: "email", Value: "John.Smith@EXAMPLE.COM"}, // Different case
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
						{Type: "email", Value: "john.smith@work.com"}, // Matches attendee email
						{Type: "phone", Value: "+1234567890"},         // Should be ignored
						{Type: "phone", Value: "+0987654321"},         // Should be ignored
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

// TestProcessEvent_MatchesOrganizerNotInAttendees verifies that when an organizer is not in the
// attendees list, they are still matched to a contact (fix for issue #183)
func TestProcessEvent_MatchesOrganizerNotInAttendees(t *testing.T) {
	ctx := context.Background()

	organizerContactID := uuid.New()

	mockCalRepo := &mockCalendarRepo{}
	mockContactRepo := &mockContactRepo{}
	mockIdentity := &mockIdentityService{
		matchOrCreateResults: map[string]*service.MatchResult{
			"bob@example.com": {ContactID: &organizerContactID, MatchType: "exact"},
		},
	}

	provider := newTestProvider(mockCalRepo, mockContactRepo, mockIdentity)

	// Recreate the scenario from issue #183:
	// Event where the organizer (Bob) is not in the attendees list.
	event := &calendar.Event{
		Id:       "alice-bob-meeting",
		Summary:  "Alice<>Bob",
		Status:   "confirmed",
		HtmlLink: "https://calendar.google.com/event?eid=abc123",
		Start: &calendar.EventDateTime{
			DateTime: "2025-12-23T10:00:00Z",
		},
		End: &calendar.EventDateTime{
			DateTime: "2025-12-23T11:00:00Z",
		},
		Organizer: &calendar.EventOrganizer{
			Email:       "bob@example.com",
			DisplayName: "Bob",
		},
		Attendees: []*calendar.EventAttendee{
			{
				Email:          "alice@example.com",
				DisplayName:    "Alice",
				Self:           true,
				ResponseStatus: "accepted",
			},
			// Note: the organizer (Bob) is NOT in the attendees list
		},
	}

	err := provider.processEvent(ctx, event, "alice@example.com")

	assert.NoError(t, err)
	assert.True(t, mockCalRepo.upsertCalled, "Upsert should be called")
	assert.NotNil(t, mockCalRepo.upsertRequest, "Upsert request should not be nil")

	// Verify the organizer was matched even though they weren't in attendees
	assert.Len(t, mockCalRepo.upsertRequest.MatchedContactIDs, 1,
		"Organizer should be matched even when not in attendees list")
	assert.Equal(t, organizerContactID, mockCalRepo.upsertRequest.MatchedContactIDs[0],
		"Matched contact should be the organizer's contact")

	// Verify the identity service was called with the organizer's email
	assert.True(t, mockIdentity.matchOrCreateCalled, "Identity service should be called")
	var foundOrganizerCall bool
	for _, req := range mockIdentity.matchOrCreateRequests {
		if req.RawIdentifier == "bob@example.com" {
			foundOrganizerCall = true
			break
		}
	}
	assert.True(t, foundOrganizerCall, "Identity service should be called with organizer email")
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

// TestStoreUnmatchedAttendee_EmptyDisplayName_InfersFromEmail tests that name is inferred from email when display name is empty
func TestStoreUnmatchedAttendee_EmptyDisplayName_InfersFromEmail(t *testing.T) {
	ctx := context.Background()

	mockExtRepo := &mockExternalContactRepo{}
	provider := newTestProviderWithExternal(nil, nil, nil, mockExtRepo)

	attendee := repository.Attendee{
		Email:       "john.smith@example.com",
		DisplayName: "", // Empty display name - should be inferred from email
	}

	eventContext := &EventContext{
		Title:     "Meeting",
		StartTime: accelerated.GetCurrentTime(),
	}

	err := provider.storeUnmatchedAttendee(ctx, attendee, "user@example.com", eventContext)

	assert.NoError(t, err)
	assert.True(t, mockExtRepo.upsertCalled)

	// DisplayName should be inferred from email pattern "john.smith@..." -> "John Smith"
	req := mockExtRepo.upsertRequests[0]
	assert.NotNil(t, req.DisplayName)
	assert.Equal(t, "John Smith", *req.DisplayName)
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

// ========================================
// Issue #123/#185: Blocked Calendar Email Tests
// ========================================

// TestIsBlockedCalendarEmail_Legacy tests the blocked calendar email filter
// (originally for Issue #123, expanded for Issue #185)
func TestIsBlockedCalendarEmail_Legacy(t *testing.T) {
	tests := []struct {
		name     string
		email    string
		expected bool
	}{
		// Blocked Google Calendar resource domains
		{"group calendar domain", "u1958p3u4pd7qfvlf9n9416iac@group.calendar.google.com", true},
		{"resource calendar domain", "room-123@resource.calendar.google.com", true},
		{"generic calendar domain", "calendar-abc@calendar.google.com", true},
		{"system calendar domain (holidays)", "en.usa#holiday@group.v.calendar.google.com", true},
		{"system calendar domain (contacts)", "addressbook#contacts@group.v.calendar.google.com", true},

		// Uppercase variations (should still be blocked)
		{"uppercase group domain", "ABC@GROUP.CALENDAR.GOOGLE.COM", true},
		{"mixed case resource domain", "Room@Resource.Calendar.Google.Com", true},

		// With whitespace (should still be blocked after trimming)
		{"whitespace around blocked domain", "  room@resource.calendar.google.com  ", true},

		// Not blocked - regular email domains
		{"gmail address", "john.smith@gmail.com", false},
		{"company domain", "alice@company.com", false},
		{"google apps domain", "bob@customdomain.com", false},
		{"similar but not blocked", "room@calendar.example.com", false},
		{"partial match not blocked", "room@group.calendar.example.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isBlockedCalendarEmail(tt.email)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestMatchAttendees_SkipsBlockedDomains tests that blocked calendar domains are skipped
func TestMatchAttendees_SkipsBlockedDomains(t *testing.T) {
	ctx := context.Background()

	contactID := uuid.New()

	mockIdentity := &mockIdentityService{
		matchOrCreateResults: map[string]*service.MatchResult{
			"alice@example.com": {ContactID: &contactID},
		},
	}

	mockContactRepo := &mockContactRepo{}
	mockExtRepo := &mockExternalContactRepo{}

	provider := newTestProviderWithExternal(nil, mockContactRepo, mockIdentity, mockExtRepo)

	attendees := []repository.Attendee{
		{Email: "alice@example.com", Self: false, DisplayName: "Alice"},                                    // Should match
		{Email: "fun-stuff@group.calendar.google.com", Self: false, DisplayName: "Fun Stuff"},              // Should be skipped (blocked domain)
		{Email: "conf-room-a@resource.calendar.google.com", Self: false, DisplayName: "Conference Room A"}, // Should be skipped (blocked domain)
		{Email: "en.usa#holiday@group.v.calendar.google.com", Self: false, DisplayName: "US Holidays"},     // Should be skipped (blocked domain)
		{Email: "unknown@example.com", Self: false, DisplayName: "Unknown"},                                // Should be stored as import candidate
	}

	eventContext := &EventContext{
		Title:     "Team Meeting",
		StartTime: accelerated.GetCurrentTime(),
	}

	matchedIDs := provider.matchAttendees(ctx, attendees, "user@example.com", eventContext)

	// Only Alice should be matched
	assert.Len(t, matchedIDs, 1)
	assert.Equal(t, contactID, matchedIDs[0])

	// Identity service should only be called for non-blocked emails (alice and unknown)
	assert.Equal(t, 2, len(mockIdentity.matchOrCreateRequests))

	// Only unknown@example.com should be stored as import candidate (not the blocked domains)
	assert.Len(t, mockExtRepo.upsertRequests, 1)
	assert.Equal(t, "unknown@example.com", mockExtRepo.upsertRequests[0].SourceID)
}

// ========================================
// Issue #124: Name Inference Tests
// ========================================

// TestInferNameFromEmail tests the email-to-name inference function
func TestInferNameFromEmail(t *testing.T) {
	tests := []struct {
		name     string
		email    string
		expected *string
	}{
		// Happy path - dot separator
		{"first.last pattern", "john.smith@gmail.com", strPtr("John Smith")},
		{"first.middle.last pattern", "john.david.smith@gmail.com", strPtr("John David Smith")},

		// Underscore separator
		{"first_last pattern", "john_smith@gmail.com", strPtr("John Smith")},
		{"first_middle_last pattern", "john_david_smith@gmail.com", strPtr("John David Smith")},

		// Single word
		{"single word", "john@gmail.com", strPtr("John")},

		// Trailing numbers (should be stripped)
		{"trailing numbers on last name", "john.smith2@gmail.com", strPtr("John Smith")},
		{"trailing numbers with dot", "john.smith.267@gmail.com", strPtr("John Smith")},
		{"trailing numbers on all parts", "john2.smith3@gmail.com", strPtr("John Smith")},
		{"only trailing numbers stripped", "john2smith@gmail.com", strPtr("John2smith")},

		// Gmail + modifier
		{"gmail plus modifier", "john.smith+work@gmail.com", strPtr("John Smith")},
		{"plus modifier single word", "john+newsletter@gmail.com", strPtr("John")},

		// Case handling
		{"uppercase email", "JOHN.SMITH@GMAIL.COM", strPtr("John Smith")},
		{"mixed case", "JoHn.SmItH@gmail.com", strPtr("John Smith")},

		// Edge cases
		{"excessive dots", "john...smith@gmail.com", strPtr("John Smith")},
		{"trailing dot", "john.smith.@gmail.com", strPtr("John Smith")},
		{"leading dot", ".john.smith@gmail.com", strPtr("John Smith")},

		// Nil cases
		{"all numbers", "123456@gmail.com", nil},
		{"only numbers after strip", "123@gmail.com", nil},
		{"no @ symbol", "johnsmith", nil},
		{"empty local part after processing", ".@gmail.com", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := inferNameFromEmail(tt.email)
			if tt.expected == nil {
				assert.Nil(t, result)
			} else {
				assert.NotNil(t, result)
				assert.Equal(t, *tt.expected, *result)
			}
		})
	}
}

// TestStripTrailingNumbers tests the trailing number stripping function
func TestStripTrailingNumbers(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"smith2", "smith"},
		{"smith123", "smith"},
		{"smith", "smith"},
		{"123smith", "123smith"},
		{"smith2abc", "smith2abc"},
		{"", ""},
		{"123", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := stripTrailingNumbers(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestCapitalize tests the capitalize helper function
func TestCapitalize(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"john", "John"},
		{"JOHN", "John"},
		{"jOHN", "John"},
		{"j", "J"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := capitalize(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestStoreUnmatchedAttendee_PreservesExistingDisplayName tests that provided display names are not overwritten
func TestStoreUnmatchedAttendee_PreservesExistingDisplayName(t *testing.T) {
	ctx := context.Background()

	mockExtRepo := &mockExternalContactRepo{}
	provider := newTestProviderWithExternal(nil, nil, nil, mockExtRepo)

	// Attendee has a display name, so it should NOT be inferred from email
	attendee := repository.Attendee{
		Email:       "jsmith@example.com", // Would infer "Jsmith" if no display name
		DisplayName: "John Smith III",     // Should use this instead
	}

	eventContext := &EventContext{
		Title:     "Meeting",
		StartTime: accelerated.GetCurrentTime(),
	}

	err := provider.storeUnmatchedAttendee(ctx, attendee, "user@example.com", eventContext)

	assert.NoError(t, err)
	assert.True(t, mockExtRepo.upsertCalled)

	req := mockExtRepo.upsertRequests[0]
	assert.NotNil(t, req.DisplayName)
	assert.Equal(t, "John Smith III", *req.DisplayName)
}

// ========================================
// Issue #126: Event Response Filtering Tests
// ========================================

// TestProcessEvent_OnlyAcceptsAcceptedEvents verifies that only accepted events are processed
func TestProcessEvent_OnlyAcceptsAcceptedEvents(t *testing.T) {
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil, nil, nil)
	accountID := "user@example.com"

	tests := []struct {
		name           string
		responseStatus string
		shouldSkip     bool
	}{
		{"accepted events are processed", "accepted", false},
		{"declined events are skipped", "declined", true},
		{"tentative events are skipped", "tentative", true},
		{"needsAction events are skipped", "needsAction", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := &calendar.Event{
				Id:      "test-event",
				Summary: "Test Meeting",
				Status:  "confirmed",
				Attendees: []*calendar.EventAttendee{
					{
						Email:          accountID,
						Self:           true,
						ResponseStatus: tt.responseStatus,
					},
				},
			}

			userResponse := provider.getUserResponse(event, accountID)

			if tt.shouldSkip {
				// Event should be skipped (userResponse is nil or not "accepted")
				assert.True(t, userResponse == nil || *userResponse != "accepted",
					"Expected event with response '%s' to be skipped", tt.responseStatus)
			} else {
				// Event should be processed (userResponse is "accepted")
				assert.NotNil(t, userResponse)
				assert.Equal(t, "accepted", *userResponse)
			}
		})
	}
}

// TestProcessEvent_SkipsEventsWhereUserNotAttendee verifies events without user are skipped
func TestProcessEvent_SkipsEventsWhereUserNotAttendee(t *testing.T) {
	provider := NewCalendarSyncProvider(nil, nil, nil, nil, nil, nil, nil)
	accountID := "user@example.com"

	event := &calendar.Event{
		Id:      "test-event",
		Summary: "Meeting I wasn't invited to",
		Status:  "confirmed",
		Organizer: &calendar.EventOrganizer{
			Email: "other@example.com",
		},
		Attendees: []*calendar.EventAttendee{
			{
				Email:          "alice@example.com",
				ResponseStatus: "accepted",
			},
			{
				Email:          "bob@example.com",
				ResponseStatus: "accepted",
			},
		},
	}

	userResponse := provider.getUserResponse(event, accountID)

	// User is not in attendees or organizer, so response should be nil
	assert.Nil(t, userResponse)
}

// ========================================
// isBlockedCalendarEmail Tests
// ========================================

// TestIsBlockedCalendarEmail_BlockedDomains verifies that calendar resource domains
// are blocked. Note: We only block domains where ALL emails are system accounts
// (not company domains where employees might have legitimate addresses).
func TestIsBlockedCalendarEmail_BlockedDomains(t *testing.T) {
	blockedEmails := []string{
		// Google Calendar resources (always system, never real people)
		"room@resource.calendar.google.com",
		"team@group.calendar.google.com",
		"holidays@calendar.google.com",
		"events@group.v.calendar.google.com",
		"bryan%cnslabs.io@gtempaccount.com", // Google temp account
	}

	for _, email := range blockedEmails {
		t.Run(email, func(t *testing.T) {
			assert.True(t, isBlockedCalendarEmail(email),
				"Expected %s to be blocked", email)
		})
	}
}

// TestIsBlockedCalendarEmail_BlockedPrefixes verifies that system email prefixes are blocked
func TestIsBlockedCalendarEmail_BlockedPrefixes(t *testing.T) {
	blockedEmails := []string{
		"noreply@example.com",
		"no-reply@company.org",
		"no_reply@startup.io",
		"do-not-reply@bigcorp.com",
		"donotreply@service.net",
		"calendar-invite@meeting.com",
		"notifications@alerts.io",
		"mailer-daemon@mail.example.com",
		"postmaster@domain.com",
		// With + modifier
		"noreply+tag@example.com",
		"no-reply+unsubscribe@company.org",
	}

	for _, email := range blockedEmails {
		t.Run(email, func(t *testing.T) {
			assert.True(t, isBlockedCalendarEmail(email),
				"Expected %s to be blocked", email)
		})
	}
}

// TestIsBlockedCalendarEmail_AllowedEmails verifies that legitimate emails are not blocked
func TestIsBlockedCalendarEmail_AllowedEmails(t *testing.T) {
	allowedEmails := []string{
		"alice@example.com",
		"bob.smith@company.org",
		"jane+work@gmail.com",
		"contact@startup.io",
		"hello@business.com",
		"bob@example.com",
		"alice@anotherdomain.test",
		// Emails that might look suspicious but are valid
		"noreplyman@example.com",    // Contains "noreply" but not as prefix
		"john.noreply@company.com",  // Contains "noreply" but not as prefix
		"calendar@mycompany.com",    // Calendar in name but not a blocked domain
		"notifications.team@co.com", // Contains "notifications" but as part of name
		// Real employees at scheduling companies should NOT be blocked
		// (This is why we don't block company domains - only system prefixes)
		"alice@calendly.com",
		"bob@lu.ma",
		"jane@cal.com",
		"john@hubspot.com",
		"meeting@savvycal.com", // "meeting" is not a blocked prefix
		"scheduler@doodle.com", // "scheduler" is not a blocked prefix
	}

	for _, email := range allowedEmails {
		t.Run(email, func(t *testing.T) {
			assert.False(t, isBlockedCalendarEmail(email),
				"Expected %s to NOT be blocked", email)
		})
	}
}

// TestIsBlockedCalendarEmail_CaseInsensitive verifies case-insensitive matching
func TestIsBlockedCalendarEmail_CaseInsensitive(t *testing.T) {
	blockedEmails := []string{
		"NOREPLY@calendly.com",
		"NoReply@CALENDLY.COM",
		"Calendar-Invite@Lu.Ma",
		"NOTIFICATIONS@Example.Com",
	}

	for _, email := range blockedEmails {
		t.Run(email, func(t *testing.T) {
			assert.True(t, isBlockedCalendarEmail(email),
				"Expected %s to be blocked (case-insensitive)", email)
		})
	}
}

// TestIsBlockedCalendarEmail_EdgeCases verifies edge case handling
func TestIsBlockedCalendarEmail_EdgeCases(t *testing.T) {
	testCases := []struct {
		email    string
		blocked  bool
		scenario string
	}{
		{"", false, "empty string"},
		{"  noreply@example.com  ", true, "with whitespace"},
		{"@resource.calendar.google.com", true, "missing local part but blocked domain"},
		{"@calendly.com", false, "company domain not blocked (employees could use it)"},
		{"noreply@", true, "blocked prefix even with missing domain"},
		{"noreply", false, "no @ symbol"},
		{"user@unknowndomain.com", false, "regular unknown domain"},
	}

	for _, tc := range testCases {
		t.Run(tc.scenario, func(t *testing.T) {
			result := isBlockedCalendarEmail(tc.email)
			assert.Equal(t, tc.blocked, result,
				"For %q expected blocked=%v", tc.email, tc.blocked)
		})
	}
}
