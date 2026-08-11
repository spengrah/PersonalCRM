package tests

import (
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCommsGroupSpaceFanout_SecondContactAlsoGetsInteraction pins the
// multi-contact chat fan-out regression: a single outbound gchat message
// fanned out to two matched contacts in the same space must aggregate into
// one interaction PER CONTACT. Before the claim/event key is contact-scoped,
// both contacts derive the identical (source, chatID, firstExternalID)
// sourceRef; the second contact to aggregate claims its staging row but its
// event publish silently no-ops against the first contact's already-published
// event (event.source_id is globally unique), so the recorder job never runs
// for it and its row is stranded processed_at IS NULL forever.
//
// spec: ING-026.fanout-claim-and-event-key-is-per-contact
func TestCommsGroupSpaceFanout_SecondContactAlsoGetsInteraction(t *testing.T) {
	t.Parallel()
	e := setupGChatEngineTest(t)
	gen, _ := migrationGenerator(t)
	suffix := gen.Prefix()

	contactA := e.newGChatContact(t, "GChat Fanout A "+suffix)
	contactB := e.newGChatContact(t, "GChat Fanout B "+suffix)
	space := "spaces/FANOUT-" + suffix
	externalID := "gchat-fanout1-" + suffix
	sentAt := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)

	// One outbound message, fanned out as two comms_message rows: identical
	// source/external_id/thread_id/sent_at/direction, different
	// matched_contact_id — mirrors what the gchat provider writes for a
	// group-space message with more than one matched participant.
	rowA := e.seedGChatRow(t, contactA.ID, space, externalID, repository.InteractionDirectionOutbound, sentAt)
	rowB := e.seedGChatRow(t, contactB.ID, space, externalID, repository.InteractionDirectionOutbound, sentAt)

	require.NoError(t, e.engine.AggregateForContact(e.ctx, contactA.ID, space))
	require.NoError(t, e.engine.AggregateForContact(e.ctx, contactB.ID, space))

	interactionsA := waitForInteractionCountExact(t, e.ctx, e.interactionRepo, contactA.ID, 1, defaultInteractionWaitTimeout)
	require.Len(t, interactionsA, 1)
	// Required failure mode: pre-fix, contact B never gets an interaction —
	// this call times out with 0 interactions observed.
	interactionsB := waitForInteractionCountExact(t, e.ctx, e.interactionRepo, contactB.ID, 1, defaultInteractionWaitTimeout)
	require.Len(t, interactionsB, 1)

	waitForGChatRowsProcessed(t, e, []uuid.UUID{rowA.ID}, interactionsA[0].ID)
	waitForGChatRowsProcessed(t, e, []uuid.UUID{rowB.ID}, interactionsB[0].ID)

	assert.NotEqual(t, interactionsA[0].ID, interactionsB[0].ID, "each contact gets its own interaction")

	// interaction.source_ref stays contact-free and SHARED between the two
	// contacts' interactions — proves the fix split the claim/event key from
	// interaction.source_ref rather than mangling the latter to dodge the
	// collision (that would break the (contact_id, source, source_ref) dedup
	// index this same value is used for).
	require.NotNil(t, interactionsA[0].SourceRef)
	require.NotNil(t, interactionsB[0].SourceRef)
	assert.Equal(t, *interactionsA[0].SourceRef, *interactionsB[0].SourceRef)
	assert.Equal(t, "gchat:"+space+":"+externalID, *interactionsA[0].SourceRef)

	// Idempotency: a second aggregation pass for both contacts adds no
	// further interactions — the staging rows are already processed.
	require.NoError(t, e.engine.AggregateForContact(e.ctx, contactA.ID, space))
	require.NoError(t, e.engine.AggregateForContact(e.ctx, contactB.ID, space))
	time.Sleep(500 * time.Millisecond) // settle any (unexpected) async write
	countA, err := e.interactionRepo.CountContactInteractions(e.ctx, contactA.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), countA, "re-aggregating contact A must not create a second interaction")
	countB, err := e.interactionRepo.CountContactInteractions(e.ctx, contactB.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), countB, "re-aggregating contact B must not create a second interaction")
}
