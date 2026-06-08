package replay

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
)

// TodoistResult is the settled outcome of a Todoist replay.
type TodoistResult struct {
	ContactIDs []uuid.UUID
}

// syntheticTodoistOAuth is a stub oauth token provider — the fake Client never
// hits the network, so the token value is irrelevant.
type syntheticTodoistOAuth struct{}

func (syntheticTodoistOAuth) GetAccessToken(_ context.Context, _ string) (string, error) {
	return "synthetic-todoist-token", nil
}
func (syntheticTodoistOAuth) HasAnyAccount(_ context.Context) bool { return true }

// fakeTodoistClient is a Client returning an empty incremental Sync (no inbound
// items). The Todoist provider has no inbound interaction graph; "settled" =
// the provider's own DB writes (task reconciliation for the seeded contacts)
// complete inside Sync. MatchIntent is n/a for Todoist.
type fakeTodoistClient struct{}

func (fakeTodoistClient) QuickAdd(_ context.Context, _ string, _ string) (*todoist.QuickAddTask, error) {
	return &todoist.QuickAddTask{}, nil
}

func (fakeTodoistClient) Sync(_ context.Context, _ string, _ []string, _ []todoist.SyncCommand) (*todoist.SyncResponse, error) {
	// Empty incremental response: no items, fresh sync token.
	return &todoist.SyncResponse{SyncToken: "synthetic-sync-token", FullSync: true}, nil
}

// ReplayTodoist drives the REAL CadenceSyncProvider with a fake Client (no
// OAuth/HTTP). It exercises cadence task reconciliation; there is no inbound
// sender or pending equivalent, so MatchIntent is ignored. "Settled" = Sync's
// own DB writes complete + any enqueued jobs drain.
//
// IMPORTANT — global reconcile scope: the provider's reconcile lists ALL
// contacts with cadence + contact_by (it has no per-contact scoping seam), so on
// the shared test DB it can create a contact_task for a cadence-bearing contact
// this replay did NOT seed. To keep the run non-destructive, the adapter
// snapshots the provider's contact_task ids before and after Sync and tracks the
// DELTA into the cleanup ledger, so teardown removes exactly the rows this run
// created — regardless of which contact they attached to. contactIDs is the
// caller's seeded set, returned for assertion convenience.
func (h *Harness) ReplayTodoist(ctx context.Context, contactIDs []uuid.UUID) (TodoistResult, error) {
	syncRepo := repository.NewSyncRepositoryWithPool(h.database.Queries, h.database.Pool)
	provider := todoist.NewCadenceSyncProvider(
		syntheticTodoistOAuth{},
		repository.NewContactTaskRepository(h.database.Queries),
		h.contactRepo,
		syncRepo,
		config.TestConfig(),
		h.bus,
		h.cadenceUpdater,
		h.database.Pool,
		func(string) todoist.Client { return fakeTodoistClient{} },
	)

	// Snapshot todoist contact_task ids before the (globally-scoped) reconcile.
	before, err := h.support.ListContactTaskIdsByProvider(ctx, todoist.SourceName)
	if err != nil {
		return TodoistResult{}, fmt.Errorf("todoist contact_task snapshot (before): %w", err)
	}
	beforeSet := make(map[uuid.UUID]struct{}, len(before))
	for _, id := range before {
		beforeSet[id] = struct{}{}
	}

	accountID := h.gen.Prefix() + "todoist"
	// project_id/label_id are required settings; supply synthetic values (the
	// fake Client never validates them against a real Todoist project).
	state := &repository.SyncState{
		Source:    repository.InteractionSourceTodoist,
		AccountID: &accountID,
		Metadata: map[string]any{
			todoist.MetadataKeyProjectID: h.gen.Prefix() + "project",
			todoist.MetadataKeyLabelID:   h.gen.Prefix() + "label",
		},
	}
	if _, err := provider.Sync(ctx, state, nil); err != nil {
		return TodoistResult{}, fmt.Errorf("todoist sync: %w", err)
	}

	// Track the contact_task rows the reconcile created (after - before).
	after, err := h.support.ListContactTaskIdsByProvider(ctx, todoist.SourceName)
	if err != nil {
		return TodoistResult{}, fmt.Errorf("todoist contact_task snapshot (after): %w", err)
	}
	h.track(func(c *created) {
		for _, id := range after {
			if _, existed := beforeSet[id]; !existed {
				c.addContactTask(id)
			}
		}
	})

	// Settle Gate B over the seeded contacts (no domain predicate — Todoist has
	// no inbound terminal row to wait on; the reconciliation is synchronous).
	if err := h.Settle(ctx, nil, ""); err != nil {
		return TodoistResult{}, err
	}
	return TodoistResult{ContactIDs: contactIDs}, nil
}
