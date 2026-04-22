package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	tgpkg "personal-crm/backend/internal/telegram"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cleanupAggregationMessages hard-deletes test messages, interactions,
// AND the published event rows from prior runs. Post-PR-6 the aggregation
// engine publishes events via bus.PublishTx which dedupes on (source,
// source_id); leftover event rows from a previous run would cause the
// next publish to no-op and the consumer to never fire. Hard-delete the
// event rows keyed on the test's chat IDs too.
func cleanupAggregationMessages(t *testing.T, database *db.Database) {
	t.Helper()
	ctx := context.Background()
	// Hard delete test messages (unique message IDs for this test suite)
	_, _ = database.Pool.Exec(ctx, "DELETE FROM telegram_message WHERE telegram_message_id IN (80001, 80002, 80003, 80011, 80012, 80013, 80021, 80022, 80031, 80032, 80041, 80042, 80051, 80052, 80061, 80062)")
	// Hard delete interactions with source_ref matching test chat IDs
	_, _ = database.Pool.Exec(ctx, "DELETE FROM interaction WHERE source_ref LIKE 'tg:100:%' OR source_ref LIKE 'tg:101:%' OR source_ref LIKE 'tg:102:%' OR source_ref LIKE 'tg:201:%' OR source_ref LIKE 'tg:202:%' OR source_ref LIKE 'tg:301:%' OR source_ref LIKE 'tg:302:%' OR source_ref LIKE 'tg:303:%'")
	// Hard delete event rows keyed on the same test chat IDs so a re-run
	// doesn't collide with the previous run's published events.
	_, _ = database.Pool.Exec(ctx, "DELETE FROM event WHERE source = 'telegram' AND (source_id LIKE 'tg:100:%' OR source_id LIKE 'tg:101:%' OR source_id LIKE 'tg:102:%' OR source_id LIKE 'tg:201:%' OR source_id LIKE 'tg:202:%' OR source_id LIKE 'tg:301:%' OR source_id LIKE 'tg:302:%' OR source_id LIKE 'tg:303:%')")
}

func setupAggregationTest(t *testing.T) (
	*repository.TelegramMessageRepository,
	*repository.InteractionRepository,
	*repository.ContactRepository,
	*service.ContactService,
	*tgpkg.AggregationEngine,
	*db.Database,
) {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	// Migrations are applied once by TestMain.

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	messageRepo := repository.NewTelegramMessageRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo, nil, nil)

	// Cutover wiring: a live events.Bus with the InteractionRecorder worker
	// running so aggregation publishes → async consumer writes interaction
	// rows. Tests poll via waitForTelegramInteractionCount /
	// waitForTelegramMessagesProcessed to wait for the async write.
	bus := setupTestEventBus(t, ctx, database, contactService)

	engine := tgpkg.NewAggregationEngine(
		2, 48, // burst window 2h, reply bridge 48h
		messageRepo, interactionRepo,
		contactService, contactService,
		bus,
	)

	// Clean up test messages from previous runs
	cleanupAggregationMessages(t, database)
	t.Cleanup(func() { cleanupAggregationMessages(t, database) })

	return messageRepo, interactionRepo, contactRepo, contactService, engine, database
}

func createTestContact(t *testing.T, contactRepo *repository.ContactRepository, name string) *repository.Contact {
	t.Helper()
	contact, err := contactRepo.CreateContact(context.Background(), repository.CreateContactRequest{
		FullName: name,
	})
	require.NoError(t, err)
	return contact
}

// createTestContactWithCadence creates a contact with a cadence + baseline
// last_contacted so Extend/Promote tests can assert cadence column
// transitions against a deterministic starting state.
func createTestContactWithCadence(t *testing.T, contactRepo *repository.ContactRepository, name, cadence string, lastContacted time.Time) *repository.Contact {
	t.Helper()
	contact, err := contactRepo.CreateContact(context.Background(), repository.CreateContactRequest{
		FullName:      name,
		Cadence:       &cadence,
		LastContacted: &lastContacted,
	})
	require.NoError(t, err)
	return contact
}

