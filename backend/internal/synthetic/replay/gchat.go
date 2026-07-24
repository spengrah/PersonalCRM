package replay

import (
	"context"
	"fmt"
	"strings"
	"time"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/identity"
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

	// Gate A: THIS replay's gchat message row is linked to an interaction.
	if err := h.Settle(ctx, h.gchatSettled(spec.ExternalID), repository.InteractionSourceGChat); err != nil {
		return GChatResult{}, err
	}
	h.trackContactInteractions(ctx, contactID)
	if err := h.assertContactVenue(ctx, contactID, repository.InteractionSourceGChat); err != nil {
		return GChatResult{}, err
	}
	return GChatResult{ContactID: contactID, ExternalID: spec.ExternalID, Matched: true}, nil
}

// GChatBatchItem is one chat payload in a batch: the seeded contact it targets
// and the message. PairKey marks the two items of a promotion pair (0 = not part
// of one). Items whose specs share a SpaceName are ONE conversation — the caller
// makes them so by cloning a spec rather than minting independent ones, which is
// also what keeps a burst inside the provider's page budget.
type GChatBatchItem struct {
	ContactID uuid.UUID
	Spec      factory.GChatMessageSpec
	PairKey   int
}

// ReplayGChatBatch drives N chat payloads through the GChatSyncProvider and
// settles once per dependency generation. Items must be in chronological replay
// order.
//
// GChat has ONE page budget of 100 shared across the membership, content, and
// edit passes of every space in a sweep, and — unlike GCal — it never drains: a
// fully processed space still costs pages on every subsequent sweep, because the
// content and edit passes each issue a list call before they can observe the
// window is empty. Re-Syncing the same space list therefore converges to a
// permanent stall well short of a large batch. The mitigation is the adapter's,
// not the provider's: ListSpaces is a caller-supplied closure, so the adapter
// PARTITIONS its spaces into buckets sized to fit the budget and presents bucket
// k on Sync k. Each bucket completes entirely within its own budget.
//
// So GChat joins GCal as a SyncCalls > 1 source by a different mechanism: GCal
// drains (the provider advances), GChat buckets (the adapter partitions).
func (h *Harness) ReplayGChatBatch(ctx context.Context, items []GChatBatchItem, opts ...BatchOption) (BatchResult, error) {
	const source = "gchat"

	options := applyBatchOptions(opts)

	entries := gchatBatchEntries(items)
	if err := validateBatchStructure(source, entries); err != nil {
		return BatchResult{}, err
	}
	accountID, err := gchatBatchAccount(items)
	if err != nil {
		return BatchResult{}, err
	}
	if err := h.validateBatchOwnership(ctx, source, entries); err != nil {
		return BatchResult{}, err
	}

	contactIDs := distinctContactIDs(entries)
	res := BatchResult{Payloads: len(items), Contacts: len(contactIDs)}
	before, err := h.snapshotInteractionIDs(ctx, contactIDs)
	if err != nil {
		return res, err
	}

	syncRepo := repository.NewSyncRepositoryWithPool(h.database.Queries, h.database.Pool)
	st, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
		Source:    repository.InteractionSourceGChat,
		AccountID: &accountID,
		Enabled:   true,
	})
	if err != nil {
		return res, fmt.Errorf("gchat create sync state: %w", err)
	}
	defer func() { _ = syncRepo.DeleteSyncState(ctx, st.ID) }()

	world := newGChatBatchWorld(items)
	provider := google.NewGChatSyncProvider(nil, h.commsRepo, syncRepo)
	provider.SetFetcherFactoryForTest(google.NewFakeChatFetcherFactoryForTest(world.fetcherFuncs()))
	provider.SetMeSetForTest(map[string]struct{}{accountID: {}})

	for _, generation := range partitionGenerations(entries) {
		genItems := make([]GChatBatchItem, 0, len(generation))
		for _, i := range generation {
			genItems = append(genItems, items[i])
		}
		world.setGeneration(genItems)

		externalIDs := make([]string, 0, len(genItems))
		for _, it := range genItems {
			externalIDs = append(externalIDs, it.Spec.ExternalID)
		}

		syncCalls, err := h.driveGChatBuckets(ctx, provider, syncRepo, world, accountID, externalIDs, options)
		res.SyncCalls += syncCalls
		if err != nil {
			return res, h.drainPartial(ctx, source, repository.InteractionSourceGChat, contactIDs, err)
		}

		// The provider only writes comms_message rows; the engine derives the
		// interaction on its own pass. One enqueue per contact covers the whole
		// generation, not one per payload.
		for _, contactID := range contactIDs {
			if _, err := h.client.Insert(ctx, consumerjobs.MessagingAggregateForContactArgs{
				ContactID: contactID,
				Source:    repository.InteractionSourceGChat,
			}, nil); err != nil {
				return res, h.drainPartial(ctx, source, repository.InteractionSourceGChat, contactIDs,
					fmt.Errorf("gchat enqueue aggregate: %w", err))
			}
		}

		// Gate A is scoped to THIS generation's external ids.
		if err := h.Settle(ctx, h.gchatBatchSettled(externalIDs), repository.InteractionSourceGChat); err != nil {
			return res, h.drainPartial(ctx, source, repository.InteractionSourceGChat, contactIDs, err)
		}
		res.SettleCalls++
	}

	res.Interactions = h.trackBatchInteractions(ctx, contactIDs, before)
	for _, contactID := range contactIDs {
		if err := h.assertContactVenue(ctx, contactID, repository.InteractionSourceGChat); err != nil {
			return res, err
		}
	}
	return res, nil
}

