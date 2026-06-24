# Architecture Rationale and Design Decisions

Understanding why things are built the way they are.

---

## Overview

**Personal CRM** is designed as a single-user, local-first system optimized for personal use on constrained hardware (Raspberry Pi).

### System Architecture

```mermaid
graph TB
    subgraph Frontend["Frontend (Next.js 15)"]
        UI[React Components]
        RQ[React Query]
        API_CLIENT[API Client]
    end

    subgraph Backend["Backend (Go + Gin)"]
        HANDLERS[Handlers]
        SERVICES[Services]
        REPOS[Repositories]
        SCHEDULER[Scheduler<br/>river PeriodicJob]

        subgraph Sync["Sync Providers"]
            GCONTACTS[Google Contacts]
            GCAL[Google Calendar]
            TODOIST_SYNC[Todoist]
        end
    end

    subgraph Database["PostgreSQL 16"]
        SQLC[sqlc Queries]
        TABLES[(Tables)]
        PGVECTOR[pgvector]
    end

    subgraph External["External Services"]
        GOOGLE[Google APIs<br/>OAuth + Contacts + Calendar]
        TODOIST[Todoist API<br/>OAuth + Tasks]
    end

    UI --> RQ --> API_CLIENT
    API_CLIENT -->|HTTP| HANDLERS
    HANDLERS --> SERVICES
    SERVICES --> REPOS
    REPOS --> SQLC --> TABLES

    SCHEDULER -->|triggers| SERVICES

    SERVICES --> Sync
    GCONTACTS --> GOOGLE
    GCAL --> GOOGLE
    TODOIST_SYNC --> TODOIST
```

### Guiding Principles

1. **Single-user, desktop-first** — No multi-tenant complexity
2. **Local-first, cloud-optional** — Runs fully offline; internet enables AI calls & backups
3. **Opinionated minimalism** — Simplest tech that works, convention over configuration
4. **Incremental AI** — Ship rock-solid CRM first, layer AI capabilities once data is reliable
5. **Test-gate phases** — Each phase must pass tests before proceeding

---

## Layered Architecture

### Why Layered?

The backend follows a strict layered architecture to maintain separation of concerns and testability:

```mermaid
sequenceDiagram
    participant Client as Frontend
    participant H as Handler
    participant S as Service
    participant R as Repository
    participant Q as sqlc Queries
    participant DB as PostgreSQL

    Client->>H: POST /api/v1/contacts
    Note over H: Validate request<br/>Parse JSON

    H->>S: CreateContact(ctx, req)
    Note over S: Business logic<br/>Orchestration

    S->>R: CreateContact(ctx, params)
    Note over R: Convert to DB types<br/>(pgtype.Text, etc.)

    R->>Q: CreateContact(ctx, args)
    Note over Q: Type-safe SQL<br/>INSERT ... RETURNING *

    Q->>DB: Execute SQL
    DB-->>Q: Row data

    Q-->>R: db.Contact
    R-->>S: repository.Contact
    Note over R: Convert to domain types

    S-->>H: *Contact, nil
    H-->>Client: 201 Created + JSON
    Note over H: api.SendSuccess()
```

**Benefits:**
- **Testability:** Each layer can be tested independently with mocks
- **Maintainability:** Changes in one layer don't cascade to others
- **Clarity:** Clear responsibility boundaries
- **Performance:** No hidden N+1 queries from ORMs

**Trade-offs:**
- More boilerplate than using an ORM directly from handlers
- Requires discipline to not skip layers

---

## Technology Choices

### Backend: Go

**Why Go?**
- Fast compilation and runtime
- Single static binary (easy deployment to Pi)
- Excellent concurrency primitives (goroutines for scheduler)
- Great standard library
- Low memory footprint (~10-20MB base)

**Why NOT Node.js/Python?**
- Needs runtime installed
- Higher memory usage
- GC pauses more noticeable on Pi

### Database: PostgreSQL + pgvector

**Why PostgreSQL?**
- Mature, battle-tested RDBMS
- Excellent JSON support (for flexible data)
- UUID native support
- Full-text search built-in
- pgvector extension for embeddings (single DB solution)

**Why NOT SQLite?**
- Considered, but pgvector support is immature
- Full-text search less powerful
- Wanted experience that scales to server deployment

