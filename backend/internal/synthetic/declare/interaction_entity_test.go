package declare

import (
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInteractionVocabulary_Validation(t *testing.T) {
	accepted := []struct {
		name   string
		entity Entity
	}{
		{"email", MessageInteraction("email", "subject", "email", AgoDays(1))},
		{"gchat", MessageInteraction("gchat", "subject", "gchat", AgoDays(1))},
		{"whatsapp", MessageInteraction("whatsapp", "subject", "whatsapp", AgoDays(1))},
		{"telegram", MessageInteraction("telegram", "subject", "telegram", AgoDays(1))},
		{"messages", MessageInteraction("messages", "subject", "messages", AgoDays(1))},
		{"burst", MessageInteraction("burst", "subject", "messages", AgoDays(1), Burst(3))},
		{"manual", LoggedInteraction("manual", "subject", "manual", AgoDays(1))},
		{"todoist", LoggedInteraction("todoist", "subject", "todoist", AgoDays(1))},
		{"anarlog", LoggedInteraction("anarlog", "subject", "anarlog_sessions", AgoDays(1))},
		{"phone", PhoneCallInteraction("phone", "subject", AgoDays(1))},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, tc.entity.validate())
		})
	}

	rejected := []struct {
		name   string
		entity Entity
	}{
		{"message-source", MessageInteraction("x", "subject", "unknown", AgoDays(1))},
		{"logged-source", LoggedInteraction("x", "subject", "unknown", AgoDays(1))},
		{"message-missing-age", MessageInteraction("x", "subject", "email")},
		{"message-zero-age", MessageInteraction("x", "subject", "email", AgoDays(0))},
		{"message-too-old", MessageInteraction("x", "subject", "email", AgoDays(61))},
		{"phone-burst", PhoneCallInteraction("x", "subject", AgoDays(1), Burst(2))},
		{"logged-burst", LoggedInteraction("x", "subject", "manual", AgoDays(1), Burst(2))},
		{"burst-zero", MessageInteraction("x", "subject", "email", AgoDays(1), Burst(0))},
		{"burst-four", MessageInteraction("x", "subject", "email", AgoDays(1), Burst(4))},
		{"dangling-contact", MessageInteraction("x", "missing", "email", AgoDays(1))},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "dangling-contact" {
				assert.Error(t, validateEntityOrder([]Entity{tc.entity}))
				return
			}
			assert.Error(t, tc.entity.validate())
		})
	}

	require.NoError(t, validateEntityOrder([]Entity{
		Contact("subject", Methods(MethodEmail)),
		MessageInteraction("message", "subject", "email", AgoDays(1)),
	}))
	assert.Error(t, validateEntityOrder([]Entity{
		MessageInteraction("message", "subject", "email", AgoDays(1)),
	}))
}

func TestInteractionVocabulary_EqualAgoDaysEqualOccurredAt(t *testing.T) {
	anchor := time.Date(2026, 8, 25, 13, 14, 15, 987654321, time.UTC)
	a := interactionTarget(anchor, 7)
	b := interactionTarget(anchor, 7)
	c := interactionTarget(anchor, 8)
	assert.Equal(t, a, b)
	assert.Zero(t, a.Nanosecond())
	assert.True(t, c.Before(a))
}

