# Mac Daemon Phase 2 — Anarlog Matching & Discovery

Sidecar to [`mac-daemon.md`](./mac-daemon.md). Refines the matching and discovery design for `anarlog_sessions` introduced in the parent spec's v2 section. Issue: [#74](https://github.com/spengrah/PersonalCRM/issues/74).

## Problem

The parent spec's v2 design matches Anarlog session participants only via explicit `anarlog_human_id` tags in `_meta.json`. In practice, sessions are routinely under-tagged — the user forgets to tag attendees, intends to come back and tag them later, or runs a session that started impromptu. The parent design therefore produces no interactions for any of these sessions, even when the same meeting is unambiguously visible in the calendar.

There is also a latent duplication risk: a calendar event for a scheduled meeting already produces one interaction per attendee. An Anarlog session covering the same meeting would, under the parent spec, produce a second set of interactions for the tagged humans — independent of and ignorant of the calendar interactions.

This spec defines how the Pi backend links sessions to existing calendar events / phone calls / FaceTimes, what it does when no link is possible, how it parses titles for matching and discovery, and how unresolvable cases are surfaced back to the user.

## Decisions

- **Unified interaction model.** When a session corresponds to an existing scheduled meeting/call, the calendar (or phone call) interaction is the canonical one. The session attaches metadata (summary, memo, future transcript pointer) via a foreign-key column on `meeting_note`. No second set of interactions is created for the same meeting.
- **Time-overlap is the primary linkage signal.** With participant overlap used as the tiebreaker when multiple candidates fall within the window.
- **Conservative title parsing.** Title-extracted name tokens never auto-create interactions on their own. They are used only for (a) disambiguating among multiple time-overlap candidates and (b) feeding the discovery candidates flow.
- **Mac-side orphan surfacing.** Untagged sessions with no time-overlap candidates produce no interactions and no Pi-side review queue. Instead the daemon raises a persistent macOS notification prompting the user to tag the session in Anarlog. The next sync picks up the change naturally.
- **Pi-side conflict surfacing.** Ambiguous time-overlap conflicts that can't be auto-resolved via participant signal are surfaced in the Pi `/imports` page under a new **Interactions** tab (conflicts + orphans).

## Data model

### `meeting_note` (additive)

```
linked_kind     TEXT NULL    -- 'event' | 'phone_call'   (extensible later)
linked_id       UUID NULL    -- polymorphic pointer; no FK constraint
linkage_state   TEXT NOT NULL
                             -- 'linked' | 'linked_impromptu'
                             -- | 'orphan_title_augmented' | 'orphan_needs_review'
                             -- | 'conflict_pending'
                             -- assigned by the ingest tx; no transient default

CHECK (linked_kind IN ('event', 'phone_call') OR linked_kind IS NULL)
CHECK ((linked_kind IS NULL) = (linked_id IS NULL))
```

The `linked_id` column is intentionally polymorphic and carries no FK. Tradeoff acknowledged: this loses referential integrity. Mitigations:

- A periodic cleanup job nullifies `(linked_kind, linked_id)` pairs whose target row no longer exists.
- Hard-deletes of `event` / `phone_call` rows are rare in normal use; soft-deletes leave the target row in place and the link semantically valid.

### `external_contact` (additive)

A new source value `anarlog_title` is added to capture weak title-extracted discovery candidates. Reuses the existing table — no new schema. `source_id` is `sha256(normalized_token || session_uuid)`. Multiple sessions surfacing the same name accumulate as separate `external_contact` rows; aggregation for the Imports UI happens at query time (group by normalized name token).

### `ingest.events` response (additive)

The existing batch response (`{accepted, duplicate, rejected, errors}`) gains an optional field:

```jsonc
"needs_attention": [
  { "session_id": "<uuid>", "reason": "orphan" | "conflict" }
]
```

The daemon consumes this to drive macOS notifications. `reason: "conflict"` notifications inform the user that a CRM-side decision is required (and could link the user to the relevant `/imports` view), but the conflict-resolution interaction itself happens in the Pi UI.

## Linkage detection algorithm

