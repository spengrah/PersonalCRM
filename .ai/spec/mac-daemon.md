# Mac Daemon (`crm-mac`) — Design Spec

**Date:** 2026-05-11
**Status:** Design — under adversarial review
**Issues this design supersedes/incorporates:** #73 (iMessage), #74 (iCloud Contacts)
**Author:** Brainstormed with Claude; final scope and decisions per user.

---

## Summary

Defines a new Mac-side daemon, `crm-mac` (Swift, launchd-managed), that runs on the user's MacBook and ingests local data sources unreachable from the Pi backend. The daemon publishes events to the existing `/api/v1/ingest/events` endpoint and heartbeats host status to a new `mac_host` model. It establishes five logical data sources (Messages.app, Phone/FaceTime call history, iCloud Contacts, Anarlog humans, Anarlog sessions) and a phased build order with v1 shipping the daemon framework plus Messages and iCloud Contacts.

Local LLM inference on the Mac is **explicitly out of scope** for this spec.

---

## Goals

- Provide a single, observable Mac-side process that ingests local data sources the Pi cannot reach.
- Reuse the existing CRM event-bus + sync infrastructure unchanged where possible; additive extensions where not (new event kinds, `mac_host` model, `external_contact` source value, `interaction.source` enum extension, identifier type additions, `external_sync_state.strategy=push` exclusion in the scheduler).
- Keep the Pi as the source of truth for sync state and CRM data; Mac is a stateless ingest pipe with minimal local cache.
- Respect Mac as a daily-driver laptop: predictable low resource footprint, energy-friendly periodic work, no background CPU during idle.
- Future-proof for multiple Mac hosts; v1 ships single-host UI.

## Non-goals

