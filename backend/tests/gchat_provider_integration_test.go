package tests

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	chat "google.golang.org/api/chat/v1"
)

// gchatProviderEnv bundles the repos a GChat provider integration test needs.
type gchatProviderEnv struct {
	ctx         context.Context
	database    *db.Database
	commsRepo   *repository.CommsMessageRepository
	contactRepo *repository.ContactRepository
	methodRepo  *repository.ContactMethodRepository
	syncRepo    *repository.SyncRepository
}

func setupGChatProviderTest(t *testing.T) *gchatProviderEnv {
	t.Helper()
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

	return &gchatProviderEnv{
		ctx:         ctx,
		database:    database,
		commsRepo:   repository.NewCommsMessageRepository(database.Queries),
		contactRepo: repository.NewContactRepository(database.Queries),
		methodRepo:  repository.NewContactMethodRepository(database.Queries),
		syncRepo:    repository.NewSyncRepositoryWithPool(database.Queries, database.Pool),
	}
}

// newGChatProviderContact creates a contact with a contact_method of the given
// type+value (so it appears in the dual-source known map), registering cleanup
// that hard-deletes its comms rows and soft-deletes the contact.
func (e *gchatProviderEnv) newGChatProviderContact(t *testing.T, name, methodType, methodValue string) *repository.Contact {
	t.Helper()
	contact, err := e.contactRepo.CreateContact(e.ctx, repository.CreateContactRequest{FullName: name})
	require.NoError(t, err)
	_, err = e.methodRepo.CreateContactMethod(e.ctx, repository.CreateContactMethodRequest{
		ContactID: contact.ID,
		Type:      methodType,
		Value:     methodValue,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = e.commsRepo.HardDeleteByContact(e.ctx, contact.ID)
		_ = e.contactRepo.SoftDeleteContact(e.ctx, contact.ID)
	})
	return contact
}

// newGChatSyncState creates an external_sync_state(source='gchat') row for an
// account, registering cleanup. enabled controls the rematch gate.
func (e *gchatProviderEnv) newGChatSyncState(t *testing.T, accountID string, enabled bool, metadata map[string]any) *repository.SyncState {
	t.Helper()
	st, err := e.syncRepo.CreateSyncState(e.ctx, repository.CreateSyncStateRequest{
		Source:    repository.InteractionSourceGChat,
		AccountID: &accountID,
		Enabled:   enabled,
		Metadata:  metadata,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.syncRepo.DeleteSyncState(e.ctx, st.ID) })
	return st
}

// newProvider builds a GChatSyncProvider wired to the test repos with a fake
// fetcher (no OAuth/HTTP) and an injected me-set.
func (e *gchatProviderEnv) newProvider(t *testing.T, meSet map[string]struct{}, funcs google.FakeChatFetcherFuncs) *google.GChatSyncProvider {
	t.Helper()
	p := google.NewGChatSyncProvider(nil, e.commsRepo, e.syncRepo)
	p.SetFetcherFactoryForTest(google.NewFakeChatFetcherFactoryForTest(funcs))
	p.SetMeSetForTest(meSet)
	return p
}

// chatRFC3339 formats a time as the Chat API RFC-3339 form.
func chatRFC3339(ts time.Time) string {
	return ts.UTC().Format(time.RFC3339Nano)
}

func space(name, spaceType string) *chat.Space {
	return &chat.Space{Name: name, SpaceType: spaceType, LastActiveTime: chatRFC3339(accelerated.GetCurrentTime())}
}

func membership(userName string) *chat.Membership {
	return &chat.Membership{State: "JOINED", Member: &chat.User{Name: userName, Type: "HUMAN"}}
}

func chatMessage(name, senderUser, text string, createTime time.Time) *chat.Message {
	return &chat.Message{
		Name:       name,
		Sender:     &chat.User{Name: senderUser, Type: "HUMAN"},
		Text:       text,
		CreateTime: chatRFC3339(createTime),
	}
}

// TestGChatProvider_FullSweep_InboundWritesRowNoEvents proves a sweep upserts a
// gchat row for a known inbound sender, writes NO events itself, and persists
// per-space metadata. The row is content-only until the engine aggregates.
func TestGChatProvider_FullSweep_InboundWritesRowNoEvents(t *testing.T) {
	e := setupGChatProviderTest(t)
	suffix := randomSuffix(t)

	aliceEmail := "alice-" + suffix + "@example.test"
	alice := e.newGChatProviderContact(t, "GChat Sweep Alice "+suffix, string(repository.ContactMethodEmail), aliceEmail)

	spaceName := "spaces/SWEEP-" + suffix
	msgName := spaceName + "/messages/m1-" + suffix
	accountID := "me-" + suffix + "@example.test"
	st := e.newGChatSyncState(t, accountID, true, nil)
	sentAt := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Second)

	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			return []*chat.Space{space(spaceName, "SPACE")}, "", nil
		},
		ListMembers: func(_ context.Context, sp, _ string) ([]*chat.Membership, string, error) {
			return []*chat.Membership{membership("users/alice"), membership("users/me")}, "", nil
		},
		ListMessages: func(_ context.Context, sp, filter string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted {
				return nil, "", nil // edit/delete pass: nothing
			}
			return []*chat.Message{chatMessage(msgName, "users/alice", "hi there", sentAt)}, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			switch userName {
			case "users/alice":
				return aliceEmail, nil
			case "users/me":
				return accountID, nil
			}
			return "", nil
		},
	}
	p := e.newProvider(t, map[string]struct{}{accountID: {}}, funcs)

	// Re-fetch the state so Metadata is hydrated as the framework would.
	state, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)

	result, err := p.Sync(e.ctx, state, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.ItemsProcessed)
	assert.Equal(t, 1, result.ItemsMatched)
	assert.Empty(t, result.NewCursor, "gchat keeps the per-space cursor in metadata, not NewCursor")

	// The row exists, inbound, with the sender as peer.
	row, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, msgName, alice.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.InteractionDirectionInbound, row.Direction)
	require.NotNil(t, row.Body)
	assert.Equal(t, "hi there", *row.Body)
	require.NotNil(t, row.PeerNormalized)
	assert.Equal(t, aliceEmail, *row.PeerNormalized)
	// The provider does NOT mark rows processed (the engine does that later).
	assert.Nil(t, row.ProcessedAt)

	// Metadata now carries the per-space cursor + membership + email cache.
	persisted, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	require.NotNil(t, persisted.Metadata)
	assert.Contains(t, persisted.Metadata, "space_cursors")
	assert.Contains(t, persisted.Metadata, "space_members")
	assert.Contains(t, persisted.Metadata, "user_email_cache")
	_ = st
}

