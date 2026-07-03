// Go-live acceptance for the GChat integration (PR 3, spec §8 Phase 3). This is
// the end-to-end proof that flipping the feature on actually works: a
// GChatSyncProvider registered in a real ProviderRegistry, an enabled
// (gchat, account) external_sync_state created exactly as the boot
// reconciliation creates it (chat-scope-gated), and the sweep driven through the
// SyncService.RunAccountSync worker entry point (NOT a direct provider.Sync
// call) so the registered-provider + state-fetch + metadata-cursor-write path is
// exercised. The REAL GChat aggregation engine + InteractionRecorder +
// CadenceUpdater are wired (via the shared gchat engine harness) so derived
// interactions + cadence are asserted, not just stored content rows.
//
// It proves: provider registered → scheduler-driven sweep → comms_message rows →
// aggregation → interactions (with a "GChat …(N messages)" description) →
// cadence (last_contacted for inbound, last_outreach_at for outbound), no
// contact created for an unknown participant, and the per-space cursor advanced
// in metadata so a second sweep is incremental.
//
// Unlike Gmail, GChat is event-free: the provider only writes content rows, and
// the aggregation engine (driven here via AggregateForContactBatch — the path
// the sweeper/reenqueuer drive) derives the interactions. The tick-enumeration /
// ListDueAccounts → enqueue chain is covered by the scheduler's own tests. The
// fetcher + me-set are injected fakes (NEVER the live Chat API); addresses are
// placeholders (NO PII).
package tests

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	syncpkg "personal-crm/backend/internal/sync"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	chat "google.golang.org/api/chat/v1"
)