// driveGChatBuckets presents the generation's spaces one bucket per Sync, then
// verifies every payload's row landed. The progress signal is the comms_message
// ROW count, not the settled count: the provider writes rows synchronously
// inside Sync, while the interaction linkage arrives later from a River
// consumer, so the settled count cannot distinguish "this space was never
// presented" from "the aggregate has not run yet".
//
// If rows are still missing after every bucket has been presented, it falls back
// to re-presenting the FULL space list while progress continues — the drain
// shape — and fails loudly when progress stops. That fallback is also what makes
// the no-bucketing case observable: with bucketing disabled the whole batch is
// one bucket, the fallback is a pure drain, and it stalls exactly as the shared
// page budget predicts.
func (h *Harness) driveGChatBuckets(
	ctx context.Context,
	provider *google.GChatSyncProvider,
	syncRepo *repository.SyncRepository,
	world *gchatBatchWorld,
	accountID string,
	externalIDs []string,
	options batchOptions,
) (syncCalls int, err error) {
	want := int64(len(externalIDs))
	rowsPresent := func(ctx context.Context) (int64, error) {
		return h.support.CountGChatMessagesByExternalIDs(ctx, externalIDs)
	}
	driveSync := func(ctx context.Context) error {
		// Re-read the state each Sync: the provider persists its per-space cursors
		// and caches into the row, and the next bucket must start from them.
		state, err := syncRepo.GetSyncStateBySource(ctx, repository.InteractionSourceGChat, &accountID)
		if err != nil {
			return fmt.Errorf("gchat get sync state: %w", err)
		}
		if _, err := provider.Sync(ctx, state, nil); err != nil {
			return fmt.Errorf("gchat sync: %w", err)
		}
		return nil
	}

	buckets := chunkStrings(world.spaceNames(), options.spacesPerSync())
	for _, bucket := range buckets {
		world.presentSpaces(bucket)
		if err := driveSync(ctx); err != nil {
			return syncCalls, err
		}
		syncCalls++
	}

	n, err := rowsPresent(ctx)
	if err != nil {
		return syncCalls, err
	}
	if n >= want {
		return syncCalls, nil
	}

	// Fallback drain over the full list, bounded. The slack is deliberately more
	// than a residual needs: a shortfall that is genuinely converging should
	// finish, and one that is not should be diagnosed as a STALL — which names
	// the real cause — rather than as a cap hit, which is ambiguous between
	// "needed one more pass" and "will never finish".
	world.presentSpaces(world.spaceNames())
	drainCalls, err := driveUntilCount(ctx, want, len(buckets)+options.drainSlackSyncs(), driveSync, rowsPresent)
	syncCalls += drainCalls
	return syncCalls, err
}