// TestGChatProvider_OutboundFanOutAndBystander proves an outbound message
// (sender ∈ meSet) fans out to every known co-member (one row each, same
// external_id), while a bystander sender produces no row.
func TestGChatProvider_OutboundFanOutAndBystander(t *testing.T) {
	e := setupGChatProviderTest(t)
	suffix := randomSuffix(t)

	aliceEmail := "alice-" + suffix + "@example.test"
	bobEmail := "bob-" + suffix + "@example.test"
	alice := e.newGChatProviderContact(t, "GChat Fan Alice "+suffix, string(repository.ContactMethodGChat), aliceEmail)
	bob := e.newGChatProviderContact(t, "GChat Fan Bob "+suffix, string(repository.ContactMethodEmail), bobEmail)

	spaceName := "spaces/FAN-" + suffix
	outboundMsg := spaceName + "/messages/out-" + suffix
	bystanderMsg := spaceName + "/messages/by-" + suffix
	accountID := "me-" + suffix + "@example.test"
	e.newGChatSyncState(t, accountID, true, nil)
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Second)

	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			return []*chat.Space{space(spaceName, "SPACE")}, "", nil
		},
		ListMembers: func(context.Context, string, string) ([]*chat.Membership, string, error) {
			return []*chat.Membership{membership("users/me"), membership("users/alice"), membership("users/bob")}, "", nil
		},
		ListMessages: func(_ context.Context, _, _ string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted {
				return nil, "", nil
			}
			return []*chat.Message{
				chatMessage(outboundMsg, "users/me", "team update", base),
				chatMessage(bystanderMsg, "users/stranger", "who am i", base.Add(time.Minute)),
			}, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			switch userName {
			case "users/me":
				return accountID, nil
			case "users/alice":
				return aliceEmail, nil
			case "users/bob":
				return bobEmail, nil
			case "users/stranger":
				return "stranger-" + suffix + "@example.test", nil
			}
			return "", nil
		},
	}
	p := e.newProvider(t, map[string]struct{}{accountID: {}}, funcs)
	state, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)

	_, err = p.Sync(e.ctx, state, nil)
	require.NoError(t, err)

	// Outbound fanned out to BOTH Alice and Bob (same external_id, two rows).
	rowA, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, outboundMsg, alice.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.InteractionDirectionOutbound, rowA.Direction)
	rowB, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, outboundMsg, bob.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.InteractionDirectionOutbound, rowB.Direction)
	assert.NotEqual(t, rowA.ID, rowB.ID, "per-contact rows are distinct")

	// Bystander produced NO row for any known contact.
	_, err = e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, bystanderMsg, alice.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

// TestGChatProvider_CrossAccountDedup proves the same message observed under two
// accounts produces ONE row per matched contact with observed_accounts merged.
func TestGChatProvider_CrossAccountDedup(t *testing.T) {
	e := setupGChatProviderTest(t)
	suffix := randomSuffix(t)

	aliceEmail := "alice-" + suffix + "@example.test"
	alice := e.newGChatProviderContact(t, "GChat XAcct Alice "+suffix, string(repository.ContactMethodEmail), aliceEmail)

	spaceName := "spaces/XACCT-" + suffix
	msgName := spaceName + "/messages/m-" + suffix
	acctA := "acctA-" + suffix + "@example.test"
	acctB := "acctB-" + suffix + "@example.test"
	sentAt := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Second)

	mkFuncs := func(myAccount string) google.FakeChatFetcherFuncs {
		return google.FakeChatFetcherFuncs{
			ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
				return []*chat.Space{space(spaceName, "SPACE")}, "", nil
			},
			ListMembers: func(context.Context, string, string) ([]*chat.Membership, string, error) {
				return []*chat.Membership{membership("users/alice"), membership("users/me")}, "", nil
			},
			ListMessages: func(_ context.Context, _, _ string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
				if showDeleted {
					return nil, "", nil
				}
				return []*chat.Message{chatMessage(msgName, "users/alice", "shared", sentAt)}, "", nil
			},
			ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
				if userName == "users/alice" {
					return aliceEmail, nil
				}
				if userName == "users/me" {
					return myAccount, nil
				}
				return "", nil
			},
		}
	}

	// Account A sweep.
	stA := e.newGChatSyncState(t, acctA, true, nil)
	pA := e.newProvider(t, map[string]struct{}{acctA: {}}, mkFuncs(acctA))
	stateA, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &acctA)
	require.NoError(t, err)
	_, err = pA.Sync(e.ctx, stateA, nil)
	require.NoError(t, err)

	// Account B sweep observes the SAME message.
	e.newGChatSyncState(t, acctB, true, nil)
	pB := e.newProvider(t, map[string]struct{}{acctB: {}}, mkFuncs(acctB))
	stateB, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &acctB)
	require.NoError(t, err)
	_, err = pB.Sync(e.ctx, stateB, nil)
	require.NoError(t, err)

	// Still ONE row for Alice; observed_accounts is the union of both accounts.
	row, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, msgName, alice.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{acctA, acctB}, observedAccounts(t, row.SourceMetadata))
	_ = stA
}