Runs inside the Pi ingest transaction when a `meeting_note.recorded` event is processed.

**Step 1 — Build candidate set.** Query the union of:

- `event` rows with `start_at` in `[session.started_at - 15min, session.started_at + 15min]`
- `phone_call` rows with `started_at` in the same window

Window is 15 minutes by default, configurable later.

**Step 2 — Resolve.**

- **0 candidates** → orphan flow (Step 4).
- **1 candidate** → auto-link. Set `(linked_kind, linked_id)`, `linkage_state = 'linked'`.
- **2+ candidates** → run participant-signal disambiguation (Step 3).

**Step 3 — Participant-signal disambiguation.** Used to break ties when 2+ time-overlap candidates exist.

1. Compute the session's *implied contact set*:
   - Resolve tagged `anarlog_human_id` participants → `contact_id` via `external_identity` lookup. Unresolved tags are dropped (an unmatched anarlog human can't help disambiguation).
   - Run title extraction (see [Title parsing](#title-parsing)) → for each extracted token, run a high-confidence single-match fuzzy lookup against `contact.full_name`. Include matched `contact_id`s. **Title tokens are used here for disambiguation only — never for creating interactions in this step.**
2. For each candidate, compute overlap count:
   - For `event`: `|implied_set ∩ event_attendees_resolved_to_contact_ids|`
   - For `phone_call`: 1 if the call's peer `contact_id ∈ implied_set` else 0
3. Decide:
   - If exactly one candidate has the strictly-highest non-zero overlap → auto-link, `linkage_state = 'linked'`.
   - Otherwise → `linkage_state = 'conflict_pending'`. No interactions created from this session. Add `{session_id, reason: 'conflict'}` to `needs_attention`.

**Step 4 — Orphan flow (zero candidates).**

- **Session has tagged humans:** create one interaction per resolved tagged contact (`source='anarlog_sessions'`, `source_ref='anarlog:<session-uuid>:<contact-uuid>'`). Run title extraction; tokens that high-confidence-match a contact also get an interaction (`source_ref='anarlog:<session-uuid>:title:<contact-uuid>'`). Unmatched tokens flow to discovery candidates. `linkage_state = 'linked_impromptu'` (or `'orphan_title_augmented'` if any title-derived interactions were added).
- **Session has no tagged humans:** **no interactions created.** Title extraction runs only for discovery (weak candidates). `linkage_state = 'orphan_needs_review'`. Add `{session_id, reason: 'orphan'}` to `needs_attention`.

**Step 5 — Walk-in supplemental interactions (linked sessions only).** When a session has been auto-linked and has tagged humans whose resolved `contact_id` is NOT in the linked event's attendee set, create one supplemental interaction per missing person (`source='anarlog_sessions'`, `source_ref='anarlog:<session-uuid>:walkin:<contact-uuid>'`). This captures walk-ins that the calendar didn't know about. We do NOT delete calendar interactions for invitees who didn't show up — too risky given imperfect tagging.

## Title parsing

Runs Pi-side. All steps are heuristics, not NLP.

### Extraction

1. Strip common meta-tokens case-insensitively: `1:1`, `sync`, `catchup`, `chat`, `call`, `meeting`, `intro`, dates (`2026-05-12` etc.), weekday names, month names.
2. Split on separators: `/`, `&`, ` and `, ` with `, ` w/ `, `+`, `:`, `-`.
3. Keep remaining tokens that are: capitalized first letter, alphabetic only, length 2–30.
4. Drop tokens in a small stopword list (project codenames are inherently hard — accept some leak-through, which the discovery flow handles gracefully via accept/dismiss).

### Use in matching

Per [Decisions](#decisions), title-extracted tokens NEVER produce an interaction in isolation. They are consumed only by:

- **Step 3 disambiguation** above — tokens that yield a high-confidence single match against `contact.full_name` join the session's implied contact set for tiebreaking.
- **Step 4 orphan flow with tagged humans** — when the session already has ≥1 tagged human (the anchor), high-confidence title-token matches also produce interactions. The anchor confirms the title format is naming real people.

A token's "high-confidence single match" is decided by the existing `backend/internal/matching/` fuzzy matcher. If the matcher returns multiple plausible matches (collision-gap rule), the token is dropped from the implied set and from interaction creation.

**Implementation note:** the existing `matching/` package targets contact-method identifier matching. A name-string variant may need to be added (`MatchByFullName(token string) ([]ContactMatch, error)`). Trace this in implementation; don't bake a new abstraction prematurely.

### Use in discovery

For any extracted token that:

- did NOT yield a high-confidence single match against an existing contact, AND
- did NOT match any existing `external_contact source='anarlog_title'` row for the same normalized token,

upsert a new `external_contact` row with `source='anarlog_title'`, `source_id = sha256(normalized_token || session_uuid)`, and metadata including the session uuid as evidence. Repeated occurrences across sessions accumulate as additional rows; the Imports UI groups them by normalized token and ranks by evidence count (implementation detail).

## Interaction-creation rules (consolidated)

| Time-overlap state | Tagged humans | Title parsing role | Interactions created | `linkage_state` |
|---|---|---|---|---|
| 1 candidate | irrelevant | disambig (n/a), discovery only | None new; calendar/call interactions stand. Walk-in supplemental for tagged-but-not-in-attendees. | `linked` |
| 2+ candidates, participant-signal tiebreaker succeeds | irrelevant | disambig (used), discovery only | Same as 1-candidate | `linked` |
| 2+ candidates, no tiebreaker | irrelevant | disambig (tried), discovery only | None until user resolves in Pi UI | `conflict_pending` |
| Conflict resolved → "this one" | irrelevant | discovery only | Same as 1-candidate | `linked` |
| Conflict resolved → "none / impromptu" | falls through | falls through | falls through | falls through |
| 0 candidates | yes | augments interactions when title matches; otherwise discovery only | One per tagged human; one per title-matched contact (tagged humans act as anchor); unmatched tokens → weak candidates | `linked_impromptu` or `orphan_title_augmented` |
| 0 candidates | no | discovery only | **None.** macOS notification raised. | `orphan_needs_review` |

## Re-sync semantics

When a session file's content hash changes (`_meta.json`, `_summary.md`, or `_memo.md` updated), the daemon re-pushes `meeting_note.recorded` with the same session uuid. Pi-side:

1. Look up existing `meeting_note` row by uuid → carry forward `linkage_state` only if the new payload doesn't materially change matching inputs (started_at, tagged participants, title).
2. If matching inputs DID change, re-run the linkage algorithm from scratch. Compute the *desired* interaction set per the rules above.
3. Diff against the existing set of session-attributed interactions:
   - INSERT interactions in `desired \ existing`
   - Soft-delete interactions in `existing \ desired` with a deletion reason like `session_re_sync_dropped`. Manual interactions (not source='anarlog_sessions') are never touched.
4. Calendar / phone-call interactions are never modified by session re-sync — they are owned by their own providers.

This pattern protects against contact renames (a title-matched interaction's basis disappears) and against re-tagging (humans added or removed in Anarlog).

## Imports UI (PR 7)

Design finalized via Claude Design (handoff: `designs/handoffs/2026-05-anarlog-sessions-attention/`). Built on the **production** Tailwind palette (not the unshipped Aged Artisanal skin); no new color tokens. The handoff's `anarlog-sessions.jsx` is the 1:1 React reference and `IMPLEMENTATION-DIFF.md` the repo-side change list. Colour semantics: amber = needs attention/conflict; green = resolved/empty; blue = the matching signal (overlap pips, tagged people, time drift); gray-dashed = title-derived low-confidence.

### Information architecture

`/imports` gains two underline-style sub-tabs — **People** and **Interactions** — with tab state held in the URL. The conflict notification deep-links onto the Interactions tab. PR 7 makes **`interactions`** the canonical param end to end: a **companion daemon change** updates the click-URL builder (`mac-daemon/Sources/CRMMacOrphanNotifications/ClickTargetURL.swift`) to emit `/imports?tab=interactions&session=<anarlog_session_uuid>` (it currently emits `tab=needs-attention`). To stay safe across the deploy gap, the UI also accepts `needs-attention` as a **transitional** inbound alias, dropped in a follow-up once the renamed daemon is deployed. (The daemon computes the click URL at tap time, so once its binary updates even already-delivered persistent notifications emit the new value — there is no permanent reason to keep the alias.)

- **People** (default tab; param `people`) — the single home for "people to add". The existing unmatched-candidate cards, now including `anarlog_humans` (exposed via a new `Anarlog` entry in `SOURCE_FILTERS` + `source-display.ts`). Below the candidate list and its pager sits a distinct **"Names found in session titles"** discovery section for `anarlog_title` tokens (see Discovery below).
- **Interactions** (param `interactions`; see the deep-link note above for the transitional `needs-attention` alias) — currently the actionable, interaction-blocking Anarlog states `conflict_pending` and `orphan_needs_review`. Named generally on purpose: this tab is the intended future home for conflict-resolution / import actions on interaction & session items from other sources too (added as needed, not in this PR). Carries an **amber count badge** (`bg-amber-600 text-white`) = conflicts + orphans; discovery is deliberately **not** counted here. An empty state ("Nothing needs attention") shows when both are zero. The `session=<anarlog_session_uuid>` param (sent by the daemon's conflict deep-link) is matched against each item's `anarlog_session_id` to scroll to and highlight that card.

Rationale for placing discovery under People rather than the Interactions tab: the Interactions tab stays scoped to interaction-level items that block logging, while everything you might add to the CRM as a person (rich candidates + weak title tokens) lives under the People tab.

### Conflict card

Compact-table treatment (the densest of the three explored; the card and radio treatments in the exploration canvas are dropped). Structure:

- **Session lede:** message icon, session title, `Anarlog session` badge, time, and a quoted summary excerpt. (The mock's "In this session" implied-participant chips and the session duration are deferred — see Fidelity below.)
- **Candidate table** — columns `Candidate | Time | Overlap`, one row per time-overlap candidate, with a per-row `This one`:
  - *Candidate:* kind icon (calendar/phone), candidate title (or peer handle for phone calls), and the candidate's attendee names (matched names emphasized).
  - *Time:* the candidate's time plus drift from the session start, computed client-side (`occurred_at − meeting_at`).
  - *Overlap:* a pip-per-attendee meter (matched attendees filled blue) plus an "N shared" label.
- **Footer:** `None of these — log as impromptu`.

Actions:

- `This one` → `POST /api/v1/meeting-notes/{id}/resolve-link {action:"link", kind, id}` → `linkage_state='linked'`. The row shows a brief green confirmation, then leaves the queue and the badge decrements.
- `None of these — log as impromptu` → `POST .../resolve-link {action:"none_of_these"}` → zero-candidate flow → `linked_impromptu`. **No confirm step** — the action is explicit and low-frequency.

Both endpoints already exist (merged in PRs 3/5).

### Orphan card

Same session lede plus a neutral note ("No calendar event or call matched this time. Usually fixed by tagging participants in Anarlog, then it re-syncs.") and two actions: `Open Anarlog` and `Log as impromptu` (→ the `none_of_these` flow). The queue is intentionally thin — no manual "link to an event…" search, since orphans are normally resolved via the Mac tagging round-trip and the Pi queue is only a fallback.

**`Open Anarlog` launches the app, not the specific session.** Anarlog (fastrepl/anarlog, a Tauri app) registers a custom URL scheme, but its deep-link handler routes only auth/billing/integration callbacks — there is **no note/session-open deep link** (`apps/desktop/src/shared/hooks/useDeeplinkHandler.ts`). So the button fires the bare scheme to launch/focus the app, and the user opens the session (whose title is shown on the card) themselves. This is preferred over opening the session directory in Finder — the on-disk session files are machine-format and not human-readable, so the folder is unhelpful. Implementation: a plain `<a href>` / `window.location` to the scheme; the OS hands the URL to the registered app even though no in-app route consumes it. Scheme value: **`hyprnote://`** (the stable build, `com.hyprnote.stable`); it is build-channel-specific (staging uses `hyprnote-staging://`), but for this single-user deployment hard-coding the stable scheme is fine — just note it in case the channel ever changes.

**In the companion daemon PR (alongside the param rename):** the macOS orphan notification currently opens the session directory in Finder (`ClickTargetURL.swift`, orphan branch returns `sessionDirURL`); switch it to fire `hyprnote://` so it launches Anarlog — matching this card's `Open Anarlog` and the same "on-disk files are useless" rationale. The conflict branch of that function is already being changed for the `tab` rename, so both edits land together.

**Later follow-up (out of scope):** if Anarlog upstream adds a real note-open deep link (e.g. `hyprnote://note/<id>`), revisit targeting the specific session on both surfaces — pending confirmation that Anarlog's note id maps to the daemon's session UUID.

### Discovery (`anarlog_title`)

Weak, name-only candidates lifted from session titles, **grouped by normalized token** and ranked by evidence count. Rendered as low-confidence rows (dashed avatar, "from title · low confidence" chip, expandable session-title evidence) under the People tab. Per row:

- `Create contact…` / `Link contact` — opens the **existing `import-link-modal.tsx`** in a name-only branch keyed on `candidate.source === 'anarlog_title'`: the Contact Methods apparatus (`MethodSelector`/`ConflictResolver`) is hidden and replaced by an info note ("No contact methods — Anarlog only captured a name…") plus the session-title evidence list; the header pager, editable name, Import/Link toggle, `ContactSelector` (link mode), and cadence are kept. The pager iterates the grouped discovery queue.
- `Not a person` — ignores the whole token group.

Accepting (import or link) or ignoring a token resolves **all** sibling `external_contact` rows for that normalized token in one call.

### Backend slice (new in PR 7)

The conflict/orphan path is fully built (PRs 1–6); the discovery path is not. PR 7 adds:

- **Grouped discovery query** — a sqlc query grouping `external_contact` rows where `source='anarlog_title'` and `match_status='unmatched'` by normalized token, returning per group: normalized token, evidence count, member row-ids, and distinct session-title evidence.
- **List endpoint** — `GET /api/v1/imports/anarlog-title` returning the grouped tokens, feeding the discovery section.
- **Token-group resolution** — `POST /api/v1/imports/anarlog-title/resolve {normalized_token, action:"import"|"link"|"ignore", …}`. The server re-resolves every sibling row for the token and applies the action atomically (reusing the existing import/link service for a representative row; siblings are marked imported/matched/ignored). Body-based — the token is not in the URL path — to avoid encoding pitfalls and stale client-supplied id lists.
- **Needs-attention candidate enrichment** — extend the `needs-attention` candidate projection (`NeedsAttentionCandidate`/`…Preview`) to include per-candidate attendees `[{name, matched}]`, where `matched` reflects membership in the session's implied set, so the conflict table can render attendee names and the overlap meter. The session's own implied-participant chips and the duration field are out of scope for v1.

### Frontend (net-new)

- Types for the needs-attention items/candidates and the discovery groups.
- Hooks: `useSessionsNeedingAttention` (GET needs-attention), `useResolveLink` (POST resolve-link), `useAnarlogTitleDiscovery` (GET grouped discovery), `useResolveDiscoveryToken` (token-group import/link/ignore), with invalidation wired into `query-invalidation.ts`.
- Components under `frontend/src/components/imports/session-attention/`: `SubTabs`, `SessionLede`, `OverlapMeter`, `ConflictCard` (+ `CandidateTable`), `OrphanCard`, `DiscoveryRow`, `ResolvedCard`, `AttentionEmptyState`.
- `import-link-modal.tsx` extended with the name-only branch (extended, not forked).
- `SOURCE_FILTERS` + `source-display.ts` gain an `Anarlog` entry for `anarlog_humans`.
- `?tab=interactions` selects the Interactions tab (`?tab=needs-attention` accepted as a transitional alias until the renamed daemon ships — see deep-link note); the `?session=<uuid>` param is consumed (scroll-to + highlight the matching `anarlog_session_id`) and then cleared mount-only via `router.replace()` so a page refresh does not re-trigger the deep-link.
- **Companion Mac-daemon change** (`mac-daemon/Sources/CRMMacOrphanNotifications/ClickTargetURL.swift` + its unit tests; own daemon build/release alongside PR 7) — two edits to the same click-URL builder: (1) rename the conflict deep-link param `needs-attention` → `interactions` (the UI's transitional alias covers the deploy gap, removed in a follow-up once the renamed daemon is deployed); (2) switch the orphan branch from opening the session directory in Finder (`sessionDirURL`) to firing `hyprnote://` to launch Anarlog.

### Fidelity

Conflict-card participant detail ships at "middle" fidelity: per-candidate attendee names + matched flags (the real disambiguation signal) are included; the lede's implied-participant chips and the session duration are deferred to keep the backend surface small. Time-drift is computed client-side from `occurred_at − meeting_at`.

### Walk-in supplemental (Step 5)

No UI. Walk-in interactions are created silently on auto-linked sessions and appear on the contact's timeline like any other interaction.

## Orphan notification UX (Mac-side)

The daemon consumes `needs_attention` items from the ingest response. For each `reason: 'orphan'`:

- Raise a macOS user notification using `UNNotificationRequest` configured as an alert (persistent — stays in Notification Center until the user dismisses).
- Notification body: session title, time, brief instruction ("Tag participants in Anarlog").
- Click action: launch Anarlog via `hyprnote://` (PR 7 companion change). As shipped in PR 6 this opened the session directory in Finder; see the note below.
- The daemon persists pending orphan session UUIDs in a small SQLite table so notification state survives daemon restart. An entry is cleared when a subsequent sync confirms the session has transitioned out of `orphan_needs_review`.

For `reason: 'conflict'` notifications: same persistent-alert mechanism, but the click action opens a deep link to the `/imports` Interactions tab in the user's browser (Pi UI via Tailscale URL configured at pairing time).

> **Shipped in PR 6** (`#339` + follow-ups; `mac-daemon/Sources/CRMMacOrphanNotifications/`). As shipped, the click-action contract (`ClickTargetURL.swift`) is: **orphan** → open the session directory (`metadata.sessionDirURL`) in Finder; **conflict** → open `/imports?tab=needs-attention&session=<anarlog_session_uuid>`.
>
> **PR 7's companion daemon edit changes both** (see Imports UI → Frontend → Companion Mac-daemon change): **orphan** → fire `hyprnote://` to launch Anarlog (on-disk files aren't human-readable); **conflict** → emit `tab=interactions` (the Pi UI accepts `needs-attention` as a transitional alias during the deploy gap). Notification copy stays as the daemon implements it.

## PR staging

Backend-first. UI is intentionally deferred to the last PR(s) so design (done separately via Claude Design) can proceed in parallel.

1. **Migration** — add `linked_kind`, `linked_id`, `linkage_state` columns to `meeting_note`; add CHECK constraints; widen `external_contact.source` to accept `anarlog_title`.
2. **Daemon: Anarlog readers** — humans + sessions, per the parent spec's v2 section. Emits `meeting_note.recorded`, `external_contact.upserted` (source=`anarlog_humans`), and tombstone variants.
3. **Pi ingest: baseline meeting_note handling** — receives `meeting_note.*` events. Implements Step 1 (candidate query), Step 2 single-candidate auto-link, and Step 4 orphan flow *without* title parsing. Step 5 walk-in supplemental. Yields a working linked / orphan dichotomy.
4. **Title parsing utility + discovery flow** — implements extraction, the name-string matcher extension to `backend/internal/matching/`, and the `external_contact source='anarlog_title'` upsert path. Wires discovery for orphan-with-tags and for any other session that runs extraction.
5. **Step 3 participant-signal disambiguation** — extends Step 2 to handle 2+ candidates via implied contact set. Introduces `conflict_pending` state. Backend API exposes endpoints to list and resolve conflicts.
6. **Mac-side orphan notification flow** — daemon consumes `needs_attention` from ingest responses, raises persistent macOS notifications, persists state, clears on resolution.
7. **Imports UI** — see [Imports UI (PR 7)](#imports-ui-pr-7). Frontend: the two-sub-tab `/imports` (**People** — incl. `anarlog_humans` as a new source + the grouped `anarlog_title` discovery section; **Interactions** — the conflict/orphan queue). Backend slice: grouped `anarlog_title` discovery query + list endpoint + token-group resolve endpoint, plus per-candidate attendee enrichment on the needs-attention projection. The conflict/orphan `needs-attention` and `resolve-link` endpoints already landed in PRs 3/5 and are reused as-is. Companion Mac-daemon change (`ClickTargetURL.swift` + tests): (1) rename the conflict deep-link param `needs-attention` → `interactions` (UI keeps a transitional alias, removed in a follow-up); (2) switch the orphan notification from Finder (`sessionDirURL`) to `hyprnote://` (launch Anarlog).

**Status:** PRs 1–6 are merged — the migration, daemon Anarlog readers, ingest/linkage, title-parsing + discovery upsert, Step 3 disambiguation, and the Mac orphan/conflict notification flow (`#339` + follow-ups) all shipped. **PR 7 is the only one remaining** — it carries the entire frontend plus the discovery backend slice above (the grouped `anarlog_title` query, token-group resolve endpoint, and needs-attention attendee enrichment — none of which were built in 2–6).

## Out of scope (for this phase)

- Transcript ingestion (`transcript.json`) — deferred per parent spec; no speaker labels yet.
- Telegram voice/video call as a linkage candidate — currently skipped at the Telegram reader level (`handlers.go:78`). Additive change; not in scope here.
- Calendar attendee deletion when session indicates a no-show — too risky given imperfect tagging.
- Auto-unlinking a session from a calendar event — `linked` → `pending` is a one-way state machine for now; an explicit "unlink" action is deferred.
- Cross-source unified discovery UI (consolidating anarlog_title, gcal_attendee, icloud_contacts, anarlog_humans into one ranked view) — already deferred in parent spec.

## Open questions

- **Stopword list for title extraction** — initial seed is small (`1:1`, `sync`, `catchup`, etc.); expand iteratively from observed false positives in dogfooding.
- **Conflict-resolution endpoint shape** — RESOLVED at PR 5: `POST /api/v1/meeting-notes/{id}/resolve-link {action:"link"|"none_of_these", kind, id}`.
- **Discovery token-group resolution endpoint** — RESOLVED at PR 7: body-based `POST /api/v1/imports/anarlog-title/resolve {normalized_token, action}` resolving all sibling rows for the token; token not in the URL path.
- **`matching.MatchByFullName` placement** — could live in `matching/` (existing fuzzy package) or in a new `matching/name.go` file. Trace existing patterns at PR 4.
- **Notification deep-link URL** — daemon needs the Pi UI base URL. Likely captured at pairing time and stored in daemon config. Confirm at PR 6.
- **Recovery from missed `needs_attention`** — if the daemon crashes or drops an ingest response before raising a notification, the pending state is invisible to the user. Mitigation: a `GET /api/v1/meeting-notes/needs-attention?host_id=...` endpoint the daemon can poll on startup to reconcile its local notification table. Decide at PR 6.

## References

- [`mac-daemon.md`](./mac-daemon.md) — parent spec; this document supersedes only the matching/discovery aspects of its "v2 — Anarlog (humans + sessions)" section.
- [`telegram-method-enrichment.md`](./telegram-method-enrichment.md) — precedent for a sidecar spec refining a phase of a larger integration.
- `backend/internal/matching/` — existing fuzzy matcher; name-string variant to be added in PR 4.
- `backend/internal/google/calendar.go` — calendar attendee filtering (skips declined/tentative/needsAction); session disambiguation reuses these resolved attendees.
- Issue [#74](https://github.com/spengrah/PersonalCRM/issues/74) — Phase 2 tracking issue.