func insertTestMessage(t *testing.T, repo *repository.TelegramMessageRepository, msgID int32, chatID int64, outgoing bool, sentAt time.Time, contactID *uuid.UUID, peerUserID int64) *repository.TelegramMessage {
	t.Helper()
	text := "test message"
	msg, err := repo.UpsertMessage(context.Background(), repository.UpsertTelegramMessageParams{
		TelegramMessageID: msgID,
		TelegramChatID:    chatID,
		ChatType:          "private",
		MessageText:       &text,
		MessageType:       "text",
		SentAt:            sentAt,
		IsOutgoing:        outgoing,
		PeerUserID:        ptrInt64(peerUserID),
	})
	require.NoError(t, err)

	if contactID != nil {
		err = repo.UpdateMessageContact(context.Background(), peerUserID, *contactID)
		require.NoError(t, err)
	}

	return msg
}

func TestAggregation_BatchOutboundBurst(t *testing.T) {
	messageRepo, interactionRepo, contactRepo, _, engine, _ := setupAggregationTest(t)
	ctx := context.Background()

	contact := createTestContact(t, contactRepo, "Aggregation Test 1")
	t.Cleanup(func() {
		_ = contactRepo.SoftDeleteContact(ctx, contact.ID)
	})

	base := accelerated.GetCurrentTime().Add(-1 * time.Hour).Truncate(time.Microsecond)

	// 3 outbound messages within 2h
	insertTestMessage(t, messageRepo, 80001, 100, true, base, &contact.ID, 70001)
	insertTestMessage(t, messageRepo, 80002, 100, true, base.Add(10*time.Minute), &contact.ID, 70001)
	insertTestMessage(t, messageRepo, 80003, 100, true, base.Add(30*time.Minute), &contact.ID, 70001)

	err := engine.AggregateAll(ctx)
	require.NoError(t, err)

	// Post-PR-6: aggregation publishes, consumer writes async.
	interactions := waitForInteractionCountExact(t, ctx, interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout)
	assert.Equal(t, "outbound", interactions[0].Direction)
	assert.Equal(t, "telegram", interactions[0].Source)
	assert.Contains(t, *interactions[0].SourceRef, "tg:100:")
}

func TestAggregation_BatchMutualBridge(t *testing.T) {
	messageRepo, interactionRepo, contactRepo, _, engine, _ := setupAggregationTest(t)
	ctx := context.Background()

	contact := createTestContact(t, contactRepo, "Aggregation Test 2")
	t.Cleanup(func() {
		_ = contactRepo.SoftDeleteContact(ctx, contact.ID)
	})

	base := accelerated.GetCurrentTime().Add(-2 * time.Hour).Truncate(time.Microsecond)

	// Outbound burst, then inbound within 48h → batch resolves to mutual
	insertTestMessage(t, messageRepo, 80011, 101, true, base, &contact.ID, 70002)
	insertTestMessage(t, messageRepo, 80012, 101, true, base.Add(5*time.Minute), &contact.ID, 70002)
	insertTestMessage(t, messageRepo, 80013, 101, false, base.Add(1*time.Hour), &contact.ID, 70002)

	err := engine.AggregateAll(ctx)
	require.NoError(t, err)

	interactions := waitForInteractionCountExact(t, ctx, interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout)
	assert.Equal(t, "mutual", interactions[0].Direction)
}

func TestAggregation_BatchNoChurnFollowUp(t *testing.T) {
	// Batch mode should NOT create an outbound interaction first and then promote —
	// it should directly create mutual. This test verifies by checking that exactly
	// one interaction is created (not an outbound followed by a mutual update).
	messageRepo, interactionRepo, contactRepo, _, engine, _ := setupAggregationTest(t)
	ctx := context.Background()

	contact := createTestContact(t, contactRepo, "Aggregation Test No Churn")
	t.Cleanup(func() {
		_ = contactRepo.SoftDeleteContact(ctx, contact.ID)
	})

	base := accelerated.GetCurrentTime().Add(-3 * time.Hour).Truncate(time.Microsecond)
	insertTestMessage(t, messageRepo, 80021, 102, true, base, &contact.ID, 70003)
	insertTestMessage(t, messageRepo, 80022, 102, false, base.Add(30*time.Minute), &contact.ID, 70003)

	err := engine.AggregateAll(ctx)
	require.NoError(t, err)

	interactions := waitForInteractionCountExact(t, ctx, interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout)
	assert.Equal(t, "mutual", interactions[0].Direction)
}

