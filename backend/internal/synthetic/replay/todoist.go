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

// namespaceScopedContactRepo wraps the real contact repository for the Todoist
// replay so the provider's reconcile is naturally scoped to THIS harness's
// contacts. The Todoist provider's reconcileContactTasks calls
// ListContactsWithContactBy to enumerate every cadence-bearing contact DB-wide
// and may create/update/close contact_task rows on any of them; this wrapper
// filters that enumeration to the harness's seeded contact ids, so a replay in
// one namespace can never create or mutate a task on another namespace's (or a
// real) contact. GetContact passes through unchanged. Test-only — no production
// provider change.
type namespaceScopedContactRepo struct {
	real    *repository.ContactRepository
	allowed map[uuid.UUID]struct{}
}

func (r *namespaceScopedContactRepo) GetContact(ctx context.Context, id uuid.UUID) (*repository.Contact, error) {
	return r.real.GetContact(ctx, id)
}

func (r *namespaceScopedContactRepo) ListContactsWithContactBy(ctx context.Context, limit int32) ([]repository.Contact, error) {
	all, err := r.real.ListContactsWithContactBy(ctx, limit)
	if err != nil {
		return nil, err
	}
	scoped := make([]repository.Contact, 0, len(r.allowed))
	for _, c := range all {
		if _, ok := r.allowed[c.ID]; ok {
			scoped = append(scoped, c)
		}
	}
	return scoped, nil
}

// ReplayTodoist drives the REAL CadenceSyncProvider with a fake Client (no
// OAuth/HTTP). It exercises cadence task reconciliation; there is no inbound
// sender or pending equivalent, so MatchIntent is ignored. "Settled" = Sync's
// own DB writes complete + any enqueued jobs drain.
//
// Namespace scoping: the provider's reconcile lists ALL cadence-bearing contacts
// DB-wide (ListContactsWithContactBy) and may create/update/close contact_task
// rows on any of them. To keep the replay non-destructive on the shared test DB
// (where the real internal/todoist cadence tests run concurrently), the adapter
// injects a namespace-scoped contact-lister wrapper so the reconcile only ever
// sees THIS harness's seeded contacts — it can never touch another namespace's
// (or a real) contact. The before/after contact_task id-delta is still tracked
// into the cleanup ledger as belt-and-suspenders. contactIDs is the caller's
// seeded set, returned for assertion convenience.
func (h *Harness) ReplayTodoist(ctx context.Context, contactIDs []uuid.UUID) (TodoistResult, error) {
	syncRepo := repository.NewSyncRepositoryWithPool(h.database.Queries, h.database.Pool)

	// Build the namespace-scoped allow-set: every contact this harness seeded
	// (the ledger) plus the caller's explicit set, so the reconcile is confined
	// to the namespace regardless of which contacts carry cadence + contact_by.
	allowed := make(map[uuid.UUID]struct{})
	for _, id := range h.snapshotContactIDs() {
		allowed[id] = struct{}{}
	}
	for _, id := range contactIDs {
		allowed[id] = struct{}{}
	}
	scopedContacts := &namespaceScopedContactRepo{real: h.contactRepo, allowed: allowed}

	provider := todoist.NewCadenceSyncProvider(
		syntheticTodoistOAuth{},
		repository.NewContactTaskRepository(h.database.Queries),
		scopedContacts,
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
