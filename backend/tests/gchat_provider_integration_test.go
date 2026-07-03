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
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	chat "google.golang.org/api/chat/v1"
)

// gchatProviderEnv bundles the repos a GChat provider integration test needs.
type gchatProviderEnv struct {
	ctx            context.Context
	database       *db.Database
	gen            *factory.Generator
	commsRepo      *repository.CommsMessageRepository
	contactRepo    *repository.ContactRepository
	contactService *service.ContactService
	syncRepo       *repository.SyncRepository
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

	contactRepo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	// nil bus + nil rematch: a single-tx multi-method seed write, no River client.
	assertSvc, cache := buildKnowledgeDeps(t, database, nil)
	contactService := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, contactTaskRepo, nil, nil,
		nil, assertSvc, cache, nil)

	gen, _ := migrationGenerator(t)

	return &gchatProviderEnv{
		ctx:            ctx,
		database:       database,
		gen:            gen,
		commsRepo:      repository.NewCommsMessageRepository(database.Queries),
		contactRepo:    contactRepo,
		contactService: contactService,
		syncRepo:       repository.NewSyncRepositoryWithPool(database.Queries, database.Pool),
	}
}

// newGChatProviderContact seeds a namespaced contact with one contact_method of
// the given type whose value is a factory-namespaced email (so it appears in the
// dual-source known map), and returns it plus that value (the match key). The
// method type varies per test (email vs gchat); the value is always an
// email-shaped address the provider resolves a chat user to.
func (e *gchatProviderEnv) newGChatProviderContact(t *testing.T, methodType string) (*repository.Contact, string) {
	t.Helper()
	spec := e.gen.Contact(factory.WithEmail())
	value := spec.Email
	contact, _, err := e.contactService.CreateContact(e.ctx, repository.CreateContactRequest{
		FullName: spec.FullName,
	}, []service.ContactMethodInput{{Type: methodType, Value: value}})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = e.commsRepo.HardDeleteByContact(e.ctx, contact.ID)
		_ = e.contactRepo.HardDeleteContact(e.ctx, contact.ID)
	})
	return contact, value
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
	t.Parallel()
	e := setupGChatProviderTest(t)
	prefix := e.gen.Prefix()

	alice, aliceEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))

	spaceName := "spaces/SWEEP-" + prefix
	msgName := spaceName + "/messages/m1-" + prefix
	accountID := prefix + "me@synthetic.example"
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
	t.Parallel()
	e := setupGChatProviderTest(t)
	prefix := e.gen.Prefix()

	alice, aliceEmail := e.newGChatProviderContact(t, string(repository.ContactMethodGChat))
	bob, bobEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))

	spaceName := "spaces/FAN-" + prefix
	outboundMsg := spaceName + "/messages/out-" + prefix
	bystanderMsg := spaceName + "/messages/by-" + prefix
	accountID := prefix + "me@synthetic.example"
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
				return "stranger-" + prefix + "@synthetic.example", nil
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
	t.Parallel()
	e := setupGChatProviderTest(t)
	prefix := e.gen.Prefix()

	alice, aliceEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))

	spaceName := "spaces/XACCT-" + prefix
	msgName := spaceName + "/messages/m-" + prefix
	acctA := "acctA-" + prefix + "@synthetic.example"
	acctB := "acctB-" + prefix + "@synthetic.example"
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
	t.Parallel()
	e := setupGChatProviderTest(t)
	prefix := e.gen.Prefix()

	alice, aliceEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))

	spaceName := "spaces/EDITRECON-" + prefix
	msgName := spaceName + "/messages/m-" + prefix
	accountID := prefix + "me@synthetic.example"
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
	ref := "gchat:" + spaceName + ":proc-" + prefix
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
	t.Parallel()
	e := setupGChatProviderTest(t)
	prefix := e.gen.Prefix()

	alice, aliceEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))

	spaceName := "spaces/FRAC-" + prefix
	msgName := spaceName + "/messages/m-" + prefix
	accountID := prefix + "me@synthetic.example"
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
	t.Parallel()
	e := setupGChatProviderTest(t)
	prefix := e.gen.Prefix()

	alice, aliceEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))

	spaceName := "spaces/DELRECON-" + prefix
	msgName := spaceName + "/messages/m-" + prefix
	accountID := prefix + "me@synthetic.example"
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
	t.Parallel()
	e := setupGChatProviderTest(t)
	prefix := e.gen.Prefix()

	_, aliceEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))

	spaceName := "spaces/METAREUSE-" + prefix
	accountID := prefix + "me@synthetic.example"
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
			return []*chat.Message{chatMessage(spaceName+"/messages/m-"+prefix, "users/alice", "hi", sentAt)}, "", nil
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
	t.Parallel()
	e := setupGChatProviderTest(t)
	prefix := e.gen.Prefix()

	alice, aliceEmail := e.newGChatProviderContact(t, string(repository.ContactMethodGChat))

	spaceName := "spaces/REMATCH-" + prefix
	msgName := spaceName + "/messages/m-" + prefix
	accountID := prefix + "me@synthetic.example"
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
	t.Parallel()
	env := setupGChatEngineTest(t) // wired engine + recorder
	gen, _ := migrationGenerator(t)
	prefix := gen.Prefix()

	methodRepo := repository.NewContactMethodRepository(env.database.Queries)
	syncRepo := repository.NewSyncRepositoryWithPool(env.database.Queries, env.database.Pool)

	spec := gen.Contact(factory.WithEmail())
	aliceEmail := spec.Email
	alice := env.newGChatContact(t, spec.FullName)
	_, err := methodRepo.CreateContactMethod(env.ctx, repository.CreateContactMethodRequest{
		ContactID: alice.ID, Type: string(repository.ContactMethodGChat), Value: aliceEmail,
	})
	require.NoError(t, err)

	spaceName := "spaces/REMATCHH-" + prefix
	msgName := spaceName + "/messages/m-" + prefix
	accountID := prefix + "me@synthetic.example"
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
	t.Parallel()
	e := setupGChatProviderTest(t)
	prefix := e.gen.Prefix()

	alice, aliceEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))

	spaceName := "spaces/PAGED-" + prefix
	accountID := prefix + "me@synthetic.example"
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
				return []*chat.Message{chatMessage(spaceName+"/messages/m1-"+prefix, "users/alice", "first", msg1At)}, "p2", nil
			case "p2":
				return []*chat.Message{chatMessage(spaceName+"/messages/m2-"+prefix, "users/alice", "second", msg2At)}, "p3", nil
			case "p3":
				return []*chat.Message{chatMessage(spaceName+"/messages/m3-"+prefix, "users/alice", "third", latestAt)}, "", nil
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
	_, err = e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, spaceName+"/messages/m3-"+prefix, alice.ID)
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
	t.Parallel()
	e := setupGChatProviderTest(t)
	prefix := e.gen.Prefix()

	dave, daveEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))

	spaceName := "spaces/NOTINCONTACTS-" + prefix
	inboundMsg := spaceName + "/messages/in-" + prefix
	outboundMsg := spaceName + "/messages/out-" + prefix
	accountID := prefix + "me@synthetic.example"
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
	t.Parallel()
	e := setupGChatProviderTest(t)
	prefix := e.gen.Prefix()

	dave, daveEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))

	spaceName := "spaces/STALENEG-" + prefix
	joinMsg := spaceName + "/messages/join-" + prefix
	accountID := prefix + "me@synthetic.example"
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
	t.Parallel()
	e := setupGChatProviderTest(t)
	prefix := e.gen.Prefix()

	dave, daveEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))

	spaceA := "spaces/GPA-" + prefix
	spaceB := "spaces/GPB-" + prefix
	msgA := spaceA + "/messages/a-" + prefix
	msgB := spaceB + "/messages/b-" + prefix
	accountID := prefix + "me@synthetic.example"
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

