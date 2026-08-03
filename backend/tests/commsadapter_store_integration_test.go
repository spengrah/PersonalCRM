// Live-store coverage for backend/internal/messaging/commsadapter's
// StoreAdapter. The package's own unit tests cover the row projection and
// interface conformance but cannot reach the database, so two behaviours are
// untestable there and are the ones that break silently:
//
//   - the store must pass ITS OWN source to every *ForSource repository call.
//     A store constructed for one source that reads another's rows would still
//     satisfy the interface, still compile, and still pass the unit suite; it
//     would simply aggregate the wrong source's messages. ListUnprocessedContactIDs
//     is the entry point AggregateAll drives, so a wrong source there silently
//     changes which contacts a whole sweep touches.
//   - GetMessageByReplyTarget must translate the repository's db.ErrNotFound
//     into (zero, false, nil). Returning the error instead would abort the
//     explicit-reply bridge for every non-reply message rather than falling
//     through to the time-window bridge.
package tests

import (
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/messaging/commsadapter"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCommsAdapterStore_ReadsAreScopedToItsOwnSource seeds one gchat row and one
// email row for the same contact, then proves a gchat-pinned StoreAdapter sees
// exactly the gchat row on all three list paths and an email-pinned one sees
// exactly the email row. A store that ignored its source, or was handed the
// wrong one, fails here.
func TestCommsAdapterStore_ReadsAreScopedToItsOwnSource(t *testing.T) {
	t.Parallel()
	ctx, commsRepo, contactRepo, _ := setupCommsMessageTest(t)
	gen, _ := migrationGenerator(t)
	suffix := gen.Prefix()

	contact := newEmailContact(t, ctx, commsRepo, contactRepo, "Store Scope "+suffix)
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)

	space := "spaces/STORE-" + suffix
	gchatRow := upsertGChatRow(t, ctx, commsRepo, contact.ID,
		space, "gchat-store-"+suffix, repository.InteractionDirectionInbound, base)

	emailThread := "thread-store-" + suffix
	emailBody := "email body"
	emailRow, err := commsRepo.UpsertMessage(ctx, repository.UpsertCommsMessageParams{
		Source:           repository.InteractionSourceEmail,
		ExternalID:       "email-store-" + suffix,
		ThreadID:         &emailThread,
		Body:             &emailBody,
		Direction:        repository.InteractionDirectionInbound,
		SentAt:           base,
		MatchedContactID: contact.ID,
	})
	require.NoError(t, err)

	gchatStore := commsadapter.NewStore(commsRepo, repository.InteractionSourceGChat)
	emailStore := commsadapter.NewStore(commsRepo, repository.InteractionSourceEmail)

	// AggregateAll's entry point: the contact appears for both sources here
	// (it has an unprocessed row in each), so the discriminating assertion is
	// the per-contact read below — this one only proves the call reaches the DB.
	gchatContacts, err := gchatStore.ListUnprocessedContactIDs(ctx)
	require.NoError(t, err)
	assert.Contains(t, gchatContacts, contact.ID)

	// Per-contact reads are where a wrong source shows up as wrong rows.
	gchatMsgs, err := gchatStore.ListUnprocessedByContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Len(t, gchatMsgs, 1, "gchat store must see exactly its own row")
	assert.Equal(t, gchatRow.ID, gchatMsgs[0].ID)
	assert.Equal(t, space, gchatMsgs[0].ChatID, "ChatID is projected from thread_id")
	assert.False(t, gchatMsgs[0].IsOutgoing)

	emailMsgs, err := emailStore.ListUnprocessedByContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Len(t, emailMsgs, 1, "email store must see exactly its own row")
	assert.Equal(t, emailRow.ID, emailMsgs[0].ID)

	// Chat-scoped read: the gchat store finds its row in the space; the email
	// store finds nothing there, because the row it could match is not its source.
	inSpace, err := gchatStore.ListUnprocessedByContactAndChat(ctx, contact.ID, space)
	require.NoError(t, err)
	require.Len(t, inSpace, 1)
	assert.Equal(t, gchatRow.ID, inSpace[0].ID)

	emailInSpace, err := emailStore.ListUnprocessedByContactAndChat(ctx, contact.ID, space)
	require.NoError(t, err)
	assert.Empty(t, emailInSpace, "email store must not read the gchat row's chat")
}

