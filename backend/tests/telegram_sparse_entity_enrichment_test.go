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
	tgpkg "personal-crm/backend/internal/telegram"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sparseEnrichEnv bundles the dependencies needed to drive HandleNewMessage /
// HandleEditMessage end-to-end against a real DB.
type sparseEnrichEnv struct {
	handler        *tgpkg.MessageHandler
	messageRepo    *repository.TelegramMessageRepository
	chatConfigRepo *repository.TelegramChatConfigRepository
	database       *db.Database
}

// setupSparseEnrichTest builds a MessageHandler with real repos, a nil
// peerMatcher (matcher logic is out of scope for these tests) and nil
// syncStateID (so the sync update is a no-op).
func setupSparseEnrichTest(t *testing.T) *sparseEnrichEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	require.NoError(t, db.RunMigrations(databaseURL, getMigrationsPath()))

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(context.Background(), cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	messageRepo := repository.NewTelegramMessageRepository(database.Queries)
	chatConfigRepo := repository.NewTelegramChatConfigRepository(database.Queries)
	syncRepo := repository.NewSyncRepository(database.Queries)

	handler := tgpkg.NewMessageHandler(
		messageRepo,
		chatConfigRepo,
		syncRepo,
		nil,    // syncStateID — nil → updateSyncTimestamp is a no-op
		111111, // selfUserID — never matches any test peer
		10,     // groupMaxMembers
		nil,    // peerMatcher — nil means matching path is skipped
		nil,    // aggregationEngine
	)

	return &sparseEnrichEnv{
		handler:        handler,
		messageRepo:    messageRepo,
		chatConfigRepo: chatConfigRepo,
		database:       database,
	}
}

// cleanupSparseEnrichTestData hard-deletes any telegram_message rows for the
// given chat_id and the chat_config row for that chat. Scoped narrowly to a
// single test's chat_id to avoid touching unrelated data.
func cleanupSparseEnrichTestData(t *testing.T, env *sparseEnrichEnv, chatID int64) {
	t.Helper()
	ctx := context.Background()
	_, _ = env.database.Pool.Exec(ctx, "DELETE FROM telegram_message WHERE telegram_chat_id = $1", chatID)
	_ = env.chatConfigRepo.DeleteConfig(ctx, chatID)
}

// makePrivateMessage builds a *tg.Message for a private chat between self and peer.
func makePrivateMessage(peerUserID int64, msgID int, sentAt time.Time, text string) *tg.Message {
	return &tg.Message{
		ID:      msgID,
		Date:    int(sentAt.Unix()),
		PeerID:  &tg.PeerUser{UserID: peerUserID},
		Message: text,
	}
}

func TestSparseEntityEnrichment_HandleNewMessage_FillsFromHistory(t *testing.T) {
	env := setupSparseEnrichTest(t)
	ctx := context.Background()
	peerUserID, peerIDStr := uniqueTestIDs(t)
	chatID := peerUserID
	t.Cleanup(func() { cleanupSparseEnrichTestData(t, env, chatID) })

	// Seed a historical row with rich entity data.
	username := "richuser" + peerIDStr
	firstName := "Rich" + peerIDStr
	_, err := env.messageRepo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 70001,
		TelegramChatID:    chatID,
		ChatType:          "private",
		MessageType:       "text",
		SentAt:            accelerated.GetCurrentTime().Add(-1 * time.Hour),
		IsOutgoing:        false,
		PeerUserID:        &peerUserID,
		PeerUsername:      &username,
		PeerFirstName:     &firstName,
	})
	require.NoError(t, err)

	// New incoming message arrives with EMPTY entities (sparse update).
	msg := makePrivateMessage(peerUserID, 70002, accelerated.GetCurrentTime(), "hello")
	entities := tg.Entities{Users: map[int64]*tg.User{}}
	update := &tg.UpdateNewMessage{Message: msg}

	require.NoError(t, env.handler.HandleNewMessage(ctx, entities, update))

	// The new row must have been enriched from history.
	got, err := env.messageRepo.GetMessage(ctx, chatID, 70002)
	require.NoError(t, err)
	require.NotNil(t, got.PeerUsername, "peer_username must be backfilled from history")
	assert.Equal(t, username, *got.PeerUsername)
	require.NotNil(t, got.PeerFirstName)
	assert.Equal(t, firstName, *got.PeerFirstName)
}

