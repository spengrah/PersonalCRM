package service

import (
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/assert"
)

func sug(t, v string) repository.PendingMethodSuggestion {
	return repository.PendingMethodSuggestion{Type: t, Value: v}
}

func TestAllowedActionsForSource(t *testing.T) {
	tests := []struct {
		source string
		want   []string
	}{
		{"gcontacts", []string{"import", "link", "ignore"}},
		{"icloud_contacts", []string{"import", "link", "ignore"}},
		{"gcal_attendee", []string{"import", "link", "ignore"}},
		{"telegram", []string{"import", "link", "ignore"}},
		{"gmail_correspondence", []string{"link", "ignore"}},
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			assert.Equal(t, tt.want, AllowedActionsForSource(tt.source))
		})
	}
}

func TestIsLinkOnlySource(t *testing.T) {
	assert.True(t, IsLinkOnlySource("gmail_correspondence"))
	assert.False(t, IsLinkOnlySource("gcontacts"))
	assert.False(t, IsLinkOnlySource("icloud_contacts"))
}

func TestRequestedKeySet(t *testing.T) {
	// Empty request → nil ("all").
	assert.Nil(t, requestedKeySet(nil))
	assert.Nil(t, requestedKeySet([]repository.PendingMethodSuggestion{}))

	set := requestedKeySet([]repository.PendingMethodSuggestion{sug("email", "a@example.test")})
	assert.True(t, set[methodDedupKey("email", "a@example.test")])
	assert.False(t, set[methodDedupKey("email", "b@example.test")])
}

func TestSubtractDismissed(t *testing.T) {
	pending := []repository.PendingMethodSuggestion{
		sug("email", "a@example.test"),
		sug("phone", "+15551230000"),
	}
	dismissed := []repository.PendingMethodSuggestion{sug("email", "a@example.test")}

	got := subtractDismissed(pending, dismissed)
	assert.Len(t, got, 1)
	assert.Equal(t, "phone", got[0].Type)
}

func TestUnionSuggestions_AppendOnlyDedup(t *testing.T) {
	base := []repository.PendingMethodSuggestion{sug("email", "a@example.test")}
	additions := []repository.PendingMethodSuggestion{
		sug("email", "a@example.test"), // duplicate of base
		sug("phone", "+15551230000"),   // new
	}
	got := unionSuggestions(base, additions)
	assert.Len(t, got, 2)
	assert.Equal(t, "a@example.test", got[0].Value)
	assert.Equal(t, "phone", got[1].Type)
}

func TestSuggestionKeySet_DedupKeyNormalizes(t *testing.T) {
	// methodDedupKey lowercases email via identity.Normalize, so two
	// case-variant emails collide.
	set := suggestionKeySet([]repository.PendingMethodSuggestion{sug("email", "A@Example.Test")})
	assert.True(t, set[methodDedupKey("email", "a@example.test")])
}

func TestExternalMethodKeySet(t *testing.T) {
	external := &repository.ExternalContact{
		Source: "gcontacts",
		Emails: []repository.EmailEntry{{Value: "a@example.test"}},
		Phones: []repository.PhoneEntry{{Value: "+15551230000"}},
	}
	set := externalMethodKeySet(external)
	assert.True(t, set[methodDedupKey("email", "a@example.test")])
	assert.True(t, set[methodDedupKey("phone", "+15551230000")])
	assert.False(t, set[methodDedupKey("email", "z@example.test")])
}

func TestCandidateSortName(t *testing.T) {
	display := "Contact A"
	first := "First"
	last := "Last"

	assert.Equal(t, "Contact A", candidateSortName(&repository.ExternalContact{DisplayName: &display}))
	assert.Equal(t, "First Last", candidateSortName(&repository.ExternalContact{FirstName: &first, LastName: &last}))
	assert.Equal(t, "First", candidateSortName(&repository.ExternalContact{FirstName: &first}))

	// Telegram username fallback strips the stored leading '@'.
	tg := &repository.ExternalContact{Source: "telegram", Metadata: map[string]any{"username": "@handle"}}
	assert.Equal(t, "handle", candidateSortName(tg))

	// No identifying fields → empty (sorts last).
	assert.Equal(t, "", candidateSortName(&repository.ExternalContact{Source: "gcontacts"}))
}

func TestValidateRequestedMethods(t *testing.T) {
	assert.NoError(t, validateRequestedMethods(nil))
	assert.NoError(t, validateRequestedMethods([]repository.PendingMethodSuggestion{sug("email", "a@example.test")}))

	assert.ErrorIs(t, validateRequestedMethods([]repository.PendingMethodSuggestion{sug("", "a@example.test")}), ErrSuggestionInvalidMethod)
	assert.ErrorIs(t, validateRequestedMethods([]repository.PendingMethodSuggestion{sug("email", "  ")}), ErrSuggestionInvalidMethod)
}

func TestSelectionsForConfirmed_MapsBackToOriginal(t *testing.T) {
	svc := &SuggestionService{}
	external := &repository.ExternalContact{
		Source: "gcontacts",
		Emails: []repository.EmailEntry{{Value: "Mixed@Example.Test"}},
	}
	// Confirmed entry carries the NORMALIZED value (as stored in pending);
	// selectionsForConfirmed maps it back to the external row's original.
	confirmed := []repository.PendingMethodSuggestion{sug("email", "mixed@example.test")}
	selections := svc.selectionsForConfirmed(external, confirmed)
	assert.Len(t, selections, 1)
	assert.Equal(t, "Mixed@Example.Test", selections[0].OriginalValue)
	assert.Equal(t, "email", selections[0].Type)
}

func TestSelectionsForConfirmed_SkipsUnappliable(t *testing.T) {
	svc := &SuggestionService{}
	external := &repository.ExternalContact{
		Source: "gcontacts",
		Emails: []repository.EmailEntry{{Value: "a@example.test"}},
	}
	// A confirmed key not present on the external row is skipped.
	confirmed := []repository.PendingMethodSuggestion{sug("email", "gone@example.test")}
	assert.Empty(t, svc.selectionsForConfirmed(external, confirmed))
}