// TestCommsAdapterStore_GetMessageByReplyTargetHitAndMiss covers the store's
// error translation: a hit returns (msg, true, nil); a miss returns
// (zero, false, nil) with NO error, because db.ErrNotFound is the expected
// "this message is not a reply to anything we staged" case rather than a
// failure. A store pinned to another source must also miss.
func TestCommsAdapterStore_GetMessageByReplyTargetHitAndMiss(t *testing.T) {
	t.Parallel()
	ctx, commsRepo, contactRepo, _ := setupCommsMessageTest(t)
	gen, _ := migrationGenerator(t)
	suffix := gen.Prefix()

	contact := newEmailContact(t, ctx, commsRepo, contactRepo, "Store Reply "+suffix)
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)

	space := "spaces/STORERT-" + suffix
	targetExtID := "gchat-storert-" + suffix
	target := upsertGChatRow(t, ctx, commsRepo, contact.ID,
		space, targetExtID, repository.InteractionDirectionInbound, base)

	gchatStore := commsadapter.NewStore(commsRepo, repository.InteractionSourceGChat)

	t.Run("hit resolves the row within (source, contact, chat) scope", func(t *testing.T) {
		got, found, err := gchatStore.GetMessageByReplyTarget(ctx, contact.ID, space, targetExtID)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, target.ID, got.ID)
		assert.Equal(t, targetExtID, got.ExternalID)
		assert.Equal(t, space, got.ChatID)
	})

	t.Run("miss returns not-found without an error", func(t *testing.T) {
		got, found, err := gchatStore.GetMessageByReplyTarget(ctx, contact.ID, space, "no-such-id-"+suffix)
		require.NoError(t, err, "db.ErrNotFound must be translated, not propagated")
		assert.False(t, found)
		assert.Equal(t, uuid.Nil, got.ID)
	})

	t.Run("another chat in the same source misses", func(t *testing.T) {
		_, found, err := gchatStore.GetMessageByReplyTarget(ctx, contact.ID, "spaces/OTHER-"+suffix, targetExtID)
		require.NoError(t, err)
		assert.False(t, found)
	})

	t.Run("a store pinned to another source misses", func(t *testing.T) {
		emailStore := commsadapter.NewStore(commsRepo, repository.InteractionSourceEmail)
		_, found, err := emailStore.GetMessageByReplyTarget(ctx, contact.ID, space, targetExtID)
		require.NoError(t, err)
		assert.False(t, found, "the row belongs to gchat; an email-pinned store must not resolve it")
	})
}

// TestGChatEngine_AggregateAllProducesInteraction drives the sweep entry point
// the other engine tests skip: AggregateAll discovers its contacts through the
// store's ListUnprocessedContactIDs rather than being handed one. It runs on an
// ephemeral database clone, so "all contacts" is exactly this test's contact.
func TestGChatEngine_AggregateAllProducesInteraction(t *testing.T) {
	t.Parallel()
	e := setupGChatEngineTest(t)
	gen, _ := migrationGenerator(t)
	suffix := gen.Prefix()

	contact := e.newGChatContact(t, "GChat AggregateAll "+suffix)
	space := "spaces/AGGALL-" + suffix
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)

	r1 := e.seedGChatRow(t, contact.ID, space, "gchat-aggall1-"+suffix, repository.InteractionDirectionInbound, base)
	r2 := e.seedGChatRow(t, contact.ID, space, "gchat-aggall2-"+suffix, repository.InteractionDirectionInbound, base.Add(10*time.Minute))

	require.NoError(t, e.engine.AggregateAll(e.ctx))

	interactions := waitForInteractionCountExact(t, e.ctx, e.interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout)
	require.Len(t, interactions, 1)
	assert.Equal(t, repository.InteractionSourceGChat, interactions[0].Source)
	assert.Equal(t, repository.InteractionDirectionInbound, interactions[0].Direction)
	require.NotNil(t, interactions[0].Description)
	assert.Equal(t, "GChat response (2 messages)", *interactions[0].Description)

	waitForGChatRowsProcessed(t, e, []uuid.UUID{r1.ID, r2.ID}, interactions[0].ID)
}