func TestSparseEntityEnrichment_HandleNewMessage_NoHistoryStaysSparse(t *testing.T) {
	env := setupSparseEnrichTest(t)
	ctx := context.Background()
	peerUserID, _ := uniqueTestIDs(t)
	chatID := peerUserID
	t.Cleanup(func() { cleanupSparseEnrichTestData(t, env, chatID) })

	// First-ever message for this peer — no history, sparse entity.
	msg := makePrivateMessage(peerUserID, 70001, accelerated.GetCurrentTime(), "first hello")
	entities := tg.Entities{Users: map[int64]*tg.User{}}
	update := &tg.UpdateNewMessage{Message: msg}

	require.NoError(t, env.handler.HandleNewMessage(ctx, entities, update))

	got, err := env.messageRepo.GetMessage(ctx, chatID, 70001)
	require.NoError(t, err)
	assert.Nil(t, got.PeerUsername, "no history → entity stays nil")
	assert.Nil(t, got.PeerFirstName)
}

func TestSparseEntityEnrichment_HandleEditMessage_FillsFromHistory(t *testing.T) {
	env := setupSparseEnrichTest(t)
	ctx := context.Background()
	peerUserID, peerIDStr := uniqueTestIDs(t)
	chatID := peerUserID
	t.Cleanup(func() { cleanupSparseEnrichTestData(t, env, chatID) })

	// Seed a historical row with rich data on a DIFFERENT message id.
	username := "edituser" + peerIDStr
	firstName := "Edit" + peerIDStr
	_, err := env.messageRepo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 70010,
		TelegramChatID:    chatID,
		ChatType:          "private",
		MessageType:       "text",
		SentAt:            accelerated.GetCurrentTime().Add(-1 * time.Hour),
		IsOutgoing:        false,
		PeerUserID:        &peerUserID,
		PeerUsername:      &username,
		PeerFirstName:     &firstName,
	})
	require.NoError(t, err)

	// Now an edit arrives for a DIFFERENT message id (70011) with sparse entities.
	editMsg := makePrivateMessage(peerUserID, 70011, accelerated.GetCurrentTime(), "edited body")
	editMsg.SetEditDate(int(accelerated.GetCurrentTime().Unix()))
	entities := tg.Entities{Users: map[int64]*tg.User{}}
	update := &tg.UpdateEditMessage{Message: editMsg}

	require.NoError(t, env.handler.HandleEditMessage(ctx, entities, update))

	got, err := env.messageRepo.GetMessage(ctx, chatID, 70011)
	require.NoError(t, err)
	require.NotNil(t, got.PeerUsername, "edit must also enrich from history")
	assert.Equal(t, username, *got.PeerUsername)
}

func TestSparseEntityEnrichment_AuthoritativeEmptyRespectsRemoval(t *testing.T) {
	env := setupSparseEnrichTest(t)
	ctx := context.Background()
	peerUserID, peerIDStr := uniqueTestIDs(t)
	chatID := peerUserID
	t.Cleanup(func() { cleanupSparseEnrichTestData(t, env, chatID) })

	// Seed history with an old username.
	oldHandle := "oldhandle" + peerIDStr
	_, err := env.messageRepo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 70001,
		TelegramChatID:    chatID,
		ChatType:          "private",
		MessageType:       "text",
		SentAt:            accelerated.GetCurrentTime().Add(-1 * time.Hour),
		IsOutgoing:        false,
		PeerUserID:        &peerUserID,
		PeerUsername:      &oldHandle,
		PeerFirstName:     strPtr("OldName"),
	})
	require.NoError(t, err)

	// New message arrives with entity RESOLVED but Username=""
	// (user removed their handle). FirstName is populated.
	msg := makePrivateMessage(peerUserID, 70002, accelerated.GetCurrentTime(), "no username now")
	entities := tg.Entities{Users: map[int64]*tg.User{
		peerUserID: {ID: peerUserID, Username: "", FirstName: "NewName"},
	}}
	update := &tg.UpdateNewMessage{Message: msg}

	require.NoError(t, env.handler.HandleNewMessage(ctx, entities, update))

	got, err := env.messageRepo.GetMessage(ctx, chatID, 70002)
	require.NoError(t, err)
	assert.Nil(t, got.PeerUsername, "authoritative empty username must NOT be backfilled")
	require.NotNil(t, got.PeerFirstName)
	assert.Equal(t, "NewName", *got.PeerFirstName)
}