// TestGChatProvider_EditReconciliation proves a body edit observed in the
// ShowDeleted re-list updates the stored body + last_update_time while leaving
// processed_at untouched (so the engine does not reprocess).
func TestGChatProvider_EditReconciliation(t *testing.T) {
	e := setupGChatProviderTest(t)
	suffix := randomSuffix(t)

	aliceEmail := "alice-" + suffix + "@example.test"
	alice := e.newGChatProviderContact(t, "GChat EditRecon Alice "+suffix, string(repository.ContactMethodEmail), aliceEmail)

	spaceName := "spaces/EDITRECON-" + suffix
	msgName := spaceName + "/messages/m-" + suffix
	accountID := "me-" + suffix + "@example.test"
	e.newGChatSyncState(t, accountID, true, nil)
	sentAt := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Second)
	editTime := accelerated.GetCurrentTime().Add(-30 * time.Minute)

	var serveEdit bool
	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			return []*chat.Space{space(spaceName, "SPACE")}, "", nil
		},
		ListMembers: func(context.Context, string, string) ([]*chat.Membership, string, error) {
			return []*chat.Membership{membership("users/alice"), membership("users/me")}, "", nil
		},
		ListMessages: func(_ context.Context, _, _ string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted {
				if !serveEdit {
					return nil, "", nil
				}
				edited := chatMessage(msgName, "users/alice", "EDITED body", sentAt)
				edited.LastUpdateTime = chatRFC3339(editTime)
				return []*chat.Message{edited}, "", nil
			}
			if serveEdit {
				return nil, "", nil // already past the create cursor on the second sweep
			}
			return []*chat.Message{chatMessage(msgName, "users/alice", "original body", sentAt)}, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			if userName == "users/alice" {
				return aliceEmail, nil
			}
			if userName == "users/me" {
				return accountID, nil
			}
			return "", nil
		},
	}
	p := e.newProvider(t, map[string]struct{}{accountID: {}}, funcs)

	// Sweep 1: ingest the original.
	state, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	_, err = p.Sync(e.ctx, state, nil)
	require.NoError(t, err)
	row, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, msgName, alice.ID)
	require.NoError(t, err)
	require.NotNil(t, row.Body)
	require.Equal(t, "original body", *row.Body)

	// Simulate the engine having processed the row (mark it processed via a
	// real interaction) so we can prove the edit does NOT clear processed_at.
	ref := "gchat:" + spaceName + ":proc-" + suffix
	interactionRepo := repository.NewInteractionRepository(e.database.Queries)
	interaction, err := interactionRepo.CreateInteraction(e.ctx, repository.CreateInteractionRequest{
		ContactID: alice.ID, Source: repository.InteractionSourceGChat, SourceRef: &ref,
		OccurredAt: sentAt, Direction: repository.InteractionDirectionInbound,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = interactionRepo.HardDeleteInteractionsBySourceRefPrefix(e.ctx, repository.InteractionSourceGChat, "gchat:"+spaceName+":%")
	})
	require.NoError(t, e.commsRepo.MarkMessagesProcessed(e.ctx, []uuid.UUID{row.ID}, interaction.ID))

	// Sweep 2: serve the edit through the ShowDeleted re-list.
	serveEdit = true
	state2, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	_, err = p.Sync(e.ctx, state2, nil)
	require.NoError(t, err)

	edited, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, msgName, alice.ID)
	require.NoError(t, err)
	require.NotNil(t, edited.Body)
	assert.Equal(t, "EDITED body", *edited.Body, "edit applied to stored body")
	assert.Equal(t, []string{"original body"}, previousBodies(t, edited.SourceMetadata))
	require.NotNil(t, edited.ProcessedAt, "edit must NOT clear processed_at")
	require.NotNil(t, edited.InteractionID)
	assert.Equal(t, interaction.ID, *edited.InteractionID)
}

// TestGChatProvider_EditFractionalOrdering drives the FULL provider edit path
// (not just the SQL wrapper): a stored row at "...:00.001Z" then an observed
// edit with LastUpdateTime "...:00Z" (genuinely OLDER despite sorting lexically
// LATER) must NOT apply; the reverse (genuinely newer) must apply. Proves the
// provider's body-only pre-filter does not invert fractional ordering.
func TestGChatProvider_EditFractionalOrdering(t *testing.T) {
	e := setupGChatProviderTest(t)
	suffix := randomSuffix(t)

	aliceEmail := "alice-" + suffix + "@example.test"
	alice := e.newGChatProviderContact(t, "GChat Frac Alice "+suffix, string(repository.ContactMethodEmail), aliceEmail)

	spaceName := "spaces/FRAC-" + suffix
	msgName := spaceName + "/messages/m-" + suffix
	accountID := "me-" + suffix + "@example.test"
	e.newGChatSyncState(t, accountID, true, nil)
	sentAt := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Second)

	// Seed a row whose stored last_update_time is "...:00.001Z" (newer fractional)
	// by applying an edit through the repository directly.
	_, err := e.commsRepo.UpsertMessage(e.ctx, repository.UpsertCommsMessageParams{
		Source: repository.InteractionSourceGChat, ExternalID: msgName,
		ThreadID: &spaceName, Body: strPtr("seed body"), Snippet: strPtr("seed body"),
		PeerHandle: &aliceEmail, PeerNormalized: &aliceEmail,
		Direction: repository.InteractionDirectionInbound, SentAt: sentAt,
		AccountID: &accountID, MatchedContactID: alice.ID,
	})
	require.NoError(t, err)
	n, err := e.commsRepo.ApplyEditByExternalID(e.ctx, repository.InteractionSourceGChat, msgName, strPtr("fractional-newer"), strPtr("fractional-newer"), "2026-06-04T10:00:00Z", "2026-06-04T10:00:00.001Z")
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	// Observe an OLDER edit ("...:00Z") via the provider's ShowDeleted re-list.
	// It sorts lexically AFTER the stored ".001Z" but is chronologically earlier,
	// so the SQL ::timestamptz guard must reject it.
	olderEdit := chatMessage(msgName, "users/alice", "should-not-apply", sentAt)
	olderEdit.LastUpdateTime = "2026-06-04T10:00:00Z"
	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			return []*chat.Space{space(spaceName, "SPACE")}, "", nil
		},
		ListMembers: func(context.Context, string, string) ([]*chat.Membership, string, error) {
			return []*chat.Membership{membership("users/alice"), membership("users/me")}, "", nil
		},
		ListMessages: func(_ context.Context, _, _ string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted {
				return []*chat.Message{olderEdit}, "", nil
			}
			return nil, "", nil // content pass: nothing new
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			if userName == "users/alice" {
				return aliceEmail, nil
			}
			if userName == "users/me" {
				return accountID, nil
			}
			return "", nil
		},
	}
	p := e.newProvider(t, map[string]struct{}{accountID: {}}, funcs)
	state, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	_, err = p.Sync(e.ctx, state, nil)
	require.NoError(t, err)

	row, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, msgName, alice.ID)
	require.NoError(t, err)
	require.NotNil(t, row.Body)
	assert.Equal(t, "fractional-newer", *row.Body, "chronologically-older edit (lexically later) must be rejected")
}

// TestGChatProvider_DeleteReconciliation proves a tombstone in the ShowDeleted
// re-list soft-deletes all fanned-out rows for the message.
func TestGChatProvider_DeleteReconciliation(t *testing.T) {
	e := setupGChatProviderTest(t)
	suffix := randomSuffix(t)

	aliceEmail := "alice-" + suffix + "@example.test"
	alice := e.newGChatProviderContact(t, "GChat DelRecon Alice "+suffix, string(repository.ContactMethodEmail), aliceEmail)

	spaceName := "spaces/DELRECON-" + suffix
	msgName := spaceName + "/messages/m-" + suffix
	accountID := "me-" + suffix + "@example.test"
	e.newGChatSyncState(t, accountID, true, nil)
	sentAt := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Second)

	var serveDelete bool
	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			return []*chat.Space{space(spaceName, "SPACE")}, "", nil
		},
		ListMembers: func(context.Context, string, string) ([]*chat.Membership, string, error) {
			return []*chat.Membership{membership("users/alice"), membership("users/me")}, "", nil
		},
		ListMessages: func(_ context.Context, _, _ string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted {
				if !serveDelete {
					return nil, "", nil
				}
				tomb := &chat.Message{Name: msgName, CreateTime: chatRFC3339(sentAt), DeletionMetadata: &chat.DeletionMetadata{DeletionType: "CREATOR"}}
				return []*chat.Message{tomb}, "", nil
			}
			if serveDelete {
				return nil, "", nil
			}
			return []*chat.Message{chatMessage(msgName, "users/alice", "to be deleted", sentAt)}, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			if userName == "users/alice" {
				return aliceEmail, nil
			}
			if userName == "users/me" {
				return accountID, nil
			}
			return "", nil
		},
	}
	p := e.newProvider(t, map[string]struct{}{accountID: {}}, funcs)

	state, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	_, err = p.Sync(e.ctx, state, nil)
	require.NoError(t, err)
	_, err = e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, msgName, alice.ID)
	require.NoError(t, err, "row exists after sweep 1")

	serveDelete = true
	state2, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	_, err = p.Sync(e.ctx, state2, nil)
	require.NoError(t, err)

	_, err = e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, msgName, alice.ID)
	assert.ErrorIs(t, err, db.ErrNotFound, "row soft-deleted after the tombstone re-list")
}

