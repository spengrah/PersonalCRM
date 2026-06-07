package replay

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	chat "google.golang.org/api/chat/v1"
)

// GChatResult is the settled outcome of a GChat replay.
type GChatResult struct {
	ContactID  uuid.UUID
	ExternalID string
	Matched    bool // false for MatchUnknown (match-only: no row, no interaction)
}

// ReplayGChat feeds a synthetic chat message through the REAL GChatSyncProvider
// (fake fetcher, no OAuth). The provider writes ONLY a comms_message row and
// publishes nothing; the adapter then drives the comms aggregation engine by
// enqueuing MessagingAggregateForContactArgs for the matched contact (source=
// gchat), which the harness's messaging_aggregate_for_contact worker runs to
// derive the interaction. For MatchUnknown the bystander writes NO comms_message
// row and produces no interaction (match-only).
//
// contactID is the seeded contact this message targets (for MatchSeeded). The
// caller must seed it with an email/gchat method matching the sender.
func (h *Harness) ReplayGChat(ctx context.Context, contactID uuid.UUID, spec factory.GChatMessageSpec) (GChatResult, error) {
	provider := google.NewGChatSyncProvider(nil, h.commsRepo, repository.NewSyncRepositoryWithPool(h.database.Queries, h.database.Pool))
	provider.SetFetcherFactoryForTest(google.NewFakeChatFetcherFactoryForTest(google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			return []*chat.Space{spec.Space}, "", nil
		},
		ListMembers: func(context.Context, string, string) ([]*chat.Membership, string, error) {
			return spec.Members, "", nil
		},
		ListMessages: func(_ context.Context, _, _ string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted {
				return nil, "", nil
			}
			return []*chat.Message{spec.Message}, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			return spec.EmailByUser[userName], nil
		},
	}))
	provider.SetMeSetForTest(map[string]struct{}{spec.AccountID: {}})

	// Create the gchat sync state (the provider requires a persisted state row).
	syncRepo := repository.NewSyncRepositoryWithPool(h.database.Queries, h.database.Pool)
	st, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
		Source:    repository.InteractionSourceGChat,
		AccountID: &spec.AccountID,
		Enabled:   true,
	})
	if err != nil {
		return GChatResult{}, fmt.Errorf("gchat create sync state: %w", err)
	}
	defer func() { _ = syncRepo.DeleteSyncState(ctx, st.ID) }()

	state, err := syncRepo.GetSyncStateBySource(ctx, repository.InteractionSourceGChat, &spec.AccountID)
	if err != nil {
		return GChatResult{}, fmt.Errorf("gchat get sync state: %w", err)
	}

	if _, err := provider.Sync(ctx, state, nil); err != nil {
		return GChatResult{}, fmt.Errorf("gchat sync: %w", err)
	}

	if spec.Intent == factory.MatchUnknown {
		// Match-only: no row, no interaction. Nothing to enqueue or wait on.
		if err := h.Settle(ctx, func(context.Context) (bool, error) { return true, nil }, ""); err != nil {
			return GChatResult{}, err
		}
		return GChatResult{ExternalID: spec.ExternalID, Matched: false}, nil
	}

	// Drive the comms aggregation for the matched contact (the provider does not
	// publish; the engine derives the interaction on its own pass).
	if _, err := h.client.Insert(ctx, consumerjobs.MessagingAggregateForContactArgs{
		ContactID: contactID,
		Source:    repository.InteractionSourceGChat,
	}, nil); err != nil {
		return GChatResult{}, fmt.Errorf("gchat enqueue aggregate: %w", err)
	}

	predicate := func(ctx context.Context) (bool, error) {
		return h.contactHasInteractionSource(ctx, contactID, repository.InteractionSourceGChat)
	}
	if err := h.Settle(ctx, predicate, repository.InteractionSourceGChat); err != nil {
		return GChatResult{}, err
	}
	h.trackContactInteractions(ctx, contactID)
	return GChatResult{ContactID: contactID, ExternalID: spec.ExternalID, Matched: true}, nil
}
