# Contact interactions surface

A per-contact read surface for interactions and their underlying content, on the contact detail page. This is the arc that precedes the LLM extraction program's first extractor (#379): the evidence surface its "visible why" points into, and the audit substrate for checking inference results against ground truth. Design sketches (settled with the user): https://claude.ai/code/artifact/2585468c-1ca0-44cc-a078-0be022013f6b

## Context & problem

The CRM records every interaction and shows none of them. There is no interactions read surface anywhere in the frontend: the interactions API module and hook expose only creation, the contact page's "Recent activity" block renders derived timestamp columns, and the paginated `GET /api/v1/contacts/:id/interactions` endpoint has no frontend caller. Raw content is stored (message bodies, meeting summaries) but unreachable from the UI. The WhatsApp integration spec's success criteria already assume a contact timeline that does not exist.

Why now: the LLM extraction program (#379, `.ai/spec/llm-extraction-program.md`) is the next product arc, and the user ruled that its first extractor ships with a visible "why" affordance from day one. That affordance needs an evidence surface — you cannot audit, trust, or correct inference over content you cannot see. This surface is also where prod's duplicate-interaction problem (#372) becomes visible, and it is the eventual home for labeling/correcting extraction results.

## What matters

In priority order, as confirmed by the user:

1. **Audit fidelity — the surface shows what the system believes, completely.** Every interaction, every source: message bursts, meetings, phone calls (metadata-only), manual log entries, Todoist-completion entries, group threads. Sourceless/content-less entries at equal rank. No filtering or weighting that makes the surface lie about recorded state. The primary job is checking system-recorded (and later inference-generated) state against ground truth.
2. **Content drill-down.** Interaction → its constituent messages/content → full plaintext. A group drill-down shows the full thread, all speakers — the evidence must be what extraction would read; a dyad-filtered view falsifies it. The cross-source content-assembly read layer this requires is a deliberate output of the arc, reused by extraction (SP3).
3. **Don't foreclose fix/merge/label/correct or the health visualization.** v1 is read-only, but the design must leave room for the #372 merge action, SP3's labeling/correction affordances, and an eventual contact-health visualization over the same data.
4. **Group interactions appear on every participant's page, visibly marked as group.** Relevance weighting comes later from inference (the substance-vs-noise extractor), never from this surface hiding rows.
5. **Feel: snappy if achievable; reverse-chron paging; no search.** Date filtering is included but secondary.

## Intent & appetite (delegation charter)

**Ranked priorities:** the "What matters" list above, in stated order. Priorities 1–2 (audit fidelity, content drill-down) are the point of the arc and are never cut; priority 3 is a standing design constraint, not cuttable machinery; priority 4 is part of audit fidelity in practice.

**Appetite:** ~4 PRs, about a week of pipeline time. On breach, cut from the bottom of the priority list upward: custom date range (keep presets) → venue dropdown filter (keep per-row venue tags) → date presets entirely → responsiveness polish beyond plain reverse-chron paging. Needing to cut deeper means the arc is mis-scoped: stop and surface to the user.

**Rabbit holes (hard fences — do not enter, even in service of a priority):**

1. Thread continuity across interaction boundaries (`venue_id` enables it later; any continuity work now is waste).
2. Within-burst pagination, unless a pathological prod burst is demonstrated.
3. HTML rendering or sanitization of any content — plaintext only.
4. Re-aggregation, dedup, or coalescing — the surface renders what aggregation produced (ING-026…ING-031 untouched); #372 duplicates render, they don't get fixed here.
5. Any write path — no new writers, no mutation endpoints; the overflow menu stays a reserved stub.
6. Search, snippet previews, relevance weighting.
7. #347 mark-contacted consolidation (same files, adjacent debt, out of scope).
8. Venue-title fallback perfectionism — group title / "DM" / email subject is the whole ruleset.
9. Generalizing the content-assembly layer beyond "interaction (or venue) → content rows".
10. API design stays two reads: the contact-scoped list and a per-interaction content read. No generic feed abstraction; no unified cursor across the upcoming union (upcoming is a bounded side-fetch, not merged paging); no denormalized counts/caches/columns (at most an index); no filter grammar beyond the two filter concepts (venue; date range, however many query fields a range needs).
11. Performance tuning beyond indexed reverse-chron paging — no query-plan spelunking unless the heaviest-contact success criterion actually fails.

**Call-me-when:** only escalations unresolvable by citing this spec/charter (the default rule). No additional triggers.

**User-reserved decision classes:** none — explicitly delegated (2026-08-24):

- UI PRs merge autonomously on green CI + merge gate; staging covers visual verification. Do not hold for user review.
- Visual deviations from the design canvas: coordinator judgment, biased toward shipping slightly-off with a small follow-up over blocking on user availability.
- CAL-024…CAL-028 retirement: retire-and-mint into the new interactions domain per `spec/README.md`'s reversal rule.
- API shape (enrich vs new endpoint): planner's call.

## Goals

- One "Interactions" section on the contact detail page, replacing the Meetings section, rendering every recorded interaction for the contact in reverse chronological order with pagination.
- Per-row: source badge, direction, timestamp, label, content indicator (message count / meeting note / call metadata / "No content"), venue tag(s) where a venue exists, group marker, and a reserved overflow-menu affordance.
- Expand-in-place drill-down showing all of an interaction's constituent content as plaintext: messages with sender and timestamp (full thread for groups), meeting note summary/memo with provenance line.
- Upcoming calendar events in the same list, marked with an "Upcoming" badge, so subsuming Meetings loses nothing.
- Filters: a venue dropdown and date presets (All / 30 days / 90 days / custom range); "All" spans future dates.
- A content-assembly read layer (interaction → its content rows across the per-source tables) built as a reusable seam, not inlined into the handler.

## Non-goals

- **No write actions of any kind**: no merge/fix (#372 stays exploration-only), no labeling/review affordances (attach later once SP3 defines candidates), no edit/delete of manual entries.
- **No search.** Reverse-chron paging plus the two filters is the full navigation surface.
- **No snippet previews in list rows.** Deliberately cut; they may return later as inference-driven summaries.
- **No thread continuity across interaction boundaries** in the drill-down (no "scroll up into the previous burst"). Deferred — venue filtering covers the need, and `interaction.venue_id` already enables continuity later with one indexed query, so this is UX scope, not a data-model gap.
- **No HTML rendering.** Plaintext everywhere (email canonical plaintext body, message text, meeting summary/memo).
- **No changes to the notes area or the "Recent activity" distillation** — both stay as they are.
- **No LLM inference, weighting, or summarization** of any kind in this arc.
- **No contact-health visualization** (future consumer of the same data).

## Relation to existing & planned work

- **#379 LLM extraction program**: this arc precedes the ball-in-court slice. The drill-down is the evidence surface the extractor's "visible why" will point into, and the content-assembly layer is shared substrate. Nothing here depends on SP3 landing.
- **#372 merge-interaction RFC**: this surface makes the documented prod duplicates (overlapping recurring calendar series → the same meeting twice in a contact's history) visible, and the per-row overflow menu is the reserved seam #372's action will occupy. This arc builds the surface, not the primitive. The venue-tag design (derived set, below) is forward-compatible with #372's cross-kind merges producing multi-venue interactions.
- **WhatsApp integration spec** (`.ai/spec/whatsapp-integration.md`): its success criteria reference a contact timeline; this arc is that surface.
- **#347 (consolidate mark-contacted mutations)**: adjacent frontend debt in the same files; not in scope, not blocked.
- **Meetings section**: subsumed and removed. Its calendar entries render in the unified list; its Upcoming view survives as the upcoming-events union plus badge. Its E2E/spec citations must be migrated, not orphaned.
- **Behavior SSOT**: the new UI behaviors land in the relevant `spec/*.yaml` domain(s) with citing tests in the same PRs, per `spec/README.md`. Retiring the Meetings section retires or re-homes its behaviors in the same PR.
- **Aggregation semantics (ING-026…ING-031)**: untouched. This surface renders what aggregation produced; it never re-aggregates, coalesces, or re-derives interactions.

## Hard constraints

- **Read-only end to end.** The surface introduces zero new writers. `InteractionRecorder` remains the sole interaction writer; sole-writer CI enforcement must not need modification.
- **Honest rendering.** Every interaction the API returns for the contact appears; nothing is hidden by default. Content-less rows say so plainly rather than being excluded or visually buried.
- **Full-thread evidence.** Drill-down on a group interaction shows all speakers' messages, unfiltered.
- **Upcoming events are not interactions.** The list is a read-layer union of interactions and future calendar events for the contact on one time axis. No interaction rows are minted for future events, ever.
- **Venue association is derived from content.** The row's venue tag(s) are computed in the read layer from the interaction's constituent content rows' container keys (a set, usually of size one) — not read from `interaction.venue_id`, which stays untouched as the graph's primary-container pointer. Venue filtering matches any interaction with content in that venue; an expanded interaction under a venue filter shows the venue-relevant messages. Manual/Todoist/call/meeting rows carry no venue tag (no shared message container).
- **Soft-delete discipline**: all reads honor `deleted_at IS NULL` per repo rules; time comparisons (upcoming/past) use `accelerated.GetCurrentTime()`.
- **No PII in any repo artifact** produced by this arc (tests, fixtures, docs), per the repo privacy rule.

## Architectural direction

- **Constraint — unified section replaces Meetings.** One list, one time axis, all sources at equal rank; upcoming calendar events joined in via the union with an "Upcoming" badge.
- **Constraint — expand-in-place drill-down.** No modal, no separate page. The content region is visually distinct from row metadata (the sketches use a gray-50 inset) so evidence reads as a different layer. Expanding a burst shows all of its messages; there is no within-burst pagination.
- **Constraint — reserved overflow affordance per row.** Present from v1 (minimal contents) so merge/label/correct actions attach later without relayout.
- **Constraint — content-assembly layer as a reusable seam.** "Given an interaction (or venue), return its content rows" across `comms_message`, `telegram_message`, `messages_message`, `meeting_note` — built once, placed where SP3's extractors can consume it, not inlined into a handler.
- **Leaning — enrich the list read rather than N+1.** The existing `GET /contacts/:id/interactions` response is metadata-only (no venue, no counts, no content indicators); the list needs those per row. Whether that is an enriched version of the existing endpoint or a new one, and how counts/tags are computed without per-row queries, is the planner's call — but the list view must not fan out a content query per row.
- **Leaning — filter state follows existing list-param conventions** (the contact list's canonical parse/build pattern) rather than ad-hoc query strings, if these filters live in the URL at all.

## Success criteria

- Every interaction recorded for a contact is visible on that contact's page, from every source, including manual and Todoist entries; a contact with none shows an honest empty state.
- Expanding a message interaction shows exactly the stored plaintext of all its messages, with sender and timestamp, including all speakers in group threads. Expanding a calendar interaction with a meeting note shows the stored summary and memo.
- A future calendar event for the contact appears in the list with an "Upcoming" badge; removing the Meetings section loses no previously visible information, within two recorded bounds: an ended-but-unsynced event may be absent between its end and the sync that records its interaction (the surface renders recorded state, per priority 1), and the upcoming side-fetch is bounded at the 250 soonest future events (strictly more reachable than today's farthest-first 100).
- Selecting a venue in the filter narrows the list to interactions with content in that venue; date presets and a custom range narrow by time; "All" includes future entries.
- The known prod duplicate-meeting case renders as two visible rows — the surface exposes the duplication rather than masking it.
- The list stays responsive on the heaviest real contacts (years of near-daily messages) via reverse-chron paging.
- The content-assembly read layer is invocable outside the HTTP handler (demonstrated by its tests), so SP3 can consume it without refactoring.

## Desired behavior sketch

The settled visual reference is the design canvas (link at top): list view with header filters (venue dropdown; All / 30 days / 90 days / Custom date pills), rows carrying source badge + direction glyph + label + venue tag + content indicator + overflow menu, an upcoming meeting at top with a blue "Upcoming" badge, a group burst expanded to a full multi-speaker thread on a gray inset, and a calendar entry expanded to its meeting note summary/memo with an on-device-provenance footnote. The canvas annotations record the seam and filter decisions.

## Assumptions & deferred questions

- **Assumed:** bursts are small enough (bounded by aggregation windows) that "expand shows all messages" needs no cap. If a pathological burst exists in prod, the planner may add paging inside the drill-down without violating the no-truncation-by-default intent.
- **Assumed:** venue titles are renderable — group chats have titles; DMs fall back to a "DM" label; email threads can fall back to a subject. Exact fallback rules are planning detail.
- **Deferred to planning:** the exact API shape (enrich vs. new endpoint), how per-row counts/indicators are computed set-based, empty-state copy, and where filter state lives.
- **Deferred beyond this arc:** thread continuity across bursts (enabled by `venue_id`), snippet/summary previews (inference-driven, SP3+), labeling/correction affordances (SP3 defines candidates first), the #372 merge action, the health visualization.

## Spec-item IDs (pre-allocated)

New behavior domain: `interactions`, prefix `IXN` (the `spec/README.md` domain-table row lands with the first PR that creates `spec/interactions.yaml`). IDs IXN-001…IXN-020 are pre-allocated to this arc; planners mint behaviors within this range only. Per `spec/README.md`, yaml rows may land as `status: proposed` ahead of implementation and flip to `current` with citing tests in the implementing PR; a `ui`/`api`-surface `current` then-item must land with its citing test in the same PR.

Initial assignment of this spec's behavioral claims:

- IXN-001 — unified Interactions section replaces Meetings: every recorded interaction, every source, reverse-chron, paginated (Goals; Success criteria 1).
- IXN-002 — per-row metadata: source badge, direction, timestamp, label, content indicator, group marker, reserved overflow affordance (Goals).
- IXN-003 — venue tags derived from constituent content containers, never from `interaction.venue_id` (Hard constraints).
- IXN-004 — expand-in-place drill-down: full plaintext, sender + timestamp, full thread for groups (Goals; Success criteria 2; Hard constraints).
- IXN-005 — calendar drill-down shows meeting-note summary/memo with provenance (Goals; Success criteria 2).
- IXN-006 — upcoming calendar events in-list with "Upcoming" badge; no interaction rows minted for future events (Goals; Hard constraints).
- IXN-007 — venue filter narrows to interactions with content in that venue (Goals; Success criteria 4).
- IXN-008 — date presets + custom range; "All" spans future dates (Goals; Success criteria 4).
- IXN-009 — honest rendering: content-less rows say so plainly; duplicates render as distinct rows; honest empty state (Hard constraints; Success criteria 1, 5).
- IXN-010 — read-only surface: the arc introduces no write endpoints and no new interaction writers (Hard constraints).
- IXN-011 — content-assembly read layer invocable outside the HTTP handler (Goals; Success criteria 7).
- IXN-012 — enriched list read without per-row content fan-out (Architectural direction; Success criteria 6).
- IXN-013…IXN-020 — reserved for planner refinement (splits, negative behaviors).

Retirements: CAL-024…CAL-028 (Meetings section UX) are retired with `notes` pointers to their IXN replacements, in the same PR that removes the Meetings section; their E2E/spec citations migrate in that PR.
