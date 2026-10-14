# Interaction review readiness

Make the contact interactions surface fit to be the labeling bench for LLM extraction (#379), starting with ball-in-court: evidence the user can trust, corrections that stick and propagate, and a data model in which extracted claims, the user's verdicts and the user's corrections live in one store. This arc precedes the first extractor; it builds no extractor, claim UI, verdict path or review queue, but must leave each of them a clean place to land.

## Terminology

- **Interaction**: the thing that happened, once, however many people were in it: a meeting, a call, an Anarlog session, an email exchange, a message burst.
- **Participation**: one contact's part in an interaction. It carries what is specific to that contact: direction (inbound, outbound, mutual), time, and whether they took part at all.
- **Observation**: a participation as a source sync recorded it, before any user correction.
- **Effective participation**: an observation with the user's corrections applied. It is what the timeline, cadence and follow-ups read.
- **Correction**: a user act that changes effective participation: a merge (these observations are one interaction) or a removal (this contact did not take part, or this interaction did not happen).

Today's `interaction` table stores observations under the name "interaction": one row per contact, with rows for the same real event linked only by a shared `source_ref` string, and nothing represents the interaction itself. This spec uses the terms above by meaning. Whether and when any table is renamed is planning detail.

## Context & problem

The per-contact interactions timeline shipped (IXN-001…IXN-014; `.agents/spec/contact-interactions-surface.md`) as a deliberately read-only surface. The extraction program's review loop needs it to become a labeling bench: the user reads an extracted claim, compares it against the evidence and against the baseline state the claim would change, and records a verdict that becomes eval material (`.agents/spec/2026-10-12-decision-models-session-notes.md`). Label quality bounds extraction quality, so the bench has to be right before the first extractor runs.

What blocks that today:

- **Evidence defects.** About 65% of iMessage rows have no body, because the Mac daemon never reads the field where macOS stores most bodies (#842). Group-chat messages from senders who are not contacts are never ingested, and outbound group messages are attributed to one arbitrary chat member (#843). Connected outbound calls render as "Missed". Missed inbound calls with no voicemail leave no trace on the timeline (#329).
- **No corrections.** The UI offers no delete or merge. A soft-delete endpoint exists with no caller; it does not fully recompute the contact's cadence fields, publishes no event, and leaves Todoist follow-ups standing. Duplicate interactions from overlapping calendar series render twice (#372). No-shows and meetings cancelled outside the calendar stay recorded as contact.
- **Corrections would not stick.** Nothing records that a row was deliberately removed or merged. Some sources happen not to re-create a deleted row; email (a new message in the same thread on the same day) and Anarlog (any session re-link) do.
- **No home for claims on the surface.** KNW-033 keeps assertions and provenance invisible over HTTP.

## What matters

Confirmed by the user, in priority order:

1. **Evidence fidelity, because label quality bounds extraction quality.** iMessage bodies (#842) and group-chat completeness and attribution (#843), going forward and for stored rows; connected outbound calls stop rendering as "Missed"; missed no-voicemail calls (#329) become visible evidence.
2. **Corrections on interactions.** User-scoped removals (a no-show removed for one attendee without touching those who came; off-calendar cancellations) and merge of duplicates (#372). Both are durable across sync, both propagate for real into cadence state and follow-up tasks rather than cosmetically, and each is a judgment a later eval or merge model can learn from.
3. **Compatibility with the next arcs, without building them.** Claim review (claim beside evidence beside baseline, the baseline being the deterministic derivation or the previously accepted state), the verdict path (a record on the Pi plus a content-free Langfuse score), and a cross-contact review queue that reuses the per-contact list's design as much as possible.
4. **Multi-modal interactions.** One interaction can hold content from several sources (calendar, meeting note, call, messages), each item keeping its own channel attribution.

## Intent & appetite (delegation charter)

**What it is for:** the user's own labeling bench for extraction output, used in dedicated review sessions (through the later cross-contact queue) and opportunistically on a contact's page. One user, attended use, on real prod data. A wrong correction costs cadence correctness and label quality; a missing correction costs the ability to clean the evidence that labels are judged against; a correction that silently undoes itself on the next sync costs trust in the whole bench.

**Appetite:** not the minimum, because shortcuts here degrade labeling and therefore extraction. More PRs than the previous interactions arc (~4) are acceptable; the user judges size when the first arc plan comes back. Within that, cut from the bottom of the priorities upward (multi-modal rendering first, then compatibility seams, then corrections), never evidence fidelity: the iMessage, group-chat and call-display fixes are the point of the arc.

**What durability means here:** a correction is applied at the one place where an observation becomes effective participation, and every sync path passes through that place. That rule is the whole durability mechanism. A demand for source-specific tombstones, a correction ledger beside SP1, or per-source workaround code is disproportionate; a demand that some path bypasses the shared place is not.

**Rabbit holes (do not enter, even in service of a priority):**

1. Unmerge, and any general undo history. A wrong merge is fixable later; it is low importance.
2. The cross-contact review queue, claim UI, verdict writes and eval snapshots: designed for, not built.
3. Polishing review flows before claims exist.
4. Chasing every sync path's re-creation edge case beyond the rule in Architectural direction; the rule is the fix, not a catalogue of special cases.
5. Building the re-scans into the product. They are one-time scripts the user triggers.
6. Chat-burst identity: what one group or one-to-one chat exchange is, and who participates in it. A group burst does not credit every chat member (a lurker who never spoke did not take part), so the right model is genuinely complicated. Deferred to the later full graph migration.
7. Moving participation itself into the graph, or deriving the per-contact rows from graph participation (the full migration).

**Call-me-when:**

- Making interactions multi-modal turns out to need a migration that rewrites existing rows beyond adding identity and backfilling it.
- Making corrections durable turns out to need changes to how a source syncs, beyond applying corrections where an observation becomes effective.
- Any correction would change cadence state in a way the rulings below do not cover.
- An evidence re-scan would change existing burst boundaries or memberships rather than enrich stored rows.

**User-reserved decisions:** the verdict record's shape, what reaches Langfuse, and how corrections move cadence state. The user ruled on the last for merges, removals, skips, date overrides and follow-ups (Architectural direction); any cadence effect outside those rulings goes to the user.

**Tradeoff rules for a delegate:**

- When a cheaper path would make the later full graph migration harder (a correction or judgment stored where a later migration would have to re-home it), take the path that does not, or bring it to the user.
- When evidence fidelity and correction scope compete for the same budget, evidence fidelity wins.
- When the surface could show more polish or show recorded state more honestly, honesty wins.

## Goals

- Evidence fixes for #842 and #843, each going forward plus a one-time re-scan of stored rows; outbound-call connection status rendered correctly; missed no-voicemail calls shown as visible, cadence-inert evidence.
- Interactions as graph nodes, added structurally, for every correctable interaction kind (all kinds except message bursts).
- Merge: the user selects two or more interactions on a contact's timeline and declares them one interaction.
- Removal: the user removes participations, choosing in the action which attendees it covers (defaulting to the current contact, with "everyone" available). Each removal records the user's reason: duplicate, didn't attend (a no-show), or didn't happen (cancelled). Scope and reason are independent: the attendee selection alone decides what is removed, and the reason only explains it.
- Corrections durable across every sync path and fully propagated into cadence state and follow-up tasks.
- Merged interactions render as multi-modal: every member's source, every content item attributed to its own channel.
- Row and drill-down components that render from an interaction plus an explicit participation context, outside the contact page, so the cross-contact queue can reuse them.
- A deliberate lift of KNW-033 for reading: the drill-down shows, read-only, the existing assertions whose provenance cites the content it displays. No accept or reject controls, no baseline comparison, no verdict, no graph browsing: those belong to the claim-review arc.

## Non-goals

- Any extractor, claim-review UI, verdict path, eval snapshot or Langfuse write (eval snapshots are deferred to the eval arc).
- The cross-contact review queue itself.
- Unmerge and undo history.
- Corrections on message bursts (chat bursts get no interaction node in this arc).
- The Google Chat DM "Group" pill and the calendar row's end-time timestamp (the user is not bothered by either).
- Renaming the `interaction` table, unless planning shows it is cheap; it is not a goal.

## Relation to existing & planned work

- **Interactions surface arc** (IXN-001…IXN-014): this arc builds on its list, drill-down, content-assembly read layer and reserved overflow seam. IXN-010 (read-only end to end) is reversed and follows retire-and-mint. IXN-009 still holds: unmerged duplicates render as distinct rows.
- **SP1 relationship data model** (KNW-001…KNW-035): corrections, claims and verdicts use its assertion store (review status, supersession, provenance to content spans) and its node merge (loser aliased to winner, assertions re-pointed). Interaction nodes join its structural node tier beside venues and entities. KNW-033 is lifted deliberately.
- **Fixes:** #842, #843, the outbound-call "Missed" label, and #329's visibility half. Implements #372's manual merge; #372's automated merging stays out (a later decision model may propose merges against the same store).
- **Enables:** the ball-in-court eval spec (decision-models session notes). Its open "Shape A" (judgments as SP1 assertions) is consistent with the rulings here, and its listed gap ("a thread needs a node kind") is closed because a thread is a venue node.
- **Must not break:** the awaiting-reply derivation and cadence skip behaviors (CAD-040…CAD-046), the ingest behaviors (ING-018…ING-039), calendar decline cleanup (CAL-008, CAL-016), and contact merge (CON-029).

## Hard constraints

- **One store for judgments.** Corrections, extracted claims and verdicts are SP1 assertions and node merges, never a parallel mechanism.
- **The later full graph migration must not be made harder.** No correction or judgment may live only on a per-contact observation row, or anywhere a later migration would have to re-home it. Observations stay derivable inputs.
- **Channel attribution survives merges.** Every content item keeps its own source and container attribution in every read, including inside a merged interaction. No read resolves a content item's container through a merge alias.
- **Merges never fabricate contact the user did not declare.** A merge combines members exactly as the user's merge ruling states; nothing else invents a direction or a time.
- **Raw evidence is never destroyed by a correction.** Content rows and observations survive merges and removals; the originally judged members and scope stay recoverable.
- **No real content at rest off the Pi** (extraction program decision): the CRM and its tooling, re-scan scripts included, keep no copy of real content anywhere else. The source databases on the user's own devices (the Mac's message database, Anarlog) are the sources being read, not copies, and are outside this constraint.
- **Missed no-voicemail calls never become cadence-bearing participations.**

## Architectural direction

- **Constraint: observations, corrections, effective participation.** Sync writes observations; the user writes corrections as SP1 assertions and node merges on interaction nodes; the timeline, cadence and follow-ups read effective participation derived from both. One writer per fact. The derived effective participation is the cache the later full migration needs.
- **Constraint: interaction nodes are structural, not assertions.** They join the node registry as a new type with its own subtype table, the way venues did, written by the record path without the assertion machinery. Every correctable observation resolves to exactly one interaction node, including observations with no venue (manual entries need a persisted identity of their own, since they have no source key). Message bursts get none. Overlapping calendar series stay two interactions until the user merges them.
- **Constraint: identity survives re-processing.** Each source's re-processing (an Anarlog re-link, an email thread gaining a message on the same day, a calendar rematch) resolves to the same interaction node it resolved to before. Where a source's natural key is unstable (Anarlog's per-contact, title and walk-in keys; email's per-day bucket), the identity is persisted and the plan names, per source, how re-processing finds it again. If finding it again needs more than the shared resolution step, that is the sync-change call-me-when.
- **Constraint: a merge declares event identity, for everyone** (user ruling). It is an interaction-node merge, global across attendees. Each contact's effective participation in the merged interaction derives from that contact's own observations only, so a merge only affects contacts who had a member observation, and a contact with two members gets one participation.
- **Constraint: a merged interaction is atomic, per contact** (user ruling). For each contact, the participation's direction combines that contact's own members (an inbound and an outbound make it mutual) and it counts once, at the latest of that contact's own members' times, matching how a burst that receives a reply is promoted to mutual at the reply's time. A contact present only in an earlier member never inherits another attendee's later member. If mutual would be wrong, the interactions are not the same interaction and the user does not merge them.
- **Constraint: a removal binds to the observations judged** (user ruling). A removal covers the observations the user selected (by attendee) of the source objects in front of them, and every later re-observation of those same source objects: an attendee a later calendar sync adds to an interaction removed as "didn't happen" for everyone is excluded too. It never covers an observation of a different source object merged in later. Example: Alice removed from meeting X as a no-show, then X merged with call Y that Alice was on: Alice is on the merged interaction through Y. Had Alice been removed from Y as well, she would not be.
- **Constraint: corrections apply wherever an observation becomes effective.** Creates, extensions, promotions to mutual and Anarlog re-links all pass through the corrections before any cadence or follow-up effect. A source still acknowledges ingesting its raw evidence when a correction excludes it.
- **Constraint: cadence and follow-ups follow effective participation, composed with the user's own cadence acts** (user rulings). A correction recomputes the contact's cadence fields from effective participation, not by patching the column that happened to match, with three composition rules: a skip is withdrawn when a correction moves the clock, as CAD-045 already states for interaction deletion and merge; the due date is recomputed only when the correction moves the contact's last-contacted time, and then becomes the later of the recomputed date and any due date the user set directly (a Todoist deadline edit) after the corrected observation, while a cadence-frequency change or a skip sets no such floor and an awaiting-reply window the user ended with a skip stays ended; follow-up creation runs its normal rules against the corrected state, a follow-up a removed outbound created is retired, and a completed Todoist task is never reopened. Local correction and remote Todoist effect are distinct states.
- **Constraint: content keeps its own locator.** Content rows stay linked to their original observation and container; a merged interaction's content is the union across its members, each item rendered with its own channel.
- **Constraint: typed timeline identity.** Every timeline item has a typed identity, and its type decides which actions it offers. A correctable interaction is identified by its interaction node, never by a per-contact row id; a message burst, an upcoming event and a missed no-voicemail call each keep an identity of their own and offer no merge or removal. Components and reads that need direction or attendee-scoped actions take an explicit participation context. The plan states how the existing list and content endpoints' ids change (their only client is this repository's frontend).
- **Constraint: corrections are serialized with sync and assertion producers.** The existing assertion-merge helper documents a race with concurrent assertion producers; a merge or removal must not strand a concurrent write on a merged-away node.
- **Constraint: re-scans are enrichment writes, never replays.** The ordinary ingest upsert deliberately ignores conflicts, so replaying repaired messages through it repairs nothing. The #842 and #843 re-scans fill bodies and attribution on existing rows under their existing identity, keeping their interaction links and processing state, add missing messages, replay no cadence effect, and preserve existing memberships and corrections. A second run changes nothing.
- **Constraint: group-chat completeness is bounded by the existing evidence window.** A message from a non-contact is added to the evidence of an interaction whose window it falls inside; it credits no participation, and no window boundary moves. Messages outside every existing window wait for chat-burst identity (rabbit hole 6).
- **Leaning: evidence fixes are independent first PRs**, since none depends on interaction identity.
- **Leaning: missed no-voicemail calls join the list as a read-time variant**, the way upcoming events do, rather than as interactions.

## Success criteria

- The daemon captures iMessage bodies from every body representation the deployed macOS stores, and on prod data after the re-scan every stored iMessage row still without a body is classified against its source record as body-less by nature (attachment-only, reaction), unavailable at the source, or a capture failure; no recoverable body remains uncaptured. The overall empty-body share is supporting evidence, not the criterion.
- A group thread's drill-down includes, within each interaction's evidence window, messages from participants who are not contacts, each labeled by an honest identifier, and the user's own outbound group messages are not attributed to one arbitrary member.
- A connected outbound call never renders as "Missed"; a missed inbound call with no voicemail appears on the contact's timeline, marked as such, and changes no cadence field.
- The user merges two duplicate calendar interactions: every attendee's timeline shows one interaction carrying both members' sources and content, each item attributed to its own channel, and later calendar syncs do not bring the duplicate back.
- Merging a contact's inbound with their later outbound produces one mutual participation counted at the later time, and the contact's cadence fields match what that single mutual participation implies; another attendee with only the earlier member keeps that member's direction and time.
- The user removes a no-show for one attendee: that attendee's cadence fields equal what they would be had the observation never existed, except where the composition rules (skip withdrawn, later hand-set values win) say otherwise; other attendees are unchanged; follow-up tasks reconcile under the follow-up rules; no later sync of any source (email same-day, Anarlog re-link included) restores it.
- A removal made before a merge still covers exactly the observations it judged after the merge, and a later re-observation of a removed source object stays removed.
- Running either re-scan a second time changes nothing.
- Every correction is readable as SP1 state with user provenance, and the members and scope the user originally judged are recoverable after any later merge.
- The row and drill-down components render outside the contact page from a typed timeline identity plus participation context, and only correctable interactions offer merge or removal.
- The drill-down shows existing assertions whose provenance cites the content it displays, over a read the KNW-033 lift deliberately allows.

## Desired behavior sketch

The row overflow menu (reserved by IXN-002) becomes live on correctable interactions only; bursts, upcoming events and missed calls offer no corrections. **Remove** opens a dialog naming the interaction, listing its attendees with the current contact pre-selected and "everyone" available, and asking for the reason (duplicate, didn't attend, didn't happen). **Merge** starts from selecting two or more correctable rows on a contact's timeline; a confirmation shows the members side by side, says the merge applies to every attendee, and states the result for the current contact (for example, "becomes one mutual interaction at 3:40pm"). A merged interaction's row shows every member source's badge; its drill-down shows each member's content in its own section, attributed to its own channel. Removed participations leave the effective timeline. A missed no-voicemail call appears as a distinct, visibly cadence-inert row. Where assertions cite content in a drill-down, they appear read-only beside the content they cite.

## Assumptions & deferred questions

- **Assumed:** duplicates, no-shows and off-calendar cancellations, the user's stated correction cases, are all non-burst interactions (meetings, calls, sessions, email, manual), so excluding message bursts loses no stated case.
- **Assumed:** the merge and removal volume is small (attended, one user), so serialization cost is irrelevant.
- **Assumed:** the #842 bodies are recoverable from the Mac's message database for stored rows, and the #843 re-scan can find the missing messages there.
- **Deferred to planning:** the interaction-node identity rule per source; whether effective participation is a maintained table or a computed read; the correction predicates' catalog entries; how calendar declines (CAL-008, CAL-016) relate to removals; the KNW-033 replacement's exact read surface; the scope dialog's copy; whether removed participations stay viewable on demand.
- **Deferred beyond this arc:** chat-burst identity and correction (full graph migration); unmerge; automated merge proposals (a later decision model); the cross-contact review queue, claim UI, verdict path and eval snapshots.

## Spec-item IDs (pre-allocated)

Ranges pre-allocated to this arc; planners mint within them only. Per `spec/README.md`, rows may land as `status: proposed` ahead of implementation and flip to `current` with citing tests in the implementing PR.

- **IXN-015…IXN-040** — interactions domain: interaction nodes and effective participation, merge, removal and scope, multi-modal rendering and attribution, missed-call evidence, outbound-call status, reusable components, assertions beside evidence. IXN-010 is retired with a notes pointer to its replacements.
- **ING-040…ING-049** — ingest: corrections applied where observations become effective, durability across sources, group-chat ingest of non-contact senders and outbound attribution on the Pi side.
- **MAC-056…MAC-062** — mac-host: daemon-side iMessage body capture and group-chat capture.
- **CAD-047…CAD-053** — cadence-followup: cadence and follow-up effects of merges and removals.
- **KNW-036…KNW-042** — knowledge: the interaction node type, correction predicates, and the KNW-033 lift (KNW-033 retired and replaced under retire-and-mint).

Initial assignment of this spec's behavioral claims:

- IXN-015 — every correctable interaction resolves to one interaction node; message bursts do not (Architectural direction).
- IXN-016 — a merge declares one interaction for every attendee; effective participation derives from each contact's own members (Goals; Architectural direction).
- IXN-017 — a merged interaction is atomic: combined direction, latest member's time (Architectural direction; Success criteria 5).
- IXN-018 — removal is scoped by the user's attendee selection, defaults to the current contact, records an independent reason, and binds to the observations judged (Goals; Architectural direction; Success criteria 7).
- IXN-019 — merged interactions render every member's source and content with per-item channel attribution (Hard constraints; Success criteria 4).
- IXN-020 — missed no-voicemail calls appear as cadence-inert evidence (Goals; Success criteria 3).
- IXN-021 — a connected outbound call never renders as missed (Goals; Success criteria 3).
- IXN-022 — every timeline item has a typed identity that decides its actions; row and drill-down render from that identity plus participation context (Architectural direction; Success criteria 10).
- IXN-023 — the drill-down shows, read-only, assertions whose provenance cites its content (Goals; Success criteria 11).
- ING-040 — corrections apply on create, extension, promotion and re-link; no sync restores a corrected observation (Architectural direction; Success criteria 6).
- ING-041 — group-chat messages from non-contacts are ingested; outbound group messages carry no arbitrary member attribution (Goals; Success criteria 2).
- MAC-056 — the daemon captures iMessage bodies from every body representation the deployed macOS stores (Goals; Success criteria 1).
- CAD-047 — a correction recomputes cadence fields from effective participation under the composition rules (Architectural direction; Success criteria 6).
- CAD-048 — follow-up tasks reconcile to corrected state; completed tasks are never reopened (Architectural direction; Success criteria 6).
- MAC-057 — the #842 re-scan is an idempotent enrichment write that replays no cadence effect (Architectural direction; Success criteria 1, 8).
- ING-042 — the #843 re-scan adds missing group messages inside existing evidence windows as an idempotent enrichment write that credits no participation and moves no window (Architectural direction; Success criteria 2, 8).
- KNW-036 — interaction nodes are structural nodes in the registry (Architectural direction).
- KNW-037 — corrections are user assertions and node merges with recoverable judged members and scope (Hard constraints; Success criteria 9).
- KNW-038 — the KNW-033 replacement: what assertion and provenance reads HTTP exposes (Goals).
- Remaining IDs in each range are reserved for planner refinement (splits, negative behaviors).

## Engines

```json engines
{ "authorFamily": "claude", "arcPlanner": "claude-opus-5-5:high", "arcReview": "gpt-6.1-sol:high" }
```

Quota is in surplus on both subscriptions, so planners, contract reviews, approval gates and single-implementer builds use no cheaper engine (such as luna or Sonnet) unless it is genuinely the best fit, and prefer Opus 5.5 over Fable. TDD pairs are the exception: their implementer, reviewer and fixer use cheaper engines (user direction, 2026-10-13), because the reviewed contract, the per-element reviewer and the approval gate check their work.
