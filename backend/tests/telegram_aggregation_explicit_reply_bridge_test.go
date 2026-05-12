package tests

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAggregation_IncrementalExplicitReplyBridge_CrossBatch exercises
// the cross-batch tryExplicitReplyBridge path (lines 277-307 of the
// pre-refactor aggregation.go) by setting up an outbound staging row
// that's already been processed and linked to an existing outbound
// interaction, then an inbound staging row whose reply_to_msg_id
// points to that processed outbound row.
//
// The cross-batch path is what the shared engine's Message.InteractionID
// plumbing exists to enable. The existing
// TestAggregation_IncrementalExplicitReplyBridge integration test
// covers a similar scenario via a single aggregation pass that marks
// the outbound interaction during the same flow; this test forces a
// >48h gap (no time-bridge possible) and verifies that the in-burst
// explicit-reply branch in resolveSessions can NOT see the prior
// outbound (it's not in the unprocessed batch), so the cross-batch
// branch in tryExplicitReplyBridge is the only path that can promote
// the interaction.
//
// Stripe-based IDs (testStripe=2) keep this disjoint from the
// inbound-coalesce (=1) and source-filter (=3) tests.
func TestAggregation_IncrementalExplicitReplyBridge_CrossBatch(t *testing.T) {
	messageRepo, interactionRepo, contactRepo, _, engine, database := setupAggregationTest(t)
	ctx := context.Background()
	eventRepo := repository.NewEventRepository(database.Queries)

	const testStripe = 2
	seed := uint64((accelerated.GetCurrentTime().UnixNano() & 0x1FFFFF) << 11)
	// nolint:gosec // test-only seed; math/rand is sufficient for ID disambiguation
	seed |= uint64(rand.Int31n(2048))

	chatID := int64(testStripe)<<32 | int64(seed)
	msgIDBase := int32(testStripe*10_000_000) + int32(seed%1_000_000)
	peerUserID := int64(testStripe*70_000_000) + int64(seed%1_000_000)
	sourceRefPrefix := fmt.Sprintf("tg:%d:%%", chatID)
	outboundSourceRef := fmt.Sprintf("tg:%d:%d", chatID, msgIDBase)

	t.Cleanup(func() {
		_ = messageRepo.HardDeleteByChatIDRange(ctx, chatID, chatID)
		_ = interactionRepo.HardDeleteInteractionsBySourceRefPrefix(ctx, repository.InteractionSourceTelegram, sourceRefPrefix)
		// Event rows are dedup-keyed on (source, source_id); purge
		// per-run envelopes via the test-only sqlc-backed wrapper so a
		// re-run does not collide on the partial unique.
		_ = eventRepo.HardDeleteEventsBySourceAndSourceIDPrefix(ctx, repository.InteractionSourceTelegram, sourceRefPrefix)
	})

	contact := createTestContact(t, contactRepo, fmt.Sprintf("Explicit Reply Bridge %d", seed))
	t.Cleanup(func() { _ = contactRepo.SoftDeleteContact(ctx, contact.ID) })

	// 90h gap > 48h reply-bridge window — time-based bridging will NOT
	// fire. Only the cross-batch explicit reply-bridge can promote.
	t0 := accelerated.GetCurrentTime().Add(-100 * time.Hour).Truncate(time.Microsecond)
	replyAt := t0.Add(90 * time.Hour)

	// Create the outbound interaction directly (so we can capture its
	// ID and link the staging row deterministically).
	outboundIA, err := interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourceTelegram,
		SourceRef:  &outboundSourceRef,
		OccurredAt: t0,
		Direction:  repository.InteractionDirectionOutbound,
	})
	require.NoError(t, err)
	outboundInteractionID := outboundIA.ID

	// Insert the outbound staging row and mark it processed pointing
	// at the outbound interaction. Now it is NOT in the unprocessed
	// batch — only the inbound reply will appear in the next
	// AggregateForContact call, forcing the cross-batch
	// tryExplicitReplyBridge path.
	outboundMsg := insertTestMessage(t, messageRepo, msgIDBase, chatID, true /*outgoing*/, t0, &contact.ID, peerUserID)
	require.NoError(t, messageRepo.MarkMessagesProcessed(ctx, []uuid.UUID{outboundMsg.ID}, outboundInteractionID))

	// Insert the inbound staging row with reply_to_msg_id pointing to
	// the outbound's TelegramMessageID, >48h after the outbound.
	replyToID := msgIDBase
	text := "explicit reply"
	_, err = messageRepo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: msgIDBase + 1,
		TelegramChatID:    chatID,
		ChatType:          "private",
		MessageText:       &text,
		MessageType:       "text",
		SentAt:            replyAt,
		IsOutgoing:        false,
		PeerUserID:        ptrInt64(peerUserID),
		ReplyToMsgID:      &replyToID,
	})
	require.NoError(t, err)
	require.NoError(t, messageRepo.UpdateMessageContact(ctx, peerUserID, contact.ID))

	preCount, err := interactionRepo.CountContactInteractions(ctx, contact.ID)
	require.NoError(t, err)

	// Aggregate. The outbound staging row is processed and invisible
	// to the unprocessed list. The inbound row, alone in the batch,
	// forms an inbound session whose ReplyTargetID matches the
	// processed outbound's ExternalID via the shared engine's
	// tryExplicitReplyBridge path — which reads Message.InteractionID
	// off the referenced row to find the existing outbound interaction
	// and promote it.
	require.NoError(t, engine.AggregateForContact(ctx, contact.ID, chatID))

	// Assert: still exactly preCount interactions, the outbound
	// promoted to mutual, occurred_at at replyAt.
	postCount, err := interactionRepo.CountContactInteractions(ctx, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, preCount, postCount, "explicit reply bridge must promote, not create")

	promoted, err := interactionRepo.GetInteraction(ctx, outboundInteractionID)
	require.NoError(t, err)
	assert.Equal(t, repository.InteractionDirectionMutual, promoted.Direction,
		"explicit reply bridge must promote outbound → mutual")
	assert.Equal(t, replyAt.UTC(), promoted.OccurredAt.UTC(),
		"explicit reply bridge must advance occurred_at to the reply timestamp")

	// The inbound staging row should be processed and linked to the
	// promoted interaction.
	inboundRow, err := messageRepo.GetMessage(ctx, chatID, msgIDBase+1)
	require.NoError(t, err)
	require.NotNil(t, inboundRow.ProcessedAt, "explicit reply bridge must mark inbound processed")
	require.NotNil(t, inboundRow.InteractionID, "explicit reply bridge must link inbound to outbound interaction")
	assert.Equal(t, outboundInteractionID, *inboundRow.InteractionID)

	// And no message.received event was published for the inbound's
	// would-be source_ref (the promote path consumes the message
	// rather than going through the publisher).
	wouldBeRef := fmt.Sprintf("tg:%d:%d", chatID, msgIDBase+1)
	_, err = eventRepo.FindEventBySource(ctx, repository.InteractionSourceTelegram, wouldBeRef)
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrNotFound), "expected ErrNotFound for the would-be inbound event, got %v", err)
}
