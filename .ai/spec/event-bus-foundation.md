# Event Bus + Worker Queue Foundation — Spec

**Issue:** #180
**Status:** Draft v5 (Codex review rounds 1-4 applied)
**Last Updated:** 2026-04-13
**Resolves:** #208 (stuck sync states), #267 (stale backdated follow-ups), #265 (Todoist transactional ordering)
**Unblocks:** #70 (Gmail), #73 (iMessage), #74 (iCloud Contacts), #260 (task taxonomy), future LLM extraction
**Narrows:** #222 (OAuth provider refactor)

---

## 1. Overview

### What

Replace the current per-provider direct-write pattern (Telegram, Calendar, Todoist each call `contactService.RecordInteraction` and downstream effects synchronously) with an append-only event table + river-backed worker queue. All sync sources publish typed events; dedicated consumer services (`InteractionRecorder`, `CadenceUpdater`, `FollowUpManager`, `RematchDispatcher`) are the sole writers for their respective domains.

Also migrate the existing `robfig/cron` sync scheduler to a river `PeriodicJob` with per-account `UniqueOpts`, eliminating the `external_sync_state.status = 'syncing'` mutex and the stuck-state bug class it enables.

### Why

1. **Gmail and iMessage are next.** Each currently implied ~300 LOC of duplicated interaction/cadence/follow-up wiring under the per-provider pattern. Paying down the duplication once is cheaper than tripling it.
2. **iMessage ships events from a remote MacBook daemon over Tailscale.** The natural shape is an HTTP ingestion endpoint writing into an event log — which is exactly the bus.
3. **Ad-hoc async work is reinvented per-feature today.** Rematch's in-memory job map, Todoist's `todoist_close_pending` retry flag, and the `external_sync_state.status = 'syncing'` mutex are three separate incomplete implementations of a durable worker queue.
4. **Setter-injection cycles between services are removable.** `ContactService ↔ FollowUpService ↔ CadenceSyncProvider` today require two-phase construction in `main.go`; consumers listening to events break the cycle.
5. **Transactional publish unblocks #265.** River's `InsertTx(ctx, tx, ...)` lets the Todoist provider atomically commit a state change + enqueue a downstream job, which is the clean answer to the three non-atomic sites in `handleTaskCompletion`, `handleSkipTrigger`, and `tryRecoverPendingTempID`.