// gchatBatchSettled is the batch Gate A: every one of these external ids has an
// interaction-linked comms_message row.
func (h *Harness) gchatBatchSettled(externalIDs []string) gateA {
	want := int64(len(externalIDs))
	return func(ctx context.Context) (bool, error) {
		n, err := h.support.CountSettledGChatMessagesByExternalIDs(ctx, externalIDs)
		return n >= want, err
	}
}

// gchatBatchEntries projects the typed items into the source-neutral view.
// Direction is whether the sender resolves to the connected account, which is
// exactly what the provider reads; the addressed identifier is the peer's chat
// address (an email, normalized the same way).
func gchatBatchEntries(items []GChatBatchItem) []batchEntry {
	out := make([]batchEntry, 0, len(items))
	for _, it := range items {
		peer, outbound := gchatSpecPeer(it.Spec)
		out = append(out, batchEntry{
			contactID:     it.ContactID,
			identifier:    it.Spec.ExternalID,
			seeded:        it.Spec.Intent == factory.MatchSeeded,
			outbound:      outbound,
			pairKey:       it.PairKey,
			addressed:     peer,
			addressedType: identity.IdentifierTypeGChat,
		})
	}
	return out
}

// gchatSpecPeer returns the peer's chat address and whether the message is
// outbound. The sender's address decides both: when it is the connected account
// the message is outbound and the peer is the other co-member.
func gchatSpecPeer(spec factory.GChatMessageSpec) (peer string, outbound bool) {
	senderUser := ""
	if spec.Message != nil && spec.Message.Sender != nil {
		senderUser = spec.Message.Sender.Name
	}
	senderEmail := spec.EmailByUser[senderUser]
	if senderEmail != spec.AccountID {
		return senderEmail, false
	}
	for _, member := range spec.Members {
		if member == nil || member.Member == nil {
			continue
		}
		if email := spec.EmailByUser[member.Member.Name]; email != "" && email != spec.AccountID {
			return email, true
		}
	}
	return "", true
}

// gchatBatchAccount returns the single connected account the batch is driven
// under; a mixed-account batch would sweep the wrong account's spaces.
func gchatBatchAccount(items []GChatBatchItem) (string, error) {
	account := items[0].Spec.AccountID
	for i, it := range items {
		if it.Spec.AccountID != account {
			return "", fmt.Errorf("gchat: item %d is on account %q but the batch is on %q: %w",
				i, it.Spec.AccountID, account, ErrBatchMixedAccounts)
		}
	}
	return account, nil
}

// --- the batch's fake chat world --------------------------------------------

// gchatBatchWorld is the space/member/message world the batch's fake fetcher
// serves. It is built once for the whole batch (membership and email resolution
// never change between generations) and re-pointed per generation and per
// bucket: setGeneration decides WHICH messages exist, presentSpaces decides
// which spaces the sweep can see at all.
type gchatBatchWorld struct {
	// order is the distinct space names in first-seen item order, so bucketing is
	// deterministic and mirrors the caller's chronological ordering.
	order []string
	// members is the FIRST item's membership for each space — a cloned
	// conversation varies only the message, so later items carry the same set.
	// emailByUser is unioned across every item, since a clone may introduce a
	// user name the first item did not name.
	members     map[string][]*chat.Membership
	emailByUser map[string]string
	// messages is the CURRENT generation's messages per space.
	messages map[string][]*chat.Message
	// visible is the space set the current Sync may see.
	visible map[string]struct{}
}

func newGChatBatchWorld(items []GChatBatchItem) *gchatBatchWorld {
	w := &gchatBatchWorld{
		members:     map[string][]*chat.Membership{},
		emailByUser: map[string]string{},
		messages:    map[string][]*chat.Message{},
		visible:     map[string]struct{}{},
	}
	seen := map[string]struct{}{}
	for _, it := range items {
		name := it.Spec.SpaceName
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			w.order = append(w.order, name)
			w.members[name] = it.Spec.Members
		}
		for user, email := range it.Spec.EmailByUser {
			w.emailByUser[user] = email
		}
	}
	return w
}

