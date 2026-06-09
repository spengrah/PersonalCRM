package tests

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upsertGChatRow seeds one comms_message(source='gchat') row for a contact in a
// space. Direction defaults to inbound. Returns the persisted row.
func upsertGChatRow(t *testing.T, ctx context.Context, commsRepo *repository.CommsMessageRepository, contactID uuid.UUID, space, externalID, direction string, sentAt time.Time) *repository.CommsMessage {
	t.Helper()
	thread := space
	body := "gchat body"
	msg, err := commsRepo.UpsertMessage(ctx, repository.UpsertCommsMessageParams{
		Source:           repository.InteractionSourceGChat,
		ExternalID:       externalID,
		ThreadID:         &thread,
		Body:             &body,
		Direction:        direction,
		SentAt:           sentAt,
		MatchedContactID: contactID,
	})
	require.NoError(t, err)
	return msg
}

// TestCommsSourceParameterizedQueries_IsolateSources proves the @source filter
// isolates rows on the shared comms_message table: gchat-source readers see
// only gchat rows (email rows are invisible) and vice-versa.
func TestCommsSourceParameterizedQueries_IsolateSources(t *testing.T) {
	t.Parallel()
	ctx, commsRepo, contactRepo, _ := setupCommsMessageTest(t)
	gen, _ := migrationGenerator(t)
	suffix := gen.Prefix()

	contact := newEmailContact(t, ctx, commsRepo, contactRepo, "Source Isolation "+suffix)
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)

	// One gchat row + one email row for the SAME contact.
	gchatRow := upsertGChatRow(t, ctx, commsRepo, contact.ID,
		"spaces/ISO-"+suffix, "gchat-iso-"+suffix, repository.InteractionDirectionInbound, base)

	emailThread := "thread-iso-" + suffix
	emailBody := "email body"
	_, err := commsRepo.UpsertMessage(ctx, repository.UpsertCommsMessageParams{
		Source:           repository.InteractionSourceEmail,
		ExternalID:       "email-iso-" + suffix,
		ThreadID:         &emailThread,
		Body:             &emailBody,
		Direction:        repository.InteractionDirectionInbound,
		SentAt:           base,
		MatchedContactID: contact.ID,
	})
	require.NoError(t, err)

	// gchat readers see only the gchat row.
	gchatRows, err := commsRepo.ListUnprocessedByContactForSource(ctx, repository.InteractionSourceGChat, contact.ID)
	require.NoError(t, err)
	require.Len(t, gchatRows, 1, "gchat reader must see exactly the one gchat row")
	assert.Equal(t, gchatRow.ID, gchatRows[0].ID)

	// email readers see only the email row.
	emailRows, err := commsRepo.ListUnprocessedByContactForSource(ctx, repository.InteractionSourceEmail, contact.ID)
	require.NoError(t, err)
	require.Len(t, emailRows, 1, "email reader must see exactly the one email row")
	assert.NotEqual(t, gchatRow.ID, emailRows[0].ID)

	// Contact-ID listers are likewise source-scoped.
	gchatContacts, err := commsRepo.ListUnprocessedContactIDsForSource(ctx, repository.InteractionSourceGChat)
	require.NoError(t, err)
	assert.Contains(t, gchatContacts, contact.ID)

	// Chat-lister returns the space for the gchat source only.
	chats, err := commsRepo.ListUnprocessedChatsByContactForSource(ctx, repository.InteractionSourceGChat, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"spaces/ISO-" + suffix}, chats)
}

// TestGetMessageByReplyTargetForSource verifies the reply-target getter:
// resolves a row by its own external_id within the (source, contact, space)
// scope, returns db.ErrNotFound for a missing target, is space-scoped (a target
// in a different space is not found), and is CONTACT-scoped — the same address
// fanned out to two contacts must resolve to the querying contact's own row,
// never the other contact's.
func TestGetMessageByReplyTargetForSource(t *testing.T) {
	t.Parallel()
	ctx, commsRepo, contactRepo, methodRepo := setupCommsMessageTest(t)
	gen, _ := migrationGenerator(t)
	suffix := gen.Prefix()

	contact := newEmailContact(t, ctx, commsRepo, contactRepo, "Reply Target "+suffix)
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)

	spaceA := "spaces/RTA-" + suffix
	spaceB := "spaces/RTB-" + suffix
	extID1 := "gchat-rt1-" + suffix
	row1 := upsertGChatRow(t, ctx, commsRepo, contact.ID, spaceA, extID1, repository.InteractionDirectionInbound, base)
	_ = upsertGChatRow(t, ctx, commsRepo, contact.ID, spaceA, "gchat-rt2-"+suffix, repository.InteractionDirectionInbound, base.Add(time.Minute))

	// Hit: row1 resolved by its external_id within spaceA for this contact.
	got, err := commsRepo.GetMessageByReplyTargetForSource(ctx, repository.InteractionSourceGChat, contact.ID, spaceA, extID1)
	require.NoError(t, err)
	assert.Equal(t, row1.ID, got.ID)

	// Miss: non-existent target.
	_, err = commsRepo.GetMessageByReplyTargetForSource(ctx, repository.InteractionSourceGChat, contact.ID, spaceA, "no-such-id-"+suffix)
	assert.ErrorIs(t, err, db.ErrNotFound)

	// Space-scoped: the same external_id looked up in a DIFFERENT space is not found.
	_, err = commsRepo.GetMessageByReplyTargetForSource(ctx, repository.InteractionSourceGChat, contact.ID, spaceB, extID1)
	assert.ErrorIs(t, err, db.ErrNotFound)

	// Contact-scoped (fanout defense): a SECOND contact with the SAME address
	// gets its own fanned-out row for the SAME (space, external_id). Each
	// contact's lookup must return its OWN row, never the other's. An unscoped
	// query would non-deterministically return one of the two and could
	// cross-link interactions.
	_ = methodRepo // contact_method not needed; fanout is modeled directly via per-contact rows
	other := newEmailContact(t, ctx, commsRepo, contactRepo, "Reply Target Other "+suffix)
	otherRow := upsertGChatRow(t, ctx, commsRepo, other.ID, spaceA, extID1, repository.InteractionDirectionInbound, base)

	gotSelf, err := commsRepo.GetMessageByReplyTargetForSource(ctx, repository.InteractionSourceGChat, contact.ID, spaceA, extID1)
	require.NoError(t, err)
	assert.Equal(t, row1.ID, gotSelf.ID, "contact's lookup must return its own row")

	gotOther, err := commsRepo.GetMessageByReplyTargetForSource(ctx, repository.InteractionSourceGChat, other.ID, spaceA, extID1)
	require.NoError(t, err)
	assert.Equal(t, otherRow.ID, gotOther.ID, "other contact's lookup must return the other contact's row")
	assert.NotEqual(t, row1.ID, otherRow.ID, "the two contacts' fanned-out rows are distinct")
}