// TestGChatProvider_ColdStartProcessesAndAdvancesDespiteResolveDebt is the
// HEADLINE REGRESSION for the incident. Multiple spaces, the resolve-cap forced to
// 0 so id-resolution is DEFERRED for every space. Each space has a message from a
// member the People API CAN resolve (the email-path match, which does not depend
// on id-resolution). Asserts: every space's message is processed AND matched (no
// freeze), every space's cursor advances, and the result counts are non-zero — the
// exact prod failure (processed=0 with all spaces held) turned into a passing
// assertion.
func TestGChatProvider_ColdStartProcessesAndAdvancesDespiteResolveDebt(t *testing.T) {
	t.Parallel()
	e := setupGChatProviderTest(t)
	prefix := e.gen.Prefix()

	alice, aliceEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))
	// A second known contact whose id is NOT in Contacts (would need members.get) —
	// deferred by cap=0, but must NOT hold any cursor.
	_, daveEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))

	spaceA := "spaces/COLDA-" + prefix
	spaceB := "spaces/COLDB-" + prefix
	msgA := spaceA + "/messages/a-" + prefix
	msgB := spaceB + "/messages/b-" + prefix
	accountID := prefix + "me@synthetic.example"
	e.newGChatSyncState(t, accountID, true, nil)
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Second)

	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			return []*chat.Space{space(spaceA, "SPACE"), space(spaceB, "SPACE")}, "", nil
		},
		ListMembers: func(_ context.Context, spaceName, _ string) ([]*chat.Membership, string, error) {
			// Both spaces include alice (People-resolvable), dave (needs members.get),
			// and me.
			return []*chat.Membership{membership("users/alice"), membership("users/dave"), membership("users/me")}, "", nil
		},
		ListMessages: func(_ context.Context, spaceName, _ string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted {
				return nil, "", nil
			}
			switch spaceName {
			case spaceA:
				return []*chat.Message{chatMessage(msgA, "users/alice", "hi from A", base)}, "", nil
			case spaceB:
				return []*chat.Message{chatMessage(msgB, "users/alice", "hi from B", base.Add(time.Minute))}, "", nil
			}
			return nil, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			switch userName {
			case "users/me":
				return accountID, nil
			case "users/alice":
				return aliceEmail, nil
			}
			return "", nil // users/dave is NOT in Contacts (id path would be needed)
		},
		// dave WOULD resolve via members.get, but cap=0 defers it every space.
		ResolveMemberID: func(_ context.Context, _, normalizedEmail string) (string, bool, error) {
			if normalizedEmail == daveEmail {
				return "users/dave", false, nil
			}
			return "", true, nil
		},
	}
	p := e.newProvider(t, map[string]struct{}{accountID: {}}, funcs)
	// Cap=0: id-resolution is deferred for EVERY space (the prod stampede).
	p.SetMemberResolveCapForTest(0)

	state, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	result, err := p.Sync(e.ctx, state, nil)
	require.NoError(t, err)

	// (a) EVERY space's message processed AND matched despite the resolve debt.
	assert.Positive(t, result.ItemsProcessed, "messages are processed even with the resolve cap at 0 (no freeze)")
	assert.Positive(t, result.ItemsMatched, "messages are matched via the People path even with the resolve cap at 0")
	rowA, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, msgA, alice.ID)
	require.NoError(t, err, "space A's message matched the People-resolvable member despite deferred id-resolution")
	assert.Equal(t, repository.InteractionDirectionInbound, rowA.Direction)
	_, err = e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, msgB, alice.ID)
	require.NoError(t, err, "space B's message matched too — the cap deferral on space A did NOT freeze space B")

	// (b) BOTH spaces' cursors advanced (no space held by the resolve debt).
	persisted, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	assert.NotEmpty(t, spaceCursorFromMetadata(t, persisted.Metadata, spaceA), "space A's cursor advanced")
	assert.NotEmpty(t, spaceCursorFromMetadata(t, persisted.Metadata, spaceB), "space B's cursor advanced")
}

