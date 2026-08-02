# WhatsApp Integration

Status: approved spec, ready for planning. Deepens and amends issue #363 (which replaced the closed Mac-daemon route, #299). Where this spec and #363 disagree, this spec wins.

## Context & problem

WhatsApp is the last significant communication channel not feeding the CRM. Contacts the user talks to on WhatsApp look neglected: their `last_contacted` never moves, cadence marks them overdue, and their timeline is empty. Telegram, iMessage/SMS, email, Google Chat, calendar, and phone calls all already feed the shared staging → aggregation → interaction pipeline; WhatsApp scaffolding exists in the identity layer (the `whatsapp` identifier type dual-mapping to `phone`, the `whatsapp` contact-method type with E.164 normalization) but nothing ingests messages, and `interaction.source = 'whatsapp'` is deliberately rejected today (with a test asserting the rejection — that test flips when this ships).

Why now: the LLM extraction program is the next major arc, and message content is its substrate. WhatsApp must land first so its conversations are part of the corpus extraction reads, not a retrofit.

## What matters (priority order)

1. **Full pipeline parity.** WhatsApp interactions feed interactions, `last_contacted`, cadence, and the contact timeline exactly as Telegram does — same aggregation semantics (bursts, sessions, reply bridging), same sole-writer recorder, same downstream consumers, zero new pipeline.
2. **Message content is captured, not just metadata.** Content lands in the shared cross-source store so the upcoming LLM extraction work reads one substrate. This is the driver of "now" and makes content storage a first-class requirement.
3. **Read-only, ban risk accepted.** The integration never sends messages or performs any user-visible outbound action. The account-risk profile of an unofficial linked-device client is understood and accepted (precedent: the Telegram MTProto client, incident-free). Graceful degradation on session death is desirable but not a core requirement — few contacts are WhatsApp-exclusive.
4. **History backfills to the start of 2026, same horizon as every other source.** No deeper. Backup-file parsing for pre-2026 history is explicitly out.

## Goals

- Ingest 1:1 and group WhatsApp messages for the user's own personal account via a linked-device client, matched to contacts through the existing identity layer.
- Store message content in `comms_message`; aggregate into interactions via the existing source-neutral aggregation engine.
- Group handling at parity with Telegram: person-to-person plus small-group tracking gated by a member-count threshold; same only-person-to-person ingestion semantics Telegram's spec defines.
- Discovery and enrichment at parity with Telegram: frequent unmatched peers surface as import candidates; contacts matched by phone gain a `whatsapp` contact-method enrichment path.
- One-time bounded history backfill on link, clamped to the 2026-01-01 horizon.
- Settings surface at parity with Telegram: pair (QR or phone pairing code), status, disconnect; feature gated by environment configuration.

## Non-Goals

