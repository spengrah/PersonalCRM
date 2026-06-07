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
// OAuth/HTTP) over the seeded contacts. It exercises cadence/follow-up task
// reconciliation; there is no inbound sender or pending equivalent, so
// MatchIntent is ignored. "Settled" = Sync's own DB writes complete + any
// enqueued jobs drain (Gate B over the seeded contacts).
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

	accountID := h.gen.Prefix() + "todoist"
	state := &repository.SyncState{Source: repository.InteractionSourceTodoist, AccountID: &accountID}
	if _, err := provider.Sync(ctx, state, nil); err != nil {
		return TodoistResult{}, fmt.Errorf("todoist sync: %w", err)
	}

	// Settle Gate B over the seeded contacts (no domain predicate — Todoist has
	// no inbound terminal row to wait on; the reconciliation is synchronous).
	if err := h.Settle(ctx, nil, ""); err != nil {
		return TodoistResult{}, err
	}
	return TodoistResult{ContactIDs: contactIDs}, nil
}
