package tests

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInteractionRepository_FindRecentInteractionBySourceAndDirection_SourceFilter
// exercises the source-neutral interaction lookup queries against a real
// database to prove the source parameter is honoured. The test mixes two
// already-allowed sources (telegram + gcal) to verify the filter at the SQL
// level; future migrations can widen the interaction.source CHECK list and
// extend this coverage.
func TestInteractionRepository_FindRecentInteractionBySourceAndDirection_SourceFilter(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	t.Parallel()
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	gen, _ := migrationGenerator(t)
	interactionRepo := repository.NewInteractionRepository(database.Queries)

	// testStripe=3 carves out a per-test subspace of chat IDs and source_ref
	// prefixes that cannot collide with the inbound-coalesce (stripe=1) or
	// explicit-reply-bridge (stripe=2) integration tests.
	const testStripe = 3
	seed := uint64((accelerated.GetCurrentTime().UnixNano() & 0x1FFFFF) << 11)
	// nolint:gosec // test-only seed; math/rand is sufficient for ID disambiguation
	seed |= uint64(rand.Int31n(2048))

	tgChatID := int64(testStripe)<<32 | int64(seed)
	tgRef := fmt.Sprintf("tg:%d:1", tgChatID)
	tgPrefix := fmt.Sprintf("tg:%d:%%", tgChatID)

	gcalRef := fmt.Sprintf("gcal:test-%d-%d", testStripe, seed)
	gcalPrefix := fmt.Sprintf("gcal:test-%d-%d%%", testStripe, seed)
	// Wider gcal cleanup pattern catches any orphaned rows from prior runs
	// that crashed before the per-run cleanup ran.
	gcalCleanupPrefix := fmt.Sprintf("gcal:test-%d-%%", testStripe)

	t.Cleanup(func() {
		_ = interactionRepo.HardDeleteInteractionsBySourceRefPrefix(ctx, repository.InteractionSourceTelegram, tgPrefix)
		_ = interactionRepo.HardDeleteInteractionsBySourceRefPrefix(ctx, repository.InteractionSourceGCal, gcalCleanupPrefix)
	})

	// The contact is an FK target referenced by ID only; the source_ref
	// stripe scheme above carries this test's isolation, so the seed just
	// needs a valid contact.
	contact, contactCleanup := seedMigrationContact(ctx, t, database, gen)
	t.Cleanup(contactCleanup)

	t0 := accelerated.GetCurrentTime().Add(-2 * time.Hour).Truncate(time.Microsecond)
	windowStart := t0.Add(-1 * time.Hour)
	windowEnd := t0.Add(1 * time.Hour)

	// Insert one telegram inbound + one gcal inbound for the same contact
	// in overlapping windows. The source filter must distinguish them.
	_, err = interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourceTelegram,
		SourceRef:  &tgRef,
		OccurredAt: t0,
		Direction:  repository.InteractionDirectionInbound,
	})
	require.NoError(t, err)
	_, err = interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourceGCal,
		SourceRef:  &gcalRef,
		OccurredAt: t0,
		Direction:  repository.InteractionDirectionInbound,
	})
	require.NoError(t, err)

	t.Run("source=telegram returns telegram row", func(t *testing.T) {
		got, err := interactionRepo.FindRecentInteractionBySourceAndDirection(
			ctx, contact.ID,
			repository.InteractionSourceTelegram, repository.InteractionDirectionInbound,
			tgPrefix, windowStart, windowEnd,
		)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, repository.InteractionSourceTelegram, got.Source)
		require.NotNil(t, got.SourceRef)
		assert.Equal(t, tgRef, *got.SourceRef)
	})

	t.Run("source=gcal returns gcal row", func(t *testing.T) {
		got, err := interactionRepo.FindRecentInteractionBySourceAndDirection(
			ctx, contact.ID,
			repository.InteractionSourceGCal, repository.InteractionDirectionInbound,
			gcalPrefix, windowStart, windowEnd,
		)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, repository.InteractionSourceGCal, got.Source)
		require.NotNil(t, got.SourceRef)
		assert.Equal(t, gcalRef, *got.SourceRef)
	})

	t.Run("unknown source returns ErrNotFound", func(t *testing.T) {
		_, err := interactionRepo.FindRecentInteractionBySourceAndDirection(
			ctx, contact.ID,
			repository.InteractionSourceTodoist, repository.InteractionDirectionOutbound,
			fmt.Sprintf("todoist:test-%d-%%", seed),
			windowStart, windowEnd,
		)
		require.Error(t, err)
		assert.True(t, errors.Is(err, db.ErrNotFound), "expected ErrNotFound, got %v", err)
	})

	t.Run("non-matching prefix returns ErrNotFound", func(t *testing.T) {
		// Use a tg prefix that does NOT match our inserted source_ref. Since
		// this prefix overlaps the test's stripe but not its source_ref, no
		// row should be returned (and no cross-test data lives in this
		// stripe range).
		nonMatchingPrefix := fmt.Sprintf("tg:%d:99999%%", tgChatID)
		_, err := interactionRepo.FindRecentInteractionBySourceAndDirection(
			ctx, contact.ID,
			repository.InteractionSourceTelegram, repository.InteractionDirectionInbound,
			nonMatchingPrefix, windowStart, windowEnd,
		)
		require.Error(t, err)
		assert.True(t, errors.Is(err, db.ErrNotFound), "expected ErrNotFound, got %v", err)
	})

	t.Run("out-of-window returns ErrNotFound", func(t *testing.T) {
		futureStart := t0.Add(10 * time.Hour)
		futureEnd := t0.Add(20 * time.Hour)
		_, err := interactionRepo.FindRecentInteractionBySourceAndDirection(
			ctx, contact.ID,
			repository.InteractionSourceTelegram, repository.InteractionDirectionInbound,
			tgPrefix, futureStart, futureEnd,
		)
		require.Error(t, err)
		assert.True(t, errors.Is(err, db.ErrNotFound), "expected ErrNotFound, got %v", err)
	})

	// FindRecentOutboundInteractionBySource has a hard-coded direction
	// ('outbound') so the source filter is its only configurable axis.
	t.Run("FindRecentOutboundInteractionBySource respects source", func(t *testing.T) {
		// Add an outbound telegram row in the window.
		obRef := fmt.Sprintf("tg:%d:2", tgChatID)
		_, err := interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
			ContactID:  contact.ID,
			Source:     repository.InteractionSourceTelegram,
			SourceRef:  &obRef,
			OccurredAt: t0.Add(5 * time.Minute),
			Direction:  repository.InteractionDirectionOutbound,
		})
		require.NoError(t, err)

		got, err := interactionRepo.FindRecentOutboundInteractionBySource(
			ctx, contact.ID,
			repository.InteractionSourceTelegram, tgPrefix, windowStart, windowEnd,
		)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, repository.InteractionSourceTelegram, got.Source)
		assert.Equal(t, repository.InteractionDirectionOutbound, got.Direction)

		// Same query with source=gcal should not return the telegram row
		// (gcal has no outbound rows here).
		_, err = interactionRepo.FindRecentOutboundInteractionBySource(
			ctx, contact.ID,
			repository.InteractionSourceGCal, gcalPrefix, windowStart, windowEnd,
		)
		require.Error(t, err)
		assert.True(t, errors.Is(err, db.ErrNotFound), "expected ErrNotFound, got %v", err)
	})
}