- Sending messages, marking as read, or any outbound/user-visible action on the WhatsApp account. Read/ingest only.
- The official WhatsApp Business/Cloud API (structurally cannot read a personal account's chats).
- Matrix bridge layers (mautrix-whatsapp) or any Mac-daemon involvement.
- Pre-2026 history; parsing `msgstore.db` / iOS backups.
- Media content download. Media messages are ingested as typed metadata (photo/audio/video/document labels), mirroring the existing sources; fetching media bytes is out of scope.
- New aggregation semantics of any kind.

## Relation to existing & planned work

- **#363 (open)** — the direction this spec deepens. Two amendments: content stages into `comms_message`, not a bespoke `whatsapp_message` table (see Architectural direction); the device-store decision #363 left open is ruled here (bundled `sqlstore`). #363 should be closed in favor of this spec when planning starts.
- **#456 (open)** — documents `comms_message` as the default staging store for new chat-like sources and proposes extracting the source-parameterized aggregation adapters GChat already carries. Natural precursor or companion to the first WhatsApp PR; at minimum WhatsApp must not add a fourth copy of the duplicated adapter glue.
- **#450 (open)** — collapse of the six source-keyed wiring registries into a per-source bundle; its body says it ideally lands before or with the first WhatsApp PR. Leaning, not a mandate: the planner may sequence WhatsApp without it, but must address the wiring-tax consequence explicitly.
- **#364 / #365 (Signal, Farcaster)** — siblings that inherit whatever path WhatsApp establishes; a reason to prefer the shared-store, shared-adapter route.
- **LLM extraction program (`.ai/spec/llm-extraction-program.md`)** — the downstream consumer motivating content capture. This spec only needs to leave WhatsApp content in the shared store; extraction design stays in its own spec.
- **Telegram integration (`.ai/spec/telegram-integration.md`, `spec/telegram.yaml`)** — the reference pattern for the client lifecycle, auth flow, group gating, discovery, rematch, backfill horizon, and settings UI. `spec/telegram.yaml` is the template for the new behavior domain this work must mint (`spec/whatsapp.yaml`, settled on both `ui` and `api` surfaces like every other domain).
- **`.ai/patterns/sync.md`** — still instructs new sources to create per-source staging tables and names `whatsapp_message`; that guidance is stale (per #456) and must be corrected when this ships so the pattern doc and the shipped route agree.
- **`.ai/spec/mac-daemon.md`** — its non-goals section already records the pivot to the on-Pi whatsmeow route; no change needed beyond keeping the issue reference current.

## Prior art & external constraints (research 2026-08-02)

- **No official route exists.** The Cloud API is business-only and cannot read a personal account's chats; the personal-side export mechanisms (per-chat `.txt` export, account-info download) are manual and non-automatable. ([Meta platform docs](https://developers.facebook.com/documentation/business-messaging/whatsapp/about-the-platform), [WhatsApp export FAQ](https://faq.whatsapp.com/1180414079177245/?cms_platform=android))
- **whatsmeow (`go.mau.fi/whatsmeow`, MPL-2.0) is the mature Go-native linked-device client** — actively maintained (commits through July 2026), powers mautrix-whatsapp and Beeper in production. Alternatives are strictly worse for this stack: Baileys (Node sidecar), whatsapp-web.js (headless Chromium), mautrix-whatsapp (Matrix layers around the same library). ([whatsmeow](https://github.com/tulir/whatsmeow))
- **History sync is one-shot and capped.** The multi-device protocol delivers roughly 3 months (web-client request) to 1 year (desktop-client request) of history, once, shortly after linking; there is no re-request short of unlink/relink. whatsmeow exposes the request size via `HistorySyncConfig.FullSyncDaysLimit` (default 365). ([mautrix backfill docs](https://docs.mau.fi/bridges/general/backfill.html), [whatsmeow docs](https://pkg.go.dev/go.mau.fi/whatsmeow))
- **Ban risk is real, unquantified for passive personal use.** Since May 2025 WhatsApp has served "account may be at risk" warnings to whatsmeow/Baileys users, including low-volume ones, with some subsequent bans; the original Baileys author received a cease-and-desist in 2023. All shipped personal-CRM/chat-unifier products (Dex, Beeper) use this route anyway. Risk is accepted (priority 3); the read-only posture is also the risk-minimizing one, since community consensus ties enforcement to anomalous outbound automation rather than passive presence. ([whatsmeow#810](https://github.com/tulir/whatsmeow/issues/810))
- **Protocol churn is a normal operating condition.** WhatsApp rotates protocol details frequently; unofficial clients break and recover with library updates. The integration must treat "WhatsApp source is down pending a dependency bump" as an expected state, surfaced but not paged on — consistent with the existing sync-staleness watchdog posture.

## Hard constraints

- Read-only against the WhatsApp account: no message sends, no read receipts, no state mutations beyond what session maintenance itself requires.
- The linked-device session must survive `crm-api` restarts without re-pairing (session state persisted in Postgres).
- History backfill is clamped to 2026-01-01; the link-time history request must ask for the maximum window (365 days) since the opportunity is one-shot. Messages older than the clamp that arrive in the sync are discarded, not stored.
- All existing repo invariants apply unchanged: layered architecture, sqlc-only SQL, `accelerated.GetCurrentTime()`, soft-delete filtering, sole-writer `InteractionRecorder`, `KnowledgeCacheUpdater` untouched.
- Identity matching reuses the existing `whatsapp` identifier type and its dual-mapping to `phone`; no new matching machinery.
- `interaction.source` gains `whatsapp` (CHECK constraint + Go constants + flipping the existing rejection test), and the new behavior domain `spec/whatsapp.yaml` lands with its citing tests per the spec-coverage rules.
- Feature is off unless explicitly enabled via environment configuration (mirroring `ENABLE_TELEGRAM_SYNC`), so environments without credentials/pairing are unaffected.

## Architectural direction

- **Constraint — in-process whatsmeow client inside `crm-api` on the Pi**, managed like the Telegram client (long-lived manager, event handlers, reconnect with backoff). No new process, no new OS container, no daemon involvement.
- **Constraint — content stages into `comms_message`** (the shared cross-source store email and GChat already use), via a `SourceAdapter` for the existing aggregation engine. No `whatsapp_message` table, no new sqlc query file for staging, no new repository. Stable per-message source refs and per-chat peer refs follow the conventions the other comms sources established.
- **Constraint — whatsmeow's device store uses the bundled `sqlstore.Container` pointed at the shared Postgres.** The library owns its `whatsmeow_*` tables and migrations. The device credentials therefore sit unencrypted at rest — an accepted, deliberate divergence from Telegram's encrypted session blob, ruled acceptable for a single-user Pi behind Tailscale on owned hardware.
- **Constraint — reuse, don't fork:** aggregation engine, `InteractionRecorder`, cadence updater, follow-up manager, event kinds, and rematch-dispatcher patterns are consumed unchanged. A `whatsapp` rematch handler registers for `whatsapp` and `phone` contact-method creation, mirroring Telegram's pair of handlers.
- **Leaning — land #456's shared-adapter extraction before or with the first WhatsApp PR**, and consider #450's bundle registration in the same window. The planner may re-sequence with evidence but must not silently copy the adapter boilerplate a fourth time.

## Success criteria

- After pairing, a WhatsApp message exchanged with a contact whose phone number is on file appears as an interaction on that contact within the normal aggregation latency, moves `last_contacted`/cadence, and shows on the timeline — with no manual steps.
- Message bodies for ingested conversations are present in `comms_message` and attributable to source `whatsapp`, queryable as extraction substrate.
- Linking backfills conversation history to 2026-01-01 where WhatsApp delivered it, and the observed backfill floor is verified and recorded after the first real link (this is also the empirical check on the one-shot history assumption).
- Group messages in tracked small groups produce interactions per the Telegram-parity rules; larger groups are ignored.
- A frequent WhatsApp peer with no matching contact surfaces as an import candidate; linking or importing them retroactively matches their staged messages (rematch).
- The session survives backend restarts without re-pairing; a dead session (logout, ban, protocol break) degrades to a visible disconnected status in settings without affecting other sources.
- The settings page can pair, show status, and disconnect the WhatsApp account end to end.

## Desired behavior sketch (user-visible)

- Settings gains a WhatsApp section beside Telegram: connect (QR code or phone pairing code), connection status, disconnect. Disconnect unlinks the device and clears the session.
- Once connected, WhatsApp conversations flow in continuously; the user sees WhatsApp-sourced interactions on contact timelines and dashboards exactly like Telegram ones, labeled with their source.
- New people the user talks with on WhatsApp appear in the imports/candidates review queue once they cross the discovery threshold; accepting one creates/links the contact and pulls in their message history from staging.
- A contact matched via phone number can gain a `whatsapp` contact method through the existing enrichment-suggestion flow.

## Assumptions & deferred questions

- **Assumed:** requesting the 365-day history window at link time yields history reaching 2026-01-01. WhatsApp does not guarantee delivery depth; if the delivered floor falls short, the gap is accepted (deep history is a non-goal) — but verify and record the observed floor at first link, and treat link-time configuration as unrecoverable-if-wrong (re-link to retry).
- **Assumed:** peer JIDs resolve to phone numbers for identity matching. WhatsApp has been rolling out privacy-preserving LID (`@lid`) identifiers that can mask phone-number JIDs, especially in groups; whatsmeow carries mapping support. The planner must verify how reliably phone numbers are recoverable for the group-and-1:1 mix this spec covers, and what matching degrades to when they are not.
- **Assumed:** Telegram's group gating (member-count threshold config) translates to WhatsApp group metadata cleanly.
- **Deferred to planning:** env var names and config surface; exact source-ref formats; sequencing against #456/#450; pairing UX details (QR rendering in the settings UI vs pairing-code entry); rate/pacing behavior on initial history ingest; retention of the `spec/whatsapp.yaml` behavior inventory (mint by mirroring `spec/telegram.yaml`'s domains against actually-built behavior).
