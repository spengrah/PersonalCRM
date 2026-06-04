# Google Chat (GChat) Integration — Functional & Technical Specification

**Issue:** #337
**Related:** #70 (Gmail — shared OAuth + `comms_message` substrate), Telegram integration (`.ai/spec/telegram-integration.md` — chat-source patterns), Telegram method enrichment (`.ai/spec/telegram-method-enrichment.md` — dual-source rematch precedent), iMessage / Mac daemon (`.ai/spec/mac-daemon.md`, `.ai/spec/mac-daemon-phase-2-anarlog-matching.md` — chat-source patterns over the shared aggregation engine)
**Status:** Draft v1
**Last Updated:** 2026-06-04

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

Add Google Chat (GChat) as a backend sync source that ingests **full message content** for chat exchanges with **known CRM contacts only**, records them as direction-aware interactions, and updates cadence timestamps. GChat is **not** a discovery source — it never creates new contacts. Like Gmail and Telegram, this feature only produces the durable content substrate plus interactions; downstream features (e.g. AI summarization) are out of scope.

The integration is structured to fit into the existing **family of chat-based message sources** — Telegram, Apple Messages (iMessage via the Mac daemon), and now GChat. Where logic already generalizes (the shared `messaging/aggregation` engine, the `comms_message` content store designed in §6 of the Gmail spec, the multi-handler-per-type `RematchService`), GChat plugs in. Where the existing logic only nominally generalizes but is in practice source-bound, this spec proposes targeted refactors in §5.X and notes whether each is RECOMMENDED to land with GChat or PROPOSED as follow-up.

### Why

Google Chat is a primary communication channel for the user's professional correspondence and personal group spaces. Syncing it means `last_contacted` / `last_outreach_at` update automatically from real chat exchanges, contact timelines reflect those exchanges, and a faithful copy of message bodies is stored locally for later use. The same posture as Gmail and Telegram applies: restrict to known contacts; mirror upstream edits and deletes; never create contacts implicitly.