// TestGChatGoLive_RegisteredProvider_SchedulerSweep_ProducesInteractionsAndCadence
// is the PR-3 go-live acceptance test.
func TestGChatGoLive_RegisteredProvider_SchedulerSweep_ProducesInteractionsAndCadence(t *testing.T) {
	t.Parallel()
	e := setupGChatEngineTest(t) // engine + live bus + recorder + cadence + repos
	gen, _ := migrationGenerator(t)
	prefix := gen.Prefix()

	contactRepo := e.contactRepo
	contactRepo.SetPool(e.database.Pool)
	methodRepo := repository.NewContactMethodRepository(e.database.Queries)
	syncRepo := repository.NewSyncRepositoryWithPool(e.database.Queries, e.database.Pool)
	identityRepo := repository.NewIdentityRepository(e.database.Queries)

	account := prefix + "me@synthetic.example"

	// Known contact + email method (appears in the provider's dual-source known
	// map). A cadence makes the contact_by recompute observable. Seeded via the
	// nil-bus ContactService (single-tx contact+method write, no River client).
	assertSvc, cache := buildKnowledgeDeps(t, e.database, nil)
	contactSvc := service.NewContactService(e.database, contactRepo, methodRepo,
		e.interactionRepo, repository.NewContactTaskRepository(e.database.Queries), nil, nil,
		nil, assertSvc, cache, nil)
	spec := gen.Contact(factory.WithEmail(), factory.WithCadence("weekly"))
	peerEmail := spec.Email
	contact, _, err := contactSvc.CreateContact(e.ctx, repository.CreateContactRequest{
		FullName: spec.FullName,
		Cadence:  spec.Cadence,
	}, []service.ContactMethodInput{{Type: string(repository.ContactMethodEmail), Value: peerEmail, IsPrimary: true}})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = e.commsRepo.HardDeleteByContact(e.ctx, contact.ID)
		_ = e.interactionRepo.HardDeleteInteractionsBySourceRefPrefix(e.ctx, repository.InteractionSourceGChat, "gchat:%")
		_ = syncRepo.DeleteSyncStatesByAccountID(e.ctx, account)
		_ = contactRepo.HardDeleteContact(e.ctx, contact.ID)
	})

	spaceName := "spaces/GOLIVE-" + prefix
	inMsg := spaceName + "/messages/in-" + prefix
	outMsg := spaceName + "/messages/out-" + prefix
	bystanderMsg := spaceName + "/messages/by-" + prefix
	base := accelerated.GetCurrentTime().Add(-time.Hour).Truncate(time.Second)
	// Unknown participant the sweep must NOT turn into a contact.
	stranger := prefix + "stranger@synthetic.example"

	funcs := google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			return []*chat.Space{space(spaceName, "SPACE")}, "", nil
		},
		ListMembers: func(context.Context, string, string) ([]*chat.Membership, string, error) {
			return []*chat.Membership{membership("users/peer"), membership("users/me"), membership("users/stranger")}, "", nil
		},
		ListMessages: func(_ context.Context, _, _ string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted {
				return nil, "", nil // edit/delete pass: nothing
			}
			return []*chat.Message{
				chatMessage(inMsg, "users/peer", "hey there", base),
				chatMessage(outMsg, "users/me", "replying", base.Add(30*time.Minute)),
				// A message between "me" and an unknown address: a bystander to
				// the known contact, must not create a contact.
				chatMessage(bystanderMsg, "users/stranger", "who am i", base.Add(45*time.Minute)),
			}, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			switch userName {
			case "users/peer":
				return peerEmail, nil
			case "users/me":
				return account, nil
			case "users/stranger":
				return stranger, nil
			}
			return "", nil
		},
	}

	provider := google.NewGChatSyncProvider(nil, e.commsRepo, syncRepo)
	provider.SetFetcherFactoryForTest(google.NewFakeChatFetcherFactoryForTest(funcs))
	provider.SetMeSetForTest(map[string]struct{}{account: {}})

	registry := syncpkg.NewProviderRegistry()
	registry.Register(provider)

	// Create the enabled (gchat, account) state exactly as the boot
	// reconciliation would: chat-scope-gated, enabled, contact_driven,
	// backfill_since, NO cursor (the provider materializes space_cursors).
	stateLister := &reconcileStubLister{accounts: []repository.OAuthCredentialStatus{
		chatScopedCred(account),
	}}
	reconcileSvc := service.NewSyncService(syncRepo, contactRepo, registry)
	reconcileSvc.SetGChatAccountLister(stateLister)
	require.NoError(t, reconcileSvc.ReconcileGChatSyncStates(e.ctx))

	// Sanity: the reconciliation created the enabled state the sweep fetches,
	// with an empty per-space cursor map (not yet materialized).
	acct := account
	created, err := syncRepo.GetSyncStateBySource(e.ctx, "gchat", &acct)
	require.NoError(t, err)
	require.True(t, created.Enabled)
	require.NotContains(t, created.Metadata, "space_cursors", "no per-space cursor before the first sweep")

	// Drive the sweep through the worker entry point: RunAccountSync fetches
	// fresh state from the registry-backed provider, runs Sync, and persists the
	// advanced per-space cursor into metadata.
	sweepSvc := service.NewSyncService(syncRepo, contactRepo, registry)
	require.NoError(t, sweepSvc.RunAccountSync(e.ctx, "gchat", &acct))

	// Content rows exist for both qualifying directions (inbound + outbound),
	// none for the unknown bystander.
	inRow, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, inMsg, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.InteractionDirectionInbound, inRow.Direction)
	outRow, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceGChat, outMsg, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.InteractionDirectionOutbound, outRow.Direction)

	// Aggregate the contact's unprocessed gchat chats — the path the
	// sweeper/reenqueuer drive (discovers the space itself, no hard-coding).
	require.NoError(t, e.engine.AggregateForContactBatch(e.ctx, contact.ID))

	// The recorder derives interactions asynchronously after the publish. An
	// inbound-then-outbound ordering produces two same-direction interactions
	// (one inbound, one outbound — the inbound precedes the outbound, so the
	// 48h reply bridge that would promote an OUTBOUND to mutual does not apply;
	// that promotion path is covered by the engine's own bridge test).
	interactions := waitForInteractionCountExact(t, e.ctx, e.interactionRepo, contact.ID, 2, defaultInteractionWaitTimeout)
	require.Len(t, interactions, 2)
	dirs := map[string]bool{}
	for _, in := range interactions {
		assert.Equal(t, repository.InteractionSourceGChat, in.Source)
		require.NotNil(t, in.Description, "every gchat interaction carries a description")
		assert.Contains(t, *in.Description, "GChat", "description is a GChat …(N messages) summary")
		assert.Contains(t, *in.Description, "message", "description reports the aggregated message count")
		dirs[in.Direction] = true
	}
	assert.True(t, dirs[repository.InteractionDirectionInbound], "an inbound gchat interaction was derived")
	assert.True(t, dirs[repository.InteractionDirectionOutbound], "an outbound gchat interaction was derived")

	// Cadence: the inbound bumps last_contacted; the outbound bumps
	// last_outreach_at.
	c := waitForBothGChatCadence(t, e.ctx, contactRepo, contact.ID)
	require.NotNil(t, c.LastContacted, "inbound bumps last_contacted")
	require.NotNil(t, c.LastOutreachAt, "outbound bumps last_outreach_at")

	// Match-only: the unknown participant never produced a contact_method.
	unknownMatches, err := identityRepo.FindContactMethodsByValue(e.ctx, []string{"email"}, stranger)
	require.NoError(t, err)
	require.Empty(t, unknownMatches, "unknown participant must not create a contact")

	// Per-space cursor advanced off empty → a second RunAccountSync is incremental.
	after, err := syncRepo.GetSyncStateBySource(e.ctx, "gchat", &acct)
	require.NoError(t, err)
	require.Contains(t, after.Metadata, "space_cursors", "per-space cursor materialized after the sweep")
	cursors, ok := after.Metadata["space_cursors"].(map[string]any)
	require.True(t, ok, "space_cursors is a map")
	require.Contains(t, cursors, spaceName, "the swept space has an advanced cursor")
}

// waitForBothGChatCadence polls until both last_contacted and last_outreach_at
// are set on the contact.
func waitForBothGChatCadence(t *testing.T, ctx context.Context, contactRepo *repository.ContactRepository, contactID uuid.UUID) *repository.Contact {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(defaultInteractionWaitTimeout)
	for accelerated.GetCurrentTime().Before(deadline) {
		c, err := contactRepo.GetContact(ctx, contactID)
		require.NoError(t, err)
		if c.LastContacted != nil && c.LastOutreachAt != nil {
			return c
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for last_contacted + last_outreach_at on contact %s", contactID)
	return nil
}