// TestGChatProvider_DeferredIDContactMatchesOnLaterSweep proves the go-forward
// backfill semantics. Sweep 1 (cap=0): a not-in-Contacts contact's id is NOT
// resolved, so their message is NOT matched — BUT the space cursor STILL advances
// and a People-resolvable co-member's message DOES match. Sweep 2 (cap raised): a
// NEW message from the now-resolvable contact, sent after the cursor, matches.
func TestGChatProvider_DeferredIDContactMatchesOnLaterSweep(t *testing.T) {
	t.Parallel()
	e := setupGChatProviderTest(t)
	prefix := e.gen.Prefix()

	alice, aliceEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))
	dave, daveEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))

	spaceName := "spaces/DEFER-" + prefix
	aliceMsg := spaceName + "/messages/alice-" + prefix
	daveMsg1 := spaceName + "/messages/dave1-" + prefix
	daveMsg2 := spaceName + "/messages/dave2-" + prefix
	accountID := prefix + "me@synthetic.example"
	e.newGChatSyncState(t, accountID, true, nil)
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Second)

	var sweep2 bool
	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			sp := space(spaceName, "SPACE")
			if sweep2 {
				sp.LastActiveTime = chatRFC3339(accelerated.GetCurrentTime())
			} else {
				sp.LastActiveTime = chatRFC3339(base)
			}
			return []*chat.Space{sp}, "", nil
		},
		ListMembers: func(context.Context, string, string) ([]*chat.Membership, string, error) {
			return []*chat.Membership{membership("users/alice"), membership("users/dave"), membership("users/me")}, "", nil
		},
		ListMessages: func(_ context.Context, _, _ string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted {
				return nil, "", nil
			}
			if sweep2 {
				// A NEW message from dave, sent after sweep-1's cursor.
				return []*chat.Message{chatMessage(daveMsg2, "users/dave", "dave after warm-up", accelerated.GetCurrentTime().Add(-time.Minute).Truncate(time.Second))}, "", nil
			}
			// Sweep 1: alice (People-resolvable) and dave (id deferred by cap=0).
			return []*chat.Message{
				chatMessage(aliceMsg, "users/alice", "alice hi", base),
				chatMessage(daveMsg1, "users/dave", "dave before warm-up", base.Add(time.Minute)),
			}, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			switch userName {
			case "users/me":
				return accountID, nil
			case "users/alice":
				return aliceEmail, nil
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

	// Sweep 1: cap=0 → dave's id is deferred.
	p.SetMemberResolveCapForTest(0)
	state1, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	_, err = p.Sync(e.ctx, state1, nil)
	require.NoError(t, err)

	// alice matched (People path); dave NOT matched (id deferred); cursor advanced.
	_, err = e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, aliceMsg, alice.ID)
	require.NoError(t, err, "the People-resolvable co-member matched on sweep 1")
	_, err = e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, daveMsg1, dave.ID)
	assert.ErrorIs(t, err, db.ErrNotFound, "dave's sweep-1 message is NOT matched (id deferred by cap=0) — go-forward gap")
	persisted1, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	cursor1 := spaceCursorFromMetadata(t, persisted1.Metadata, spaceName)
	require.NotEmpty(t, cursor1, "the cursor ADVANCED on sweep 1 despite the deferred id (no freeze)")

	// Sweep 2: cap raised → dave's id resolves; a new message from him matches.
	sweep2 = true
	p.SetMemberResolveCapForTest(50)
	state2, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	_, err = p.Sync(e.ctx, state2, nil)
	require.NoError(t, err)

	row, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, daveMsg2, dave.ID)
	require.NoError(t, err, "dave's NEW message matches once his id warms up (go-forward)")
	assert.Equal(t, repository.InteractionDirectionInbound, row.Direction)
}

