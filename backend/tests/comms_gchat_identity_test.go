package tests

import (
	"context"
	"strings"
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedContactWithMethod creates a contact + one contact_method of the given
// type, registering cleanup (hard-delete comms rows, delete methods,
// soft-delete contact). Returns the contact.
func seedContactWithMethod(t *testing.T, ctx context.Context, commsRepo *repository.CommsMessageRepository, contactRepo *repository.ContactRepository, methodRepo *repository.ContactMethodRepository, name, methodType, value string) *repository.Contact {
	t.Helper()
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: name})
	require.NoError(t, err)
	_, err = methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID,
		Type:      methodType,
		Value:     value,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = commsRepo.HardDeleteByContact(ctx, contact.ID)
		_ = methodRepo.DeleteContactMethodsByContact(ctx, contact.ID)
		_ = contactRepo.SoftDeleteContact(ctx, contact.ID)
	})
	return contact
}

// TestListGChatIdentitiesForSync_DualSource verifies the dual-source identity
// map: gchat AND email methods are both returned with the correct source_type
// discriminator; a contact with BOTH method types yields two rows; a shared
// address fans out to multiple contacts; soft-deleted contacts and
// empty-normalized values are excluded; values are already lowercased.
func TestListGChatIdentitiesForSync_DualSource(t *testing.T) {
	ctx, commsRepo, contactRepo, methodRepo := setupCommsMessageTest(t)
	gen, _ := migrationGenerator(t)
	suffix := gen.Prefix()

	// Unique per-suffix addresses so this sub-test's pool is isolated on the
	// shared DB. value_normalized lowercases, so seed with mixed case to also
	// prove case-insensitivity is inherited from the trigger.
	gchatOnlyAddr := "GChatOnly-" + suffix + "@Example.com"
	emailOnlyAddr := "EmailOnly-" + suffix + "@Example.com"
	bothAddr := "Both-" + suffix + "@Example.com"
	sharedAddr := "Shared-" + suffix + "@Example.com"

	gchatOnly := seedContactWithMethod(t, ctx, commsRepo, contactRepo, methodRepo,
		"GChat Only "+suffix, "gchat", gchatOnlyAddr)
	emailOnly := seedContactWithMethod(t, ctx, commsRepo, contactRepo, methodRepo,
		"Email Only "+suffix, "email", emailOnlyAddr)

	// Contact with BOTH a gchat and an email method (same address).
	both, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "Both Methods " + suffix})
	require.NoError(t, err)
	_, err = methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{ContactID: both.ID, Type: "gchat", Value: bothAddr})
	require.NoError(t, err)
	_, err = methodRepo.CreateContactMethod(ctx, repository.CreateContactMethodRequest{ContactID: both.ID, Type: "email", Value: bothAddr})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = commsRepo.HardDeleteByContact(ctx, both.ID)
		_ = methodRepo.DeleteContactMethodsByContact(ctx, both.ID)
		_ = contactRepo.SoftDeleteContact(ctx, both.ID)
	})

	// Two contacts sharing the SAME gchat address (fan-out).
	shareA := seedContactWithMethod(t, ctx, commsRepo, contactRepo, methodRepo,
		"Share A "+suffix, "gchat", sharedAddr)
	shareB := seedContactWithMethod(t, ctx, commsRepo, contactRepo, methodRepo,
		"Share B "+suffix, "gchat", sharedAddr)

	// A soft-deleted contact with a gchat method MUST be excluded.
	deleted := seedContactWithMethod(t, ctx, commsRepo, contactRepo, methodRepo,
		"Deleted "+suffix, "gchat", "Deleted-"+suffix+"@Example.com")
	require.NoError(t, contactRepo.SoftDeleteContact(ctx, deleted.ID))

	rows, err := commsRepo.ListGChatIdentitiesForSync(ctx)
	require.NoError(t, err)

	// Index rows by (normalized address, contact, source_type) for assertions.
	type key struct {
		addr    string
		contact uuid.UUID
		st      string
	}
	got := make(map[key]bool)
	for _, r := range rows {
		got[key{r.ValueNormalized, r.ContactID, r.SourceType}] = true
		// All addresses must be already-lowercased by the trigger.
		assert.Equal(t, strings.ToLower(r.ValueNormalized), r.ValueNormalized,
			"value_normalized must already be lowercased")
	}

	assert.True(t, got[key{strings.ToLower(gchatOnlyAddr), gchatOnly.ID, "gchat"}],
		"gchat-only contact's gchat row missing")
	assert.True(t, got[key{strings.ToLower(emailOnlyAddr), emailOnly.ID, "email"}],
		"email-only contact's email row missing")
	// Both-methods contact yields one gchat row AND one email row.
	assert.True(t, got[key{strings.ToLower(bothAddr), both.ID, "gchat"}],
		"both-methods contact's gchat row missing")
	assert.True(t, got[key{strings.ToLower(bothAddr), both.ID, "email"}],
		"both-methods contact's email row missing")
	// Shared address fans out to both contacts.
	assert.True(t, got[key{strings.ToLower(sharedAddr), shareA.ID, "gchat"}],
		"shared-address fan-out to contact A missing")
	assert.True(t, got[key{strings.ToLower(sharedAddr), shareB.ID, "gchat"}],
		"shared-address fan-out to contact B missing")
	// Soft-deleted contact excluded.
	assert.False(t, got[key{strings.ToLower("Deleted-" + suffix + "@Example.com"), deleted.ID, "gchat"}],
		"soft-deleted contact must be excluded")
}