### Key Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Worker queue library | [river](https://riverqueue.com/) | Postgres-native (already have pgx/v5 & pg16), `UniqueOpts`, `PeriodicJob`, job-lease heartbeats with auto-retry-on-crash, `InsertTx`. Alternatives (neoq, goqite, asynq, faktory) either lack required primitives or add infra. See `.ai/log/plan/event-bus-foundation.md` (plan doc, forthcoming) for the alternatives matrix. |
| Event-vs-interaction boundary | `event` is raw substrate; `interaction` is derived semantic output written by `InteractionRecorder` | Some events (`calendar.declined`, `message.deleted`) produce no interaction. Keeping `interaction` preserves all existing queries, repositories, and invariants. |
| Consumer dispatch model | **Hybrid via direct-invoke primitives.** Consumers are defined as plain funcs (`func(ctx, tx, event) error`). River workers are thin wrappers that fetch the event and call the func. Manual UI path calls the consumer funcs **directly, in the same `pgx.Tx`** as the event insert — no river hop. Sync providers use `PublishTx` which enqueues river jobs for async processing. | Making `POST /contacts/:id/interactions` eventually-consistent would surprise the frontend. The direct-invoke design gives a single implementation (the consumer func) two call sites (synchronous for manual, async for sync providers) without duplicating logic. |
| Event publish atomicity | Atomic: one `pgx.Tx` covers the business write + `event` row + `river.InsertTx` for each consumer job | Uses the existing `Pool.Begin` pattern already present in `CreateContact`, `UpdateContact`, `MergeContacts`. No repository-wide tx threading required — services pass the `pgx.Tx` to `EventBus.PublishTx` directly. |
| Event payload schema | Typed Go structs per `kind`, marshaled to `payload jsonb` with a top-level `version` discriminator | Compile-time safety when adding/renaming fields; version discriminator reserves forward migration space. Loose `map[string]any` was considered and rejected for drift risk. |
| Forward-only field writes | SQL-level (`UPDATE contact SET last_contacted = $1 WHERE id = $2 AND ($1 > last_contacted OR last_contacted IS NULL)`) | Atomic under concurrency; no race window between read and write. Required for the acceptance criterion "out-of-order ingestion never moves monotone-forward fields backward." |
| Cutover mechanism | Per-consumer env flag (`EVENT_BUS_INTERACTION_MODE`, `EVENT_BUS_CADENCE_MODE`, `EVENT_BUS_FOLLOWUP_MODE`) with values `off \| shadow \| cutover` | Each consumer ships in `shadow` mode first, runs alongside the direct path, logs divergences; a tiny follow-up PR flips to `cutover`. **Rollback semantics:** the flag is a runtime switch ONLY during the shadow-window (between the shadow PR and the cutover PR). After the cutover PR merges, the direct-path code is removed; the flag has no `off`-mode to roll back to. Post-cutover rollback requires `git revert`. This is a deliberate trade-off — keeping the direct path forever would block the deduplication, cycle-breaking, and testability benefits that motivate #180. |
| Rematch migration approach | **True event consumer** via `contact_methods.added` (single bundled event per mutation). `ContactService` publishes the event with a pre-generated `rematch_job_id` embedded in the payload and all new methods in a slice. `RematchDispatcher` consumer reads it and invokes the rematch pipeline internally. Frontend mutation response still carries `rematch_job_id` (synchronously returned by `ContactService`). `GET /rematch/jobs/:id` still serves progress. | Satisfies #180's "RematchDispatcher becomes an event consumer" goal without breaking the #182 frontend contract. The `rematch_job_id` is generated before publish, so the caller has the ID even though the work runs async. `contactService.SetRematchService` setter is removed. |
| Rematch dedupe + serialization | **Dedupe:** `event.source_id = rematch_job_id` — the `event` table's `UNIQUE (source, source_id)` index prevents duplicate publish of the same mutation's event. River `UniqueOpts{ByArgs: []string{"ContactID", "RematchJobID"}, ByState: [scheduled, available, running, retryable]}` is a belt-and-suspenders check at enqueue. Distinct `RematchJobID`s for the same contact (= distinct mutations) produce distinct jobs — no cross-mutation collapse. **Execution serialization:** `RematchService.contactLocks sync.Map` (per the existing #182 spec) continues to serialize the handler body if two distinct-job-ID river jobs for the same contact execute concurrently on different workers. | UniqueOpts is a dedup primitive; contactLocks is the serialization primitive. Two different mechanisms, two different jobs. One-job-per-mutation contract from `.ai/spec/rematch-service.md` is preserved. |
| Todoist retry queue migration | `todoist_close_pending` metadata flag retired; river job `TodoistCloseTaskJob` with exponential retry owns the close command | River's native retry replaces the in-band "retry on next sync tick" polling. |
| River migrations | Applied via a separate `river migrate-up` step in `RunMigrations`, not interleaved with app migrations | River owns a `river_migration` table; keeping them separate avoids numbering collisions and matches river's documented deployment model. |
| Event table ownership | `event` table accessed through `repository/event.go`; no direct queries from services | Follows layered-architecture rule. |
| Publisher-side idempotency | `UNIQUE (source, source_id)` on `event`; `INSERT ... ON CONFLICT DO NOTHING` | External daemons can safely retry a batched post; in-process publishers that pass a stable `source_id` are automatically deduped. |
| Consumer-side idempotency | Each consumer scoped to its own invariants: `InteractionRecorder` reuses the existing `FindBySourceRef` path; `CadenceUpdater` uses forward-only SQL; `FollowUpManager` uses the new two-step create pattern described in §3.4.3 (insert-local-pending → remote-create → finalize); `RematchDispatcher` uses existing `matched_contact_ids` append-if-absent | Each consumer must tolerate river's retry/crash-restart semantics. `FollowUpManager`'s prior ordering (remote Todoist `item_add` before local insert) is **unsafe** under retry — redesigned in §3.4.3. |
| Event ordering guard | `FollowUpManager.CreateOrRefreshFollowUp` queries `interactionRepo.HasResponseAfter(contact_id, outreach_at)` before creating a follow-up; if a later inbound/mutual interaction exists for that contact, skip creation | Out-of-order event delivery (inbound arrives before outbound is processed) would otherwise create a stale follow-up that should have been suppressed. Complements the backdated-outbound skip. |

### Non-Goals (Deferred)

- **LLM extraction pipeline.** The worker queue unblocks it; no extractor is built here.
- **Knowledge graph layer** (`entity`/`relation`/`claim`).
- **Soft-delete consistency cleanup** across `contact_method` / `external_identity` / `external_contact` / `contact_task` — tracked separately.
- **Frontend changes** beyond what's required to keep existing flows working (no new UI).
- **Event-replay tooling / admin UI.** Events remain queryable by SQL; a replay CLI is future work.
- **Retiring the `rematch_job_id` return from mutation responses.** PR 10 makes `RematchDispatcher` a true event consumer, but `ContactService` still returns a pre-generated `rematch_job_id` synchronously to preserve the #182 frontend contract. A future migration could remove that return and switch the frontend to a status API or SSE push — out of scope here.
- **Migrating `PostImportHook` to events.** Out of scope; it already works.
- **Observability dashboards** beyond structured logs + river's built-in job state. Metrics/alerting are a follow-up.

### Resolves

- **#208** — Step 3 migrates the sync scheduler to a river `PeriodicJob` with per-account `UniqueOpts`. River's job-lease heartbeat auto-resumes killed syncs. The `external_sync_state.status = 'syncing'` mutex is retired. The bug class ceases to exist rather than being cleaned up by a watchdog.
- **#267** — Step 9 adds the backdated-outbound skip check inside `FollowUpManager.CreateOrRefreshFollowUp`. `accelerated.GetCurrentTime().Sub(outreachAt) > cadence_days * 24h && !isManual` → skip follow-up creation. Interaction still recorded; cadence still advances.
- **#265** — Step 11 migrates the three non-atomic Todoist sites (`handleTaskCompletion`, `handleSkipTrigger`, `tryRecoverPendingTempID`) to transactional event publish via `river.InsertTx`. The `replayCommittedUnsafe` mitigation in `processItems` is removed. The two guardrail tests (`TestHandleTaskCompletion_StateUpdateFailureDoesNotReturnError`, `TestHandleSkipTrigger_FailureDoesNotReturnErrorAndSetsUnsafe`) are deleted.

---

## 2. Functional Specification

### 2.1 User-Visible Behavior

Almost all behavior is preserved. The only user-visible changes:

1. **Manual "Record Interaction" still feels synchronous.** Response returns after `InteractionRecorder` + `CadenceUpdater` have committed for that event. Target latency: unchanged (<100ms local).
2. **Sync ticks crash-resume.** If the backend dies mid-sync, the next worker heartbeat resumes. No `status = 'syncing'` manual reset ever needed.
3. **Backdated outbound sync no longer creates stale Todoist follow-ups.** Historic-backfilled outbounds older than one cadence period skip the follow-up-task creation step. (User has not seen cosmetic cruft anyway since the #268 dismissal UI shipped; this removes the source.)
4. **External daemons can ingest events.** `POST /api/ingest/events` accepts API-key-authenticated batched event POSTs. First consumer: the iMessage MacBook daemon (not shipped here).

No frontend UI changes.

### 2.2 Acceptance Criteria

- [ ] `event` table exists, append-only, unique `(source, source_id)` idempotency, indexed on `(source, observed_at DESC)`.
- [ ] `CadenceUpdater` is the sole writer of `contact.last_contacted`, `contact.last_outreach_at`, `contact.last_response_at`, `contact.contact_by`. Static analysis: `grep` for direct UPDATE to these columns returns only the `CadenceUpdater`'s SQL.
- [ ] `InteractionRecorder` is the sole writer of `interaction` rows. `grep` for `INSERT INTO interaction` returns only its sqlc query.
- [ ] All existing providers publish events; no direct calls to `contactService.RecordInteraction` from provider code.
- [ ] `FollowUpManager` and `RematchDispatcher` consume events; `ContactService.SetFollowUpManager` and `ContactService.SetRematchService` setter-injection calls in `main.go` are removed.
- [ ] `FollowUpManager` skips follow-up creation for backdated outbound interactions (outreach older than one cadence period, `!isManual`). Unit test seeds a 3-month-old outbound; asserts no Todoist task is created, but the interaction and cadence fields are updated.
- [ ] `FollowUpManager` skips follow-up creation when a later inbound/mutual interaction exists for the contact (out-of-order event delivery guard). Integration test: publish inbound at T, then outbound at T-1d → assert no `contact_task` row.
- [ ] `FollowUpManager` crash-safe creation: simulated crash between local-insert (step 1) and Todoist remote-create (step 2); after retries, exactly one `contact_task` row exists and exactly one Todoist task exists. Verified via `followup_two_step_crash_test.go`.
- [ ] `InteractionRecorder` emits `interaction.recorded` atomically with the interaction insert — a DB-error injection test confirms that a failed interaction insert produces no downstream consumer jobs in `river_job`.
- [ ] Out-of-order ingestion (event B with `observed_at < last_contacted`) does NOT move `last_contacted` backward. Integration test seeds contact with `last_contacted = T-1d`, publishes event with `observed_at = T-7d`, asserts `last_contacted` unchanged.
- [ ] Replaying the same event (same `source` + `source_id`) is a no-op. Integration test seeds an event twice; asserts one `interaction` row, one cadence write.
- [ ] `POST /api/ingest/events` accepts batched events, API-key protected, dedup-aware, returns `{accepted, duplicate, rejected}` counts.
- [ ] River worker queue is wired. `make test` passes with river migrations applied. A lease-expired worker mid-sync auto-resumes on next fetch — verified via `sync_worker_leased_retry_test.go` (cancels worker context mid-job, advances river's lease clock, asserts re-dispatch and clean completion).
- [ ] Scheduler migrated to river periodic job. `external_sync_state.status = 'syncing'` column is unused; `UpdateSyncStateStatus(SyncStatusSyncing, ...)` call is removed.
- [ ] Telegram, Calendar, Manual UI, and Todoist providers all emit events. Direct writes from these providers to `interaction`, cadence columns, follow-up tasks, and rematch jobs are gone.
- [ ] The `replayCommittedUnsafe` + `processItemResult.Unsafe` mitigation in `processItems` is removed. The two Todoist guardrail tests (#265) are deleted.
- [ ] All existing `make test`, `make test-frontend`, and `make test-e2e` pass.
- [ ] New test coverage: event idempotency (unique `(source, source_id)`), out-of-order ingestion, consumer dispatch (each consumer fires on relevant `kind`), external HTTP publisher path (happy + auth-failure + malformed-batch + duplicate-in-batch), worker-crash sync recovery, backdated-outbound follow-up skip, shadow-mode divergence detection (shadow logs exist when paths diverge, don't exist when they agree).

---

## 3. Technical Specification

### 3.1 `event` Table

**Migration 036: `event` table**

```sql
-- 036_event_table.up.sql
CREATE TABLE event (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source text NOT NULL,
    source_id text,
    kind text NOT NULL,
    payload jsonb NOT NULL,
    observed_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_event_source_source_id
    ON event (source, source_id)
    WHERE source_id IS NOT NULL;

CREATE INDEX idx_event_source_observed_at
    ON event (source, observed_at DESC);

CREATE INDEX idx_event_kind_observed_at
    ON event (kind, observed_at DESC);
```

**Notes:**
- No `contact_id` column. Events are raw substrate; consumer jobs carry `contact_id` in their args if/when resolved.
- No `deleted_at`. Events are append-only; nothing deletes them. A future retention job may archive by `observed_at`; out of scope here.
- `source_id` is nullable because some internal events (e.g., `interaction.manual` with no external ref) don't have one. The unique index is partial.
- `kind` uses dot-notation namespacing: `{source_domain}.{action}`. Examples: `message.received`, `message.sent`, `calendar.attended`, `calendar.declined`, `task.completed`, `task.skipped`, `task.outreach_detected`, `interaction.manual`, `contact_methods.added`, `interaction.recorded`.

### 3.2 Typed Event Kinds

**`backend/internal/events/kinds.go`**

```go
package events

import (
    "encoding/json"
    "fmt"
    "time"

    "github.com/google/uuid"
)

type Kind string

const (
    // Raw-signal events — published by provider/publisher code.
    KindMessageReceived       Kind = "message.received"
    KindMessageSent           Kind = "message.sent"
    KindCalendarAttended      Kind = "calendar.attended"
    KindCalendarDeclined      Kind = "calendar.declined"
    KindTaskCompleted         Kind = "task.completed"
    KindTaskSkipped           Kind = "task.skipped"
    KindTaskOutreachDetected  Kind = "task.outreach_detected"
    KindInteractionManual     Kind = "interaction.manual"
    KindContactMethodsAdded   Kind = "contact_methods.added"

    // Derived events — emitted by consumers as part of the two-stage pipeline.
    // Published atomically inside the consumer's transaction.
    KindInteractionRecorded   Kind = "interaction.recorded"
)

// Envelope is the wire/DB shape. Payload is kind-specific.
type Envelope struct {
    ID         uuid.UUID       `json:"id"`
    Source     string          `json:"source"`
    SourceID   string          `json:"source_id,omitempty"`
    Kind       Kind            `json:"kind"`
    Payload    json.RawMessage `json:"payload"`
    ObservedAt time.Time       `json:"observed_at"`
}

// Per-kind typed payloads. Each includes Version int for forward compat.

type MessageReceivedPayload struct {
    Version    int        `json:"version"`   // start at 1
    ContactID  *uuid.UUID `json:"contact_id,omitempty"` // nil if unmatched
    PeerRef    string     `json:"peer_ref"`  // e.g. "tg:12345:67890"
    MessageAt  time.Time  `json:"message_at"`
    // ... direction-specific fields
}

type InteractionManualPayload struct {
    Version     int       `json:"version"`
    ContactID   uuid.UUID `json:"contact_id"`
    Direction   string    `json:"direction"` // outbound|inbound|mutual
    OccurredAt  time.Time `json:"occurred_at"`
    Description string    `json:"description,omitempty"`
}

// ... one payload struct per Kind.

// Marshal/Unmarshal helpers assert Kind matches payload type.
func Marshal[P any](kind Kind, payload P) (json.RawMessage, error) { /*...*/ }
func Unmarshal[P any](env Envelope, dst *P) error { /*...*/ }
```

**Notes:**
- Each payload type has a `Version int` field. When a field is added non-breakingly, the same struct is extended. When a breaking change happens, introduce `V2` and teach the consumer to handle both.
- Consumers use `Unmarshal[P](env, &p)` to decode into their expected payload type; type mismatch is a fatal job error.

### 3.3 `EventBus` — Publish Helper

**`backend/internal/events/bus.go`**

```go
package events

import (
    "context"
    "encoding/json"

    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/riverqueue/river"
)

type Bus struct {
    pool   *pgxpool.Pool
    river  *river.Client[pgx.Tx]
    eventRepo EventRepository
}

// Publish publishes a single event and enqueues all consumer jobs
// atomically in its own transaction. For publishing inside an existing
// transaction (e.g., alongside a business write), use PublishTx.
func (b *Bus) Publish(ctx context.Context, env Envelope) error { /*...*/ }

// PublishTx publishes an event + enqueues all consumer jobs within the
// caller's tx. The caller owns commit/rollback.
func (b *Bus) PublishTx(ctx context.Context, tx pgx.Tx, env Envelope) error {
    if err := b.eventRepo.InsertEvent(ctx, tx, env); err != nil {
        if errors.Is(err, db.ErrDuplicate) {
            // (source, source_id) collision → idempotent no-op
            return nil
        }
        return fmt.Errorf("insert event: %w", err)
    }
    for _, job := range consumerJobsForKind(env.Kind, env.ID) {
        if _, err := b.river.InsertTx(ctx, tx, job, nil); err != nil {
            return fmt.Errorf("enqueue consumer job %T: %w", job, err)
        }
    }
    return nil
}
```

**consumerJobsForKind** is a static table mapping `Kind` → list of river job kinds to enqueue. Each job carries the event `id` as its arg; the worker fetches the event from the `event` table when it runs (keeps job-arg payload small).

### 3.4 Consumer Services

**Design: direct-invoke consumer funcs with river-worker wrappers.**

Each consumer exposes a plain method `HandleEvent(ctx context.Context, tx pgx.Tx, env Envelope) error` on its consumer struct. The method is the single source of truth for its domain logic.

Two call sites invoke it:

1. **River worker (async)** — a thin wrapper worker fetches the event by ID, opens a `pgx.Tx`, and calls `HandleEvent`. On error, river retries per `WorkerOpts.MaxAttempts` with exponential backoff. On success, the tx commits. Used by sync providers (Telegram / Calendar / Todoist / HTTP ingest).

2. **Synchronous in-process (manual UI)** — the `RecordInteraction` HTTP handler opens a `pgx.Tx`, calls `bus.PublishTx(ctx, tx, env)` to insert the event row + enqueue the async consumer jobs (for *other* consumers), and then **directly invokes `interactionRecorder.HandleEvent(ctx, tx, env)`** in the same tx before commit. `CadenceUpdater.HandleEvent` runs next (also inline) because the manual handler wants to reflect cadence updates in the response. `FollowUpManager` runs async via river (the user doesn't block on Todoist's network call).

This gives the manual path synchronous `interaction` + `contact` cadence writes, while the Todoist round-trip (which can be slow) stays async.

**Mapping: which consumers run inline vs async for manual UI**

| Consumer | Manual UI path | Sync-provider path |
|---|---|---|
| `InteractionRecorder` | inline (same tx) | async (river) |
| `CadenceUpdater` | inline (same tx) | async (river) |
| `FollowUpManager` | async (river) — Todoist I/O | async (river) |
| `RematchDispatcher` | async (river) — not triggered by `interaction.manual` anyway | async (river) |

Consumer implementations live at `backend/internal/consumer/`:

- `consumer/interaction_recorder.go` — `InteractionRecorder` (struct + `HandleEvent` + river-worker wrapper)
- `consumer/cadence_updater.go` — `CadenceUpdater`
- `consumer/followup_manager.go` — `FollowUpManager`
- `consumer/rematch_dispatcher.go` — `RematchDispatcher`

**River-worker wrapper pattern (shared):**

```go
type interactionRecorderJobArgs struct {
    EventID uuid.UUID `json:"event_id"`
}
func (interactionRecorderJobArgs) Kind() string { return "interaction_recorder" }

type interactionRecorderWorker struct {
    river.WorkerDefaults[interactionRecorderJobArgs]
    bus     *events.Bus
    pool    *pgxpool.Pool
    handler *InteractionRecorder
}

func (w *interactionRecorderWorker) Work(
    ctx context.Context, j *river.Job[interactionRecorderJobArgs],
) error {
    env, err := w.bus.GetEvent(ctx, j.Args.EventID)
    if err != nil { return fmt.Errorf("fetch event: %w", err) }
    return pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
        return w.handler.HandleEvent(ctx, tx, env)
    })
}
```

#### 3.4.1 `InteractionRecorder`

**Input events:** `message.received`, `message.sent`, `calendar.attended`, `task.completed`, `task.outreach_detected`, `interaction.manual`.

**Output:** one `interaction` row per event (idempotent by `(source, source_ref)`), AND one `interaction.recorded` event emitted in the same transaction.

**Atomicity contract:**

```go
func (r *InteractionRecorder) HandleEvent(
    ctx context.Context, tx pgx.Tx, env Envelope,
) error {
    // 1. Resolve contact_id if unset; determine direction from Kind.
    // 2. Idempotent insert into `interaction` via FindBySourceRef/ExtendInteraction/
    //    PromoteInteractionToMutual (same semantics as today's RecordInteraction).
    //    If the interaction already exists (replay), return early with nil — no
    //    `interaction.recorded` emit, because the downstream consumers already
    //    ran on the first pass.
    // 3. Publish `interaction.recorded` event via bus.PublishTx(ctx, tx, ...) —
    //    same tx as the interaction insert. This atomically enqueues
    //    CadenceUpdater + FollowUpManager jobs. A crash between the insert
    //    and the publish is impossible; either both commit or both roll back.
    return nil
}
```

**Key property:** `interaction` insert and `interaction.recorded` event emission share one `pgx.Tx`. River's `InsertTx` ensures the downstream consumer jobs land atomically with the interaction row. This closes the atomicity hole called out in #265 at the application level (the Todoist provider's own atomicity fix is PR 11; this is the downstream-of-Recorder guarantee).

**Shadow mode:** when `EVENT_BUS_INTERACTION_MODE=shadow`, the worker performs the write AND the existing direct path continues to write. A sidecar table `event_shadow_observation` records what each path produced; a background diff job (or query) logs divergences. When `cutover`, shadow bookkeeping is skipped and direct-path writes are removed from provider code.

#### 3.4.2 `CadenceUpdater`

**Input events:** `interaction.recorded` — a new event emitted by `InteractionRecorder` after it writes an interaction row. This is a two-stage pipeline: provider event → `InteractionRecorder` → `interaction.recorded` event → `CadenceUpdater`.

(Alternative considered: have `CadenceUpdater` also consume the raw provider events. Rejected because it would duplicate the interaction-vs-no-interaction logic.)

**Output:** forward-only writes to `contact.last_contacted`, `contact.last_outreach_at`, `contact.last_response_at`, `contact.contact_by`.

**Forward-only SQL** (new sqlc query):

```sql
-- name: UpdateContactCadenceForward :exec
UPDATE contact SET
    last_contacted = CASE
        WHEN @apply_last_contacted::bool AND (last_contacted IS NULL OR @last_contacted::timestamptz > last_contacted)
        THEN @last_contacted
        ELSE last_contacted END,
    last_outreach_at = CASE
        WHEN @apply_last_outreach_at::bool AND (last_outreach_at IS NULL OR @last_outreach_at::timestamptz > last_outreach_at)
        THEN @last_outreach_at
        ELSE last_outreach_at END,
    last_response_at = CASE
        WHEN @apply_last_response_at::bool AND (last_response_at IS NULL OR @last_response_at::timestamptz > last_response_at)
        THEN @last_response_at
        ELSE last_response_at END,
    contact_by = CASE
        WHEN @apply_contact_by::bool AND (contact_by IS NULL OR @contact_by::timestamptz > contact_by)
        THEN @contact_by
        ELSE contact_by END,
    updated_at = NOW()
WHERE id = @contact_id AND deleted_at IS NULL;
```

**Direction semantics** (preserves existing rules from `applyInteractionEffects`):
- `outbound` → apply `last_outreach_at` only; no `contact_by` change.
- `inbound` → apply all four fields.
- `mutual` → apply all four fields.

**Manual source exception:** manual-source events update unconditionally (user correction). The forward-only guard is bypassed for `source='manual'` via a separate non-forward query. Manual writes still flow through `CadenceUpdater` — it remains the sole writer of the four cadence columns; the branching is inside the consumer.

**`MergeContacts` path:** contact-merge combines two contacts' cadence values and must go through `CadenceUpdater`. The mechanism is **`CadenceUpdater.BulkApply(ctx, tx, contactID, fields) error`** — a direct-invoke method on the consumer struct, called by `MergeContacts` within its existing `pgx.Tx`. `BulkApply` internally reuses the same forward-only and manual-unconditional code paths as `HandleEvent`. `MergeContacts` does NOT publish synthetic events; it calls the consumer method directly. This keeps `CadenceUpdater` as the single location of cadence-write SQL.

(An earlier draft considered publishing synthetic `interaction.manual` events per cadence field during merge. Rejected: it would require up to 4 synthetic events per merge, each triggering downstream FollowUpManager jobs, which is wrong semantically — merge is not a user interaction. `BulkApply` is the chosen path.)

#### 3.4.3 `FollowUpManager`

**Input events:** `interaction.recorded` (emitted by `InteractionRecorder`).

**Logic:**
- `outbound` → `CreateOrRefreshFollowUp` unless (a) backdated or (b) already-responded (see guards below).
- `inbound`/`mutual` → `CompleteFollowUp`.

**Three skip-guards applied to outbound:**

```go
func (m *FollowUpManager) shouldCreate(
    ctx context.Context, tx pgx.Tx, contactID uuid.UUID,
    outreachAt time.Time, cadenceDays int, isManual bool,
) (bool, error) {
    // Guard 1 (resolves #267): backdated outbound — stale-cosmetic skip.
    if !isManual {
        cutoff := time.Duration(cadenceDays) * 24 * time.Hour
        if accelerated.GetCurrentTime().Sub(outreachAt) > cutoff {
            return false, nil
        }
    }
    // Guard 2: out-of-order delivery — if a later response exists, skip.
    hasResp, err := m.interactionRepo.HasResponseAfter(
        ctx, tx, contactID, outreachAt,
    )
    if err != nil { return false, err }
    if hasResp { return false, nil }
    // Guard 3 (existing behavior): duplicate pending follow-up exists.
    pending, err := m.taskRepo.FindPendingFollowUp(ctx, tx, contactID)
    if err != nil { return false, err }
    if pending != nil { return false, nil }
    return true, nil
}
```

`HasResponseAfter` is a new sqlc query: `SELECT 1 FROM interaction WHERE contact_id = $1 AND direction IN ('inbound','mutual') AND occurred_at > $2 AND deleted_at IS NULL LIMIT 1`.

**Crash-safe creation (two-step + deterministic remote temp_id):**

The existing `CreateOrRefreshFollowUp` does remote Todoist `item_add` BEFORE the local `contact_task` insert. Under river retry semantics, a crash between those two operations duplicates the task on next attempt. Even with local-first ordering, a crash after Todoist succeeds but before local finalize can duplicate the remote Todoist task.

Redesigned with **three guarantees**:

1. **Local idempotency via unique index.** Step 1 inserts `contact_task` row with `state='pending_remote_create'`, `external_task_id=NULL`, deterministic `idempotency_key = hash(contact_id, outreach_at, kind='follow_up')`. A partial unique index on `(contact_id, kind, idempotency_key) WHERE deleted_at IS NULL` prevents duplicate local rows on river's job retry. Retry's second attempt to insert collides on the index and the job reads the existing row instead.

2. **Remote idempotency via Todoist temp_id.** Step 2's `TodoistFollowUpCreateJob` sends the Todoist Sync API `item_add` command with `temp_id = contact_task.id.String()`. Todoist's Sync API deduplicates on `temp_id` server-side: a second call with the same `temp_id` returns the same resource ID rather than creating a new task. See Todoist API docs on temp_id mapping. This closes the "remote-succeeded-but-local-failed" crash window.

3. **Atomic finalize.** On step 2 success, the worker updates `state='active'`, `external_task_id=<from response>` in a single `UPDATE` — no race window. Uniqueness on `(contact_id, kind, idempotency_key)` survives the state transition because the index is partial on `deleted_at IS NULL`, not on `state`.

On repeated failure after `MaxAttempts=10` exponential backoff, state stays `pending_remote_create`; an admin alert log fires; the task is still visible in the CRM (just not in Todoist).

**Interaction with response/completion flow:** `FindPendingFollowUp` (`contact_task.sql:137`) is updated to match `state IN ('pending_remote_create', 'active')` so that a response arriving while step 2 is in flight correctly cancels the pending row. `CompleteFollowUp` is extended to:
- If matched row has `state='active'` with `external_task_id` → close remote + set `state='completed'` (existing path).
- If matched row has `state='pending_remote_create'` → set `state='completed'` locally AND enqueue a `TodoistFollowUpCloseJob` that will attempt the close once the row eventually gets a remote ID (alternative: cancel step 2 before it runs by setting a sentinel state that step 2 checks on start). Simplest: mark `state='completed'`, and step 2, on start, checks state — if `completed`, creates remotely and immediately closes. The two-call cost is acceptable for the rare race.

**Migration** (PR 9 migration, `backend/migrations/04x_contact_task_followup_idempotency.up.sql`):
- Add `idempotency_key text` column to `contact_task`.
- Extend `contact_task.state` CHECK constraint to include `pending_remote_create` (current values include `managed`, `active`, `completed`, `archived` per migration 029; audit and restate the full list to avoid accidental omission).
- `CREATE UNIQUE INDEX idx_contact_task_idempotency ON contact_task (contact_id, kind, idempotency_key) WHERE deleted_at IS NULL AND idempotency_key IS NOT NULL;`
- Backfill `idempotency_key` for existing rows to NULL (they pre-date the new semantics — unaffected by the partial unique index).

**Close retries:** the current `todoist_close_pending` metadata flag is retired. A failed Todoist close is represented as a `TodoistFollowUpCloseJob` on river with `WorkerOpts{MaxAttempts: 10}` and exponential backoff. The `ListFollowUpsWithPendingClose` query and `RetryPendingCloses` method are removed.

**Inbound/mutual completion idempotency:** `CompleteFollowUp` already no-ops when no pending follow-up exists (`followup.go:100`) — this is correct under retry and remains unchanged.

#### 3.4.4 `RematchDispatcher`

**Input events:** `contact_methods.added` (plural). Publisher is `ContactService` (in `CreateContact` / `UpdateContact` method-diff branch) and `EnrichmentService` (in the import-link and Google-Contacts-sync paths). **One event per mutation**, carrying ALL newly-added methods in a single payload — not one event per method.

**Why bundled:** A mutation adds N methods atomically; the user expects one rematch job ID that covers all of them. One event per method (even with a shared job ID) would either force N river jobs (breaking the "one job per mutation" UX), or — under UniqueOpts dedup — collapse to one job that processes only one method (correctness bug).

**Event payload:**

```go
type ContactMethodsAddedPayload struct {
    Version      int                `json:"version"`
    ContactID    uuid.UUID          `json:"contact_id"`
    Methods      []ContactMethodRef `json:"methods"`      // all new methods
    RematchJobID uuid.UUID          `json:"rematch_job_id"`
}

type ContactMethodRef struct {
    Type  string `json:"type"`  // email|phone|telegram|...
    Value string `json:"value"` // normalized
}
```

**Publisher flow (ContactService.CreateContact / UpdateContact):**

```go
newMethods := s.diffNewMethods(existing, requested)
if len(newMethods) == 0 {
    return contact, uuid.Nil, nil
}
jobID := uuid.New()
env := events.Envelope{
    SourceID: jobID.String(), // idempotency on retry
    Kind: events.KindContactMethodsAdded,
    Payload: mustMarshal(ContactMethodsAddedPayload{
        Version: 1, ContactID: contactID, Methods: newMethods, RematchJobID: jobID,
    }),
    ObservedAt: accelerated.GetCurrentTime(),
}
if err := s.bus.PublishTx(ctx, tx, env); err != nil {
    return nil, uuid.Nil, err
}
// Register in-memory job synchronously so GET /rematch/jobs/:id works
// before the consumer has picked up.
s.rematchRegistry.RegisterPending(jobID, contactID, newMethods)
return contact, jobID, nil
```

`RegisterPending` creates the in-memory `RematchJob{Status: "pending"}` entry so `GET /rematch/jobs/:id` returns it immediately, even before the worker picks up.

**Consumer (`RematchDispatcher.HandleEvent`):**

Reads the event, looks up the `RematchJobID`, and runs the existing handler pipeline over the **full `Methods` slice** — `PeerMatcher.OnPeerLinked` for each Telegram method, calendar-rematch SQL for each email, etc. Updates the in-memory `RematchJob` entry as it progresses (per-method partial counts; final aggregate). This is the current `RematchService.run()` body (which already iterates over methods), lifted out of its goroutine and invoked from the consumer.

**River worker options:**

```go
river.InsertOpts{
    UniqueOpts: river.UniqueOpts{
        ByArgs:  []string{"ContactID", "RematchJobID"},
        ByState: []rivertype.JobState{
            rivertype.JobStateScheduled,
            rivertype.JobStateAvailable,
            rivertype.JobStateRunning,
            rivertype.JobStateRetryable,
        },
    },
    MaxAttempts: 3,
}
```

This dedupes repeated `contact_methods.added` events with the same `(contact_id, job_id)` — the `event.source_id = job_id.String()` unique index on the `event` table gives publisher-side idempotency; `UniqueOpts` is a belt-and-suspenders check at enqueue time. Distinct `RematchJobID`s produce distinct river jobs, which is correct (they represent separate mutations).

**Per-contact execution serialization — `contactLocks sync.Map` is kept.** River's `UniqueOpts` dedupes at enqueue; it does not serialize execution. If two mutations for the same contact produce two jobs with different `RematchJobID`s, both can run concurrently on separate workers. The existing `contactLocks` mutex in `RematchService` (per #182 spec requirement) serializes them at execution time, preventing interleaved writes to `matched_contact_ids`.

**Progress tracking:** `jobs sync.Map` in `RematchService` persists in-memory. On backend restart mid-job, river re-dispatches the job; the in-memory entry is recreated from the payload. If the process crashed between `RegisterPending` and event-processing, the `GET /rematch/jobs/:id` endpoint returns 404 for a brief window (the client's retry loop handles this).

**Setter injection removed:** `contactService.SetRematchService(rematchService)` is gone; `ContactService` holds only `events.Bus` and an optional `rematchRegistry` (a narrow interface with just `RegisterPending`). `RematchDispatcher` consumer holds the full `RematchService` reference. No cycle.

**Public API unchanged.** `POST /contacts`, `PUT /contacts/:id`, `POST /import/:id/link` still return `rematch_job_id` in their response. `GET /rematch/jobs/:id` still serves progress. Frontend `RematchJobsProvider`, `useRematchJob` untouched.

#### 3.4.5 Publisher Hooks (Summary)

| Source | Trigger function | Emits |
|---|---|---|
| Telegram | `aggregation.go:AggregateForContact` (inbound) | `message.received` |
| Telegram | `aggregation.go:AggregateForContact` (outbound) | `message.sent` |
| Calendar | `calendar.go:updateLastContactedForPastEvents` | `calendar.attended` |
| Todoist | `provider.go:handleTaskCompletion` (cadence/follow_up, outbound path) | `task.completed` |
| Todoist | `provider.go:handleTaskCompletion` (action) | `task.completed` (mutual variant) |
| Todoist | `provider.go:handleSkipTrigger` | `task.skipped` |
| Todoist | `provider.go:closeOnOutreach` | no event (purely cleanup; the originating source already emitted) |
| Manual UI | `contact.go:RecordInteraction` | `interaction.manual` |
| Contact edit | `contact.go:CreateContact`, `contact.go:UpdateContact` (diff), `enrichment.go` (import link paths) | `contact_methods.added` (one per mutation; payload carries `[]methods`) |
| External daemon | `POST /api/ingest/events` | per-batch, kind passed in payload |

### 3.5 HTTP Ingestion Endpoint

**`POST /api/v1/ingest/events`**

Request body (batched):

```json
{
  "events": [
    {
      "source": "imessage",
      "source_id": "guid-abc-123",
      "kind": "message.received",
      "payload": { "version": 1, "peer_ref": "imsg:+1-555-555-5555", "message_at": "2026-04-12T14:00:00Z" },
      "observed_at": "2026-04-12T14:00:00Z"
    }
  ]
}
```

Response:

```json
{ "accepted": 1, "duplicate": 0, "rejected": 0, "errors": [] }
```

**Constraints:**
- Auth: inside `v1.Use(auth.APIKeyMiddleware)` group. Standard `X-API-Key` or `Authorization: ApiKey` header.
- Max batch: 500 events per request.
- Payload validation: kind must be in known set; payload must unmarshal into the kind's typed struct. Unknown kinds → rejected.
- Duplicates (`(source, source_id)` already seen) → counted as `duplicate`, not failures.
- Transactional: the entire batch is one `pgx.Tx`; consumer jobs for all events are `InsertTx`-enqueued. All-or-nothing on unexpected errors; duplicates within a batch are silent no-ops via `ON CONFLICT DO NOTHING`.

### 3.6 Scheduler Migration

**Before:** `backend/internal/scheduler/scheduler.go` uses `robfig/cron` with a 5-min tick calling `SyncService.RunDueSyncs`. Per-account gating via `external_sync_state.status = 'syncing'`.

**After:**

```go
// river periodic job, configured in main.go
periodicJobs := []*river.PeriodicJob{
    river.NewPeriodicJob(
        river.PeriodicInterval(5*time.Minute),
        func() (river.JobArgs, *river.InsertOpts) {
            return SchedulerTickArgs{}, nil
        },
        &river.PeriodicJobOpts{RunOnStart: true},
    ),
}
```

`SchedulerTickWorker` enumerates due `external_sync_state` rows (via the existing `ListDueSyncStates`) and for each row enqueues a `SyncProviderAccountJob{Source, AccountID}` via a repository helper that performs an atomic per-account claim inside a single transaction:

```go
// Pseudocode for EnqueueAccountSyncIfNotInFlight(ctx, source, accountID):
//   BEGIN
//   SELECT pg_advisory_xact_lock(hashtextextended($1 || '|' || $2, 0));  -- per-account lock
//   SELECT 1 FROM river_job WHERE kind='sync_provider_account' AND state IN ('available','running','retryable','scheduled') AND args @> ...;
//   IF in-flight: return deduped-noop.
//   ELSE: riverClient.InsertTx(ctx, tx, SyncProviderAccountJobArgs{Source, AccountID}, nil);
//   COMMIT
```

The advisory lock + in-flight check + `InsertTx` run in one `pgx.Tx`; concurrent callers for the same `(source, account_id)` serialize on the advisory lock and see the prior insert when they look, so the dedup is race-free across concurrent tick workers or a tick colliding with `TriggerSync`.

**Why not `river.UniqueOpts{ByArgs: true}`?** River's `UniqueOpts` dedup window is intentionally short and limited to `available`/`running`/`retryable` states; it does not guarantee "no two in-flight for the same args" across all race windows we care about (crash-recovery + manual `TriggerSync` + 5-minute tick). Advisory-lock + explicit in-flight check is the simpler correctness story and keeps the invariant readable in one SQL transaction.

`SyncProviderAccountWorker` calls `provider.Sync(ctx, state)` — the existing provider interface unchanged. River's job lease + heartbeat + `JobRescuer` auto-resumes on crash.

**Orphan `external_sync_log` handling on crash/retry:** when a worker crashes mid-sync, its `external_sync_log` row is left `status='running'`. When river re-leases and the retry attempt begins, the worker marks any pre-existing `running` row for this `(source, account_id)` as `status='abandoned'` with `error_message='abandoned by retry; worker did not finish'` before inserting the new run's log row. This requires extending the `external_sync_log.status` CHECK constraint (migration 037) to allow `'abandoned'`.

**Retired:**
- `external_sync_state.status = 'syncing'` writes. Column kept for migration safety (reverting is easier than a down migration); scheduler no longer reads or writes it.
- `SyncStatusSyncing` constant usage. (Constant stays defined with a deprecation comment; no caller remains.)
- `UpdateSyncStateStatus(..., SyncStatusSyncing, ...)` call sites in `performSync`.
- `TriggerSync`'s "already syncing" hard-block — replaced with the advisory-lock-aware enqueue helper, which returns a deduped-noop when a job is already in-flight.

**Watchdog from #208:** not needed. Removing the mutex + adopting river's heartbeat/`JobRescuer` eliminates the bug class.

### 3.7 Main.go Wiring Changes

**Construction order (new):**

```go
// 1. Database, repos, auth (unchanged).
// 2. River client
riverClient, err := river.NewClient(riverdriver.NewPostgres(pool), &river.Config{ /*...*/ })

// 3. EventBus
eventBus := events.NewBus(pool, riverClient, eventRepo)

// 4. ContactService — no more setter injection for FollowUpManager / RematchService.
contactService := service.NewContactService(contactRepo, methodRepo, eventBus, ...)

// 5. Consumer workers
river.AddWorker(workers, consumer.NewInteractionRecorderWorker(contactRepo, interactionRepo, eventRepo, flags))
river.AddWorker(workers, consumer.NewCadenceUpdaterWorker(contactRepo, eventRepo, flags))
river.AddWorker(workers, consumer.NewFollowUpManagerWorker(followUpRepo, todoistClient, eventRepo, flags))
river.AddWorker(workers, consumer.NewRematchDispatcherWorker(rematchService, eventRepo))
river.AddWorker(workers, scheduler.NewSyncProviderAccountWorker(syncService))
river.AddWorker(workers, scheduler.NewSchedulerTickWorker(syncService, riverClient))

// 6. River Start — blocking until Stop
go riverClient.Start(ctx)
```

**Removed:**
- `contactService.SetRematchService(rematchService)` — `RematchDispatcher` consumer listens for `contact_methods.added`.
- `contactService.SetFollowUpManager(followUpService)` — `FollowUpManager` consumer listens for `interaction.recorded`.
- `todoistProvider.SetFollowUpCloser(followUpService)` — `TodoistCloseTaskJob` handles remote close retries.
- `scheduler.Start()` — replaced by river's own scheduler driven by `PeriodicJob`.

### 3.8 Database Migrations

Migration numbering continues from 035.

- **036** — `event` table (schema in §3.1).
- **River's own migrations** — applied via `river migrate-up` in `RunMigrations` after golang-migrate runs our migrations (not allocated a number in our sequence; River manages its own `river_migration` tracking table).
- **037** — extends `external_sync_log.status` CHECK to allow `'abandoned'` (used by the river-based scheduler to mark pre-crash `'running'` rows as abandoned when a retry attempt begins; see §3.6).
- **038** — `event_shadow_observation` table. Used only during shadow-mode PRs; dropped in a later PR once all consumers are in cutover.
  ```sql
  CREATE TABLE event_shadow_observation (
      id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
      event_id uuid REFERENCES event(id),
      observed_writes jsonb NOT NULL,
      observed_at timestamptz NOT NULL DEFAULT now()
  );
  CREATE INDEX ON event_shadow_observation (event_id);
  ```
- **039 (end of migration)** — drops `event_shadow_observation` and unused `SyncStatusSyncing` column values. Runs at the end of the full migration once cutover is complete.

### 3.9 Configuration

New env vars (all optional; defaults shown):

```
EVENT_BUS_INTERACTION_MODE=off      # off|shadow|cutover
EVENT_BUS_CADENCE_MODE=off          # off|shadow|cutover
EVENT_BUS_FOLLOWUP_MODE=off         # off|shadow|cutover
EVENT_BUS_REMATCH_MODE=off          # off|cutover (no shadow — no divergence risk, internal swap only)
EVENT_BUS_INGEST_ENABLED=false      # gates POST /api/v1/ingest/events
RIVER_WORKER_CONCURRENCY=10         # river default; override for single-user Pi
RIVER_LOG_LEVEL=info
```

Each consumer flag progresses through **three phases tied to PR lifecycle**:

| Phase | Flag value | Direct path | Consumer | Rollback |
|---|---|---|---|---|
| 1: shadow PR merged, pre-bake | `shadow` | active (writes) | active (logs only, no side effects) | flip to `off` at runtime — consumer disabled, direct path still writes |
| 2: shadow bake window | `shadow` | active | active (logs only) | same as phase 1 |
| 3: cutover PR merged | `cutover` | **removed from code** | active (sole writer) | `git revert` the cutover PR — flag flip alone won't restore behavior |
| 4: cleanup PR merged | flag gone | — | active (sole writer) | `git revert` cutover + cleanup PRs |

**Operational consequence:** runtime-flag rollback is a safety net for the shadow window only. Once the cutover PR merges and the direct path is deleted, the only rollback is a `git revert`. The spec acknowledges this trade-off explicitly — preserving forever-rollback would require keeping the direct path alive forever, defeating the point of the migration.

The `EVENT_BUS_*_MODE=off` value after cutover is effectively broken (disables the consumer without restoring the direct path); it's retained in the code only until the cleanup PR removes the flag entirely.

---

## 4. API Changes

### 4.1 New Endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | `/api/v1/ingest/events` | API-key | Accept batched events from external daemons |

Request/response shape: see §3.5.

### 4.2 Changed Endpoints

None. The existing `POST /api/v1/contacts/:id/interactions`, `PUT /api/v1/contacts/:id`, etc., retain their current request/response shape. The only change is that the write path runs through `InteractionRecorder`/`CadenceUpdater` consumers (hybrid model: still synchronous from the caller's perspective).

### 4.3 Removed Endpoints

None.

### 4.4 Frontend Contract

Unchanged. `RematchJobsProvider`, `useRematchJob`, `GET /rematch/jobs/:id` all keep current semantics.

---

## 5. PR Sequencing Plan

This section is the hand-off to future `/plan-and-ship` runs. Each PR below maps to one `.ai/log/plan/<name>.md` plan doc.

**Sequencing principle:** each PR is independently mergeable and behavior-preserving. Shadow-mode PRs land in production and bake for 24-72 hours before the cutover PR flips the flag and removes the legacy path.

**Dependency graph:**

```
PR 1 (river foundation)
  └── PR 2 (event table + EventBus)
        ├── PR 3 (scheduler → river periodic)
        ├── PR 4 (HTTP ingestion)
        └── PR 5 (InteractionRecorder shadow) — telegram/calendar/manual publishers start emitting
              └── PR 6 (InteractionRecorder cutover)
                    └── PR 7 (CadenceUpdater shadow)
                          └── PR 8 (CadenceUpdater cutover)
                                ├── PR 9a (FollowUpManager shadow + #267 logic)
                                │     └── PR 9b (FollowUpManager cutover)
                                └── PR 10 (RematchDispatcher event consumer)
                                      └── PR 11 (Todoist provider migration + #265 fix)
                                            └── PR 12 (cleanup: remove shadow table, retire status column, delete guardrail tests)
```

PR 9a / PR 9b can run in parallel with PR 10 (independent consumers).

### PR 1 — River foundation

**Scope:**
- Add `github.com/riverqueue/river` and `riverdriver/riverpgxv5` to `go.mod`.
- Apply river's own migrations via `river migrate-up` in `RunMigrations`.
- Wire `river.Client` startup in `main.go` (`Start`/`Stop` with lifecycle).
- Add a single trivial test worker to prove registration works (e.g., `NoopJobArgs`).
- Add `backend/internal/eventbus/` (skeleton package) and `backend/internal/consumer/` directories with READMEs only.

**Acceptance:**
- `make test` passes.
- `river_migration` table exists post-migration.
- `riverClient.Start(ctx)` runs without error.
- No behavior change for any existing feature.

**Est size:** Small. 1-2 days.

### PR 2 — `event` table + EventBus publish helper

**Scope:**
- Migration 036 (`event` table) and sqlc queries: `InsertEvent`, `GetEvent`, `FindEventBySource`.
- `backend/internal/repository/event.go`.
- `backend/internal/events/{kinds.go,bus.go}` — typed event kinds (all 10: 9 raw-signal + `interaction.recorded`), `Envelope`, `Bus.Publish`, `Bus.PublishTx`.
- `consumerJobsForKind` registry stub (returns empty slice for all kinds; actual consumers come later).
- Tests: event insert, `source_id` uniqueness, typed marshal/unmarshal per kind.

**Acceptance:**
- Events can be published; no consumers fire (registry empty).
- Unique `(source, source_id)` confirmed via integration test.

**Est size:** Small-medium. 2-3 days.

### PR 3 — Sync scheduler → river periodic job

**Scope:**
- `SchedulerTickJob` + `SyncProviderAccountJob` workers.
- Replace `robfig/cron` in `scheduler/scheduler.go` with river `PeriodicJob`.
- `EnqueueAccountSyncIfNotInFlight(ctx, source, accountID)` repository helper that does the atomic per-account claim (advisory lock + in-flight check + `InsertTx`) described in §3.6.
- Migration 037: extend `external_sync_log.status` CHECK to allow `'abandoned'`. Worker marks pre-crash `'running'` rows as `'abandoned'` on retry before inserting the new run's log row.
- Remove `UpdateSyncStateStatus(..., SyncStatusSyncing, ...)` calls; stop reading `status='syncing'` in `ListDueSyncStates`.
- Update `sync.go:performSync` to drop the mutex block.
- Replace `TriggerSync`'s "already syncing" hard-block with the enqueue helper's dedup-return.
- Deprecation comments on `SyncStatusSyncing` and `RunDueSyncs`.
- Tests:
    - `sync_worker_leased_retry_test.go` — cancel worker ctx mid-job, advance river's lease clock (short-lease test harness), assert job is re-dispatched and completes cleanly; no `status='syncing'` lingers; prior `'running'` log marked `'abandoned'`.
    - Load-style integration test — 50 goroutines calling the enqueue helper for the same `(source, account_id)` must see exactly one `river_job` inserted.
    - Realistic retry-budget load test — 10 simulated provider accounts, random failures injected, verify no double-runs or missed ticks.
- Closes #208.

**Acceptance:**
- Sync cadence observed unchanged in prod.
- Stuck `status='syncing'` rows no longer produced.
- Integration + load tests pass.
- **72h prod-Pi soak gate (single-user adaptation):** this is a single-user, Pi-deployed system with no separate staging tier. Soak runs directly on the prod Pi for ≥72h *before merging the PR* — deploy the feature branch via `git fetch && git checkout feat/event-bus-pr3-scheduler-river && systemctl restart personalcrm-backend`. Evidence attached to the PR body: zero stuck `'syncing'` rows, zero duplicate runs for the same `(source, account_id, window)`, sync result counts within ±5% of the pre-migration baseline (snapshot baseline before deploy via the same SQL queries against current `main`). **Rollback plan documented in the PR body:** `ssh raspberet "cd ~/PersonalCRM && git checkout main && systemctl restart personalcrm-backend"` if any anomaly appears during the window. Acceptable because (a) single-user system, (b) sync is read-mostly into the DB so no data loss risk, (c) River's `JobRescuer` + the automated test suite (50-goroutine race, retry-budget load, leased-retry integration) already cover the #208 bug class — the soak is belt-and-suspenders confidence, not primary verification.

**Est size:** Medium. 3-5 days (careful testing required; this is load-bearing) + 72h soak wall-clock.

**Risk area:** sync timing changes. Mitigated by the 50-goroutine race test, the retry-budget load test, and the 72h staging soak gate above.

### PR 4 — HTTP ingestion endpoint

**Scope:**
- `POST /api/v1/ingest/events` handler inside the v1 API-key group.
- Request validation (kind whitelist, payload typed unmarshal, batch size limit).
- Handler calls `Bus.PublishTx` in a single transaction.
- E2E test with valid / auth-failure / malformed / duplicate-in-batch fixtures.

**Acceptance:**
- Endpoint accepts batched events; duplicates counted.
- `EVENT_BUS_INGEST_ENABLED=false` (default) returns 404.

**Est size:** Small. 1-2 days.

### PR 5 — `InteractionRecorder` shadow mode

**Scope:**
- `consumer/interaction_recorder.go` with `HandleEvent(ctx, tx, env)` method + river-worker wrapper.
- `InteractionRecorder.HandleEvent` atomically (a) inserts the `interaction` row and (b) publishes `interaction.recorded` via `bus.PublishTx(ctx, tx, ...)` in the same tx. On replay (existing interaction found) it returns early without emitting — so CadenceUpdater/FollowUpManager are only triggered on the first successful insert.
- `consumerJobsForKind` registers the interaction-recorder river job for `message.received`, `message.sent`, `calendar.attended`, `task.completed`, `task.outreach_detected`, `interaction.manual`.
- Telegram / Calendar paths add `eventBus.Publish(...)` calls alongside their existing `contactService.RecordInteraction` calls.
- Manual UI handler uses the **direct-invoke path**: opens `pgx.Tx`, calls `bus.PublishTx` for the `interaction.manual` event, then calls `interactionRecorder.HandleEvent(ctx, tx, env)` inline in the same tx, commits. The response returns after the interaction is in DB.
- Shadow-mode bookkeeping: direct-path writes append to `event_shadow_observation`; consumer checks divergence and logs.
- Migration 038 (`event_shadow_observation` table).
- Config: `EVENT_BUS_INTERACTION_MODE=shadow` enabled via env for staging/prod bake.
- Todoist is intentionally NOT migrated here — that's PR 11.

**Acceptance:**
- `interaction` rows produced by the consumer match direct-path writes 1:1 over a 48-hour bake.
- Divergence log empty except documented edge cases.
- Manual `POST /contacts/:id/interactions` still returns synchronously with the interaction persisted (integration test asserts response-then-refetch sees the write).
- Replayed event (same `source_id`) produces no duplicate `interaction.recorded` emit.

**Est size:** Medium. 4-6 days.

### PR 6 — `InteractionRecorder` cutover

**Scope:**
- Flip `EVENT_BUS_INTERACTION_MODE=cutover` default.
- Remove the direct `contactService.RecordInteraction` call sites from Telegram aggregation and Calendar sync — they now only `Publish` the event; the async consumer handles the write.
- Manual UI handler retains the direct-invoke path (it already went through `HandleEvent` in PR 5 via inline call; now it no longer calls `contactService.RecordInteraction` at all — the consumer IS the write path).
- `applyInteractionEffects` still does cadence writes (CadenceUpdater cutover comes in PR 8). It now reads from the `interaction` row written by the consumer rather than from an in-flight function argument; refactored accordingly.
- Remove shadow-mode bookkeeping code from consumer + direct path (but not the `event_shadow_observation` table yet — PR 7 reuses it).

**Acceptance:**
- Grep: no `contactService.RecordInteraction` call outside `InteractionRecorder.HandleEvent` itself.
- All interaction writes flow through the consumer.
- Manual-write UX unchanged from user's perspective (integration test: POST interaction, GET contact, see the write).
- No divergence logged during the PR 5 bake window (verified before merging).

**Est size:** Small-medium. 2-3 days.

### PR 7 — `CadenceUpdater` shadow mode

**Scope:**
- Add the `interaction.recorded` event emitted by `InteractionRecorder` after write.
- `consumer/cadence_updater.go` with forward-only SQL.
- Shadow-mode: consumer computes what it WOULD write; `applyInteractionEffects` still does the actual write; divergence logged.
- Config: `EVENT_BUS_CADENCE_MODE=shadow`.

**Acceptance:**
- Shadow writes match direct writes 1:1 over 48-hour bake.
- Out-of-order test (synthetic) confirms the forward-only guard fires.

**Est size:** Medium. 3-5 days.

### PR 8 — `CadenceUpdater` cutover

**Scope:**
- `EVENT_BUS_CADENCE_MODE=cutover`.
- Remove cadence writes from `applyInteractionEffects` — moved into `CadenceUpdater.HandleEvent`.
- `MergeContacts` in `ContactService` is refactored to call `cadenceUpdater.BulkApply(ctx, tx, ...)` directly (direct-invoke path), NOT the sqlc queries.
- Manual correction (e.g., `UpdateContactLastContacted` service method) publishes `interaction.manual` events that flow through `CadenceUpdater` via the direct-invoke path — so manual correction uses the same consumer code. The non-forward (unconditional) SQL branch inside `CadenceUpdater` is taken based on `env.Source == "manual"`.
- The old sqlc queries (`UpdateContactLastContactedInbound`, `UpdateContactOutreachAt`, `UpdateContactMutualFields`, `UpdateContactResponseFields`) are now called ONLY from inside `consumer/cadence_updater.go` (as implementation details of `HandleEvent` / `BulkApply`). All service-layer and repository-layer call sites outside `cadence_updater.go` are removed.

**Acceptance:**
- `CadenceUpdater` is the sole writer of the four cadence columns. Verified by:
  - `sole_writer_static_test.go` AST walker fails the build if any non-`consumer/cadence_updater.go` file contains an `UPDATE contact SET last_contacted|last_outreach_at|last_response_at|contact_by` call.
  - `grep "UpdateContactLastContacted\|UpdateContactOutreachAt\|UpdateContactMutualFields\|UpdateContactResponseFields"` outside `consumer/cadence_updater.go` and its test file returns zero hits.
- Manual correction still works end-to-end (integration test: POST manual interaction at older timestamp → consumer applies non-forward; cadence columns reflect the correction).
- Merge still works end-to-end (integration test: merge two contacts with different `last_contacted` values → consumer applies forward-max rules).

**Est size:** Medium. 3-4 days (was 2-3; merge refactor adds surface).

### PR 9a — `FollowUpManager` shadow mode

**Scope:**
- `consumer/followup_manager.go` with `HandleEvent(ctx, tx, env)` implementing the three-guard skip logic + two-step creation.
- Migration `04x_contact_task_followup_idempotency.up.sql` adds `idempotency_key` column + `pending_remote_create` state + partial unique index.
- `interaction.recorded` consumer registered on river but, under `EVENT_BUS_FOLLOWUP_MODE=shadow`, the consumer computes what it WOULD do and writes to `event_shadow_observation` — it does NOT perform the two-step creation or call Todoist. The existing direct path in `service/followup.go` continues to run unchanged, producing the real side effects.
- The consumer's skip-guards (backdated / out-of-order / duplicate-pending) are evaluated in shadow mode and divergences vs. the direct path's decisions are logged.
- `TodoistFollowUpCloseJob` and `TodoistFollowUpCreateJob` river workers defined but invoked only in cutover mode.
- No `main.go` setter removals yet — setters stay for shadow period.

**Acceptance:**
- 48-hour bake shows shadow-observed create/skip decisions match the direct path 1:1 (modulo known deviations: the direct path creates follow-ups for backdated outbounds that the consumer would skip; these are the intentional #267 fix and are documented in the shadow report, not treated as failures).
- No new `contact_task` rows created by the consumer during shadow.
- No Todoist API calls from the consumer during shadow.

**Est size:** Medium. 4-5 days.

### PR 9b — `FollowUpManager` cutover

**Scope:**
- Flip `EVENT_BUS_FOLLOWUP_MODE=cutover`.
- Enable the real two-step creation in the consumer (`pending_remote_create` insert + `TodoistFollowUpCreateJob`).
- Remove `applyInteractionEffects` call to `followUpMgr.CreateOrRefreshFollowUp` and `followUpMgr.CompleteFollowUp` — now the consumer owns these.
- Remove `followUpService.RetryPendingCloses` and `ListFollowUpsWithPendingClose`.
- Remove `contactService.SetFollowUpManager` and `todoistProvider.SetFollowUpCloser` setter-injection in `main.go`.
- Closes #267.

**Acceptance:**
- Backdated-outbound test: 3-month-old outbound → interaction written, cadence advanced, no Todoist task created, no `contact_task` row.
- Out-of-order test: publish inbound at T, then outbound at T-1d → no follow-up created.
- Crash-recovery test (`followup_two_step_crash_test.go` + `followup_temp_id_dedup_test.go`) passes.
- Failed Todoist close retries via river; succeeds within 10 attempts or logs alert.
- Grep: `todoist_close_pending` metadata key is no longer written.
- Grep: `SetFollowUpManager`, `SetFollowUpCloser` calls gone from `main.go`.
- No shadow-mode divergences beyond the documented #267-related ones (audited from PR 9a's bake).

**Rollback plan:** consistent with §3.9 Phase 3 — runtime flag flip alone cannot restore follow-up behavior after PR 9b merges because the direct-path code is gone. If a prod regression surfaces post-cutover, `git revert PR 9b` is the rollback path; PR 9a's shadow mode can then be re-enabled to diagnose. This is accepted as the trade-off for removing `followUpService.RetryPendingCloses` + setter-injection; preserving runtime rollback would require keeping the old direct path indefinitely.

**Est size:** Medium. 2-3 days (mostly deletion + flag flip; logic landed in 9a).

### PR 10 — `RematchDispatcher` event consumer

**Scope:**
- Add `contact_methods.added` kind + `ContactMethodsAddedPayload` (plural; single event carries all new methods for a mutation).
- `ContactService.CreateContact` / `UpdateContact` (method-diff) and `EnrichmentService` (import-link + Google-Contacts-sync paths) publish ONE `contact_methods.added` event per mutation **via `PublishTx`** in the same `pgx.Tx` as the method row inserts. `event.source_id = rematch_job_id` provides idempotent publish.
- `ContactService` calls `rematchRegistry.RegisterPending(jobID, ...)` synchronously before returning, so `GET /rematch/jobs/:id` works immediately.
- `RematchDispatcher.HandleEvent` consumer dispatches to the existing rematch handlers (lifted out of `RematchService.run`).
- River `UniqueOpts{ByArgs: ["ContactID", "RematchJobID"]}` + kept `contactLocks` mutex for execution serialization.
- Remove `contactService.SetRematchService` setter from `main.go`. `ContactService` now takes `events.Bus` + `rematchRegistry` in constructor args.
- Remove the in-process `go s.run(j)` pipeline from `RematchService`; keep `GetJob`, `contactLocks`, `jobs sync.Map`, and helper methods.
- Frontend unchanged.

**Acceptance:**
- All existing rematch E2E tests pass unchanged.
- Back-to-back mutations for same contact with distinct methods run serially (contactLocks test).
- Double-published `contact_methods.added` for same `(contact_id, rematch_job_id)` dedupes to one job (UniqueOpts test). Also: `event.source_id` unique constraint prevents duplicate events on publisher retry.
- Worker killed mid-rematch: job re-dispatched on next heartbeat; in-memory `RematchJob` entry repopulates; final status reflects the completed re-run.
- `contactService.SetRematchService` call no longer present in `main.go`.

**Est size:** Medium. 4-5 days.

### PR 11 — Todoist provider migration + #265 fix

**Scope:**
- `handleTaskCompletion`, `handleSkipTrigger`, `tryRecoverPendingTempID` wrapped in `pgx.Tx`; state update + `eventBus.PublishTx` atomic.
- Remove `processItemResult.Unsafe` + `replayCommittedUnsafe` mitigation in `processItems`.
- Delete the two guardrail tests (`TestHandleTaskCompletion_StateUpdateFailureDoesNotReturnError`, `TestHandleSkipTrigger_FailureDoesNotReturnErrorAndSetsUnsafe`) — replaced by a real atomic-transaction test.
- `closeOnOutreach` keeps publishing no events (originating source already emitted).
- `todoistProvider.SetFollowUpCloser` removed (retry is now river-driven from PR 9).
- Closes #265.

**Acceptance:**
- Todoist sync produces identical `interaction`/cadence state under injected DB failures.
- The former "second skip ran twice on replay" scenario is now impossible (atomic commit).
- All Todoist E2E tests pass.

**Est size:** Medium-large. 5-7 days. Most complex provider to migrate; touches the state machine most carefully.

### PR 12 — Cleanup

**Scope:**
- Drop `event_shadow_observation` table (migration 039).
- Remove all shadow-mode feature flag references.
- Remove `external_sync_state.status='syncing'` column usage (optionally drop the CHECK value; column kept for down-migration safety).
- Delete dead helpers: `applyInteractionEffects` (if fully replaced), `RetryPendingCloses`, `contactLocks`, `scheduler.Start` (replaced by river).
- Update docs: `.ai/guides/architecture.md`, `.ai/patterns/sync.md`.

**Acceptance:**
- No dead code flagged by staticcheck.
- Architecture guide reflects new consumer topology.

**Est size:** Small. 1-2 days.

### Total estimate

12 substantive PRs (1, 2, 3, 4, 5, 6, 7, 8, 9a, 9b, 10, 11) + cleanup PR 12 ≈ **7-10 weeks of engineering effort**, spread across shadow-mode bake periods. Shadow-mode bakes can overlap with subsequent PR development if the flag-gating is clean. PRs 9a/9b and 10 are parallelizable against each other.

---

## 6. Testing Strategy

### 6.1 Unit Tests

- `events/bus_test.go` — `Publish`, `PublishTx`, duplicate handling, unknown-kind rejection, per-kind payload type assertion.
- `events/kinds_test.go` — marshal/unmarshal roundtrip for each kind; version field present.
- `consumer/interaction_recorder_test.go` — per-kind routing, idempotency on replay, shadow-mode divergence detection.
- `consumer/cadence_updater_test.go` — direction rules (outbound/inbound/mutual field masks), forward-only guard, manual-source bypass.
- `consumer/followup_manager_test.go` — backdated-outbound skip (non-manual vs manual), close on inbound, `TodoistCloseTaskJob` retry enqueue.
- `consumer/rematch_dispatcher_test.go` — uniqueness dedup, event → `RematchService.StartRematchForContact` wiring.

### 6.2 Integration Tests (`backend/tests/`)

**Event plumbing:**
- `event_idempotency_test.go` — insert same `(source, source_id)` twice via `POST /ingest/events`; assert one `event` row, one `interaction` row downstream.
- `event_out_of_order_cadence_test.go` — seed contact with `last_contacted = T-1d`; publish event `observed_at = T-7d`; assert `last_contacted` unchanged.
- `event_out_of_order_followup_test.go` — publish inbound at T for contact X; publish outbound at T-1d for contact X; assert no follow-up task created (out-of-order guard fires).
- `event_consumer_dispatch_test.go` — publish each kind; assert the expected consumer jobs land in river (query `river_job` for expected rows).
- `external_ingest_e2e_test.go` — POST valid batch, invalid auth, malformed payload, duplicate-in-batch, batch-too-large.

**Transactional atomicity:**
- `interaction_recorded_atomic_test.go` — inject DB error on `interaction` insert; assert `interaction.recorded` event is NOT in `event` table (tx rolled back) and no downstream consumer job is in `river_job`.
- `followup_two_step_crash_test.go` — insert `contact_task` in `pending_remote_create` state; simulate Todoist network failure on the remote create call repeatedly; assert only one `contact_task` row exists (unique index guards against duplicate); after success, row transitions to `active`.
- `followup_temp_id_dedup_test.go` — simulates "remote succeeded, local finalize failed, retry replays": (1) first invocation inserts `contact_task` with `state='pending_remote_create'`, then the fake Todoist server accepts the `item_add` with `temp_id=<contact_task.id>` and returns external ID `X`, but an injected DB error prevents the local `UPDATE` to `state='active'`. (2) River retries the job; the worker calls `item_add` again with the same `temp_id`; the fake Todoist server returns the same external ID `X` (temp_id mapping honored server-side). (3) Local `UPDATE` succeeds on the retry. **Assertions:** exactly one `contact_task` row with `state='active'`, `external_task_id='X'`; exactly one task in the fake Todoist server; no orphaned rows or tasks.
- `rematch_dispatcher_unique_opts_test.go` — publish two `contact_methods.added` events with same `(contact_id, rematch_job_id)` (simulating publisher retry); assert only one river job runs.
- `rematch_dispatcher_multi_method_test.go` — publish one `contact_methods.added` carrying 3 methods (email + phone + telegram); assert all three handlers run and all three are reflected in the final `RematchJob.matched` count.

**Crash / retry semantics (replaces SIGKILL approach):**
- `sync_worker_leased_retry_test.go` — start a `SyncProviderAccountJob` with a long-running provider mock; cancel the worker's context before completion; advance river's lease clock (via `riverClient.TestSubscribe` + manual `JobRetry`, or by setting a very short lease for the test). Assert: job is re-dispatched by river and completes cleanly on the second attempt. No `status='syncing'` lingers on `external_sync_state`.
- `followup_manager_retry_test.go` — register a `TodoistFollowUpCreateJob` whose worker returns an error on the first 2 attempts and success on the 3rd. Assert: river retries with backoff (`MaxAttempts=10`); end state shows `contact_task.state='active'` with `external_task_id` set; exactly one Todoist API call on the success path (test uses a call-counter mock).
- `river_periodic_scheduler_tick_test.go` — register the `SchedulerTickJob` periodic job with a 1s period under test time acceleration; assert it fires N times in a short test window; assert per-account `UniqueOpts` prevents double-runs even when ticks overlap.

**Backdated-outbound (resolves #267):**
- `backdated_followup_skip_test.go` — publish outbound event with `observed_at = now - 100d`, cadence = 30d, `!isManual`; assert interaction + cadence written, no `contact_task` follow-up row, no Todoist API call.
- `backdated_manual_outbound_creates_followup_test.go` — same event but `isManual=true`; assert follow-up IS created.

**Sole-writer enforcement (structural):**
- `sole_writer_static_test.go` — a `go test` that uses `go/ast` (or a scripted `rg` walk of sqlc call sites) to enforce:
  - `queries.InsertInteraction*` called only from `consumer/interaction_recorder.go`.
  - `queries.UpdateContactLastContacted*`, `queries.UpdateContactOutreachAt`, `queries.UpdateContactMutualFields`, `queries.UpdateContactResponseFields`, `queries.UpdateContactCadenceForward` called only from `consumer/cadence_updater.go`. **No merge-helper exemption** — `MergeContacts` goes through `CadenceUpdater.BulkApply`, which lives in the same file.
  - Fails the build if a new call site appears.
- Alternative (simpler): a lint/grep check baked into `make lint` that runs `rg` for the offending patterns outside the allowed files. Either works; the AST version is more robust.

**Shadow-mode divergence detection:**
- `shadow_mode_agreement_test.go` — in shadow mode, publish 50 synthetic events covering all kinds; assert `event_shadow_observation` has zero divergence rows at the end.
- `shadow_mode_detects_drift_test.go` — deliberately inject a divergence (e.g., test hook makes consumer write a different timestamp); assert divergence row is logged with expected fields.

### 6.3 E2E Tests (`frontend/tests/e2e/`)

- Existing rematch E2E tests must keep passing unchanged (verifies §3.4.4 API-preserving migration).
- Existing Telegram / Calendar / Todoist E2E coverage verified pre- and post-cutover (shadow mode ensures they pass during the transition).
- New test (step 4 PR): manual "Record Interaction" UI path still synchronous; user sees updated contact state on refetch.
- Add new E2E tags to `frontend/tests/e2e/test-map.json` if any new test files.

### 6.4 CI Gates

- `make lint` — staticcheck clean, no `golangci-lint` regressions.
- `make test` — unit + integration green.
- `make test-e2e` — full Playwright suite green before each cutover PR merges.
- Shadow-mode PRs must include 48-hour bake on staging before the cutover PR merges.

### 6.5 Load / Soak

- Scheduler migration (PR 3): 72-hour soak on staging with 4 provider accounts ticking every 5 min; verify no stuck `status='syncing'`, no duplicate runs, sync results unchanged.
- Event table growth: verify event-table row count growth is bounded; set up a retention job plan (out of scope here, but called out).

---

## 7. Risk Areas

| Risk | Mitigation |
|---|---|
| River migration incompat with app migrations | Separate migration track (`river migrate-up` is a distinct step); documented in `RunMigrations`. |
| Shadow-mode divergence swept under "known edge cases" | Each shadow-mode PR must include a sample of the divergence log in the PR description; any non-zero divergence requires explicit sign-off. |
| Out-of-order events break monotone-forward invariant | SQL-level `CASE WHEN ... > last_contacted` guard; integration test. |
| Hybrid sync/async contract confusion | Explicit doc in `.ai/guides/architecture.md` (PR 12): which writes are synchronous from caller's perspective. |
| River job args drift (rename/renumber) | River stores `kind` as a string; renames are handled via register-by-kind; add one worker at a time. |
| Cadence forward-only prevents legitimate manual corrections | Manual-source events bypass the guard via a separate query; tested. |
| `event_shadow_observation` table grows unboundedly during bake | Table is time-scoped to shadow-mode period; dropped in PR 12. Add size-check assert in PR 5's bake report. |
| Todoist migration (PR 11) lands behind other consumers | Intentional — it's the highest-risk state machine; later PRs inherit stable consumer infra. |
| Rematch UniqueOpts too aggressive (dedupes legitimate rematches) | `UniqueOpts.ByArgs = ["ContactID", "RematchJobID"]`. Distinct mutations have distinct `RematchJobID`s → distinct river jobs. Same-mutation publisher retries have the same `RematchJobID` and dedupe as intended. Tested via `rematch_dispatcher_unique_opts_test.go`. |

---

## 8. Open Questions & Future Work

### Open for reviewer input

1. **Event retention.** No retention policy in this spec. How long do we keep `event` rows? A year? Indefinite? (Single-user Pi — storage is not urgent.) Suggest: indefinite for now; add retention job when volume justifies.
2. **Observability.** River ships a Pro dashboard and there's a community OSS view. Do we want to set up a `/admin/river` view now, or defer? (Defer to a follow-up PR; logs + `SELECT FROM river_job` suffices for MVP.)
3. **Multiple consumer workers for the same kind.** Current design: one worker per kind per consumer. If throughput ever matters, scale via `river.Workers.ConcurrencyLimit`. Not an issue at single-user scale.
4. **Event schema governance.** When we add an event kind or bump payload version, what's the review process? Suggest: treat events like API contracts — new kinds require a spec entry; version bumps require a migration note.

### Future work (not in scope)

- **Retire the `rematch_job_id` return from mutation responses.** Once post-cutover, migrate the frontend to a "list pending rematch jobs for contact X" query (or SSE push). At that point, `ContactService` mutations no longer need to thread a jobID through the response envelope — events fully decouple. Requires a frontend rework to `RematchJobsProvider` + a new status API. Out of scope here to preserve the #182 investment.
- **Event replay CLI.** "Rerun consumer X over events Y..Z" is useful for backfills and debugging.
- **Soft-delete cleanup across `contact_method` / `external_identity` / `external_contact`.** Tracked separately; the event bus is the natural mechanism but is not the motivator.
- **LLM extraction pipeline.** `event` → extractor → structured claims. The event bus is the input side; the extractor is future work.
- **Knowledge graph** (`entity` / `relation` / `claim`). Same.
- **Admin UI for event / job inspection.** Defer.

---

## 9. Related Docs

- `.ai/spec/rematch-service.md` — #182; rematch service internals, preserved unchanged by this spec's PR 10.
- `.ai/log/plan/cadence-task-close-on-outreach.md` — cadence/task semantics this spec must preserve in `CadenceUpdater`.
- `.ai/log/plan/todoist-followup-deadline-regression.md` — `contact_by` forward-only semantics; `watchdog_days` per cadence.
- `.ai/log/plan/todoist-processitem-error-propagation.md` — the three non-atomic sites resolved in PR 11.
- `.ai/guides/architecture.md` — layered-architecture rules that consumer services must follow.
- Issue #180 — the originating issue with architecture sketch.
- Issue #208, #267, #265 — resolved by PRs 3, 9, 11 respectively.