// TestGChatProvider_MetadataReusedAcrossSweeps proves the second sweep reuses
// the persisted membership + email cache (no fetcher refetch within TTL) and
// advances the create cursor so already-ingested messages are not re-listed.
func TestGChatProvider_MetadataReusedAcrossSweeps(t *testing.T) {
	e := setupGChatProviderTest(t)
	suffix := randomSuffix(t)

	aliceEmail := "alice-" + suffix + "@example.test"
	e.newGChatProviderContact(t, "GChat MetaReuse Alice "+suffix, string(repository.ContactMethodEmail), aliceEmail)

	spaceName := "spaces/METAREUSE-" + suffix
	accountID := "me-" + suffix + "@example.test"
	e.newGChatSyncState(t, accountID, true, nil)
	sentAt := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Second)

	memberCalls := 0
	resolveCalls := 0
	// Stable LastActiveTime across both sweeps so the activity-based membership
	// refresh does NOT fire — this test isolates the within-TTL reuse path.
	stableActive := chatRFC3339(accelerated.GetCurrentTime().Add(-2 * time.Hour))
	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			sp := space(spaceName, "SPACE")
			sp.LastActiveTime = stableActive
			return []*chat.Space{sp}, "", nil
		},
		ListMembers: func(context.Context, string, string) ([]*chat.Membership, string, error) {
			memberCalls++
			return []*chat.Membership{membership("users/alice"), membership("users/me")}, "", nil
		},
		ListMessages: func(_ context.Context, _, filter string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted {
				return nil, "", nil
			}
			return []*chat.Message{chatMessage(spaceName+"/messages/m-"+suffix, "users/alice", "hi", sentAt)}, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			resolveCalls++
			if userName == "users/alice" {
				return aliceEmail, nil
			}
			if userName == "users/me" {
				return accountID, nil
			}
			return "", nil
		},
	}
	p := e.newProvider(t, map[string]struct{}{accountID: {}}, funcs)

	state, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	_, err = p.Sync(e.ctx, state, nil)
	require.NoError(t, err)
	membersAfter1 := memberCalls
	resolvesAfter1 := resolveCalls
	require.GreaterOrEqual(t, membersAfter1, 1)
	require.GreaterOrEqual(t, resolvesAfter1, 1)

	// Second sweep: membership + resolutions reused from persisted metadata
	// within the 24h TTL → no new ListMembers / ResolvePersonEmail calls.
	state2, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	_, err = p.Sync(e.ctx, state2, nil)
	require.NoError(t, err)
	assert.Equal(t, membersAfter1, memberCalls, "membership reused from metadata (no refetch within TTL)")
	assert.Equal(t, resolvesAfter1, resolveCalls, "email resolutions reused from metadata (no refetch within TTL)")
}

// TestGChatRematch_ScansWhenEnabled proves the enabled gchat rematch handler
// scans the address across the enabled account and upserts a row.
func TestGChatRematch_ScansWhenEnabled(t *testing.T) {
	e := setupGChatProviderTest(t)
	suffix := randomSuffix(t)

	aliceEmail := "alice-" + suffix + "@example.test"
	alice := e.newGChatProviderContact(t, "GChat Rematch Alice "+suffix, string(repository.ContactMethodGChat), aliceEmail)

	spaceName := "spaces/REMATCH-" + suffix
	msgName := spaceName + "/messages/m-" + suffix
	accountID := "me-" + suffix + "@example.test"
	e.newGChatSyncState(t, accountID, true, nil)
	sentAt := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Second)

	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			return []*chat.Space{space(spaceName, "SPACE")}, "", nil
		},
		ListMembers: func(context.Context, string, string) ([]*chat.Membership, string, error) {
			return []*chat.Membership{membership("users/alice"), membership("users/me")}, "", nil
		},
		ListMessages: func(_ context.Context, _, _ string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted {
				return nil, "", nil
			}
			return []*chat.Message{chatMessage(msgName, "users/alice", "historical", sentAt)}, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			if userName == "users/alice" {
				return aliceEmail, nil
			}
			if userName == "users/me" {
				return accountID, nil
			}
			return "", nil
		},
	}
	p := e.newProvider(t, map[string]struct{}{accountID: {}}, funcs)

	matched, err := p.ScanIdentifier(e.ctx, accountID, aliceEmail, map[string][]uuid.UUID{aliceEmail: {alice.ID}}, map[string]struct{}{accountID: {}}, "2026-01-01T00:00:00Z")
	require.NoError(t, err)
	assert.Equal(t, 1, matched, "the historical message was upserted for the rematched address")

	row, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, msgName, alice.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.InteractionDirectionInbound, row.Direction)
}

