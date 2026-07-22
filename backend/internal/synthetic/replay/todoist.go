package replay

import (
	"context"
	"fmt"
	"math/big"
	"strings"
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

// Synthetic follow-up shape constants mirror what FollowUpManager.applyCreate
// writes in production (content link, marker instance, project/label metadata).
// syntheticIntegrationInstanceID is shared by the marker's Instance and the
// integration_instance_id metadata key so they agree, exactly as prod does.
const (
	syntheticFrontendBase          = "https://synthetic.local"
	syntheticIntegrationInstanceID = "synthetic-todoist-instance"
	syntheticFollowUpProjectID     = "synthetic-followup-project"
	syntheticFollowUpLabelName     = "synthetic-followup-label"
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

// fakeTodoistClient is the Client the Todoist replay drives instead of the real
// Sync API. It has no inbound interaction graph; "settled" = the provider's own DB
// writes (task reconciliation for the seeded contacts) complete inside Sync.
// MatchIntent is n/a for Todoist. Two behaviors, both always on:
//
//   - It ALWAYS finalizes item_add commands. For each command carrying a TempID
//     (only item_add sets one), Sync returns TempIDMap[TempID] = a prod-shaped
//     alphanumeric id, so the provider's inline processTempIDMappings swaps the
//     temp UUID for a finalized Todoist-v1 id WITHIN the same Sync call — leaving
//     every reconcile-created cadence_due row with an alphanumeric external_task_id
//     and a cleared pending_temp_id, exactly as production's temp→real handoff does.
//   - It returns the configured `items` as the incremental Sync response. Empty for
//     the plain reconcile-only replay (ReplayTodoist); the recurring-edit replay
//     (ReplayTodoistRecurringEdit) configures one recurring item so processItem
//     drives the real handleRecurringDetection path.
type fakeTodoistClient struct {
	items []todoist.SyncItem
}

func (fakeTodoistClient) QuickAdd(_ context.Context, _ string, _ string) (*todoist.QuickAddTask, error) {
	return &todoist.QuickAddTask{}, nil
}

func (c fakeTodoistClient) Sync(_ context.Context, _ string, _ []string, commands []todoist.SyncCommand) (*todoist.SyncResponse, error) {
	resp := &todoist.SyncResponse{SyncToken: "synthetic-sync-token", FullSync: true, Items: c.items}
	for _, cmd := range commands {
		// Only item_add carries a TempID; finalize each so the reconcile's inline
		// processTempIDMappings resolves temp→real within this same tick.
		if cmd.TempID == "" {
			continue
		}
		if resp.TempIDMap == nil {
			resp.TempIDMap = make(map[string]string)
		}
		resp.TempIDMap[cmd.TempID] = finalizeTempID(cmd.TempID)
	}
	return resp, nil
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

// newScopedTodoistProvider builds the REAL CadenceSyncProvider wired to the fake
// Client, with the namespace-scoped contact-lister BOTH Todoist replays share so
// the scoping cannot drift between them. The provider's reconcile lists ALL
// cadence-bearing contacts DB-wide (ListContactsWithContactBy) and may
// create/update/close contact_task rows on any of them; the wrapper filters that
// enumeration to this harness's seeded contacts (the ledger ∪ extra) so a replay
// in one namespace can never touch another namespace's (or a real) contact on the
// shared test DB. clientItems is the inbound incremental Sync response — nil for
// the reconcile-only replay, one recurring item for the recurring-edit replay.
func (h *Harness) newScopedTodoistProvider(clientItems []todoist.SyncItem, extra []uuid.UUID) *todoist.CadenceSyncProvider {
	syncRepo := repository.NewSyncRepositoryWithPool(h.database.Queries, h.database.Pool)

	allowed := make(map[uuid.UUID]struct{})
	for _, id := range h.snapshotContactIDs() {
		allowed[id] = struct{}{}
	}
	for _, id := range extra {
		allowed[id] = struct{}{}
	}
	scopedContacts := &namespaceScopedContactRepo{real: h.contactRepo, allowed: allowed}

	return todoist.NewCadenceSyncProvider(
		syntheticTodoistOAuth{},
		repository.NewContactTaskRepository(h.database.Queries),
		scopedContacts,
		syncRepo,
		config.TestConfig(),
		h.bus,
		h.cadenceUpdater,
		h.database.Pool,
		func(string) todoist.Client { return fakeTodoistClient{items: clientItems} },
		// Temp-ID finalize deps: the fake client always finalizes item_add
		// commands via its TempIDMap, and reconcile-created cadence rows are
		// `managed`, so finalizeTempIDMappingTx takes the managed branch (no
		// riverInserter / remoteClose). remoteCloseEnabled=false mirrors the
		// harness's follow-up mode (off).
		nil,
		false,
	)
}

// ReplayTodoist drives the REAL CadenceSyncProvider with a fake Client (no
// OAuth/HTTP). It exercises cadence task reconciliation; there is no inbound
// sender or pending equivalent, so MatchIntent is ignored. "Settled" = Sync's
// own DB writes complete + any enqueued jobs drain. The provider is built with the
// namespace-scoped contact-lister (see newScopedTodoistProvider) so the reconcile
// only touches this harness's contacts on the shared test DB. The before/after
// contact_task id-delta is still tracked into the cleanup ledger as belt-and-
// suspenders. contactIDs is the caller's seeded set, returned for assertion
// convenience.
func (h *Harness) ReplayTodoist(ctx context.Context, contactIDs []uuid.UUID) (TodoistResult, error) {
	provider := h.newScopedTodoistProvider(nil, contactIDs)

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

// ReplayTodoistRecurringEdit drives the contact's managed cadence_due task to the
// `unmanaged` surface state through the REAL production path — the only way prod
// reaches it. It feeds the CadenceSyncProvider one inbound Todoist item: the task's
// own (finalized, alphanumeric) external id, now marked recurring. processItem
// matches it by external id and routes through handleRecurringDetection, exactly as
// production does when a user edits the Todoist task to repeat. This replaces the
// former raw UpdateContactTaskState write, which forged a state the lifecycle would
// never otherwise hold.
//
// Ordering contract: call AFTER ReplayTodoist, whose reconcile finalizes the temp
// id into the alphanumeric external id this method matches on (matching on the temp
// UUID would miss). Namespace scoping: provider.Sync ALWAYS reconciles after
// processing items (DB-wide), so the provider is built with the same
// namespace-scoped contact-lister ReplayTodoist uses (the shared
// namespaceScopedContactRepo wrapper) — by construction the trailing reconcile
// enumerates only this harness's contacts, never re-stranding a temp id or touching
// another namespace. That confinement is regression-guarded by the cross-namespace
// bystander in TestReplayTodoistRecurringEdit_UnmanagesViaRealPath (an other-namespace
// reconcile-eligible contact an unscoped reconcile would create a task on).
// The just-unmanaged row survives that reconcile:
// GetContactTaskByContactCadenceDue is state-agnostic and reconcile skips a
// non-managed row, and the partial unique index blocks a duplicate regardless. The
// task id is already in the cleanup ledger (ReplayTodoist tracked it). Draws no
// generator PRNG (reads a seeded contact + its task), so it is safe after the
// deterministic id sequence the earlier replays depend on.
func (h *Harness) ReplayTodoistRecurringEdit(ctx context.Context, contactID uuid.UUID) error {
	taskRepo := repository.NewContactTaskRepository(h.database.Queries)
	task, err := taskRepo.GetContactTaskByContactCadenceDue(ctx, contactID, todoist.SourceName)
	if err != nil {
		return fmt.Errorf("get cadence-due task for contact %s: %w", contactID, err)
	}

	// One inbound item: the task's finalized external id, edited to recur.
	recurringItem := todoist.SyncItem{
		ID:  task.ExternalTaskID,
		Due: &todoist.SyncDue{IsRecurring: true},
	}
	provider := h.newScopedTodoistProvider([]todoist.SyncItem{recurringItem}, []uuid.UUID{contactID})

	accountID := h.gen.Prefix() + "todoist"
	state := &repository.SyncState{
		Source:    repository.InteractionSourceTodoist,
		AccountID: &accountID,
		Metadata: map[string]any{
			todoist.MetadataKeyProjectID: h.gen.Prefix() + "project",
			todoist.MetadataKeyLabelID:   h.gen.Prefix() + "label",
		},
	}
	if _, err := provider.Sync(ctx, state, nil); err != nil {
		return fmt.Errorf("todoist recurring-edit sync for contact %s: %w", contactID, err)
	}

	// Settle Gate B over the seeded contacts (no domain predicate — the recurring
	// detection's state write is synchronous within Sync).
	if err := h.Settle(ctx, nil, ""); err != nil {
		return err
	}
	return nil
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
func (h *Harness) SeedPendingFollowUp(ctx context.Context, contactID uuid.UUID, fullName string) (uuid.UUID, error) {
	taskRepo := repository.NewContactTaskRepository(h.database.Queries)
	deadline := accelerated.GetCurrentTime().Add(followUpSeedWindow)

	content := fmt.Sprintf("Follow up: [%s](%s/contacts/%s) (awaiting reply)", fullName, syntheticFrontendBase, contactID)
	markerJSON, err := contacttask.EncodeMarker(contacttask.CRMMarker{
		ContactID: contactID.String(),
		Kind:      contacttask.KindReachOut,
		Lifecycle: contacttask.LifecycleFollowUpLoop,
		Instance:  syntheticIntegrationInstanceID,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("encode follow-up marker for contact %s: %w", contactID, err)
	}

	task, err := taskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
		ContactID:      contactID,
		Provider:       todoist.SourceName,
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleFollowUpLoop,
		ExternalTaskID: externalTaskIDForContact(contactID),
		State:          string(repository.ContactTaskStateManaged),
		Metadata: map[string]any{
			"due_date":                deadline.Format(todoist.DateFormat),
			"content":                 content,
			"marker_json":             string(markerJSON),
			"project_id":              syntheticFollowUpProjectID,
			"label_name":              syntheticFollowUpLabelName,
			"integration_instance_id": syntheticIntegrationInstanceID,
		},
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("seed pending follow-up for contact %s: %w", contactID, err)
	}
	h.track(func(c *created) { c.addContactTask(task.ID) })
	return task.ID, nil
}

// Visible-task spread constants. The spread attaches user-style MANUAL tasks to a
// fixed, creation-index-selected subset of the cadence-bearing catalog so the
// product-visible task axis (manual/follow-up tasks — the contact page never lists
// cadence_due) forms a 0/1/multiple distribution.
const (
	// visibleSpreadMinCatalog is the minimum cadence-bearing catalog size the
	// spread requires: it addresses creation indices 1, 2, and 3, so index 3 must
	// exist. Below this the spread is a documented no-op (protects a custom short
	// profile that lowers SeededContacts).
	visibleSpreadMinCatalog = 4
	// base62UUIDWidth is the maximum number of base62 digits a 128-bit UUID needs
	// (ceil(128 * ln2 / ln62) = 22). paddedBase62UUID left-pads to this fixed width
	// so the (uuid, ordinal) external id is injective.
	base62UUIDWidth = 22
	// visibleTaskOrdinalWidth is the fixed base62 width of the per-contact task
	// ordinal appended to the padded UUID.
	visibleTaskOrdinalWidth = 2
)

// manualTaskSpec is one MANUAL task the visible-task spread seeds, addressed by
// creation index into the cadence-bearing catalog slice.
type manualTaskSpec struct {
	contactIdx int    // index into the creation-ordered cadence-bearing catalog ids
	ordinal    int    // this contact's Nth manual task (0-based) — external-id uniqueness
	kind       string // rotates reach_out/send/reminder so all three kinds appear
	ageDays    int    // created_at = anchor - ageDays days (varies the link age)
}

// visibleTaskSpread is the FIXED, creation-index-based manual-task allocation:
// indices 1 and 2 each get one manual task (the 1-visible cohort); index 3 gets
// two (the >1-visible cohort). Every other catalog contact gets none (the
// 0-visible majority — it keeps its background cadence_due row, which the contact
// page never lists). Selection is by creation index, never by a random contact
// UUID, so it is byte-stable across reseeds. Kinds cover all three user-pickable
// values; ageDays spans three link-age buckets (days / weeks / months).
var visibleTaskSpread = []manualTaskSpec{
	{contactIdx: 1, ordinal: 0, kind: contacttask.KindReachOut, ageDays: 2},
	{contactIdx: 2, ordinal: 0, kind: contacttask.KindSend, ageDays: 21},
	{contactIdx: 3, ordinal: 0, kind: contacttask.KindReminder, ageDays: 90},
	{contactIdx: 3, ordinal: 1, kind: contacttask.KindReachOut, ageDays: 2},
}

// SpreadResult is the settled outcome of SeedVisibleTaskSpread.
type SpreadResult struct {
	// ManualTasks is the total managed manual contact_task rows created.
	ManualTasks int
	// ContactsWithManualTasks is the distinct catalog contacts that received ≥1
	// manual task (the 1-visible + >1-visible cohorts).
	ContactsWithManualTasks int
	// ContactsWithMultipleManualTasks is the distinct catalog contacts that
	// received >1 manual task (the >1-visible cohort).
	ContactsWithMultipleManualTasks int
}

// SeedVisibleTaskSpread attaches user-style MANUAL tasks to a deterministic,
// creation-index-selected subset of the cadence-bearing catalog contacts, so the
// product-VISIBLE task axis (the manual/follow-up tasks the contact page lists —
// it never lists cadence_due) forms a realistic 0/1/multiple distribution.
//
// cadenceBearingIDs must be the CREATION-ORDERED cadence-bearing catalog ids: the
// spread selects cohorts by fixed index into that slice (see visibleTaskSpread),
// never by a random contact UUID, so the assignment is byte-stable across reseeds.
// It draws NO generator PRNG (only repository reads + writes), so it is safe to run
// inside the existing task block without shifting the deterministic id sequence the
// earlier source replays depend on. Below visibleSpreadMinCatalog contacts it is a
// documented no-op (the fixed indices would be out of range).
//
// Manual tasks are state='managed' (a user-created linked task's steady state;
// completion happens remotely) with lifecycle='manual' and a rotating kind. Each
// carries a distinct link age (created_at = anchor - ageDays) via
// CreateContactTaskAtTime. External ids are fixed-width, injective, alphanumeric
// (paddedBase62UUID) so a contact's second task never collides on
// unique_external_task_id. Records the reserved cohort ids on the Harness via
// SetManualCohortIDs for subject-scoped assertions.
func (h *Harness) SeedVisibleTaskSpread(ctx context.Context, cadenceBearingIDs []uuid.UUID) (SpreadResult, error) {
	if len(cadenceBearingIDs) < visibleSpreadMinCatalog {
		return SpreadResult{}, nil
	}

	taskRepo := repository.NewContactTaskRepository(h.database.Queries)
	anchor := h.gen.Anchor()

	perContact := make(map[uuid.UUID]int)
	cohortOrder := make([]uuid.UUID, 0, len(visibleTaskSpread))
	for _, spec := range visibleTaskSpread {
		contactID := cadenceBearingIDs[spec.contactIdx]

		contact, err := h.contactRepo.GetContact(ctx, contactID)
		if err != nil {
			return SpreadResult{}, fmt.Errorf("visible task spread: get contact %s: %w", contactID, err)
		}
		content := fmt.Sprintf("Task: [%s](%s/contacts/%s)", contact.FullName, syntheticFrontendBase, contactID)

		createdAt := anchor.Add(-time.Duration(spec.ageDays) * 24 * time.Hour)
		task, err := taskRepo.CreateContactTaskAtTime(ctx, repository.CreateContactTaskRequest{
			ContactID:      contactID,
			Provider:       todoist.SourceName,
			Kind:           spec.kind,
			Lifecycle:      contacttask.LifecycleManual,
			ExternalTaskID: paddedBase62UUID(contactID, spec.ordinal),
			State:          string(repository.ContactTaskStateManaged),
			Metadata:       map[string]any{"content": content},
		}, createdAt)
		if err != nil {
			return SpreadResult{}, fmt.Errorf("visible task spread: create manual task on contact %s: %w", contactID, err)
		}
		h.track(func(c *created) { c.addContactTask(task.ID) })

		if perContact[contactID] == 0 {
			cohortOrder = append(cohortOrder, contactID)
		}
		perContact[contactID]++
	}

	res := SpreadResult{ManualTasks: len(visibleTaskSpread)}
	for _, count := range perContact {
		res.ContactsWithManualTasks++
		if count > 1 {
			res.ContactsWithMultipleManualTasks++
		}
	}
	h.SetManualCohortIDs(cohortOrder)
	return res, nil
}

// paddedBase62UUID builds a fixed-width, injective, alphanumeric external task id
// from a contact UUID and a per-contact ordinal. base62UUID's width VARIES with the
// numeric value (a leading-zero byte shrinks it), so a bare concat of the id and an
// ordinal would not be injective — a shorter id + a longer ordinal could collide
// with a longer id + a shorter ordinal. Left-padding the UUID component to
// base62UUIDWidth and the ordinal to visibleTaskOrdinalWidth makes the split
// unambiguous, so (contactID, ordinal) → id is one-to-one. The result stays
// alphanumeric (base62 digits + '0' padding), matching the Todoist-v1 id shape.
func paddedBase62UUID(id uuid.UUID, ordinal int) string {
	raw := base62UUID(id)
	padded := strings.Repeat("0", base62UUIDWidth-len(raw)) + raw
	ord := new(big.Int).SetInt64(int64(ordinal)).Text(62)
	ordPadded := strings.Repeat("0", visibleTaskOrdinalWidth-len(ord)) + ord
	return padded + ordPadded
}

// externalTaskIDForContact derives a stable, alphanumeric external id from the
// contact's UUID via base62 (digits 0-9a-zA-Z). This matches the Todoist-v1 id
// shape (alphanumeric strings, not UUIDs) and is unique per contact, so it never
// collides on the global partial-unique follow-up index.
func externalTaskIDForContact(contactID uuid.UUID) string {
	return base62UUID(contactID)
}

// base62UUID renders a UUID's 16 bytes as base62 (0-9a-zA-Z) — the Todoist-v1 id
// shape (alphanumeric, not a hyphenated UUID). Bijective on the bytes, so the
// result is globally unique and satisfies the strict '^[A-Za-z0-9]+$' finalized-id
// coherence gate.
func base62UUID(id uuid.UUID) string {
	return new(big.Int).SetBytes(id[:]).Text(62)
}

// finalizeTempID maps an item_add command's temp id to the prod-shaped
// alphanumeric id the fake Sync returns for it (mirroring the temp→real handoff
// the real Todoist Sync API performs). The provider mints temp ids via
// uuid.New().String(), so the normal path parses + base62-encodes the UUID to
// match PR1's follow-up-id convention; a non-UUID temp id (never emitted by the
// provider) falls back to base62 of the raw bytes so the result stays alphanumeric
// and unique regardless.
func finalizeTempID(tempID string) string {
	if u, err := uuid.Parse(tempID); err == nil {
		return base62UUID(u)
	}
	return new(big.Int).SetBytes([]byte(tempID)).Text(62)
}
