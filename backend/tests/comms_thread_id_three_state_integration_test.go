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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file is Test 5 of the sqlc type-override arc (repo-shrink-sqlc-overrides
// PR1, plan §5.5/§7.5): it discriminates NULL, empty-string, and a real value
// on the two peer-field/thread-id sites the override flip made
// compiler-blind — a guard that regresses from `p != nil && *p != ""` to a
// bare `p != nil` compiles equally well but silently lets an empty string
// leak through as if it were a real value.

// TestCommsThreadID_ThreeStateDiscrimination pins the comms-message side:
// ListUnprocessedChatsByContactForSource must treat a NULL thread_id and an
// empty-string thread_id identically (both absent), and only surface the row
// carrying a real value. The underlying query (ListUnprocessedCommsChatsByContact)
// does NOT filter on thread_id nullness itself — both NULL and empty-string
// rows reach Go, and the repository wrapper is the only place the
// distinction is made.
func TestCommsThreadID_ThreeStateDiscrimination(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	gen, ns := migrationGenerator(t)
	contact, contactCleanup := seedMigrationContact(ctx, t, database, gen)
	comms := repository.NewCommsMessageRepository(database.Queries)
	t.Cleanup(func() {
		_ = comms.HardDeleteByContact(ctx, contact.ID)
		contactCleanup()
	})

	source := "gchat-" + ns
	sentAt := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)
	emptyThread := ""
	realThread := ns + "-thread"

	base := repository.UpsertCommsMessageParams{
		Source:           source,
		Direction:        repository.InteractionDirectionInbound,
		SentAt:           sentAt,
		MatchedContactID: contact.ID,
	}

	nullRow := base
	nullRow.ExternalID = ns + "-null"
	nullRow.ThreadID = nil
	_, err = comms.UpsertMessage(ctx, nullRow)
	require.NoError(t, err)

	emptyRow := base
	emptyRow.ExternalID = ns + "-empty"
	emptyRow.ThreadID = &emptyThread
	_, err = comms.UpsertMessage(ctx, emptyRow)
	require.NoError(t, err)

	valueRow := base
	valueRow.ExternalID = ns + "-value"
	valueRow.ThreadID = &realThread
	_, err = comms.UpsertMessage(ctx, valueRow)
	require.NoError(t, err)

	chats, err := comms.ListUnprocessedChatsByContactForSource(ctx, source, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{realThread}, chats,
		"NULL and empty-string thread_id must both be filtered out; only the real value surfaces")
}

// TestTelegramPeerEntity_ThreeStateDiscrimination pins the Telegram side:
// GetPeerEntityByUserID promotes both a NULL peer_username and a blank ("")
// one to nil, and only a genuinely non-blank value survives into
// PeerEntity.PeerUsername. The empty-string case is the discriminating one —
// a NULL-only guard would already pass the NULL and value cases.
func TestTelegramPeerEntity_ThreeStateDiscrimination(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	_, ns := migrationGenerator(t)
	repo := repository.NewTelegramMessageRepository(database.Queries)
	sentAt := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Microsecond)

	seedPeer := func(suffix string, username *string) int64 {
		t.Helper()
		peerID, _ := uniqueTestIDs(t, ns+"-"+suffix)
		t.Cleanup(func() { _ = repo.HardDeleteByChatIDRange(ctx, peerID, peerID) })
		_, err := repo.UpsertMessage(ctx, repository.UpsertTelegramMessageParams{
			TelegramMessageID: 1,
			TelegramChatID:    peerID,
			ChatType:          "private",
			MessageType:       "text",
			SentAt:            sentAt,
			PeerUserID:        &peerID,
			PeerUsername:      username,
		})
		require.NoError(t, err)
		return peerID
	}

	emptyUsername := ""
	realUsername := ns + "-realuser"

	nullPeer := seedPeer("null", nil)
	emptyPeer := seedPeer("empty", &emptyUsername)
	valuePeer := seedPeer("value", &realUsername)

	nullEntity, err := repo.GetPeerEntityByUserID(ctx, nullPeer)
	require.NoError(t, err)
	require.NotNil(t, nullEntity)
	assert.Nil(t, nullEntity.PeerUsername, "NULL peer_username must surface as nil")

	emptyEntity, err := repo.GetPeerEntityByUserID(ctx, emptyPeer)
	require.NoError(t, err)
	require.NotNil(t, emptyEntity)
	assert.Nil(t, emptyEntity.PeerUsername,
		"empty-string peer_username must be promoted to nil, not surfaced as a blank pointer")

	valueEntity, err := repo.GetPeerEntityByUserID(ctx, valuePeer)
	require.NoError(t, err)
	require.NotNil(t, valueEntity)
	require.NotNil(t, valueEntity.PeerUsername)
	assert.Equal(t, realUsername, *valueEntity.PeerUsername)
}