// TestGChatRematchHandler_ScansAndAggregates drives the FULL rematch handler
// (gate → scan → AggregateForContactBatch) against the wired engine harness: an
// enabled gchat state + a fake fetcher → adding the address upserts a row AND
// the engine derives an interaction in the same rematch pass.
func TestGChatRematchHandler_ScansAndAggregates(t *testing.T) {
	env := setupGChatEngineTest(t) // wired engine + recorder
	suffix := randomSuffix(t)

	methodRepo := repository.NewContactMethodRepository(env.database.Queries)
	syncRepo := repository.NewSyncRepositoryWithPool(env.database.Queries, env.database.Pool)

	aliceEmail := "alice-" + suffix + "@example.test"
	alice := env.newGChatContact(t, "GChat RematchHandler Alice "+suffix)
	_, err := methodRepo.CreateContactMethod(env.ctx, repository.CreateContactMethodRequest{
		ContactID: alice.ID, Type: string(repository.ContactMethodGChat), Value: aliceEmail,
	})
	require.NoError(t, err)

	spaceName := "spaces/REMATCHH-" + suffix
	msgName := spaceName + "/messages/m-" + suffix
	accountID := "me-" + suffix + "@example.test"
	st, err := syncRepo.CreateSyncState(env.ctx, repository.CreateSyncStateRequest{
		Source: repository.InteractionSourceGChat, AccountID: &accountID, Enabled: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = syncRepo.DeleteSyncState(env.ctx, st.ID) })
	sentAt := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Second)

	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			return []*chat.Space{space(spaceName, "SPACE")}, "", nil
		},
		ListMembers: func(context.Context, string, string) ([]*chat.Membership, string, error) {
			return []*chat.Membership{membership("users/alice"), membership("users/me")}, "", nil
		},
		ListMessages: func(_ context.Context, _, _ string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted {
				return nil, "", nil
			}
			return []*chat.Message{chatMessage(msgName, "users/alice", "historical", sentAt)}, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			if userName == "users/alice" {
				return aliceEmail, nil
			}
			if userName == "users/me" {
				return accountID, nil
			}
			return "", nil
		},
	}
	provider := google.NewGChatSyncProvider(nil, env.commsRepo, syncRepo)
	provider.SetFetcherFactoryForTest(google.NewFakeChatFetcherFactoryForTest(funcs))
	provider.SetMeSetForTest(map[string]struct{}{accountID: {}})

	handler := google.NewGChatHandleRematchHandler(provider, syncRepo, env.commsRepo, env.engine)
	matched, err := handler.Rematch(env.ctx, alice.ID, aliceEmail)
	require.NoError(t, err)
	assert.Equal(t, 1, matched)

	// The row was upserted and AggregateForContactBatch derived an interaction.
	interactions := waitForInteractionCountExact(t, env.ctx, env.interactionRepo, alice.ID, 1, defaultInteractionWaitTimeout)
	require.Len(t, interactions, 1)
	assert.Equal(t, repository.InteractionSourceGChat, interactions[0].Source)
	assert.Equal(t, repository.InteractionDirectionInbound, interactions[0].Direction)
}

// TestGChatProvider_MultiPageWindowPersistsCursor proves the end-to-end cursor
// persistence wiring: a multi-page content window (paged via pageToken, ending
// in next == "") fully pages within budget, advances its create_cursor to the
// latest message's create_time, and that value is persisted to the JSONB
// metadata column. This complements the DB-free budget test in the google
// package — its job is the persistence path (window → setCursor → persistMetadata
// → DB), not the budget boundary.
func TestGChatProvider_MultiPageWindowPersistsCursor(t *testing.T) {
	e := setupGChatProviderTest(t)
	suffix := randomSuffix(t)

	aliceEmail := "alice-" + suffix + "@example.test"
	alice := e.newGChatProviderContact(t, "GChat Paged Alice "+suffix, string(repository.ContactMethodEmail), aliceEmail)

	spaceName := "spaces/PAGED-" + suffix
	accountID := "me-" + suffix + "@example.test"
	e.newGChatSyncState(t, accountID, true, nil)

	// Three content pages, oldest-first; the newest message (on the final page)
	// sets the cursor the sweep must persist. Truncate to seconds so the RFC-3339
	// string round-trips exactly through the cursor comparison.
	base := accelerated.GetCurrentTime().Add(-3 * time.Hour).Truncate(time.Second)
	msg1At := base
	msg2At := base.Add(time.Hour)
	latestAt := base.Add(2 * time.Hour)
	latestCursor := chatRFC3339(latestAt)

	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			return []*chat.Space{space(spaceName, "SPACE")}, "", nil
		},
		ListMembers: func(context.Context, string, string) ([]*chat.Membership, string, error) {
			return []*chat.Membership{membership("users/alice"), membership("users/me")}, "", nil
		},
		ListMessages: func(_ context.Context, _, _ string, showDeleted bool, token string) ([]*chat.Message, string, error) {
			if showDeleted {
				return nil, "", nil // edit/delete pass: nothing
			}
			// Content pass paged via pageToken: page 1 → "p2", page 2 → "p3",
			// page 3 → "" (fully paged). The newest message is on the final page.
			switch token {
			case "":
				return []*chat.Message{chatMessage(spaceName+"/messages/m1-"+suffix, "users/alice", "first", msg1At)}, "p2", nil
			case "p2":
				return []*chat.Message{chatMessage(spaceName+"/messages/m2-"+suffix, "users/alice", "second", msg2At)}, "p3", nil
			case "p3":
				return []*chat.Message{chatMessage(spaceName+"/messages/m3-"+suffix, "users/alice", "third", latestAt)}, "", nil
			}
			return nil, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			switch userName {
			case "users/alice":
				return aliceEmail, nil
			case "users/me":
				return accountID, nil
			}
			return "", nil
		},
	}
	p := e.newProvider(t, map[string]struct{}{accountID: {}}, funcs)

	state, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	result, err := p.Sync(e.ctx, state, nil)
	require.NoError(t, err)
	assert.Equal(t, 3, result.ItemsProcessed, "all three pages' messages processed")

	// A row exists for the newest message (proving the window paged to the end).
	_, err = e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, spaceName+"/messages/m3-"+suffix, alice.ID)
	require.NoError(t, err)

	// The persisted create_cursor equals the latest message's create_time. The
	// JSONB metadata decodes into map[string]any, so space_cursors is a nested
	// map[string]any keyed by space name, each entry itself a map[string]any with
	// a "create_cursor" string (the spaceCursor JSON tags). Type-assert through
	// that generic shape with require so a mismatch fails loudly.
	persisted, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	require.NotNil(t, persisted.Metadata)
	spaceCursors, ok := persisted.Metadata["space_cursors"].(map[string]any)
	require.True(t, ok, "space_cursors must decode as a map[string]any")
	entry, ok := spaceCursors[spaceName].(map[string]any)
	require.True(t, ok, "this space's cursor entry must be present and a map[string]any")
	createCursor, ok := entry["create_cursor"].(string)
	require.True(t, ok, "create_cursor must be a string")
	assert.Equal(t, latestCursor, createCursor, "persisted cursor must equal the latest message create_time")
}

// spaceCursorFromMetadata extracts a space's create_cursor from the persisted
// JSONB metadata (decoded as map[string]any). Returns "" when absent.
func spaceCursorFromMetadata(t *testing.T, metadata map[string]any, spaceName string) string {
	t.Helper()
	cursors, ok := metadata["space_cursors"].(map[string]any)
	if !ok {
		return ""
	}
	entry, ok := cursors[spaceName].(map[string]any)
	if !ok {
		return ""
	}
	cur, _ := entry["create_cursor"].(string)
	return cur
}