### Key Decisions (verified — do not re-debate)

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Source name | `gchat` | Already a valid `contact_method.type` value (`backend/migrations/007_add_contact_methods.up.sql:13`); aligns with `external_sync_state.source` and the future `interaction.source` constant |
| Runtime | Pi backend sync provider (alongside `gcal`, `email`) | Reuses Google OAuth and the existing scheduler tick. Polling-only — no public webhook endpoint on the Pi behind Tailscale (see real-time discussion in §10) |
| MVP scope | **All three space types**: DMs (`DIRECT_MESSAGE`), group chats (`GROUP_CHAT`), named spaces (`SPACE`) | Telegram already handles group/multi-member messaging with sender-attribution semantics; the same logic carries over (§4) |
| Discovery | **None** — known contacts only, match-or-skip | Same posture as Gmail and Telegram. Users add contacts elsewhere; GChat just records exchanges with people already in the CRM |
| Identity matching | **Two dual-source mechanisms** layered together (each addresses a different code path; see §5.3, §5.X.7, §5.X.2 for why both): (1) **Sender-resolution dual-source** encoded directly in the sqlc query — `ListGChatIdentitiesForSync` selects rows where `type IN ('gchat', 'email')`, so a single per-sweep query builds a map covering both contact-method types. The identity-mapping-layer mapping (`MapIdentifierTypeToContactMethodTypes(IdentifierTypeGChat) → [gchat, email]`) is intentionally NOT added in MVP — see §5.X.7 — because no caller invokes `IdentityService.MatchOrCreate` with `IdentifierTypeGChat`. The sqlc query is the **single source of truth** for the `(gchat, email)` set in MVP; two encodings would risk drift. (2) **Backfill-trigger dual-source** at the rematch-handler layer — TWO `RematchHandler` registrations (`IdentifierType()="gchat"` and `IdentifierType()="email"`) both invoke the GChat backfill | (1) the sqlc query carries the dual-source set; the `MapIdentifierTypeToContactMethodTypes` WhatsApp precedent (`backend/internal/identity/normalize.go:102-103`) is preserved as a reference shape if/when an actual `MatchOrCreate(IdentifierTypeGChat, ...)` caller emerges. (2) is the **Telegram** pattern (two `RematchHandler` registrations across two `IdentifierType` strings) — `backend/internal/telegram/rematch.go:74-95` (username handler, `IdentifierType()="telegram"`) + `backend/internal/telegram/rematch.go:97-121` (phone handler, `IdentifierType()="phone"`). Both layers are needed because they fire on different events: (1) runs when resolving a sender on every sweep; (2) runs once when a user adds `email`/`gchat` to an existing contact. They're not redundant — neither covers the other's trigger |
| Real-time vs polling | Polling | No public webhook on the Pi. The user is single-account on a `@gmail.com` consumer account; even if Workspace Events ever opens to consumer accounts, Pub/Sub delivery still needs a public endpoint |
| Content storage | Reuse **`comms_message`** — no new content table | `comms_message` (`backend/migrations/058_comms_message.up.sql`) was designed for this. Its `claimed_at` / `claimed_session_ref` columns are already reserved on the schema for future chat-source migration. GChat is the FIRST chat source to use them |
| Cross-account dedup | `(source='gchat', external_id=<chat message resource name>, matched_contact_id)` | Chat message resource name (`spaces/<space>/messages/<id>`) treated as cross-account-stable, analog of Gmail's RFC822 Message-ID. Reuses the existing `idx_comms_message_dedup` partial unique index (`backend/migrations/058_comms_message.up.sql:31-33`). **PROVISIONAL** — cross-account stability of the Chat resource name is not yet cited from official docs; verify against the Chat API reference during implementation. If the resource name proves account-scoped, fall back to a synthesized key (`gchat:<space>:<account>:<id>`) matching the Gmail `nomsgid:` fallback shape (`backend/migrations/058_comms_message.up.sql:9`) |
| Direction model | Sender vs the connected account's `users/me`; inbound/outbound per message; promote to mutual on mixed-direction within a session | Same per-message model the rest of the codebase uses; reuses `PromoteInteractionToMutualTx` / `ExtendInteractionTx` |
| Aggregation | **Telegram-style burst + reply-bridge** over the shared `messaging/aggregation` engine. NOT Gmail's `(contact, thread, day)` shape — that's email-specific. Burst-window 2h, reply-bridge 48h initially (Telegram defaults; revisit per §10) | Chat sources are interactive: a burst of outbound followed by an inbound reply is one mutual interaction, not three day-buckets. The engine is already abstracted as a per-source adapter (`backend/internal/messaging/aggregation/interfaces.go`) and proven on two sources (Telegram, Messages) |
| Edits & deletes | Mirror upstream — update stored `comms_message` content on edit, soft-delete on delete; derived interaction unchanged | Matches Telegram (`.ai/spec/telegram-integration.md` §2.2). The interaction reflects "the exchange happened"; edits/deletes don't unwind cadence |
| Backfill | New-method handlers (`GChatHandleRematchHandler`, `GChatEmailRematchHandler`) registered on `KindContactMethodsAdded` for types `gchat` AND `email`; onboarding scan from a fixed start date (default 2026-01-01, mirroring Gmail's anchor) | Multi-handler-per-type is already supported by `RematchService` (`backend/internal/service/rematch.go:180-183`). The email handler runs alongside `GmailRematchHandler` and `CalendarRematchHandler` — RematchService already fans out |
| Cadence | Reuse existing `CadenceUpdater` via `interaction.recorded`; inbound/mutual → `last_contacted`, outbound → `last_outreach_at` | No new cadence semantics. Architectural-coherence check confirmed: Telegram's cadence rules match Gmail's (both go through the same direction-conditional `RecordInteraction`) and GChat fits the same shape |
| Pipeline | **Telegram/Messages-style**, not Gmail-style. Provider writes content rows to `comms_message` (source='gchat') with no per-message events. A periodic worker (the existing `MessagingAggregateForContactArgs` path) invokes the shared `aggregation.Engine`, which publishes the generic `KindMessageReceived`/`KindMessageSent` it already emits for Telegram and Messages — `backend/internal/messaging/aggregation/engine.go:489-545`. Cadence and follow-up consumers downstream of those existing kinds already do the right thing. Edits and deletes are reflected by direct UPDATE / soft-delete on the `comms_message` row (no provider-published `gchat.*` events) — the engine processes by `processed_at IS NULL`, so an edited row is not reprocessed and a soft-deleted row drops out of future aggregation, leaving the prior derived interaction untouched (consistent with the locked edit/delete semantics row above) | This avoids the hybrid pattern the prior draft proposed (provider per-message events PLUS delegating to the shared engine, which itself emits its own events on session creation — duplicate work and incoherent kind routing). Gmail publishes per-message events because Gmail bypasses the shared engine and uses its own `EmailInteractionConsumer` (`backend/internal/consumer/email_interaction.go:46-98`); Telegram and Messages do not publish per-message events from the provider because the engine drives publication. Reusing the engine (§5.X.1 RECOMMENDED) implies the Telegram/Messages flow, not Gmail's |
| Cross-source thread merging | **Out of scope** for MVP | Two separate interactions per source even on the same day with the same contact; deferred (§10) |

### Production scale (sizing)

The user's GChat usage is sparse relative to Gmail — most personal correspondence is over email, calendar, and Telegram. Concrete sweep cost: ~handful of spaces per account; one `spaces.list` call per sweep (or one `spaces.list` if a per-account "active space" cache is wired); per-space `spaces.messages.list` page bound by the cursor window; per-known-member `spaces.members.list` per discovered space (cached). Steady-state per-sweep API spend is expected to be dominated by `spaces.list` + a single `spaces.messages.list` per space that had activity. To be measured during MVP rollout.

---

## 2. Functional Specification

### 2.1 User Stories

1. **Connect once** — having connected a Google account (already supported), GChat sync begins after the new Chat scopes are granted. A one-time re-consent fires when the new scopes (`chat.spaces.readonly` + `chat.messages.readonly`) are added to the application's scope set. The operator-side Chat App config (name, avatar, description; Interactive features OFF) must be in place in the GCP project for `spaces.*` to return data (see §7).
2. **Automatic cadence** — when I exchange messages with a known contact in any space (DM, group chat, or named space), `last_contacted` (inbound/mutual) or `last_outreach_at` (outbound) updates without manual logging.
3. **Chat on the timeline** — GChat exchanges appear as interactions on the contact, aggregated per Telegram-style burst+reply session, with a direction signal.
4. **Follow-ups** — an outbound message to a contact participates in the existing outreach/follow-up lifecycle (a follow-up task is created/refreshed on outbound, same as Telegram/email/calls).
5. **No surprises** — messages from people not in my CRM are never ingested and never create contacts. Spaces with zero known members are still enumerated (so we don't miss a known member joining later) but produce no rows until at least one member is known.
6. **History on add** — when I add a `gchat` method OR an `email` method to a contact, their recent GChat history (within the backfill window) is pulled in across all connected accounts whose GChat sync is enabled.

### 2.2 What Gets Synced

| Item | Synced | Details |
|------|--------|---------|
| DMs with a known contact (`spaceType: DIRECT_MESSAGE`) | Yes | Primary target; direction follows sender vs `users/me` |
| Group chats with at least one known member (`spaceType: GROUP_CHAT`) | Yes | Per-message attribution: a message qualifies only when its sender is in `M ∪ knownIdentities`; other known co-members are bystanders for that specific message (§4). The sender-only-attribution-for-inbound rule is lifted from Telegram (`backend/internal/telegram/message.go:99-126`: `PeerChat` sets `PeerUserID = &senderID` per-message, only when the sender is not self). The outbound-fan-out-across-known-co-members rule is the Gmail per-participant pattern, not Telegram — see §4.3 |
| Named spaces with at least one known member (`spaceType: SPACE`) | Yes | Same per-message attribution rule as group chats |
| Spaces with zero known members | No | Enumerated (so a future membership change activates them) but produce no `comms_message` rows |
| Edits | Yes | `comms_message.body`, `snippet`, and `source_metadata` are updated in place; `processed_at` and `interaction_id` are unchanged; derived interaction is unaffected |
| Deletions | Yes | `comms_message.deleted_at` is set (soft-delete) via the same partial-unique-index-friendly path Gmail uses (`backend/migrations/058_comms_message.up.sql:32`). Derived interaction is unchanged (the exchange still happened); follow Telegram precedent |
| Drafts | No | Not sent yet — Chat API does not surface drafts to the consumer scopes |
| Reactions | No | Out of scope for MVP — same posture Telegram takes |
| Slash-command output / app-posted messages | No | `message.sender.type != "HUMAN"` filtered at the provider boundary |
| Threaded replies inside a SPACE | Yes (treated as ordinary messages) | The aggregation key uses space resource name as `ChatID`; thread context is preserved in `source_metadata` for later refinement (§10) |
| Bot conversations / channels | No | Not interpersonal |

**Per message stored (in `comms_message`):**

| Field | Stored | Notes |
|-------|--------|-------|
| Body (plaintext) | Yes | `message.text` |
| Subject | No (null) | Chat sources have no subject; column is nullable (`backend/migrations/058_comms_message.up.sql:11`) |
| Snippet | Yes | First N chars of body, mirroring Telegram's description shape |
| Participants (sender, space members) | Yes | Sender stored top-level (`peer_handle`/`peer_normalized`); other members in `source_metadata` |
| Sent timestamp | Yes | `sent_at` = `message.createTime` |
| Direction | Yes | Per-message inbound/outbound vs `users/me` (§4.1) |
| Chat message resource name | Yes | `external_id` — cross-account-stable dedup key |
| Space resource name | Yes | `thread_id` (reused; chat sources don't have email threads, so the column carries the space scope) |
| Account that observed it | Yes | `account_id` — provenance / cross-account merge target (same set-union pattern Gmail uses, §5.4 of Gmail spec) |
| `spaceType` + `messageType` | Yes | `source_metadata.space_type`, `source_metadata.message_type` |
| Attachment metadata | Yes | `source_metadata.attachments[]` = filename, mime, size (metadata only) |
| Attachment binary content | No | Out of scope, same as Gmail |
| Edit history | Yes | `source_metadata.edited_at`, `source_metadata.previous_bodies[]` (last-N capped) |

### 2.3 Connect Once: prerequisite Chat App config

GChat's read-only OAuth scopes return data only when the GCP project has a Chat App configured on the Chat API Configuration page. This is a **project-level operator setup step**, separate from the OAuth client. Required fields: app name, avatar URL, description. Interactive features must be **OFF** (we never receive webhooks, we never respond to slash commands). This is documented in §7 as part of the operator setup guide; the spec assumes it has been done.

The authenticating user is a personal `@gmail.com` account added as a Test User on the External-type consent screen. Verified by manual OAuth Playground probe (resolved: see the auth-model probe outcome in the brief).

---

## 3. Fetch Strategy

### 3.1 Steady-state incremental (per account)

For each connected account whose GChat sync is enabled, each sweep tick (default 15 min, matching Gmail/Calendar):

1. **Enumerate active spaces** via `spaces.list` (no `spaceType` filter — fetch all three types in one call). The response carries `spaceType`, `name` (resource), `lastActiveTime`, `membershipCount.joinedDirectHumanUserCount`, and optionally `displayName` (for `SPACE`). Filter out spaces with `externalUserAllowed=true` only if a later policy says so; for MVP, include them.
2. **Resolve known membership per space.** For `DIRECT_MESSAGE`, the peer is implicit — the OTHER member of the 2-person space. For `GROUP_CHAT` and `SPACE`, call `spaces.members.list` (cached per space-version; we don't refetch unless `lastActiveTime` advanced and the cached version is older than the cache TTL — see §3.4 cache discussion). Membership rows give us `users/<id>` references; **the sender's resource name is the email of their Google account** (consumer accounts) or their Workspace user id (Workspace accounts). For the MVP target (consumer `@gmail.com`), the sender carries the email and is matched against the dual-source known-identity map (§5.3). Cache the resolved (`spaces/<id>` → `[ContactID]`) map per space.
3. **Build the known-identity map** ONCE per sweep: `{ normalized_value → []ContactID }` over BOTH `contact_method.type='gchat'` AND `contact_method.type='email'` (since GChat senders carry email addresses and the user may have only added an `email` method). This is the dual-source map — see §5.3 for the new sqlc query.
4. **Skip enumeration when no known member is in the space.** If `spaces.members.list` returns no known identities, the space is logged at DEBUG (with the resource name only, never with member emails) and skipped. The space's existence is still tracked in `external_sync_state.metadata.active_spaces` so a future member-list change reactivates it.
5. **Per-space message page** via `spaces.messages.list` with `filter=createTime >= <space_cursor>` (RFC 3339 timestamps are accepted by the Chat API). The `space_cursor` is a per-space cursor stored under `external_sync_state.metadata.space_cursors` (see §3.3 for the cursor shape decision).
6. **For each message**, resolve `(sender_email, direction)` against the known-identity map. The sender qualifies the row; other known co-members are bystanders for that specific message and do not produce additional **inbound** rows (an inbound message from Alice in a group with Bob and Carol produces one row keyed on Alice — Bob and Carol do not get phantom rows). For DMs, every message qualifies (the DM peer is known by definition). For group/named spaces, only messages whose sender is a known contact OR is me produce row(s). On the **outbound** side, fan-out across the set of known co-members applies — see §4.3 for the per-known-co-member row rule and why outbound is asymmetric to inbound. Match-only — never create a contact.
7. **Advance the per-space cursor** to `max(processedMessage.createTime)`. Re-fetch overlap on the next sweep is harmless — `(source='gchat', external_id, matched_contact_id)` dedup on `idx_comms_message_dedup` (`backend/migrations/058_comms_message.up.sql:31-33`) makes ingestion idempotent.
8. **Detect edits and deletes — PROVISIONAL mechanism.** Preferred: a second-pass per-space `spaces.messages.list?filter=updateTime >= <edit_cursor>` over a separate per-space cursor. The `filter` grammar on `spaces.messages.list` is documented to support `createTime` ordering / filtering; whether `updateTime` is included in the filterable field set is **not yet verified from official documentation**. Implementation must verify this against the live Chat API reference before committing to the cursor design. **Fallback (if `updateTime` is not filterable) — bounded 7-day sliding window:** per-sweep, per-space, fetch each `comms_message` row whose stored `sent_at` is within the last 7 days via `spaces.messages.get` and compare `lastUpdateTime` against the stored value. Bound rationale: edits >7 days after send are vanishingly rare in chat (overwhelmingly typo-fixes within minutes, occasionally same-day corrections); the 7-day window covers virtually all real cases. Independent of total stored row count — a space with 6 months of history still costs only 7 days × messages/day per sweep, NOT 180 days. Concrete ceiling at the user's sparse GChat usage (~5 msgs/day across a handful of spaces): ~35 `messages.get` calls per sweep per account ≈ 3,400/day at the 15-min cadence — well under any plausible quota. The cursor name (`edit_cursor`) and the per-space cursor map shape in §3.3 are unchanged either way; only the fetch mechanism varies. On a hit, if the fetched body differs from the stored body, treat as edit; if the API returns 404 or a tombstone marker, soft-delete the `comms_message` row. Edits to messages older than 7 days are explicitly accepted as a lost-update — same posture as the edit-after-delete race in §10 open question 6 (low-frequency, high-mitigation-cost, low-impact).

### 3.2 Onboarding backfill

When GChat sync is first enabled for an account (per-space cursors empty), seed each space's `createTime` cursor to `backfill_since` (default **2026-01-01**, configurable per `external_sync_state.metadata.backfill_since`). Page each space to completion, then set the cursor to the latest processed `createTime`. The `updateTime` cursor seeds to the same anchor on first run.

Mirror Gmail's window approach: each sweep processes at most N windows per space (`gchatMaxWindowsPerSync = 24` analog) to keep one run bounded. The cursor only advances after a window is listed, paged, fetched, and filtered successfully — a partial-window crash starts the window over on the next sweep.

### 3.3 Per-space vs single-cursor per account

**Decision: per-space cursors stored in `external_sync_state.metadata.space_cursors` as a JSON map `{ "spaces/<id>": { "create_cursor": "2026-06-04T00:00:00Z", "edit_cursor": "..." } }`.** A single-cursor-per-account would force a global watermark across all spaces — a quiet group space would be re-scanned every sweep until a single active space's cursor advanced past it, inflating API spend. Per-space cursors mirror Telegram's per-chat sync approach (`telegram_chat_config.backfill_cursor` per chat, per `.ai/spec/telegram-integration.md` §5.2) and contain the cursor advance to spaces that actually had activity.

The cursor map fits comfortably under the `external_sync_state.metadata JSONB` column; expected per-account scale is <100 spaces. If the map ever exceeds ~64 KB we promote to a dedicated `gchat_space_state` table (NOT planned for MVP).

**Stale-cursor reaper.** A space the user leaves (or that becomes inaccessible — archived, deleted, permission revoked) stays in the cursor map forever otherwise. On each sweep, the enumerator's `spaces.list` response is the authoritative active set; any cursor entry for a `spaces/<id>` NOT in the latest `spaces.list` response is moved to `metadata.archived_space_cursors` (kept for a grace period — 30 days — in case access is restored, then dropped). The archived map is logged at INFO with the count only (never with space resource names beyond DEBUG). This keeps the active cursor map bounded without losing recently-active state to a transient permission flap.

**Cursor restoration on re-discovery.** If a subsequent sweep's `spaces.list` response surfaces a `spaces/<id>` that is currently in `archived_space_cursors` (i.e. access was restored before the 30-day grace expired), the reaper restores the entry: it is moved BACK from `archived_space_cursors` into `space_cursors` at its archived cursor value, and the regular per-space message-paging path resumes from that cursor. Any messages that arrived during the inaccessibility window AND were already observed via another connected account are handled by cross-account dedup on the partial unique index `idx_comms_message_dedup` (`backend/migrations/058_comms_message.up.sql:31-33`) — the upsert's set-union merge on `observed_accounts[]` records the new observation without duplicating the row. After the 30-day grace expires, the cursor is dropped; subsequent re-discovery is treated as a fresh space and reseeded from `metadata.backfill_since` per §3.2.

### 3.4 Space membership cache

`spaces.members.list` is called only when:

- a space is observed for the first time (cache miss);
- a space's `lastActiveTime` advanced (any cached version);
- the cache entry's age exceeds `gchatMembershipCacheTTL = 24h`, regardless of whether `lastActiveTime` advanced.

The age-based trigger is the safety net for the "known contact joins a quiet space" case: `lastActiveTime` only advances on new message activity, so a membership change in a long-quiet space would otherwise leave the cache stale indefinitely. The 24h TTL bounds that staleness.

The cache lives in `external_sync_state.metadata.space_members` as `{ "spaces/<id>": { "version": <lastActiveTime>, "members": ["users/<id>", ...] } }`. Only the resource names are cached, never the resolved contact IDs — the contact resolution runs every sweep against the live known-identity map (so a newly added `gchat`/`email` method takes effect on the next sweep without cache invalidation).

### 3.5 Why not the alternatives

- **`spaces.list` with `filter=spaceType="DIRECT_MESSAGE"`**: Chat API supports filters but they don't bundle types in OR-clauses; we'd need three calls. One unfiltered call returns all three types — strictly cheaper.
- **Per-message lastUpdateTime poll**: O(N) API calls per sweep where N is total stored messages. `updateTime` cursor is O(edits-since-cursor).
- **Real-time via Workspace Events / Pub/Sub**: requires a public webhook endpoint (Pi is behind Tailscale) AND consumer-account Workspace Events support (not currently available). See §10.

---

## 4. Direction & Aggregation Model

### 4.1 Participant & direction rule

Let `M` = the set of normalized identities representing the connected account's `users/me` — primarily the account's own email (verified via `users.me.lookup` on connect, cached per-account in `external_sync_state.metadata.me_identities`). For a chat message with sender `f` (a `users/<id>` reference) and the message's space `S`:

- **`f ∈ M`** → message is **outbound** from this connected account.
- **`f` resolves to a known contact `C` (via the dual-source identity map)** → message is **inbound** for `C`.
- **`f` resolves to neither M nor any known contact** → bystander, no row produced. The space's other members (whoever they are) are NOT separately attributed; this is the same rule Telegram applies (`.ai/spec/telegram-integration.md` §3 — `direction` is per-message, sender-only).

For DMs (`DIRECT_MESSAGE`, exactly two members), the peer is the other member; every message qualifies (one direction or the other). For `GROUP_CHAT` and `SPACE`, a message qualifies only when the sender is in `M ∪ knownIdentities` — co-members who are CRM contacts but did not send this message are bystanders for the cadence-attribution purpose.

Practical effect: a group space with three known members produces interactions only for messages they actually send. Their being co-present does not produce phantom **inbound** rows. This matches Telegram's inbound-group rule verbatim (`backend/internal/telegram/message.go:99-126`). The outbound-side rule is different — see §4.3 for the per-known-co-member fan-out on outbound, which is the Gmail per-participant pattern, NOT Telegram's.

### 4.2 Aggregation key & engine (adopted from Telegram, NOT Gmail)

GChat plugs into the shared aggregation engine at `backend/internal/messaging/aggregation` — the same engine Telegram (`backend/internal/telegram/aggregation.go:60-102`) and Messages (`backend/internal/messages/aggregation_adapter.go:34-73`) use today.

**Adapter contract per `SourceAdapter` (`backend/internal/messaging/aggregation/interfaces.go:44-81`):**

- `SourceName()` → `"gchat"`
- `SourceRef(chatID, firstExternalID string)` → `"gchat:<space_resource>:<first_message_resource>"`. The space resource name is opaque (e.g. `spaces/AAAA9876`) but does not legitimately contain `_` or `%`, so no LIKE-escape is required — same posture Telegram takes (Telegram's `_`/`%` escape concern only applies to source adapters whose chat IDs may contain those characters; Apple Messages requires escaping per `backend/internal/messages/aggregation_adapter.go:107-114`).
- `SourceRefPrefix(chatID string)` → `"gchat:<space_resource>:%"`. Confirm at implementation time that Chat space resource names never embed `_`/`%`; if they ever do, adopt the Messages-style escape (recommendation: defensively apply the same `strings.NewReplacer` escape regardless — see §5.X.4).
- `PeerRef(chatID string)` → `"gchat:<space_resource>"`
- `Description(direction string, msgCount int)` → `"GChat outreach (3 messages)"` / `"GChat response (2 messages)"` / `"GChat exchange (...)"`, mirroring Telegram's phrasing verbatim per the existing pattern at `backend/internal/telegram/aggregation.go:146-155`.

**Burst + reply-bridge:** initial values match Telegram (`burstWindowHours=2`, `replyBridgeHours=48`). These are tunable per env var (§7). The aggregation engine handles burst grouping (same-direction messages within 2h form one burst), reply-bridging (inbound burst within 48h after an outbound burst promotes to mutual), and explicit reply detection (Chat API exposes `message.thread.name` and `message.threadReply` — see §5.X.3 for an open question on whether to wire explicit reply bridging through `ReplyTargetID`).

**Why NOT Gmail's `(contact, thread, day)` shape:** Email threads have explicit `In-Reply-To` headers and natural daily boundaries (people don't send 50 emails in 10 minutes). Chat is interactive — a 30-message back-and-forth across 90 minutes IS one conversation, not a thread-day bucket. Telegram's burst+bridge collapses this correctly; thread-day would create 30 same-bucket interactions that all extend each other (a denial-of-service against the consumer's per-source-ref advisory lock, plus a misleading 30-message description). The shared aggregator already solves this and is proven on two sources.

### 4.3 Bystanders, fan-out, and the inbound/outbound asymmetry

A message whose sender is neither me nor a known contact is NOT stored. The interesting case is outbound from me to a group that contains multiple known contacts.

**Decision: outbound fan-out across the set of known co-members; inbound stays sender-only.** Concretely:

- **Inbound** from a known sender (e.g. Alice) in a group that also has Bob and Carol as known co-members → ONE row keyed on `matched_contact_id=Alice`. Bob and Carol do not get phantom rows. This matches Telegram's sender-only attribution: for `PeerChat` messages, Telegram sets `parsed.PeerUserID = &senderID` only when the sender is not self (`backend/internal/telegram/message.go:99-126`), and stores one row per (message, sender).
- **Outbound** from me to a group with two known contacts (Alice and Bob) → TWO rows: `matched_contact_id=Alice` and `matched_contact_id=Bob`. Both rows reference the same chat message resource name and share provenance via `source_metadata.observed_accounts[]`. Each row contributes to that contact's `last_outreach_at` independently.

**Precedent for outbound fan-out: Gmail, not Telegram.** The `comms_message` schema is explicitly designed for per-participant granularity — migration 058 line 4 states *"One row = one message x one qualifying contact (per-participant granularity)"* (`backend/migrations/058_comms_message.up.sql:4`). Gmail's provider implements this directly: every qualifying `(message, contact)` pair produces a distinct row and a distinct event, with `SourceID = row.ExternalID + ":" + row.ContactID.String()` (`backend/internal/google/gmail.go:872-895`). The `idx_comms_message_dedup` unique index on `(source, external_id, matched_contact_id)` (`backend/migrations/058_comms_message.up.sql:31-33`) is what makes this safe.

**Telegram does NOT fan out across known co-members** — explicitly verified. For an outbound `PeerChat` message (`senderID == selfUserID`), Telegram leaves `parsed.PeerUserID = nil` (`backend/internal/telegram/message.go:114-115`) — there is no per-known-co-member row at all on the outbound side; outbound updates the chat scope, not each co-member's view. The prior draft incorrectly claimed Telegram precedent here.

**Why the asymmetry is right, not a bug:**

- For inbound, sender-only attribution is correct because the cadence semantics ("this person reached out to me") only apply to the actual sender. Bob and Carol being present doesn't mean they reached out.
- For outbound, fan-out across the known set is correct because the cadence semantics ("I reached out to this person") apply to every known recipient. Outbound to a group containing Alice and Bob legitimately advances both `last_outreach_at(Alice)` and `last_outreach_at(Bob)` — the user did reach out to both.

This is an **owned design choice** that follows the Gmail per-participant precedent, not a borrowed Telegram pattern.

### 4.4 Cadence

No new cadence logic. Inbound/mutual → `last_contacted` + `last_interaction_at` + `last_response_at` + recalculates `contact_by`. Outbound → only `last_outreach_at`. The aggregation engine emits `interaction.recorded` on create and applies cadence inline on extend/promote, exactly as Telegram and Gmail already do — both delivery paths are exercised by the engine today.

**Telegram cadence semantics verification:** Telegram's `interaction.recorded` on create + inline extend/promote matches Gmail's two-path cadence model (Gmail spec §4.3). They are identical because both go through the same `RecordInteractionTx` and the same `ExtendInteractionTx` / `PromoteInteractionToMutualTx` primitives. GChat will go through the same shared engine and the same primitives. Confirmed; no deviation.

---

## 5. Technical Specification

### 5.1 Components

| Component | Location (new unless noted) | Responsibility |
|-----------|------------------------------|----------------|
| `GChatSyncProvider` | `backend/internal/google/gchat.go` | Implements `sync.SyncProvider`; enumerates spaces, resolves membership, pages messages per cursor window, resolves senders against the dual-source identity map, writes `comms_message` content rows directly (with provenance merge on conflict). **Does NOT publish per-message events** — see §5.2. Edits and deletes are reflected by UPDATE / soft-delete on the existing `comms_message` row in the same provider sweep |
| `GChatHandleRematchHandler` | `backend/internal/google/gchat_rematch.go` | Implements `service.RematchHandler` with `IdentifierType()="gchat"`. Identifier-scoped backfill on `contact_methods.added` for `gchat` methods. Calls the shared `aggregation.Engine.AggregateForContactBatch` after linking, same shape as `peerRematchBase.rematchPeers` (`backend/internal/telegram/rematch.go:35-70`) |
| `GChatEmailRematchHandler` | `backend/internal/google/gchat_rematch.go` | Implements `service.RematchHandler` with `IdentifierType()="email"`. Identifier-scoped backfill for the email half of the dual-source rule. Co-registers with `GmailRematchHandler` and `CalendarRematchHandler` (multi-handler-per-type is already supported — `backend/internal/service/rematch.go:180-183`) |
| `commsMessageStoreAdapter` | `backend/internal/google/gchat_aggregation.go` | NEW adapter implementing `aggregation.MessageStore` against `comms_message` rows filtered by `source='gchat'`. Mirror of `telegramMessageStoreAdapter` (`backend/internal/telegram/aggregation.go:161-256`) / `messagesMessageStoreAdapter` (`backend/internal/messages/aggregation_adapter.go:141-220`). Reads/writes `claimed_at` / `claimed_session_ref` / `processed_at` / `interaction_id` directly on `comms_message` (the columns are already reserved on the schema — `backend/migrations/058_comms_message.up.sql:22-25`) |
| `gchatAdapter` | `backend/internal/google/gchat_aggregation.go` | `SourceAdapter` impl (name, source_ref, prefix, peer_ref, description). Mirror of `messagesAdapter` (`backend/internal/messages/aggregation_adapter.go:82-135`) |
| GChat `AggregatorReenqueuer` registry entry | `backend/cmd/crm-api/main.go` (modify) | Register an `AggregatorReenqueuer` for `source="gchat"` (`backend/internal/consumer/aggregator_reenqueuer.go:30-56`) so straggler-message reenqueue from the engine's own `KindMessageReceived`/`KindMessageSent` publish path drives back to `engine.AggregateForContact(contactID, chatID)` — same hook Telegram uses (`backend/internal/consumer/aggregator_reenqueuer.go:68-80`) |
| OAuth scopes | `backend/internal/google/oauth.go:26` (modify) | Append `chat.spaces.readonly` and `chat.messages.readonly` to `Scopes` |
| Identity dual-source query | `backend/internal/db/queries/contact_method.sql` (extend) | New `ListGChatIdentitiesForSync` returning `(value_normalized, contact_id, source_type)` over rows where `type IN ('gchat', 'email')`. The `source_type` discriminator lets the provider log which side of the dual-source matched, helpful for the §10 cleanup question |
| Repository methods on `comms_message` | `backend/internal/repository/comms_message.go` (extend) | `ListUnprocessedContactIDs`, `ListUnprocessedByContact`, `ListUnprocessedByContactAndChat`, `GetMessageByReplyTarget`, `MarkMessagesProcessed`, `ClaimMessagesTx`, `ClearStaleClaimTx` — all source-parameterized (so the same methods serve Telegram/Messages after their later migration onto `comms_message`) |
| New sqlc queries | `backend/internal/db/queries/comms_message.sql` (extend) | Mirrors the existing telegram/messages queries but scoped by `source` parameter |
| `InteractionSourceGChat` constant | `backend/internal/repository/interaction.go` (modify) | `InteractionSourceGChat = "gchat"` |
| Source CHECK migration | `backend/migrations/06X_interaction_source_gchat.up.sql` (new) | Add `'gchat'` to `interaction.source` CHECK. Same migration also tightens the `comms_message.thread_id` column COMMENT to document chat-source reuse (P1-7 — see §6.2). Pair with constant update in the SAME PR — pattern locked in by `.ai/spec/2026-06-01-gmail-integration-design.md` §6.2 |
| Registration | `backend/cmd/crm-api/main.go` (modify) | Register provider, both rematch handlers, GChat `AggregatorReenqueuer` entry, GChat entry in the `PerSourceChatListerRegistry` (`backend/internal/scheduler/messaging_aggregate_worker.go:39-61`); reconcile-on-boot `external_sync_state(source='gchat')` row per Google credential |

**Notably ABSENT from the components list (compared to the prior draft):**

- No `GChatInteractionConsumer` river worker — the shared `aggregation.Engine` already publishes the events the rest of the system needs.
- No new event kinds (`KindGChatReceived`/`KindGChatSent`/`KindGChatEdited`/`KindGChatDeleted`). The engine emits the generic `KindMessageReceived`/`KindMessageSent` (`backend/internal/events/kinds.go:19-20`, used by the engine at `backend/internal/messaging/aggregation/engine.go:489-545`) with `Source = "gchat"` set from the adapter's `SourceName()`. Cadence and follow-up consumers already route on those generic kinds.
- No `consumerJobsForKind` routing addition. The only addition to `events/bus.go` is whatever per-source plumbing the existing `KindMessage*` consumers already do — verified at implementation time, but no new fan-out needed.
- No audit-only `gchat.edited` / `gchat.deleted` events. Edits and deletes are recoverable from `comms_message.source_metadata.edited_at` / `comms_message.deleted_at` directly; an audit-only event with no consumer would be dead weight.

### 5.2 Data flow (per qualifying message)

The provider writes content rows directly to `comms_message` — no per-message events. The shared aggregation engine drives all downstream signals via its existing `KindMessageReceived`/`KindMessageSent` publication path. This is the Telegram/Messages pattern, NOT Gmail's per-message publish-before-mutate pattern. The rationale is in §1's "Pipeline" decision row.

```
GChatSyncProvider.Sync(account)                       [per space, per message, per qualifying contact]
  └─ tx (one tx per qualifying (message, contact) row):
       upsert comms_message ON CONFLICT (source='gchat', external_id, matched_contact_id)
              DO UPDATE: merge provenance by SET-UNION on source_metadata.observed_accounts[]
                         (content fields immutable on conflict; same merge rule Gmail §5.4 uses)
     commit
       └─ no event published from the provider — the engine drives downstream signal on its next pass

  Replay from another account: the ON CONFLICT path merges provenance into the existing row;
  no extra row is created. The aggregation pass that already processed the original message
  does not reprocess (idempotent via processed_at IS NULL filter on the store adapter).

aggregation.Engine (existing, triggered by:                 [periodic sweeper tick
                    - the periodic `MessagingAggregateSweeperWorker` tick                              OR contact-driven enqueue
                      (`backend/internal/scheduler/messaging_aggregate_sweeper_worker.go:30`),         OR rematch handler]
                      which lists distinct unprocessed-row contact IDs per source via the
                      `UnprocessedContactLister` registry and enqueues a
                      `MessagingAggregateForContactArgs` per contact
                    - the existing `AggregatorReenqueuer` post-engine hook
                    - `RematchHandler.Rematch` after a `gchat`/`email` method is linked)
  └─ For source="gchat":
       ├─ ListUnprocessedByContactAndChat reads pending comms_message rows (source='gchat', processed_at IS NULL)
       ├─ Burst-groups within window
       ├─ Reply-bridges across the 48h window OR via explicit reply (§5.X.3 open question)
       ├─ ClaimRowsTx + PublishTx(KindMessageReceived | KindMessageSent, source="gchat") inside one tx
       │  (atomic claim+publish — same race-safety mechanism Telegram and Messages already use;
       │   `backend/internal/messaging/aggregation/engine.go:489-545`)
       └─ MarkMessagesProcessed on commit; clears claim, sets interaction_id
```

Content lives only in `comms_message`. The events published by the engine are the generic source-agnostic `KindMessageReceived`/`KindMessageSent` already emitted today; the `Source` field on the envelope distinguishes `"gchat"` from `"telegram"` / `"messages"`. The aggregation engine's existing race-mechanics (claim, stale-claim recovery, boundary-shift detection — `backend/internal/messaging/aggregation/engine.go`) are inherited unchanged.

**Edit handling (no events):**

```
GChatSyncProvider observes message via the edit-cursor mechanism (§3.1 step 8, PROVISIONAL)
  └─ tx:
       fetch message from API
       if existing comms_message row + body differs:
           UPDATE comms_message
             SET body = new, snippet = recomputed,
                 source_metadata = jsonb_set(source_metadata, '{edited_at}', now)
                                   plus push previous body onto source_metadata.previous_bodies[] (last-3 cap)
     commit

The aggregation engine does NOT reprocess: rows are filtered by `processed_at IS NULL`, so an
edited row already at processed_at != NULL is invisible to the next engine pass. The derived
interaction is unchanged — consistent with the locked edit-semantics row in §1.
```

**Delete handling (no events):**

```
GChatSyncProvider observes 404 / tombstone via the edit-cursor mechanism or a targeted messages.get
  └─ tx:
       UPDATE comms_message SET deleted_at = now
     commit

The aggregation engine ignores soft-deleted rows. The derived interaction is unchanged — consistent
with the locked delete-semantics row in §1.
```

### 5.3 Known-contact enforcement (defence in depth)

1. **Map-level**: dual-source map at sweep start. Unmatched sender → not stored.
2. **Match-level**: participant resolution is a lookup; no `MatchOrCreate` discovery path. Consistent with Gmail/Telegram "match-or-skip."
3. **Space-level**: a space with no known members is skipped at enumeration time — saves API budget and avoids leaking unrelated names into logs.

### 5.4 Cross-account dedup

Same shape as Gmail: `(source='gchat', external_id=<chat_message_resource>, matched_contact_id)` UNIQUE on `WHERE deleted_at IS NULL`. The Chat message resource name (`spaces/<space>/messages/<id>`) is cross-account-stable for a given space, so the same message observed in two connected accounts (e.g., two accounts both members of the same group) targets the same `comms_message` row per contact. Provenance is merged by set-union on `source_metadata.observed_accounts[]`, mirroring Gmail's §5.4 logic.

There is no "missing message-id fallback" case to handle — Chat API always returns a resource name. The Gmail-style `nomsgid:<account>:<gmail_id>` synthesis is not needed.

### 5.5 Shared-Logic Refactor Opportunities

Side-by-side analysis of Telegram, Messages (iMessage), and Gmail surfaced the following duplicated logic. Each is marked PROPOSED (worth bundling with GChat) or RECOMMENDED (worth doing as part of GChat because GChat would otherwise force a third copy) or FUTURE (worth doing but not blocking GChat).

#### 5.X.1 (RECOMMENDED with GChat) Reuse the shared `messaging/aggregation` engine via a new `comms_message`-backed `MessageStore` adapter

**Status quo.** The shared engine already abstracts the burst/bridge/claim logic via `aggregation.SourceAdapter` + `aggregation.MessageStore` (`backend/internal/messaging/aggregation/interfaces.go:44-136`). Two adapters exist today: `telegramMessageStoreAdapter` (`backend/internal/telegram/aggregation.go:161-256`) and `messagesMessageStoreAdapter` (`backend/internal/messages/aggregation_adapter.go:141-220`). Both read source-specific staging tables (`telegram_message`, `messages_message`).

**The new wrinkle.** GChat is the FIRST chat source to use `comms_message` instead of its own staging table. The `comms_message` schema already reserves the claim columns (`backend/migrations/058_comms_message.up.sql:22-25`) precisely for this. So we need a `commsMessageStoreAdapter` that:

- Reads `comms_message` rows where `source = 'gchat'` (parameterized so it's reusable after Telegram/Messages later migrate onto `comms_message`)
- Projects `comms_message` rows into `aggregation.Message` (ID, ChatID = thread_id which carries the space resource name for chat sources, IsOutgoing = direction == 'outbound', SentAt, ExternalID = external_id, InteractionID, ClaimedAt, ClaimedSessionRef, ReplyTargetID from `source_metadata.reply_target` if wired)
- Implements all eight `MessageStore` methods against `comms_message` (with corresponding sqlc queries scoped by `source` parameter)

**Recommendation: RECOMMENDED with GChat.** The adapter must be written for GChat anyway; making it source-parameterized from day one costs nothing extra and gives Telegram/Messages a migration target later. The source-parameterized sqlc queries are no more complex than the per-source ones — they just take `source = sqlc.arg(source)` instead of being hard-coded.

**Cited code references:**
- `backend/internal/telegram/aggregation.go:161-256` (telegram store adapter)
- `backend/internal/messages/aggregation_adapter.go:141-220` (messages store adapter)
- `backend/migrations/058_comms_message.up.sql:22-25` (reserved claim columns)
- `backend/internal/messaging/aggregation/interfaces.go:98-136` (MessageStore contract)

#### 5.X.2 (PROPOSED — future cleanup) Extract dual-source rematch helpers

**Status quo.** Telegram has two rematch handlers (`UsernameRematchHandler`, `PhoneRematchHandler`) sharing a `peerRematchBase` (`backend/internal/telegram/rematch.go:21-70`). The shared base owns the iteration / link / aggregate loop. The two handlers differ only in which sqlc lookup (`FindDistinctUnmatchedPeerUserIDsByUsername` vs `FindDistinctUnmatchedPeerUserIDsByPhone`) and how the value is normalized (raw vs digits-only).

GChat introduces ANOTHER dual-source pair (`GChatHandleRematchHandler` for `gchat` type + `GChatEmailRematchHandler` for `email` type). Their bodies will be 95% identical: both call `ScanIdentifier` on the provider for the new value, both iterate over enabled-`gchat` accounts.

**Recommendation: PROPOSED — future cleanup.** The two GChat handlers are already simple enough that a custom abstraction at this point would add more types than it removes (the value normalization differs: `gchat` and `email` both normalize via the same `matching.NormalizeEmail` function, so the "differ only in normalization" axis isn't even there for GChat). After GChat lands, a generic `IdentifierFanoutRematchHandler[Provider, Normalizer]` could collapse `GmailRematchHandler` + `GChatEmailRematchHandler` + the `GChatHandleRematchHandler` + the calendar email handler into a single parameterized factory. NOT a blocker for GChat.

**Cited code references:**
- `backend/internal/telegram/rematch.go:21-122` (telegram base + two handlers)
- `backend/internal/google/gmail_rematch.go:32-136` (gmail's email rematch handler)

#### 5.X.3 (PROPOSED) Wire explicit-reply bridging via `ReplyTargetID` for chat sources

**Status quo.** The `aggregation.Message.ReplyTargetID` field carries the source-defined "external message id of the referenced message" (`backend/internal/messaging/aggregation/interfaces.go:34`). Telegram populates it from `reply_to_msg_id` (`backend/internal/telegram/aggregation.go:251-254`); Messages populates it from `reply_to_guid` (`backend/internal/messages/aggregation_adapter.go:214-217`). The engine uses it in `tryExplicitReplyBridge` to bridge across the time window when an inbound message explicitly replies to an outbound message.

**GChat option.** Chat API exposes `message.threadReply: true` and `message.thread.name`, and replies in threads can be addressed via `message.quotedMessageMetadata` (limited availability). The simplest mapping: when a message is a `threadReply` AND the thread head is in scope, use the thread head's resource name as `ReplyTargetID`. The schema does not have a direct column on `comms_message` for this — it would live in `source_metadata.reply_target_external_id`. The store adapter would project it into `aggregation.Message.ReplyTargetID`.

**Recommendation: PROPOSED for the MVP, leaning toward defer.** Burst+time-window bridging already handles the common case (an inbound burst within 48h after an outbound burst promotes to mutual). Explicit-reply bridging is only valuable when a delayed reply (>48h) is unambiguously tied to a specific earlier message. For chat, the bursts are tight enough that the time window almost always covers it. Decision deferred to implementation: if the project setup ergonomics for `quotedMessageMetadata` are clean, wire it; if not, defer to a follow-up. NOT a blocker.

**Cited code references:**
- `backend/internal/messaging/aggregation/interfaces.go:34` (ReplyTargetID contract)
- `backend/internal/telegram/aggregation.go:251-254` (telegram's reply-target mapping)
- `backend/internal/messages/aggregation_adapter.go:214-217` (messages' reply-target mapping)

#### 5.X.4 (PROPOSED — defensive) Apply LIKE-escape on `SourceRefPrefix` for all chat sources

**Status quo.** Telegram's chat IDs are numeric int64 → escape is unnecessary (`backend/internal/telegram/aggregation.go:138-140`). Messages' chat.guid contains `_` → escape is mandatory (`backend/internal/messages/aggregation_adapter.go:107-114`). The `SourceAdapter` contract explicitly notes adapters that may contain `_`/`%` MUST escape. The compliance is per-adapter today.

**GChat consideration.** Chat space resource names like `spaces/AAAA9876` do not legitimately contain `_` or `%` today. But the format is opaque and could change. The Messages-style escape is one `strings.NewReplacer` call — negligible cost.

**Recommendation: PROPOSED — defensive.** Apply the escape unconditionally in `gchatAdapter.SourceRefPrefix`. Mirror the Messages adapter line-for-line. Costs nothing; future-proofs against Chat resource name format drift.

**Cited code references:**
- `backend/internal/messages/aggregation_adapter.go:107-114` (escape implementation)
- `backend/internal/messaging/aggregation/interfaces.go:58-68` (contract requiring escape)

#### 5.X.5 (PROPOSED — future cleanup) Share the Gmail / GChat "scan a window per account, advance cursor on success" plumbing

**Status quo.** Gmail's `Sync` (`backend/internal/google/gmail.go`) implements per-account cursor windowing, max-windows-per-sync budget, safety lag, and rate-limit handling. GChat will need the same — per-space (not per-account) cursor windowing, with a `gchatMaxWindowsPerSpace`/`gchatMaxWindowsPerSync` budget and a `gchatSearchSafetyLag` analog. The logic is structurally similar but scoped differently.

**Recommendation: PROPOSED — future cleanup, after both providers are stable.** The plumbing isn't quite identical enough to extract today (per-account-windowed vs per-space-windowed), and extracting before we know what the GChat-specific edge cases look like would be premature. Re-evaluate after MVP rollout: if there are 2-3 follow-on chat sources (WhatsApp, Slack), extract a `windowedSync` helper at that point. NOT a blocker.

**Cited code references:**
- `backend/internal/google/gmail.go:35-75` (Gmail's window constants)

#### 5.X.6 (NOT recommended) Generalize Gmail's `BuildMethodsFromExternal` to populate gchat from email

**Status quo.** `BuildMethodsFromExternal` in `backend/internal/service/external_methods.go` is the shared helper that converts an `ExternalContact` into `ContactMethodInput`s. It has special-case branches for `Source == "telegram"` (extract `metadata.username`). GChat could in principle add a branch reading `metadata.gchat_address`.

**Recommendation: NOT recommended.** GChat is not a discovery source — there are no `external_contact` rows for GChat. The helper is only invoked on imports, and GChat does no imports. Adding a branch for a source that never feeds the helper would be dead code.

#### 5.X.7 (DEFERRED) `IdentifierTypeGChat` constant — keep; multi-mapping in `MapIdentifierTypeToContactMethodTypes` — NOT added in MVP

**Status quo.** `backend/internal/identity/normalize.go:90-107` maps an `IdentifierType` to the set of `contact_method.type` values it should match against. **The actual one-to-many ("dual-source") precedent in this file is WhatsApp**, not iMessage. WhatsApp at `backend/internal/identity/normalize.go:102-103` is the ONLY existing mapping that returns multiple contact-method types: `IdentifierTypeWhatsApp` → `[ContactMethodTypeWhatsApp, ContactMethodTypePhone]`. iMessage and Telegram each map to a SINGLE type:

- `IdentifierTypeIMessageEmail` → `[ContactMethodTypeEmail]` (`backend/internal/identity/normalize.go:98-99`) — single
- `IdentifierTypeIMessagePhone` → `[ContactMethodTypePhone]` (`backend/internal/identity/normalize.go:100-101`) — single
- `IdentifierTypeTelegram` → `[ContactMethodTypeTelegram]` (`backend/internal/identity/normalize.go:96-97`) — single

iMessage and Telegram both achieve their "dual-source" effect by having TWO IdentifierType values (e.g. `IdentifierTypeIMessageEmail` + `IdentifierTypeIMessagePhone`) routed to from different rematch handlers — that's the rematch-handler-layer dual-source pattern (§1, identity-matching decision row). WhatsApp is the only existing one-IdentifierType-many-ContactMethodType ("sender-resolution dual-source") precedent.

**What this spec keeps and what it drops.**

- **Keep:** add `IdentifierTypeGChat IdentifierType = "gchat"` (constant) and `ContactMethodTypeGChat = "gchat"` (constant). Normalization: `Normalize(raw, IdentifierTypeGChat)` delegates to `normalizeEmail` (since GChat sender addresses ARE emails). These are cheap, harmless, and let future callers reference the right enum value.
- **Drop from MVP:** the proposed `MapIdentifierTypeToContactMethodTypes(IdentifierTypeGChat) → [ContactMethodTypeGChat, ContactMethodTypeEmail]` multi-mapping. Adding it now would be **dead code** — the sweep-time identity map is built directly from the sqlc query `ListGChatIdentitiesForSync` (filter `WHERE type IN ('gchat', 'email')`), and **no caller invokes `IdentityService.MatchOrCreate(IdentifierTypeGChat, ...)`** in MVP. Two encodings of the same `(gchat, email)` set (one in `normalize.go`, one in the sqlc query) could drift independently. Until an actual `MatchOrCreate(IdentifierTypeGChat, ...)` caller emerges, the sqlc query is the **single source of truth** for the dual-source set.

**Recommendation: DEFERRED.** Re-evaluate if/when a code path needs sender-resolution dual-source via the identity-mapping layer. Until then, the WhatsApp precedent at `backend/internal/identity/normalize.go:102-103` documents the shape that addition would take.

**This is distinct from the backfill-trigger dual-source** (the two RematchHandler registrations described in §1 and the Telegram precedent at `backend/internal/telegram/rematch.go:74-95` + `backend/internal/telegram/rematch.go:97-121`), which IS added in MVP. The two mechanisms cover non-overlapping triggers (see §1's identity-matching row); the deferred mapping addition would cover sender resolution at sweep time, the two RematchHandlers cover contact-method-added at link time. Neither subsumes the other.

**Cited code references:**
- `backend/internal/identity/normalize.go:15-31` (IdentifierType enum)
- `backend/internal/identity/normalize.go:36-41` (ContactMethodType enum)
- `backend/internal/identity/normalize.go:87-107` (mapping helper — WhatsApp at lines 102-103 is the precedent shape)

### 5.6 Why this isn't `gchat_message`

Earlier integrations (Telegram, Messages) each added a dedicated staging table. The decision NOT to do that for GChat is deliberate:

1. The `comms_message` schema explicitly **reserves the claim columns** for chat sources (`backend/migrations/058_comms_message.up.sql:22-25`) and the comments in 058 call out future telegram/messages migration as planned.
2. Adding `gchat_message` would mean a third parallel adapter that's structurally identical to the comms-backed one and would need migrating onto `comms_message` later anyway.
3. The source-parameterized adapter is no more work than a source-specific one and gives Telegram/Messages a proven landing pad.

This is RECOMMENDED, and §5.X.1 captures it explicitly.

---

## 6. Database Changes

### 6.1 `interaction.source` CHECK constraint

Add `'gchat'` to the allowed `source` values. Mirror Gmail's 059 migration verbatim:

| Migration | Action |
|-----------|--------|
| New `06X_interaction_source_gchat.up.sql` | `DROP CONSTRAINT interaction_source_check` then `ADD CONSTRAINT ... CHECK (source IN ('manual','gcal','todoist','telegram','messages','anarlog_sessions','phone_calls','email','gchat'))` |
| Constants | Add `InteractionSourceGChat = "gchat"` to `backend/internal/repository/interaction.go` in the SAME PR |
| Source-check integration test | Update `backend/tests/interaction_source_descriptor_check_test.go` to assert the new value (test enforces the Go constants equal the live CHECK set) |

### 6.2 `comms_message` no schema change, but `thread_id` column comment broadens

The schema already permits `source='gchat'` (it's `TEXT NOT NULL` with no CHECK — see `backend/migrations/058_comms_message.up.sql:8`). The reserved `claimed_at` / `claimed_session_ref` columns activate for the first time under GChat's aggregation path. No structural migration needed.

**Column-comment broadening for `thread_id`.** The original 058 migration's inline SQL comment scopes `thread_id` to email: *"thread_id TEXT, -- email: Gmail threadId"* (`backend/migrations/058_comms_message.up.sql:10`). GChat reuses the column to carry the **space resource name** (`spaces/<id>`). The inline comment in migration 058 is immutable history — to broaden the documented semantics, the same migration that adds `'gchat'` to `interaction.source` CHECK (§6.1) also issues an explicit `COMMENT ON COLUMN comms_message.thread_id IS '...'` statement that documents the dual semantics:

```sql
COMMENT ON COLUMN comms_message.thread_id IS
  'email: Gmail threadId; gchat/telegram/messages: space/chat scope resource name';
```

`COMMENT ON COLUMN` survives in the live catalog (unlike inline SQL comments) and is the canonical documentation surface from this migration forward.

### 6.3 sqlc queries

New queries to add to `backend/internal/db/queries/comms_message.sql` (source-parameterized so they serve both GChat today and Telegram/Messages on later migration):

- `ListUnprocessedCommsContactIDs(source)`
- `ListUnprocessedCommsByContact(source, contact_id)`
- `ListUnprocessedCommsByContactAndChat(source, contact_id, chat_id)`
- `GetCommsMessageByReplyTarget(source, chat_id, reply_target_id)`
- `MarkCommsMessagesProcessed(message_ids, interaction_id)`
- `ClaimCommsMessagesTx(message_ids, session_ref)`
- `ClearStaleCommsClaimTx(message_ids, expected_session_ref)`

**All new queries MUST scope to `deleted_at IS NULL`** (per the soft-delete invariant), and all unprocessed-row readers (`ListUnprocessedCommsContactIDs`, `ListUnprocessedCommsByContact`, `ListUnprocessedCommsByContactAndChat`) MUST additionally scope to `processed_at IS NULL`. This mirrors the existing pattern in the same file — see e.g. `GetCommsMessage` (`backend/internal/db/queries/comms_message.sql:96-100`) for the `deleted_at IS NULL` precedent and `MarkCommsMessagesProcessed` (`backend/internal/db/queries/comms_message.sql:122-127`) for the combined `processed_at IS NULL AND deleted_at IS NULL` precedent. The edit/delete semantics in §1 and §5.2 depend on this: edited rows have `processed_at IS NOT NULL` and are correctly skipped; soft-deleted rows have `deleted_at IS NOT NULL` and are correctly skipped. Without these filters the engine would reprocess edits and resurrect deleted messages — violating the locked edit/delete row in §1.

New query for the dual-source identity map:

- `ListGChatIdentitiesForSync` — returns `(value_normalized, contact_id, source_type)` for rows where `type IN ('gchat', 'email')` AND contact is non-deleted. Reuses the existing trigger-normalized `value_normalized` column. The `source_type` discriminator lets the provider log which side matched (debug-level only — never with the value).

### 6.4 `external_sync_state` no schema change

`external_sync_state.source` is free-text (no CHECK — `backend/migrations/011_external_sync.up.sql:7`). One row per `(source='gchat', account_id)`; `strategy='contact_driven'` (same as Gmail); `metadata` carries the per-space cursor map (§3.3), the membership cache (§3.4), the `backfill_since` anchor, and the `me_identities` cache.

### 6.5 `interaction.source` constants update + test

Per the pattern locked in by Gmail spec §6.2: the migration AND the constant addition AND the source-check test update land together. Anything less leaves the Go-vs-DB invariant cracked.

---

## 7. Configuration

| Setting | Default | Where |
|---------|---------|-------|
| Sync interval | 15 min | `SourceConfig.DefaultInterval` |
| Backfill start date | 2026-01-01 | `external_sync_state.metadata.backfill_since` per account |
| Burst window | 2 h | `GCHAT_BURST_WINDOW_HOURS` env var (analog of Telegram's `TELEGRAM_BURST_WINDOW_HOURS`) |
| Reply bridge | 48 h | `GCHAT_REPLY_BRIDGE_HOURS` env var |
| Max windows per sync | 24 | provider constant (analog of `gmailMaxWindowsPerSync`) |
| Membership cache TTL | 24 h | provider constant `gchatMembershipCacheTTL` |
| Enablement | reconciliation creates/enables a `gchat` state per Google credential | **not** auto-provided by the scheduler — same loop the Gmail spec adds in its §7 |

**OAuth scopes added at `backend/internal/google/oauth.go:26`:**

```go
var Scopes = []string{
    "openid",
    "email",
    "profile",
    gmail.GmailReadonlyScope,
    calendar.CalendarReadonlyScope,
    people.ContactsReadonlyScope,
    // NEW: chat scopes — one-time re-consent required on existing accounts
    "https://www.googleapis.com/auth/chat.spaces.readonly",
    "https://www.googleapis.com/auth/chat.messages.readonly",
}
```

A one-time re-consent prompt is required for already-connected accounts to pick up the new scopes — surface a reconnect indicator in the settings UI when an account is missing them.

**Workspace-account sender resolution may need People API.** For consumer `@gmail.com` accounts (the MVP target — see §10 Q4), `message.sender.name` resolves to the user's email address directly. For Workspace senders, the `users/<id>` form may need a `people.get` lookup to map id → email. The `people.ContactsReadonlyScope` is already granted (`backend/internal/google/oauth.go` Scopes list, line `people.ContactsReadonlyScope`) for Calendar attendee resolution, so no new scope is required — but the call is added to the People API quota. People API daily quota for the People service is a known-unknown; rough order of magnitude is in the tens of thousands of requests/day per project, but the exact number must be verified against the GCP console during implementation. If Workspace lookups become quota-dominant, cache by `users/<id>` with the same TTL as `gchatMembershipCacheTTL`.

**New Go API dependency:** `google.golang.org/api/chat/v1`.

**Operator setup (one-time, in the GCP project):**

1. Open the Chat API Configuration page for the GCP project.
2. Configure the Chat App: name, avatar URL, description. **Disable** Interactive features (no slash commands, no webhooks).
3. Verify the personal `@gmail.com` user is a Test User on the External-type consent screen (already done per the OAuth probe).
4. Confirm `spaces.list` returns data via OAuth Playground before enabling the feature flag in the Pi.

**No new env vars beyond the burst/reply tuning above.** OAuth credentials are shared with Gmail/Calendar — `GOOGLE_OAUTH_CLIENT_ID` / `GOOGLE_OAUTH_CLIENT_SECRET` are already in the env.

---

## 8. Implementation Phases

The implementation is sequenced as **three PRs** for review hygiene and bisectability — NOT as a staged-deploy risk strategy. All three PRs may safely deploy together; the GChat integration is **opt-in by construction** and remains inert until two operator-side prerequisites are satisfied: (a) the one-time GCP Chat App configuration (§7) and (b) OAuth consent for the new `chat.spaces.readonly` + `chat.messages.readonly` scopes on at least one connected Google account. Without those, no GChat code path executes — there are no public webhooks, no scheduled jobs that fire without an enabled `external_sync_state(source='gchat')` row, and no rematch fan-out without a `gchat` or `email` method linked to a contact. Deployment risk to the rest of the system (single-user, Pi-hosted, Tailscale-fronted) is bounded to the schema migration in PR 1, which is additive only (one `CHECK` constraint enum value + one `COMMENT ON COLUMN`).

The §5.X refactor opportunities marked RECOMMENDED are woven into the PRs below; PROPOSED-only items remain as a tail. The phase ordering is a recommended merge sequence, not a deploy-staging requirement.

### Phase 1: Schema + identity + comms_message-backed aggregation engine wiring

**Inert until:** Phase 3 wires enablement and the user completes the Chat App config + OAuth consent.

- New migration adding `'gchat'` to `interaction.source` CHECK; constant `InteractionSourceGChat`; source-check integration-test update.
- `COMMENT ON COLUMN comms_message.thread_id` broadening per §6.2 in the same migration.
- New constants: `ContactMethodTypeGChat`, `IdentifierTypeGChat`; `Normalize(_, IdentifierTypeGChat)` delegates to `normalizeEmail`. **NO new `MapIdentifierTypeToContactMethodTypes` branch** — see §5.X.7 (DEFERRED). The sender-resolution dual-source set is carried by the sqlc query, not the identity-mapping layer.
- New sqlc query `ListGChatIdentitiesForSync` (dual-source via `WHERE type IN ('gchat', 'email')`).
- Extend `CommsMessageRepository` with the eight source-parameterized `MessageStore` methods (RECOMMENDED §5.X.1) + corresponding sqlc queries, ALL scoped to `deleted_at IS NULL` (and `processed_at IS NULL` for unprocessed-readers — §6.3).
- `gchatAdapter` (`SourceAdapter` impl, RECOMMENDED §5.X.4 defensive LIKE-escape applied).
- `commsMessageStoreAdapter` parameterized by `source='gchat'` (proves the substrate per §5.X.1; reusable by Telegram/Messages on later migration).
- Construct an `aggregation.Engine` instance wired with the GChat adapter pair, the existing `events.Bus`, and the `AggregatorReenqueuer`. The engine itself is unchanged; only the per-source wiring is new.
- Register the GChat engine in the `PerSourceChatListerRegistry` (`backend/internal/scheduler/messaging_aggregate_worker.go:39-61`) and as an `AggregatorReenqueuer` entry for `source="gchat"` (`backend/internal/consumer/aggregator_reenqueuer.go:30-56`).
- **No new event kinds added.** The engine already publishes `KindMessageReceived`/`KindMessageSent` with `Source = "gchat"` set from the adapter's `SourceName()`.
- Unit + integration tests: dual-source identity map; source-parameterized query selection; CHECK constraint round-trip; burst grouping over `comms_message` rows; reply-bridge to mutual; edit no-op (engine ignores rows with non-null `processed_at`); delete no-op (engine ignores soft-deleted rows); claim race recovery.

### Phase 2: Provider + rematch + bounded edit/delete handling + per-sweep metrics

**Inert until:** Phase 3 wires enablement and the user completes Chat App config + OAuth consent.

- OAuth scopes added at `oauth.go:26`. New Go dep `chat/v1`.
- `GChatSyncProvider` with: `spaces.list` enumeration; membership resolution with cache (`lastActiveTime` OR age > TTL triggers); per-space cursor pagination; dual-source identity resolution against `ListGChatIdentitiesForSync`; content upsert with set-union provenance merge (no event publish); stale-cursor reaper with restoration on re-discovery (§3.3).
- Edit detection via the PROVISIONAL `updateTime` cursor (§3.1 step 8) — verify support against Chat API docs before wiring, fall back to the bounded 7-day per-row `messages.get` polling described in §3.1 step 8 if `updateTime` is not filterable. **Edit handling is bounded from the first commit:** `source_metadata.previous_bodies[]` is capped at last-3 at write time (no separate hardening PR). Delete handling soft-deletes the `comms_message` row.
- `GChatHandleRematchHandler` (`IdentifierType="gchat"`) and `GChatEmailRematchHandler` (`IdentifierType="email"`); both register with `RematchService` via the existing fan-out (multi-handler-per-type, already supported); both call `aggregation.Engine.AggregateForContactBatch` after linking.
- **Metrics inline at write time:** `metrics.Increment(...)` calls embedded in the provider sweep loop and the edit/delete reconciliation path for `items_processed`, `items_matched`, `spaces_skipped_no_known_member`, `edits_applied`, `deletes_applied`. Add inline with the loops that drive them, not retroactively.
- Unit tests: chunk/page handling against a fake `chat.Service`; sender-attribution rule (inbound = one row, sender only); outbound fan-out across known co-members rule (one row per known co-member); DM-single-row case; cross-account dedup via provenance merge; cursor advancement after successful window; cursor restoration after `archived_space_cursors` reappearance; membership cache age-TTL refresh; `previous_bodies[]` cap holds across repeated edits; bounded 7-day fallback respects the window.

### Phase 3: Registration + enablement reconciliation + status endpoint (feature goes live)

**Activates the feature** once Chat App config and OAuth consent are completed by the user.

- Wire provider, both rematch handlers, `AggregatorReenqueuer` registry entry, `PerSourceChatListerRegistry` entry in `main.go`.
- Boot reconciliation creates an `external_sync_state(source='gchat', account_id)` row per Google credential that has the new chat scopes granted; sets `enabled=true`; sets `metadata.backfill_since` and an empty `space_cursors` map.
- OAuth-connect reconciliation also creates/enables on a fresh account connection.
- Surface a "reconnect required" indicator in the settings UI for accounts missing the new scopes.
- Status endpoint richness in `GET /api/v1/sync/status?source=gchat` mirroring Gmail's status shape. Lands with the reconnect indicator since both surfaces touch the same UI.
- Integration test: enabling a fresh account triggers a sweep that enumerates spaces → membership-resolves → stores `comms_message` rows → the next aggregation pass derives interactions → cadence updates as expected.

### PROPOSED-only follow-ups (NOT bundled with GChat MVP)

- §5.X.2 Extract generic `IdentifierFanoutRematchHandler` after GChat lands (groups Gmail + GChat-email + Calendar email + GChat-handle).
- §5.X.3 Wire explicit-reply bridging via `quotedMessageMetadata` if delayed-reply false-negatives are observed in practice.
- §5.X.5 Extract `windowedSync` helper once 2-3 windowed providers exist.
- §5.X.7 Add the `MapIdentifierTypeToContactMethodTypes(IdentifierTypeGChat) → [gchat, email]` multi-mapping if/when an `IdentityService.MatchOrCreate(IdentifierTypeGChat, ...)` caller emerges.
- Telegram + Messages migration onto `comms_message` (separate, independent work; substrate is proven by GChat Phase 1).

---

## 9. Testing Strategy

### 9.1 Unit

- `gchatAdapter` source-ref formatting; LIKE-escape regardless of input shape (defensive).
- `commsMessageStoreAdapter` row projection: `comms_message` → `aggregation.Message` (ChatID = space resource via thread_id, IsOutgoing per direction, ExternalID = external_id, InteractionID / ClaimedAt / ClaimedSessionRef preserved).
- Dual-source identity map construction from `ListGChatIdentitiesForSync` rows — fan-out to multiple contacts on shared address; case-insensitivity; both gchat and email rows surfaced.
- Sender-vs-me direction resolution: sender in M → outbound; sender in known map → inbound; sender in neither → skip; DM peer implicit; group-fan-out rule (outbound to N known contacts produces N rows; inbound from one known sender produces one row).
- Edit detection: same-resource second fetch with differing body advances the body + records edited_at; identical body is a no-op.
- Delete detection: 404 / tombstone marker soft-deletes the comms_message row.
- Chunk/cursor advancement: cursor only advances after full window success; partial-window crash restarts the window.

### 9.2 Integration

- Full sweep against a fake `chat.Service`: spaces.list → membership resolve → messages.list → comms_message rows written (no events published by the provider). A subsequent aggregation pass then publishes `KindMessageReceived`/`KindMessageSent` with `Source="gchat"`.
- Aggregation engine produces correct interactions: same-direction burst within 2h → one interaction; reply within 48h → promote to mutual; reply after 48h → two separate interactions.
- Cross-account: same message resource observed in two accounts → ONE comms_message row per (matched_contact_id), provenance merged across `observed_accounts[]`.
- Edit reconciliation: existing interaction unchanged after an edit; `comms_message.body` and `source_metadata.edited_at` updated; the engine's next pass does NOT reprocess the row (`processed_at IS NOT NULL`).
- Delete reconciliation: `comms_message.deleted_at` set; existing interaction unchanged; the engine's next pass ignores the soft-deleted row.
- Dual-source rematch fan-out: adding a `gchat` method fires the gchat handler; adding an `email` method fires Calendar + Gmail + GChatEmail handlers; verify the GChatEmail handler scans the right address.
- Membership cache hit/miss: first sweep populates cache; second sweep within TTL reuses; lastActiveTime advance forces refresh.
- Zero-known-member space is enumerated but skipped; membership change re-activates it on the next sweep.
- Cursor crash safety: kill the provider mid-window; restart; the same window restarts from its anchor.

### 9.3 E2E

- `SyncBadge` for `'gchat'` source renders correctly on the settings page (mirrors `gcal` / `email` badges).
- Contact detail page shows a GChat interaction line item with the right direction signal and aggregated message count.

### 9.4 Manual verification

- Connect a real Google account; verify re-consent prompt fires for the new scopes; confirm `spaces.list` returns data; confirm a known-member DM produces an interaction; confirm a group-chat outbound to two known members produces two interaction rows.

---

## 10. Open Questions & Future Work

### Open questions

1. **Per-thread aggregation inside named SPACES.** Threaded replies in a SPACE could legitimately be modeled as separate aggregation scopes — a 50-message thread is one conversation, but two threads in the same SPACE on the same day are two conversations. The MVP collapses all threads in a SPACE into a single space-scope aggregation key. If this proves too coarse, the `source_ref` shape can be widened to `gchat:<space>:<thread>:<first_message>` — but this is NOT a zero-migration change. Existing v1 rows are written under `gchat:<space>:<first_message>`; switching post-MVP means those existing rows fall outside the new `SourceRefPrefix` LIKE pattern (`gchat:<space>:<thread>:%`) and the engine would no longer recognize them when building subsequent sessions. A widening release requires a one-time data migration that rewrites the existing `interaction.source_ref` values to the new shape (e.g. `UPDATE interaction SET source_ref = ... WHERE source = 'gchat' AND source_ref NOT LIKE 'gchat:%:%:%'`), gated on a feature flag. Both `thread.name` and message `name` are stored in `source_metadata`, so the rewrite has the data it needs — but it's a backfill, not just an adapter swap.
2. **Explicit-reply bridging via `quotedMessageMetadata`.** Detailed in §5.X.3. Defer to implementation; revisit if delayed-reply false-negatives are observed.
3. **Membership-cache staleness when a member leaves a space.** The cache TTL is now 24h with an age-based refresh trigger (§3.4), but a member who leaves a quiet space could still be attributed to it for up to 24h. Acceptable for MVP; revisit if production reveals false-positive attributions.
4. **Workspace-account senders.** The MVP target is a personal `@gmail.com` account; senders in `users/<id>` form for Workspace accounts (vs `users/<email>` for consumer) need a separate resolution path via People API `people.get` (already-granted `people.ContactsReadonlyScope`, see §7). The `users.me.lookup` cache stores both forms; verify quota during implementation.
5. **Backfill window for a Workspace-hosted SPACE** with many years of history — the `backfill_since=2026-01-01` anchor matches Gmail, but a SPACE the user joined in 2020 has 6 years of pre-anchor content that won't be ingested. This is consistent with Gmail's behavior and intentional; called out for transparency.
6. **Edit-after-delete race.** If a message is edited and then deleted before the next edit-cursor sweep, the cursor surfaces only the delete event (assuming `updateTime` is monotonic and the delete bumps `updateTime` after the edit). The edited body is then lost — the original body in `comms_message` is whatever was there before the edit, and the row is soft-deleted before we see the edit body. **Acceptable for MVP.** Mitigations are expensive (per-row `messages.get` on every edit-cursor hit, or maintaining a separate "lastSeenUpdateTime" per row) and the failure mode is benign: we lose the edit history of a message the user already chose to delete. Document the loss; revisit only if production shows it matters.
7. **`updateTime` filter support in `spaces.messages.list`.** Marked PROVISIONAL in §3.1 step 8. Resolve at implementation time by reading the Chat API reference and either confirming with a citation OR falling back to the per-row polling mechanism described in §3.1 step 8.

### Future work (not in scope)

- **Cross-source thread/day merging.** Two interactions per source on the same day with the same contact even if one is GChat and one is Telegram. Future revisit (referenced in Gmail spec §10 as the same deferral).
- **Real-time via Workspace Events.** If/when Workspace Events ever supports consumer accounts AND the Pi gains a public webhook surface (it currently doesn't — Tailscale only), revisit Pub/Sub-based real-time. Polling is the right choice for MVP.
- **Per-thread aggregation refinement.** See open question 1.
- **Telegram + Messages migration onto `comms_message`.** Substrate is proven by GChat (§5.X.1); separate, independent work to migrate the existing two sources onto the shared table.
- **GChat → CRM contact discovery.** Surface frequent unmatched senders as import candidates (parallel to Telegram's discovery flow). Deferred — match-or-skip is the deliberate MVP posture.
- **`IdentifierFanoutRematchHandler` extraction.** §5.X.2 follow-up after GChat lands.
- **GChat reactions and attachment content** — not requested; can be added later if AI summarization wants them.
- **Slash-command / app-message ingestion.** Out of scope; not interpersonal.