func TestAggregation_ChatScoped(t *testing.T) {
	messageRepo, interactionRepo, contactRepo, _, engine, database := setupAggregationTest(t)
	ctx := context.Background()

	contact := createTestContact(t, contactRepo, "Aggregation Chat Scoped")
	t.Cleanup(func() {
		_ = contactRepo.SoftDeleteContact(ctx, contact.ID)
	})

	base := accelerated.GetCurrentTime().Add(-1 * time.Hour).Truncate(time.Microsecond)

	// Same contact, different chats → separate interactions
	insertTestMessage(t, messageRepo, 80031, 201, true, base, &contact.ID, 70004)
	insertTestMessage(t, messageRepo, 80032, 202, true, base.Add(5*time.Minute), &contact.ID, 70004)

	err := engine.AggregateAll(ctx)
	require.NoError(t, err)

	// Wait for both chats' interactions to land (one per chat).
	_ = waitForTelegramInteractionCount(t, ctx, database.Pool, contact.ID, 2, defaultInteractionWaitTimeout)
	interactions, err := interactionRepo.ListContactInteractions(ctx, contact.ID, 100, 0)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(interactions), 2) // at least 2 (one per chat)
}

func TestAggregation_IncrementalCoalescing(t *testing.T) {
	messageRepo, interactionRepo, contactRepo, _, engine, _ := setupAggregationTest(t)
	ctx := context.Background()

	// Seed with cadence + a baseline last_contacted so we can observe
	// cadence column transitions after ExtendInteraction runs.
	initialLastContacted := accelerated.GetCurrentTime().Add(-24 * time.Hour).Truncate(time.Microsecond)
	contact := createTestContactWithCadence(t, contactRepo, "Aggregation Incremental Coalesce", "weekly", initialLastContacted)
	t.Cleanup(func() {
		_ = contactRepo.SoftDeleteContact(ctx, contact.ID)
	})
	require.NotNil(t, contact.ContactBy, "weekly cadence should seed contact_by")
	initialContactBy := *contact.ContactBy
	initialLastInteractionAt := contact.LastInteractionAt

	base := accelerated.GetCurrentTime().Add(-30 * time.Minute).Truncate(time.Microsecond)

	// First outbound message → aggregation publishes, consumer writes.
	insertTestMessage(t, messageRepo, 80041, 301, true, base, &contact.ID, 70005)
	err := engine.AggregateForContact(ctx, contact.ID, 301)
	require.NoError(t, err)

	interactions := waitForInteractionCountExact(t, ctx, interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout)
	assert.Equal(t, "outbound", interactions[0].Direction)

	// Second outbound message within burst window → aggregation's incremental
	// path calls ExtendInteraction (sync, not via event bus — plan Decision 3).
	// Still just one interaction row.
	extendTime := base.Add(10 * time.Minute)
	insertTestMessage(t, messageRepo, 80042, 301, true, extendTime, &contact.ID, 70005)
	err = engine.AggregateForContact(ctx, contact.ID, 301)
	require.NoError(t, err)

	interactions, err = interactionRepo.ListContactInteractions(ctx, contact.ID, 100, 0)
	require.NoError(t, err)
	assert.Len(t, interactions, 1) // still just 1 interaction (coalesced)

	// Assert ExtendInteraction's cadence side-effects.
	// Outbound extends bump ONLY last_outreach_at; last_contacted,
	// last_interaction_at, last_response_at and contact_by must stay put
	// (spec §3.4.2 direction rules). ExtendInteraction routes through
	// CadenceUpdater.ApplyInteraction post-cutover.
	updated, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.LastOutreachAt, "outbound extend must set last_outreach_at")
	assert.Equal(t, extendTime.UTC(), updated.LastOutreachAt.UTC(),
		"outbound ExtendInteraction should advance last_outreach_at to the extend timestamp")
	require.NotNil(t, updated.LastContacted)
	assert.Equal(t, initialLastContacted.UTC(), updated.LastContacted.UTC(),
		"outbound must NOT bump last_contacted")
	if initialLastInteractionAt == nil {
		assert.Nil(t, updated.LastInteractionAt, "outbound must NOT set last_interaction_at")
	} else {
		assert.Equal(t, initialLastInteractionAt.UTC(), updated.LastInteractionAt.UTC(),
			"outbound must NOT bump last_interaction_at")
	}
	assert.Nil(t, updated.LastResponseAt, "outbound must NOT set last_response_at")
	require.NotNil(t, updated.ContactBy)
	assert.Equal(t, initialContactBy.UTC(), updated.ContactBy.UTC(),
		"outbound must NOT recompute contact_by")
}