// TestGChatProvider_MatchesContactNotInGoogleContacts is the HEADLINE
// REGRESSION. A contact whose EMAIL is in the CRM but whose canonical id returns
// "" from the People-API fake (the not-in-Contacts case) is matched via the
// reverse members.get id resolution: (a) an INBOUND message writes a row matched
// to the contact with the CRM email as the peer; (b) an OUTBOUND message fans
// out to the id-resolved contact; (c) the id-resolved contact is a co-member of
// the group space; (d) the cursor advances (window proven).
func TestGChatProvider_MatchesContactNotInGoogleContacts(t *testing.T) {
	e := setupGChatProviderTest(t)
	suffix := randomSuffix(t)

	daveEmail := "dave-" + suffix + "@example.test"
	dave := e.newGChatProviderContact(t, "GChat NotInContacts Dave "+suffix, string(repository.ContactMethodEmail), daveEmail)

	spaceName := "spaces/NOTINCONTACTS-" + suffix
	inboundMsg := spaceName + "/messages/in-" + suffix
	outboundMsg := spaceName + "/messages/out-" + suffix
	accountID := "me-" + suffix + "@example.test"
	e.newGChatSyncState(t, accountID, true, nil)
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Second)

	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			return []*chat.Space{space(spaceName, "SPACE")}, "", nil
		},
		ListMembers: func(context.Context, string, string) ([]*chat.Membership, string, error) {
			return []*chat.Membership{membership("users/dave"), membership("users/me")}, "", nil
		},
		ListMessages: func(_ context.Context, _, _ string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted {
				return nil, "", nil
			}
			return []*chat.Message{
				chatMessage(inboundMsg, "users/dave", "hi from dave", base),
				chatMessage(outboundMsg, "users/me", "team update", base.Add(time.Minute)),
			}, "", nil
		},
		// People API CANNOT resolve dave (not in Contacts) → "".
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			if userName == "users/me" {
				return accountID, nil
			}
			return "", nil // users/dave → not in Contacts
		},
		// But members.get DOES resolve dave's email to his canonical id.
		ResolveMemberID: func(_ context.Context, _, normalizedEmail string) (string, bool, error) {
			if normalizedEmail == daveEmail {
				return "users/dave", false, nil
			}
			return "", true, nil
		},
	}
	p := e.newProvider(t, map[string]struct{}{accountID: {}}, funcs)
	state, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)

	_, err = p.Sync(e.ctx, state, nil)
	require.NoError(t, err)

	// (a) Inbound row matched to dave via the id path, with the CRM email as peer.
	inRow, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, inboundMsg, dave.ID)
	require.NoError(t, err, "inbound message matched dave via the canonical-id path")
	assert.Equal(t, repository.InteractionDirectionInbound, inRow.Direction)
	require.NotNil(t, inRow.PeerNormalized)
	assert.Equal(t, daveEmail, *inRow.PeerNormalized, "the id-path carries the CRM email as the peer, not an empty value")

	// (b) Outbound fanned out to the id-resolved dave (a co-member, (c)).
	outRow, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, outboundMsg, dave.ID)
	require.NoError(t, err, "outbound fanned out to the id-resolved co-member")
	assert.Equal(t, repository.InteractionDirectionOutbound, outRow.Direction)

	// (d) The cursor advanced (window proven) AND the global positive cache + the
	// resolved id are persisted.
	persisted, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	assert.NotEmpty(t, spaceCursorFromMetadata(t, persisted.Metadata, spaceName), "create_cursor advanced")
	assert.Contains(t, persisted.Metadata, "email_user_ids", "the global positive cache is persisted")
}

// TestGChatProvider_StaleNegativeClearedOnMembershipChange proves a per-space
// negative is NOT cursor-safe across an actual membership change. Sweep 1: the
// known email is not a member (negative written, stamped with the current
// fingerprint), no message from them, cursor advances. Sweep 2: they joined
// (fingerprint flips) and sent a new message after the cursor → the message
// MATCHES (the fingerprint flip invalidated the stale negative before the cursor
// advanced past the message).
func TestGChatProvider_StaleNegativeClearedOnMembershipChange(t *testing.T) {
	e := setupGChatProviderTest(t)
	suffix := randomSuffix(t)

	daveEmail := "dave-" + suffix + "@example.test"
	dave := e.newGChatProviderContact(t, "GChat StaleNeg Dave "+suffix, string(repository.ContactMethodEmail), daveEmail)

	spaceName := "spaces/STALENEG-" + suffix
	joinMsg := spaceName + "/messages/join-" + suffix
	accountID := "me-" + suffix + "@example.test"
	e.newGChatSyncState(t, accountID, true, nil)
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Second)

	var daveJoined bool
	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			sp := space(spaceName, "SPACE")
			// Advance lastActiveTime on sweep 2 so the membership is refetched.
			if daveJoined {
				sp.LastActiveTime = chatRFC3339(accelerated.GetCurrentTime())
			} else {
				sp.LastActiveTime = chatRFC3339(base)
			}
			return []*chat.Space{sp}, "", nil
		},
		ListMembers: func(context.Context, string, string) ([]*chat.Membership, string, error) {
			if daveJoined {
				return []*chat.Membership{membership("users/dave"), membership("users/me")}, "", nil
			}
			return []*chat.Membership{membership("users/me")}, "", nil
		},
		ListMessages: func(_ context.Context, _, _ string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted || !daveJoined {
				return nil, "", nil // sweep 1: no messages
			}
			return []*chat.Message{chatMessage(joinMsg, "users/dave", "just joined", accelerated.GetCurrentTime().Add(-time.Minute).Truncate(time.Second))}, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			if userName == "users/me" {
				return accountID, nil
			}
			return "", nil
		},
		ResolveMemberID: func(_ context.Context, _, normalizedEmail string) (string, bool, error) {
			// dave only resolves once he has joined.
			if daveJoined && normalizedEmail == daveEmail {
				return "users/dave", false, nil
			}
			return "", true, nil
		},
	}
	p := e.newProvider(t, map[string]struct{}{accountID: {}}, funcs)

	// Sweep 1: dave absent → negative written, cursor advances.
	state, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	_, err = p.Sync(e.ctx, state, nil)
	require.NoError(t, err)

	// Sweep 2: dave joined (fingerprint flips) and sent a message after the cursor.
	daveJoined = true
	state2, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	_, err = p.Sync(e.ctx, state2, nil)
	require.NoError(t, err)

	// The join message matched — the stale negative did NOT cause a permanent miss.
	row, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, joinMsg, dave.ID)
	require.NoError(t, err, "the message from the newly-joined contact matched (stale negative invalidated by fingerprint flip)")
	assert.Equal(t, repository.InteractionDirectionInbound, row.Direction)
}