// TestGChatProvider_NoCurrentMemberSpaceStillPagesAndAdvances proves the
// freeze + former-member correctness fix. A space whose CURRENT membership has NO
// known CRM contact (the contact LEFT) but whose history contains an INBOUND
// message from a known contact (sender email ∈ knownMap, matched via the People
// email-sender path INDEPENDENT of current membership). Asserts the space pages
// content, the former-member's message MATCHES, and the cursor ADVANCES. A second
// space whose only known participant is a DEFERRED not-in-Contacts contact (cap=0,
// no known inbound sender) STILL advances its cursor (window pages to zero
// matchable rows and proven advances it).
func TestGChatProvider_NoCurrentMemberSpaceStillPagesAndAdvances(t *testing.T) {
	t.Parallel()
	e := setupGChatProviderTest(t)
	prefix := e.gen.Prefix()

	// Former member: a known contact who LEFT the space but has inbound history.
	former, formerEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))
	// Deferred contact for the second space (id never resolves under cap=0).
	deferred, deferredEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))

	formerSpace := "spaces/FORMER-" + prefix
	formerMsg := formerSpace + "/messages/former-" + prefix
	deferredSpace := "spaces/DEFERREDONLY-" + prefix
	accountID := prefix + "me@synthetic.example"
	e.newGChatSyncState(t, accountID, true, nil)
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Second)

	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			return []*chat.Space{space(formerSpace, "SPACE"), space(deferredSpace, "SPACE")}, "", nil
		},
		ListMembers: func(_ context.Context, spaceName, _ string) ([]*chat.Membership, string, error) {
			// Neither space currently has a known CRM contact as a member: the former
			// space has only "me"; the deferred space has "me" + a member whose id
			// would only resolve via members.get (deferred by cap=0).
			if spaceName == deferredSpace {
				return []*chat.Membership{membership("users/deferred"), membership("users/me")}, "", nil
			}
			return []*chat.Membership{membership("users/me")}, "", nil
		},
		ListMessages: func(_ context.Context, spaceName, _ string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted {
				return nil, "", nil
			}
			if spaceName == formerSpace {
				// An INBOUND message from the former member (still in history).
				return []*chat.Message{chatMessage(formerMsg, "users/former", "i left but said this", base)}, "", nil
			}
			return nil, "", nil // deferred space: no matchable rows this sweep
		},
		// The former member's SENDER id resolves to their email via People (the
		// email-sender path, independent of current membership). deferred + me too.
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			switch userName {
			case "users/me":
				return accountID, nil
			case "users/former":
				return formerEmail, nil
			}
			return "", nil // users/deferred not in Contacts
		},
		ResolveMemberID: func(_ context.Context, _, normalizedEmail string) (string, bool, error) {
			if normalizedEmail == deferredEmail {
				return "users/deferred", false, nil
			}
			return "", true, nil
		},
	}
	p := e.newProvider(t, map[string]struct{}{accountID: {}}, funcs)
	// cap=0 so the deferred space's only known participant is never id-resolved.
	p.SetMemberResolveCapForTest(0)

	state, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	result, err := p.Sync(e.ctx, state, nil)
	require.NoError(t, err)

	// (a) The former member's inbound message is matched (a watermark-skip shortcut
	// would have dropped it). (c) ItemsMatched > 0 for this space.
	row, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, formerMsg, former.ID)
	require.NoError(t, err, "the former member's inbound message matched via the email-sender path despite no current membership")
	assert.Equal(t, repository.InteractionDirectionInbound, row.Direction)
	assert.Positive(t, result.ItemsMatched, "the former-member match counts")

	// (b) Both spaces' cursors advanced (no freeze on the no-current-member spaces).
	persisted, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	assert.NotEmpty(t, spaceCursorFromMetadata(t, persisted.Metadata, formerSpace), "the former-member space's cursor advanced")
	assert.NotEmpty(t, spaceCursorFromMetadata(t, persisted.Metadata, deferredSpace), "the deferred-only space's cursor advanced (window paged to zero matchable rows)")

	// The deferred contact has no row this sweep (go-forward), but caused no freeze.
	rows, err := e.commsRepo.ListByContact(e.ctx, deferred.ID)
	require.NoError(t, err)
	assert.Empty(t, rows, "the deferred contact is not matched this sweep (go-forward), but did not hold any cursor")
}