func TestAggregation_IncrementalReplyBridge(t *testing.T) {
	messageRepo, interactionRepo, contactRepo, _, engine, _ := setupAggregationTest(t)
	ctx := context.Background()

	// Seed with cadence + a baseline last_contacted so we can observe
	// cadence column transitions after PromoteInteractionToMutual runs.
	initialLastContacted := accelerated.GetCurrentTime().Add(-30 * 24 * time.Hour).Truncate(time.Microsecond)
	contact := createTestContactWithCadence(t, contactRepo, "Aggregation Incremental Bridge", "weekly", initialLastContacted)
	t.Cleanup(func() {
		_ = contactRepo.SoftDeleteContact(ctx, contact.ID)
	})
	require.NotNil(t, contact.ContactBy)
	initialContactBy := *contact.ContactBy

	base := accelerated.GetCurrentTime().Add(-2 * time.Hour).Truncate(time.Microsecond)

	// Create outbound interaction first (async via event bus).
	insertTestMessage(t, messageRepo, 80051, 302, true, base, &contact.ID, 70006)
	err := engine.AggregateForContact(ctx, contact.ID, 302)
	require.NoError(t, err)

	interactions := waitForInteractionCountExact(t, ctx, interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout)
	assert.Equal(t, "outbound", interactions[0].Direction)

	// Inbound reply within 48h → aggregation's reply-bridge path calls
	// PromoteInteractionToMutual (sync, plan Decision 3 — extend/promote
	// stay direct-path through PR 6).
	replyTime := base.Add(30 * time.Minute)
	insertTestMessage(t, messageRepo, 80052, 302, false, replyTime, &contact.ID, 70006)
	err = engine.AggregateForContact(ctx, contact.ID, 302)
	require.NoError(t, err)

	interactions = waitForInteractionDirection(t, ctx, interactionRepo, contact.ID, "mutual", defaultInteractionWaitTimeout)
	require.Len(t, interactions, 1) // still 1, promoted
	assert.Equal(t, "mutual", interactions[0].Direction)

	// Assert PromoteInteractionToMutual's cadence side-effects.
	// Mutual promotion via ApplyInteraction should advance ALL
	// cadence columns (last_contacted, last_interaction_at,
	// last_outreach_at, last_response_at) to the reply timestamp and
	// recompute contact_by from the contact's cadence.
	updated, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.LastContacted)
	assert.Equal(t, replyTime.UTC(), updated.LastContacted.UTC(),
		"mutual promote should advance last_contacted")
	require.NotNil(t, updated.LastInteractionAt)
	assert.Equal(t, replyTime.UTC(), updated.LastInteractionAt.UTC(),
		"mutual promote should advance last_interaction_at")
	require.NotNil(t, updated.LastOutreachAt)
	assert.Equal(t, replyTime.UTC(), updated.LastOutreachAt.UTC(),
		"mutual promote should advance last_outreach_at")
	require.NotNil(t, updated.LastResponseAt)
	assert.Equal(t, replyTime.UTC(), updated.LastResponseAt.UTC(),
		"mutual promote should set last_response_at")
	require.NotNil(t, updated.ContactBy)
	assert.True(t, updated.ContactBy.After(initialContactBy),
		"mutual promote should recompute contact_by forward (initial=%s, updated=%s)",
		initialContactBy, updated.ContactBy)
}