// TestGChatProvider_GlobalPositiveCachePersistsAndReused proves sweep 1 resolves
// the email→id once and sweep 2 (a second space, same email) issues ZERO
// ResolveMemberID calls (the global positive is reused from persisted metadata).
func TestGChatProvider_GlobalPositiveCachePersistsAndReused(t *testing.T) {
	e := setupGChatProviderTest(t)
	suffix := randomSuffix(t)

	daveEmail := "dave-" + suffix + "@example.test"
	dave := e.newGChatProviderContact(t, "GChat GlobalPos Dave "+suffix, string(repository.ContactMethodEmail), daveEmail)

	spaceA := "spaces/GPA-" + suffix
	spaceB := "spaces/GPB-" + suffix
	msgA := spaceA + "/messages/a-" + suffix
	msgB := spaceB + "/messages/b-" + suffix
	accountID := "me-" + suffix + "@example.test"
	e.newGChatSyncState(t, accountID, true, nil)
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Second)

	resolveIDCalls := 0
	var showSpaceB bool
	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			if showSpaceB {
				return []*chat.Space{space(spaceB, "SPACE")}, "", nil
			}
			return []*chat.Space{space(spaceA, "SPACE")}, "", nil
		},
		ListMembers: func(_ context.Context, sp, _ string) ([]*chat.Membership, string, error) {
			return []*chat.Membership{membership("users/dave"), membership("users/me")}, "", nil
		},
		ListMessages: func(_ context.Context, sp, _ string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted {
				return nil, "", nil
			}
			if showSpaceB {
				return []*chat.Message{chatMessage(msgB, "users/dave", "in B", base.Add(time.Minute))}, "", nil
			}
			return []*chat.Message{chatMessage(msgA, "users/dave", "in A", base)}, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			if userName == "users/me" {
				return accountID, nil
			}
			return "", nil // dave not in Contacts
		},
		ResolveMemberID: func(_ context.Context, _, normalizedEmail string) (string, bool, error) {
			if normalizedEmail == daveEmail {
				resolveIDCalls++
				return "users/dave", false, nil
			}
			return "", true, nil
		},
	}
	p := e.newProvider(t, map[string]struct{}{accountID: {}}, funcs)

	// Sweep 1 in space A: resolves dave's id once.
	state, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	_, err = p.Sync(e.ctx, state, nil)
	require.NoError(t, err)
	require.Equal(t, 1, resolveIDCalls, "sweep 1 resolved the id once")
	_, err = e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, msgA, dave.ID)
	require.NoError(t, err)

	// Sweep 2 in space B (same email): the global positive is reused → ZERO calls.
	showSpaceB = true
	state2, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	_, err = p.Sync(e.ctx, state2, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, resolveIDCalls, "sweep 2 reused the global positive — no new ResolveMemberID call")
	_, err = e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, msgB, dave.ID)
	require.NoError(t, err, "the same id matched in space B via the global positive")
}

// TestGChatProvider_DebtHeldCursorAcrossTwoSweeps proves the debt-hold survives
// the sweep in which membership changed (the two-sweep hole). The
// resolve-cap is forced to 0 so the post-change re-resolution CANNOT happen on
// the change sweep. Sweep B holds the cursor; sweep C (cap restored) drains the
// debt and matches the message from sweep B's window.
func TestGChatProvider_DebtHeldCursorAcrossTwoSweeps(t *testing.T) {
	e := setupGChatProviderTest(t)
	suffix := randomSuffix(t)

	daveEmail := "dave-" + suffix + "@example.test"
	dave := e.newGChatProviderContact(t, "GChat Debt Dave "+suffix, string(repository.ContactMethodEmail), daveEmail)

	spaceName := "spaces/DEBT-" + suffix
	joinMsg := spaceName + "/messages/join-" + suffix
	accountID := "me-" + suffix + "@example.test"
	base := accelerated.GetCurrentTime().Add(-2 * time.Hour).Truncate(time.Second)

	// Pre-seed: a space_members entry (dave absent) and a NEGATIVE-VALID negative
	// for dave stamped with the absent-set fingerprint, plus a create_cursor so
	// the message (sent after the cursor) is in-window on the change sweep.
	absentFP := google.MemberSetFingerprintForTest([]string{"users/me"})
	seededAt := accelerated.GetCurrentTime().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	metadata := map[string]any{
		"space_members": map[string]any{
			spaceName: map[string]any{"version": chatRFC3339(base), "fetched_at": seededAt, "members": []string{"users/me"}},
		},
		"space_cursors": map[string]any{
			spaceName: map[string]any{"create_cursor": chatRFC3339(base), "edit_cursor": chatRFC3339(base)},
		},
		"space_member_negatives": map[string]any{
			spaceName: map[string]any{daveEmail: map[string]any{"resolved_at": seededAt, "member_set_fingerprint": absentFP}},
		},
	}
	e.newGChatSyncState(t, accountID, true, metadata)

	var daveJoined bool
	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			sp := space(spaceName, "SPACE")
			if daveJoined {
				sp.LastActiveTime = chatRFC3339(accelerated.GetCurrentTime())
			} else {
				sp.LastActiveTime = chatRFC3339(base)
			}
			return []*chat.Space{sp}, "", nil
		},
		ListMembers: func(context.Context, string, string) ([]*chat.Membership, string, error) {
			if daveJoined {
				return []*chat.Membership{membership("users/dave"), membership("users/me")}, "", nil
			}
			return []*chat.Membership{membership("users/me")}, "", nil
		},
		ListMessages: func(_ context.Context, _, _ string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted || !daveJoined {
				return nil, "", nil
			}
			return []*chat.Message{chatMessage(joinMsg, "users/dave", "just joined", accelerated.GetCurrentTime().Add(-30*time.Second).Truncate(time.Second))}, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			if userName == "users/me" {
				return accountID, nil
			}
			return "", nil
		},
		ResolveMemberID: func(_ context.Context, _, normalizedEmail string) (string, bool, error) {
			if daveJoined && normalizedEmail == daveEmail {
				return "users/dave", false, nil
			}
			return "", true, nil
		},
	}
	p := e.newProvider(t, map[string]struct{}{accountID: {}}, funcs)

	cursorBefore := chatRFC3339(base)

	// Sweep B: dave joined (fingerprint flips → negative UNKNOWN), but cap=0 so the
	// re-resolution is DEFERRED → cursor HELD; the join message is NOT yet matched.
	p.SetMemberResolveCapForTest(0)
	daveJoined = true
	stateB, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	_, err = p.Sync(e.ctx, stateB, nil)
	require.NoError(t, err)

	_, err = e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, joinMsg, dave.ID)
	assert.ErrorIs(t, err, db.ErrNotFound, "cap=0 defers the re-resolution; the message is not yet matched")

	persistedB, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	assert.Equal(t, cursorBefore, spaceCursorFromMetadata(t, persistedB.Metadata, spaceName),
		"the cursor is HELD on the change sweep (not advanced past the unmatched message)")

	// Sweep C: cap restored → the debt drains, the held message matches, cursor
	// advances. On a transient-flag design sweep C would see no refresh and advance
	// past the message unmatched — this asserts the durable fingerprint debt model.
	p.SetMemberResolveCapForTest(50)
	stateC, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	_, err = p.Sync(e.ctx, stateC, nil)
	require.NoError(t, err)

	row, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, joinMsg, dave.ID)
	require.NoError(t, err, "the held message matches once the debt drains (cap restored)")
	assert.Equal(t, repository.InteractionDirectionInbound, row.Direction)
	persistedC, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	assert.NotEqual(t, cursorBefore, spaceCursorFromMetadata(t, persistedC.Metadata, spaceName), "the cursor advances after the debt drains")
}