// TestGChatProvider_ColdStartFrontSpaceDoesNotStarveLaterSpaces proves the budget
// amortization. Several spaces in stable ListSpaces order; the FRONT one has a
// multi-page history that pages fully (proven, cursor advances) within budget,
// leaving budget for later spaces. Asserts the front space advances AND a later
// space is also processed in the same sweep (forward progress, no starvation).
func TestGChatProvider_ColdStartFrontSpaceDoesNotStarveLaterSpaces(t *testing.T) {
	t.Parallel()
	e := setupGChatProviderTest(t)
	prefix := e.gen.Prefix()

	alice, aliceEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))

	frontSpace := "spaces/FRONT-" + prefix
	laterSpace := "spaces/LATER-" + prefix
	laterMsg := laterSpace + "/messages/later-" + prefix
	accountID := prefix + "me@synthetic.example"
	e.newGChatSyncState(t, accountID, true, nil)
	base := accelerated.GetCurrentTime().Add(-3 * time.Hour).Truncate(time.Second)

	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			// Stable order: the front space first, the later space second.
			return []*chat.Space{space(frontSpace, "SPACE"), space(laterSpace, "SPACE")}, "", nil
		},
		ListMembers: func(context.Context, string, string) ([]*chat.Membership, string, error) {
			return []*chat.Membership{membership("users/alice"), membership("users/me")}, "", nil
		},
		ListMessages: func(_ context.Context, spaceName, _ string, showDeleted bool, token string) ([]*chat.Message, string, error) {
			if showDeleted {
				return nil, "", nil
			}
			if spaceName == frontSpace {
				// A multi-page history for the front space (three pages).
				switch token {
				case "":
					return []*chat.Message{chatMessage(frontSpace+"/messages/f1-"+prefix, "users/alice", "f1", base)}, "fp2", nil
				case "fp2":
					return []*chat.Message{chatMessage(frontSpace+"/messages/f2-"+prefix, "users/alice", "f2", base.Add(time.Minute))}, "fp3", nil
				case "fp3":
					return []*chat.Message{chatMessage(frontSpace+"/messages/f3-"+prefix, "users/alice", "f3", base.Add(2*time.Minute))}, "", nil
				}
				return nil, "", nil
			}
			// The later space: a single page.
			return []*chat.Message{chatMessage(laterMsg, "users/alice", "later", base.Add(time.Hour))}, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			switch userName {
			case "users/me":
				return accountID, nil
			case "users/alice":
				return aliceEmail, nil
			}
			return "", nil
		},
	}
	p := e.newProvider(t, map[string]struct{}{accountID: {}}, funcs)

	state, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	_, err = p.Sync(e.ctx, state, nil)
	require.NoError(t, err)

	// The front space paged fully and advanced; the later space was ALSO processed
	// in the same sweep — the front space's multi-page window did not starve it.
	persisted, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	assert.NotEmpty(t, spaceCursorFromMetadata(t, persisted.Metadata, frontSpace), "the front space advanced (fully paged)")
	assert.NotEmpty(t, spaceCursorFromMetadata(t, persisted.Metadata, laterSpace), "the later space was reached in the same sweep — no starvation")
	_, err = e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, laterMsg, alice.ID)
	require.NoError(t, err, "the later space's message was processed despite the front space's multi-page history")
}

