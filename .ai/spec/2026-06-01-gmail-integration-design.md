# Gmail Integration — Functional & Technical Specification

**Issue:** #70 (supersedes the issue body and revision comment, which are treated as broad overview only)
**Related:** #337 (GChat — co-designed for storage/matching reuse), AI inference over content (separate future thread)
**Status:** Draft v2 (incorporates Codex review pass)
**Last Updated:** 2026-06-01

---

## Table of Contents

1. [Overview](#1-overview)
2. [Functional Specification](#2-functional-specification)
3. [Fetch Strategy](#3-fetch-strategy)
4. [Direction & Aggregation Model](#4-direction--aggregation-model)
5. [Technical Specification](#5-technical-specification)
6. [Database Changes](#6-database-changes)
7. [Configuration](#7-configuration)
8. [Implementation Phases](#8-implementation-phases)
9. [Testing Strategy](#9-testing-strategy)
10. [Open Questions & Future Work](#10-open-questions--future-work)

---

## 1. Overview

### What

Add Gmail as a backend sync source that ingests **full email content** for emails exchanged with **known CRM contacts only**, records them as direction-aware interactions, and updates cadence timestamps. Gmail is **not** a discovery source — it never creates new contacts. AI inference over the stored content is explicitly out of scope and handled in a separate future thread; this feature only produces the durable content substrate plus interactions.

### Why

Email is a primary communication channel that currently leaves no trace in the CRM. Syncing it means `last_contacted` / `last_outreach_at` update automatically from real correspondence, contact timelines reflect email exchanges, and a faithful copy of message bodies is stored locally for later AI summarization/meeting-prep. Restricting to known contacts + excluding Gmail's bulk-mail categories keeps the cadence signal clean and avoids ingesting newsletters, receipts, and bulk mail.

### Key Decisions (from design session)

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Runtime | Pi backend sync provider (like `gcal`) | Reuses existing multi-account Google OAuth; `gmail.readonly` scope is already requested |
| Discovery | **None** — known contacts only, match-or-skip | Gmail is too noisy to be a contact source; user adds contacts elsewhere |
| Content scope | Full plaintext body (canonical) + retained HTML + attachment metadata | "Ingest full content"; substrate for later AI. Telegram/iMessage already store full bodies |
| UI | **None in this feature** | Store-only; emails appear on the timeline as interaction line-items like other sources |
| Category filter | Negative exclusions `-category:promotions -category:social -category:updates -category:forums` (SPAM/TRASH excluded by default) | Keeps cadence/`last_contacted` signal to real correspondence. A positive `category:primary` matches NOTHING on accounts where the Gmail category tabs are disabled (Primary is unpopulated there), so negative exclusions are used: they degrade gracefully (exclude nothing when categories are off, still drop bulk mail when on). The known-contact address filter is the real noise gate |
| Fetch strategy | Combined search query per account (`from:/to:/cc:/bcc:` OR-set, chunked by encoded byte length) | tens of list calls/sweep vs ~1,044 for per-contact, identical body-fetch cost; both fetch only known-contact mail |
| Incremental cursor | `after:<epoch-seconds>` per account, stored in `external_sync_state.sync_cursor` | Gmail `after:` supports second precision; overlap re-fetch absorbed by dedup |
| Cross-account dedup | RFC822 `Message-ID` header | Stable across all mailboxes; the Gmail per-mailbox `id` is not |
| Direction | Per-message inbound/outbound vs the "me" address set; interaction promoted to `mutual` on mixed thread-day | Reuses the existing `direction` model and `PromoteInteractionToMutual` |
| Aggregation | Deterministic `(contact, thread, local calendar-day)` key; consumer branches **create vs extend/promote** explicitly | Email has real threading; reuses `ExtendInteractionTx`/`PromoteInteractionToMutualTx`, no fuzzy sessionizer |
| Content storage | New **shared `comms_message`** table | Gmail uses it now; GChat reuses it. `telegram_message`/`messages_message` migrate onto it later (separate work) |
| Backfill | `GmailRematchHandler` on `contact_methods.added` (rematch becomes multi-handler-per-type) + onboarding scan from a fixed start date (default 2026-01-01) | Mirrors the messages "identifier-scoped backwards scan" and the Telegram fixed-since anchor; calendar already owns `email` rematch, so handlers must fan out |
| Cadence | Existing `CadenceUpdater` via `interaction.recorded` | inbound/mutual → `last_contacted`; outbound → `last_outreach_at` |
| Pipeline | Provider writes content + publishes `email.received`/`email.sent`; consumer derives interaction | Consistent with the event-bus SSOT used by messages/calls |

### Production scale (sizing, from the Pi)

451 active contacts; **261 with email**; **329 distinct email addresses**; **4 connected Google accounts**; max 6 emails on one contact. At this scale the combined-query approach is a handful of OR-chunks × 4 accounts in `messages.list` calls per sweep — negligible against the rate budget. Body fetches (`messages.get`, 20 units each) dominate; a per-account/per-sweep `seen` set (§3.1) de-duplicates messages returned by multiple OR-chunks so each message body is fetched once. This per-message cost is the same under per-contact or combined strategies — the same message set is fetched either way.

---

## 2. Functional Specification

### 2.1 User Stories

1. **Connect once** — having connected a Google account (already supported), email sync begins automatically for that account; no extra consent because `gmail.readonly` is already granted.
2. **Automatic cadence** — when I exchange non-bulk email with a known contact, `last_contacted` (inbound/mutual) or `last_outreach_at` (outbound) updates without manual logging.
3. **Email on the timeline** — email exchanges appear as interactions on the contact, aggregated per thread per day, with a direction signal.
4. **Follow-ups** — an outbound email to a contact participates in the existing outreach/follow-up lifecycle (a follow-up task is created/refreshed on outbound, same as Telegram/calls).
5. **No surprises** — emails from people not in my CRM are never ingested and never create contacts; bulk/promotional mail from known contacts is excluded.
6. **History on add** — when I add an email address to a contact, their recent email history (within the backfill window) is pulled in.

### 2.2 What Gets Synced

| Item | Synced | Details |
|------|--------|---------|
| Non-bulk mail to/from a known contact | Yes | The core target |
| Promotions / Social / Updates / Forums | No | Excluded via `-category:promotions -category:social -category:updates -category:forums` — but only effective when the account has Gmail category tabs enabled; the known-contact address filter is the primary noise gate |
| SPAM / TRASH | No | Excluded |
| Drafts | No | Not sent/received |
| Email involving zero known contacts | No | Query never returns it (server-side `from:/to:/cc:/bcc:` filter) |
| Group email where a known contact is only a co-recipient (not corresponding with me) | No | See participant rule in §4.1 |
| Mail in any connected account | Yes | All 4 accounts swept; cross-account duplicates collapsed by Message-ID |

**Per message stored (in `comms_message`):**

| Field | Stored | Notes |
|-------|--------|-------|
| Body (plaintext) | Yes | Canonical content; extracted from `text/plain`, else stripped from `text/html` |
| Body (HTML) | Yes (when present) | Retained in `source_metadata` for fidelity; not rendered in this feature |
| Subject | Yes | Top-level column (null for non-email sources) |
| Snippet | Yes | Gmail-provided preview |
| Participants (from / to / cc / bcc) | Yes | `source_metadata`; the matched contact's handle is also top-level |
| Sent timestamp | Yes | `sent_at` (Gmail `internalDate`) |
| Direction | Yes | Per-message inbound/outbound (§4.1) |
| RFC822 Message-ID | Yes | `external_id` — cross-account dedup key |
| Gmail thread id | Yes | `thread_id` — aggregation key component |
| Gmail per-account message id + account | Yes | `source_metadata` (provenance) |
| Attachment metadata | Yes | `source_metadata.attachments[]` = filename, mime, size; **no content downloaded** |
| Attachment/body binary content | No | Out of scope |

---

## 3. Fetch Strategy

### 3.1 Steady-state incremental (per account)

For each connected account, each sweep (default every 15 min via the existing scheduler tick):

1. Load the known-contact email map: `{ normalized_email → []contact_id }` for all email `contact_method`s of non-deleted contacts (new repository query — the framework's `contacts []Contact` slice is **not** hydrated with methods, so the provider loads its own map). The map value is a **slice** because addresses are only unique per `(contact_id, type, value_normalized)`, not globally — a shared address (e.g., a couple's joint inbox, or a data-entry collision) can belong to several contacts, and each must get its own interaction. Ambiguous-address counts are logged.
2. Build OR-chunks of `(from:<addr> OR to:<addr> OR cc:<addr> OR bcc:<addr>)` clauses — one group per address, covering every recipient dimension the participant rule uses (§4.1). Chunk by **encoded query byte length** with a conservative cap (≈6 KB, well under the practical ~8 KB GET-URL limit; no documented `q` length limit exists) rather than a fixed address count. Each chunk query is: `-category:promotions -category:social -category:updates -category:forums after:<cursor_epoch> ( <group> OR <group> ... )`.
3. Page through each chunk (`users.messages.list`, 5 units), keeping a **per-account, per-sweep `seen` set of Gmail message ids**. A message whose participants' addresses fall into different chunks is returned by more than one chunk query, so fetch `users.messages.get?format=full` (20 units) only for ids not yet in `seen` — this prevents redundant body fetches (and quota spend) for cross-chunk duplicates. Every returned message already involves a known contact, so nothing fetched is wasted on non-contacts.
4. For each message, resolve qualifying `(contact, direction)` participants (§4.1) against the in-memory map (**match-only — never create a contact**), fanning out to **all** contacts that share a matched address. Persist + publish per qualifying participant (§5.2).
5. Advance the account cursor to the max `internalDate` processed in the sweep. Re-fetch overlap on the next sweep is harmless — dedup on `(source, external_id, contact)` and on the event `(source, source_id)` make ingestion idempotent.

### 3.2 Onboarding backfill

When email sync is first enabled for an account (cursor empty), run the same chunked query with `after:<backfill_since>` (default **2026-01-01**, configurable in `external_sync_state.metadata`). Page to completion, then set the cursor to the latest processed `internalDate`.

### 3.3 New-contact / new-address backfill

Adding an email address to a contact already fires `KindContactMethodsAdded`, which today triggers `CalendarRematchHandler`. Register a parallel **`GmailRematchHandler`** for the `email` method type that runs an identifier-scoped historical query across all connected accounts: `-category:promotions -category:social -category:updates -category:forums after:<backfill_since> (from:<new addr> OR to:<new addr> OR cc:<new addr> OR bcc:<new addr>)`. This mirrors the messages integration's "30-day identifier-scoped backwards scan."

**Required refactor:** `RematchService` today stores exactly one handler per method type (`map[string]RematchHandler`; `Register` overwrites the slot), and `CalendarRematchHandler` already returns `IdentifierType() == "email"`. Registering Gmail naively would clobber calendar. Change `RematchService` to hold **multiple handlers per type** (`map[string][]RematchHandler`, `Register` appends, dispatch fans out to all), then register both calendar and Gmail for `email`. (Alternative considered and rejected: a composite email handler — fan-out is cleaner and extensible to future per-type handlers.) Steady-state account cursors are not rewound; this is a targeted one-shot scan for the new identifier.

### 3.4 Why not the alternatives

- **Per-contact queries** (issue revision comment): ~1,044 `messages.list` calls/sweep vs ~28; identical body-fetch cost; still needs the same backfill mechanism. Strictly dominated.
- **Account-wide History API**: fetches the entire noisy inbox to discard most of it, adds historyId-expiry (404 → full resync) handling, and the user explicitly wants to avoid ingesting all mail. Not used.

---

## 4. Direction & Aggregation Model

### 4.1 Participant & direction rule

Let `M` = the set of normalized addresses of all connected accounts (the `oauth_credential.account_id` values; extensible later with send-as aliases). For a message with sender `f` and recipient set `R` (To ∪ Cc ∪ Bcc), and a known contact `C` with address set `A_C`:

- **Inbound for C** iff `f ∈ A_C` **and** `M ∩ R ≠ ∅` — C wrote, I received.
- **Outbound for C** iff `f ∈ M` **and** `A_C ∩ R ≠ ∅` — I wrote, C received.
- Otherwise C does **not** qualify (e.g., a known contact merely co-CC'd by a third party on a thread that isn't to/from me). Bystanders are skipped.

The fetch query (§3.1) ORs `from:/to:/cc:/bcc:` for each address precisely so it returns every message this rule can qualify — the query's recipient dimensions and the rule's `R` are kept identical. (Bcc only surfaces on the user's own sent mail; it is included so an outbound Bcc to a known contact is not missed.) A single message can qualify multiple contacts (e.g., I email two known contacts → two outbound rows/interactions, the per-attendee analogue of calendar). It cannot be both inbound and outbound for the same C, since `f` is either mine or theirs.

### 4.2 Aggregation: thread + calendar-day

Interactions are keyed `source = 'email'`, `source_ref = "<contact_uuid>:<thread_id>:<local_day>"`, where `local_day` is the calendar day of `sent_at` in the server's `time.Local` (the same timezone `backend/internal/cadence/date.go` uses for date-only cadence math). Per-day is the starting cutoff — same thread on Tuesday and Wednesday are distinct interactions.

The consumer (§5.2) **must branch explicitly** — `RecordInteractionTx` is *not* a find-or-extend primitive. It calls `FindBySourceRefTx`, and when the key already exists it returns `IsReplay: true` *without* extending; the standard recorder also suppresses `interaction.recorded` on replay. The extend/promote primitives, by contrast, apply cadence *directly* and emit *no* `interaction.recorded`. So for each qualifying `(message, C, direction)` the consumer:

1. `FindBySourceRefTx(C, 'email', source_ref)`.
2. **Not found** → `RecordInteractionTx` (creates the row **and** publishes `interaction.recorded` → cadence/follow-up). `direction` = message direction, `occurred_at` = `sent_at`, `description` = subject.
3. **Found** → reuse the existing primitives, which apply cadence inline:
   - If the message direction differs from the interaction's stored direction → `PromoteInteractionToMutualTx`.
   - Else → `ExtendInteractionTx`.
   - **Forward-only timestamp guard:** `ExtendInteractionTx`/`PromoteInteractionToMutualTx` write the supplied timestamp *unconditionally* (no `max` in SQL), so the consumer passes `occurred_at` only when `sent_at` is later than the stored value; otherwise it still promotes/extends direction but leaves the timestamp. This keeps out-of-order backfill from moving `occurred_at` backward.

Cadence is therefore handled on *both* paths but by different mechanisms — `interaction.recorded` on create, the direct-apply primitives on extend/promote (see §4.3). Because the key is deterministic, ordering and replays converge to the same result, and we avoid the fuzzy claim/session machinery (`backend/internal/messaging/aggregation`) that Telegram and iMessage use — we reuse only its lower-level `extender`/`promoter` primitives, not its gap-based sessionizer.

### 4.3 Cadence

Cadence direction rules are unchanged: inbound/mutual bump `last_contacted` (+ `last_interaction_at`, `last_response_at`, `contact_by`); outbound bumps only `last_outreach_at`; follow-ups are managed by `FollowUpManager` on outbound. Two delivery paths (per §4.2): **create** publishes `interaction.recorded` → `CadenceUpdater`/`FollowUpManager` consume it; **extend/promote** apply cadence inline via `ExtendInteractionTx`/`PromoteInteractionToMutualTx` (these do not publish `interaction.recorded`). No new cadence logic — but tests must cover both paths because they reach cadence differently.

---

## 5. Technical Specification

### 5.1 Components

| Component | Location (new unless noted) | Responsibility |
|-----------|------------------------------|----------------|
| `GmailSyncProvider` | `backend/internal/google/gmail.go` | Implements `SyncProvider`; builds OR-chunks, fetches, resolves participants, writes content, publishes events |
| `GmailRematchHandler` | `backend/internal/google/gmail_rematch.go` | Identifier-scoped backfill on `contact_methods.added`. Requires `RematchService` to support multiple handlers per type (§3.3) |
| `RematchService` fan-out | `backend/internal/service/rematch.go` (modify) | `map[string][]RematchHandler`; `Register` appends; dispatch invokes all handlers for the type |
| `CommsMessageRepository` | `backend/internal/repository/comms_message.go` | CRUD + provenance-merging upsert for the shared content table; implements `StagingProcessor` (exact signature below) |
| Email identity query | `backend/internal/db/queries/contact_method.sql` (extend) | `ListEmailIdentitiesForSync` → rows of `(value_normalized, contact_id)` for all known email methods (many-to-one allowed) |
| `EmailInteractionConsumer` | `backend/internal/consumer/email_interaction.go` | Handles `email.received`/`email.sent`: explicit create-vs-extend/promote branch (§4.2), links `comms_message.interaction_id` via `MarkProcessedTx` |
| Event kinds | `backend/internal/events/kinds.go` (extend) | `KindEmailReceived`, `KindEmailSent` + lightweight payload |
| Body extraction | within provider | MIME walk: prefer `text/plain`; else strip `text/html`; collect attachment metadata |
| Registration | `backend/cmd/crm-api/main.go` (extend) | Register provider, consumer routing, rematch handler |

### 5.2 Data flow (per qualifying participant)

```
GmailSyncProvider.Sync(account)                      [per chunk, per message, per qualifying contact]
  └─ tx:                                              (publish-before-mutate ordering)
       bus.PublishTx(email.received | email.sent,
                     source_id = "<rfc822_message_id>:<contact_uuid>",
                     payload   = { external_id, contact_id, thread_id, local_day, sent_at, direction, subject })
       upsert comms_message ON CONFLICT (source, external_id, matched_contact_id)
              DO UPDATE: merge provenance by SET-UNION — add account_id to source_metadata.observed_accounts[]
                         only if absent, and record this account's gmail message id keyed by account
                         (content fields are immutable on conflict)
     commit
       └─ (replay from another account: PublishTx dedups on (source, source_id) → no consumer re-enqueued,
           but the upsert still set-union-MERGES provenance so every observing account is recorded;
           a same-account replay from cursor overlap is a no-op because the account is already present)

EmailInteractionConsumer.HandleEvent                 [river job]
  └─ tx:
       locate comms_message by (source='email', external_id, matched_contact_id=contact)    (natural key — no cross-tx id coupling)
       FindBySourceRefTx(contact, 'email', "<contact>:<thread>:<day>")
         ├─ not found → RecordInteractionTx           (creates + publishes interaction.recorded)   §4.2
         └─ found     → ExtendInteractionTx | PromoteInteractionToMutualTx  (apply cadence inline; forward-only ts)
       MarkProcessedTx(ctx, tx, []uuid.UUID{comms_message.id}, interactionID, sessionRef) (int64, error)
                                                       (exact StagingProcessor signature; registry dispatches by source)
     commit
```

Content lives only in `comms_message`; the event payload stays lightweight (ids + metadata), mirroring how messages/calls keep bodies in their staging tables rather than in the event log. Note the asymmetry: only the **create** branch publishes `interaction.recorded`; the extend/promote branch applies cadence inline and publishes nothing further.

### 5.3 Known-contact enforcement (defence in depth)

1. **Query-level**: the `from:/to:/cc:/bcc:` OR-set only ever returns mail involving a known address.
2. **Match-level**: participant resolution is a lookup in the known-contact map; unmatched addresses are ignored. No `MatchOrCreate` discovery path is used — consistent with the messages/calls "match-or-skip" rule.

### 5.4 Multi-account dedup

`external_id` is the RFC822 `Message-ID`. The same email observed in two accounts targets the same `comms_message` row (per contact) and the same event `source_id`, so the *interaction* is derived exactly once — but the upsert is **not** a no-op: it `DO UPDATE`s to merge provenance by **set-union**, adding the second account to `source_metadata.observed_accounts[]` only if not already present and recording that account's per-mailbox Gmail message id. The merge must be idempotent: cursor overlap (§3.1) re-runs the upsert for the *same* account on most sweeps, and a blind append would grow `observed_accounts[]` without bound. Content fields are immutable on conflict (first writer wins).

**Missing/malformed Message-ID fallback:** a small fraction of mail lacks a usable RFC822 `Message-ID`. When absent or unparseable, synthesize a deterministic `external_id` of `"nomsgid:<account_id>:<gmail_message_id>"`. Such a message will *not* cross-account dedup (it can't be correlated across mailboxes), which is acceptable — the worst case is one duplicate interaction for a header-less message, and the thread-day `source_ref` still collapses same-thread duplicates within an account.

---

## 6. Database Changes

### 6.1 New table: `comms_message` (shared content store)

| Column | Type | Null | Notes |
|--------|------|------|-------|
| `id` | UUID PK | no | |
| `source` | TEXT | no | `'email'` (later `'telegram'`, `'messages'`, `'gchat'`) |
| `external_id` | TEXT | no | Email: RFC822 Message-ID (cross-account stable); fallback `nomsgid:<account>:<gmail_id>` when absent (§5.4) |
| `thread_id` | TEXT | yes | Email: Gmail `threadId` |
| `subject` | TEXT | yes | Email subject; null for chat sources |
| `body` | TEXT | yes | Canonical plaintext content |
| `snippet` | TEXT | yes | Short preview |
| `peer_handle` | TEXT | yes | Raw address of the contact side |
| `peer_normalized` | TEXT | yes | Normalized (lowercased email) |
| `direction` | TEXT | no | CHECK (`inbound`,`outbound`) — per-message |
| `sent_at` | TIMESTAMPTZ | no | |
| `account_id` | TEXT | yes | Connected account that observed it (provenance) |
| `source_metadata` | JSONB | no, default `'{}'` | html body, labels, to/cc/bcc lists, attachments[], `observed_accounts[]`, per-account gmail ids |
| `matched_contact_id` | UUID FK contact | no | The known contact this row concerns (per-participant row) |
| `interaction_id` | UUID FK interaction | yes | Set when the consumer aggregates it |
| `claimed_at` | TIMESTAMPTZ | yes | Reserved for future Telegram/messages migration; unused by email |
| `claimed_session_ref` | TEXT | yes | Reserved (as above) |
| `processed_at` | TIMESTAMPTZ | yes | Set on aggregation |
| `deleted_at` | TIMESTAMPTZ | yes | Soft delete |
| `created_at` | TIMESTAMPTZ | no, default `NOW()` | |

Indexes / constraints:

- `UNIQUE (source, external_id, matched_contact_id) WHERE deleted_at IS NULL` — idempotent per-participant dedup, cross-account safe.
- `INDEX (matched_contact_id, sent_at DESC)` — content lookup per contact.
- `INDEX (source, thread_id)` — thread grouping.
- FKs cascade on contact/interaction delete consistent with existing staging tables.

Granularity: one row = **one message × one qualifying contact** — strictly per-message, never a thread/day bundle. The bundle lives at the interaction level: many messages in the same thread+day aggregate into one interaction (§4.2), but each message keeps its own row and full body (the right granularity for later AI use). A single email to N qualifying contacts stores N rows (body repeated); N is small for personal correspondence, and this keeps each row mapping to exactly one message + one contact, consistent with the existing per-source staging tables and avoiding a separate link table.

### 6.2 `interaction.source` CHECK constraint

Add `'email'` to the allowed `source` values (currently `manual, gcal, todoist, telegram, messages, anarlog_sessions, phone_calls`). This is **not migration-only**: add an `InteractionSourceEmail` constant in `backend/internal/repository/interaction.go` and update the source-check integration test (`backend/tests/interaction_source_descriptor_check_test.go`), which asserts the Go constants equal the live CHECK set, in the same change.

### 6.3 `external_sync_state`

No schema change. One row per `(source='email', account_id)`; `strategy = 'contact_driven'`; `sync_cursor` holds the epoch-seconds cursor; `metadata` holds `{ "backfill_since": "2026-01-01" }`.

### 6.4 Out of scope (future, separate migration)

Migrating `telegram_message` and `messages_message` onto `comms_message`. The `StagingProcessor` registry already lets the old and new tables coexist, so this feature does not touch them.

---

## 7. Configuration

| Setting | Default | Where |
|---------|---------|-------|
| Sync interval | 15 min | `SourceConfig.DefaultInterval` |
| Backfill start date | 2026-01-01 | `external_sync_state.metadata.backfill_since` |
| Category filter | `-category:promotions -category:social -category:updates -category:forums` | provider constant (negative exclusions; robust when Gmail category tabs are disabled) |
| OR-chunk budget | ≈6 KB encoded query | provider constant (byte-budgeted, not a fixed address count) |
| Aggregation day timezone | server `time.Local` | matches `backend/internal/cadence/date.go`; test DST/date-boundary behavior |
| Enablement | reconciliation creates/enables an `email` state per Google credential | **not** auto-provided by the scheduler — see below |

**Enablement is an explicit step.** The scheduler only enumerates already-enabled, due rows (`ListDueAccounts`) and the tick worker only enqueues those; nothing auto-creates a sync state. A reconciliation routine must create/enable a `(source='email', account_id)` `external_sync_state` for every existing Google credential **and** for newly connected ones — run it on boot and from the OAuth connect path (or extend the existing `TriggerSync`/state-creation flow). Without this, no email sync ever runs.

OAuth scope (`gmail.readonly`) is already requested; existing connected accounts may need a one-time re-consent to pick it up if granted before it was added — verify per account and surface a reconnect prompt only if missing.

---

## 8. Implementation Phases

1. **Schema + repository** — `comms_message` migration; `CommsMessageRepository` with the provenance-merging upsert (`ON CONFLICT DO UPDATE`) and the exact `StagingProcessor.MarkProcessedTx(ctx, tx, []uuid.UUID, interactionID, sessionRef) (int64, error)` impl + registry registration; `ListEmailIdentitiesForSync` returning `(value_normalized, contact_id)` pairs (many-to-one); `interaction.source` CHECK migration **plus** the `InteractionSourceEmail` constant and source-check test update; sqlc regen. Integration tests for upsert idempotency, provenance merge across accounts, and the source-check test.
2. **Provider (steady-state)** — `GmailSyncProvider`: byte-budgeted OR-chunk builder (`from:/to:/cc:/bcc:`), paging with a per-account/per-sweep `seen` Gmail-id set (skip duplicate `messages.get`), `messages.get?format=full` body/MIME extraction, participant/direction resolution against the `email → []contact_id` map (fan-out to all sharers), Message-ID extraction + fallback, set-union provenance-merging content upsert + event publish. Unit tests for chunk byte-budgeting, cross-chunk `seen` dedup, body extraction, participant rule (incl. cc/bcc + bystander), ambiguous shared address; integration test with a mock Gmail service.
3. **Consumer + aggregation** — `KindEmailReceived/Sent`; `EmailInteractionConsumer` with the **explicit create-vs-extend/promote branch** (§4.2), forward-only timestamp guard, `MarkProcessedTx` linking. Integration tests must cover both cadence paths (create→`interaction.recorded`; extend/promote→inline), mixed-direction promotion, out-of-order backfill not moving `occurred_at` backward, and cross-account single-derivation.
4. **Rematch fan-out + backfill** — refactor `RematchService` to `map[string][]RematchHandler` (append + dispatch-all) and keep calendar working; add `GmailRematchHandler` for `email`; onboarding empty-cursor window. Tests for fan-out (calendar + gmail both fire), new-address scan, onboarding.
5. **Registration + enablement reconciliation** — wire provider/consumer/rematch + event kinds (`AllKinds`, payload registry, `consumerJobsForKind`) in `main.go`; add the boot + OAuth-connect reconciliation that creates/enables an `email` `external_sync_state` per Google credential. End-to-end sync test against the mock.

Each phase is independently testable. The feature stays inert until phase 5 wires enablement, so phases 1–4 land without affecting production behavior.

---

## 9. Testing Strategy

- **Unit**: OR-chunk construction + byte-budget cap; MIME body extraction (`text/plain`, html-only, multipart, attachments); Message-ID extraction + fallback synthesis; participant/direction rule across inbound/outbound/cc/bcc/group/bystander cases; ambiguous shared address → fan-out to multiple contacts; `local_day` boundary in `time.Local` (incl. a DST transition).
- **Integration** (mock Gmail service): full sweep → content rows + interactions; thread-day aggregation (same day extend, next day new); **both cadence paths** — create publishes `interaction.recorded`, extend/promote applies cadence inline; mixed-direction → `mutual`; out-of-order backfill does not move `occurred_at` backward; cross-account same Message-ID → one interaction but set-union-merged `observed_accounts[]` provenance; same-account cursor-overlap replay does not grow `observed_accounts[]` (idempotent merge); a message spanning multiple OR-chunks is body-fetched once (per-sweep `seen` set); match-only (unknown participant never creates a contact); cursor advance + overlap idempotency; rematch fan-out (calendar + gmail both fire on `contact_methods.added`); onboarding + new-address backfill.
- **No E2E/UI** (store-only feature). Manual verification: connect a real account, confirm interactions and `last_contacted`/`last_outreach_at` updates, confirm bodies stored, confirm no contacts created.
- Follows repo testing rules: `accelerated.GetCurrentTime()`, sqlc-only queries (incl. test fixtures), `make test && make test-e2e-diff`.

---

## 10. Open Questions & Future Work

- **Cross-account threading**: `thread_id` is Gmail's per-account thread id. A thread split across two accounts would aggregate as two interactions. Rare; deferred. (Per-message dedup by Message-ID is unaffected.)
- **Send-as aliases**: the "me" set is the connected account addresses only in v1. If outbound from aliases is mis-detected, extend `M` with Gmail send-as settings later.
- **Aggregation cutoff**: per-day is the starting point; revisit if it proves too coarse/fine.
- **HTML retention**: stored but unused; the later AI thread decides whether it needs HTML or just plaintext.
- **Legacy table consolidation**: migrate `telegram_message` / `messages_message` onto `comms_message` as separate work.
- **GChat (#337)**: reuses `comms_message`, the known-contact map, match-or-skip, and the `<contact>:<thread>:<day>` aggregation pattern with its own `gchat` source and event kinds; OAuth gains the Chat scope.
- **AI inference**: consumes `comms_message.body` in a separate thread; no dependency taken here beyond storing the content.