**Why pgvector over separate vector DB?**
- Simpler deployment (one DB instead of two)
- Easier backups (single pg_dump)
- Atomic transactions across relational + vector data
- Good enough performance for single-user use

**Database Tables:**

| Table | Purpose | Key Relations |
|-------|---------|---------------|
| **Core** | | |
| `contact` | People in CRM. ContactService dual-writes a `node(type='person')` at the contact's own id in the same tx on create — the `contact.id == node.id` invariant — and syncs `node.canonical_label` on rename | Parent of most entities; 1:1 person `node` (shared id) |
| `contact_method` | Email, phone, social handles | → contact |
| `note` | Freeform notes | → contact |
| `tag` | Contact categorization | ↔ contact (via contact_tag) |
| **Sync & Identity** | | |
| `external_sync_state` | Sync status per provider/account | |
| `external_sync_log` | Audit log of sync runs | → external_sync_state |
| `external_identity` | Maps external IDs to contacts | → contact |
| `external_contact` | Import candidates from Google/iCloud | → contact (optional) |
| `contact_enrichment` | Tracks field enrichment sources | → contact, external_contact |
| `oauth_credential` | OAuth tokens for Google/Todoist | |
| **Calendar** | | |
| `calendar_event` | Synced calendar events | |
| `calendar_event_attendee` | Event attendees | → calendar_event, contact |
| **Tasks** | | |
| `contact_task` | Todoist tasks linked to contacts | → contact |
| **Event Bus** | | |
| `event` | Append-only raw event log feeding the worker queue (spec §3.1) | (append-only; no FKs) |
| **Graph (SP1)** | | |
| `node` | Uniform registry every graph entity attaches to (person/entity/venue); caller-supplied id (person id == contact id, maintained by the ContactService dual-write + the `068_backfill_person_nodes` backfill); CHECK-enum `type`; `merged_into` self-FK merge alias; single `deleted_at` tombstone | self-FK (`merged_into`); person node shares the contact's id |
| `entity_type` | Per-TYPE entity-subtype catalog (`resolution_config` JSONB; curated/provisional status) | (catalog; no FKs) |
| `entity` | Structural subtype rows for organizations/places/topics/tags; per-instance `detail` JSONB; unique `(subtype, normalized_name)` | → node (PK/FK, ON DELETE CASCADE), → entity_type |
| `venue` | Structural subtype rows for shared interaction containers (email threads, group chats, DMs, meetings, calls, sessions); unique `(source, kind, source_container_id)` | → node (PK/FK, ON DELETE CASCADE) |
| `predicate` | Catalog of edge/fact types (subject/object/value typing, cardinality, symmetry, inverse pairing, temporal profile, review policy, valid-time dedup bucket); curated-core rows seeded, provisional minted at runtime; nullable `embedding vector(1536)` | self-FK (`inverse_predicate`) |
| `assertion` | Bi-temporal fact/edge row (valid-time + knowledge-time clocks); exactly-one-payload CHECK; write-API-computed `proposition_key` with a plain partial-unique live index; DEFERRABLE `superseded_by` self-FK; status state machine (proposed→accepted\|rejected, accepted→superseded\|retracted) | → node (subject + object, restrict), → predicate (restrict), self-FK (`superseded_by`, DEFERRABLE) |
| `assertion_provenance` | Corroborating source locators for an assertion; PK `(assertion_id, locator_hash)`; closed `source_kind` enum; polymorphic no-FK `source_id`; `(source_kind, source_id)` reverse-lookup index | → assertion (ON DELETE CASCADE); `source_id` polymorphic (no FK) |
| **Observability** | | |
| `sync_staleness_breach` | Open/resolved sync-staleness breaches recorded by the watchdog (partial unique index on open rows; no `updated_at`/`deleted_at`) | (system-derived; no FKs) |
| **Future/Unused** | | |
| `interaction` | Interaction logging (not yet used) | → contact |
| `connection` | Contact-to-contact relationships | → contact × 2 |
| `note_embedding` | Vector embeddings (future AI) | → note |
| `contact_summary` | AI summaries (future) | → contact |

Schema in `backend/migrations/`. Run `make sqlc` after SQL changes.

### Query Layer: sqlc (NOT an ORM)

**Why sqlc?**
- Write actual SQL (full PostgreSQL feature access)
- Compile-time type safety (catches errors before runtime)
- Zero runtime overhead (no reflection, no query building)
- Clear SQL → clear performance characteristics
- No hidden N+1 queries

