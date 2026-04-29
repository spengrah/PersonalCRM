# Task Direction Taxonomy + Lifecycle Decoupling — Spec

**Issue:** #260 (originating); #264, #255, #259, #180 (dependencies, all merged)
**Status:** Draft v8 (Recent activity simplified — `last_contacted` dropped from view, awaiting-reply inlined; contact list view column rename added)
**Branch:** `feat/task-direction-taxonomy` (proposed)
**Last Updated:** 2026-04-28

---

## 1. Overview

### What

Replace the single `contact_task.kind` axis with two orthogonal axes:

- **`kind`** — semantic completion behavior (what direction does the resulting interaction have, and does it spawn a follow-up?)
- **`lifecycle`** — origin/scheduling/dismissal rules (how was this task created, how is it scheduled, how does it die?)

User-pickable kinds become **`reach_out`**, **`send`**, **`reminder`** (three options in the AddTaskModal picker; legacy `meet` and `action` stay in the schema but are not user-creatable).

The existing `Mark as Contacted` button on the contact detail page evolves into a `Log Interaction` modal supporting all three directions (mutual / outbound / inbound) with a backdate-able date picker. The standalone "Last contacted" inline-edit row is removed; `last_contacted` folds into a renamed "Recent activity" section alongside the existing directional signals.

### Why

The current `kind` column conflates two orthogonal concerns. `cadence`, `follow_up`, and any new `reach_out` user-creatable kind would all share completion semantics (outbound interaction + spawn follow-up) but diverge on lifecycle behavior (creation source, scheduling rule, dismissal handling). This bundling has made it awkward to (a) expand the user-creatable task surface beyond the single legacy `action` kind, and (b) reason about what semantics a given task has on completion.