// spaceNames returns the CURRENT generation's distinct space names, in the
// batch's first-seen order. Only these are worth presenting: a space with no
// message in this generation would consume budget and yield nothing, and its
// lastActiveTime would be undefined.
func (w *gchatBatchWorld) spaceNames() []string {
	out := make([]string, 0, len(w.order))
	for _, name := range w.order {
		if len(w.messages[name]) > 0 {
			out = append(out, name)
		}
	}
	return out
}

// setGeneration points the world at one generation's messages.
func (w *gchatBatchWorld) setGeneration(items []GChatBatchItem) {
	w.messages = map[string][]*chat.Message{}
	for _, it := range items {
		w.messages[it.Spec.SpaceName] = append(w.messages[it.Spec.SpaceName], it.Spec.Message)
	}
}

// presentSpaces restricts what the next Sync's ListSpaces returns.
func (w *gchatBatchWorld) presentSpaces(names []string) {
	w.visible = make(map[string]struct{}, len(names))
	for _, n := range names {
		w.visible[n] = struct{}{}
	}
}

// spaceFor rebuilds the chat.Space for a name. lastActiveTime is the newest
// message create time in the space, which is what the provider's membership
// cache compares against.
func (w *gchatBatchWorld) spaceFor(name string) *chat.Space {
	lastActive := ""
	for _, m := range w.messages[name] {
		if m != nil && laterRFC3339(m.CreateTime, lastActive) {
			lastActive = m.CreateTime
		}
	}
	return &chat.Space{Name: name, SpaceType: "SPACE", LastActiveTime: lastActive}
}

// fetcherFuncs is the fake fetcher over this world. It honors the provider's
// create_time filter — the real API does, and without it a space re-presented in
// a later generation would re-serve the earlier generation's messages.
func (w *gchatBatchWorld) fetcherFuncs() google.FakeChatFetcherFuncs {
	return google.FakeChatFetcherFuncs{
		ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) {
			out := make([]*chat.Space, 0, len(w.visible))
			for _, name := range w.order {
				if _, ok := w.visible[name]; ok {
					out = append(out, w.spaceFor(name))
				}
			}
			return out, "", nil
		},
		ListMembers: func(_ context.Context, spaceName, _ string) ([]*chat.Membership, string, error) {
			return w.members[spaceName], "", nil
		},
		ListMessages: func(_ context.Context, spaceName, filter string, showDeleted bool, _ string) ([]*chat.Message, string, error) {
			if showDeleted {
				// The batch models no edits or deletions, matching the single adapter.
				return nil, "", nil
			}
			after := chatFilterCreateTimeFloor(filter)
			out := make([]*chat.Message, 0, len(w.messages[spaceName]))
			for _, m := range w.messages[spaceName] {
				if m != nil && laterRFC3339(m.CreateTime, after) {
					out = append(out, m)
				}
			}
			return out, "", nil
		},
		ResolvePersonEmail: func(_ context.Context, userName string) (string, error) {
			return w.emailByUser[userName], nil
		},
	}
}

// chatFilterCreateTimeFloor extracts the timestamp from the provider's
// `create_time > "<ts>"` filter. An unparseable filter yields "", which lets
// everything through — the permissive direction, so a filter-shape change
// surfaces as re-served messages (idempotent upserts) rather than lost ones.
func chatFilterCreateTimeFloor(filter string) string {
	open := strings.Index(filter, `"`)
	if open < 0 {
		return ""
	}
	rest := filter[open+1:]
	closeIdx := strings.Index(rest, `"`)
	if closeIdx < 0 {
		return ""
	}
	return rest[:closeIdx]
}

// laterRFC3339 reports whether a is strictly later than b, by INSTANT rather
// than string order so differing fractional-second precision cannot mis-order
// two timestamps. An empty or unparseable b is treated as "no floor".
func laterRFC3339(a, b string) bool {
	if b == "" {
		return a != ""
	}
	at, aErr := time.Parse(time.RFC3339Nano, a)
	bt, bErr := time.Parse(time.RFC3339Nano, b)
	if aErr != nil || bErr != nil {
		return a > b
	}
	return at.After(bt)
}
