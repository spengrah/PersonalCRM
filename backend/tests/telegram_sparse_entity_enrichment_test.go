package tests

import (
	"context"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	tgpkg "personal-crm/backend/internal/telegram"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingFetcher wraps a real PeerEntityFetcher and records call counts.
// Used to assert the sparse-entity fallback lookup did/didn't fire — for
// example, to verify the tracking-gate short-circuits before enrichment.
type countingFetcher struct {
	inner tgpkg.PeerEntityFetcher
	count int64
}

func (c *countingFetcher) GetPeerEntityByUserID(ctx context.Context, peerUserID int64) (*repository.PeerEntity, error) {
	atomic.AddInt64(&c.count, 1)
	return c.inner.GetPeerEntityByUserID(ctx, peerUserID)
}

func (c *countingFetcher) Count() int64 { return atomic.LoadInt64(&c.count) }

// sparseEnrichEnv bundles the dependencies needed to drive HandleNewMessage /
// HandleEditMessage end-to-end against a real DB.
type sparseEnrichEnv struct {
	handler        *tgpkg.MessageHandler
	messageRepo    *repository.TelegramMessageRepository
	chatConfigRepo *repository.TelegramChatConfigRepository
	externalRepo   *repository.ExternalContactRepository
	contactRepo    *repository.ContactRepository
	database       *db.Database
}

// setupSparseEnrichTest builds a MessageHandler with real repos and a real
// PeerMatcher (discoveryMinMsgs=1 so a single message is enough to upsert
// the discovery candidate and exercise the enriched-fields-into-discovery
// path).
func setupSparseEnrichTest(t *testing.T) *sparseEnrichEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	// Migrations are applied once by TestMain.

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(context.Background(), cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	messageRepo := repository.NewTelegramMessageRepository(database.Queries)
	chatConfigRepo := repository.NewTelegramChatConfigRepository(database.Queries)
	syncRepo := repository.NewSyncRepository(database.Queries)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	enrichmentRepo := repository.NewEnrichmentRepository(database.Queries)
	identityRepo := repository.NewIdentityRepository(database.Queries)

	identitySvc := service.NewIdentityService(identityRepo)
	enrichmentSvc := service.NewEnrichmentService(database, contactRepo, contactMethodRepo, enrichmentRepo)
	matcher := tgpkg.NewPeerMatcher(identitySvc, messageRepo, externalRepo, enrichmentSvc, 1)

	handler := tgpkg.NewMessageHandler(
		messageRepo,
		chatConfigRepo,
		syncRepo,
		nil,    // syncStateID — nil → updateSyncTimestamp is a no-op
		111111, // selfUserID — never matches any test peer
		10,     // groupMaxMembers
		matcher,
		nil, // aggregationEngine — exercised separately
	)

	return &sparseEnrichEnv{
		handler:        handler,
		messageRepo:    messageRepo,
		chatConfigRepo: chatConfigRepo,
		externalRepo:   externalRepo,
		contactRepo:    contactRepo,
		database:       database,
	}
}

// cleanupExternalBySource deletes any external_contact rows associated with
// the given source ID (decimal peer user ID). Lets a test that exercises the
// matcher/discovery path leave no orphans.
func cleanupExternalBySource(t *testing.T, env *sparseEnrichEnv, peerIDStr string) {
	t.Helper()
	ctx := context.Background()
	_, _ = env.database.Queries.DeleteExternalContactsBySourceIDPrefix(ctx, pgtype.Text{String: peerIDStr, Valid: true})
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

	// Wrap the real fetcher so we can prove the fallback was NOT consulted.
	counter := &countingFetcher{inner: env.messageRepo}
	env.handler.SetPeerEntityFetcher(counter)

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

	// And the enrichment fallback must have been short-circuited by the
	// tracking gate — proves enrichment is placed AFTER shouldTrackChat.
	assert.Equal(t, int64(0), counter.Count(), "enrichSparseEntity must not run for ignored group chats")
}

// TestSparseEntityEnrichment_HandleEditMessage_FillsFromHistory has already
// covered the "older row is rich, sparse new arrives" path. This test covers
// the inverse recency edge case Codex flagged in PR #278: after the user has
// removed their handle in an authoritative update (Username=""), a SUBSEQUENT
// sparse update must NOT resurrect the old handle from the historical row.
// The fix relies on the persisted peer_entity_resolved flag so that
// GetPeerEntityByUserID prefers the most recent authoritative row over any
// older non-blank value.
func TestSparseEntityEnrichment_AuthoritativeRemovalSticksAcrossSparseUpdates(t *testing.T) {
	env := setupSparseEnrichTest(t)
	ctx := context.Background()
	peerUserID, peerIDStr := uniqueTestIDs(t)
	chatID := peerUserID
	t.Cleanup(func() { cleanupSparseEnrichTestData(t, env, chatID) })

	// Step 1: legacy historical row with rich entity data (resolved=false).
	oldHandle := "oldhandle" + peerIDStr
	_, err := env.messageRepo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
		TelegramMessageID: 70001,
		TelegramChatID:    chatID,
		ChatType:          "private",
		MessageType:       "text",
		SentAt:            accelerated.GetCurrentTime().Add(-2 * time.Hour),
		IsOutgoing:        false,
		PeerUserID:        &peerUserID,
		PeerUsername:      &oldHandle,
		PeerFirstName:     strPtr("OldName"),
	})
	require.NoError(t, err)

	// Step 2: authoritative update arrives with Username="" (handle removed)
	// but FirstName populated. Drives HandleNewMessage so the row is stored
	// with peer_entity_resolved=true and peer_username=NULL.
	authMsg := makePrivateMessage(peerUserID, 70002, accelerated.GetCurrentTime().Add(-1*time.Hour), "removed my handle")
	authEntities := tg.Entities{Users: map[int64]*tg.User{
		peerUserID: {ID: peerUserID, Username: "", FirstName: "NewName"},
	}}
	require.NoError(t, env.handler.HandleNewMessage(ctx, authEntities, &tg.UpdateNewMessage{Message: authMsg}))

	// Step 3: a NEW sparse update arrives. Without the recency fix, the
	// fallback would prefer the older non-blank row and resurrect oldHandle.
	sparseMsg := makePrivateMessage(peerUserID, 70003, accelerated.GetCurrentTime(), "next message")
	require.NoError(t, env.handler.HandleNewMessage(ctx, tg.Entities{Users: map[int64]*tg.User{}}, &tg.UpdateNewMessage{Message: sparseMsg}))

	got, err := env.messageRepo.GetMessage(ctx, chatID, 70003)
	require.NoError(t, err)
	assert.Nil(t, got.PeerUsername, "removal must stick — sparse update must NOT resurrect old handle")
	require.NotNil(t, got.PeerFirstName, "first_name from authoritative row should still flow through")
	assert.Equal(t, "NewName", *got.PeerFirstName)
}