Direction modeling (#255/#259) and the event bus cutover (#180) landed in the past few months. Direction is now first-class on `interaction`; per-kind completion logic flows through the event bus rather than the Todoist provider's monolithic completion handler. The infrastructure is ready for this split.

The user-facing motivation: the CRM is currently cadence-shaped — the only "thing the system actively does for you" is "tell you when to reach out." Users have richer task shapes they want to express (send a gift, prep before a meeting, plan an intro) that today either don't fit `action`'s mutual-on-completion semantics or get awkwardly forced into the cadence loop.

### Key Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Architecture | Two orthogonal axes: `kind` (semantics) + `lifecycle` (origin/rules) | Decoupling resolves the redundancy where `cadence`/`follow_up`/`reach_out` would all share completion semantics but diverge on rules; principled separation of "what" from "how" |
| User-pickable kinds | `reach_out`, `send`, `reminder` (3) | Three meaningfully different direction-shapes a contact-action can have: outbound + ball-on-them, outbound + terminal, no interaction |
| `meet` as user kind | Dropped | Synchronous engagements live in Google Calendar; calendar sync produces mutual interactions automatically; manual logger covers retroactive logging. No task shape needs a `meet`-creating path |
| Lifecycle values | `manual`, `cadence_due`, `followup_loop` | One per origin: user-picker, scheduler, FollowUpManager. Scheduler/FollowUpManager always set `kind = reach_out` |
| Picker labels | "Reach out" / "Send" / "Reminder" | "Reminder" defended as catchall by exclusion (cf. "Task" earlier rejected); user-facing accuracy with `send` and `reach_out` |
| Display label per task | Derived from (kind, lifecycle) pair (e.g., `reach_out + cadence_due` → "Cadence" badge) | Picker shows three options; badge surfaces origin for system tasks without forcing them into the picker |
| Existing `cadence` rows | Migrate → `kind=reach_out, lifecycle=cadence_due` | Lossless; semantics match |
| Existing `follow_up` rows | Migrate → `kind=reach_out, lifecycle=followup_loop` | Lossless; semantics match |
| Existing `action` rows | Stay as-is (`kind=action`, `lifecycle=manual`) | Deprecated kind dispatched as mutual for legacy rows; no new instances. `action` → `meet` migration ruled out (no behavior change); `action` → `send` ruled out (would contradict historical interaction direction) |
| Down-migration safety | Abort if any user-created row exists with a kind/lifecycle pair the old schema can't represent — concretely: `lifecycle = 'manual'` AND `kind IN ('reach_out', 'send', 'reminder')`. Also abort if duplicate `(contact_id, provider, lifecycle)` rows exist for `cadence_due` or `followup_loop` (would hit the new partial unique indexes). | Forward-only realistic posture; the down is a safety net only when no user-created tasks have landed yet AND no migrated rows somehow created uniqueness conflicts |
| Reminder completion event | **Do not publish `task.completed` for `kind=reminder`.** Provider's `handleTaskCompletion` short-circuits: transitions local state to `completed` in a single-statement tx (no event). Trade-off: reminder completions are absent from the event audit trail and the `event` table. Justification: any other choice (publishing with `direction="none"`, a new event kind) requires extending the payload schema and consumer dispatch for zero downstream consumer benefit. The local row's `state` transition is the durable record. | Cleaner than introducing a no-op direction or a new event kind; observability cost is acceptable for a kind that is explicitly "no interaction recorded" by definition |
| `send` follow-up suppression mechanism | **Extend payload contracts.** Add a `suppress_follow_up bool` field (zero value `false` = "do not suppress" = current behavior preserved) to `TaskCompletedPayload` (V2) and `InteractionRecordedPayload` (V3). Provider sets `suppress_follow_up=true` only when emitting `task.completed` for `kind=send`. InteractionRecorder propagates the flag onto `interaction.recorded`. FollowUpManager reads `suppress_follow_up` and early-returns when `true`. | Without an explicit signal, FollowUpManager spawns a follow-up for *every* outbound `interaction.recorded` (current contract per `followup_manager.go`). `task_kind` in the payload doesn't reach FollowUpManager; threading kind through would be a wider contract change. A boolean hint is the minimum viable suppression mechanism and is forward-additive. *Polarity choice* — see also dedicated `SuppressFollowUp` polarity row below |
| Column DEFAULT permanence | **Keep `lifecycle TEXT NOT NULL DEFAULT 'manual'` permanently** (do NOT drop the default after backfill). | Eliminates the version-skew window where existing sqlc inserts (which omit `lifecycle`) would fail against a NOT NULL column without DEFAULT. Cost: developers don't get a NOT NULL violation if they forget to set lifecycle in new code. Mitigation: code review + integration tests + the explicit kind/lifecycle CHECK constraints catch typos. The `manual` default is also semantically correct for the rare case where it falls through |
| Legacy `last_contacted` API endpoint migration | All callers of `useUpdateLastContacted` migrate to call `POST /interactions` directly (with `direction=mutual`, `occurred_at` defaulting to `now`). Affects `frontend/src/app/dashboard/page.tsx` (overdue-contact "Mark as Contacted" buttons), `frontend/src/app/contacts/page.tsx` (contact-list row "Mark as Contacted" buttons), and the contact detail page (header button → modal). The PATCH endpoint, its handler, and `contactsApi.updateLastContacted` / `useUpdateLastContacted` are removed in this PR. | Earlier wording wrongly claimed the inline pencil was the only caller; the dashboard and contacts list both depend on the same mutation. Single endpoint = single direction-aware code path; eliminates dual-write surface across three frontend surfaces |
| Task-list filter API contract | Backend handler accepts `?kind=` (validator widened to `oneof=reach_out send reminder meet action`) AND a new `?lifecycle=` param (`oneof=manual cadence_due followup_loop`). Frontend filter UI (badge-driven) translates each badge to a `(kind, lifecycle)` query-param pair. Old `?kind=cadence` and `?kind=follow_up` values are NOT accepted post-migration — frontend is the only caller and is updated in this PR. | Locks the load-bearing API contract that earlier draft left "specify in plan." `lifecycle` becomes a first-class filter axis since the same `kind=reach_out` covers three lifecycles |
| Lookup API lifecycle-awareness | Repository lookups currently keyed on `kind` (e.g., `GetContactTaskByContact(ctx, contactID, provider, kind)`) gain a `lifecycle` parameter where the call site cares about origin (cadence recovery, follow-up uniqueness checks). Most call sites become `(provider, lifecycle)` keyed since the new `kind` for system tasks is uniformly `reach_out`. | Without this, the post-migration recovery code can't distinguish a cadence-due row from a follow-up-loop row from a manual reach-out — they share `kind=reach_out` |
| Legacy `action` row dismissal semantics | **Preserved unchanged**. Existing Todoist deletion of `kind=action` continues to transition state to `unmanaged` (current `provider.go` behavior). The new `lifecycle=manual` dismissal rule ("deletion → `dismissed`") applies only to new `kind IN (reach_out, send, reminder)` rows. | Avoids a silent behavior change on the user's historical task data; legacy rows keep legacy semantics by deprecation, not by retroactive policy |
| Two-axis dispatch principle (clarified) | *Behavioral* dispatch reads exactly one axis (interaction direction reads `kind`; cadence-skip math reads `lifecycle`). *Display labels and uniqueness predicates* may combine `(kind, lifecycle)` because the semantic cell is the cross-product. | Earlier wording overclaimed "no code path reads both"; that's only true for behavior selection, not for derived view formatting or constraint definitions |
| Composite (kind, lifecycle) CHECK | Add a row-level `CHECK` constraint enforcing the valid pairs explicitly: `(reach_out, manual)`, `(reach_out, cadence_due)`, `(reach_out, followup_loop)`, `(send, manual)`, `(reminder, manual)`, `(meet, manual)`, `(action, manual)`. Independent CHECKs on each column would permit invalid pairs like `(send, followup_loop)` or `(reminder, cadence_due)` whose semantics would contradict between behavior dispatch (reads `kind`) and lifecycle dispatch (reads `lifecycle`). | Hard DB-level guarantee that every row sits in a known semantic cell. Cheap to add; impossible to drift past in a future code change |
| `SuppressFollowUp` polarity | Use `SuppressFollowUp bool` (not `SpawnFollowUp`) on `TaskCompletedPayload` V2 and `InteractionRecordedPayload` V3. Provider sets `SuppressFollowUp=true` only for `kind=send`. Default zero value (`false`) means "do not suppress" = current behavior. FollowUpManager: `if p.SuppressFollowUp { return; }`. | Go bool zero-values to false, which under a `SpawnFollowUp` polarity would mean "do not spawn = suppress" — a constructor forgetting to set the field would silently break follow-up creation. Inverting the polarity to `SuppressFollowUp` makes the zero value semantically safe; only the `send` path needs explicit `true` |
| Manual logger UI | Evolve "Mark as Contacted" header button → "Log Interaction" modal (direction + date pickers). All other "Mark as Contacted" call sites (dashboard overdue list, contact list rows, inline pencil-edit) migrate to `POST /interactions` directly. The legacy `PATCH /contacts/:id/last-contacted` endpoint and its frontend hook are removed. | One concept, one entry point. Three frontend surfaces (detail page, dashboard, contact list) consolidate onto one direction-aware mutation. Earlier draft wrongly claimed the inline pencil was the only caller — the dashboard and contact list both depend on the same mutation |
| Logger time precision | Date-only | Cadence math is date-precision; users don't generally remember exact times |
| `last_contacted` display | Folds into renamed "Recent activity" section alongside `last_outreach_at`, `last_response_at`, awaiting-reply indicator | All read-only; updates only via the logger |
| Call-connects-mutual edge case | Punt — `reach_out` always records outbound on completion; follow-up self-corrects when their inbound reply arrives | Async messaging dominates; the rare connected-call case is small data-quality cost; follow-up loop self-heals on engagement |
| Interaction model gaps surfaced | Backdated timestamp picker (already supported), gap 1 confirmed in scope. Topic field (gap 4), interaction weight (gap 5), subkind (gap 6), `expects_reply` hint (gap 7) flagged as future-additive but out of scope here | Schema-additive future work; current model is comfortable with these additions |
| Kind/lifecycle CHECK constraints | Add CHECKs constraining each to its known set | Typo guard; matches Option A's principle that every kind/lifecycle is a known semantic cell |
| Todoist marker JSON | New tasks store both `kind` and `lifecycle`; reading-side parsing of legacy markers infers `lifecycle` from old `kind` value | Round-trip integrity; no Todoist-side rewrite needed for tasks created before this migration |
| Multi-contact events (β) | Out of scope | User has a graph data model planned for future; not appropriate to grow the interaction model toward it now |

### Non-Goals

- Modeling open obligations as a first-class concept (D priority, low; collapses into tasks-with-or-without-deadlines and the existing follow-up loop).
- Memory / structured facts about contacts (B priority; orthogonal — interaction model is comfortable with future `topic` field but the work itself is its own scope).
- Multi-contact events / shared calendar event entities (β; reserved for the future graph model).
- Sphere/role/context tagging on contacts (F priority; orthogonal).
- Renaming the existing `note` entity (mentioned as one option to free up "Note" for the picker label; deferred).
- Surfacing `last_outreach_at` on contact list views (display change is detail-page only here).
- Recurring user-created tasks beyond what Todoist's recurrence already provides.
- Adding interaction `topic`, `weight`, `subkind`, or `expects_reply` columns now (tracked as future work).
- A backfill script to migrate `action` rows to `meet` (`action` stays as a deprecated kind in perpetuity).

---

## 2. Architecture

### The two-axis model

```
contact_task
  kind       reach_out | send | reminder | meet (legacy) | action (legacy)
  lifecycle  manual | cadence_due | followup_loop
  state      managed | completed | dismissed | …    (unchanged)
```

**`kind` controls completion semantics**:

| kind | Completion → interaction | Spawns follow-up? |
|---|---|---|
| `reach_out` | outbound | yes |
| `send` | outbound | no |
| `reminder` | *no interaction recorded* | no |
| `meet` (legacy) | mutual | no |
| `action` (legacy, deprecated) | mutual | no |

**`lifecycle` controls origin/scheduling/dismissal**:

| lifecycle | Origin | Scheduling rule | Dismissal semantics |
|---|---|---|---|
| `manual` | user picker (AddTaskModal) | user-supplied deadline (or none) | Todoist deletion = "this didn't happen / nevermind" |
| `cadence_due` | scheduler when `contact_by` reached | deadline = `contact_by`; drift resolution per #293 | Todoist deletion = "skip this cycle, advance contact_by" |
| `followup_loop` | FollowUpManager after each outbound interaction | deadline = outbound time + follow-up window | Todoist deletion = "stop nagging me, don't record outbound" (#264) |

**State space is constrained**: only `kind=reach_out` participates in all three lifecycles. `send`/`reminder` are user-only. Legacy kinds receive no new instances.

### Dispatch principle

**Behavioral** dispatch reads exactly one axis:

- **Interaction direction on completion** → reads `kind`.
- **Cadence-skip math, drift resolution, scheduler invariants** → read `lifecycle = cadence_due`.
- **Follow-up dismissal, auto-completion, unique-live index predicates** → read `lifecycle = followup_loop`.
- **Manual-task dismissal (new kinds only)** → reads `lifecycle = manual`.
- **Legacy `kind=action` dismissal** → preserved as-is (deletion → `unmanaged`); dispatched separately on the deprecated kind.

**Derived/composite reads** may combine `(kind, lifecycle)` — but only for view formatting or constraint expression, never for branching live behavior:

- Task-list badge label → derived from the `(kind, lifecycle)` pair (Section 5 table).
- Display-label code reads both because the semantic cell *is* the cross-product.
- Repository lookup APIs accept either axis depending on caller intent (e.g., scheduler recovery passes `lifecycle`; legacy `action` cleanup may still pass `kind`).

### Tasks vs. interactions

- **Tasks** are forward-looking intent ("I plan to reach out").
- **Interactions** are backward-looking events ("communication happened").
- Task completion publishes `task.completed`; the InteractionRecorder consumer writes the interaction with the direction implied by the kind.
- `reminder` kind is the only one that does not produce an interaction — completion transitions state locally and emits no interaction-producing event.

---

## 3. Schema Migration

Single migration `046_contact_task_lifecycle`. High-level steps (DDL specifics belong in the implementation plan; this section locks the *order* and *invariants*):

1. **Add column.** `lifecycle TEXT NOT NULL DEFAULT 'manual'`. **The DEFAULT is permanent** — it is *not* dropped after backfill. Rationale: existing sqlc-generated INSERT statements omit `lifecycle` in their column lists; keeping the DEFAULT means an old binary running briefly against the new schema (or a future code path that forgets to set `lifecycle`) will not fail. The CHECK constraints below catch typos; the DEFAULT is the floor.
2. **Backfill.** `UPDATE` existing `kind='cadence'` rows → `(reach_out, cadence_due)`; existing `kind='follow_up'` rows → `(reach_out, followup_loop)`. `kind='action'` rows untouched (already `manual` from DEFAULT).
3. **Rewrite partial unique indexes** to key on `lifecycle` instead of `kind`. The indexed-columns set changes for two of the three indexes (the old indexes carried `kind` in the indexed tuple; the new indexes drop it because the partial predicate now constrains lifecycle):
   - `unique_contact_provider_cadence` — old: indexed `(contact_id, provider, kind) WHERE kind='cadence'`. New: indexed `(contact_id, provider) WHERE lifecycle='cadence_due'`. **No state predicate** (matches existing migration-029 semantics: globally unique per contact/provider for cadence rows, regardless of state).
   - `idx_contact_task_followup_unique_live` — old: indexed `(contact_id, provider) WHERE kind='follow_up' AND state IN ('managed', 'pending_remote_create')`. New: indexed `(contact_id, provider) WHERE lifecycle='followup_loop' AND state IN ('managed', 'pending_remote_create')`. **Index name preserved** because `followup_manager.go` pattern-matches on `ConstraintName == "idx_contact_task_followup_unique_live"` for unique-violation recovery.
   - `idx_contact_task_followup_idempotency` — old: indexed `(contact_id, kind, idempotency_key) WHERE idempotency_key IS NOT NULL`. New: indexed `(contact_id, idempotency_key) WHERE lifecycle='followup_loop' AND idempotency_key IS NOT NULL`. Dropping `kind` from the indexed columns is safe because the partial predicate now constrains the row class to followup-loop only.
4. **Add CHECK constraints**:
   - Per-column typo guards: `kind IN (reach_out, send, reminder, meet, action)`, `lifecycle IN (manual, cadence_due, followup_loop)`.
   - **Composite pair CHECK**: `(kind, lifecycle)` must be one of `(reach_out, manual), (reach_out, cadence_due), (reach_out, followup_loop), (send, manual), (reminder, manual), (meet, manual), (action, manual)`. This enforces the constrained state space (Section 2 table) at the DB level — without it, independent column CHECKs would permit semantically incoherent rows like `(send, followup_loop)` or `(reminder, cadence_due)`.
5. **Update SQL queries** referencing old kind values. Affected files: `backend/internal/db/queries/contact.sql` (the contact-list `followup_filter` predicates appear in multiple list queries) and `backend/internal/db/queries/contact_task.sql` (pending follow-up and idempotency lookups). Migrate each `kind = 'follow_up'` to `lifecycle = 'followup_loop'`. Run `make sqlc`.

**Migration order safety:** the `UPDATE`s in step 2 must complete before step 4 runs the CHECK creation — otherwise the CHECK creation would scan rows mid-backfill. Indexes in step 3 can be reordered with backfill but are placed after backfill to avoid scanning during the bulk update.

### Down-migration

Aborts with an explicit error in any of the following preconditions:

- Any row exists with `lifecycle = 'manual'` AND `kind IN ('reach_out', 'send', 'reminder')` — these user-creatable kinds have no representation in the pre-migration schema.
- Any duplicate `(contact_id, provider)` exists across all `lifecycle = 'cadence_due'` rows (any state) — would violate the pre-migration `unique_contact_provider_cadence` index, which is globally unique per contact/provider with no state filter.
- Any duplicate `(contact_id, provider)` exists across `lifecycle = 'followup_loop'` rows in `state IN ('managed', 'pending_remote_create')` — would violate the pre-migration `idx_contact_task_followup_unique_live` index.

If preconditions pass, the down migration reverses the index predicate changes (back to kind-keyed), runs the inverse `UPDATE`s (`reach_out + cadence_due` → `cadence`; `reach_out + followup_loop` → `follow_up`; `action + manual` stays `action`), drops the CHECK constraints, drops the column.

---

## 4. Backend Behavior

### Dispatch surface (full enumeration of touched sites)

The migration is not a small SQL rename — `kind` is currently a persistence key, idempotency namespace, Todoist marker field, API filter validator, frontend type discriminator, and publisher-side event semantic. All of the following dispatch sites must be updated coherently in this PR:

**Backend Go:**
- `backend/internal/todoist/provider.go` — `handleTaskCompletion`, `handleSkipTrigger`, deadline-edit/drift handlers, and `tryRecoverPendingTempID` all branch on kind.
- `backend/internal/consumer/followup_manager.go` — task creation, idempotency-key lookup, and `FindPendingFollowUp` repository call.
- `backend/internal/scheduler/scheduler.go` — cadence task creation path.
- `backend/internal/service/contact_task.go` — `CreateActionTask` hardcodes `kind = action`; gains a kind parameter.
- `backend/internal/repository/contact_task.go` — lookup APIs gain optional `lifecycle` parameter; existing `kind`-keyed callers either migrate or stay as legacy-only paths.
- `backend/internal/api/handlers/contact_task.go` — `oneof=action cadence follow_up` validator becomes `oneof=reach_out send reminder` for creation, plus a wider set for filtering.
- `backend/internal/api/handlers/contact.go` — the `PATCH /contacts/:id/last-contacted` handler is removed (per Section 1 decision; "Mark as Contacted" cuts over to `POST /interactions`).

**Backend SQL:**
- `backend/internal/db/queries/contact.sql` — eight `followup_filter` predicates across contact-list queries.
- `backend/internal/db/queries/contact_task.sql` — pending follow-up lookup, idempotency lookup, and any other kind-filtered queries.

**Frontend TS:**
- `frontend/src/types/contact-task.ts` — `kind` enum (now `reach_out | send | reminder | meet | action`) plus a new `lifecycle` enum/discriminator.
- `frontend/src/lib/contact-tasks-api.ts` — request/response types include `lifecycle`; create-task body accepts `kind`.
- `frontend/src/hooks/use-contact-tasks.ts` — query keys and invalidation; mutation accepts `kind`.
- `frontend/src/components/contacts/add-task-modal.tsx` — 3-option picker.
- `frontend/src/components/contacts/tasks-section.tsx` — badge derivation (kind, lifecycle table) and filter UI translates badge values to `(kind, lifecycle)` query params.
- `frontend/src/app/contacts/[id]/page.tsx` — Mark as Contacted button → Log Interaction modal; inline pencil-edit removed; Recent activity section rename + merge of `last_contacted` into the directional row.
- `frontend/src/app/dashboard/page.tsx` — overdue-list "Mark as Contacted" button cuts over to `POST /interactions`.
- `frontend/src/app/contacts/page.tsx` — contact-list row "Mark as Contacted" button cuts over to `POST /interactions`.
- `frontend/src/lib/contacts-api.ts` — `updateLastContacted` method removed.
- `frontend/src/hooks/use-contacts.ts` — `useUpdateLastContacted` hook removed.
- `frontend/src/lib/query-invalidation.ts` — invalidation rules updated; the existing `interaction.created` rule (or its equivalent) is the path for the consolidated quick-action.

**Cross-cutting:**
- API Swagger docs / OpenAPI generated comments — the removed PATCH endpoint comment block and `UpdateLastContactedRequest` model are deleted; `POST /interactions` parameter docs reviewed for the broader caller set.
- `backend/cmd/crm-api/main.go` route registration — the PATCH route deletion.
- E2E test files / Playwright spec files exercising "Mark as Contacted" — selectors and assertions updated where they assumed the old endpoint or the inline-edit UI.
- Test fixtures, mock servers, and any hardcoded `kind` values in test data (search across `*_test.go` for `"action"`, `"cadence"`, `"follow_up"` literals).
- Project docs / READMEs in `.ai/` that reference the old kind taxonomy or the removed endpoint.

**Todoist marker JSON:**
- Read path translates `kind=cadence|follow_up|action` to the new `(kind, lifecycle)` pair before any DB lookup. New writes include both fields.

### Task creation paths

- `ContactTaskService.CreateActionTask` accepts a `kind` parameter validated against `{reach_out, send, reminder}`; always sets `lifecycle = manual`.
- API handler accepts `kind` in `POST /contacts/:id/tasks` body.
- Scheduler (cadence) and FollowUpManager (followup_loop) set `kind = reach_out` and the appropriate lifecycle when creating system tasks.

### Task completion → interaction

Todoist provider's `handleTaskCompletion` reads `contact_task.kind` to derive interaction direction:

- `reach_out` → publish `task.completed` with `direction=outbound`, `suppress_follow_up=false` (default — follow-up will be spawned).
- `send` → publish `task.completed` with `direction=outbound`, **`suppress_follow_up=true`** (suppresses the follow-up that would otherwise be spawned by FollowUpManager).
- `meet` / `action` (legacy) → publish with `direction=mutual`. (`suppress_follow_up` is irrelevant for non-outbound; consumer ignores.)
- `reminder` → **do not publish `task.completed` at all**. Provider transitions state to `completed` in a single-statement tx and returns. No interaction row, no follow-up, no event.

**Trade-off (locked) for reminder:** reminder completions are absent from the event audit trail. Alternative considered (publish with `direction="none"` or a new `task.reminder_completed` event) was rejected because it requires extending payload schemas and consumer dispatch for zero downstream consumer benefit. If observability is needed in the future, a dedicated event kind can be added additively.

### `send` follow-up suppression — payload contract change

This is the single load-bearing event-bus change in this PR. Without it, the existing pipeline would spawn a follow-up for *every* outbound interaction, including `send` task completions — which contradicts `send`'s "ball stays with me" semantics.

**`TaskCompletedPayload` (V2)** gains:

```go
type TaskCompletedPayload struct {
    Version     int       `json:"version"`         // bump to 2
    ContactID   uuid.UUID `json:"contact_id"`
    TaskID      string    `json:"task_id"`
    TaskKind    string    `json:"task_kind"`       // "reach_out" | "send" | "meet" | "action"
    CompletedAt time.Time `json:"completed_at"`
    Direction   string    `json:"direction"`
    SuppressFollowUp bool `json:"suppress_follow_up"` // NEW. Zero value (false) preserves current behavior. true only when TaskKind=="send"
}
```

**`InteractionRecordedPayload` (V3)** gains the same field. InteractionRecorder propagates `SuppressFollowUp` from the `task.completed` payload onto its emitted `interaction.recorded`. For non-task-completion sources (telegram outbound, manual-logger outbound), InteractionRecorder leaves `SuppressFollowUp=false` (zero value; current behavior preserved).

**FollowUpManager** consumes `interaction.recorded`. Adds an early-return guard: `if p.SuppressFollowUp { return; }`. All other follow-up creation paths (cadence-due → outbound interaction, telegram outbound) carry `SuppressFollowUp=false` (zero value) and behave exactly as today.

**Why `SuppressFollowUp` and not `SpawnFollowUp`:** Go bool zero-values to false. With a `SpawnFollowUp` polarity, a constructor that forgets to set the field would zero-value to "do not spawn" = silently disable follow-ups. With `SuppressFollowUp`, the zero value is the safe default ("do not suppress = spawn as today"); only the `send` path needs explicit `true`.

**Version migration — applied separately to each payload type.** Each rule sets the zero-value field when an older payload is decoded, but with the inverted polarity the zero value (`false`) is *already* the safe default — these rules are belt-and-braces in case future polarity changes are made:

- `TaskCompletedPayload`: current is V1; bump to V2 with the new `SuppressFollowUp` field. With the chosen polarity, V1 payloads decode to `SuppressFollowUp=false` automatically, which is the correct behavior. No explicit rule needed beyond accepting V1 envelopes.
- `InteractionRecordedPayload`: current is V2 (per `events/kinds.go` — V2 was added in #180 PR 7 for `PrevCadenceSnapshot`); bump to V3 with the new `SuppressFollowUp` field. With the chosen polarity, V2 payloads decode to `SuppressFollowUp=false` automatically, which is the correct behavior. No explicit rule needed beyond accepting V2 envelopes.

Polarity is doing the version-skew protection here: zero-value is safe, so we get backward compatibility for free across both payload types. The reflect-based `kindPayloadTypes` registry in `events/kinds.go` is updated to the new struct shapes.

**Other `interaction.recorded` consumers must accept V3.** `CadenceUpdater` currently asserts `p.Version != 2` and rejects (per `cadence_updater.go`); `FollowUpManager` does the same. Both consumer version checks must be updated to **accept V2 OR V3** — V2 for any rows in the `event` table from before the bump, V3 for new emissions. New field semantics:

- `SuppressFollowUp` is read by FollowUpManager only.
- CadenceUpdater ignores `SuppressFollowUp` (irrelevant to cadence math); accepting V3 is a no-op for its logic.

This is a forward-additive contract change. The polarity choice + version-acceptance widening together ensure no V2 row in the `event` table is mishandled when consumers cut over.

The `task.completed` payload's `task_kind` field is populated with post-migration `kind` values for observability. Consumers (InteractionRecorder, CadenceUpdater, FollowUpManager) dispatch on `direction` and (now) `suppress_follow_up`, not `task_kind`.

### System task copy generation

The Todoist task content (the body that shows up in the Todoist UI) is generated from `lifecycle`, not `kind`:

- `cadence_due` → `"Reach out to {contact name}"` (current behavior preserved).
- `followup_loop` → `"Follow up with {contact name}"` (current behavior preserved).
- `manual` → user-supplied text (no system copy).

### Dismissal / skip-trigger handling

Todoist provider's `handleSkipTrigger` dispatches on `lifecycle` for non-legacy kinds:

- `cadence_due` → cadence-skip math (advance `contact_by`, current behavior preserved).
- `followup_loop` → #264 dismissal (no outbound recorded, no new follow-up).
- `manual` (new kinds: `reach_out` / `send` / `reminder`) → simple state transition to `dismissed`; no interaction recorded; no follow-up created.

**Legacy `kind=action` rows preserve current behavior**: Todoist deletion / label removal continues to transition state to `unmanaged` (matches today's `provider.go` path). This is *not* the new `manual` lifecycle's "deletion → `dismissed`" rule. Two paths because:

- Legacy `action` rows pre-date the dismissal model in #264; their existing semantics (unmanaged on Todoist removal, the user can re-enable later) is the historical contract.
- Retroactively changing dismissal behavior on a user's historical task data is a silent behavior change we explicitly avoid.
- The split is finite: no new `kind=action` rows get created, so the legacy path naturally drains.

Implementation: `handleSkipTrigger` first checks `kind == 'action'` for the legacy path; otherwise dispatches on `lifecycle` for the new path.

### Manual interaction logger

The "Log Interaction" modal calls the existing `POST /interactions` endpoint (already accepts `direction`, `occurred_at`, `description` per `backend/internal/api/handlers/interaction.go`).

The legacy `PATCH /contacts/:id/last-contacted` endpoint and its handler are **removed** in this PR. **All** existing frontend callers of `useUpdateLastContacted` migrate to `POST /interactions`:

- **Contact detail header button** (`frontend/src/app/contacts/[id]/page.tsx`) → "Log Interaction" modal → `POST /interactions` with user-picked direction + date.
- **Inline pencil-edit on `last_contacted`** (same file) → removed entirely.
- **Dashboard overdue-list "Mark as Contacted" buttons** (`frontend/src/app/dashboard/page.tsx`) → one-click → `POST /interactions` with `direction=mutual`, `occurred_at=now`. (Quick-action UX preserved; same one-click feel; underlying call changes.)
- **Contact-list row "Mark as Contacted" buttons** (`frontend/src/app/contacts/page.tsx`) → same as dashboard.
- **`contactsApi.updateLastContacted`** in `frontend/src/lib/contacts-api.ts` and **`useUpdateLastContacted`** in `frontend/src/hooks/use-contacts.ts` are removed.

Net API surface change: existing `POST /interactions` becomes the single mutation path for manual interactions; one legacy endpoint + handler + Swagger doc removed; three frontend surfaces consolidated onto one mutation.

### Todoist marker JSON evolution

The CRM marker embedded in Todoist task descriptions (read on every sync to identify CRM-managed tasks) currently looks like:

```json
{"crm": true, "contact_id": "...", "kind": "cadence|follow_up|action", "instance": "..."}
```

After migration, new markers look like:

```json
{"crm": true, "contact_id": "...", "kind": "reach_out|send|reminder|meet", "lifecycle": "manual|cadence_due|followup_loop", "instance": "..."}
```

**Read-side translation** is mandatory — Todoist tasks created before this migration will continue to live in Todoist with old-format markers indefinitely. The marker parser (currently in `provider.go` around line 1490 in `tryRecoverPendingTempID` and similar reconciliation paths) translates old → new in-memory before any DB lookup:

| Legacy marker `kind` | Translated to (kind, lifecycle) |
|---|---|
| `cadence` (or absent — defaulted to cadence today) | `(reach_out, cadence_due)` |
| `follow_up` | `(reach_out, followup_loop)` |
| `action` | `(action, manual)` (kind preserved as legacy) |

**Lookup-API consequence**: `repository.GetContactTaskByContact(ctx, contactID, provider, kind)` and similar lookups must accept and use `lifecycle` for system kinds — after migration, three different DB rows for the same contact can all have `kind=reach_out` (one per lifecycle), so kind alone is insufficient. The repository signature gains a lifecycle parameter; legacy `action` lookup paths can keep the kind-only signature since `action` rows don't share `kind` with anything else.

### Version skew (DB migrated, old binary running) — stop-the-world required

**The deployment is stop-the-world, and that operational guarantee is load-bearing for correctness.** The CHECK constraints catch *insert* violations from an out-of-order deployment — but they do NOT catch *update* drift on existing rows, which is the more dangerous case. Specifically:

- The Todoist sync path resolves an incoming task by `external_task_id` (kind-independent — see `contact_task.sql` `GetContactTaskByExternalID`). After migration, a row that was `kind='follow_up'` pre-migration is now `(reach_out, followup_loop)`. An old binary loading that row by external ID sees `kind='reach_out'` (unrecognized as follow-up) and dispatches via the wrong branch in `provider.go`'s `handleSkipTrigger` — routing to cadence-skip math instead of `handleFollowUpDismissal`. Result: `contact_by` advances incorrectly; a replacement cadence task is created in place of dismissal. **No CHECK violation, no insert error — silent semantic drift.**
- Same pattern for `handleTaskCompletion` (kind-dispatched), drift-resolution, and any other update-path branches in the old binary.

**Operational requirement (load-bearing):** the migration runs only after the old binary is fully stopped. The new binary starts only after the migration completes. There must be no window where old code is processing Todoist sync ticks, scheduler work, or event-bus consumers against a migrated database.

The deployment script (or the operator) must enforce this ordering:

```
1. Stop personalcrm-backend systemd service.
2. Run migration 046 against the DB.
3. Verify migration succeeded (row counts, CHECK constraint installed).
4. Deploy new binary.
5. Start personalcrm-backend systemd service.
```

**The CHECK constraints remain as a defense-in-depth signal.** If step 1 is skipped — old binary still running during step 2 — any insert from the old code path will fail loudly (since the new kind CHECK rejects `cadence`/`follow_up`). This catches the *insert* mistake but not the *update* mistake. The update mistake has no CHECK-based safety net; the only mitigation is the operational ordering above.

**Why this is acceptable:** this CRM is single-user, single-deployment on a Pi running as `personalcrm-backend` systemd unit. The deployment is already stop-start (not zero-downtime, no rolling restart, no blue-green). Encoding "stop → migrate → start" into the runbook is consistent with the existing operational model — not a new constraint.

---

## 5. Frontend Changes

### AddTaskModal

Add a 3-option kind picker at the top of the form: **Reach out / Send / Reminder**. Default: Reach out. Pass `kind` through `useCreateActionTask` to the API.

### Contact detail page header

`Mark as Contacted` button → `Log Interaction` button. Click opens a modal containing:

- Direction picker (Mutual / Outbound / Inbound). Default: Mutual.
- Date picker (date-only). Default: today.
- Submit calls the manual logger endpoint.

Modal pattern matches AddTaskModal for keyboard/mobile consistency.

### Contact detail page body

- Standalone "Last contacted" row removed.
- "Direction signals" row renamed "Recent activity" and now also displays `last_contacted` alongside `last_outreach_at`, `last_response_at`, awaiting-reply indicator.
- All four are read-only; updates only via the new Log Interaction modal.

### Task list display

Each task in the contact detail task list (and any other surface listing tasks) shows a kind badge derived from `(kind, lifecycle)`:

| (kind, lifecycle) | Badge |
|---|---|
| `reach_out + cadence_due` | Cadence |
| `reach_out + followup_loop` | Follow-up |
| `reach_out + manual` | Reach out |
| `send + manual` | Send |
| `reminder + manual` | Reminder |
| `action` (legacy) | Action *(legacy styling if useful)* |

Existing task-list filter (which today filters by `?kind=action|cadence|follow_up`) updated to filter by the badge value, which under the hood translates to a (kind, lifecycle) pair.

### Contact list view — Last Contact column

The contact list page (`frontend/src/app/contacts/page.tsx`) currently has a "Last Contact" sortable column rendering `contact.last_contacted`, sorted by `last_contacted`. Per the same reasoning as the contact-detail "Recent activity" change (Section 4 manual logger discussion + the `last_contacted` semantic mismatch with its label), the list view's column is replaced:

- **Header:** "Last response" (was "Last Contact")
- **Cell value:** `contact.last_response_at` (was `contact.last_contacted`)
- **Sort field:** `last_response_at` (was `last_contacted`)
- **Default sort direction:** desc (most recent response first) — preserves current behavior

**Cross-cutting changes** required for the new sortable field (per CLAUDE.md "When You Change" table — sortable contact field touches 4 SQL queries + handler oneof + `SortField` type + `ContactListParams.sort`):

- Backend: SQL sort queries gain `last_response_at` ordering; handler validator's `oneof=` includes `last_response_at`; service's `SortField` type updated. The deprecated `last_contacted` sort can stay for one PR cycle as a safety net or be removed in the same PR — implementation-plan call.
- Frontend: `SortField` type union includes `last_response_at`; the cell + header + default-sort logic switch over.

**Sequencing note:** this change is internally separable from the rest of the spec — the contact list view does not depend on the task taxonomy refactor or the Log Interaction modal. **It can ship in its own PR before, after, or alongside the main work**, hitting this same spec. If split, the implementation plan should call out the dependency direction (none — purely cosmetic/sort change with the cross-cutting field plumbing).

### UI sketches

Wireframes for the two new/restructured UI elements: SVG for visual fidelity. Final visual polish (exact colors, spacing, lucide icon choices) iterates in dev; these capture the structural decisions.

**SVG wireframes**
- [`wireframes/log-interaction-modal.svg`](./wireframes/log-interaction-modal.svg) — desktop + mobile, with annotations
- [`wireframes/recent-activity-section.svg`](./wireframes/recent-activity-section.svg) — desktop + mobile, with annotations

#### Log Interaction modal

**Design decisions:**

- *Component types:* segmented control for direction (visible choices, fastest to pick), native date input. No description/topic field — kept as a quick logger.
- *Label copy:* modal title `Log Interaction for {contact}`; field labels `Direction` and `Date`; submit `Log`.
- *Visual weight:* direction picker is the primary decision; date is secondary because the default is usually correct.
- *Iconography:* arrow symbols on direction segments echo the existing direction-signal arrows (`↗` outbound, `↙` inbound). Modal close button uses lucide `X` icon to match AddTaskModal.
- *Spacing:* `px-4 py-3` header/footer + `px-4 py-4 space-y-4` body, mirroring AddTaskModal exactly.
- *Keyboard:* Escape closes; outside-click closes; initial focus lands on the selected `Mutual` segment; Tab moves through direction → date → Cancel → Log. Submit is always enabled because defaults are valid.
- *API:* submit calls `POST /interactions` with the chosen `direction` and `occurred_at` (date-only).

**Alternatives considered:**

1. **Segmented control** *(recommended)* — fastest, all options visible, good mobile fit. Trade-off: short labels rely on iconography to disambiguate.
2. **Radio list with helper text** — clearer for first-time use (especially explaining "Mutual"). Trade-off: feels like a heavier form, takes more vertical space.
3. **Dropdown / select** — smallest footprint. Trade-off: hides the most important choice and adds taps; works against the quick-logging intent.

**Spec tension flagged:** AddTaskModal focuses a text input on open; this modal has no text field, so initial focus lands on the selected direction segment. This is a deliberate divergence from AddTaskModal's pattern, justified by the absence of a free-text input here.

#### Recent activity section

Replaces the current standalone "Last contacted" `<dl>` row AND the conditional "Direction signals" row. Single consolidated row, two direction-aware signals, optional inline awaiting-reply indicator.

**`last_contacted` is dropped from the displayed signals** — it's bumped by inbound + mutual events (per `repository.CadenceApplyFlagsByDirection`), which is the same direction set as `last_response_at`. The two columns carry effectively the same data; surfacing both is redundant, and the historical "Last contacted" label has been quietly mismatched with the data ever since the direction model landed (#255/#259) — the label suggests outbound action ("I contacted them") but the column tracks inbound/mutual engagement. The DB column stays for cadence math; the UI switches to the directionally-correct labels.

**Design decisions:**

- *Two signals only:* `last_outreach_at` and `last_response_at`. They are directionally orthogonal — outreach is mine, response is theirs. `last_contacted` is dropped from the view.
- *Component:* keep the existing contact detail `<dl>` row pattern. No primary-vs-secondary highlight; the two values are equal-weight within the row.
- *Awaiting-reply pairing:* inline at the end of the `Last response` line, not as a standalone third row. Conceptually pairs with the "they haven't responded yet" state. Saves vertical space.
- *Awaiting-reply indicator:* amber text, renders only when `has_pending_followup` is true. No "no pending" placeholder.
- *Read-only:* pencil affordance removed entirely. Editing moves to the Log Interaction modal.
- *Empty states:* `Last outreach` / `Last response` lines simply don't render when null. The Recent activity row itself remains visible always (preserves the always-on row replacing the old standalone "Last contacted" row).
- *Spacing:* preserves existing `py-4 sm:py-5 sm:grid sm:grid-cols-3 sm:gap-4 sm:px-6`. Mobile layout collapses gracefully via the existing `sm:` breakpoint.

**Alternatives considered:**

1. **Two flat directional lines + inline awaiting-reply** *(recommended)* — minimal vertical space, clean direction-orthogonal framing. Trade-off: no primary signal highlighted; relies on the user to read both lines.
2. **Primary `last_response_at` highlight + secondary `last_outreach_at`** — emphasizes "when did they engage" as the dominant question. Trade-off: outreach feels demoted; both axes matter equally for assessing the relationship.
3. **Three flat rows** (keeping `last_contacted` despite redundancy) — preserves the most existing data on screen. Trade-off: redundant signals, label-vs-data mismatch on `last_contacted` persists.

---

## 6. Rollout

Single PR. Internally staged: schema + backfill migration → consumer/provider/handler updates that read the new column → frontend changes. No shadow/dual-write phase — the migration is small and reversible (with the abort safety on the down).

**Deployment is stop-the-world.** Per Section 4 "Version skew," the operational ordering must be: stop systemd unit → run migration → deploy new binary → start systemd unit. There must be no window where old code processes work against a migrated DB. This is a load-bearing operational requirement (CHECK constraints catch insert drift but not update-path drift). The deployment runbook documents this ordering explicitly.

---

## 7. Test Plan

**Migration tests**

- Backfill produces the expected `(kind, lifecycle)` pairs from prior `cadence` / `follow_up` / `action` rows. Legacy `action` rows end up `(action, manual)`.
- Down-migration preflight: aborts with a clear error when **any** of the unmappable conditions holds (`(reach_out, manual)` rows present, `(send, manual)` rows present, `(reminder, manual)` rows present, duplicate `(contact_id, provider)` for `cadence_due` or `followup_loop` would be created).
- Down-migration on a clean migrated DB (no user-creates) reverses cleanly: rows return to `cadence` / `follow_up` / `action`; partial unique indexes return to kind-keyed predicates.
- CHECK constraints reject unknown kind / lifecycle values.
- **Composite (kind, lifecycle) CHECK**: insert attempts of invalid pairs are rejected. Concrete cases: `(send, followup_loop)`, `(send, cadence_due)`, `(reminder, cadence_due)`, `(reminder, followup_loop)`, `(action, cadence_due)`, `(action, followup_loop)`, `(meet, cadence_due)`, `(meet, followup_loop)` — all must fail with the composite CHECK violation.
- **Version-skew insert guard**: an attempt to `INSERT INTO contact_task ... kind = 'cadence'` post-migration (simulating an old binary insert) fails with a CHECK violation rather than silently creating an `(cadence, manual)` row. Same for `kind = 'follow_up'`. *Note*: this only catches insert drift; update-path drift on existing rows is mitigated operationally via stop-the-world deployment, not testable from the new binary's test suite. The deployment runbook ordering is the only safety net for that class of drift.
- **DEFAULT preservation**: `INSERT` statements that omit `lifecycle` succeed and the row gets `lifecycle = 'manual'` (DEFAULT not dropped post-backfill).
- Migration ordering: backfill `UPDATE` runs before CHECK creation (CHECK creation on a not-yet-backfilled column would fail validation).

**Unit / integration — completion semantics**

- `reach_out` completion → `task.completed` published with `direction=outbound`, `suppress_follow_up=false` (zero value). InteractionRecorder writes interaction; FollowUpManager spawns follow-up.
- `send` completion → `task.completed` published with `direction=outbound`, **`suppress_follow_up=true`**. InteractionRecorder writes interaction. FollowUpManager **observes the flag and early-returns; does NOT spawn a follow-up**. Explicit assertion: zero `kind=reach_out, lifecycle=followup_loop` rows created post-completion.
- `reminder` completion → **no `task.completed` event published**, **no interaction row written**, **no cadence column bumped**, state transitions to `completed` locally.
- `meet` legacy completion → mutual interaction recorded.
- `action` legacy completion → mutual interaction recorded (regression-guard for behavior preservation).
- **TaskCompletedPayload V1 → V2 decode**: synthetic V1 envelope (no `suppress_follow_up` field) decodes with `SuppressFollowUp=false` (Go zero value); downstream FollowUpManager spawns follow-up. The polarity choice (Section 4) makes the zero value the safe default — no explicit post-decode rule needed.
- **InteractionRecordedPayload V2 → V3 decode**: synthetic V2 envelope (no `suppress_follow_up` field) decodes with `SuppressFollowUp=false` (zero value); FollowUpManager spawns follow-up. **This is the load-bearing test that prevents the V3 bump from silently suppressing follow-ups for V2 rows already in the `event` table.**
- **CadenceUpdater accepts V3**: synthetic V3 `interaction.recorded` envelope decodes and CadenceUpdater applies cadence math correctly. Regression-guard against the existing `p.Version != 2` rejection.
- **CadenceUpdater accepts V2**: synthetic V2 `interaction.recorded` envelope (no `SuppressFollowUp` field) still applies cadence math correctly. Confirms V2/V3 dual-acceptance during the transition window.
- **Cross-source preservation**: a telegram-outbound `interaction.recorded` event (no upstream `task.completed`) still spawns a follow-up — `SuppressFollowUp=false` (zero value) for non-task-completion sources.

**Unit / integration — dismissal semantics per lifecycle**

- `cadence_due` Todoist deletion → cadence-skip math advances `contact_by`; no interaction recorded.
- `followup_loop` Todoist deletion → state → `dismissed`; no outbound recorded; no successor follow-up created (#264).
- `manual` Todoist deletion (new kinds) → state → `dismissed`; no interaction recorded.
- **Legacy `action` Todoist deletion** → state → `unmanaged` (regression-guard: behavior unchanged from pre-migration).

**Unit / integration — uniqueness invariants**

- Two simultaneous cadence-due tasks for the same contact/provider → second insert fails on `unique_contact_provider_cadence`.
- Two simultaneous followup_loop tasks (live state) for the same contact/provider → second insert fails on `idx_contact_task_followup_unique_live`. Recovery path in `followup_manager.go` correctly identifies the constraint by name and falls through to refresh.
- Two manual reach-out tasks for the same contact → both succeed (no uniqueness on manual lifecycle).

**Unit / integration — marker round-trip**

- Old marker (`kind=cadence`, no `lifecycle`) → translated in-memory to `(reach_out, cadence_due)` → DB lookup uses lifecycle → finds the migrated row.
- Old marker (`kind=follow_up`) → translated to `(reach_out, followup_loop)` → DB lookup succeeds.
- Old marker (`kind=action`) → translated to `(action, manual)` → DB lookup succeeds.
- New marker (both `kind` and `lifecycle` present) → no translation; direct lookup.
- Marker missing both fields → reject (current behavior preserved).

**Unit / integration — manual logger**

- `POST /interactions` with `direction=outbound` → bumps `last_outreach_at`, not `last_contacted`.
- `POST /interactions` with `direction=mutual` → bumps `last_contacted` and `last_response_at`.
- `POST /interactions` with `direction=inbound` → bumps `last_contacted` and `last_response_at`; auto-completes any pending follow-up.
- `POST /interactions` with backdated `occurred_at` → cadence math uses the backdated time.
- Removed `PATCH /contacts/:id/last-contacted` endpoint returns 404 / 405.
- **Dashboard "Mark as Contacted" quick-action** issues `POST /interactions { direction: "mutual" }` and updates contact state correctly (regression test for the migrated dashboard caller).
- **Contact-list "Mark as Contacted" quick-action** same as dashboard (regression test for the migrated contacts-list caller).

**Unit / integration — SQL filter coverage**

- Contact-list queries with `followup_filter=has_followup` correctly identify contacts with `lifecycle=followup_loop` rows in active states (post-migration semantics).
- `contact_task` list filter API: `?kind=` accepts only post-migration values (`reach_out|send|reminder|meet|action`) — old `cadence`/`follow_up` values rejected with 400. New `?lifecycle=` param validated against `manual|cadence_due|followup_loop`. Frontend filter UI translates each badge to a `(kind, lifecycle)` pair.

**E2E**

- Create each user-pickable kind via the picker (Reach out / Send / Reminder); verify correct badge in the task list.
- Complete each kind from Todoist; verify the correct downstream effect (interaction direction, follow-up spawn, or no-op for reminder).
- Dismiss a `reach_out + manual` task via Todoist deletion; verify no interaction recorded and no follow-up spawned.
- Dismiss a legacy `action` task via Todoist deletion; verify state goes to `unmanaged` (regression).
- Log Interaction modal: log mutual, outbound, inbound; verify each updates the correct cadence column on the contact.
- Recent activity section shows merged `last_contacted` + `last_outreach_at` + `last_response_at` + awaiting-reply indicator; no inline-edit UI.

**Observability**

- Reminder completion: confirm explicitly via test that no `event` row is written and no `task.completed` envelope is published. (Regression-guard against accidental future change.)
- Todoist tasks created pre-migration with old markers continue to round-trip cleanly through one full sync cycle on the migrated codebase.

---

## 8. Out of Scope (Future)

Captured here to preserve context for the implementation plan, not to scope into this work:

- **Interaction `topic` field** — needed for memory (B priority); schema-additive.
- **Interaction `weight` field** — needed for proactive nudging (C) and reflection (E); schema-additive.
- **Interaction `subkind` / medium** — `text vs voice` granularity within a source.
- **Interaction `expects_reply` hint on outbound** — cleaner alternative to dismissal-based escape from the follow-up loop.
- **Multi-contact events** (β) — reserved for the planned graph data model.
- **`action` → `meet` backfill** — could be done later if symmetry feels valuable, but no current need.
- **Interaction "doesn't count" / `cadence_relevant: bool` flag** — when an interaction shouldn't reset the cadence clock.
- **`last_outreach_at` surfaced on contact list views** — currently detail-page only.

---

## 9. References

- **GH Issue:** #260 (originating)
- **Dependencies (merged):** #180 (event bus), #255 / #259 (direction model), #264 (follow-up dismissal), #293 (drift fix)
- **Design conversation:** brainstorming session 2026-04-27 / 2026-04-28 (this spec is the deliverable)
- **CLAUDE.md** project instructions
- **`.ai/rules/core.md`** absolute rules (sqlc, time acceleration, layered architecture, etc.)
- **`.ai/rules/code-review.md`** review standards
