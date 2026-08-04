package service

import (
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveContactFromMethods_AmbiguityIsPerContactNotPerRow pins the
// distinction the matcher rests on: an identifier type may map to SEVERAL
// contact-method types, so one contact returns one row per method it holds the
// value under. Counting rows refuses to match a contact that is not in doubt —
// and because reconciliation mints the second method on a peer's FIRST
// successful match, a row count makes every peer match exactly once and strand
// from its second message onward.
func TestResolveContactFromMethods_AmbiguityIsPerContactNotPerRow(t *testing.T) {
	t.Parallel()

	alice := uuid.New()
	bob := uuid.New()
	row := func(contactID uuid.UUID, methodType string) repository.ContactMethodMatch {
		return repository.ContactMethodMatch{ID: uuid.New(), ContactID: contactID, Type: methodType}
	}

	t.Run("no rows is unmatched", func(t *testing.T) {
		t.Parallel()
		id, mt := resolveContactFromMethods("+15550000000", nil)
		assert.Nil(t, id)
		assert.Equal(t, repository.MatchTypeUnmatched, mt)
	})

	t.Run("one row matches exactly", func(t *testing.T) {
		t.Parallel()
		id, mt := resolveContactFromMethods("+15550000001", []repository.ContactMethodMatch{
			row(alice, "phone"),
		})
		require.NotNil(t, id)
		assert.Equal(t, alice, *id)
		assert.Equal(t, repository.MatchTypeExact, mt)
	})

	t.Run("several rows on ONE contact still match exactly", func(t *testing.T) {
		t.Parallel()
		// The shape reconciliation creates: the same number held as both a
		// whatsapp and a phone method by a single contact.
		id, mt := resolveContactFromMethods("+15550000002", []repository.ContactMethodMatch{
			row(alice, "whatsapp"),
			row(alice, "phone"),
		})
		require.NotNil(t, id)
		assert.Equal(t, alice, *id)
		assert.Equal(t, repository.MatchTypeExact, mt,
			"two methods on one contact is not a conflict about WHO the peer is")
	})

	t.Run("rows across DISTINCT contacts stay unmatched", func(t *testing.T) {
		t.Parallel()
		id, mt := resolveContactFromMethods("+15550000003", []repository.ContactMethodMatch{
			row(alice, "phone"),
			row(bob, "whatsapp"),
		})
		assert.Nil(t, id, "two contacts claiming one identifier is the real ambiguity")
		assert.Equal(t, repository.MatchTypeUnmatched, mt)
	})

	t.Run("many rows across distinct contacts stay unmatched", func(t *testing.T) {
		t.Parallel()
		id, mt := resolveContactFromMethods("+15550000004", []repository.ContactMethodMatch{
			row(alice, "phone"),
			row(alice, "whatsapp"),
			row(bob, "phone"),
		})
		assert.Nil(t, id)
		assert.Equal(t, repository.MatchTypeUnmatched, mt)
	})
}