**Why NOT GORM/Ent/Other ORMs?**
- ORMs abstract away SQL, making optimization harder
- Runtime overhead from reflection
- Magic behavior can hide performance issues
- sqlc gives control without sacrificing safety

**Trade-off:**
- More verbose (write SQL manually)
- Need to run `make sqlc` after SQL changes
- But: Know exactly what queries run, when

### Frontend: Next.js 15 + React 19

**Why Next.js?**
- Excellent DX (developer experience)
- App Router for modern React patterns
- Can do SSR, SSG, or SPA (flexible deployment)
- Great image optimization
- Built-in routing

**Why React Query (TanStack Query)?**
- Simple caching and refetching
- Loading/error states handled
- Optimistic updates
- Stale-while-revalidate
- No complex state management needed

**State Management via Centralized Invalidation:**
- Domain events trigger query invalidations
- Single source of truth for cross-domain effects
- See "Frontend State Management" section below

**Pi Deployment Consideration:**
- Next.js SSR mode uses ~100MB RAM
- Can export static site (~5MB) with nginx
- Decision deferred until Pi deployment

### Scheduler: river PeriodicJob

**Location:** `backend/internal/scheduler/` (tick_worker.go, sync_worker.go, args.go)

Historically the scheduler used `robfig/cron` to trigger
`syncService.RunDueSyncs()` every 5 minutes. As of #180 PR 3 the
scheduler runs on top of `github.com/riverqueue/river` periodic jobs.

**Current Jobs:**
| Job | Schedule | Description |
|-----|----------|-------------|
| `scheduler_tick` | Every 5 min (`RunOnStart: true`) | SchedulerTickWorker enumerates due `external_sync_state` rows and enqueues one `sync_provider_account` river job per account. |
| `sync_provider_account` | Dispatched on-demand | SyncProviderAccountWorker calls `syncService.RunAccountSync(source, accountID)` for a single account. River's lease + rescuer provides durable crash-recovery. |
| `sync_staleness_watchdog` | Every 5 min (`RunOnStart: true`) | StalenessWatchdogWorker compares per-source freshness timestamps (`external_sync_state` last-success/error + `mac_host.source_health` last-pushed + `mac_host.last_heartbeat_at`) against config-backed `SYNC_STALENESS_*` thresholds and reconciles breaches into `sync_staleness_breach`. Registered unconditionally (independent of `ENABLE_EXTERNAL_SYNC`). Read via `GET /api/v1/sync/staleness`. |
| `assertion_rollover` | Daily | AssertionRolloverWorker (SP1 graph) terminalizes bounded-with-pending-successor assertions whose `valid_to` has passed: rows matching `status='accepted' AND knowledge_to IS NULL AND superseded_by IS NOT NULL AND valid_to <= now` flip to `status='superseded'`, `closure_reason='superseded'`, `knowledge_to=now`, emitting `assertion.superseded` per row. Stateless catch-up sweep; never touches successor-less bounded-past facts. |

Dedup is done in a repository helper
(`EnqueueAccountSyncIfNotInFlight`) that wraps
`pg_advisory_xact_lock(hashtextextended(source||'|'||account_id, 0))` +
`CountInFlightSyncJobs` + `InsertTx` in a single transaction. Two
concurrent callers for the same account serialize on the advisory
lock, so the pre-check cannot race with a concurrent insert.