func TestSparseEntityEnrichment_StraightRenameCurrentNonBlankWins(t *testing.T) {
	env := setupSparseEnrichTest(t)
	ctx := context.Background()
	peerUserID, peerIDStr := uniqueTestIDs(t)
	chatID := peerUserID
	t.Cleanup(func() { cleanupSparseEnrichTestData(t, env, chatID) })

	oldHandle := "old" + peerIDStr
	_, err := env.messageRepo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 70001,
		TelegramChatID:    chatID,
		ChatType:          "private",
		MessageType:       "text",
		SentAt:            accelerated.GetCurrentTime().Add(-1 * time.Hour),
		IsOutgoing:        false,
		PeerUserID:        &peerUserID,
		PeerUsername:      &oldHandle,
	})
	require.NoError(t, err)

	// New message carries an authoritative tg.User with a NEW handle.
	newHandle := "new" + peerIDStr
	msg := makePrivateMessage(peerUserID, 70002, accelerated.GetCurrentTime(), "renamed")
	entities := tg.Entities{Users: map[int64]*tg.User{
		peerUserID: {ID: peerUserID, Username: newHandle},
	}}
	update := &tg.UpdateNewMessage{Message: msg}

	require.NoError(t, env.handler.HandleNewMessage(ctx, entities, update))

	got, err := env.messageRepo.GetMessage(ctx, chatID, 70002)
	require.NoError(t, err)
	require.NotNil(t, got.PeerUsername)
	assert.Equal(t, newHandle, *got.PeerUsername, "current non-blank handle must win, fallback must not run")

	// Original row should still carry the old handle untouched.
	original, err := env.messageRepo.GetMessage(ctx, chatID, 70001)
	require.NoError(t, err)
	require.NotNil(t, original.PeerUsername)
	assert.Equal(t, oldHandle, *original.PeerUsername)
}

func TestSparseEntityEnrichment_UntrackedGroupSkipsBeforeEnrichment(t *testing.T) {
	env := setupSparseEnrichTest(t)
	ctx := context.Background()
	peerUserID, peerIDStr := uniqueTestIDs(t)
	groupChatID := peerUserID + 1 // distinct from peer id
	t.Cleanup(func() { cleanupSparseEnrichTestData(t, env, groupChatID) })

	// Pre-mark the group chat as ignored.
	_, err := env.chatConfigRepo.UpsertConfig(ctx, repository.UpsertTelegramChatConfigParams{
		TelegramChatID: groupChatID,
		ChatType:       "group",
		Status:         "ignored",
	})
	require.NoError(t, err)

	// Build a sparse group message from peerUserID.
	msg := &tg.Message{
		ID:      70001,
		Date:    int(accelerated.GetCurrentTime().Unix()),
		PeerID:  &tg.PeerChat{ChatID: groupChatID},
		Message: "ignored chat msg " + peerIDStr,
	}
	msg.SetFromID(&tg.PeerUser{UserID: peerUserID})
	entities := tg.Entities{Users: map[int64]*tg.User{}}
	update := &tg.UpdateNewMessage{Message: msg}

	require.NoError(t, env.handler.HandleNewMessage(ctx, entities, update))

	// No row should have been inserted for the ignored chat.
	_, err = env.messageRepo.GetMessage(ctx, groupChatID, 70001)
	require.Error(t, err, "ignored group chat must not insert any message row")
}
