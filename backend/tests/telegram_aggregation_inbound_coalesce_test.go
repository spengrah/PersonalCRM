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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAggregation_IncrementalInboundCoalesce covers the
// same-direction inbound-extend branch of AggregateForContact (the old
// aggregation.go lines 247-263). The existing
// TestAggregation_IncrementalCoalescing covers outbound-extend; this
// test fills the inbound-extend gap so "semantics preserved verbatim"
// holds for both directions of same-direction coalescing.
//
// Stripe-based IDs (testStripe=1) keep chat IDs disjoint from the
// other two new integration tests (explicit-reply-bridge=2, source-
// filter=3) and from the existing aggregation tests (chat IDs 100-303).
// Cleanup uses the new sqlc-backed HardDelete helpers per core.md rule
// 2 (no raw SQL in Go).
func TestAggregation_IncrementalInboundCoalesce(t *testing.T) {
	t.Parallel()
	messageRepo, interactionRepo, _, _, engine, database := setupAggregationTest(t)
	ctx := context.Background()
	eventRepo := repository.NewEventRepository(database.Queries)

	const testStripe = 1
	seed := uint64((accelerated.GetCurrentTime().UnixNano() & 0x1FFFFF) << 11)
	// nolint:gosec // test-only seed; math/rand is sufficient for ID disambiguation
	seed |= uint64(rand.Int31n(2048))

	chatID := int64(testStripe)<<32 | int64(seed)
	msgIDBase := int32(testStripe*10_000_000) + int32(seed%1_000_000)
	peerUserID := int64(testStripe*70_000_000) + int64(seed%1_000_000)
	sourceRefPrefix := fmt.Sprintf("tg:%d:%%", chatID)
	baselineSourceRef := fmt.Sprintf("tg:%d:%d", chatID, msgIDBase)

	t.Cleanup(func() {
		_ = messageRepo.HardDeleteByChatIDRange(ctx, chatID, chatID)
		_ = interactionRepo.HardDeleteInteractionsBySourceRefPrefix(ctx, repository.InteractionSourceTelegram, sourceRefPrefix)
		// Event rows are dedup-keyed on (source, source_id). Clean any
		// envelopes the create-path may have emitted under this test's
		// source_ref prefix so a re-run does not collide on the partial
		// unique. Uses the test-only sqlc-backed wrapper to comply with
		// the "no raw SQL in Go" rule.
		_ = eventRepo.HardDeleteEventsBySourceAndSourceIDPrefix(ctx, repository.InteractionSourceTelegram, sourceRefPrefix)
	})

	gen, _ := migrationGenerator(t)
	contact, contactCleanup := seedMigrationContact(ctx, t, database, gen)
	t.Cleanup(contactCleanup)

	t0 := accelerated.GetCurrentTime().Add(-2 * time.Hour).Truncate(time.Microsecond)

	// Seed a baseline inbound interaction directly. Use the
	// repository's CreateInteraction so we can capture the ID for
	// assertions, and so we don't rely on the publisher path here.
	baseline, err := interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourceTelegram,
		SourceRef:  &baselineSourceRef,
		OccurredAt: t0,
		Direction:  repository.InteractionDirectionInbound,
	})
	require.NoError(t, err)
	baselineID := baseline.ID

	preCount, err := interactionRepo.CountContactInteractions(ctx, contact.ID)
	require.NoError(t, err)

	// Insert an unprocessed inbound staging row in the same chat,
	// inside the burst window (30 min later).
	extendTime := t0.Add(30 * time.Minute)
	insertTestMessage(t, messageRepo, msgIDBase+1, chatID, false /*outgoing*/, extendTime, &contact.ID, peerUserID)

	// Aggregate — the engine should hit the inbound-extend branch and
	// call ExtendInteraction on the baseline.
	err = engine.AggregateForContact(ctx, contact.ID, chatID)
	require.NoError(t, err)

	// Assert: still exactly preCount interactions (no new row created),
	// baseline's occurred_at advanced to extendTime, description
	// reflects the inbound extend.
	postCount, err := interactionRepo.CountContactInteractions(ctx, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, preCount, postCount, "inbound extend must not create a new interaction")

	got, err := interactionRepo.GetInteraction(ctx, baselineID)
	require.NoError(t, err)
	assert.Equal(t, repository.InteractionDirectionInbound, got.Direction,
		"inbound extend must not change direction")
	assert.Equal(t, extendTime.UTC(), got.OccurredAt.UTC(),
		"inbound extend must advance occurred_at to the extend timestamp")
	require.NotNil(t, got.Description)
	assert.Equal(t, "Telegram response (1 messages)", *got.Description,
		"description must follow the preserved Telegram format")

	// The staging row should now be processed and linked to the
	// baseline interaction.
	unprocessed, err := messageRepo.ListUnprocessedByContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Empty(t, unprocessed, "extend path must mark staging rows processed")

	stagedRow, err := messageRepo.GetMessage(ctx, chatID, msgIDBase+1)
	require.NoError(t, err)
	require.NotNil(t, stagedRow.ProcessedAt, "extend path must set processed_at")
	require.NotNil(t, stagedRow.InteractionID, "extend path must set interaction_id")
	assert.Equal(t, baselineID, *stagedRow.InteractionID,
		"extend path must link the staging row to the baseline interaction")

	// The extend path must NOT publish a message.received event for
	// the extending message's claim/event key (would-be source_ref is
	// based on msgIDBase+1, scoped to this contact the same way the
	// engine derives its claim key). Use FindEventBySource for a
	// targeted query — avoids cross-query counts on the shared DB.
	wouldBeRef := fmt.Sprintf("tg:%d:%d:%s", chatID, msgIDBase+1, contact.ID.String())
	_, err = eventRepo.FindEventBySource(ctx, repository.InteractionSourceTelegram, wouldBeRef)
	require.Error(t, err, "extend path must not publish a create event for the inbound extension")
	assert.True(t, errors.Is(err, db.ErrNotFound), "expected ErrNotFound for the missing extend event, got %v", err)
}