// TestSparseEntityEnrichment_EnrichedFieldsFlowToDiscoveryCandidate verifies
// the user-visible "Unknown card → real name" outcome: when a sparse
// incoming message arrives for an unmatched peer that has historical entity
// data, the enriched username/first_name flows through MatchPeer →
// UpdateDiscoveryCandidatesForPeer and lands on the external_contact row.
func TestSparseEntityEnrichment_EnrichedFieldsFlowToDiscoveryCandidate(t *testing.T) {
	env := setupSparseEnrichTest(t)
	ctx := context.Background()
	peerUserID, peerIDStr := uniqueTestIDs(t)
	chatID := peerUserID
	t.Cleanup(func() {
		cleanupSparseEnrichTestData(t, env, chatID)
		cleanupExternalBySource(t, env, peerIDStr)
	})

	// Seed history with rich entity data; the handler must consult this when
	// the new sparse update arrives.
	username := "connor" + peerIDStr
	firstName := "Connor" + peerIDStr
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

	// Sparse incoming message — no tg.User in entities.
	msg := makePrivateMessage(peerUserID, 70002, accelerated.GetCurrentTime(), "ping")
	entities := tg.Entities{Users: map[int64]*tg.User{}}
	update := &tg.UpdateNewMessage{Message: msg}

	require.NoError(t, env.handler.HandleNewMessage(ctx, entities, update))

	// The external_contact (discovery candidate) must reflect the ENRICHED
	// fields, not nils. Without the enrichment fix this row's first_name
	// would be NULL and metadata.username would be absent → "Unknown" card.
	got, err := env.externalRepo.GetBySource(ctx, "telegram", strconv.FormatInt(peerUserID, 10), nil)
	require.NoError(t, err)
	require.NotNil(t, got, "external_contact must be upserted with enriched fields")
	require.NotNil(t, got.FirstName, "first_name must be populated from history")
	assert.Equal(t, firstName, *got.FirstName)
	assert.Equal(t, "@"+username, got.Metadata["username"], "metadata.username must reflect enriched handle")
}