- Local inference (separate future spec).
- Pi → Mac inbound calls (daemon is push-only).
- Native Mac UI (menu bar app, settings GUI). Observability is via CLI on Mac and the existing Pi web frontend.
- Native Mac notifications.
- Attachment content/files (only metadata: type tag, optionally filename/MIME/size when cheap).
- Mac-side data sources we don't need: Mail.app, Notes.app, Reminders.app, Calendar.app, Signal, Photos.app, Voice Memos.
- Browser extension companion.
- WhatsApp ingestion — re-scoped to an on-Pi integration via `whatsmeow` (issue #363), mirroring Telegram: a long-running Go client on the Pi, not a Mac local-DB reader. It reuses the source-neutral aggregation pipeline (`backend/internal/messaging/aggregation`) and the `whatsapp` identifier type (`identity/normalize.go`); the #363 work adds its own `interaction.source='whatsapp'` value + staging table. The Mac-local-store design (Core Data DB, FDA, snapshot reading, mutation-scan edits) does not transfer to a network client.

---

## Vision

The Mac daemon ingests five logical data sources, publishing events to the Pi:

| Source | Strategy | Identifier type emitted | Content fidelity | Backfill floor | Discovery? |
|---|---|---|---|---|---|
| `messages` (iMessage + SMS via Messages.app) | event stream | `phone`, `email` (generic — see Identifier Types below) | full text + type tag | 2026-01-01 | no |
| `phone_calls` (Phone + FaceTime via CallHistoryDB) | event stream | `phone`, `email` (generic; channel via `service` column) | direction, duration, answered/missed, voicemail-presence flag, service tag | 2026-01-01 | no |
| `icloud_contacts` | entity sync | n/a (matches via contact methods) | full entity, into existing `external_contact` table | all current | yes |
| `anarlog_humans` | entity sync | `anarlog_human_id` (new) | full entity | all current | yes |
| `anarlog_sessions` | event stream | participants by `anarlog_human_id` | `_meta.json` + `_summary.md` + optional `_memo.md` | 2026-01-01 | no |

**Data flow:** the daemon publishes events to `/api/v1/ingest/events` (existing event-bus ingest endpoint). The Pi's event-bus consumers derive interactions, enrichments, and downstream effects from those events.

**Polling cadence (per source):**
- `messages`: scheduled ~60-90s
- `phone_calls`: scheduled ~60-90s (same cadence as `messages` — both are user-perceived realtime sources gated on the same FDA-protected sqlite stores)
- `icloud_contacts`: scheduled ~15min (full delta via `CNChangeHistoryFetchRequest`)
- `anarlog_humans`: scheduled ~5min (mtime-based)
- `anarlog_sessions`: **event-driven via FSEvents**, hourly safety poll for catchup and tombstone detection

**Backfill:** all event streams backfill to `2026-01-01`. Pattern follows Telegram's `backfill_cursor` + `backfill_complete` model. iCloud Contacts and Anarlog humans sync all current entities; entity tombstones are first-class (see deletion handling below).

---

## Architecture

### 1. The `crm-mac` daemon (Swift)

**Language & stack:** all-Swift, SPM-built. Single binary `crm-mac` that runs as both the launchd agent (`crm-mac daemon`) and the user-facing CLI. Rationale for Swift documented in conversation; key driver is `CNContactStore` access and modern macOS lifecycle APIs (`SMAppService`, FSEvents via `DispatchSource`, Keychain, `os.Log`).

**Repo layout:** `mac-daemon/` at repo root.

**Process model:** Swift binary, launchd user-domain agent. Registered via `SMAppService.agent` (macOS 13+) with a fallback plist for older systems. Runs as the user (not root) — Full Disk Access and Contacts permission are user-scoped.

**Internal components:**
- **Source readers** — one Swift module per source. Isolated; a crashing reader does not affect others.
- **Per-source scheduler** — each source has either an `NSBackgroundActivityScheduler` activity (messages, icloud_contacts, anarlog_humans) or an FSEvents source + safety poll (anarlog_sessions). See "Scheduling determinism" note in Failure Modes.
- **Pi client** — `URLSession` with API key auth from Keychain. Retries with bounded exponential backoff. Chunked batch uploads (~500 events per push). No local queue.
- **Heartbeat reporter** — every ~60s, POSTs rich status to `/api/v1/host/<id>/heartbeat`. Heartbeat payload defined below under "Observability."
- **CLI dispatcher** — `swift-argument-parser` subcommands.

**State on Mac:** minimal. Pi is source of truth for cursors. State cached locally in `~/Library/Application Support/crm-mac/state.json` for fast warm restart; Pi is authoritative on conflict (see Cursor Protocol).

**Configuration:**
- `~/Library/Application Support/crm-mac/config.json` — non-secret: Pi URL, Anarlog folder path, iCloud container allowlist, per-source enabled flags.
- macOS Keychain — secrets: Pi API key, `mac_host` UUID, cursor epoch token.

**CLI surface:**
```
crm-mac daemon                # launchd entry point — runs forever
crm-mac install [--upgrade]   # install or upgrade; interactive setup; consumes pairing token
crm-mac configure             # re-run container allowlist, path config, source enable/disable
crm-mac uninstall             # remove service registration, binary, keychain, config (NOT Pi-side state)
crm-mac status                # current sync state + heartbeat health + cursor lag per source
crm-mac logs [--source X] [--follow]
crm-mac doctor                # check permissions, Pi reachability, source readability
crm-mac version               # daemon version + protocol version
```

**Lifecycle — install + pairing flow:**
1. User clicks "Pair new Mac" in Pi web UI. Pi mints a short-TTL (10 min), single-use pairing token. Token shown in UI.
2. User runs `crm-mac install` on Mac, pastes pairing token.
3. Daemon POSTs `/api/v1/host` with `{pairing_token, hostname, daemon_version, protocol_version}`. Pi validates token, mints `mac_host` row, returns `{host_id, api_key, cursor_epoch}`. Both stored in Keychain.
4. Daemon runs the iCloud container allowlist picker (`CNContainer` enumeration with `--containers iCloud,Local` as a non-interactive override).
5. Daemon prompts for Anarlog folder path (default: `~/Documents/notes/meetings`).
6. Daemon runs `doctor` to surface permission gaps with deep links.
7. Daemon registers via `SMAppService` and starts.

The pairing token model means there's no admin/bootstrap key in widespread use — every host has its own scoped API key. The pairing endpoint is the only un-authenticated path; the rest live behind the host's API key middleware.

**Lifecycle — update:** drop new binary; `crm-mac install --upgrade` re-registers and bounces. Daemon includes its `protocol_version` in heartbeat; Pi can refuse stale daemons (see Version Skew).

**Lifecycle — uninstall:** removes service, binary, keychain, config. Does NOT delete Pi-side `mac_host` row or staging data — that's a separate `DELETE /api/v1/host/<id>` from the Pi UI.

**Permissions:** Full Disk Access (chat.db), Contacts permission (native `requestAccess`), Files & Folders for Anarlog path. `doctor` enumerates and provides `x-apple.systempreferences:` deep links.

**Testing & CI:** structure Swift modules so OS-permission-mediated code (Full Disk Access for chat.db, `CNContactStore` consent, Files & Folders) is a thin shell over pure-logic modules — chat.db parsing against a fixture SQLite file, identifier filtering, push payload serialization, pairing client logic, cursor/state management. CI runs `swift build` + `swift test` as the `mac-daemon-tests` job inside `.github/workflows/ci.yml` on `macos-15` (pinned), path-filtered to `mac-daemon/**` so Pi-side PRs don't burn macOS minutes (macOS runners are ~10× Linux cost). Integration tests that require TCC permissions, real iCloud state, or live `~/Library/Messages/chat.db` access live behind a `make test-daemon-local` target run on the developer's Mac before push — not in CI. The first Swift PR (daemon skeleton) lands this CI job so all subsequent Swift PRs have signal from day one.

### 2. Data sources

#### `messages` (iMessage + SMS via Messages.app)

- **Source:** `~/Library/Messages/chat.db`, read-only via GRDB.swift.
- **Cursor:** `MAX(ROWID)` from `message` table.
- **Identifier types emitted:** generic `phone` and `email`. Channel/provenance carried in `source='messages'`. Rationale: Messages.app delivers both iMessage and SMS uniformly; `imessage_*` identifier types in the existing codebase are semantically misleading (SMS is not iMessage) and will be retired in a cleanup migration. Identifier type describes identifier shape; source describes the integration. See "Identifier Types" appendix.
- **Content:** message text, `is_from_me`, `date`, `guid` (per-message stable ID — used as primary dedup key), `chat.guid`, group flag, reply-to, `message_type` tag (text/photo/audio/video/document/other) inferred from `attachment.uti` or `mime_type`. Optional attachment metadata: filename, size, mime_type (no content).
- **Group chat attribution:** ONE interaction is created — for the sender only. Other participants in a group chat are NOT marked as having "contacted" the user when a third party sends. Matches Telegram's current behavior (see `backend/internal/telegram/aggregation.go`). Group chat participant *membership* is recorded in the staging table for context but does not produce per-participant interactions.
- **Aggregation:** v1 prerequisite. Telegram's existing aggregator (`backend/internal/telegram/aggregation.go`) is extracted into a shared `backend/internal/messaging/aggregation` package with a source-neutral interface. Burst-windowing, reply bridging, direction inference (inbound/outbound/mutual) all carry over unchanged. The Mac-specific staging table (`messages_message`) satisfies the shared aggregator's interface. The aggregator's input filter is the **claim-aware** filter: `processed_at IS NULL AND (claimed_at IS NULL OR claimed_at < NOW() - INTERVAL '5 minutes')` — extends Telegram's existing `ListUnprocessedByContact` query. Concurrency is handled by **per-`(contact_id, source)` River job serialization** (the aggregator runs as a single River job keyed on `(contact_id, source)`); no PostgreSQL advisory lock needed. The key includes source so jobs for different sources (e.g. `messages` and `telegram`) on the same contact can run in parallel without contention. **Two-path output**, matching Telegram exactly:
  - **Extend path:** if a session resolves to a recent existing interaction in window (same contact + chat + same-or-mutual direction), the aggregator extends that interaction via direct `ContactService.ExtendInteraction` call AND marks the staging rows `processed_at = NOW()` + `interaction_id = <existing>` in the same call. **No event is published for extensions** — `cadence_updater` / `followup_manager` do not re-fire on conversation continuations (already the right product behavior per Telegram).
  - **Create path:** if no recent existing interaction matches, the aggregator publishes a `message.received` or `message.sent` event. The `InteractionRecorder` consumer creates the interaction row AND marks the staging rows processed atomically in the same tx (per the comment in `aggregation.go:323` — "gives us atomicity between the interaction row and the telegram_message.interaction_id FK").

  This **eliminates the cross-batch burst fragmentation problem in steady state**: when message B arrives in a later poll than message A and A's interaction already exists, the extend path picks B up into A's interaction. Concurrency between the aggregator and the InteractionRecorder introduces a narrower race (B arrives between aggregator-publish and InteractionRecorder-commit) which is resolved by the explicit `claimed_at` claim mechanism on the create path — see "Race mechanics" under Event flow.
- **Edits / deletes / reactions:** acknowledged out of scope for v1. chat.db represents reactions (tapbacks) as `associated_message_*` columns on a new message row; edits update existing rows (post-iOS 16). A `MAX(ROWID)` cursor sees new rows but not mutations to old rows. v1 stored content reflects message state at first ingestion. v2 adds a "mutation scan" reader that scans recent rows (last N days) for `date_edited`/`is_deleted` changes.
- **Permission:** Full Disk Access.
- **Discovery role:** none. The `messages` source is purely an *interaction* source; all senders must already exist as CRM `contact_method` rows (populated by `icloud_contacts`, `gcontacts`, or manual entry). Unlike Telegram (where the handle space is platform-local and the source legitimately doubles as a discovery surface), iMessage/SMS senders are phone numbers and emails — identifiers that any contact source can already supply, so re-using Messages.app as a discovery surface produces zero unique signal and a lot of spam/business/shortcode noise.
- **Sender filtering:** the daemon filters chat.db senders against the Pi's canonicalized phone/email identifier set **before** forwarding any message. Only rows whose sender resolves to a known `contact_method` are emitted as `raw_message.*` events. Senders that are not in the CRM — spam, businesses, shortcodes, one-time confirmations, unsaved numbers — never leave the Mac. This subsumes the prior plan's `messages_rematch` worker (no staged-but-unmatched rows exist) and eliminates the need for separate content-pattern or org-name heuristics.
- **Identifier set source:** `GET /api/v1/host/:id/known-identifiers` returns `{phones: [...], emails: [...]}` from every `contact_method` row in the CRM, canonicalized via the existing `identity/normalize.go` rules (E.164 phones, lowercased emails). The daemon refreshes on every heartbeat tick (~60s) and caches the response. Cross-source by design — phones added via iCloud, Google Contacts, manual entry, or any future source flow through the same endpoint.
- **Cold-start race recovery (30-day backwards scan):** when a `known-identifiers` refresh yields a newly-added identifier (computed by set-diff against the daemon's previous cache), the daemon performs a one-time backwards scan of chat.db over the last **30 days** for that sender's `handle.id` and forwards any matching rows. The 30-day window covers the "friend-of-a-friend met in a group chat, contact information saved after the fact" case: their prior messages get backdated into the CRM as soon as they appear in `contact_method`. Idempotency comes from the existing event-log `(source, source_id)` dedup — overlap with messages already forwarded via the live cursor is absorbed safely.

#### `phone_calls` (Phone + FaceTime via CallHistoryDB)

- **Source:** `~/Library/Application Support/CallHistoryDB/CallHistory.storedata` (Core Data SQLite). Read-only via GRDB.swift, opened through `SQLiteSnapshotReader.readOnlyURI(for:)` (the chokepoint that produces `?mode=ro`, NOT `immutable=1`); back-off on `SQLITE_BUSY` (the Phone/FaceTime apps hold a writer lock while a call is in progress). Unlike `chat.db` (which Messages.app checkpoints aggressively), macOS does not checkpoint CallHistoryDB's WAL on any predictable cadence — observed in production: `CallHistory.storedata` stale by days while `CallHistory.storedata-wal` is megabytes of live writes — so `immutable=1` would make the reader blind to recent calls. SQLITE_BUSY retries cover the concurrent-writer case; reopen-every-tick keeps each handle short-lived. Covers Continuity-mirrored iPhone voice calls, native macOS Phone-app calls (macOS Sequoia+), FaceTime audio, and FaceTime video — all unified in `ZCALLRECORD`.
- **Cursor:** `MAX(ZDATE)` from `ZCALLRECORD` (Mac absolute time — seconds since 2001-01-01, converted to UTC at emit). Same `backfill_cursor` + `live_cursor` JSON shape as `messages`; install-time max is the live-cursor anchor.
- **Identifier types emitted:** generic `phone` and `email`. Channel/provenance carried in `source='phone_calls'`, with the specific service (voice / facetime_audio / facetime_video) recorded in the `service` column on the `phone_call` staging row. The service enum derives from `ZSERVICE_PROVIDER` + `ZCALLTYPE` together: `com.apple.Telephony` → `voice`; `com.apple.FaceTime` + `ZCALLTYPE=8` → `facetime_audio`; `com.apple.FaceTime` + `ZCALLTYPE=16` → `facetime_video`. (`ZSERVICE_PROVIDER` alone is insufficient — empirically only two values exist on macOS Sequoia.) Rationale matches `messages`: identifier type describes identifier shape, source describes the integration; FaceTime-only handles (Apple IDs) naturally fall into `email`.
- **Content:** per-call record — direction (`ZORIGINATED`: 1=outgoing), peer handle (`ZADDRESS`, normalized via existing `identity/normalize.go` E.164/email rules), service (derived from `ZSERVICE_PROVIDER` + `ZCALLTYPE` per above), answered (`ZANSWERED` — **reliable only for inbound**; see "connected outbound" rule below), duration in seconds (`ZDURATION`), voicemail-present flag (`ZHASMESSAGE` — macOS sets to 1 when an unanswered inbound call left a voicemail; verified empirically against a real CallHistoryDB to have zero false positives, all flagged rows being `ZANSWERED=0 ZORIGINATED=0`), call timestamp (`ZDATE`), unique ID (`ZUNIQUE_ID` — primary dedup key, globally unique per call across Continuity-paired devices). **Voicemail audio + transcripts are not stored on the Mac** — they remain on the paired iPhone; Continuity propagates only the presence flag. `ZHASMESSAGE` is therefore the only voicemail signal available to the daemon, and surfacing voicemail content (audio/transcript) on the contact timeline is explicitly out of scope until an iOS companion source exists.
- **Interaction semantics:** every call row publishes a `call.*` event and produces a `phone_call` staging row. **Whether the staging row also produces an `interaction` row depends on whether content was delivered**, decided by the table below. The principle: an interaction (and the `last_contacted` bump that comes with `inbound`) requires that real content arrived from the contact. An unanswered inbound call that didn't leave a voicemail conveyed only an *attempt to reach* — not content — and so does not create an interaction.

  | `ZORIGINATED` | `ZANSWERED` | `ZDURATION` | `ZHASMESSAGE` | Interpretation | Interaction created? | Direction | Cadence effect |
  |---|---|---|---|---|---|---|---|
  | 0 (inbound) | 1 | — | — | Answered inbound | yes | `inbound` | bumps `last_contacted`, `last_response_at`, `last_interaction_at`, `contact_by` |
  | 0 (inbound) | 0 | — | 1 | Voicemail received | yes | `inbound` | same as answered inbound |
  | 0 (inbound) | 0 | — | 0 / NULL | Missed inbound, no voicemail | **no** | — | none |
  | 1 (outbound) | * | >0 | — | Connected outbound | yes | `outbound` | bumps `last_outreach_at` only |
  | 1 (outbound) | * | 0 | — | Missed outbound | yes | `outbound` | bumps `last_outreach_at` only |

  This is deliberately asymmetric with `messages`: an inbound iMessage always conveys content (the text exists regardless of whether the user reads it) and always bumps `last_contacted`; a missed call with no voicemail does not. Voicemail presence (`ZHASMESSAGE=1`) is the gating signal that distinguishes "they reached me" from "they tried".

  The missed-inbound-no-voicemail row is the only case where a staging row exists without a corresponding `interaction` row. The contact-timeline UI projects such rows via a union (`interaction UNION SELECT FROM phone_call WHERE matched_contact_id IS NOT NULL AND interaction_id IS NULL`) so the operator still sees "missed call" entries on the timeline; cadence math is unaffected because no interaction row exists. No new `direction` value (e.g. `attempt_inbound`) and no per-event `affects_cadence` flag — both would be footguns for future sources. The absence of an interaction row IS the no-cadence-effect signal.

  **Outbound "connected" is decided by `ZDURATION > 0`, not `ZANSWERED`.** macOS does not reliably populate `ZANSWERED=1` for outbound calls — verified empirically (zero `ZANSWERED=1 ZORIGINATED=1` rows in a real CallHistoryDB despite many real connected outbound conversations). `ZDURATION>0` is the only reliable "the other side picked up" signal for outbound. An interaction is still created for missed outbound (so the user sees their attempts on the timeline), and outbound *always* bumps `last_outreach_at` regardless of whether it connected — consistent with "attempted to reach".

  Outbound never bumps `last_contacted`. There is no `mutual` promotion path for calls (calls aren't bursted/bridged the way Messages are); a call you place and a call they return show up as two separate interactions on the timeline.
- **Aggregation:** **none.** Calls are discrete events with explicit start time and duration — they are *not* burst-windowed. Each call event flows directly through the Pi ingest service inline (Stage 1 only), analogous to `meeting_note.recorded`: in the ingest tx, upsert `phone_call` staging row (dedup by `ZUNIQUE_ID`), match identifier to `contact_id`, and — *if the row qualifies per the interaction-semantics table above* — create one interaction with `source='phone_calls'` + duration/service/voicemail metadata, emit `interaction.recorded`. Rows that don't qualify (missed inbound, no voicemail) skip the interaction insert + `interaction.recorded` publish but the staging row is still upserted, matched, and marked `processed_at`. No Stage 2/3, no `claimed_at` / `claimed_session_ref` columns, no aggregator River job. (Architectural simplification vs `messages` paid by the source itself being simpler.)
- **Edits / deletes:** CallHistoryDB rows are append-only in normal use. The Phone app's "Clear All Recents" action **does** delete rows; v1 treats deletions as out-of-scope (interactions stay in the CRM). v2 mutation-scan reader (same one needed for messages edits) covers the delete case via `(source, source_id)` reconciliation.
- **Permission:** Full Disk Access — same gate as `messages`. No new permission prompt if the user has already granted FDA for `messages`.
- **Discovery role:** none. Same reasoning as `messages` — call handles are phones/emails that any contact source can already supply. Daemon-side filtering uses the same `known-identifiers` set.
- **Sender filtering:** reuses the `messages` daemon-side filter unchanged. The daemon canonicalizes each call's peer handle and forwards only rows matching the known phone/email set. Spam calls, robocalls, businesses, unsaved numbers never leave the Mac.
- **Identifier set source:** same `GET /api/v1/host/:id/known-identifiers` endpoint as `messages`. No new endpoint; the existing per-heartbeat refresh covers both sources.
- **Cold-start race recovery (30-day backwards scan):** identical model to `messages`. When `known-identifiers` diff shows a newly-added identifier, the daemon scans the last 30 days of `ZCALLRECORD` for that handle and forwards any matches. Event-log `(source, source_id)` dedup absorbs overlap.

#### `icloud_contacts`

- **Source:** `CNContactStore` via Contacts framework. **Not** AddressBook SQLite.
- **Cursor:** `CNChangeHistoryFetchRequest` delta token (macOS 13+). First run fetches all contacts in allowlisted containers; afterward fetches changes since last token.
- **Pi-side storage:** existing **`external_contact` table** with `source='icloud_contacts'`, `source_id = CNContact identifier`. NOT a separate `icloud_contact` table. Reuses existing match_status enum, duplicate_of_id deduplication, and Import UI. Adds a new `container_identifier` field to `external_contact.metadata` JSONB to track which `CNContainer` produced each row.
- **Identifier type:** n/a — match flows through existing `MatchOrCreate` on each contact method (email/phone) from the contact's data, same as `gcontacts`.
- **Content:** display_name, first/last name, organization, job_title, emails JSONB, phones JSONB, postal addresses, birthday, photo_url (we do not yet store photos; capture URL/path for future).
- **Deletion handling:** `CNChangeHistoryFetchRequest` reports `delete` and `update` events alongside `add`. Daemon emits explicit `external_contact.deleted` events; **the ingest service inline (Stage 1)** sets `external_contact.deleted_at = NOW()` (new column added in same migration). `crm_contact_id` and `match_status` are **preserved unchanged** — `deleted_at` is the tombstone mechanism; the existing FK link enables transparent revive (if the same source_id reappears, the upsert path NULLs `deleted_at` and the row is functionally restored with its original link intact). Removed contact methods within an updated contact are diffed and synced.
- **Token expiration / full resync:** if the change-history token expires (Apple may invalidate it; spec assumes possible), `CNContactStore` returns an error. Daemon detects, drops cursor, performs full re-sync against the current contact set, and reconciles tombstones (any previously-synced contact not present in the full set is treated as deleted via `external_contact.deleted` event).
- **Container moves:** a contact moving between containers (e.g., promoted from On-My-Mac to iCloud) is observed as a delete+add. Same contact identifier may not be preserved; the daemon emits both events and relies on existing fuzzy de-dup to keep the CRM contact intact via email/phone match.
- **Container allowlist:** `crm-mac install` enumerates `CNContainer`s and prompts user. Defaults: include iCloud and On-My-Mac; exclude all CardDAV/Exchange (covered by `gcontacts`). New containers fail-closed; `doctor` warns on overlap with active `gcontacts` integration.
- **Permission:** Contacts via `requestAccess`.
- **Discovery:** yes — every contact either matches or surfaces as import candidate, identical to `gcontacts` flow.

#### `anarlog_humans`

- **Source:** `~/Documents/notes/meetings/humans/<uuid>.md` (user-configurable path). YAML frontmatter + optional markdown body (`memo`). Direct file reads (Anarlog CLI rejected: no structured-output guarantees).
- **Cursor + tombstones + change detection:** state tracked as `{uuid → {mtime, content_hash}}`. mtime advance triggers a content-hash check (SHA-256 of file bytes); only push on hash change. **Mtime alone is insufficient** — Mac filesystem timestamps can be preserved across copies, rounded by filesystems, or move backward under sync tools like iCloud/Dropbox/git restore. **Tombstone detection** via full inventory scan (every safety-poll interval): a UUID present in last scan but absent in current scan is emitted as `external_contact.deleted` for source=anarlog_humans.
- **Identifier type emitted:** new `anarlog_human_id` (UUID, no contact_method mapping — pure identity-storage hook for future session matching). Match for the human itself happens via the human record's name/emails fields.
- **Content:** frontmatter (`name`, `emails[]`, `job_title`, `linkedin_username`, `org_id`, `user_id`, `pin_order`, `pinned`, `created_at`) + optional body (`memo`). Defensive parsing: whitelist known fields, accept both `[]` and CSV array formats (Anarlog v1.0.1 changed serialization), tolerate timestamp format drift.
- **Self-human exclusion:** the file `00000000-0000-0000-0000-000000000000.md` is skipped at reader level.
- **Pi-side storage:** existing **`external_contact` table** with `source='anarlog_humans'`, `source_id = anarlog UUID`. Same model as `icloud_contacts`. The `anarlog_human_id` is also written to `external_identity` for session-participant lookup.
- **Permission:** Files & Folders for the configured path.
- **Discovery:** yes — Anarlog humans are import candidates in the existing Import UI.

#### `anarlog_sessions`

- **Source:** `~/Documents/notes/meetings/sessions/<uuid>/`. Daemon reads `_meta.json`, `_summary.md`, optional `_memo.md`. **`transcript.json` is out of scope for v1** (no speaker labels, word-level only).
- **Cursor + tombstones + change detection:** state tracked as `{uuid → {meta_mtime, meta_hash, summary_hash, memo_hash}}`. Re-push on any hash change of `_meta.json`, `_summary.md`, or `_memo.md`. Full inventory scan on hourly safety poll detects deleted sessions; emit `meeting_note.deleted` events. Same mtime-unreliability reasoning as `anarlog_humans`.
- **Identifier type:** participants referenced by `anarlog_human_id` via `_meta.json.participants[].human_id`. Resolution flows through `anarlog_humans` entity sync.
- **Content:** session id, title, created_at, participants (UUIDs), tags, `_summary.md` body, `_memo.md` body (if present).
- **Permission:** Files & Folders.
- **Discovery:** none.
- **Skip list:** daemon explicitly ignores `chats/`, `daily_notes.json`, `chat_shortcuts.json`, `memories.json`, `tasks.json`, `settings.json`, `store.json`, `search_index/`, `plugins/`, `events.json`, `calendars.json` (calendar already covered by `gcal`).

### 3. Pi-side model

#### Event flow

The daemon publishes events to the existing `/api/v1/ingest/events` endpoint **without changing its wire contract.** The endpoint accepts `{events: [{source, source_id, kind, payload, observed_at}]}` and returns `{accepted, duplicate, rejected, errors}` — unchanged. Cursor management lives on separate endpoints (see Cursor Protocol).

**Three-stage flow, matching the existing Telegram precedent:**

**Stage 1 — Pi ingest service (synchronous, single transaction per batch):**
For each raw event from the daemon: validate, match identity via `IdentityService.MatchOrCreate`, upsert into the appropriate staging table (dedup by canonical source_id, with `processed_at = NULL` on insert). On batch commit, enqueue an aggregator River job per affected `(contact_id, source)` pair (deduplicated by `(contact_id, source)` — River's built-in uniqueness key). For non-message events (`external_contact.*`, `meeting_note.*`), Stage 1 ALSO performs the full domain work inline in the same tx: upsert `external_contact` row (or `meeting_note` staging row), run identity match, etc. — there is no Stage 2/3 for non-message events; the consumer entries in the table below are descriptive of the inline work, not separate River jobs. The daemon's batch returns `{accepted, duplicate, rejected, errors}` based on staging-row dedup. **Stage 2/3 (aggregation + interaction creation) only applies to `raw_message.*` events.**

**Stage 2 — Aggregator (River job, per-contact serialized):**
For each contact with new unprocessed staging rows, the shared aggregator job runs: lists rows from the source-specific staging table where `processed_at IS NULL AND (claimed_at IS NULL OR claimed_at < NOW() - INTERVAL '5 minutes')` (a stale-claim recovery window — see Race Mechanics below), groups into bursts, resolves into sessions. For each session, either:

- **Extend path:** find a recent existing interaction matching the session — call `ContactService.ExtendInteraction` and mark the staging rows `processed_at = NOW()` + `interaction_id = <existing>` in the same call. **No event published.**
- **Create path:** atomically (a) set `claimed_at = NOW()` and `claimed_session_ref = <deterministic_sourceRef>` on the included rows, AND (b) publish the create-event (`message.received` / `message.sent`) with `source_id = <deterministic_sourceRef>`. The claim and the publish are in one DB tx so they succeed or fail together.

River's per-key job-uniqueness ensures at most one aggregator instance runs per `(contact_id, source)` at a time.

**Stage 3 — `InteractionRecorder` (River consumer, async):**
Consumes create-events from Stage 2. In a single tx: insert the interaction row (idempotent by `(source, source_ref)`); call `StagingRepo.MarkProcessed(ctx, source, ids, interaction_id)` to set `processed_at = NOW()`, **clear `claimed_at = NULL` and `claimed_session_ref = NULL`**, and set `interaction_id = <new>` on those rows; emit `interaction.recorded`. **Post-commit hook: re-enqueue the aggregator job for the same `(contact_id, source)`** — this catches any rows that arrived between Stage 2 and Stage 3 and were excluded by the claim, and any rows whose claim expired during a long-running Stage 3.

Downstream consumers (`cadence_updater`, `followup_manager`) react to `interaction.recorded`.

**Race mechanics — the dedup-then-stuck case Codex flagged:**

Without a claim mechanism, the following race can lose work:

1. Message A arrives → staged with `processed_at=NULL`. Aggregator enqueued.
2. Aggregator pass 1 lists `[A]`, emits create-event sourceRef X.
3. Before InteractionRecorder commits, message B arrives → staged with `processed_at=NULL`. Aggregator enqueued.
4. River starts aggregator pass 2 (allowed because pass 1 has completed publishing). Pass 2 lists `[A, B]` (A still unprocessed), groups into a single session, emits same deterministic sourceRef X.
5. Event-log `(source, source_id)` dedup catches it. Pass 2's event is dropped.
6. InteractionRecorder eventually processes the original event with `message_ids=[A]` only. Marks A processed. B stays `processed_at=NULL`, never reconsidered until another trigger arrives.

**The claim + recovery design resolves this:**
- Step 4's pass 2 lists rows where `processed_at IS NULL AND claimed_at IS NULL`. A was claimed in pass 1 → excluded. Pass 2 sees only `[B]`. Either it finds A's just-created interaction within window (extends, picks up B), or it creates a new session for B.
- **If pass 2 runs before InteractionRecorder commits A's interaction:** pass 2 sees only B and creates a new session. Result: A and B in two adjacent interactions instead of one. Acceptable; matches v1's "occasional fragmentation" tradeoff.
- **If pass 2 runs after InteractionRecorder commits A's interaction:** pass 2 finds A's interaction within window via the extend path and includes B in it. Single interaction.
- **Post-commit re-enqueue from Stage 3** ensures pass 2 is triggered immediately after every Stage 3 commit, maximizing the second case (extend wins) over the first (fragmentation).
- **Stale-claim recovery (`claimed_at < NOW() - INTERVAL '5 minutes'`):** the recovery path is NOT to re-publish the create-event (event-log `(source, source_id)` dedup would suppress the retry). Instead, the aggregator detects that an event already exists for `claimed_session_ref` and **re-enqueues a River InteractionRecorder job for that existing event** (by event ID, not by re-publishing). The consumer processes the existing event row idempotently: if the interaction was already created, just complete the `MarkProcessed` step (clear `claimed_at`, set `processed_at`, set `interaction_id`); if not, create the interaction + mark in one tx. This makes Stage 3 retryable indefinitely without depending on event re-publication. **Bounds the worst-case stuck duration to the claim TTL.** If `claimed_session_ref` exists in claim but NOT in event log (impossible in practice given the same-tx claim+publish, but defensive): the aggregator clears the claim and treats rows as unclaimed (next pass re-emits cleanly).

**Cursor commit safety:** the daemon commits the source cursor only after the ingest response (Stage 1 commit). At that point, staging rows are durable AND an aggregator job is queued. If Stage 2 or Stage 3 fails transiently, River retries. Daemon re-pushes after cursor-commit failure are absorbed by `(source, source_id)` event-log dedup AND by per-source staging-table unique constraints (`guid`, etc.).

| Event kind | Status | Producer (Stage 1) | Consumer (Stage 2) | Stage-1 side effects | Stage-2 side effects |
|---|---|---|---|---|---|
| `raw_message.received`, `raw_message.sent` (messages) | **new daemon-emitted kinds** | daemon | (no event-bus consumer; processed by ingest service + aggregator job) | Stage 1: match identity, upsert staging row (dedup by Guid), enqueue aggregator job for affected `(contact_id, source)` | n/a — these events stay in the event-log as raw input record; not delivered to bus consumers |
| `message.received`, `message.sent` (create path, post-aggregation) | existing | Aggregator (Stage 2 River job) | `InteractionRecorder` (generalized) | published only when aggregator's create path applies — i.e., session has no recent existing interaction to extend. source_id = deterministic burst sourceRef | InteractionRecorder creates interaction row, atomically marks referenced staging rows processed via source-neutral StagingRepo.MarkProcessed, emits `interaction.recorded` |
| (extend path) | n/a — no event | n/a | n/a | When aggregator finds a recent matching interaction, it extends via direct `ContactService.ExtendInteraction` and marks staging rows processed in the same call. No event is published — extensions intentionally do NOT re-fire cadence/followup consumers (matches Telegram's product behavior). | n/a |
| `external_contact.upserted` (icloud_contacts, anarlog_humans) | new | Pi ingest service inline (Stage 1 only) | n/a — fully inline | upsert `external_contact` row, run identity match against contact methods, store event row for audit | n/a |
| `external_contact.deleted` | new | same | n/a — fully inline | set `external_contact.deleted_at = NOW()`. **Does NOT detach `crm_contact_id`** — the `deleted_at` filter already hides the row from match/import queries; preserving the FK enables transparent revive (the upsert path NULLs out `deleted_at` and the prior link remains intact). | n/a |
| `meeting_note.recorded` (anarlog_sessions) | new | Pi ingest service inline (Stage 1 only) | n/a — fully inline | upsert `meeting_note` staging row (dedup by session UUID), resolve participants via `external_identity` for `anarlog_human_id`, create one interaction per matched participant, emit `interaction.recorded` via existing bus | n/a |
| `meeting_note.deleted` | new | same | n/a — fully inline | mark staging row `deleted_at = NOW()`; mark the linked interactions soft-deleted via existing interaction soft-delete | n/a |
| `call.received`, `call.sent` (phone_calls) | new | Pi ingest service inline (Stage 1 only) | n/a — fully inline | upsert `phone_call` staging row (dedup by `ZUNIQUE_ID`), match phone/email identifier via existing matcher. **Conditionally** create one interaction with `source='phone_calls'` + duration/service/voicemail metadata and emit `interaction.recorded` via existing bus — skipped for missed-inbound-no-voicemail rows (`ZORIGINATED=0 AND ZANSWERED=0 AND ZHASMESSAGE IN (0, NULL)`); see `phone_calls` source section for the full decision table | n/a |

**`mac_host.heartbeat` is NOT an event** — it's a dedicated endpoint (`POST /api/v1/host/:id/heartbeat`) writing directly to the `mac_host` table. Heartbeats don't traverse the event bus; they're operational state, not domain events.

**`MessageReceivedPayload` V2 extension** (additive, backward-compatible):
```go
type MessageReceivedPayload struct {
    // V1 fields (existing, used by Telegram):
    Version           int        `json:"version"`           // bumped to 2 by Mac daemon
    ContactID         *uuid.UUID `json:"contact_id,omitempty"` // nil if unmatched at producer
    PeerRef           string     `json:"peer_ref"`          // e.g. "messages:+15551234567"
    MessageAt         time.Time  `json:"message_at"`
    Description       *string    `json:"description,omitempty"`
    ExternalMessageID string     `json:"external_message_id,omitempty"`  // chat.db ROWID
    Direction         string     `json:"direction,omitempty"`
    MessageIDs        []uuid.UUID `json:"message_ids,omitempty"` // Source-neutral list of staging-row UUIDs (telegram_message or messages_message)

    // V2 fields (optional; populated by Mac daemon emitting raw_message.*, AND by Mac
    // aggregator emitting aggregated message.received/.sent for source-neutral session events):
    HostID           *uuid.UUID  `json:"host_id,omitempty"`             // mac_host.id provenance
    Guid             *string     `json:"guid,omitempty"`                // iMessage guid (raw_message events only)
    ChatID           *string     `json:"chat_id,omitempty"`             // chat.db chat.guid
    PeerName         *string     `json:"peer_name,omitempty"`           // display name from chat partner (if available)
    RawText          *string     `json:"raw_text,omitempty"`            // full message body (raw_message events; condensed digest for aggregated)
    MessageType      *string     `json:"message_type,omitempty"`        // text/photo/audio/video/document/other
    IsGroup          *bool       `json:"is_group,omitempty"`
    Attachments      []AttachmentMeta `json:"attachments,omitempty"`
}

// MessageIDs (existing field) carries source-neutral staging-row UUIDs for ALL
// sources post-aggregation. Telegram populates with telegram_message UUIDs; Mac
// aggregator populates with messages_message UUIDs.
// The generalized InteractionRecorder dispatches on event envelope source to mark
// rows processed_at = NOW() via StagingRepo.MarkProcessed(ctx, source, ids).

type AttachmentMeta struct {
    Type     string  `json:"type"`               // photo/audio/video/document/other
    Filename *string `json:"filename,omitempty"`
    MimeType *string `json:"mime_type,omitempty"`
    Size     *int64  `json:"size,omitempty"`
}
```

V2 fields are populated by the Mac daemon. Telegram's existing producer continues emitting V1; existing consumer logic unchanged. The aggregator (extracted from telegram, now in `backend/internal/messaging/aggregation`) emits V1 or V2 events depending on source. `InteractionRecorder` consumes both; new V2 fields are passed through to the interaction row's metadata where useful.

`interaction.source` CHECK constraint extended via migration to add `messages`, `phone_calls`, `anarlog_sessions`. `external_sync_state.strategy` CHECK constraint extended to add `push`.

**Non-message event payloads (new):**

```go
// External contact entity sync (icloud_contacts, anarlog_humans)
type ExternalContactUpsertedPayload struct {
    Version          int                    `json:"version"` // start at 1
    HostID           uuid.UUID              `json:"host_id"`
    Source           string                 `json:"source"`    // "icloud_contacts" | "anarlog_humans"
    SourceID         string                 `json:"source_id"` // CNContact identifier or Anarlog UUID
    DisplayName      *string                `json:"display_name,omitempty"`
    FirstName        *string                `json:"first_name,omitempty"`
    LastName         *string                `json:"last_name,omitempty"`
    Emails           []ContactMethodValue   `json:"emails"`    // [{value, type, primary}]
    Phones           []ContactMethodValue   `json:"phones"`
    Addresses        []AddressValue         `json:"addresses,omitempty"`
    Organization     *string                `json:"organization,omitempty"`
    JobTitle         *string                `json:"job_title,omitempty"`
    Birthday         *string                `json:"birthday,omitempty"` // ISO date
    PhotoURL         *string                `json:"photo_url,omitempty"`
    Metadata         map[string]any         `json:"metadata,omitempty"` // source-specific (container_identifier, anarlog app_version, etc.)
}

type ExternalContactDeletedPayload struct {
    Version  int       `json:"version"` // start at 1
    HostID   uuid.UUID `json:"host_id"`
    Source   string    `json:"source"`
    SourceID string    `json:"source_id"`
}

// Meeting note entity (anarlog_sessions)
type MeetingNoteRecordedPayload struct {
    Version       int        `json:"version"` // start at 1
    HostID        uuid.UUID  `json:"host_id"`
    Source        string     `json:"source"`           // "anarlog_sessions"
    SourceID      string     `json:"source_id"`        // anarlog session UUID
    Title         *string    `json:"title,omitempty"`
    MeetingAt     time.Time  `json:"meeting_at"`
    Summary       *string    `json:"summary,omitempty"`        // _summary.md body
    Memo          *string    `json:"memo,omitempty"`           // _memo.md body, if present
    ParticipantIDs []string  `json:"participant_ids,omitempty"` // anarlog_human UUIDs
    Tags          []string   `json:"tags,omitempty"`
}

type MeetingNoteDeletedPayload struct {
    Version  int       `json:"version"` // start at 1
    HostID   uuid.UUID `json:"host_id"`
    Source   string    `json:"source"`
    SourceID string    `json:"source_id"`
}
```

**Canonical `source_id` values per event kind.** The existing event table has `UNIQUE INDEX idx_event_source_source_id ON event (source, source_id) WHERE source_id IS NOT NULL` (migration 036). Same `(source, source_id)` is dedup-rejected. For **mutable** entity events, the daemon must include a content discriminator in `source_id` so updates aren't lost as duplicates of the initial upsert.

| Event kind | `source_id` value | Notes |
|---|---|---|
| `raw_message.received`, `raw_message.sent` (daemon → ingest) | iMessage `guid` (messages) | Globally unique per message; second-host push returns `duplicate` |
| `message.received`, `message.sent` (aggregator → bus, **create path only**) | Telegram's existing deterministic burst `sourceRef` pattern: e.g., `messages:<chat_id>:<earliest_msg_guid>` (mirrors `backend/internal/telegram/aggregation.go:89` `sourceRef()` method) | Deterministic from the session's first message. If aggregator re-runs on the same message set (e.g., after River retry), source_id is stable → dedup absorbs duplicate emit. Note: the cross-batch fragmentation case is handled by the aggregator's **extend path** (no event), not by source_id semantics — see Aggregation section. |
| `external_contact.upserted` (icloud_contacts, anarlog_humans) | `<entity_uuid>@<content_hash>` where content_hash = SHA-256 hex of the canonicalized event payload (excluding the source_id itself and the host_id) | Same content → same source_id → dedup as no-op. Content change → new source_id → new event. |
| `external_contact.deleted` | `<entity_uuid>@deleted@<previous_content_hash>` where previous_content_hash is the SHA-256 of the last content state the daemon observed before this entity disappeared | Deterministic — re-push after cursor-commit failure produces the same source_id because the previous content hash is fixed. Supports delete → revive → delete: each revive cycle's last-observed content yields a different prior hash, so each subsequent delete is its own unique event. If the daemon doesn't know the previous content (e.g., daemon restart with no local cache), it falls back to fetching the entity's current content from `GET /known-ids` extended response (which can include last-content-hash per known ID) or uses sentinel `<entity_uuid>@deleted@unknown` — accepting one-time dedup against any prior `@unknown` delete for the same entity. |
| `meeting_note.recorded` (anarlog_sessions) | `<session_uuid>@<content_hash>` | Same model as `external_contact.upserted` |
| `meeting_note.deleted` | `<session_uuid>@deleted@<previous_content_hash>` | Same model as `external_contact.deleted` |

**Distinct event-kind families:** daemon-emitted `raw_message.*` (raw input) and aggregator-emitted `message.*` (aggregated session output) are **different kinds**. Daemon never publishes `message.received`/`message.sent` directly — only `raw_message.*`. The Pi ingest service is the boundary that transforms raw events into aggregated session events. `InteractionRecorder` only consumes `message.received`/`message.sent` — it never sees raw events. This avoids the producer-vs-aggregator naming collision and preserves the existing Telegram consumer flow unchanged (Telegram's producer already emits aggregated `message.*` directly).

New event kinds to add to `events/kinds.go`: `raw_message.received`, `raw_message.sent`, `external_contact.upserted`, `external_contact.deleted`, `meeting_note.recorded`, `meeting_note.deleted`.

**Raw message payload schemas** (concrete Go types, distinct from `MessageReceivedPayload`):

```go
// RawMessageReceivedPayload — emitted by Mac daemon for messages.
// Carries the full raw data needed by the Pi ingest service to match identity,
// upsert staging, and run aggregation. NOT consumed by any bus consumer —
// processed inline within the ingest transaction.
type RawMessageReceivedPayload struct {
    Version      int       `json:"version"` // start at 1
    HostID       uuid.UUID `json:"host_id"`
    Source       string    `json:"source"`         // "messages"
    Guid         string    `json:"guid"`           // iMessage guid
    ChatID       string    `json:"chat_id"`        // chat.db chat.guid
    PeerHandle   string    `json:"peer_handle"`    // raw handle as observed (phone/email)
    PeerName     *string   `json:"peer_name,omitempty"`
    Text         *string   `json:"text,omitempty"` // full message body
    MessageType  string    `json:"message_type"`   // text/photo/audio/video/document/other
    IsGroup      bool      `json:"is_group"`
    SentAt       time.Time `json:"sent_at"`        // from source data, NOT daemon clock
    ReplyToGuid  *string   `json:"reply_to_guid,omitempty"`
    Attachments  []AttachmentMeta `json:"attachments,omitempty"`
}

// RawMessageSentPayload — identical shape; separate type for outbound messages
// emitted by the user. Used by aggregator to determine direction.
type RawMessageSentPayload RawMessageReceivedPayload
```

The existing `MessageReceivedPayload` and `MessageSentPayload` (used post-aggregation) keep their V1 fields for Telegram compatibility; the V2 fields described earlier remain optional and may carry post-aggregation digest info (e.g., session-level message_type if all underlying messages share a type) when emitted by the Mac aggregator. **The `MessageIDs` field is generalized**: it is the source-neutral list of staging-row UUIDs that compose the aggregated session, regardless of source. The generalized `InteractionRecorder` looks up the source from the event envelope and marks the corresponding staging table's rows `processed_at = NOW()` via a source-neutral repository method (`StagingRepo.MarkProcessed(ctx, source, ids)`).

#### New tables

- **`mac_host`** — `id` (UUID, stable across reinstalls via Keychain), `hostname`, `daemon_version`, `protocol_version`, `last_heartbeat_at`, `permissions` JSONB, `source_health` JSONB (per-source last_cursor, last_pushed_at, last_error, schema_version), `cursor_epoch` BIGINT (incremented when Pi restored from backup; daemon must match for pushes), `api_key_hash TEXT`, `api_key_revoked_at TIMESTAMPTZ`, `created_at`, `updated_at`. Table designed for multi-host future-proofing, **but v1 enforces single non-revoked host** via a partial unique index: `CREATE UNIQUE INDEX idx_mac_host_singleton ON mac_host ((true)) WHERE api_key_revoked_at IS NULL`. Multi-host operation requires resolving content-hash source_id collisions across hosts — explicitly deferred to a follow-up spec.
- **`messages_message`** — staging table mirroring `telegram_message`. **Globally unique on `guid`** (iMessage's per-message stable ID — cross-host dedup). Columns include: `guid`, `chat_guid`, `peer_handle`, `peer_normalized`, `text`, `message_type`, `sent_at`, `is_outgoing`, `is_group_chat`, `matched_contact_id`, `interaction_id`, `mac_host_id` (provenance only), `processed_at` (NULL until Stage 3 commits), **`claimed_at` TIMESTAMPTZ** (NULL until aggregator's create path claims; cleared by InteractionRecorder when Stage 3 commits), **`claimed_session_ref` TEXT** (deterministic sourceRef of the pending create-event; cleared by Stage 3). Partial index on `(matched_contact_id) WHERE processed_at IS NULL AND claimed_at IS NULL` to support the aggregator's input filter efficiently.
- **`anarlog_session_message`** (or `meeting_note`) — staging table for anarlog session content. Unique on `anarlog_session_id` (UUID). Columns: session UUID, title, summary, memo, participants JSONB, created_at, `mac_host_id`. **No `claimed_at`/`claimed_session_ref` needed** — anarlog sessions are 1:1 with events (no burst aggregation), so the create-path race doesn't apply.
- **`phone_call`** — staging table for Phone/FaceTime call records. **Globally unique on `call_unique_id`** (`ZCALLRECORD.ZUNIQUE_ID` — Continuity propagates the same value across paired devices, so two Macs pushing the same call dedup at the staging unique constraint). Columns: `call_unique_id`, `peer_handle`, `peer_normalized`, `service` (`voice` | `facetime_audio` | `facetime_video`), `direction` (`inbound` | `outbound`), `answered` BOOL (inbound only — see source section), `has_voicemail` BOOL (mirrors `ZHASMESSAGE`; always FALSE for outbound), `duration_seconds` INT, `started_at` TIMESTAMPTZ, `matched_contact_id`, `interaction_id` **NULLABLE** (NULL for missed-inbound-no-voicemail rows that intentionally did not produce an interaction; see `phone_calls` source section), `mac_host_id`, `processed_at`. **No `claimed_at`/`claimed_session_ref` needed** — calls are 1:1 with interactions when an interaction is created (no burst aggregation). `processed_at` is set in the same ingest tx as the optional interaction insert, regardless of whether an interaction was actually created — it reflects "ingest service has finished with this row", not "an interaction exists". The contact-timeline UI projects `phone_call` rows with `interaction_id IS NULL AND matched_contact_id IS NOT NULL` alongside `interaction` rows so missed-no-voicemail calls remain visible to the operator without affecting cadence.

**Telegram migration:** the existing `telegram_message` table also gains `claimed_at` and `claimed_session_ref` columns via the same migration. The shared aggregator requires them on its source-neutral repository interface. Existing rows backfill with NULL (safe — equivalent to "unclaimed"); the Telegram aggregator's call sites update to use the claim-aware filter.

#### Existing tables — used unchanged or with additive extensions

- **`external_contact`** — used for `source IN ('gcontacts', 'icloud_contacts', 'anarlog_humans')` (see source naming matrix below). **Schema additions:** new `deleted_at TIMESTAMPTZ` column for tombstones, and container_identifier (for iCloud) / app_version (for Anarlog) tracked in `metadata` JSONB.
  - **Query rules for `deleted_at`:** every query against `external_contact` must filter `WHERE deleted_at IS NULL` unless explicitly working with tombstones. This includes: identity matching candidate lookups, import-candidate UI listings, duplicate-of-id resolution, rematch-worker scans, and per-source statistics. Mirrors the existing soft-delete pattern across the codebase (per `.ai/rules/core.md`). The upsert path (driven by `external_contact.upserted` events) NULLs out `deleted_at` if the row was previously tombstoned, restoring it transparently.
- **`external_identity`** — used for `(identifier, identifier_type, source)` tracking. New identifier type `anarlog_human_id`. Existing `imessage_phone` / `imessage_email` types become unused; cleanup migration in a follow-up.
- **`external_sync_state`** — one row per `(source, account_id)` where `account_id = mac_host.id`. Strategy CHECK constraint extended to include `'push'`. The existing `cursor TEXT` column is sufficient for opaque cursor storage.
- **`interaction.source`** CHECK constraint extended to include `'messages'`, `'phone_calls'`, `'anarlog_sessions'`.
- **`sync_log`** — one entry per batch.
- **`event` table + River workers** — unchanged.

#### Source naming matrix

Canonical names per surface. v1 migrations align Mac-introduced sources with the existing convention (using the registered provider name as the source string everywhere data ingress occurs).

| Logical source (this spec) | Provider registry name | `external_sync_state.source` | `external_contact.source` | `interaction.source` | Event envelope `source` field | Frontend label |
|---|---|---|---|---|---|---|
| Google Contacts (existing) | `gcontacts` | `gcontacts` | `gcontacts` | n/a (entity sync, no interactions) | `gcontacts` | Google Contacts |
| Google Calendar (existing) | `gcal` | `gcal` | n/a | `gcal` | `gcal` | Google Calendar |
| Telegram (existing) | `telegram` | `telegram` | n/a | `telegram` | `telegram` | Telegram |
| Todoist (existing) | `todoist` | `todoist` | n/a | `todoist` | `todoist` | Todoist |
| Messages.app (new) | `messages` | `messages` | n/a (no contact entity sync) | `messages` | `messages` | Messages |
| Phone / FaceTime (new) | `phone_calls` | `phone_calls` | n/a (no contact entity sync) | `phone_calls` | `phone_calls` | Phone & FaceTime |
| iCloud Contacts (new) | `icloud_contacts` | `icloud_contacts` | `icloud_contacts` | n/a (entity sync) | `icloud_contacts` | iCloud Contacts |
| Anarlog Humans (new) | `anarlog_humans` | `anarlog_humans` | `anarlog_humans` | n/a (entity sync) | `anarlog_humans` | Anarlog Humans |
| Anarlog Sessions (new) | `anarlog_sessions` | `anarlog_sessions` | n/a | `anarlog_sessions` | `anarlog_sessions` | Anarlog Sessions |

**Note on the existing `external_contact.source='google'` value (currently in production data):** there is a naming gap between `external_contact.source='google'` and provider registry `gcontacts`. v1 does NOT propose renaming the existing column value; new sources align to the registry name, and the existing `'google'` value continues to function (with a documented inconsistency). A future cleanup migration could rename `'google'` → `'gcontacts'` for full alignment, scheduled when the team is ready for the data migration.

#### Sync provider registration

The 5 sources register in `ProviderRegistry` with new `Strategy=push` (added to existing `contact_driven`, `fetch_all`, `fetch_filtered`). For `push` providers:
- `Sync()` is a no-op.
- The scheduler's `ListDueAccounts` **explicitly excludes push-strategy providers**. Push providers never get enqueued by the periodic tick. Tested explicitly to prevent silent regression.
- The integrations UI treats push providers as first-class: shows last heartbeat-reported cursor, last-pushed-at, errors. Status is reported via heartbeat, not via internal sync logs.

#### Per-source rematch workers

Mirror Telegram's pattern. Run on `contact_method.created` (for phone/email-derived sources) and on `external_contact.upserted` (with `match_status=matched`) (for `anarlog_humans` → `anarlog_sessions` chain):
- `anarlog_sessions_rematch` — on `external_contact.upserted` (with `match_status=matched`) for source=anarlog_humans

**No `messages_rematch` worker.** Messages-source filtering happens **on the Mac daemon** (see "Sender filtering" + "Cold-start race recovery" under the `messages` source description). The daemon never forwards unmatched senders, so no staged-but-unmatched rows exist on the Pi to rematch. New `contact_method` rows are picked up by the daemon's next `known-identifiers` refresh, which triggers a 30-day backwards scan of chat.db for the newly-known sender.

**No `phone_calls_rematch` worker** — same reasoning as `messages`. The daemon's `known-identifiers` filter + 30-day backwards scan over `ZCALLRECORD` handles new-contact backfill identically.

#### Endpoints

```
POST   /api/v1/host                                # pairing token → host_id + api_key (unauthenticated; pairing-token-only)
POST   /api/v1/host/:id/heartbeat                  # rich status update; updates mac_host.last_heartbeat_at and per-source health
GET    /api/v1/host/:id/sync/:source/cursor        # daemon fetches {cursor, epoch, backfill_complete} for a source
POST   /api/v1/host/:id/sync/:source/cursor        # daemon commits new cursor after successful event push
GET    /api/v1/host/:id/sync/:source/known-ids     # returns [{source_id, last_content_hash}] for entities the Pi has for this (host, source). Used for tombstone reconciliation (entity in Pi-set but absent from daemon scan → daemon emits external_contact.deleted with previous content hash) and for delete source_id determinism (daemon uses the returned last_content_hash to build the deterministic delete source_id).
GET    /api/v1/host/:id/known-identifiers          # returns {phones: [...], emails: [...]} — canonicalized identifier set across ALL contact_method rows in the CRM (cross-source by design). Used by the `messages` source for daemon-side sender filtering: only senders matching this set are forwarded as raw_message.* events. Cold-start race handled via daemon-side 30-day backwards scan on newly-known identifiers — see "Sender filtering" + "Cold-start race recovery" under the `messages` source in section 2.
POST   /api/v1/ingest/events                       # existing endpoint, used by daemon for all data events; contract unchanged
GET    /api/v1/host/:id                            # for Pi UI
DELETE /api/v1/host/:id                            # user uninstall path; cascades source state
```

**Two-tier authentication.** Mac-related routes split into **daemon self-service routes** (authenticated as the host itself) and **admin/UI routes** (authenticated as the human user via the existing global API key).

1. `mac_host` schema stores `api_key_hash TEXT` (bcrypt hash) and `api_key_revoked_at TIMESTAMPTZ` columns. Plaintext host key returned once at pairing.
2. **Daemon self-service routes** — authenticated by new `MacHostAuthMiddleware`:
   - `POST /api/v1/host/:id/heartbeat`
   - `GET  /api/v1/host/:id/sync/:source/cursor`
   - `POST /api/v1/host/:id/sync/:source/cursor`
   - `GET  /api/v1/host/:id/sync/:source/known-ids`
   - `GET  /api/v1/host/:id/known-identifiers`
   - `POST /api/v1/ingest/events` (when `X-Mac-Host-ID` header is present)
   - Middleware reads `X-Mac-Host-ID` header and Bearer token; looks up `mac_host` by ID; verifies `api_key_revoked_at IS NULL`; validates token against `api_key_hash` (constant-time bcrypt compare); for `:id` parameter routes, asserts URL `:id` matches `X-Mac-Host-ID`.
3. **Admin/UI routes** — authenticated by the existing global `APIKeyMiddleware` (the Pi UI's existing auth surface):
   - `GET    /api/v1/host`         (list all registered Macs)
   - `GET    /api/v1/host/:id`     (Mac status detail for UI)
   - `DELETE /api/v1/host/:id`     (uninstall flow — also revokes host key by setting `api_key_revoked_at`)
   - `POST   /api/v1/host/pairing-token` (Pi UI requests a new pairing token to display)
   - `POST   /api/v1/ingest/events` (when `X-Mac-Host-ID` header is absent — other publishers continue using the global key)
4. **Pairing endpoint** (`POST /api/v1/host`) bypasses both — validates against the short-TTL pairing_token (separate table `mac_host_pairing_token` with `token_hash`, `expires_at`, `consumed_at`, `created_by_user`).
5. **Revocation:** UI's "uninstall Mac" flow calls `DELETE /api/v1/host/:id` which sets `api_key_revoked_at`. Daemon's next request returns 401; daemon detects, halts, and surfaces the error in `crm-mac status` and `os.Log`.

**Routing implementation:** the ingest endpoint's auth path is selected by presence of `X-Mac-Host-ID`. All other route paths are statically partitioned into the two tiers above by URL pattern.

#### Cursor protocol

Cursors are managed on a **dedicated endpoint** rather than baked into the ingest contract — this preserves the existing `/api/v1/ingest/events` wire contract that other publishers depend on.

**Cursor representation:** opaque `TEXT` per source. Pi never interprets the cursor value; it only stores and returns it. Per-source semantics:
- `messages`: cursor is a JSON object `{"backfill_cursor": "<value>", "live_cursor": "<value>", "backfill_complete": bool}` — backfill and live progress coexist. `backfill_cursor` walks **backward** from the install-time `MAX(ROWID)` toward 2026-01-01; `live_cursor` advances **forward** from the install-time max. When `backfill_complete=true`, only `live_cursor` is consulted on subsequent polls. Late-arriving historical rows (e.g., Messages sync from another device backfills past data) are picked up because the daemon polls from `live_cursor` forward only; rows older than `live_cursor` are not revisited, but the daemon emits them during the initial backward backfill pass. Acceptable tradeoff documented; v2 mutation-scan reader covers the long tail.
- `icloud_contacts`: cursor is the opaque `CNChangeHistory` token (Apple-managed, persisted as base64 string). Backfill is a single bulk fetch on first run; no separate backfill_cursor. Token expiration → daemon performs full resync: fetches all contacts via `CNContactStore`, fetches `GET /known-ids` from Pi (returning source_ids the Pi has for this source/host), computes set diff. Contacts in Pi-set but not in current scan → emit `external_contact.deleted` events. Contacts in current scan but not in Pi-set → emit `external_contact.upserted`. Contacts in both → emit `upserted` (content-hash dedup absorbs no-ops).
- `anarlog_humans`, `anarlog_sessions`: cursor is a JSON map `{uuid: {content_hash, mtime}}` snapshot of the last successful full scan. mtime is a cheap optimization (skip files with unchanged mtime), but **content_hash is the truth** — a file with same mtime but different hash IS re-pushed. Tombstones: UUIDs in previous cursor but absent from current scan are emitted as `*.deleted` events. On cursor-state loss (daemon reinstall, Pi restore, epoch mismatch), daemon fetches `GET /known-ids` from Pi and reconciles same as iCloud token-expiration flow.

**Daemon flow per source poll:**
1. `GET /api/v1/host/:id/sync/:source/cursor` → returns `{cursor: "X", epoch: 42, backfill_complete: true}`. Daemon caches both values locally.
2. Read source data from cursor `X` → produce events.
3. `POST /api/v1/ingest/events` with batch. Existing 4xx/5xx error handling applies. Returns `{accepted, duplicate, rejected, errors}`.
4. If batch outcome is "all rows accepted or duplicate; no rejections," daemon advances cursor: `POST /api/v1/host/:id/sync/:source/cursor` with `{cursor: "Y", epoch: 42, base_cursor: "X"}`.
5. Pi validates `epoch` matches current `mac_host.cursor_epoch` AND `base_cursor` matches stored cursor for that source. On match: stores new cursor `Y`. On mismatch: returns `409 Conflict` with `{current_cursor, current_epoch}`. Daemon refetches and retries from step 2.
6. If any events were rejected: daemon does NOT advance cursor. Next poll re-reads and re-pushes; staging-table unique constraints on `guid`/session UUID dedupe naturally.

**`cursor_epoch` semantics:** column on `mac_host`. Bumped manually (or via admin endpoint) whenever Pi is restored from backup or its sync state is reset. Forces daemons to refetch all cursors and reconcile. Protects against the "Pi restored to older state while daemon held newer cursor" failure mode.

**Cursor commit vs event materialization:**
- Stage 1 ingest is transactional per batch (existing behavior). In a single tx: insert raw event row(s); upsert staging row(s) with `processed_at = NULL`; enqueue aggregator River job(s) for affected contacts. All durable on commit.
- Aggregation (Stage 2) and interaction creation (Stage 3) happen asynchronously. Cursor safety does **not** depend on either succeeding; it depends only on Stage 1's tx committing.
- Cursor commit happens *after* successful ingest response. At that point, raw events + staging rows + queued aggregator jobs are all durable.
- If cursor commit fails after a successful ingest: next poll re-reads source from the old cursor and re-pushes the same events. Raw events dedup at `(source, source_id)` on the event table; staging rows dedup at the per-source unique constraint. Result: idempotent end-to-end.
- If Stage 2 (aggregator) fails before publishing: River retries the aggregator job. Rows remain `claimed_at = NULL AND processed_at = NULL`; clean retry.
- If Stage 3 (InteractionRecorder) fails: River retries that consumer against the same event row. Rows persist with `claimed_at IS NOT NULL AND processed_at = NULL`. If River exhausts retries, stale-claim recovery (aggregator's next pass after 5 min) re-enqueues a consumer job against the existing event by ID lookup (NOT by re-publishing — see Race mechanics).
- No "phantom cursor advance past unmaterialized data" risk because Stage 1 (the durability boundary) is atomic. No "lost messages" risk because `processed_at IS NULL` rows are the persistent claim that work remains.

#### Multi-host content-level dedup

Same message landing on two Macs is deduplicated at the staging-table unique constraint:
- `messages_message`: unique on `guid` (iMessage's stable per-message ID). First push wins; second push is a no-op insert (returns existing row).
- `anarlog_session_message`: unique on `anarlog_session_id`.

`mac_host_id` on staging rows is provenance only. Interactions are created once per matched message; subsequent hosts pushing the same content noop on staging insert.

#### Frontend additions

- **New "Mac" settings page:** lists `mac_host` rows (single host in v1, multi-host display when 2nd registers); per-host shows connection status from heartbeat age, version + protocol version, permissions state (FDA/Contacts/Files), source-level health, "uninstall" button (calls `DELETE /api/v1/host/:id`).
- **Five new sources auto-appear in the existing Integrations list.**
- **Contact detail timeline:** messages, Anarlog session interactions appear via existing `interaction.source` rendering; new source values get icons/labels.
- **Discovery UI:** existing Import page gets icloud + anarlog contacts via `external_contact.source` filter, no new tables needed.
- **No UI uses `identifier_type` directly** (verified via grep) — identifier_type is internal matching plumbing.

### 4. Identifier types — explicit per-source mapping

| Source | Identifier type | Rationale |
|---|---|---|
| `messages` (phone handles) | `phone` (generic) | Messages.app delivers both iMessage and SMS; `imessage_phone` is semantically inaccurate. `source='messages'` carries channel provenance. |
| `messages` (email handles) | `email` (generic) | Same reasoning. |
| `phone_calls` (voice + FaceTime audio) | `phone` (generic) | CallHistoryDB carries Continuity voice, native macOS Phone-app voice, and FaceTime audio uniformly; service tier recorded as a payload column, not in identifier type. |
| `phone_calls` (FaceTime video to Apple ID) | `email` (generic) | FaceTime video calls placed to an Apple ID surface as an email handle in `ZADDRESS`. Same generic mapping as messages. |
| `icloud_contacts` | n/a — matches via individual phone/email contact methods, same as `gcontacts` |
| `anarlog_humans` | `anarlog_human_id` (new) | UUID identity space, no contact_method mapping; pure storage hook for future session→human→contact resolution. |
| `anarlog_sessions` | (consumes) `anarlog_human_id` | Participant resolution flows through `external_identity` keyed on `anarlog_human_id`. |

**Existing types becoming unused:** `imessage_phone`, `imessage_email`. Cleanup migration removes them in a follow-up PR (no v1 dependency).

### 5. Failure modes & resilience

**Daemon crash:** launchd restarts. On startup: re-fetch cursors from Pi (including `cursor_epoch`), re-validate permissions, resume polling. No data loss; source data is durable.

**Pi unreachable:** poller tick fails fast (short timeout), logs to `os.Log`, retries next tick with bounded exponential backoff. No local queue.

**Mac asleep / closed lid / offline:** launchd suspends. On wake, `NSBackgroundActivityScheduler` triggers the next scheduled run. Catch-up via cursor-based incremental sync.

**Permission revoked:** reader emits structured permission error, marks itself unhealthy in heartbeat. `doctor` shows remediation. Other sources unaffected.

**Source schema drift (chat.db, Anarlog):** reader detects via `PRAGMA table_info` (SQLite) or whitelist-parsing failure (markdown). Marks unhealthy, includes specific column-name diff in heartbeat error. Ships a daemon update to fix.

**Pi cursor / Mac cursor disagree:** governed by Cursor Protocol — `cursor_epoch` + `base_cursor` precondition. Daemon refetches on `409 Conflict`; safe even if Pi was restored mid-flight.

**Duplicate ingestion:** staging-table unique constraints on content-level IDs (`guid`, anarlog UUID). Replays are safe noops. The event table's `UNIQUE (source, source_id) WHERE source_id IS NOT NULL` partial index (migration 036) provides a second idempotency layer at the event-log level.

**Backfill in progress + new live events:** Telegram's `backfill_cursor` + `live_cursor` pattern. Backfill completes at 2026-01-01 floor.

**Multiple Mac hosts pushing simultaneously:** first-to-push wins at staging unique constraint; subsequent hosts noop. Cursors are per-`(source, host_id)` so each host advances independently, but interactions are content-deduped.

**Scheduling determinism (NSBackgroundActivityScheduler):** macOS may coalesce or defer background activity for energy reasons, especially on battery. v1 accepts this tradeoff — we're already comfortable with Todoist-tier latency (~1-2 min). If determinism becomes a problem (e.g., users consistently see >5min lag on AC power), fall back to `Timer` + `DispatchSource` + explicit `ProcessInfo.beginActivity` markers. Heartbeat tracks actual schedule vs target so this is observable.

**Version skew:** daemon's `protocol_version` reported in heartbeat. Pi defines `MIN_PROTOCOL_VERSION`; older daemons get a `412 Precondition Failed` with upgrade instructions. Skew is observable in the Pi UI ("Mac daemon version 1.2 is incompatible with this Pi version; upgrade with `crm-mac install --upgrade`").

**Daemon-Pi clock skew:** all event timestamps come from source data (chat.db, file mtimes, Anarlog frontmatter). Daemon clock is only used for `last_heartbeat_at` display purposes; minor skew is acceptable. Pi never trusts daemon-supplied "now."

### 6. Observability

**Heartbeat payload (POSTed every ~60s):**
```json
{
  "host_id": "uuid",
  "hostname": "<mac-hostname>",
  "daemon_version": "0.3.1",
  "protocol_version": 1,
  "cursor_epoch": 42,
  "permissions": {"fda": true, "contacts": true, "files_anarlog": true},
  "sources": {
    "messages": {
      "enabled": true,
      "last_scheduled_at": "2026-05-11T15:30:00Z",
      "last_pushed_at":   "2026-05-11T15:30:02Z",
      "observed_cursor":  104289,
      "pushed_cursor":    104289,
      "schema_version":   "chat_db_v1",
      "backfill_complete": true,
      "last_error":       null,
      "last_error_at":    null
    },
    ...
  }
}
```

**Mac-side:** `crm-mac status` (instant local view), `crm-mac logs --source X --follow`, `crm-mac doctor`. All logs to `os.Log` with subsystem `com.spengrah.crm-mac`.

**Pi-side:** new Mac settings page surfacing heartbeat fields. Existing integrations UI shows per-source last-pushed and errors. New cursor-lag visualization: `observed_cursor - pushed_cursor` per source highlights stuck pushes.

**Alerting:** none in v1. UI cues are sufficient (single user). Future hook: Pi emits toast via #196 infrastructure when any source's last-pushed lag exceeds threshold.

---

## Phasing

### v1 — Daemon framework + `messages` + `icloud_contacts`

Validates the trickiest cross-cutting concerns (FDA permission, container allowlist, pairing flow, event-bus generalization).

**Daemon shell (Swift):**
- SPM scaffold, `crm-mac` binary with `daemon` and CLI subcommands
- `SMAppService` registration, install/uninstall/configure flow
- Keychain integration, config file, `state.json` cache
- `URLSession` Pi client with auth, retries, chunked uploads
- `NSBackgroundActivityScheduler` per-source registration
- `os.Log` integration
- `crm-mac doctor` Pi-reachability + FDA + Contacts probes
- Pairing token flow (Pi UI generates → daemon consumes)

**Pi side:**
- `mac_host` table + endpoints (`POST /host` with pairing token; heartbeat; DELETE; GET)
- Cursor protocol implementation (`cursor_epoch`, `base_cursor` validation)
- `sync_strategy=push` added to enum; scheduler `ListDueAccounts` exclusion + regression test
- **Shared aggregation extraction:** Telegram's `backend/internal/telegram/aggregation.go` (and its `interactionExtender` interface) extracted into `backend/internal/messaging/aggregation` with a source-neutral interface. The `messageRepo` dependency becomes a source-neutral interface implemented by per-source staging repositories (`messages_message`, existing `telegram_message`). Telegram's existing call site swapped to use the shared package — semantic behavior preserved verbatim. The aggregator continues to handle burst-windowing, reply bridging, direction inference, and the two-path output (extend vs create) identically across sources.
- **Ingest service generalization:** receives Mac-emitted events at `/api/v1/ingest/events`. For `raw_message.*` events, per batch in single tx: (1) match identifier → contact_id via existing `IdentityService.MatchOrCreate`; (2) upsert staging rows with `processed_at = NULL` (dedup by Guid); (3) record event rows; (4) enqueue aggregator River jobs deduplicated by `(contact_id, source)`. Stage 2 aggregator + Stage 3 InteractionRecorder run async; both already exist for Telegram (with minor source-neutralizing refactor). For `external_contact.*` and `meeting_note.*` events, ingest service does all domain work inline in the same tx — no async stages.
- **Aggregator River job scheduling:** new River job kind `MessagingAggregateForContact(contact_id, source)`. Uniqueness key = (contact_id, source). When ingest stages new rows, it enqueues this job; if a job with the same key is already pending/running, the enqueue is a no-op (River's built-in dedup). Each job processes all **eligible** rows for that (contact, source) pair — the claim-aware filter `processed_at IS NULL AND (claimed_at IS NULL OR claimed_at < NOW() - INTERVAL '5 minutes')`. The job exits when no eligible rows remain; rows currently claimed by a pending create-event are intentionally skipped and will be drained by Stage 3's post-commit re-enqueue (or by stale-claim recovery if Stage 3 fails). This ensures per-contact serialization across batches.
- **InteractionRecorder generalization:** the existing consumer is extended to use `StagingRepo.MarkProcessed(ctx, source, ids, interaction_id)` — a new source-neutral repository method that dispatches to the right per-source staging table. Today the consumer directly calls `telegram_message.MarkMessagesProcessed`; the refactor replaces this with the dispatching method without changing the consumer's flow semantics.
- `interaction.source` CHECK constraint migration: add `messages`
- **`GET /api/v1/host/:id/known-identifiers` endpoint:** returns `{phones: [...], emails: [...]}` — canonicalized phone/email identifier set across ALL contact_method rows (cross-source by design). Used by the `messages` source for daemon-side sender filtering. Body added in this phase; see "Sender filtering" + "Cold-start race recovery" under the `messages` source in section 2 for the daemon-side consumption contract.
- `external_sync_state.strategy` CHECK constraint migration: add `push`
- New Mac settings page in Next.js frontend

**`messages` source:**
- chat.db reader (GRDB.swift, read-only)
- `messages_message` staging table (unique on `guid`) + migration + sqlc queries + repository
- Event publisher: `raw_message.received` / `raw_message.sent` events to `/api/v1/ingest/events` (Pi-side aggregator transforms into aggregated `message.received`/`message.sent`)
- **Daemon-side `known-identifiers` filter:** the daemon fetches `GET /known-identifiers` on every heartbeat, caches the response, and only forwards chat.db rows whose sender matches a known phone/email. Non-matched senders never leave the Mac.
- **30-day backwards scan on newly-known identifiers:** when a `known-identifiers` refresh diff shows new identifiers, the daemon performs a one-time chat.db scan over the last 30 days for those senders and forwards any matches; `(source, source_id)` event-log dedup absorbs overlap with live-cursor emissions.
- Backfill to 2026-01-01 with `backfill_cursor` pattern (filter applies — only matched senders forwarded)
- Identity matching: Pi-side uses existing matcher with generic `phone`/`email` types; effectively trivial since the daemon pre-filters, but kept as a defence-in-depth fallback
- No `messages_rematch` worker on the Pi — daemon-side filter + backwards scan replaces it
- Group-chat attribution: sender-only interactions (no per-participant)
- Edits/deletes/reactions: accepted as v1 limitation; documented in plan

**`icloud_contacts` source:**
- `CNContactStore` reader with `CNChangeHistoryFetchRequest`
- Container allowlist picker (`crm-mac install` and `crm-mac configure`)
- Event publisher: `external_contact.upserted`, `external_contact.deleted`
- Ingest service extended to handle `external_contact.upserted` / `external_contact.deleted` events inline for `source='icloud_contacts'` (no separate consumer)
- Token expiration → full resync + tombstone reconciliation
- Reuses existing Import UI via `external_contact.source='icloud_contacts'`

**Value at end of v1:** `last_contacted` auto-updates from iMessage/SMS via Messages.app; iCloud contacts surface as import candidates and enrich existing CRM contacts.

### v1 PR-by-PR decomposition

v1 ships across 8 PRs. PRs 1-5 are Pi-side (Go); PRs 6-8 are Mac-side (Swift). Each PR is independently mergeable and reviewable.

#### Pi side (merged)

| PR | # | Scope |
|---|---|---|
| PR1 | [#301](https://github.com/spengrah/PersonalCRM/pull/301) | `mac_host` table + pairing/auth/heartbeat endpoints, `MacHostAuthMiddleware` |
| PR2 | [#302](https://github.com/spengrah/PersonalCRM/pull/302) | Extract Telegram burst aggregator into source-neutral `backend/internal/messaging/aggregation` |
| PR3 | [#303](https://github.com/spengrah/PersonalCRM/pull/303) | `messages_message` staging table, claim-aware aggregator, `messages` source registration |
| PR4 | [#304](https://github.com/spengrah/PersonalCRM/pull/304) | Ingest service generalization, per-event savepoint pattern, `messages` push provider, host-auth dispatch middleware, `events.ValidatePayload` boundary |
| PR5 | [#305](https://github.com/spengrah/PersonalCRM/pull/305) | `external_contact.upserted` / `.deleted` events (inline ingest), `external_contact.deleted_at` soft-delete + query sweep, `GET /host/:id/known-identifiers` body, `icloud_contacts` push provider stub |

#### Mac side (remaining)

##### PR6 — Daemon skeleton + scheduler shell + CI

**Repo location:** new `mac-daemon/` directory at repo root (sibling to `backend/`, `frontend/`).

**Package structure:** SPM package at `mac-daemon/Package.swift`. Thin-shell-over-pure-logic per the Testing & CI section — executable target stays minimal; testable logic lives in library targets.

**Targets:**
- `crm-mac` executable (CLI entrypoint, `swift-argument-parser`)
- `CRMMacCore` library (pure-logic: cursor mgmt, state file, source-plugin protocol)
- `CRMMacPiClient` library (`URLSession`-based, pure protocol-level — no system deps)
- `CRMMacSystem` library (Keychain, `SMAppService`, `NSBackgroundActivityScheduler`, `os.Log` adapters — thin shells)
- Test targets matching each library

**CLI surface** (subcommands stubbed; only `install`, `uninstall`, `doctor`, `status` functional in PR6):
- `crm-mac daemon` — long-running daemon process (in PR6: registers stub no-op `NSBackgroundActivityScheduler` jobs, runs heartbeat loop, exits cleanly on SIGTERM)
- `crm-mac install --pair <token>` — exchange pairing token for API key (Pi side issues via new `crm-api pair --host <name>` CLI), store in Keychain, register `SMAppService` login item
- `crm-mac uninstall` — unregister `SMAppService`, optionally purge Keychain + state
- `crm-mac configure` — stubbed; full impl in PR8 (container allowlist picker)
- `crm-mac doctor` — Pi reachability probe (hits `/host/:id/known-identifiers` with stored key), Keychain access check, `SMAppService` status check
- `crm-mac status` — print last heartbeat, scheduler state, current cursors

**Pairing token UX:**
- New `crm-api pair --host <name>` subcommand on the Pi side (Go) issues a short-lived pairing token by calling `POST /host` from PR1; prints token to stdout.
- User runs `crm-api pair --host mac-1` on the Pi, copies token, runs `crm-mac install --pair <token>` on the Mac.
- (Web UI button is a follow-up polish item per Open Questions.)

**Auth + storage:**
- API key stored in macOS Keychain (`kSecClassGenericPassword`, service = `xyz.spengrah.crm-mac`, account = host ID).
- `state.json` at `~/Library/Application Support/crm-mac/state.json` — JSON file with per-source cursor primitives (read/write only; no cursor advancement logic yet — that's PR7/8).

**Pi client:**
- `URLSession`-based; auth header from Keychain.
- Retry policy: exponential backoff on 5xx, give up on 4xx (no chunked uploads in PR6 — that's PR7's batch event push).
- Smoke test in `doctor`: `GET /host/:id/known-identifiers` returns 200 with valid auth.

**Lifecycle:**
- `SMAppService` agent registration in `install`, unregister in `uninstall`.
- Ad-hoc codesign (`codesign -s -`) in build step; no Developer ID, no notarization. First-launch gatekeeper bypass via right-click-Open documented in install output.

**Logging:** `os.Log` with `Logger(subsystem: "xyz.spengrah.crm-mac", category: <component>)`. Privacy-aware redaction for any identifier data (`.private` interpolation).

**Scheduler shell:**
- `NSBackgroundActivityScheduler` registration with one stub no-op job per planned source (`messages`, `icloud_contacts`).
- Defines `SourcePlugin` protocol that PR7 (`messages`) and PR8 (`icloud_contacts`) conform to:
  ```swift
  protocol SourcePlugin {
      var identifier: String { get }
      var schedulerInterval: TimeInterval { get }
      func performTick(context: SourceContext) async throws
  }
  ```
- `SourceContext` carries Pi client, cursor read/write, logger.
- **Data sources conform to `DataSourcePlugin`, a sub-protocol of `SourcePlugin`** (added later). `DataSourcePlugin` requires `mutator`/`clock`/`logger` + a `performTick()` hook; its protocol extension provides the `tick()` witness that auto-persists the per-tick liveness heartbeat (`state.sources[id].lastScheduledAt`) before delegating to `performTick()`, so a data plugin cannot forget the on-disk heartbeat. Conformers implement `performTick()` ONLY — declaring a concrete `tick()` would shadow the extension default and silently disable the heartbeat (enforced by the conformance suite + a `func tick(`-ban grep). Operational loops (`HeartbeatLoop`, the notification reconcile loop) stay on the bare `SourcePlugin`. The bare snippet above is the base contract; the in-tree `SourcePlugin` shipped as `id`/`tickInterval`/`tick()`.

**CI:**
- New `mac-daemon-tests` job inside `.github/workflows/ci.yml` on `macos-15` (pinned).
- Gated on a `mac_daemon` paths filter that triggers on `mac-daemon/**` or the `ci.yml` workflow file itself.
- Runs `swift build -c release -Xswiftc -warnings-as-errors` and `swift test`.
- No additional secrets required.

**Tests:**
- Unit tests for pure-logic modules: cursor read/write, source-plugin protocol contracts, Pi client request shaping (mocked `URLSession`).
- Thin-shell modules (Keychain, `SMAppService`, `NSBackgroundActivityScheduler`) covered by smoke tests that exercise the system API surface but tolerate sandboxed CI limitations.
- `doctor` integration test against a mock Pi (local HTTP server in-process).

**Definition of done:**
1. On a fresh macOS machine: `crm-api pair --host mac-1` → `crm-mac install --pair <token>` → daemon is registered as a login item, has Pi-side API key in Keychain, `crm-mac doctor` reports green.
2. `crm-mac daemon` runs without crashing; stub scheduler jobs fire on schedule and log a no-op tick.
3. `swift test` passes on `macos-15` in CI.

**Out of scope (deferred to PR7/PR8):**
- chat.db reader, 30-day backwards scan (PR7)
- `messages_message` event publishing (PR7 — Pi side already accepts these from PR3/PR4)
- `CNContactStore` reader, change-history fetching (PR8)
- Container allowlist picker (PR8)
- `external_contact.upserted`/`.deleted` event publishing (PR8 — Pi side already accepts these from PR5)
- Backfill, tombstone reconciliation
- Developer ID signing, notarization

##### PR7 — `messages` source

Anchor only; full scope brainstormed before PR7 kicks off.

- GRDB.swift integration; chat.db read-only reader.
- Daemon-side `known-identifiers` fetch + cache + sender filter.
- Live cursor (event-stream tail) + 30-day backwards scan on newly-known identifiers.
- Backfill to 2026-01-01.
- Event publishing: `raw_message.received` / `raw_message.sent` to `POST /api/v1/ingest/events`.
- Conforms `MessagesSource` to `DataSourcePlugin` (the heartbeat-persisting sub-protocol of `SourcePlugin` from PR6).
- Pi-side bits already merged in PR2/PR3/PR4.

##### PR8 — `icloud_contacts` source

Anchor only; full scope brainstormed before PR8 kicks off.

- `CNContactStore` reader with `CNChangeHistoryFetchRequest`.
- Container allowlist picker UX in `crm-mac install` and `crm-mac configure`.
- Token expiration → full resync + tombstone reconciliation.
- Event publishing: `external_contact.upserted` / `external_contact.deleted` to `POST /api/v1/ingest/events`.
- Conforms `ICloudContactsSource` to `DataSourcePlugin` (the heartbeat-persisting sub-protocol of `SourcePlugin` from PR6).
- Pi-side bits already merged in PR5.

### v1.5 — Phone & FaceTime call history

Small follow-on to v1 that reuses every cross-cutting concern v1 paid for (FDA permission, pairing, `MacHostAuthMiddleware`, ingest-service inline-event pattern from `meeting_note`/`external_contact`, `known-identifiers` filter, push-strategy provider scheduling, Mac settings page). Adds one Swift source reader, one staging table, one set of event kinds, one migration. No aggregator changes (calls are 1:1 with interactions; no burst-windowing).

**Daemon:**
- Swift `PhoneCalls` reader (GRDB.swift, read-only): opens `~/Library/Application Support/CallHistoryDB/CallHistory.storedata` through `SQLiteSnapshotReader.readOnlyURI(for:)` (`?mode=ro`, NOT `immutable=1` — macOS does not checkpoint CallHistoryDB's WAL frequently, so an immutable reader would miss days of calls); back-off on `SQLITE_BUSY`.
- Schema validation via `PRAGMA table_info` on `ZCALLRECORD` at startup; mark reader unhealthy on column-name drift. Required columns at minimum: `ZUNIQUE_ID`, `ZDATE`, `ZADDRESS`, `ZORIGINATED`, `ZANSWERED`, `ZDURATION`, `ZSERVICE_PROVIDER`, `ZCALLTYPE`, `ZHASMESSAGE`. The reader must explicitly check `ZHASMESSAGE` is present (voicemail semantics depend on it) and refuse to start if missing (Phone/FaceTime DB has migrated in past macOS releases — defence-in-depth).
- Cursor: `MAX(ZDATE)` from `ZCALLRECORD`. Same `backfill_cursor` + `live_cursor` JSON shape as `messages`.
- Backfill to 2026-01-01.
- Reuses the daemon-side `known-identifiers` filter + 30-day backwards-scan logic from v1's `messages` source (extract into a shared helper if not already factored out in PR7).
- Service enum derivation: `ZSERVICE_PROVIDER + ZCALLTYPE` together — see source description above. Reader must reject unknown combinations with a structured warning rather than silently emitting a wrong service tag.
- Event publishing: `call.received` / `call.sent` to `POST /api/v1/ingest/events`. Payload includes `has_voicemail` (`ZHASMESSAGE`), `answered` (`ZANSWERED`, inbound only), `duration_seconds` (`ZDURATION`), `service`, `direction`, `started_at`, `peer_handle`, `peer_normalized`, `call_unique_id`. All inbound rows that survive the known-identifiers filter are forwarded — including missed-no-voicemail — so the Pi can decide interaction creation centrally rather than the daemon embedding cadence policy.
- Conforms `PhoneCallsSource` to `DataSourcePlugin` (the heartbeat-persisting sub-protocol of `SourcePlugin` from PR6).

**Pi backend:**
- New `phone_call` staging table (globally unique on `call_unique_id`, `interaction_id` nullable).
- New event kinds: `call.received`, `call.sent`. Ingest service handles both **inline (Stage 1 only)** — the same pattern used for `meeting_note.recorded` and `external_contact.upserted`. No new River jobs.
- **Conditional interaction creation:** ingest service consults the decision table from the `phone_calls` source section. Missed-inbound-no-voicemail rows produce a `phone_call` staging row (matched, `processed_at` set) but no `interaction` and no `interaction.recorded` event, so the cadence-updater consumer never sees them and `last_contacted` is unaffected.
- `interaction.source` CHECK constraint extension: add `phone_calls`.
- `ProviderRegistry` registration for `phone_calls` (Strategy=push); scheduler exclusion already wired in v1.
- Frontend: `phone_calls` auto-appears in the Integrations list and Mac settings page via the existing rendering paths. New icon/label for `interaction.source='phone_calls'` in the contact timeline, plus a "missed call" timeline projection that surfaces `phone_call` rows where `interaction_id IS NULL AND matched_contact_id IS NOT NULL` (visually distinct from interaction-backed rows; clearly labeled as cadence-inert).

**Out of scope for v1.5:**
- Voicemail audio + transcript on the contact timeline. Mac has no local copy (verified: no `~/Library/Voicemail/`, no `*.amr` files, no transcript columns in `ZCALLRECORD`). Surfacing voicemail content requires an iOS companion source — deferred indefinitely.
- Delete handling. `ZCALLRECORD` rows are append-only in normal use; the Phone app's "Clear All Recents" deletes rows but v1.5 treats deletes as out-of-scope (interactions remain in the CRM). Same v2 mutation-scan reader pattern as `messages` will cover this when it lands.

**Outcome:** Phone and FaceTime calls (Continuity-mirrored, native macOS Phone app, FaceTime audio/video) appear on contact timelines with duration + service tier. Answered inbound calls and voicemail-leaving missed calls bump `last_contacted`; missed inbound calls without voicemail appear on the timeline as "missed call" but do not affect cadence (no content was delivered). Outbound calls — connected or not — bump `last_outreach_at` only.

### v2 — Anarlog (humans + sessions)

> Matching and discovery logic for `anarlog_sessions` is refined in [`mac-daemon-phase-2-anarlog-matching.md`](./mac-daemon-phase-2-anarlog-matching.md). The bullets below describe the daemon-side readers and the baseline Pi-side wiring; the sidecar spec supersedes the "participant resolution + per-matched-participant interaction creation" bullet with a fuller design that handles session↔calendar linkage, untagged orphans, title parsing, and conflict resolution.

- File-format inspection sub-task (research already done; see `.ai/log/plan/mac-daemon-research-findings.md`).
- Swift readers (`AnarlogHumans` mtime-based, `AnarlogSessions` FSEvents + safety poll).
- New identifier type `anarlog_human_id` in `external_identity` enum.
- `external_contact.source='anarlog_humans'` (no new contact table).
- New `meeting_note` staging table + `meeting_note.recorded` / `meeting_note.deleted` event kinds.
- Ingest service extended to handle `meeting_note.*` events inline (participant resolution + per-matched-participant interaction creation + `interaction.recorded` emit, all in the ingest tx). No separate consumer.
- Tombstone events (`external_contact.deleted`, `meeting_note.deleted`).
- `anarlog_sessions_rematch` worker on `external_contact.upserted` (with `match_status=matched`) for source=anarlog_humans.

---

## Open questions

Items deferred to implementation phase (genuine design decisions, not unresolved questions):

- **Exact `messages_message` column list** — implementation plan derives from the chat.db schema captured in research findings.
- **Anarlog folder path UX edge cases** — `doctor` validation rules when path is configured but doesn't exist, or contains no `humans/`/`sessions/` subdirectories.
- **Backfill progress UI** — Telegram pattern covers the data; whether to surface a progress bar in v1 is a small polish item.
- **Pairing token UI** — exact UX of token generation and display in the Pi web UI. Functional design: button → token displayed in modal with copy-to-clipboard + countdown timer.

Items intentionally **out of v1 scope, queued for follow-up specs:**

- **Edit/delete/reaction sync for messages** — needs a separate "mutation scan" reader pattern over a recent-N-day window.
- **Anarlog transcript ingestion** — speaker diarization required before transcripts become CRM-useful.
- **Cross-source unified discovery UI** — consolidate import candidates from multiple sources into a single ranked view.
- **Cleanup migration to remove dead `imessage_phone`/`imessage_email` identifier types** — non-blocking; data has no producers.

---

## References

**Issues this incorporates:**
- #73 — feat: iMessage integration
- #74 — feat: iCloud Contacts integration

**Existing patterns referenced:**
- `backend/internal/sync/provider.go` — `SyncProvider` interface, `ProviderRegistry`
- `backend/internal/api/handlers/ingest.go` + `backend/internal/service/ingest.go` — existing `/api/v1/ingest/events` ingest path
- `backend/internal/telegram/aggregation.go` — message-burst aggregation, reply bridging, direction-aware cadence; v1 extracts to shared package
- `backend/internal/identity/normalize.go` — identifier type mapping and matching
- `backend/migrations/014_external_contact.up.sql` — unified external_contact table
- `backend/migrations/032_telegram.up.sql` — telegram_message staging precedent
- `backend/internal/consumer/interaction_recorder.go` — currently telegram-only; v1 generalizes
- `.ai/patterns/sync.md` — sync pipeline architecture
- `.ai/spec/event-bus-foundation.md` — event-bus design context
- `.ai/spec/telegram-integration.md` — similar-shape integration spec
- `.ai/log/plan/mac-daemon-research-findings.md` — preliminary research findings (gitignored)
- PR #298 — versioned event payloads
