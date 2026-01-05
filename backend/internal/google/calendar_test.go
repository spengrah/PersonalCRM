package google

import (
	"testing"

	"personal-crm/backend/internal/repository"

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