// TestGChatProvider_NoDoubleProcessOnReResolve proves idempotency: a message
// processed on sweep 1 via the People path is re-listed on sweep 2 (cursor reset)
// and now ALSO id-resolves the sender — but the upsert dedup yields exactly ONE
// row per contact, no duplicate. Guards the "process with partial index now,
// fuller later" safety claim.
func TestGChatProvider_NoDoubleProcessOnReResolve(t *testing.T) {
	t.Parallel()
	e := setupGChatProviderTest(t)
	prefix := e.gen.Prefix()

	dave, daveEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))

	spaceName := "spaces/NODOUBLE-" + prefix
	msgName := spaceName + "/messages/m-" + prefix
	accountID := prefix + "me@synthetic.example"
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Second)

	var davePeopleResolvable = true
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
			// The SAME message is returned on both sweeps (sweep 2 re-lists it via the
			// reset cursor below).
			return []*chat.Message{chatMessage(msgName, "users/dave", "hello", base)}, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			if userName == "users/me" {
				return accountID, nil
			}
			if userName == "users/dave" && davePeopleResolvable {
				return daveEmail, nil // sweep 1: matched via the People path
			}
			return "", nil
		},
		ResolveMemberID: func(_ context.Context, _, normalizedEmail string) (string, bool, error) {
			// On sweep 2 dave is matched via the id path instead.
			if normalizedEmail == daveEmail {
				return "users/dave", false, nil
			}
			return "", true, nil
		},
	}
	// Seed an empty cursor so sweep 1 lists the message; sweep 2 resets the cursor
	// to re-list the same message (overlap), now also id-resolving the sender.
	e.newGChatSyncState(t, accountID, true, nil)
	p := e.newProvider(t, map[string]struct{}{accountID: {}}, funcs)

	// Sweep 1: matched via the People path.
	state1, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	_, err = p.Sync(e.ctx, state1, nil)
	require.NoError(t, err)

	// Reset the space cursor (simulating a manual cursor reset / overlap) and flip
	// dave to People-UNresolvable so sweep 2 matches him via the id path instead.
	// Read-modify-write only the cursor so the other persisted keys (membership,
	// positive cache) are preserved, then re-list the same message.
	davePeopleResolvable = false
	state1b, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	resetMeta := state1b.Metadata
	if resetMeta == nil {
		resetMeta = map[string]any{}
	}
	resetMeta["space_cursors"] = map[string]any{
		spaceName: map[string]any{"create_cursor": chatRFC3339(base.Add(-time.Hour)), "edit_cursor": chatRFC3339(base.Add(-time.Hour))},
	}
	_, err = e.syncRepo.UpdateSyncStateMetadata(e.ctx, state1b.ID, resetMeta)
	require.NoError(t, err)

	// Sweep 2: the same message is re-listed and id-resolves the sender.
	state2, err := e.syncRepo.GetSyncStateBySource(e.ctx, repository.InteractionSourceGChat, &accountID)
	require.NoError(t, err)
	_, err = p.Sync(e.ctx, state2, nil)
	require.NoError(t, err)

	// Exactly ONE row for the message+contact (upsert dedup; no duplicate).
	rows, err := e.commsRepo.ListByContact(e.ctx, dave.ID)
	require.NoError(t, err)
	count := 0
	for _, r := range rows {
		if r.ExternalID == msgName {
			count++
		}
	}
	assert.Equal(t, 1, count, "re-processing the same message (People path then id path) upserts ONE row, not a duplicate")
}

