package declare

import (
	"testing"
	"time"

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