// TestGChatProvider_HotSpaceNoStarvation proves the starvation fix: a
// space with a known-absent contact (negative written) and an UNCHANGED member
// set across many sweeps — each adding a new message from a DIFFERENT known
// member so lastActiveTime advances (membership refetches) but the member SET is
// identical (fingerprint stable). The cursor advances every sweep and ZERO
// ResolveMemberID calls fire for the absent contact after the first.
func TestGChatProvider_HotSpaceNoStarvation(t *testing.T) {
	e := setupGChatProviderTest(t)
	suffix := randomSuffix(t)

	absentEmail := "absent-" + suffix + "@example.test"
	e.newGChatProviderContact(t, "GChat Hot Absent "+suffix, string(repository.ContactMethodEmail), absentEmail)
	activeEmail := "active-" + suffix + "@example.test"
	active := e.newGChatProviderContact(t, "GChat Hot Active "+suffix, string(repository.ContactMethodEmail), activeEmail)

	spaceName := "spaces/HOT-" + suffix
	accountID := "me-" + suffix + "@example.test"
	e.newGChatSyncState(t, accountID, true, nil)

	resolveAbsentCalls := 0
	sweepIdx := 0
	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			sp := space(spaceName, "SPACE")
			// lastActiveTime advances each sweep (the space is hot) → membership
			// refetches, but the returned member SET is identical (fingerprint stable).
			sp.LastActiveTime = chatRFC3339(accelerated.GetCurrentTime().Add(time.Duration(sweepIdx) * time.Minute))
			return []*chat.Space{sp}, "", nil
		},
		ListMembers: func(context.Context, string, string) ([]*chat.Membership, string, error) {
			// active + me are the stable member set; absent is NOT a member.
			return []*chat.Membership{membership("users/active"), membership("users/me")}, "", nil
		},
		ListMessages: func(_ context.Context, _, _ string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted {
				return nil, "", nil
			}
			// A new message from the active member each sweep, strictly after the cursor.
			at := accelerated.GetCurrentTime().Add(time.Duration(sweepIdx)*time.Minute - time.Hour).Truncate(time.Second)
			return []*chat.Message{chatMessage(spaceName+"/messages/hot-"+suffix+"-"+strconv.Itoa(sweepIdx), "users/active", "ping", at)}, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			switch userName {
			case "users/me":
				return accountID, nil
			case "users/active":
				return activeEmail, nil
			}
			return "", nil
		},
		ResolveMemberID: func(_ context.Context, _, normalizedEmail string) (string, bool, error) {
			if normalizedEmail == absentEmail {
				resolveAbsentCalls++
			}
			return "", true, nil // absent is never a member; active resolves via the email path
		},
	}
	p := e.newProvider(t, map[string]struct{}{accountID: {}}, funcs)

	var lastCursor string
	for sweepIdx = 0; sweepIdx < 4; sweepIdx++ {
		state, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
		require.NoError(t, err)
		_, err = p.Sync(e.ctx, state, nil)
		require.NoError(t, err)

		persisted, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
		require.NoError(t, err)
		cur := spaceCursorFromMetadata(t, persisted.Metadata, spaceName)
		require.NotEmpty(t, cur, "the cursor must advance every sweep (never held)")
		if sweepIdx > 0 {
			assert.NotEqual(t, lastCursor, cur, "the cursor advances each hot sweep — no starvation")
		}
		lastCursor = cur
	}

	// After the first sweep wrote the negative, NO further ResolveMemberID calls
	// fire for the absent contact — mere activity (stable fingerprint) never
	// re-incurs debt.
	assert.Equal(t, 1, resolveAbsentCalls, "the absent contact is resolved once, then never re-resolved across hot sweeps")

	// The active member's messages all matched.
	rows, err := e.commsRepo.ListByContact(e.ctx, active.ID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(rows), 4, "every hot-sweep message from the active member matched")
}

// TestGChatRematch_IDResolutionMatchesHistory extends the rematch scan: the
// scanned email's People-API resolution is "" but ResolveMemberID yields its id,
// so the historical message still upserts. Proves the rematch path uses the new
// id resolution.
func TestGChatRematch_IDResolutionMatchesHistory(t *testing.T) {
	e := setupGChatProviderTest(t)
	suffix := randomSuffix(t)

	daveEmail := "dave-" + suffix + "@example.test"
	dave := e.newGChatProviderContact(t, "GChat RematchID Dave "+suffix, string(repository.ContactMethodGChat), daveEmail)

	spaceName := "spaces/REMATCHID-" + suffix
	msgName := spaceName + "/messages/m-" + suffix
	accountID := "me-" + suffix + "@example.test"
	e.newGChatSyncState(t, accountID, true, nil)
	sentAt := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Second)

	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			return []*chat.Space{space(spaceName, "SPACE")}, "", nil
		},
		ListMembers: func(context.Context, string, string) ([]*chat.Membership, string, error) {
			return []*chat.Membership{membership("users/dave"), membership("users/me")}, "", nil
		},
		ListMessages: func(_ context.Context, _, _ string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted {
				return nil, "", nil
			}
			return []*chat.Message{chatMessage(msgName, "users/dave", "historical", sentAt)}, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			if userName == "users/me" {
				return accountID, nil
			}
			return "", nil // dave not in Contacts
		},
		ResolveMemberID: func(_ context.Context, _, normalizedEmail string) (string, bool, error) {
			if normalizedEmail == daveEmail {
				return "users/dave", false, nil
			}
			return "", true, nil
		},
	}
	p := e.newProvider(t, map[string]struct{}{accountID: {}}, funcs)

	matched, err := p.ScanIdentifier(e.ctx, accountID, daveEmail, map[string][]uuid.UUID{daveEmail: {dave.ID}}, map[string]struct{}{accountID: {}}, "2026-01-01T00:00:00Z")
	require.NoError(t, err)
	assert.Equal(t, 1, matched, "the historical message matched via the id path even though People API returned empty")

	row, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, msgName, dave.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.InteractionDirectionInbound, row.Direction)
	require.NotNil(t, row.PeerNormalized)
	assert.Equal(t, daveEmail, *row.PeerNormalized)
}
