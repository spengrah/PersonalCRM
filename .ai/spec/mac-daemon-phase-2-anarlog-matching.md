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
- **Pi-side conflict surfacing.** Ambiguous time-overlap conflicts that can't be auto-resolved via participant signal are surfaced in the Pi `/imports` page as a new "Sessions needing attention" tab.

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

## Conflict-resolution UX (Pi-side)

A new tab on `/imports` titled "Sessions needing attention" lists all `meeting_note` rows with `linkage_state IN ('conflict_pending', 'orphan_needs_review')`. (The latter is rare in practice — most orphans get resolved via the Mac notification round-trip — but the queue exists as a fallback.)

Each row shows: session title, time, summary excerpt, and (for conflicts) the candidate events/calls with their participant-overlap counts and a one-click "this one" / "none of these" action. Picking "this one" sets `(linked_kind, linked_id)` and transitions to `linked`. "None of these" promotes the session to the zero-candidate flow (re-runs Step 4 / Step 5 logic).

> **Visual design TBD.** Layout, copy, badge style, empty-state, conflict-card structure, and integration into the existing Imports page are all open. UI design will be done separately via Claude Design and integrated in a final UI PR (see [PR staging](#pr-staging) below).

## Orphan notification UX (Mac-side)

The daemon consumes `needs_attention` items from the ingest response. For each `reason: 'orphan'`:

- Raise a macOS user notification using `UNNotificationRequest` configured as an alert (persistent — stays in Notification Center until the user dismisses).
- Notification body: session title, time, brief instruction ("Tag participants in Anarlog").
- Click action: open the session's directory or `_meta.json` in the user's default file handler (Anarlog or Finder).
- The daemon persists pending orphan session UUIDs in a small SQLite table so notification state survives daemon restart. An entry is cleared when a subsequent sync confirms the session has transitioned out of `orphan_needs_review`.

For `reason: 'conflict'` notifications: same persistent-alert mechanism, but the click action opens a deep link to the `/imports` "Sessions needing attention" tab in the user's browser (Pi UI via Tailscale URL configured at pairing time).

> **Visual design TBD.** Notification copy, click affordances, and the Notification Center grouping behavior will be finalized alongside the Pi UI work via Claude Design.

## PR staging

Backend-first. UI is intentionally deferred to the last PR(s) so design (done separately via Claude Design) can proceed in parallel.

1. **Migration** — add `linked_kind`, `linked_id`, `linkage_state` columns to `meeting_note`; add CHECK constraints; widen `external_contact.source` to accept `anarlog_title`.
2. **Daemon: Anarlog readers** — humans + sessions, per the parent spec's v2 section. Emits `meeting_note.recorded`, `external_contact.upserted` (source=`anarlog_humans`), and tombstone variants.
3. **Pi ingest: baseline meeting_note handling** — receives `meeting_note.*` events. Implements Step 1 (candidate query), Step 2 single-candidate auto-link, and Step 4 orphan flow *without* title parsing. Step 5 walk-in supplemental. Yields a working linked / orphan dichotomy.
4. **Title parsing utility + discovery flow** — implements extraction, the name-string matcher extension to `backend/internal/matching/`, and the `external_contact source='anarlog_title'` upsert path. Wires discovery for orphan-with-tags and for any other session that runs extraction.
5. **Step 3 participant-signal disambiguation** — extends Step 2 to handle 2+ candidates via implied contact set. Introduces `conflict_pending` state. Backend API exposes endpoints to list and resolve conflicts.
6. **Mac-side orphan notification flow** — daemon consumes `needs_attention` from ingest responses, raises persistent macOS notifications, persists state, clears on resolution.
7. **UI** — Pi `/imports` "Sessions needing attention" tab + Anarlog-title weak candidates section. Visual design TBD via Claude Design; this PR integrates the design output.

PRs 2–6 land in sequence on the backend. PR 7 is the only one that blocks on UI design work, which can proceed in parallel from PR 1 onward.

## Out of scope (for this phase)

- Transcript ingestion (`transcript.json`) — deferred per parent spec; no speaker labels yet.
- Telegram voice/video call as a linkage candidate — currently skipped at the Telegram reader level (`handlers.go:78`). Additive change; not in scope here.
- Calendar attendee deletion when session indicates a no-show — too risky given imperfect tagging.
- Auto-unlinking a session from a calendar event — `linked` → `pending` is a one-way state machine for now; an explicit "unlink" action is deferred.
- Cross-source unified discovery UI (consolidating anarlog_title, gcal_attendee, icloud_contacts, anarlog_humans into one ranked view) — already deferred in parent spec.

## Open questions

- **Stopword list for title extraction** — initial seed is small (`1:1`, `sync`, `catchup`, etc.); expand iteratively from observed false positives in dogfooding.
- **Conflict-resolution endpoint shape** — `POST /api/v1/meeting-notes/{id}/resolve-link {kind, id}` vs a more generic action API. Decide at PR 5.
- **`matching.MatchByFullName` placement** — could live in `matching/` (existing fuzzy package) or in a new `matching/name.go` file. Trace existing patterns at PR 4.
- **Notification deep-link URL** — daemon needs the Pi UI base URL. Likely captured at pairing time and stored in daemon config. Confirm at PR 6.
- **Recovery from missed `needs_attention`** — if the daemon crashes or drops an ingest response before raising a notification, the pending state is invisible to the user. Mitigation: a `GET /api/v1/meeting-notes/needs-attention?host_id=...` endpoint the daemon can poll on startup to reconcile its local notification table. Decide at PR 6.

## References

- [`mac-daemon.md`](./mac-daemon.md) — parent spec; this document supersedes only the matching/discovery aspects of its "v2 — Anarlog (humans + sessions)" section.
- [`telegram-method-enrichment.md`](./telegram-method-enrichment.md) — precedent for a sidecar spec refining a phase of a larger integration.
- `backend/internal/matching/` — existing fuzzy matcher; name-string variant to be added in PR 4.
- `backend/internal/google/calendar.go` — calendar attendee filtering (skips declined/tentative/needsAction); session disambiguation reuses these resolved attendees.
- Issue [#74](https://github.com/spengrah/PersonalCRM/issues/74) — Phase 2 tracking issue.