func TestAggregation_IncrementalExplicitReplyBridge(t *testing.T) {
	messageRepo, interactionRepo, contactRepo, _, engine, database := setupAggregationTest(t)
	ctx := context.Background()

	contact := createTestContact(t, contactRepo, "Aggregation Explicit Reply Bridge")
	t.Cleanup(func() {
		_ = contactRepo.SoftDeleteContact(ctx, contact.ID)
	})

	// Use timestamps >48h apart to prove explicit reply bridges regardless of time
	outboundTime := accelerated.GetCurrentTime().Add(-72 * time.Hour).Truncate(time.Microsecond)
	inboundTime := accelerated.GetCurrentTime().Add(-1 * time.Hour).Truncate(time.Microsecond)

	// Step 1: outbound message → aggregate → consumer async-writes the interaction.
	insertTestMessage(t, messageRepo, 80061, 303, true, outboundTime, &contact.ID, 70007)
	err := engine.AggregateForContact(ctx, contact.ID, 303)
	require.NoError(t, err)

	interactions := waitForInteractionCountExact(t, ctx, interactionRepo, contact.ID, 1, defaultInteractionWaitTimeout)
	assert.Equal(t, "outbound", interactions[0].Direction)

	// Step 2: inbound message with reply_to_msg_id pointing to the outbound message
	replyToID := int32(80061)
	text := "reply message"
	_, err = messageRepo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 80062,
		TelegramChatID:    303,
		ChatType:          "private",
		MessageText:       &text,
		MessageType:       "text",
		SentAt:            inboundTime,
		IsOutgoing:        false,
		PeerUserID:        ptrInt64(70007),
		ReplyToMsgID:      &replyToID,
	})
	require.NoError(t, err)
	err = messageRepo.UpdateMessageContact(ctx, 70007, contact.ID)
	require.NoError(t, err)

	// Need to mark the outbound message as processed with the interaction_id
	// so tryExplicitReplyBridge can find it
	outboundMsg, err := messageRepo.GetMessage(ctx, 303, 80061)
	require.NoError(t, err)
	err = messageRepo.MarkMessagesProcessed(ctx, []uuid.UUID{outboundMsg.ID}, interactions[0].ID)
	require.NoError(t, err)

	// Step 3: aggregate incrementally — PromoteInteractionToMutual is the
	// sync service path for reply-bridging (plan Decision 3 — extend/promote
	// stay direct-path through PR 6). No event-bus wait needed for the
	// promotion itself, but the prior async outbound write must have landed
	// before the promotion can find the interaction row to update — and it
	// did above via waitForInteractionCountExact. Re-list to capture the
	// now-promoted row.
	err = engine.AggregateForContact(ctx, contact.ID, 303)
	require.NoError(t, err)

	interactions = waitForInteractionDirection(t, ctx, interactionRepo, contact.ID, "mutual", defaultInteractionWaitTimeout)
	require.Len(t, interactions, 1)
	assert.Equal(t, "mutual", interactions[0].Direction)

	// Clean up test-specific data
	_, _ = database.Pool.Exec(ctx, "DELETE FROM telegram_message WHERE telegram_message_id IN (80061, 80062)")
	_, _ = database.Pool.Exec(ctx, "DELETE FROM interaction WHERE source_ref LIKE 'tg:303:%'")
}

