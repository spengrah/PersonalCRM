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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cleanupAggregationMessages hard-deletes test messages and interactions.
func cleanupAggregationMessages(t *testing.T, database *db.Database) {
	t.Helper()
	ctx := context.Background()
	// Hard delete test messages (unique message IDs for this test suite)
	_, _ = database.Pool.Exec(ctx, "DELETE FROM telegram_message WHERE telegram_message_id IN (80001, 80002, 80003, 80011, 80012, 80013, 80021, 80022, 80031, 80032, 80041, 80042, 80051, 80052, 80061, 80062)")
	// Hard delete interactions with source_ref matching test chat IDs
	_, _ = database.Pool.Exec(ctx, "DELETE FROM interaction WHERE source_ref LIKE 'tg:100:%' OR source_ref LIKE 'tg:101:%' OR source_ref LIKE 'tg:102:%' OR source_ref LIKE 'tg:201:%' OR source_ref LIKE 'tg:202:%' OR source_ref LIKE 'tg:301:%' OR source_ref LIKE 'tg:302:%' OR source_ref LIKE 'tg:303:%'")
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

	err := db.RunMigrations(databaseURL, getMigrationsPath())
	require.NoError(t, err)

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
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo)

	engine := tgpkg.NewAggregationEngine(
		2, 48, // burst window 2h, reply bridge 48h
		messageRepo, interactionRepo,
		contactService, contactService, contactService,
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

	interactions, err := interactionRepo.ListContactInteractions(ctx, contact.ID, 100, 0)
	require.NoError(t, err)
	require.Len(t, interactions, 1)
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

	interactions, err := interactionRepo.ListContactInteractions(ctx, contact.ID, 100, 0)
	require.NoError(t, err)
	require.Len(t, interactions, 1)
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

	interactions, err := interactionRepo.ListContactInteractions(ctx, contact.ID, 100, 0)
	require.NoError(t, err)
	require.Len(t, interactions, 1)
	assert.Equal(t, "mutual", interactions[0].Direction)
}

func TestAggregation_ChatScoped(t *testing.T) {
	messageRepo, interactionRepo, contactRepo, _, engine, _ := setupAggregationTest(t)
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

	interactions, err := interactionRepo.ListContactInteractions(ctx, contact.ID, 100, 0)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(interactions), 2) // at least 2 (one per chat)
}

func TestAggregation_IncrementalCoalescing(t *testing.T) {
	messageRepo, interactionRepo, contactRepo, _, engine, _ := setupAggregationTest(t)
	ctx := context.Background()

	contact := createTestContact(t, contactRepo, "Aggregation Incremental Coalesce")
	t.Cleanup(func() {
		_ = contactRepo.SoftDeleteContact(ctx, contact.ID)
	})

	base := accelerated.GetCurrentTime().Add(-30 * time.Minute).Truncate(time.Microsecond)

	// First outbound message → creates interaction
	insertTestMessage(t, messageRepo, 80041, 301, true, base, &contact.ID, 70005)
	err := engine.AggregateForContact(ctx, contact.ID, 301)
	require.NoError(t, err)

	interactions, err := interactionRepo.ListContactInteractions(ctx, contact.ID, 100, 0)
	require.NoError(t, err)
	require.Len(t, interactions, 1)
	assert.Equal(t, "outbound", interactions[0].Direction)

	// Second outbound message within burst window → should coalesce (extend), not create new
	insertTestMessage(t, messageRepo, 80042, 301, true, base.Add(10*time.Minute), &contact.ID, 70005)
	err = engine.AggregateForContact(ctx, contact.ID, 301)
	require.NoError(t, err)

	interactions, err = interactionRepo.ListContactInteractions(ctx, contact.ID, 100, 0)
	require.NoError(t, err)
	assert.Len(t, interactions, 1) // still just 1 interaction (coalesced)
}

func TestAggregation_IncrementalReplyBridge(t *testing.T) {
	messageRepo, interactionRepo, contactRepo, _, engine, _ := setupAggregationTest(t)
	ctx := context.Background()

	contact := createTestContact(t, contactRepo, "Aggregation Incremental Bridge")
	t.Cleanup(func() {
		_ = contactRepo.SoftDeleteContact(ctx, contact.ID)
	})

	base := accelerated.GetCurrentTime().Add(-2 * time.Hour).Truncate(time.Microsecond)

	// Create outbound interaction first
	insertTestMessage(t, messageRepo, 80051, 302, true, base, &contact.ID, 70006)
	err := engine.AggregateForContact(ctx, contact.ID, 302)
	require.NoError(t, err)

	interactions, err := interactionRepo.ListContactInteractions(ctx, contact.ID, 100, 0)
	require.NoError(t, err)
	require.Len(t, interactions, 1)
	assert.Equal(t, "outbound", interactions[0].Direction)

	// Inbound reply within 48h → should promote to mutual
	insertTestMessage(t, messageRepo, 80052, 302, false, base.Add(30*time.Minute), &contact.ID, 70006)
	err = engine.AggregateForContact(ctx, contact.ID, 302)
	require.NoError(t, err)

	interactions, err = interactionRepo.ListContactInteractions(ctx, contact.ID, 100, 0)
	require.NoError(t, err)
	require.Len(t, interactions, 1) // still 1, promoted
	assert.Equal(t, "mutual", interactions[0].Direction)
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

	// Step 1: outbound message → aggregate → creates outbound interaction
	insertTestMessage(t, messageRepo, 80061, 303, true, outboundTime, &contact.ID, 70007)
	err := engine.AggregateForContact(ctx, contact.ID, 303)
	require.NoError(t, err)

	interactions, err := interactionRepo.ListContactInteractions(ctx, contact.ID, 100, 0)
	require.NoError(t, err)
	require.Len(t, interactions, 1)
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

	// Step 3: aggregate incrementally
	err = engine.AggregateForContact(ctx, contact.ID, 303)
	require.NoError(t, err)

	// Assert: 1 mutual interaction (bridged via explicit reply despite >48h gap)
	interactions, err = interactionRepo.ListContactInteractions(ctx, contact.ID, 100, 0)
	require.NoError(t, err)
	require.Len(t, interactions, 1)
	assert.Equal(t, "mutual", interactions[0].Direction)

	// Clean up test-specific data
	_, _ = database.Pool.Exec(ctx, "DELETE FROM telegram_message WHERE telegram_message_id IN (80061, 80062)")
	_, _ = database.Pool.Exec(ctx, "DELETE FROM interaction WHERE source_ref LIKE 'tg:303:%'")
}