func TestInteractionVocabulary_DrilldownValidation(t *testing.T) {
	accepted := []struct {
		name     string
		entities []Entity
	}{
		{"group-thread", []Entity{
			Contact("subject", Methods(MethodEmail)),
			Contact("speaker", Methods(MethodEmail)),
			MessageInteraction("thread", "subject", "gchat", AgoDays(1), GroupThread("speaker")),
		}},
		{"linked-note", []Entity{
			Contact("subject", Methods(MethodEmail)),
			CalendarEvent("meeting", "subject", StartedDaysAgo(4)),
			LinkedMeetingNote("note", "meeting"),
		}},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, validateEntityOrder(tc.entities))
		})
	}

	rejected := []struct {
		name     string
		entities []Entity
		contains string
	}{
		{"group-email", []Entity{Contact("subject", Methods(MethodEmail)), Contact("speaker", Methods(MethodEmail)), MessageInteraction("x", "subject", "email", AgoDays(1), GroupThread("speaker"))}, "gchat"},
		{"group-whatsapp", []Entity{Contact("subject", Methods(MethodEmail)), Contact("speaker", Methods(MethodEmail)), MessageInteraction("x", "subject", "whatsapp", AgoDays(1), GroupThread("speaker"))}, "gchat"},
		{"group-telegram", []Entity{Contact("subject", Methods(MethodEmail)), Contact("speaker", Methods(MethodEmail)), MessageInteraction("x", "subject", "telegram", AgoDays(1), GroupThread("speaker"))}, "gchat"},
		{"group-messages", []Entity{Contact("subject", Methods(MethodEmail)), Contact("speaker", Methods(MethodEmail)), MessageInteraction("x", "subject", "messages", AgoDays(1), GroupThread("speaker"))}, "gchat"},
		{"group-phone", []Entity{Contact("subject", Methods(MethodEmail)), Contact("speaker", Methods(MethodEmail)), PhoneCallInteraction("x", "subject", AgoDays(1), GroupThread("speaker"))}, "GroupThread"},
		{"group-logged", []Entity{Contact("subject", Methods(MethodEmail)), Contact("speaker", Methods(MethodEmail)), LoggedInteraction("x", "subject", "manual", AgoDays(1), GroupThread("speaker"))}, "GroupThread"},
		{"group-empty-speaker", []Entity{Contact("subject", Methods(MethodEmail)), MessageInteraction("x", "subject", "gchat", AgoDays(1), GroupThread(""))}, "speaker handle"},
		{"group-dangling-speaker", []Entity{Contact("subject", Methods(MethodEmail)), MessageInteraction("x", "subject", "gchat", AgoDays(1), GroupThread("missing"))}, "EARLIER"},
		{"group-burst", []Entity{Contact("subject", Methods(MethodEmail)), Contact("speaker", Methods(MethodEmail)), MessageInteraction("x", "subject", "gchat", AgoDays(1), Burst(2), GroupThread("speaker"))}, "Burst"},
		{"note-empty-handle", []Entity{Contact("subject", Methods(MethodEmail)), CalendarEvent("meeting", "subject", StartedDaysAgo(4)), LinkedMeetingNote("", "meeting")}, "handle"},
		{"note-empty-ref", []Entity{Contact("subject", Methods(MethodEmail)), CalendarEvent("meeting", "subject", StartedDaysAgo(4)), LinkedMeetingNote("note", "")}, "event handle"},
		{"note-dangling-ref", []Entity{Contact("subject", Methods(MethodEmail)), LinkedMeetingNote("note", "missing")}, "EARLIER"},
		{"note-contact-ref", []Entity{Contact("subject", Methods(MethodEmail)), LinkedMeetingNote("note", "subject")}, "calendar_event"},
		{"contact-ref-calendar-event", []Entity{Contact("subject", Methods(MethodEmail)), CalendarEvent("meeting", "subject", StartedDaysAgo(4)), MessageInteraction("x", "meeting", "email", AgoDays(1))}, "calendar_event"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			err := validateEntityOrder(tc.entities)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.contains)
			if tc.name == "note-contact-ref" {
				assert.Contains(t, err.Error(), "contact")
			}
		})
	}
}

func TestInteractionVocabulary_SessionUUIDDeterminism(t *testing.T) {
	prefix := "synth-test-"
	first := anarlogSessionUUID(prefix, "note-a")
	second := anarlogSessionUUID(prefix, "note-a")
	assert.Equal(t, first, second)
	assert.NotEqual(t, first, anarlogSessionUUID(prefix, "note-b"))
	assert.NotEqual(t, anarlogSessionEventPrefix(prefix), anarlogSessionEventPrefix("synth-other-"))

	var noteHandles []string
	for _, d := range Registered() {
		for _, e := range d.Entities {
			if p, ok := e.(*linkedMeetingNotePlan); ok {
				noteHandles = append(noteHandles, p.name)
			}
		}
	}
	noteIDs := make([]uuid.UUID, 0, len(noteHandles))
	for _, handle := range noteHandles {
		id := anarlogSessionUUID(prefix, handle)
		noteIDs = append(noteIDs, id)
		assert.True(t, strings.HasPrefix(id.String(), anarlogSessionEventPrefix(prefix)))
	}
	for i, id := range noteIDs {
		for j := 0; j < i; j++ {
			assert.NotEqual(t, noteIDs[j], id, "session uuid collision for registered note occurrence %q", noteHandles[i])
		}
	}
	// Pin the prescribed 96-bit prefix + 32-bit tail construction rather than
	// merely checking UUID formatting; a random tail would be a replay hazard.
	wantPrefix := sha256.Sum256([]byte("anarlog:" + prefix))
	wantTail := sha256.Sum256([]byte("anarlog-tail:" + prefix + ":note-a"))
	for i := 0; i < 12; i++ {
		assert.Equal(t, wantPrefix[i], first[i])
	}
	for i := 0; i < 4; i++ {
		assert.Equal(t, wantTail[i], first[12+i])
	}
}

func TestInteractionVocabulary_FilterValidation(t *testing.T) {
	cases := []struct {
		name    string
		entity  Entity
		wantErr string
	}{
		{name: "message synced ceiling", entity: MessageInteraction("message", "subject", "email", AgoDays(61)), wantErr: "1..60"},
		{name: "phone synced ceiling", entity: PhoneCallInteraction("phone", "subject", AgoDays(61)), wantErr: "1..60"},
		{name: "logged ceiling", entity: LoggedInteraction("logged", "subject", "manual", AgoDays(101)), wantErr: "1..100"},
		{name: "logged floor", entity: LoggedInteraction("logged", "subject", "manual", AgoDays(0)), wantErr: "1..100"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.entity.validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}

	accepted := []Entity{
		MessageInteraction("message", "subject", "email", AgoDays(60)),
		LoggedInteraction("logged-91", "subject", "manual", AgoDays(91)),
		LoggedInteraction("logged-100", "subject", "manual", AgoDays(100)),
	}
	for _, entity := range accepted {
		assert.NoError(t, entity.validate())
	}
}