// TestListDistinctUnmatchedPeers_PrefersPopulatedNameRow locks in the
// ORDER BY extension: with two unmatched rows for the same peer, the one with
// non-null first/last names wins the DISTINCT ON, even if it's older. Ensures
// a single aggregation pass picks the most-populated row.
func TestListDistinctUnmatchedPeers_PrefersPopulatedNameRow(t *testing.T) {
	messageRepo, _, _, _, _, database := setupAggregationTest(t)
	ctx := context.Background()

	const testPeerID int64 = 90001
	const testChatID int64 = 900
	username := "dale"
	firstName := "Dale"
	lastName := "Dobeck"

	// Narrow cleanup scoped to this test's peer/chat — use the sqlc helper
	// (the "Never write raw SQL in Go" rule applies in tests too).
	t.Cleanup(func() {
		_, _ = database.Queries.DeleteTelegramMessagesByPeerUserID(
			ctx,
			pgtype.Int8{Int64: testPeerID, Valid: true},
		)
	})

	base := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	text := "hi"

	// Newer row: has username but no names.
	_, err := messageRepo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 90001,
		TelegramChatID:    testChatID,
		ChatType:          "private",
		MessageText:       &text,
		MessageType:       "text",
		SentAt:            base,
		IsOutgoing:        false,
		PeerUserID:        ptrInt64(testPeerID),
		PeerUsername:      &username,
	})
	require.NoError(t, err)

	// Older row: has username AND names. Older means it'd lose the sent_at DESC
	// tiebreak; the name-prefering ORDER BY clauses should still pick it.
	_, err = messageRepo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 90002,
		TelegramChatID:    testChatID,
		ChatType:          "private",
		MessageText:       &text,
		MessageType:       "text",
		SentAt:            base.Add(-1 * time.Hour),
		IsOutgoing:        false,
		PeerUserID:        ptrInt64(testPeerID),
		PeerUsername:      &username,
		PeerFirstName:     &firstName,
		PeerLastName:      &lastName,
	})
	require.NoError(t, err)

	peers, err := messageRepo.ListDistinctUnmatchedPeers(ctx)
	require.NoError(t, err)

	var got *repository.UnmatchedPeer
	for i := range peers {
		if peers[i].PeerUserID == testPeerID {
			got = &peers[i]
			break
		}
	}
	require.NotNil(t, got, "expected test peer to be returned by ListDistinctUnmatchedPeers")
	require.NotNil(t, got.PeerFirstName, "populated-name row should win even though it is older")
	assert.Equal(t, "Dale", *got.PeerFirstName)
	require.NotNil(t, got.PeerLastName)
	assert.Equal(t, "Dobeck", *got.PeerLastName)
}

// TestListDistinctUnmatchedPeers_TreatsBlankStringsAsAbsent guards the ORDER BY
// clauses: a row with blank peer_first_name / peer_last_name must not outrank
// a row with a real name. Without the `<> ”` guards the outbound-private-chat
// shape (blank strings instead of NULL) would keep winning the tiebreak and
// the batch path would re-seed external_contact with blanks.
func TestListDistinctUnmatchedPeers_TreatsBlankStringsAsAbsent(t *testing.T) {
	messageRepo, _, _, _, _, database := setupAggregationTest(t)
	ctx := context.Background()

	const testPeerID int64 = 90002
	const testChatID int64 = 901
	empty := ""
	firstName := "Dale"
	lastName := "Dobeck"

	t.Cleanup(func() {
		_, _ = database.Queries.DeleteTelegramMessagesByPeerUserID(
			ctx,
			pgtype.Int8{Int64: testPeerID, Valid: true},
		)
	})

	base := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	text := "hi"

	// Newer row: blank entity strings.
	_, err := messageRepo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 90003,
		TelegramChatID:    testChatID,
		ChatType:          "private",
		MessageText:       &text,
		MessageType:       "text",
		SentAt:            base,
		IsOutgoing:        true,
		PeerUserID:        ptrInt64(testPeerID),
		PeerFirstName:     &empty,
		PeerLastName:      &empty,
	})
	require.NoError(t, err)

	// Older row: populated entity data.
	_, err = messageRepo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 90004,
		TelegramChatID:    testChatID,
		ChatType:          "private",
		MessageText:       &text,
		MessageType:       "text",
		SentAt:            base.Add(-1 * time.Hour),
		IsOutgoing:        false,
		PeerUserID:        ptrInt64(testPeerID),
		PeerFirstName:     &firstName,
		PeerLastName:      &lastName,
	})
	require.NoError(t, err)

	peers, err := messageRepo.ListDistinctUnmatchedPeers(ctx)
	require.NoError(t, err)

	var got *repository.UnmatchedPeer
	for i := range peers {
		if peers[i].PeerUserID == testPeerID {
			got = &peers[i]
			break
		}
	}
	require.NotNil(t, got)
	require.NotNil(t, got.PeerFirstName)
	assert.Equal(t, "Dale", *got.PeerFirstName, "blank-string row must not outrank a real-name row")
	require.NotNil(t, got.PeerLastName)
	assert.Equal(t, "Dobeck", *got.PeerLastName)
}