**Why river in-process?**
- Durable queue persisted in Postgres (survives crashes).
- Built-in `JobRescuer` rescues stuck `running` jobs after
  `RescueStuckJobsAfter` (default 1h), eliminating the old
  `status='syncing'` mutex + watchdog story (closes #208).
- Periodic jobs via `river.NewPeriodicJob` replace cron scheduling.
- Runs in same process as API — no external scheduler.

**Why NOT separate cron + systemd?**
- More moving parts.
- Harder to test.
- Time acceleration feature requires control.

---

## Key Design Decisions

### 1. Time Acceleration Feature

**Problem:** Testing reminders with real-world cadences (weekly, monthly) takes too long.

**Solution:** `accelerated.GetCurrentTime()` instead of `time.Now()`

```go
// Environment variables control time
TIME_ACCELERATION=1440  // 1 minute = 1 day
TIME_BASE=2024-01-01T00:00:00Z

// All code uses
now := accelerated.GetCurrentTime()
```

**Benefits:**
- Test weekly cadences in minutes
- Reproducible test scenarios
- Easy to switch environments (testing/staging/prod)

**Trade-off:**
- Must remember to NEVER use `time.Now()` directly
- Requires discipline

### 2. Soft Deletes

**Decision:** Use `deleted_at TIMESTAMPTZ` instead of hard deletes.

**Rationale:**
- Recover from accidental deletions
- Maintain referential integrity for analytics
- Audit trail

**Implementation:**
```sql
deleted_at TIMESTAMPTZ  -- NULL = not deleted

-- All queries filter
WHERE deleted_at IS NULL
```

**Trade-off:**
- Need to remember to filter in all queries
- Database grows (but negligible for single user)

### 3. UUID Primary Keys

**Decision:** Use UUIDs instead of auto-incrementing integers.

**Rationale:**
- No sequential ID enumeration (privacy)
- Client-side generation possible (offline-first)
- Easier data merging/syncing
- Industry standard for distributed systems

**Trade-off:**
- Slightly larger (16 bytes vs 4-8 bytes)
- Not human-readable
- But: negligible for single-user scale

### 4. Repository Pattern with Type Conversion

**Problem:** sqlc generates types with `pgtype.Text`, `pgtype.UUID`, etc. These are verbose to work with.

**Solution:** Repository layer converts to clean domain types.

```go
// sqlc generates this
type DbContact struct {
    ID    pgtype.UUID
    Email pgtype.Text  // nullable
}

// Repository converts to this
type Contact struct {
    ID    uuid.UUID
    Email *string  // nullable
}
```

**Benefits:**
- Cleaner code in services/handlers
- Hide database-specific types
- Easier testing (no pgtype in mocks)

**Trade-off:**
- Boilerplate conversion functions
- But: centralized in repository, reusable

### 5. Fork-First Contribution Strategy

**Decision:** Build features for personal use first, contribute back later.

**Rationale:**
- Faster iteration (no PR review cycles)
- Test in real-world use before sharing
- Can make breaking changes freely
- Pi-specific features stay in fork

**How It Works:**
1. Build and test on Pi
2. Identify generic improvements
3. Create clean branch from upstream
4. Cherry-pick or reimplement
5. Submit PR

### 6. Persisted contact_by for Overdue Calculation

**Problem:** Computing overdue contacts on-the-fly requires loading all contacts and filtering in memory, which doesn't scale and makes database-level sorting/filtering impossible.

**Solution:** Store `contact_by DATE` on the contact record.

**How it works:**
- `contact_by` = next date by which contact should be reached
- Calculated as `last_contacted + cadence_days` (or `created_at + cadence_days` for new contacts)
- Updated automatically on: create, update cadence, mark as contacted
- Overdue query: `WHERE contact_by < today`

**Why DATE not TIMESTAMP?**
- Cadences are day-based (weekly=7 days, monthly=30 days)
- Date-only comparison is simpler and timezone-safe
- Exception: Testing mode uses in-memory timestamp comparison for accelerated cadences

**UI Exposure:**
- `contact_by` is an **internal field** NOT displayed in the frontend
- Users see `last_contacted` and `cadence` instead
- Frontend receives overdue status via the overdue contacts API

**Trade-offs:**
- Must keep `contact_by` in sync on every write path
- But: Enables efficient database queries for overdue contacts

---

## Frontend State Management

### Problem: Cross-Domain Data Consistency

Some backend operations affect multiple data domains:

| Action | Backend Effect |
|--------|----------------|
| Mark contact as contacted | Updates contact AND completes auto-reminders |
| Delete contact | Soft-deletes contact AND its reminders |

If the frontend only invalidates contact queries, reminder data becomes stale.

### Solution: Centralized Invalidation Registry

Instead of scattering `queryClient.invalidateQueries()` calls across hooks, we use a centralized registry:

```
frontend/src/lib/
├── query-keys.ts         # Centralized query key definitions
└── query-invalidation.ts # Domain event → query key mapping
```

**Domain Events:**
```typescript
type DomainEvent =
  | 'contact:created'
  | 'contact:updated'
  | 'contact:deleted'
  | 'contact:touched'   // marked as contacted
  | 'reminder:created'
  | 'reminder:completed'
  | 'reminder:deleted'
```

**Invalidation Rules:**
```typescript
const invalidationRules = {
  'contact:touched': [
    contactKeys.lists(),
    contactKeys.overdue(),
    reminderKeys.all,  // Cross-domain: backend completes reminders
  ],
  'contact:deleted': [
    contactKeys.lists(),
    reminderKeys.all,  // Cross-domain: backend deletes reminders
  ],
  // ...
}
```

**Usage in Mutations:**
```typescript
onSuccess: (data) => {
  queryClient.setQueryData(contactKeys.detail(data.id), data)
  invalidateFor('contact:touched')  // Single call handles all effects
}
```

### Why This Approach?

**Considered alternatives:**

| Approach | Pros | Cons |
|----------|------|------|
| Direct invalidation in each hook | Simple | Easy to forget cross-domain effects |
| Server-Sent Events (SSE) | Real-time, server-driven | More infrastructure, Pi resource usage |
| Polling | Simple | Battery/CPU drain, not event-driven |

**Centralized registry chosen because:**
- Single source of truth for invalidation logic
- Cross-domain effects are explicit and auditable
- Easy to extend when adding new features
- No additional infrastructure (SSE/WebSocket)
- Works well with `refetchOnWindowFocus` for edge cases

**Trade-offs:**
- Slightly more indirection than direct calls
- Must keep registry in sync with backend behavior

**Full documentation:** See `docs/FRONTEND_STATE.md`

---

## Performance Optimizations for Pi

### Database Connection Pool

```go
config.MaxConns = 5                    // Pi doesn't need many
config.MinConns = 2                    // Keep 2 warm
config.MaxConnLifetime = 1 * time.Hour // Recycle connections
config.MaxConnIdleTime = 30 * time.Minute
```

**Rationale:**
- Pi has limited resources
- Single user = low concurrency
- Warm connections for responsiveness

### PostgreSQL Tuning

```sql
-- For Pi 4/5 with 4-8GB RAM
shared_buffers = 256MB
effective_cache_size = 1GB
maintenance_work_mem = 64MB
work_mem = 16MB
max_connections = 20
```

**Rationale:**
- Conservative memory usage
- Assume 2-4GB available for Postgres
- Leave room for Go backend + OS

---

## Security Model

### Single-User Assumptions

**No Authentication for Local Access:**
- Tailscale provides network-level auth
- Pi only accessible via Tailscale
- Simpler than managing auth tokens

**Future: API Key for Tailscale Access**
- Simple `X-API-Key` header
- Stored in environment variable
- Good enough for personal VPN

### Data Privacy

**Local-First Design:**
- All data on Pi (not cloud)
- LLM inference on Mac (not Pi, not cloud)
- Mac polls Pi for tasks
- Results written back to Pi

**Trade-offs:**
- LLM features only work when Mac online
- But: preserves privacy
- And: Pi stays 24/7 source of truth

---

## External Sync Architecture

### Overview

External sync connects the CRM to external data sources (Gmail, iMessage, Google Calendar, Google Contacts, etc.) to:
- Automatically update `last_contacted` when you communicate with contacts
- Track interactions across platforms
- Import contact data from external sources

See [Sync Data Flow Diagram](../patterns/sync.md#sync-data-flow-diagram) for the full Mermaid diagram.

### Two Sync Strategies

**Contact-Driven Sync (Gmail, iMessage, Calendar):**
- Query external sources only for known CRM contacts
- Avoids noise from spam, newsletters, unknown senders
- Sync provider passes `KnownContactID` to skip identity search

**Discovery Sync (Google Contacts, iCloud Contacts):**
- Fetch all data from external source
- Use identity matching to find CRM contact matches
- Surface unmatched entries for manual review/import

### Identity Matching System

Connects external identifiers (emails, phones, handles) to CRM contacts:

```
External Identifier → Normalize → Search/Cache → Match Result
     │                    │            │              │
     │                    │            │              └─ ContactID (or nil)
     │                    │            └─ external_identity table
     │                    └─ E.164 phones, lowercase emails
     └─ "John Doe <john@example.com>"
```

**Key Design Decisions:**

1. **Normalization first:** All identifiers normalized before matching (E.164 for phones, lowercase for emails)

2. **Cache in database:** `external_identity` table caches matches for O(1) subsequent lookups

3. **Two modes:**
   - `KnownContactID` set → Skip search, just record mapping (contact-driven)
   - `KnownContactID` nil → Search `contact_method` table (discovery)

4. **Unmatched review:** Identities without matches stored for manual linking via API

**Files:**
- `backend/internal/identity/normalize.go` — Normalization logic
- `backend/internal/service/identity.go` — Matching service
- `backend/internal/repository/identity.go` — Data access
- `backend/migrations/012_external_identity.up.sql` — Schema

**Full documentation:** See `docs/IDENTITY_MATCHING.md`

### Sync Provider Interface

All sync providers implement:

```go
type SyncProvider interface {
    Name() string
    Sync(ctx context.Context, state *SyncState) (*SyncResult, error)
}
```

Providers are registered with the sync service and can be triggered via API or scheduler.

**Provider Settings Storage:**

Provider-specific settings (e.g., Todoist project_id, label_id) are stored in `external_sync_state.metadata` JSONB column, not in separate settings tables. This keeps all provider state in one place and simplifies the schema.

```go
// Settings read/write via helper functions
settings := getSettingsFromMetadata(state.Metadata)
settings.ProjectID = "12345"
syncRepo.UpdateSyncStateMetadata(ctx, accountID, providerName, settingsToMetadata(settings))
```

**Provider Registration (main.go):**

New providers require three steps in main.go:
1. Initialize provider with dependencies
2. Register with `providerRegistry.Register()`
3. Add routes to router

See Google Calendar or Todoist providers as reference implementations.

### Mac Daemon push providers (Phase 1)

Phase 1 introduces a Mac-side daemon that pushes data to the Pi via a
new family of authenticated endpoints under `/api/v1/host/...`. The
Pi-side surface in PR1 is:

- `mac_host` table — one row per paired Mac (singleton index on
  non-revoked rows in v1).
- `mac_host_pairing_token` table — short-lived single-use tokens used
  during the bootstrap pair flow.
- `external_sync_state.strategy` extended with `'push'` (migration
  048). Push-strategy rows are skipped by the scheduler tick.
- `MacHostAuthMiddleware` — separate from the global API-key
  middleware. The daemon authenticates with
  `Authorization: Bearer <host-key>` + `X-Mac-Host-ID: <uuid>`.
- Three-stage transactional CAS (`SyncRepository.CommitMacHostCursor`)
  for cursor commits: epoch check, then insert-or-update, then
  re-read on race. Returns 409 with `current_cursor` / `current_epoch`
  on mismatch so the daemon can rebase.

Subsequent PRs add the Swift daemon, source readers (Messages, iCloud
Contacts), the aggregator extraction, and the new event kinds. See
`.ai/spec/mac-daemon.md` for the full phase spec.

---

## Event Bus and Consumers

### Overview

The event bus is an append-only `event` table (migration 036) plus a set of river-backed consumers that subscribe to specific `Kind` values and perform the authoritative domain writes. Publishers commit rows into `event` inside their caller's `pgx.Tx` via `events.Bus.PublishTx`; river workers dispatch per-kind consumer jobs after the commit lands.

Modes: `InteractionRecorder`, `CadenceUpdater`, and `FollowUpManager` each have an `EVENT_BUS_*_MODE` flag set to `cutover` (default) or `off`. `CadenceMode=off` and `FollowUpMode=off` require the paired `EVENT_BUS_{CADENCE,FOLLOWUP}_UNSAFE_ALLOW_OFF=true` safety gate because those consumers are the sole writers of their columns / tables. `InteractionMode=off` has no unsafe gate because publishers are not sole-writer-gated. `RematchDispatcher` has no mode flag; rollback is `git revert`. The historical `shadow` value is retired — the cutover posture is the only supported operating mode. See `backend/internal/config/config.go` for the validation rules.

### Consumer Topology

```mermaid
flowchart LR
    Publisher["Publisher<br/>(telegram, gcal, todoist,<br/>ManualInteractionHandler,<br/>HTTP /ingest/events)"]
    Publisher -->|events.Bus.PublishTx| EventTable["event table<br/>(append-only)"]
    EventTable -->|river job per kind| InteractionRecorder
    InteractionRecorder -->|interaction.recorded<br/>(V2 payload)| CadenceUpdater
    InteractionRecorder -->|interaction.recorded| FollowUpManager
    EventTable -->|contact_methods.added| RematchDispatcher
```

Concrete writer → kind mapping:

| Consumer | Subscribes to | Authoritative write |
|---|---|---|
| `InteractionRecorder` | `message.received`, `message.sent`, `calendar.attended`, `task.completed`, `task.outreach_detected`, `interaction.manual` | `interaction` row insert + emits `interaction.recorded` V2 |
| `CadenceUpdater` | `interaction.recorded` | `contact.last_contacted`, `last_interaction_at`, `last_outreach_at`, `last_response_at`, `contact_by` |
| `FollowUpManager` | `interaction.recorded` | `contact_task.kind='follow_up'` lifecycle (create / refresh / complete) + Todoist create/close/refresh river jobs |
| `RematchDispatcher` | `contact_methods.added` | Serializes per-contact rematch via `contactLocks` and runs `RematchService.Run` |

The InteractionRecorder invokes CadenceUpdater + FollowUpManager **inline** after `bus.PublishTx` so cadence + follow-up state apply synchronously in the caller's tx. The queued river workers for the same event become durable no-ops via `event_consumer_claim` when they eventually run. This pattern closes the queued-worker replay hole while keeping per-consumer retries available.

**SP1 graph assertion events.** `AssertService` emits five `assertion.*` kinds — `assertion.proposed`, `assertion.accepted`, `assertion.superseded`, `assertion.rejected`, `assertion.provenance_added` — onto the same bus via `PublishTx`, idempotency-keyed on `<assertion_id>:<transition>` (and `<assertion_id>:provenance:<locator_hash>` for the many-per-assertion provenance kind). They have **no consumer in SP1** (`consumerJobsForKind` returns nil for them); the change-feed, embedding, `relationship_signal`-recompute, projection-cache, and review-surface consumers are SP3/SP4. A `Retract` emits `assertion.superseded` (closure_reason distinguishes it; there is no separate `assertion.retracted` kind).

### Sole-Writer Map

Each consumer is the single source of truth for specific contact / contact_task columns post-cutover. Direct writes from anywhere else MUST be routed through the consumer's public API (e.g. `CadenceUpdater.ApplyInteraction` / `BulkApply` / `ApplyContactByOverride`). The sole-writer invariant is enforced by `scripts/ci/*-sole-writer-guard.sh` grep-based CI checks.

| Column / table | Sole writer | Notes |
|---|---|---|
| `contact.last_contacted` | `CadenceUpdater` | Forward-max on non-manual sources; unconditional on manual. |
| `contact.last_interaction_at` | `CadenceUpdater` | Same column set as `last_contacted` except merge does NOT bump. |
| `contact.last_outreach_at` | `CadenceUpdater` | Outbound / mutual directions only. |
| `contact.last_response_at` | `CadenceUpdater` | Inbound / mutual directions only. |
| `contact.contact_by` | `CadenceUpdater` | Recomputed from cadence string on interaction; user-override path via `ApplyContactByOverride`. |
| `contact_task` (kind='follow_up') lifecycle | `FollowUpManager` | `CreateContactTaskTx`, `UpdateContactTaskStateTx`, `UpdateContactTaskMetadataTx` are callable only via the manager's entry points. |
| `interaction` row insert | `InteractionRecorder` (inline via `ContactService.RecordInteractionTx`) | Non-bus wrappers route via `ContactService.RecordInteraction`. |

### Hybrid Sync / Async Contract

"Sync from caller" means the write is visible in the caller's tx before it returns. "Post-commit best-effort" means the local write is in-tx but a follow-up external call runs after tx commit with a river-backed retry fallback. "Async" means the caller returns before the domain write lands.

| Write | Sync from caller? | Notes |
|---|---|---|
| `interaction` row insert | **Yes** (in caller's tx) | Via `interactionRecorder` / `ContactService.RecordInteractionTx`. |
| `contact.last_contacted` / `last_outreach_at` / `last_response_at` / `contact_by` | **Yes** (in caller's tx) | Inline via `CadenceUpdater.HandleEvent` from the recorder's post-emit path (or `ApplyInteraction` for non-bus wrappers). |
| `contact_task` follow-up local state (create / refresh / complete) | **Yes** (in caller's tx) | `pending_remote_create` insert + `metadata['due_date']` refresh commit inline with the interaction. |
| Todoist `item_add` for new follow-ups | **Async** | Enqueued as `TodoistFollowUpCreateJob`; worker finalizes `external_task_id` on success. |
| Todoist `item_update` for follow-up refresh | **Post-commit best-effort, river fallback** | Closure returned from `FollowUpManager.HandleEvent` runs after `tx.Commit` and issues `item_update` directly; on failure enqueues `TodoistFollowUpRefreshJob`. Caller DOES wait for the post-commit closure on UI-initiated paths, so caller-visible latency includes best-effort Todoist RTT. |
| Todoist `item_close` for follow-up completion | **Post-commit best-effort, river fallback** | Same closure pattern as refresh; on failure enqueues `TodoistFollowUpCloseJob`. |
| Rematch contact-method reprocessing | **Async** | Enqueued via `RematchDispatcher`; caller sees `rematch_job_id` in the response envelope and polls `GET /rematch/jobs/:id`. |
| HTTP ingest (`POST /ingest/events`) | **Async** | The `event` row is inserted synchronously inside the request tx, but the consumer runs in river. The HTTP response returns after the insert, not after the consumer runs. |

**Operator takeaway:** "sync from caller" means the contact's cadence / task state is readable immediately after the caller returns. It does NOT mean the Todoist side of the world is in sync — that's best-effort with retry for refresh / close, and fully async for create. UI code handles this via React Query's stale-time refresh + follow-up-status polling.

### Mode Flags

Interaction, cadence, and follow-up consumers each have an `EVENT_BUS_*_MODE` flag that defaults to `cutover`. The alternate value is `off`. Sole-writer consumers (cadence, follow-up) require the paired `EVENT_BUS_{CADENCE,FOLLOWUP}_UNSAFE_ALLOW_OFF=true` safety gate — disabling them freezes the underlying columns / tables. Interaction mode has no unsafe gate: publishers are not sole-writer-gated, so silencing them is safe. `RematchDispatcher` has no mode flag. The historical `shadow` mode — used for per-event divergence observation during the cutover series — has been retired along with the `event_shadow_*` observation tables (migrations 038, 039, 042 dropped in 044). Rollback from a problem in cutover is `git revert`, not a runtime flag flip.

**Key files:**
- `backend/internal/events/bus.go` — publisher API (`PublishTx`, `GetEvent`).
- `backend/internal/consumer/` — all four consumers + river worker wrappers.
- `backend/internal/consumer/README.md` — per-consumer operator reference.
- `backend/internal/eventbus/README.md` — publisher + bus surface reference.

---

## Testing Philosophy

### Test Pyramid

```
         E2E (Playwright)
        - Full workflows
       - Slow, brittle
      - Run pre-push (diff-selected) and in CI (full)

       Integration Tests
      - DB + Repository
     - Postgres required
    - Run pre-push and in CI

      Unit Tests
     - Fast, isolated
    - Mock dependencies
   - Run constantly
```

**Rationale:**
- Most tests at unit level (fast feedback)
- Integration tests for critical paths
- E2E for happy path only

### Time-Accelerated Testing

Use `.env.example.testing` for fast cadence testing:
```bash
TIME_ACCELERATION=1440  # 1 min = 1 day
# Weekly cadence = 2 minutes
# Monthly cadence = 8 minutes
```

**Benefits:**
- Test reminder generation quickly
- Reproducible scenarios
- Find timing bugs

---

## Deployment Strategy

### Development (Laptop)

```bash
make dev
# Docker Compose (PostgreSQL)
# Go backend (hot reload)
# Next.js frontend (hot reload)
```

### Production (Pi)

**Systemd Services**
```ini
[Unit]
Description=Personal CRM Backend

[Service]
ExecStart=/home/pi/crm-api
Restart=always
```

---

## Migration Strategy

### golang-migrate

**Why NOT build migration runner in code?**
- Separation of concerns
- Can run migrations independently
- Standard tool

**Why ALSO auto-run on startup?**
- Convenience in development
- Prevents "forgot to migrate" errors
- Can disable in production if desired

### Migration Principles

1. **One logical change per file**
2. **Always provide down migration**
3. **Never modify after merge**
4. **Test both up and down**
5. **Add indexes in separate migration if large**

---

---

*For feature development process, see [`.ai/guides/feature-development.md`](./feature-development.md)*

*For common code patterns, see [`.ai/patterns/`](../patterns/)*

*For current development rules, see [`.ai/rules/core.md`](../rules/core.md)*
