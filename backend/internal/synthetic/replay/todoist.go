package replay

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
)

// followUpSeedWindow puts the seeded follow-up's deadline in the near future, so
// the row reads as a live, not-yet-breached loop (the production deadline is a
// cadence-scaled window from the outbound — CAD-011). Read off the accelerated
// clock, never the wall clock.
const followUpSeedWindow = 3 * 24 * time.Hour

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
		// Temp-ID finalize deps: the fake client returns an empty sync (no
		// temp_id_mapping), so the state-aware finalize never runs here.
		// remoteCloseEnabled=false mirrors the harness's follow-up mode (off).
		nil,
		false,
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

// TransitionTodoistCadenceTaskState fetches the seeded contact's managed Todoist
// cadence-due task (created by ReplayTodoist's reconcile) and transitions it to
// the target state via the production UpdateContactTaskState path, returning the
// task id. The prod-shaped profile uses it to cover the non-managed contact_task
// surface states (completed/dismissed/unmanaged) that the empty fake-Todoist sync
// never produces — reconcile alone creates only `managed` rows. The task id is
// already in the cleanup ledger (ReplayTodoist tracked it), so no extra tracking
// is needed. Errors (rather than no-ops) if the contact has no cadence-due Todoist
// task, so a mis-wired seed fails loudly.
func (h *Harness) TransitionTodoistCadenceTaskState(ctx context.Context, contactID uuid.UUID, state repository.ContactTaskState) (uuid.UUID, error) {
	taskRepo := repository.NewContactTaskRepository(h.database.Queries)
	task, err := taskRepo.GetContactTaskByContactCadenceDue(ctx, contactID, todoist.SourceName)
	if err != nil {
		return uuid.Nil, fmt.Errorf("get cadence-due task for contact %s: %w", contactID, err)
	}
	updated, err := taskRepo.UpdateContactTaskState(ctx, task.ID, state)
	if err != nil {
		return uuid.Nil, fmt.Errorf("transition contact_task %s to %s: %w", task.ID, state, err)
	}
	return updated.ID, nil
}

// SeedPendingFollowUp gives a contact a LIVE follow-up loop — the "awaiting reply"
// state (`has_pending_followup`, CAD-029/CAD-036).
//
// Why this is seeded directly instead of driven through the production path: a
// follow-up is normally opened by the FollowUpManager consumer on an outbound
// interaction (CAD-011), but the seed harness wires that consumer in
// FollowUpModeOff (harness_setup.go) — and even with it on, CAD-012 suppresses
// follow-ups for backdated automated outbounds, which is every interaction a
// historical replay produces. So no seeded world could ever contain this state.
//
// That was not a cosmetic gap. With zero such contacts in the world, the tours
// could not capture the state, and the judge — shown only contact pages with no
// "Awaiting reply" marker — concluded the FEATURE DID NOT EXIST and reported a
// confident, well-cited, false regression on CAD-036, every run. Absence of
// evidence is indistinguishable from absence of the feature, and no judge-side
// rigor can fix that; only the evidence can.
//
// The row mirrors the production shape FollowUpManager writes (provider/kind/
// lifecycle/due_date metadata), settling on `managed` — the steady state after a
// remote create, and the same state the empty fake-Todoist sync leaves cadence
// tasks in. Draws NO generator PRNG (it only reads a seeded contact id), so it is
// safe to run alongside the other non-generator replays without shifting the
// deterministic id sequence earlier source replays depend on.
//
// CALLER'S CONTRACT: `contactID` must be a contact with a cadence AND an outbound —
// a follow-up is opened BY an outbound (CAD-011), so one on a contact with neither
// renders as "Awaiting reply" with nothing to be awaiting a reply TO, a state
// production cannot reach. (The judge failed the contact page for exactly that when
// this seed first hung a follow-up on an arbitrary contact.) This is NOT asserted
// here: the cadence engine writes last_outreach_at asynchronously from its River
// worker (and is its sole writer — CAD-005, CI-guarded), so it is still nil at seed
// time. The caller establishes the chain by construction (see the awaiting-reply
// scenario in profiles.go), and the profile coverage check proves it post-Quiesce.
func (h *Harness) SeedPendingFollowUp(ctx context.Context, contactID uuid.UUID) (uuid.UUID, error) {
	taskRepo := repository.NewContactTaskRepository(h.database.Queries)
	deadline := accelerated.GetCurrentTime().Add(followUpSeedWindow)
	task, err := taskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
		ContactID: contactID,
		Provider:  todoist.SourceName,
		Kind:      contacttask.KindReachOut,
		Lifecycle: contacttask.LifecycleFollowUpLoop,
		State:     string(repository.ContactTaskStateManaged),
		Metadata:  map[string]any{"due_date": deadline.Format(todoist.DateFormat)},
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("seed pending follow-up for contact %s: %w", contactID, err)
	}
	h.track(func(c *created) { c.addContactTask(task.ID) })
	return task.ID, nil
}
