# Telegram Integration — Functional & Technical Specification

**Issue:** #75
**Status:** Draft v3
**Last Updated:** 2026-04-06

---

## Table of Contents

1. [Overview](#1-overview)
2. [Functional Specification](#2-functional-specification)
3. [Cross-Cutting: Interaction Direction Model](#3-cross-cutting-interaction-direction-model)
4. [Cross-Cutting: Outreach + Follow-Up Lifecycle](#4-cross-cutting-outreach--follow-up-lifecycle)
5. [Technical Specification](#5-technical-specification)
6. [Database Migrations](#6-database-migrations)
7. [API Changes](#7-api-changes)
8. [Configuration](#8-configuration)
9. [Implementation Phases](#9-implementation-phases)
10. [Testing Strategy](#10-testing-strategy)
11. [Code Review Evals](#11-code-review-evals)
12. [UI Specifications](#12-ui-specifications)
13. [Open Questions & Future Work](#13-open-questions--future-work)

---

## 1. Overview

### What

Sync Telegram message history via the MTProto client API to track messaging interactions with CRM contacts. Uses `gotd/td` to authenticate as the user's own account and access all conversations.

### Why

Telegram is a primary communication channel. Unlike calendar events or task completions (inherently mutual), messaging introduces directionality: sending a message is outreach; receiving a reply is engagement. This integration both adds Telegram as a sync source AND introduces a cross-cutting interaction direction model that enriches the CRM's understanding of relationship health.

### Key Decisions (from design session)

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Runtime | Pi backend (sync provider) | MTProto works over internet; no Mac worker dependency |
| Content scope | Metadata + full message text + media references | Personal software, no privacy concern; ~100-250MB/yr storage |
| Groups | Small groups default-on, opt-in/opt-out | Track groups with ≤N members; user can ignore any group or opt-in to larger ones |
| Direction model | `direction` column on `interaction` table | Simplest schema change; applies to all sources |
| Aggregation | Asymmetric windows, messaging sources only | gcal/todoist are discrete events; messaging needs sessionization |
| Cadence model | Outreach tasks + Todoist follow-up | Completing outreach records outbound (doesn't reset cadence); creates follow-up task in Todoist |
| Backfill | Since configurable date (default 2026-01-01) | Captures all CRM-era history without over-fetching |
| Auth | Web UI in CRM settings | Matches OAuth provider UX pattern |
| Discovery | Unified import candidates | Telegram peers alongside Google/iCloud contacts |
| Connection | Long-lived MTProto connection | Lower overhead than polling, real-time updates, deletion/edit tracking, automatic reconnection |

---

## 2. Functional Specification

### 2.1 User Stories

**As a user, I want to...**

1. **Connect my Telegram account** via CRM settings (enter phone, receive code, enter code + optional 2FA password) so that message history syncs automatically.
2. **See Telegram interactions on contact detail pages** with direction signals so I know whether we're actually engaging or I'm doing all the talking.
3. **Have `last_contacted` update automatically** when I have a mutual or inbound exchange with a CRM contact on Telegram. Outbound-only messages do NOT reset `last_contacted` or `contact_by` — they record that I reached out, but the cadence clock only resets on actual engagement. I close the cadence task myself in Todoist when I feel I've done my part.
4. **Get follow-up reminders** when I reach out to someone (via Telegram, completing an outreach task, or any other outbound interaction) and they don't respond within a reasonable window. A follow-up Todoist task is created or refreshed on every outbound interaction.
5. **Discover new contacts** from Telegram — people I message regularly who aren't yet in my CRM appear as import candidates.
6. **See relationship health signals** — distinguish between contacts I'm actively engaging with vs. contacts where I'm doing all the talking.

### 2.2 What Gets Synced

| Content | Synced | Details |
|---------|--------|---------|
| Private chats (1:1) | Yes | Primary sync target |
| Group chats (≤N members) | Yes (default) | Only sender's `last_contacted` updated; N configurable (default 10) |
| Group chats (>N members) | No (default) | Skipped unless explicitly opted-in via settings |
| Supergroups | Yes | Treated same as groups (same size cutoff and opt-in/opt-out) |
| Channels | No | Broadcast-only, not interpersonal |
| Bot conversations | No | Not personal contacts |
| Saved Messages (self-chat) | No | Not interpersonal |
| Secret chats | No | Not accessible via MTProto client API |

**Per message:**

| Field | Stored | Notes |
|-------|--------|-------|
| Message text | Yes | Full text body |
| Timestamps | Yes | sent_at, edited_at |
| Direction | Yes | is_outgoing boolean |
| Sender identity | Yes | user_id, username, first/last name, phone |
| Chat context | Yes | chat_id, chat_type, chat_title (groups) |
| Reply-to reference | Yes | reply_to_msg_id for aggregation bridging |
| Media type | Yes | 'photo', 'voice', 'video', 'document', 'sticker' — type only, no file download |
| Media caption | Yes | Caption text if present |
| Message edits | Yes | Updated text + edited_at timestamp (real-time via UpdateEditMessage handler) |
| Message deletions | Yes | Soft-delete the `telegram_message` row via UpdateDeleteMessages handler; derived interaction unchanged (the exchange still happened) |

### 2.3 Message Aggregation (Sessionization)

Individual messages are rolled up into `interaction` records using asymmetric window rules. This only applies to messaging sources (Telegram, future iMessage/WhatsApp). gcal/todoist continue writing one interaction per event.

**Rules:**

1. **Burst window (2h):** Messages in the same direction within 2 hours of each other form a single burst. A gap > 2h starts a new burst.
2. **Reply bridge (48h):** If an inbound message (or burst) occurs within 48 hours after an outbound burst for the same contact, the outbound and inbound are merged into a single `interaction(direction=mutual)`.
3. **Explicit reply always bridges:** If a message has `reply_to_msg_id` pointing to a message in the most recent outbound burst (regardless of time gap), it bridges into a mutual interaction.
4. **Direction assignment:**
   - Burst contains only outbound messages → `interaction(direction=outbound)`
   - Burst contains only inbound messages → `interaction(direction=inbound)`
   - Merged outbound + inbound → `interaction(direction=mutual)`

**Session key and idempotency:** Each aggregated interaction gets a `source_ref` of the form `tg:{chat_id}:{first_msg_id}` where `chat_id` is the Telegram chat ID and `first_msg_id` is the Telegram message ID of the first message in the burst/session. This key is immutable — it never changes, even when a delayed reply promotes an outbound interaction to mutual. Promotion is done as an **in-place update** of the existing interaction row (changing `direction` and `occurred_at`), not a delete-and-recreate. Using chat_id + message_id rather than contact_id avoids requiring the contact to be matched at session-key creation time (matching happens after message storage). See [Section 5.5](#55-message-aggregation-engine) for full details.

**Example timeline:**

```
10:00  You → Alice: "Hey, how's the project going?"     ─┐
10:02  You → Alice: "I had some ideas about the API"      │ outbound burst
10:05  You → Alice: "Let me know when you're free"       ─┘
                                                            ↕ 48h reply window
11:30  Alice → You: "Hey! Yeah let's chat"               ─┐
11:32  Alice → You: "I'm free tomorrow afternoon"         ─┘ inbound burst (within 48h)
                                                          
Result: ONE interaction(direction=mutual, occurred_at=11:32, source_ref=tg:123456789:50001)
```

```
Monday 10:00  You → Bob: "Following up on last week"     → outbound burst
                                                           ↕ 48h reply window expires
Thursday 15:00  Bob → You: "Sorry, been slammed"         → separate inbound burst
                                                          
Result: TWO interactions
  1. interaction(direction=outbound, occurred_at=Mon 10:00)  — unreplied outreach
  2. interaction(direction=inbound, occurred_at=Thu 15:00)   — separate exchange
```

### 2.4 Contact Card Changes

The contact detail page gains new signals:

```
Alice Smith
  ● Monthly cadence — next touch in 12 days
  ↗ Last outreach: 3 days ago (Telegram)
  ↙ Last response: 3 days ago (Telegram)
  ⚠ Awaiting reply from Bob since Tue (5 days)     ← only shown when follow-up task pending
```

New derived fields on `contact`:
- `last_interaction_at` — last mutual OR inbound interaction (drives cadence clock)
- `last_outreach_at` — last outbound OR mutual interaction
- `last_response_at` — last inbound OR mutual interaction

### 2.5 Cadence, Outreach Tasks, and `last_contacted` Semantics

**This is the canonical definition.** The direction model changes which interactions reset the cadence clock:

| Field | Updated when | Drives |
|-------|-------------|--------|
| `last_contacted` | mutual OR inbound interaction only | `contact_by` calculation (cadence due date) |
| `last_interaction_at` | mutual OR inbound interaction | Display; future cadence migration target |
| `last_outreach_at` | outbound OR mutual interaction | Display |
| `last_response_at` | inbound OR mutual interaction | Display |

**Critical change:** `last_contacted` no longer updates on outbound-only interactions. This means:
- An outbound-only Telegram message does NOT reset `contact_by` (the cadence due date).
- But `contact_by` remains unchanged, so the contact will still appear as "due" if no mutual/inbound interaction follows.

**Outreach task model (replaces "cadence task" conceptually):**

The existing cadence Todoist tasks are reframed as **outreach tasks**. The task says "Reach out to Alice" — completing it means "I did my part." This is the full lifecycle:

```
1. Cadence overdue → Todoist outreach task: "Reach out to Alice"
2. User sends Alice a Telegram message
   → aggregation engine creates interaction(source=telegram, direction=outbound)
   → last_outreach_at updated, last_contacted NOT updated, contact_by NOT reset
   → Follow-up task CREATED in Todoist: "Follow up: Alice (awaiting reply)"
     due date = now + watchdog window (user can adjust in Todoist)
   → No new outreach task while follow-up is pending (grace period)
3. User also completes outreach task in Todoist ("I did my part")
   → interaction(source=todoist, direction=outbound)
   → Existing follow-up task REFRESHED (due date reset to now + window)

Path A: Alice responds (detected by Telegram sync)
→ mutual interaction recorded → last_contacted resets → contact_by recalculates
→ Follow-up task auto-completed in Todoist
→ New outreach task scheduled for next cadence period

Path B: Follow-up due date passes, no response
→ Task appears overdue in Todoist → user follows up or adjusts due date
→ If user completes follow-up → new outbound interaction, new follow-up created
→ Cycle continues until response or user removes cadence
```

**Key behavioral changes from current system:**
- Any outbound interaction (Telegram message, Todoist task completion, manual) creates or refreshes a follow-up Todoist task. This is centralized in `RecordInteraction`.
- Completing an outreach task in Todoist now records `direction=outbound` (previously: `direction=mutual` by default).
- This means completing the Todoist task does NOT reset the cadence clock.
- The user can adjust the follow-up due date directly in Todoist.
- No new outreach task is generated while a follow-up task exists for the same contact.

**Backward compatibility:** Existing callers (gcal, manual) default to `direction=mutual`, which updates `last_contacted` exactly as before. Zero behavioral change for non-messaging sources. The todoist provider's `handleTaskCompletion` changes from implicit `direction=mutual` to explicit `direction=outbound`.

### 2.5.1 Group Chat Management

Groups are managed via a `telegram_chat_config` table. Default behavior is size-based with per-chat overrides:

```
TELEGRAM_GROUP_MAX_MEMBERS=10 (configurable)

Per chat status: 'auto' | 'ignored' | 'tracked'

Logic:
  if status = 'ignored'  → always skip
  if status = 'tracked'  → always track (regardless of size)
  if status = 'auto' AND member_count <= max → track
  if status = 'auto' AND member_count > max  → skip
```

A settings UI page lists discovered group chats with member count and current status, letting the user toggle between auto/ignored/tracked.

**Member count updates:** `member_count` is refreshed when the chat is first discovered (via `getDialogs` during backfill) and updated whenever a `ChatParticipants` update is received via the long-lived connection. If a group grows past the threshold and its status is `auto`, it will stop being tracked. The settings UI shows the current member count so the user can override to `tracked` if desired.

### 2.6 Discovery Flow

Unmatched Telegram peers (people you message who aren't CRM contacts) surface as import candidates via the existing `external_contact` table and `GET /api/v1/imports/candidates` endpoint.

During sync, each unmatched peer is upserted as an `external_contact` row:

```
external_contact:
  source:       'telegram'
  source_id:    peer_user_id (as string)
  account_id:   NULL (single-account)
  display_name: peer_first_name + peer_last_name
  first_name:   peer_first_name
  last_name:    peer_last_name
  match_status: 'unmatched'
  metadata:     {"username": "@handle", "message_count": 42, "last_message_at": "...", "outbound_count": 20, "inbound_count": 22}
```

This plugs directly into the existing `ListImportCandidates` handler (`backend/internal/api/handlers/import.go`), which reads from `external_contact` and supports `source` filtering.

**Threshold:** Metadata `message_count >= 3` to appear. The handler already supports match_status filtering; Telegram peers appear with status `unmatched` alongside Google/iCloud contacts.

**Actions from candidates list:**
- Create contact (pre-fills name, adds Telegram username as contact method)
- Dismiss (`match_status='ignored'` — won't reappear)
- Ignore (leave for later — stays in list)

**Identity linkage:** When a peer is matched (either auto-matched or imported), an `external_identity` row is also created/updated with `identifier_type='telegram'` and `contact_id` set. This enables future syncs to match via the identity table without re-scanning `external_contact`.

### 2.7 Authentication Flow (Web UI)

Settings → Integrations → Telegram:

1. User enters phone number (international format)
2. Backend initiates MTProto auth → Telegram sends code to user's Telegram app
3. User enters the code in the CRM UI
4. If 2FA is enabled, user enters their 2FA password
5. Session established, encrypted, stored in DB
6. Status shows "Connected as @username" with disconnect option

**Auth state machine constraint (v1):** The MTProto auth flow is stateful — the `gotd/td` client holds challenge state (code hash, SRP state) in memory within its `Run` callback. In v1, auth is a **single-process, in-memory flow**:

- `POST /auth/start` creates a `telegram.Client`, calls `Run`, and holds it in a server-side `authSession` map keyed by a random token (returned to the frontend).
- `POST /auth/verify-code` and `/auth/verify-password` use the token to find the in-memory client and complete the flow.
- A **5-minute TTL** governs the auth session. If the user doesn't complete within 5 minutes, the in-memory client is closed and they must restart.
- If the backend process restarts mid-auth, the in-memory state is lost. The user simply restarts the auth flow — this is acceptable for a one-time setup operation.
- Only one auth flow can be active at a time (single-user CRM).

The `telegram_session.auth_state` column tracks the persistent state (`disconnected` | `connected`) — NOT the transient auth-in-progress states. Transient states exist only in memory.

The completed session persists across backend restarts. Re-authentication is only needed if the session is invalidated (e.g., user terminates session from Telegram settings, or Telegram forces re-auth).

---

## 3. Cross-Cutting: Interaction Direction Model

This is NOT Telegram-specific. It modifies the core `interaction` table and `contact` table to support directionality across all sources.

### 3.1 Interaction Table Changes

```sql
-- Add direction column
ALTER TABLE interaction ADD COLUMN direction TEXT NOT NULL DEFAULT 'mutual';
ALTER TABLE interaction ADD CONSTRAINT interaction_direction_check
    CHECK (direction IN ('outbound', 'inbound', 'mutual'));

-- Update source constraint to include telegram
ALTER TABLE interaction DROP CONSTRAINT interaction_source_check;
ALTER TABLE interaction ADD CONSTRAINT interaction_source_check
    CHECK (source IN ('manual', 'gcal', 'todoist', 'telegram'));
```

Existing rows (all gcal/todoist/manual) default to `direction='mutual'`, which is correct — meetings are mutual, task completions are mutual, and manual interactions are user-asserted.

### 3.2 Contact Table Changes

```sql
-- New denormalized timestamp columns
ALTER TABLE contact ADD COLUMN last_interaction_at TIMESTAMPTZ;
ALTER TABLE contact ADD COLUMN last_outreach_at TIMESTAMPTZ;
ALTER TABLE contact ADD COLUMN last_response_at TIMESTAMPTZ;

-- Backfill from existing last_contacted (all existing interactions are mutual)
UPDATE contact
SET last_interaction_at = last_contacted,
    last_outreach_at = last_contacted,
    last_response_at = last_contacted
WHERE last_contacted IS NOT NULL;
```

**Update semantics (canonical):**

| Interaction direction | Updates `last_contacted` | Updates `last_interaction_at` | Updates `last_outreach_at` | Updates `last_response_at` | Recalculates `contact_by` |
|----------------------|--------------------------|-------------------------------|---------------------------|---------------------------|---------------------------|
| `mutual` | Yes | Yes | Yes | Yes | Yes |
| `inbound` | Yes | Yes | No | Yes | Yes |
| `outbound` | **No** | No | Yes | No | **No** |

This is the key behavioral change: outbound-only interactions update `last_outreach_at` but do NOT touch `last_contacted` or `contact_by`. The cadence clock only resets on actual engagement.

### 3.3 RecordInteraction Changes

`RecordInteractionRequest` gains a `Direction` field:

```go
type RecordInteractionRequest struct {
    ContactID   uuid.UUID
    Source      string
    SourceRef   *string
    OccurredAt  time.Time
    Description *string
    Direction   string // "outbound", "inbound", "mutual" — defaults to "mutual" if empty
}
```

`ContactService.RecordInteraction` is updated to:
1. Default `Direction` to `"mutual"` if empty (backward compat).
2. Write direction to the interaction row.
3. **Conditionally update contact fields based on direction** (see table above).
4. Only recalculate `contact_by` when `last_contacted` actually changes.

Existing callers (gcal, todoist, manual) don't set Direction, so it defaults to "mutual" — zero behavioral change.

---

## 4. Cross-Cutting: Outreach + Follow-Up Lifecycle

### 4.1 Concept

On **any outbound interaction** (Telegram message, Todoist task completion, manual entry), the CRM:
1. Records an outbound interaction (does NOT reset cadence).
2. Creates or refreshes a **follow-up task in Todoist** with a due date based on the follow-up window.
3. Suppresses new outreach tasks while a follow-up is pending.

If the contact responds (detected by Telegram sync or other sources), the follow-up is auto-completed. If not, the follow-up appears overdue in Todoist, prompting the user to act.

### 4.2 Default Follow-Up Windows (by cadence)

| Cadence | Default Follow-Up Due | User Can Adjust? |
|---------|----------------------|-----------------|
| weekly | 3 days | Yes, via Todoist due date |
| biweekly | 5 days | Yes |
| monthly | 7 days | Yes |
| quarterly | 14 days | Yes |
| biannual | 21 days | Yes |
| annual | 21 days | Yes |
| (no cadence) | No follow-up | N/A |

The window is a default — the user can adjust the Todoist due date to any value after the follow-up is created. This gives per-outreach customizable intervals for free.

### 4.3 Mechanism

**Canonical rule: On ANY outbound interaction recorded (any source):**

This is the single trigger for follow-up tasks. It fires whether the outbound interaction comes from a Telegram message (via the aggregation engine), a completed Todoist outreach task, a completed follow-up task, or a manual interaction logged as outbound.

1. If contact has no cadence: do nothing.
2. If a pending follow-up task already exists (`kind='follow_up', state='managed'`): **refresh it** — update the Todoist due date to `now + watchdog_window` (the user reached out again, so the reply clock resets).
3. If no pending follow-up exists: **create one** in Todoist.
   - Content: "Follow up: {contact name} (awaiting reply)"
   - Due date: `now + watchdog_window` (from cadence defaults table above)
   - Stored as `contact_task(kind='follow_up', provider='todoist', external_task_id=todoist_task_id)`
4. No new outreach/cadence task is created while a `kind='follow_up'` task exists for this contact.

**Specific source behaviors:**

- **Telegram outbound message** → aggregation engine creates `interaction(direction=outbound)` → follow-up created/refreshed.
- **Todoist outreach task completed** → `handleTaskCompletion` records `interaction(source=todoist, direction=outbound)` → follow-up created/refreshed.
- **Todoist follow-up task completed** (user manually followed up) → `handleTaskCompletion` records `interaction(source=todoist, direction=outbound)` → new follow-up created (cycle continues).
- **Manual outbound interaction** → `RecordInteraction(direction=outbound)` → follow-up created/refreshed.

The user can break the follow-up cycle by: (a) removing the contact's cadence, (b) not completing the follow-up task, or (c) the contact responding (see below).

**On inbound/mutual interaction recorded (e.g., Telegram reply detected):**
1. Update `last_contacted`, `contact_by`, `last_interaction_at`, `last_response_at`.
2. Find any pending follow-up task for this contact (`kind='follow_up', state='managed'`).
3. If found: complete it in Todoist (via API) and mark `state='completed'` locally.
4. The cadence reconciliation loop will create a new outreach task for the next cadence period.

**Where the follow-up logic lives:** In `ContactService.RecordInteraction`, after creating the interaction and updating contact fields. To avoid coupling `ContactService` directly to the Todoist API, follow-up operations go through a `followUpManager` interface (same pattern as `interactionRecorder`):

```go
// Defined in repository or service package
type followUpManager interface {
    CreateOrRefreshFollowUp(ctx context.Context, contact repository.Contact, outreachAt time.Time) error
    CompleteFollowUp(ctx context.Context, contactID uuid.UUID) error
}
```

The Todoist provider implements this interface. `ContactService` receives it via constructor injection. If Todoist is not configured, a no-op implementation is used. This keeps `ContactService` decoupled from Todoist while centralizing the trigger rule — every caller (Telegram, Todoist, manual, future sources) gets follow-up behavior for free.

**Error isolation:** Follow-up creation/completion failures must not cause `RecordInteraction` to fail. The `followUpManager` calls are best-effort with error logging. The interaction itself is always recorded regardless.

### 4.4 Follow-Up Tasks in Todoist

Follow-up tasks are **first-class Todoist tasks**, not internal-only. They use the existing `contact_task` table and Todoist sync infrastructure.

**Schema compatibility:** The `contact_task` table already supports this with no migration:
- `kind='follow_up'` — `kind` is TEXT without a CHECK constraint. The partial unique index from migration 029 only enforces uniqueness for `kind='cadence'`. Multiple follow-up tasks per contact are already allowed.
- `external_task_id` — follow-up tasks get a real Todoist task ID, so the NOT NULL constraint is satisfied.
- `provider='todoist'` — same provider as outreach tasks.

**Handler change:** `ListContactTasksQuery.Kind` validator must add `follow_up` to `oneof=action cadence follow_up`.

**`handleTaskCompletion` changes (critical):** The current `handleTaskCompletion` independently recalculates `contact_by` and creates the next cadence task *after* calling `RecordInteraction` (lines 447-465 in `todoist/provider.go`). This must be gated on task kind:

- `kind='cadence'` (outreach task completed): record `direction=outbound` via `RecordInteraction` (which skips `contact_by`). Do NOT recalculate `contact_by` or create next cadence task. Instead, create a follow-up task. The existing `contact_by` recalculation and next-task creation in `handleTaskCompletion` must be removed/skipped for this path.
- `kind='follow_up'` (follow-up task completed): record `direction=outbound` via `RecordInteraction`. Create a new follow-up task (cycle continues). Do NOT recalculate `contact_by` or create a cadence task.
- `kind='action'` (action task completed): unchanged (mark completed, no new task).

**sqlc query additions:**
- `FindPendingFollowUp`: find `kind='follow_up', state='managed'` for a given contact.
- `CompleteFollowUpForContact`: mark follow-up tasks as `state='completed'` when a response arrives.

**Todoist task creation (in handleTaskCompletion):**

```go
// After recording outbound interaction from completed outreach task:
if contact.Cadence != "" {
    followUpContent := fmt.Sprintf("Follow up: %s (awaiting reply)", contact.DisplayName())
    dueDate := accelerated.GetCurrentTime().Add(watchdogWindowForCadence(contact.Cadence))

    // Create in Todoist via existing API
    todoistTask, err := p.todoistClient.CreateTask(ctx, followUpContent, dueDate, projectID)

    // Store locally
    p.contactTaskRepo.Create(ctx, repository.ContactTask{
        ContactID:      contact.ID,
        Provider:       "todoist",
        Kind:           "follow_up",
        ExternalTaskID: todoistTask.ID,
        State:          "managed",
        Metadata:       map[string]any{"outreach_at": now.Format(time.RFC3339)},
    })
}
```

### 4.5 Task Reconciliation Changes

The existing `reconcileContactTasks` logic must be updated:
- **Skip contacts with a pending follow-up task** (`kind='follow_up', state='managed'`). These contacts are in a grace period — reached out to but no response yet.
- This prevents: outreach completed → outbound doesn't reset cadence → contact still "due" → new outreach task created immediately (loop).

---

## 5. Technical Specification

### 5.1 Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  Pi Backend                                                  │
│                                                              │
│  Long-lived MTProto Connection (gotd/td)                    │
│      │  Starts on backend boot if Telegram is connected     │
│      │  Auto-reconnects with exponential backoff             │
│      │  Ping keepalive every 60s (~60 bytes)                │
│      │                                                       │
│      ├─► Update Handlers (real-time)                        │
│      │     ├─ OnNewMessage → store + match + aggregate      │
│      │     ├─ OnEditMessage → update telegram_message text  │
│      │     └─ OnDeleteMessages → soft-delete rows           │
│      │                                                       │
│      ├─► Initial Sync (on connect / reconnect)              │
│      │     ├─ GetDialogs (conversations list)               │
│      │     ├─ GetHistory (messages per chat, since cursor)  │
│      │     └─ Gap recovery via pts/qts state                │
│      │                                                       │
│      ├─► Message Processing                                 │
│      │     ├─ Store raw messages (telegram_message)         │
│      │     ├─ Match peers → external_identity + external_contact│
│      │     └─ Aggregate into interactions (sessionization)  │
│      │                                                       │
│      └─► Contact Updates                                    │
│            ├─ RecordInteraction (with direction)             │
│            ├─ Update last_contacted (mutual/inbound only)    │
│            ├─ Create/refresh follow-up task (on outbound)   │
│            └─ Auto-complete follow-up task (on inbound/mutual)│
│                                                              │
│  Auth Handler (settings UI — in-memory state machine)       │
│      ├─ POST /api/v1/telegram/auth/start                    │
│      ├─ POST /api/v1/telegram/auth/verify-code              │
│      ├─ POST /api/v1/telegram/auth/verify-password          │
│      └─ DELETE /api/v1/telegram/auth                        │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

### 5.2 gotd/td Integration

**Library:** `github.com/gotd/td` — pure Go MTProto implementation.

**Key packages:**
- `github.com/gotd/td/telegram` — client, auth, session
- `github.com/gotd/td/tg` — all Telegram types (messages, users, chats, updates)
- `github.com/gotd/td/telegram/auth` — auth flows (phone + code + 2FA)
- `github.com/gotd/td/telegram/query` — pagination helpers for dialogs/messages
- `github.com/gotd/contrib/middleware/ratelimit` — proactive rate limiter
- `github.com/gotd/contrib/middleware/floodwait` — reactive FLOOD_WAIT handler

**Client lifecycle — long-lived connection:**
- The client runs persistently inside `client.Run(ctx, func(ctx) error { ... })` for the lifetime of the backend process.
- Started on backend boot (if Telegram session exists and is connected).
- Session storage: custom `session.Storage` backed by `telegram_session` DB table (encrypted).
- Rate limiting: token bucket (1 request per 200ms) + flood wait handler as middleware.
- Auth: separate from sync. Auth holds a temporary client in memory for up to 5 min (see [Section 2.7](#27-authentication-flow-web-ui)).
- On successful auth: start the long-lived connection.
- On disconnect: automatic reconnection with exponential backoff (built into gotd's `reconnectUntilClosed`).

**Update handlers (real-time):**

```go
dispatcher := tg.NewUpdateDispatcher()
dispatcher.OnNewMessage(handleNewMessage)         // private + group messages
dispatcher.OnNewChannelMessage(handleNewChannel)   // supergroup/channel messages
dispatcher.OnEditMessage(handleEditMessage)        // edited messages
dispatcher.OnDeleteMessages(handleDeleteMessages)  // deleted message IDs

gaps := updates.New(updates.Config{
    Handler:  dispatcher,
    Storage:  postgresStateStorage,  // persists pts/qts/seq for gap recovery
})
```

**Gap recovery (pts/qts state):**
- The `telegram/updates` package tracks sequence numbers (pts/qts/seq) to detect missed updates after disconnects.
- On reconnect, it calls `updates.getDifference` to fetch any missed updates.
- Requires implementing `updates.StateStorage` interface (9 methods) backed by PostgreSQL — ~150 lines of sqlc/repository code.
- Also requires `updates.ChannelAccessHasher` interface (2 methods) — ~30 lines.
- A 15-minute idle timeout triggers periodic `getDifference` as a safety net.

**Initial sync (backfill on first connect):**
- `messages.getDialogs` — list conversations (via `query.GetDialogs` iterator)
- `messages.getHistory` — get history per chat (via `query.Messages` iterator)
- Neither call marks messages as read (that requires `messages.readHistory`, which we never call).
- Backfill runs once on first connect, then update handlers take over.

**Backfill resumability:** If the backend restarts mid-backfill, progress must not be lost. Per-chat backfill state is tracked via the `telegram_chat_config` table — add a `backfill_cursor` column (last fetched message ID) and a `backfill_complete` boolean. On restart, only chats with `backfill_complete = false` are re-scanned, starting from `backfill_cursor`. This is set in the migration 032 schema (see Section 6).

**Runtime overhead on Pi:**
- ~4 goroutines idle (reconnect loop, ping loop, read loop, update handler)
- ~5MB steady-state memory (state maps, peer cache)
- One 60-byte ping packet per minute
- Less overhead than polling (no repeated key exchange)

**Message type handling:**
- `tg.Message` — main type. Fields: `Message` (text), `Out` (bool), `Date` (unix), `ReplyTo` (reply header), `Media` (media class), `ID`, `EditDate`
- Media detection via `tg.MessageMediaClass` type switch: `MessageMediaPhoto`, `MessageMediaDocument`, `MessageMediaWebPage`, etc.
- Reply-to via `tg.MessageReplyHeader` → `ReplyToMsgID`
- Edit handling: `UpdateEditMessage` handler receives the updated message; overwrite stored text and set `edited_at`.
- Delete handling: `UpdateDeleteMessages` handler receives message IDs; soft-delete the `telegram_message` rows. Derived interaction unchanged.

**Gotchas:**
- Client is only valid inside `Run` callback
- Peer storage needed for resolving user IDs from compact updates (use `gotd/contrib/storage`)
- `tg` package is enormous (generated from TL schema); use https://ref.gotd.dev for docs
- Persistent connections are normal Telegram client behavior — no account restriction risk

### 5.3 Session Storage

Sessions contain MTProto auth keys (sensitive, equivalent to being logged in). Stored encrypted in PostgreSQL using the existing `TokenEncryptionKey` (AES-256-GCM, same pattern as OAuth tokens).

```go
// DatabaseSessionStorage implements session.Storage backed by telegram_session table
type DatabaseSessionStorage struct {
    repo          *repository.TelegramSessionRepository
    encryptionKey []byte
}

func (s *DatabaseSessionStorage) LoadSession(ctx context.Context) ([]byte, error) {
    session, err := s.repo.GetSession(ctx)
    if err != nil { return nil, err }
    return decrypt(session.SessionDataEncrypted, session.EncryptionNonce, s.encryptionKey)
}

func (s *DatabaseSessionStorage) StoreSession(ctx context.Context, data []byte) error {
    encrypted, nonce := encrypt(data, s.encryptionKey)
    return s.repo.UpsertSession(ctx, encrypted, nonce)
}
```

### 5.4 Connection Manager (not a SyncProvider)

Telegram does **not** use the `sync.SyncProvider` interface. The existing sync architecture (`RunDueSyncs` → `SyncProvider.Sync()` on a schedule) assumes stateless, interval-based polling. Telegram's long-lived connection is a fundamentally different lifecycle.

Instead, Telegram is a **connection manager** — a standalone component with its own lifecycle, started on backend boot:

```go
// TelegramManager manages the long-lived MTProto connection and message processing.
// It is NOT a sync.SyncProvider. It runs independently of the scheduler.
type TelegramManager struct {
    sessionRepo         *repository.TelegramSessionRepository
    messageRepo         *repository.TelegramMessageRepository
    chatConfigRepo      *repository.TelegramChatConfigRepository
    updateStateRepo     *repository.TelegramUpdateStateRepository
    externalContactRepo *repository.ExternalContactRepository
    identityService     identityMatcher
    interactionRecorder interactionRecorder
    aggregationEngine   *AggregationEngine
    encryptionKey       []byte
    apiID               int
    apiHash             string

    client              *telegram.Client  // nil when disconnected
    cancel              context.CancelFunc
}

// Start launches the long-lived connection. Called on backend boot
// if a connected session exists. Also called after successful auth.
func (m *TelegramManager) Start(ctx context.Context) error {
    // 1. Load encrypted session from DB
    // 2. Create client with session storage, rate limiter, flood wait middleware
    // 3. Set up update dispatcher (OnNewMessage, OnEditMessage, OnDeleteMessages)
    // 4. Set up gap recovery (updates.New with PostgreSQL-backed StateStorage)
    // 5. Run client in background goroutine (auto-reconnects)
    // 6. Perform initial backfill if first connect (getDialogs + getHistory)
}

// Stop gracefully shuts down the connection.
func (m *TelegramManager) Stop() { m.cancel() }

// Status returns connection health for the settings UI.
func (m *TelegramManager) Status() TelegramStatus {
    // connected/disconnected/reconnecting, last message time, error if any
}
```

**Integration with external_sync_state:** Telegram still creates an `external_sync_state` row (source='telegram') for status tracking and the settings UI. But `RunDueSyncs` does not manage it — the connection manager updates `last_sync_at` and `status` directly.

**Why not SyncProvider:** The SyncProvider interface assumes:
- Stateless: `Sync()` is called, does work, returns. No persistent state between calls.
- Scheduled: `RunDueSyncs` checks `next_sync_at` and calls `Sync()` when due.
- Interval-based: `DefaultInterval` sets the polling frequency.

Telegram needs:
- Stateful: persistent MTProto connection with auth keys, pts/qts state.
- Event-driven: update handlers fire on incoming messages, not on a schedule.
- Long-lived: connection persists for the lifetime of the backend process.

The backfill (initial sync on first connect) uses `getDialogs` + `getHistory` internally but does not go through the SyncProvider interface.

### 5.5 Message Aggregation Engine

The aggregation engine processes raw `telegram_message` rows into `interaction` records. It is invoked from update handlers (on each new message batch) and from the backfill pipeline (on initial connect). It processes messages that haven't been aggregated yet.

**Session key:** Each aggregated interaction gets `source_ref = "tg:{chat_id}:{first_msg_id}"` where `chat_id` is the Telegram chat ID and `first_msg_id` is the Telegram message ID of the first message in the burst/session. This is stable and deterministic — re-running aggregation for the same messages produces the same key.

**Mutability and delayed reply bridging:**

When a reply arrives within the bridge window of a previously-committed outbound interaction:

1. The aggregation engine queries for a recent outbound interaction with source `telegram` for this contact within the reply bridge window (using `FindInWindow` or a direct query on `source_ref` prefix).
2. If found: **update the existing interaction row in place** — set `direction='mutual'` and `occurred_at` to the reply timestamp. The `source_ref` is unchanged (immutable key). This requires a new repository method `UpdateInteractionDirection(ctx, id, direction, occurredAt)`.
3. After the in-place update, call the contact-field update logic for `direction=mutual`: this sets `last_contacted`, `last_interaction_at`, `last_response_at`, and recalculates `contact_by`.
4. If a pending follow-up Todoist task exists for this contact, auto-complete it (response received).
5. **Reassign prior outbound messages:** Update all `telegram_message` rows that reference this `interaction_id` — they already point to the correct (now-mutual) interaction, so no reassignment is needed. The `interaction_id` FK is stable because we update in place rather than delete+recreate.
6. Set `processed_at` and `interaction_id` on the newly-arrived inbound messages, pointing to the same (now-mutual) interaction.

**Timezone handling for `occurred_at` and contact field updates:** Telegram message timestamps (`msg.Date`) are Unix UTC. When the aggregation engine sets `occurred_at` on an interaction and triggers `last_contacted` / `contact_by` updates, it must account for the user's local timezone. A message at 11pm CT on March 31st is April 1st UTC — the `contact_by` calculation (which determines "days since last contact") should use the user's local date, not UTC, to avoid off-by-one errors at day boundaries. This applies to all timestamp-driven contact field updates from Telegram messages.

**`processed_at` semantics:**
- Set on `telegram_message` rows when they are successfully assigned to an `interaction_id`.
- Messages with `processed_at IS NULL AND matched_contact_id IS NOT NULL` are the aggregation input set.
- When a reply arrives: the reply message is unprocessed, triggers re-aggregation for that contact, and the engine checks for bridgeable outbound interactions in the window. Because promotion is in-place, previously-processed outbound messages keep their existing `interaction_id` — no reassignment needed.

```go
type AggregationEngine struct {
    burstWindowHours    int     // default: 2
    replyBridgeHours    int     // default: 48
    messageRepo         *repository.TelegramMessageRepository
    interactionRepo     *repository.InteractionRepository
    interactionRecorder interactionRecorder
}

func (e *AggregationEngine) AggregateForContact(ctx context.Context, contactID uuid.UUID) error {
    // 1. Fetch unprocessed telegram_messages for this contact, ordered by sent_at
    // 2. Group into bursts (same direction, within burst window)
    // 3. For each burst:
    //    a. Check if it bridges with a recent outbound interaction
    //       (reply bridge window OR reply_to_msg_id match)
    //    b. If bridging: UPDATE existing interaction in place
    //       (set direction='mutual', occurred_at=reply timestamp)
    //       then update contact fields for mutual direction
    //       (outbound messages already reference this interaction_id — no reassignment needed)
    //    c. If not bridging: CREATE new interaction with appropriate direction
    // 4. Set processed_at and interaction_id on all newly-processed telegram_messages
}
```

### 5.6 Identity Matching

Telegram peers are matched to CRM contacts in priority order:

1. **Primary: Telegram username** — Match `peer_username` against `external_identity(identifier_type='telegram')`. Also check `contact_method(type='telegram')`. Uses `IdentityService.MatchOrCreate` in discovery mode.
2. **Secondary: Phone number** — Match `peer_phone` (if available; Telegram only shares phone for mutual contacts) against `external_identity(identifier_type='phone')`. Uses `IdentityService.MatchOrCreate`.
3. **Unmatched** — Upsert `external_contact(source='telegram', source_id=peer_user_id)` for discovery. Also create `external_identity(identifier_type='telegram', source='telegram', contact_id=NULL)` for future matching.

**No fuzzy auto-linking by peer name (v1):** Authoritative `PeerMatcher` still does exact username/phone only — fuzzy name matching is never used to auto-link a peer to a CRM contact. The risk of false positives from common first names (e.g., "John", "Alex") outweighs the benefit.

**Read-time suggested matches include `@username` (issue #272):** On the import-candidates endpoint, `ImportMatchService` additionally compares a normalized `@username` against `contact.full_name` via pg_trgm to populate `suggested_match` on handle-only candidates. This is a read-time computation only — it does not affect authoritative linking or the `external_contact.match_status`. Two safeguards keep the signal clean: an exact-handle bonus (same strict-equality normalization on both sides) pushes clear matches over the confidence threshold, and a collision-gap rule suppresses ambiguous handles where top-1 and runner-up are within 0.15 of each other. Users still confirm each suggestion via the existing link UI.

**Reverse-direction back-linking (deferred to #182):** Adding a Telegram `contact_method` to an existing CRM contact does NOT retroactively link previously-unmatched `telegram_message` rows or `external_contact` discovery rows for that handle. Users can still link via the import-candidate UI (`POST /imports/:id/link`), which calls the existing `OnPeerLinked` path. Generic back-linking is tracked under #182's `RematchService`.

### 5.7 Discovery via external_contact

Discovery uses the existing import candidates pipeline built on the `external_contact` table and `GET /api/v1/imports/candidates` endpoint (`backend/internal/api/handlers/import.go:109`).

During sync, unmatched peers are upserted into `external_contact`:

```sql
INSERT INTO external_contact (source, source_id, display_name, first_name, last_name, match_status, metadata)
VALUES ('telegram', :peer_user_id, :display_name, :first_name, :last_name, 'unmatched',
        jsonb_build_object(
            'username', :username,
            'message_count', :count,
            'last_message_at', :last_at,
            'outbound_count', :out_count,
            'inbound_count', :in_count
        ))
ON CONFLICT (source, source_id, COALESCE(account_id, ''))
DO UPDATE SET
    display_name = EXCLUDED.display_name,
    metadata = external_contact.metadata || EXCLUDED.metadata,
    synced_at = NOW();
```

The existing `ListImportCandidates` handler returns these alongside Google/iCloud contacts. The frontend import candidates page already supports `source` filtering.

---

## 6. Database Migrations

### Migration 031: Interaction Direction + Contact Fields

```sql
-- 031_interaction_direction.up.sql

-- 1. Add direction column to interaction
ALTER TABLE interaction ADD COLUMN direction TEXT NOT NULL DEFAULT 'mutual';
ALTER TABLE interaction ADD CONSTRAINT interaction_direction_check
    CHECK (direction IN ('outbound', 'inbound', 'mutual'));

-- 2. Update source constraint to include telegram
ALTER TABLE interaction DROP CONSTRAINT interaction_source_check;
ALTER TABLE interaction ADD CONSTRAINT interaction_source_check
    CHECK (source IN ('manual', 'gcal', 'todoist', 'telegram'));

-- 3. Add new contact timestamp columns
ALTER TABLE contact ADD COLUMN last_interaction_at TIMESTAMPTZ;
ALTER TABLE contact ADD COLUMN last_outreach_at TIMESTAMPTZ;
ALTER TABLE contact ADD COLUMN last_response_at TIMESTAMPTZ;

-- 4. Backfill: existing last_contacted represents mutual interactions
UPDATE contact
SET last_interaction_at = last_contacted,
    last_outreach_at = last_contacted,
    last_response_at = last_contacted
WHERE last_contacted IS NOT NULL;

-- 5. Indexes for sorting/filtering contact list by new fields
CREATE INDEX idx_contact_last_interaction_at ON contact(last_interaction_at DESC NULLS LAST)
    WHERE deleted_at IS NULL;
CREATE INDEX idx_contact_last_outreach_at ON contact(last_outreach_at DESC NULLS LAST)
    WHERE deleted_at IS NULL;
CREATE INDEX idx_contact_last_response_at ON contact(last_response_at DESC NULLS LAST)
    WHERE deleted_at IS NULL;
```

**New sqlc query (in `interaction.sql`):**

```sql
-- name: UpdateInteractionDirection :one
-- Promote an outbound interaction to mutual when a reply arrives (in-place update)
UPDATE interaction
SET direction = sqlc.arg(direction),
    occurred_at = sqlc.arg(occurred_at)
WHERE id = sqlc.arg(id)
  AND deleted_at IS NULL
RETURNING *;
```

**Rollback (`031_interaction_direction.down.sql`):**

```sql
DROP INDEX IF EXISTS idx_contact_last_response_at;
DROP INDEX IF EXISTS idx_contact_last_outreach_at;
DROP INDEX IF EXISTS idx_contact_last_interaction_at;
ALTER TABLE contact DROP COLUMN IF EXISTS last_response_at;
ALTER TABLE contact DROP COLUMN IF EXISTS last_outreach_at;
ALTER TABLE contact DROP COLUMN IF EXISTS last_interaction_at;
ALTER TABLE interaction DROP CONSTRAINT IF EXISTS interaction_direction_check;
ALTER TABLE interaction DROP COLUMN IF EXISTS direction;
ALTER TABLE interaction DROP CONSTRAINT IF EXISTS interaction_source_check;
ALTER TABLE interaction ADD CONSTRAINT interaction_source_check
    CHECK (source IN ('manual', 'gcal', 'todoist'));
```

### Migration 032: Telegram Tables

```sql
-- 032_telegram.up.sql

-- Telegram MTProto update state (pts/qts/seq for gap recovery)
CREATE TABLE telegram_update_state (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id BIGINT NOT NULL,          -- Telegram user ID
    pts INTEGER NOT NULL DEFAULT 0,
    qts INTEGER NOT NULL DEFAULT 0,
    seq INTEGER NOT NULL DEFAULT 0,
    date INTEGER NOT NULL DEFAULT 0,  -- Unix timestamp from Telegram
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (user_id)
);

-- Channel-specific pts state
CREATE TABLE telegram_channel_state (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    channel_id BIGINT NOT NULL,
    pts INTEGER NOT NULL DEFAULT 0,
    access_hash BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (channel_id)
);

-- Group chat configuration (opt-in/opt-out/auto)
CREATE TABLE telegram_chat_config (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    telegram_chat_id BIGINT NOT NULL,
    chat_title TEXT,
    chat_type TEXT NOT NULL CHECK (chat_type IN ('private', 'group', 'supergroup')),
    member_count INTEGER,
    status TEXT NOT NULL DEFAULT 'auto'
        CHECK (status IN ('auto', 'ignored', 'tracked')),
    backfill_cursor INTEGER,           -- last fetched message ID for resumable backfill
    backfill_complete BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (telegram_chat_id)
);

CREATE INDEX idx_telegram_chat_config_status ON telegram_chat_config(status);

-- Telegram session storage (encrypted MTProto auth keys)
-- auth_state only tracks persistent state (disconnected | connected).
-- Transient auth-in-progress state lives in server memory (see spec Section 2.7).
-- Single-row table (singleton pattern via CHECK constraint on id)
CREATE TABLE telegram_session (
    id INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),  -- enforces single row
    session_data_encrypted BYTEA NOT NULL,
    encryption_nonce BYTEA NOT NULL,
    phone_number TEXT,
    telegram_user_id BIGINT,
    username TEXT,
    auth_state TEXT NOT NULL DEFAULT 'disconnected'
        CHECK (auth_state IN ('disconnected', 'connected')),
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- Telegram message staging
CREATE TABLE telegram_message (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),

    -- Telegram identifiers
    telegram_message_id INTEGER NOT NULL,
    telegram_chat_id BIGINT NOT NULL,

    -- Chat info
    chat_type TEXT NOT NULL CHECK (chat_type IN ('private', 'group', 'supergroup')),
    chat_title TEXT,

    -- Message content
    message_text TEXT,
    message_type TEXT NOT NULL DEFAULT 'text'
        CHECK (message_type IN ('text', 'photo', 'voice', 'video', 'document', 'sticker', 'other')),
    sent_at TIMESTAMPTZ NOT NULL,
    edited_at TIMESTAMPTZ,
    is_outgoing BOOLEAN NOT NULL,

    -- Reply threading
    reply_to_msg_id INTEGER,

    -- Peer info
    peer_user_id BIGINT,
    peer_username TEXT,
    peer_first_name TEXT,
    peer_last_name TEXT,
    peer_phone TEXT,

    -- CRM links
    matched_contact_id UUID REFERENCES contact(id) ON DELETE SET NULL,
    interaction_id UUID REFERENCES interaction(id) ON DELETE SET NULL,

    -- Processing
    processed_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ,  -- set when message is deleted in Telegram (via UpdateDeleteMessages handler)

    created_at TIMESTAMPTZ DEFAULT NOW(),

    UNIQUE (telegram_chat_id, telegram_message_id)
);

-- Indexes
CREATE INDEX idx_telegram_message_contact ON telegram_message(matched_contact_id)
    WHERE matched_contact_id IS NOT NULL;
CREATE INDEX idx_telegram_message_sent_at ON telegram_message(sent_at DESC);
CREATE INDEX idx_telegram_message_chat_msg ON telegram_message(telegram_chat_id, telegram_message_id DESC);
CREATE INDEX idx_telegram_message_unprocessed ON telegram_message(matched_contact_id, sent_at)
    WHERE processed_at IS NULL AND matched_contact_id IS NOT NULL;
CREATE INDEX idx_telegram_message_peer ON telegram_message(peer_user_id)
    WHERE matched_contact_id IS NULL AND peer_user_id IS NOT NULL;
```

**Rollback (`032_telegram.down.sql`):**

```sql
DROP TABLE IF EXISTS telegram_message;
DROP TABLE IF EXISTS telegram_session;
DROP TABLE IF EXISTS telegram_chat_config;
DROP TABLE IF EXISTS telegram_channel_state;
DROP TABLE IF EXISTS telegram_update_state;
```

---

## 7. API Changes

### 7.1 Telegram Auth Endpoints

```
POST   /api/v1/telegram/auth/start           — Start auth (send phone number, returns auth_token)
POST   /api/v1/telegram/auth/verify-code      — Submit verification code (requires auth_token)
POST   /api/v1/telegram/auth/verify-password   — Submit 2FA password (requires auth_token)
DELETE /api/v1/telegram/auth                   — Disconnect (delete session)
GET    /api/v1/telegram/auth/status            — Connection status
```

**POST /api/v1/telegram/auth/start**
```json
// Request
{ "phone_number": "+15551234567" }
// Response
{ "auth_token": "random-uuid", "status": "awaiting_code", "code_type": "app", "expires_in": 300 }
```

**POST /api/v1/telegram/auth/verify-code**
```json
// Request
{ "auth_token": "random-uuid", "code": "12345" }
// Response (success)
{ "status": "connected", "username": "spengrah", "user_id": 123456789 }
// Response (needs 2FA)
{ "status": "awaiting_password" }
```

**POST /api/v1/telegram/auth/verify-password**
```json
// Request
{ "auth_token": "random-uuid", "password": "my2fapassword" }
// Response
{ "status": "connected", "username": "spengrah", "user_id": 123456789 }
```

**GET /api/v1/telegram/auth/status**
```json
{ "status": "connected", "username": "spengrah", "last_sync_at": "2026-04-05T10:00:00Z" }
```

### 7.2 Group Chat Management Endpoints

```
GET    /api/v1/telegram/chats              — List discovered group chats with config
PATCH  /api/v1/telegram/chats/:chat_id     — Update chat status (auto/ignored/tracked)
```

**GET /api/v1/telegram/chats**
```json
{
  "data": [
    {
      "telegram_chat_id": -100123456789,
      "chat_title": "Close Friends",
      "chat_type": "group",
      "member_count": 5,
      "status": "auto",
      "effective_tracked": true
    },
    {
      "telegram_chat_id": -100987654321,
      "chat_title": "Company All-Hands",
      "chat_type": "supergroup",
      "member_count": 200,
      "status": "auto",
      "effective_tracked": false
    }
  ]
}
```

`effective_tracked` is derived: `true` if `status='tracked'` OR (`status='auto'` AND `member_count <= TELEGRAM_GROUP_MAX_MEMBERS`). This lets the UI show which groups are actually being tracked without the client reimplementing the logic.

**PATCH /api/v1/telegram/chats/:chat_id**
```json
// Request
{ "status": "ignored" }
// Response
{ "telegram_chat_id": -100123456789, "status": "ignored", "effective_tracked": false }
```

### 7.3 Discovery / Import Candidates

Uses the existing endpoint (no new route needed):

```
GET /api/v1/imports/candidates?source=telegram
```

### 7.4 Interaction Endpoints (Modified)

Existing `GET /api/v1/contacts/:id/interactions` response gains `direction` field:

```json
{
  "data": [
    {
      "id": "...",
      "source": "telegram",
      "direction": "mutual",
      "occurred_at": "2026-04-05T10:30:00Z",
      "description": "Telegram exchange (3 messages)"
    }
  ]
}
```

### 7.5 Contact Endpoints (Modified)

Existing contact response gains new fields:

```json
{
  "last_contacted": "2026-04-05T10:30:00Z",
  "last_interaction_at": "2026-04-05T10:30:00Z",
  "last_outreach_at": "2026-04-03T14:00:00Z",
  "last_response_at": "2026-04-05T10:30:00Z",
  "has_pending_followup": false
}
```

---

## 8. Configuration

### 8.1 Environment Variables and Config Migration

The existing bot-API config fields are replaced:

```go
// config.go — FeatureFlags
type FeatureFlags struct {
    EnableVectorSearch  bool // Default: false
    EnableTelegramSync  bool // Default: false  ← was EnableTelegramBot
    EnableCalendarSync  bool // Default: false
    EnableExternalSync  bool // Default: false
}

// config.go — ExternalConfig
type ExternalConfig struct {
    SessionSecret      string
    AnthropicAPIKey    string
    TelegramAPIID      int    // ← was TelegramBotToken
    TelegramAPIHash    string // ← new
    APIKey             string
    BackupPath         string
    HomeServerHost     string
    HomeServerUser     string
    TokenEncryptionKey string
}
```

**Removed:** `ENABLE_TELEGRAM_BOT`, `TELEGRAM_BOT_TOKEN`
**Added:** `ENABLE_TELEGRAM_SYNC`, `TELEGRAM_API_ID` (int), `TELEGRAM_API_HASH` (string)

**Validation:** When `EnableTelegramSync` is true, require both `TelegramAPIID != 0` and `TelegramAPIHash != ""`. Same pattern as the existing `EnableTelegramBot` → `TelegramBotToken` validation in `config.go:270`.

**Environment variable mapping:**
```bash
ENABLE_TELEGRAM_SYNC=false
TELEGRAM_API_ID=12345678          # from https://my.telegram.org/apps
TELEGRAM_API_HASH=abcdef123456    # from https://my.telegram.org/apps
```

### 8.2 Aggregation Tuning (env vars with defaults)

```bash
TELEGRAM_BURST_WINDOW_HOURS=2         # Same-direction burst grouping
TELEGRAM_REPLY_BRIDGE_HOURS=48        # Outbound→inbound bridging window
TELEGRAM_BACKFILL_SINCE=2026-01-01    # Initial sync horizon (YYYY-MM-DD)
TELEGRAM_DISCOVERY_MIN_MESSAGES=3     # Min messages to surface as candidate
TELEGRAM_GROUP_MAX_MEMBERS=10         # Auto-track groups with <= N members
```

### 8.3 Follow-Up Windows (env vars with defaults)

```bash
WATCHDOG_WEEKLY_DAYS=3
WATCHDOG_BIWEEKLY_DAYS=5
WATCHDOG_MONTHLY_DAYS=7
WATCHDOG_QUARTERLY_DAYS=14
WATCHDOG_BIANNUAL_DAYS=21
WATCHDOG_ANNUAL_DAYS=21
```

---

## 9. Implementation Phases

### Phase 1: Cross-Cutting Data Model + Outreach Lifecycle (no Telegram code)

**Goal:** Add direction to interaction, new contact fields, outreach→follow-up task lifecycle.

**Changes:**
- Migration 031 (direction + contact fields)
- Update `RecordInteractionRequest` + `ContactService.RecordInteraction` to handle direction
- Direction-conditional update logic: outbound skips `last_contacted`/`contact_by`, mutual/inbound updates all fields
- Update `contact` API response to include new fields
- Update `interaction` API response to include direction
- Update handler Kind validation to include `follow_up`
- Modify `handleTaskCompletion` in todoist provider: cadence task completion records `direction=outbound` and creates follow-up Todoist task
- Modify `reconcileContactTasks`: skip contacts with pending follow-up task
- Handle follow-up task completion: record outbound, create next follow-up
- Handle inbound/mutual interaction: auto-complete pending follow-up in Todoist
- Update frontend contact card to display direction signals and follow-up status
- Tests: unit for direction-conditional logic, integration for full outreach→follow-up→response lifecycle, E2E for contact card display

**Impact on existing code:** Moderate. Default direction is 'mutual', so gcal and manual callers are unchanged. Todoist provider's `handleTaskCompletion` changes from implicit mutual to explicit outbound. Task reconciliation gains a follow-up check.

### Phase 2: Telegram Auth + Connection Infrastructure (internal plumbing, behind feature flag)

**Goal:** Backend can authenticate with Telegram and maintain a long-lived connection. No user-visible sync behavior yet — `ENABLE_TELEGRAM_SYNC` feature flag gates all Telegram functionality. The settings page shows auth flow only; no message data appears anywhere in the CRM until Phase 4 is complete.

**Changes:**
- Migration 032 (telegram tables including update_state, channel_state, chat_config)
- Add `gotd/td` and `gotd/contrib` dependencies
- `backend/internal/telegram/` package: client wrapper, DB session storage, in-memory auth session manager
- Implement `updates.StateStorage` interface (9 methods) backed by PostgreSQL (`telegram_update_state` + `telegram_channel_state`)
- Implement `updates.ChannelAccessHasher` interface (2 methods)
- Long-lived connection lifecycle: start on boot if connected, auto-reconnect, graceful shutdown
- Auth handler endpoints (start, verify-code, verify-password, disconnect, status) with auth_token and 5-min TTL
- Frontend: Telegram settings page — auth flow only (phone input, code input, 2FA input, connection status). Group chat management shows empty-state placeholder ("Connect Telegram to manage group chats").
- Config migration: replace `EnableTelegramBot`/`TelegramBotToken` with `EnableTelegramSync`/`TelegramAPIID`/`TelegramAPIHash`
- Tests: unit for session encryption/decryption, integration for auth state transitions, connection lifecycle

### Phase 3: Message Sync + Storage (internal plumbing, still behind feature flag)

**Goal:** Messages flow from Telegram (real-time + backfill) into `telegram_message` table. Still no user-visible interactions — messages are stored but not yet matched or aggregated.

**Changes:**
- Update handlers: `OnNewMessage`, `OnEditMessage`, `OnDeleteMessages` wired to message processing pipeline
- Initial backfill: dialog + history fetching for first connect (since `TELEGRAM_BACKFILL_SINCE`)
- Gap recovery: automatic via `updates.getDifference` on reconnect
- Message parsing (text, media type, reply_to, edits, deletions)
- Raw message upsert in `telegram_message` (ON CONFLICT update edits)
- Group chat filtering: respect `telegram_chat_config` status and `TELEGRAM_GROUP_MAX_MEMBERS`
- Group chat management API + settings UI: `GET/PATCH /api/v1/telegram/chats` — now that dialogs are being fetched, the chat list is populated and the user can configure tracking
- Wire update handlers into TelegramManager
- Tests: unit for message parsing, integration for message processing with mocked update handlers

### Phase 4: Identity Matching + Interaction Recording (feature goes live)

**Goal:** Messages are matched to contacts and create directional interactions. This is where the feature becomes user-visible — Telegram interactions appear on contact pages, follow-up tasks are created, `last_contacted` updates.

**Changes:**
- Peer matching via identity service (username → phone → fuzzy name)
- Unmatched peers upserted into `external_contact(source='telegram')` for discovery
- Aggregation engine (burst grouping, reply bridging with session keys, direction assignment, delayed reply promotion via in-place update)
- `RecordInteraction` calls with direction for matched contacts
- Follow-up task lifecycle: outbound interaction triggers follow-up creation; inbound/mutual auto-completes follow-up
- Frontend: Telegram peers in import candidates list (existing page, source filter)
- Tests: unit for aggregation engine (including delayed reply bridging), integration for full sync→match→interact→discover flow, E2E for discovery

**Note:** Contact card direction signals and follow-up status are implemented in Phase 1 (they work for Todoist/manual flows immediately). This phase does not duplicate that work.

---

## 10. Testing Strategy

### Unit Tests

| Area | What to Test |
|------|-------------|
| Message parsing | Parse `tg.Message` into internal `TelegramMessage` struct — text, media type, reply_to, edit detection, direction |
| Aggregation engine | Burst grouping with various time gaps; reply bridging within/outside 48h window; explicit reply_to bridging across time gap; direction assignment per burst; delayed reply promotion (in-place update to mutual); session key stability across re-runs |
| Session encryption | Encrypt/decrypt roundtrip; invalid key → error; wrong key → error |
| Identity matching | Username normalization (@-stripping, lowercase); phone normalization; match priority (username > phone > name) |
| Follow-up window calculation | Correct default window for each cadence (weekly through annual, including biannual); no follow-up when no cadence |
| Follow-up refresh on repeated outbound | Second outbound before reply refreshes due date (not creates a duplicate) |
| Direction → contact field updates | Outbound updates only `last_outreach_at`; inbound updates `last_contacted` + `last_interaction_at` + `last_response_at`; mutual updates all five |
| RecordInteraction direction-conditional | Outbound does NOT recalculate `contact_by`; mutual/inbound DOES; empty direction defaults to mutual |
| Group chat effective_tracked | `auto` + small group → tracked; `auto` + large group → skipped; `ignored` overrides small; `tracked` overrides large |
| Auth session TTL and guard | Token expires after 5 minutes; second concurrent auth attempt rejected; expired token returns appropriate error |
| Edit/delete handler mapping | `UpdateEditMessage` → telegram_message text updated + edited_at set; `UpdateDeleteMessages` → telegram_message soft-deleted; derived interaction unchanged |

### Integration Tests

| Area | What to Test |
|------|-------------|
| RecordInteraction with direction | Direction persisted correctly; denormalized contact fields updated per direction; backward compat (no direction = mutual) |
| Aggregation → interaction creation | Full pipeline: raw messages → bursts → interactions with correct direction and source_ref |
| Delayed reply bridging | Outbound interaction created, then inbound arrives in window: interaction updated in place to mutual, contact fields updated, follow-up task auto-completed |
| Outreach→follow-up lifecycle (Todoist) | Completing cadence task records outbound + creates follow-up Todoist task; inbound interaction auto-completes follow-up; completing follow-up creates another follow-up (cycle) |
| Outbound Telegram → follow-up | Outbound Telegram interaction creates follow-up task without any Todoist task completion; second outbound refreshes the due date |
| Manual outbound → follow-up | Manual interaction with direction=outbound creates follow-up for contact with cadence; no follow-up for contact without cadence |
| Follow-up reconciliation suppression | `reconcileContactTasks` skips contacts with pending follow-up (`kind='follow_up', state='managed'`); does not skip after follow-up is completed |
| TelegramManager start/stop | Start with existing connected session → connection established; start with no session → no-op; stop → graceful shutdown |
| TelegramManager reconnect + gap recovery | Disconnect simulated → reconnect with preserved pts/qts state; `StateStorage` reads/writes persist across restart |
| StateStorage persistence | All 9 `updates.StateStorage` methods read/write correctly against PostgreSQL; `ChannelAccessHasher` stores and retrieves access hashes |
| telegram_chat_config CRUD | Create/read/update chat config; computed `effective_tracked` correct for all status + member_count combinations |
| Sync flow | Mocked update handler events → messages stored → matched → interactions created with correct direction |
| Discovery candidates | Unmatched peers upserted as `external_contact(source='telegram')`; appear in `ListImportCandidates` with source filter; threshold filtering (< 3 messages excluded) |

### API Tests

| Area | What to Test |
|------|-------------|
| Auth: valid flow | POST start → awaiting_code; POST verify-code → connected (or awaiting_password); POST verify-password → connected |
| Auth: invalid/expired token | verify-code with wrong token → 400; verify-code after 5min TTL → 410 Gone |
| Auth: duplicate concurrent auth | Second POST start while first is in progress → 409 Conflict |
| Auth: disconnect | DELETE while connected → session cleared, status=disconnected; DELETE while disconnected → 200 (idempotent) |
| Auth: status | GET before connect → disconnected; GET after connect → connected with username and last_sync_at |
| Chat management: list | GET /telegram/chats returns discovered chats with correct `effective_tracked` |
| Chat management: update | PATCH /telegram/chats/:id with status=ignored → effective_tracked=false; PATCH with status=tracked on large group → effective_tracked=true |
| Contact fields | GET /contacts/:id includes `last_interaction_at`, `last_outreach_at`, `last_response_at`, `has_pending_followup` with correct values after outbound and inbound interactions |
| Interaction direction | GET /contacts/:id/interactions returns `direction` field on each interaction |

### E2E Tests

| Area | What to Test |
|------|-------------|
| Telegram settings: auth flow | Phone input → code input → connected state → disconnect flow |
| Telegram settings: group management | Group chat list displays; toggle status auto→ignored; toggle status auto→tracked; effective_tracked updates |
| Contact card: direction signals | `last_outreach_at` and `last_response_at` displayed with correct relative timestamps |
| Contact card: pending follow-up | "Awaiting reply" indicator shown when follow-up task pending; disappears after response |
| Contact task list: follow-up items | Follow-up tasks appear with distinct label/icon; kind filter includes `follow_up` |
| Import candidates: Telegram peers | Telegram peers appear in candidates list; create-contact action pre-fills name and adds Telegram contact method |

### Manual Testing

- Full auth flow with real Telegram account (phone + code + optional 2FA)
- Real-time message reception via long-lived connection
- Verify messages are NOT marked as read
- Verify rate limiting under real Telegram load (flood wait handling)
- Test session persistence across backend restarts (connection re-establishes)
- Test auth restart after process kill mid-auth (user can restart flow)
- Test session invalidation recovery (user terminates session from Telegram settings)
- Test gap recovery: disconnect Pi network briefly, send messages during gap, verify all recovered on reconnect
- Test group chat filtering: large group ignored, small group tracked, override both directions

---

## 11. Code Review Evals

Tests (Section 10) verify deterministic correctness. This section defines **qualitative evaluation criteria** for code review agents — things that require judgment, not assertions. Each eval has a rubric: what "good" looks like, common failure modes, and what to look for.

### 11.1 Code Quality & Clarity

**What to evaluate:**
- Does the direction-conditional logic in `RecordInteraction` read clearly? The branching (outbound → skip last_contacted; mutual/inbound → update everything; follow-up create/refresh/complete) is the most complex new logic. It should be obvious from reading the code which fields update for which direction, without tracing through multiple helper functions.
- Is the aggregation engine's burst/bridge logic understandable? It's inherently stateful — reviewers should be able to follow the session-building algorithm linearly.
- Does the TelegramManager lifecycle (start/stop/reconnect) have clear state transitions? Are error paths (session expired, auth revoked, network timeout) handled explicitly vs. silently swallowed?
- Are the gotd/td types handled correctly? Type switches on `tg.MessageClass`, `tg.MessageMediaClass`, `tg.PeerClass` etc. should cover all expected cases with an explicit default/fallback.

**Common failure modes:**
- Direction logic scattered across multiple service methods instead of centralized in `RecordInteraction`
- Aggregation engine that's hard to unit test because it's entangled with repository calls
- Silent error swallowing in update handlers (message processing shouldn't crash the connection, but errors should be logged with enough context to debug)
- Stringly-typed direction/kind/status values without constants

### 11.2 Naming & Conventions

**What to evaluate:**
- Do new types, methods, and packages follow existing codebase conventions? Check against `backend/internal/google/calendar.go` (sync provider pattern), `backend/internal/todoist/provider.go` (task lifecycle), `backend/internal/identity/normalize.go` (identity matching).
- Is the `backend/internal/telegram/` package organized like other integration packages? (client wrapper, handlers, data types)
- Are new sqlc query names consistent with existing patterns? (`GetX`, `ListX`, `CreateX`, `UpsertX`, `FindXByY`)
- Are new API routes consistent with existing route naming? (`/api/v1/telegram/auth/...`, `/api/v1/telegram/chats/...`)

**Common failure modes:**
- Inconsistent casing or abbreviation (`tg_msg` vs `telegramMessage` vs `TelegramMsg`)
- Package-level functions where methods on a struct would match the codebase pattern
- New constants defined in the wrong package (shared types go in `repository`, not `service`)

### 11.3 Error Handling & Resilience

**What to evaluate:**
- Does the long-lived connection degrade gracefully? Network outage → reconnect with backoff → gap recovery → resume. No data loss, no user-visible errors during recovery.
- Do update handlers isolate failures? A malformed message from one chat should not block processing of other chats. A failed identity match should not prevent message storage.
- Is the auth flow resilient to partial completion? Backend restart mid-auth → clean state, user can restart. Telegram sends code but user never enters it → 5-min TTL cleans up.
- Are Todoist API failures during follow-up creation/completion handled? If Todoist is down, the outbound interaction should still be recorded; follow-up creation can retry on next interaction or sync cycle.
- Does the aggregation engine handle edge cases? Empty message text (media-only), messages from deleted accounts, messages in chats that were later deleted.

**Common failure modes:**
- `panic` or unrecovered errors in goroutines (especially update handlers running inside `client.Run`)
- Follow-up task creation failure causing the entire `RecordInteraction` to fail (should be best-effort, not blocking)
- Rate limit errors surfaced as user-visible 500s instead of handled with backoff

### 11.4 Performance & Resource Usage

**What to evaluate:**
- **Backfill performance:** Initial backfill (default since 2026-01-01) across many chats. Are messages fetched with appropriate batching and rate limiting? Is progress resumable if interrupted (cursor-based)?
- **Aggregation engine:** Does it process only unprocessed messages (partial index scan), or does it re-scan all messages on every run? For a contact with 10k messages, aggregation should only touch the new ones.
- **Query patterns:** Are the new contact fields (`last_interaction_at`, `last_outreach_at`, `last_response_at`) indexed for the sort/filter patterns the frontend uses? Check that the contact list page can sort by these without sequential scans.
- **Pi resource footprint:** Steady-state should be ~4 goroutines + ~5MB memory for the MTProto connection. Spikes during backfill or reconnect gap recovery are acceptable but should be bounded.
- **Todoist API overhead:** Follow-up creation/completion/refresh adds Todoist API calls on every outbound and inbound interaction. Is there batching or debouncing to avoid hammering the Todoist API during a large sync?

**Common failure modes:**
- Missing indexes on `telegram_message(matched_contact_id, sent_at) WHERE processed_at IS NULL`
- Aggregation engine loading all messages into memory instead of streaming
- N+1 query patterns in the message processing pipeline (one identity lookup per message instead of batching)
- `contact_by` recalculation on every mutual interaction even when the timestamp hasn't changed

### 11.5 Copy & UX

**What to evaluate:**
- **Todoist task wording:** "Follow up: Alice (awaiting reply)" — is this clear enough when the user sees it in their Todoist inbox alongside other tasks? Would "Awaiting reply from Alice" be better? Does the task description include useful context (how they reached out, when)?
- **Settings page copy:** Does the Telegram connection page explain what gets synced (and what doesn't) before the user enters their phone? Is the 5-minute auth timeout communicated?
- **Contact card labels:** "Last outreach: 3 days ago" / "Last response: 3 days ago" / "Awaiting reply since Tue (5 days)" — are these scannable? Do they convey the right urgency level? Is "awaiting reply" phrased from the right perspective (the user's, not the contact's)?
- **Group chat management:** Is the "auto / ignored / tracked" toggle self-explanatory? Does the user understand what "auto" means without reading docs (size-based threshold)?
- **Error states:** What does the user see if Telegram auth fails? If the connection drops? If a group chat they're tracking gets deleted? Are error messages actionable?
- **Import candidate cards:** Do Telegram peers show enough context to decide whether to import? (username, message count, last message date, direction breakdown)

**Common failure modes:**
- Technical jargon in user-facing copy ("MTProto", "interaction", "outbound")
- Passive voice in task names ("A follow-up is needed" instead of "Follow up with Alice")
- Missing empty states (no groups discovered yet, no Telegram peers found, no direction data available)
- Inconsistent terminology between Todoist task names and CRM UI labels

### 11.6 Security & Privacy

**What to evaluate:**
- **Session encryption:** Is the AES-256-GCM encryption correctly implemented? Unique nonce per encrypt operation? Key derived from `TokenEncryptionKey` consistently?
- **Auth token handling:** Is the in-memory auth token unpredictable (crypto/rand, not math/rand)? Is it cleared on completion/expiry? Is it transmitted only in request bodies, not URL params?
- **Message content storage:** Full message text is stored in PostgreSQL. Is the DB encrypted at rest (Pi disk encryption)? Are there any API endpoints that could inadvertently leak message text to unauthorized callers?
- **Telegram API credentials:** `api_id` and `api_hash` are app-level (not secret per Telegram docs), but are they excluded from API responses and frontend bundles?
- **Phone number handling:** Phone number is submitted during auth but only stored in `telegram_session.phone_number` for display. Is it ever logged? Included in error messages? Returned in API responses beyond the settings page?

**Common failure modes:**
- Nonce reuse in session encryption
- Auth token logged at INFO level
- Message text included in error logs or API error responses
- Phone number in sync log metadata

### 11.7 Architectural Coherence

**What to evaluate:**
- **TelegramManager vs. existing patterns:** The manager is a new architectural pattern (long-lived connection) in a codebase that's otherwise request/response + scheduled jobs. Does it integrate cleanly with the existing startup/shutdown lifecycle in `main.go`? Is it testable in isolation?
- **Direction model integration:** Does the `direction` column feel natural on the `interaction` table? Are there any queries or UI views where direction adds confusion rather than clarity?
- **Follow-up lifecycle centralization:** The "on any outbound → create/refresh follow-up" rule lives in `RecordInteraction`. Is this the right place, or does it create a hidden coupling between the interaction recording path and the Todoist API? Would a domain event / observer pattern be cleaner?
- **Aggregation engine boundaries:** Is the engine a clean, testable unit with clear inputs (unprocessed messages) and outputs (interactions), or is it entangled with the identity matching / contact updating / follow-up creation pipeline?
- **Migration safety:** Can migration 031 (direction + contact fields) be deployed independently of migration 032 (Telegram tables)? Is rollback safe for each?

**Common failure modes:**
- TelegramManager that's only testable with a real Telegram connection (no interface boundaries for mocking)
- Follow-up creation inside `RecordInteraction` making it impossible to record an interaction without a Todoist dependency
- Aggregation engine that mutates contact state directly instead of returning interaction records for the caller to process

### 11.8 Observability & Debuggability

**What to evaluate:**
- **Reconnect and gap recovery logging:** When the long-lived connection drops and re-establishes, is there a clear log trail? Reviewer should see: disconnect reason, backoff duration, reconnect success, pts/qts state before and after gap recovery, number of missed updates recovered. Without this, production debugging requires attaching to the process.
- **Message processing pipeline visibility:** For each sync batch (update handler or backfill), are key metrics logged? Messages received, matched, unmatched, interactions created, follow-ups created/refreshed/completed. The existing gcal sync logs `items_processed` / `items_matched` via `SyncResult` — Telegram should provide equivalent visibility even though it's not a SyncProvider.
- **Skipped chat logging:** When a group chat is skipped (too large, status=ignored), is it logged at DEBUG with the chat title and reason? This is essential for diagnosing "why isn't this contact showing interactions" without querying the DB.
- **Auth lifecycle logging:** Auth start (phone submitted, no phone in log), code sent, code verified, 2FA required, 2FA verified, session stored, auth timeout, auth error — each should be a distinct log event with the auth token (not the phone or code).
- **Follow-up task lifecycle logging:** Follow-up created (contact, due date), refreshed (old due date → new), auto-completed (response source), Todoist API failure (with retry intent). A missing follow-up should be diagnosable from logs alone.
- **Status endpoint richness:** Does `GET /api/v1/telegram/auth/status` return enough for a human to reason about health? Connected since, last message received at, last error (if any), reconnect count, messages processed today. Not just "connected" / "disconnected".
- **Drift diagnosis:** Can someone determine whether `external_sync_state`, `telegram_session.auth_state`, and the actual MTProto connection are in agreement without reading code? The status endpoint or a health check should surface discrepancies.

**Common failure modes:**
- Reconnect loop logged only at DEBUG (invisible in production default log level)
- Gap recovery silently succeeds with zero recovered messages (was there really no gap, or did recovery fail silently?)
- Follow-up creation failure logged but not surfaced in status or health check
- `external_sync_state.status` stuck on "syncing" after a crash (no cleanup on unclean shutdown)
- Skipped chats logged per-message instead of per-chat (log spam)

### 11.9 Layer Discipline & Package Boundaries

**What to evaluate:**
- **Handler → Service → Repository → sqlc:** The core rule from `.ai/rules/core.md`. Reviewers must verify that the new `telegram/` package does not call sqlc queries directly — it should go through repository methods. The `TelegramManager` is not a handler, but it must still use repositories, not raw queries. The auth and chat HTTP handlers must call through a service or manager, never directly to repositories.
- **No raw SQL in Go:** All queries must be defined in `.sql` files under `backend/internal/db/queries/` and generated via `make sqlc`. The aggregation engine's window/bridge logic may be tempting to implement as raw SQL — it should be application-level Go code operating on repository-returned data.
- **No gotd/td types leaking across package boundaries:** `tg.Message`, `tg.User`, `tg.MessageMediaClass`, etc. must be confined to the `backend/internal/telegram/` package. Other packages (service, repository, handlers) should only see CRM-native types (`repository.TelegramMessage`, `repository.Interaction`, etc.). This prevents the rest of the codebase from acquiring a dependency on the gotd type system.
- **No Todoist SDK types in the aggregation engine:** The aggregation engine should produce interactions and signal "create/refresh/complete follow-up" without knowing about the Todoist API. The Todoist calls should happen at the caller level (manager or service), not inside the engine.
- **Identity matching via service interface:** The `telegram/` package should call identity matching through a defined interface (like gcal's `identityMatcher`), not by importing the identity package directly. This keeps the dependency graph clean and testable.

**Common failure modes:**
- `TelegramManager` importing `backend/internal/db` directly instead of going through repository
- Handler calling `manager.ProcessMessage()` which internally calls `queries.CreateInteraction()` — skipping the service layer
- Aggregation engine returning `tg.Message` structs instead of converting to internal types at the boundary
- `gotd/td` types appearing in repository struct fields or service method signatures
- Circular dependency: `telegram/` → `service/` → `telegram/` (use interfaces to break the cycle, same pattern as todoist's `interactionRecorder`)

---

## 12. UI Specifications

This section enumerates the frontend work required. Detailed visual designs are deferred to implementation time.

### 12.1 Settings: Telegram Integration Page

**Route:** `/settings/telegram`

**States:**
1. **Not configured** — "Connect Telegram" button. Brief explanation of what gets synced.
2. **Auth in progress** — Multi-step form: phone number input → code input → optional 2FA password input. Loading states between steps. 5-minute timeout warning.
3. **Connected** — "Connected as @username" with last sync time. "Disconnect" button.
4. **Group chat management** — Table of discovered group chats: chat title, member count, status toggle (auto/tracked/ignored). Appears after first sync when groups are discovered.

### 12.2 Contact Detail Page: Direction Signals

**New section on contact card** (below cadence indicator):

- "Last outreach: N days ago (source)" — shown when `last_outreach_at` is set
- "Last response: N days ago (source)" — shown when `last_response_at` is set
- "Awaiting reply since {date} ({N days})" — shown when a follow-up task exists (`kind='follow_up', state='managed'`). Warning/amber styling.

### 12.3 Import Candidates Page

No new page needed. Existing `/imports/candidates` page shows Telegram peers alongside Google/iCloud contacts when `source=telegram` filter is applied. Telegram-specific metadata (username, message count) displayed in candidate cards.

### 12.4 Follow-Up Task Display

Follow-up tasks appear in the existing task list on the contact detail page with `kind='follow_up'`. The frontend task list filtering (`oneof=action cadence`) must add `follow_up`. Follow-up tasks should be visually distinguished (e.g., different icon or label: "Follow-up" vs "Outreach").

---

## 13. Open Questions & Future Work

### Open Questions

1. **Aggregation edge cases — multi-source same day:** If a contact has both Telegram and gcal interactions on the same day, they produce separate interaction records (different sources), which is correct. The contact card should show a unified "last interaction" across all sources via `last_interaction_at` (source-agnostic).
2. **Concurrent MTProto sessions:** Telegram limits the number of concurrent sessions per account (~10). Creating a CRM session shouldn't invalidate mobile/desktop sessions, but verify during manual testing.

### Future Work (Not in Scope)

- **Knowledge capture (agent-brain pattern)** — pipe interesting Telegram messages into contact notes or a knowledge base. Referenced as a long-term direction.
- **iMessage integration (#73)** — same aggregation engine, different transport (Mac worker + SQLite). The aggregation engine and direction model built here are reusable.
- **WhatsApp integration** — same aggregation engine, different API.
- **Relationship health dashboard** — aggregate directional interaction data into per-contact health scores and trends.
- **Multiple Telegram accounts** — `SupportsMultiAccount` is set to false initially but the sync infrastructure supports it.
- **Message search** — full-text search over Telegram message content via pg_trgm (already enabled).

---

## Appendix A: File Inventory

### New Files

| File | Purpose |
|------|---------|
| `backend/migrations/031_interaction_direction.up.sql` | Direction column + contact fields + indexes |
| `backend/migrations/031_interaction_direction.down.sql` | Rollback |
| `backend/migrations/032_telegram.up.sql` | Telegram session, message, update state, channel state, and chat config tables |
| `backend/migrations/032_telegram.down.sql` | Rollback |
| `backend/internal/db/queries/telegram.sql` | sqlc queries for telegram tables |
| `backend/internal/telegram/client.go` | gotd/td client wrapper |
| `backend/internal/telegram/session.go` | Encrypted DB session storage |
| `backend/internal/telegram/auth.go` | In-memory auth session manager (5-min TTL) |
| `backend/internal/telegram/connection.go` | Long-lived MTProto connection lifecycle (start, reconnect, shutdown) |
| `backend/internal/telegram/handlers.go` | Update handlers (OnNewMessage, OnEditMessage, OnDeleteMessages) |
| `backend/internal/telegram/state_storage.go` | `updates.StateStorage` + `ChannelAccessHasher` implementations (PostgreSQL-backed) |
| `backend/internal/telegram/manager.go` | TelegramManager: connection lifecycle, backfill, message processing |
| `backend/internal/telegram/aggregation.go` | Message → interaction aggregation engine |
| `backend/internal/telegram/auth_handler.go` | Auth flow HTTP handlers |
| `backend/internal/telegram/chat_handler.go` | Group chat management HTTP handlers (list chats, update status) |
| `backend/internal/repository/telegram_session.go` | Session repository |
| `backend/internal/repository/telegram_message.go` | Message repository |
| `backend/internal/repository/telegram_update_state.go` | StateStorage + ChannelAccessHasher repository |
| `backend/internal/repository/telegram_chat_config.go` | Chat config repository |
| `backend/internal/db/queries/telegram_update_state.sql` | sqlc queries for pts/qts state |
| `backend/internal/db/queries/telegram_chat_config.sql` | sqlc queries for chat config |
| `frontend/src/app/settings/telegram/page.tsx` | Telegram settings page |
| `frontend/src/components/TelegramAuthFlow.tsx` | Auth flow UI component |

### Modified Files

| File | Change |
|------|--------|
| `backend/internal/repository/interaction.go` | Add Direction to request/response structs, add InteractionSourceTelegram, add UpdateInteractionDirection method |
| `backend/internal/db/queries/interaction.sql` | Add UpdateInteractionDirection query for in-place promotion |
| `backend/internal/service/contact.go` | Direction-conditional RecordInteraction: outbound skips last_contacted/contact_by; followUpManager integration for create/refresh/complete |
| `backend/internal/db/queries/contact_task.sql` | Add FindPendingFollowUp, CompleteFollowUpForContact queries |
| `backend/internal/repository/contact_task.go` | Add follow-up task query methods |
| `backend/internal/api/handlers/interaction.go` | Direction in API response |
| `backend/internal/api/handlers/contact.go` | New fields in contact API response |
| `backend/internal/api/handlers/contact_task.go` | Add 'follow_up' to Kind validation |
| `backend/internal/todoist/provider.go` | handleTaskCompletion records direction=outbound; creates follow-up Todoist task; reconcileContactTasks skips contacts with pending follow-up |
| `backend/internal/config/config.go` | Replace TelegramBot config → TelegramSync config (API ID + hash) |
| `backend/cmd/crm-api/main.go` | Create TelegramManager, wire auth handler, start connection on boot |
| `frontend/src/types/contact.ts` | New contact fields (last_interaction_at, etc.) |
| `frontend/src/app/contacts/[id]/page.tsx` | Display direction signals + pending follow-up status |
| `frontend/src/app/settings/page.tsx` | Link to Telegram settings |

### Dependency Additions

| Dependency | Purpose |
|------------|---------|
| `github.com/gotd/td` | MTProto client |
| `github.com/gotd/contrib` | Rate limiting, flood wait, peer storage middleware |