// TestGChatProvider_HotSpaceNoStarvation proves the starvation fix: a
// space with a known-absent contact (negative written) and an UNCHANGED member
// set across many sweeps — each adding a new message from a DIFFERENT known
// member so lastActiveTime advances (membership refetches) but the member SET is
// identical (fingerprint stable). The cursor advances every sweep and ZERO
// ResolveMemberID calls fire for the absent contact after the first.
func TestGChatProvider_HotSpaceNoStarvation(t *testing.T) {
	t.Parallel()
	e := setupGChatProviderTest(t)
	prefix := e.gen.Prefix()

	_, absentEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))
	active, activeEmail := e.newGChatProviderContact(t, string(repository.ContactMethodEmail))

	spaceName := "spaces/HOT-" + prefix
	accountID := prefix + "me@synthetic.example"
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
			return []*chat.Message{chatMessage(spaceName+"/messages/hot-"+prefix+"-"+strconv.Itoa(sweepIdx), "users/active", "ping", at)}, "", nil
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
	t.Parallel()
	e := setupGChatProviderTest(t)
	prefix := e.gen.Prefix()

	dave, daveEmail := e.newGChatProviderContact(t, string(repository.ContactMethodGChat))

	spaceName := "spaces/REMATCHID-" + prefix
	msgName := spaceName + "/messages/m-" + prefix
	accountID := prefix + "me@synthetic.example"
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
