# Seed realism: archetype-driven interaction histories

Status: DESIGN APPROVED (conceptual), pending implementation plan. 2026-07-24.

## Motivation

The synthetic seed's contact population misrepresented how the application actually produces contact state, and that misrepresentation escalated into real (wasted) engineering work. Staging's seed carried a majority of contacts in a "clock-start" shape (bare `last_contacted ≈ created_at` with no directional timestamps and no backing evidence) — a state the production write path essentially never produces. An issue (#737) and a prod-backfill investigation were built on that phantom majority before prod measurement showed the real population is dominated by *mechanistically produced* states: contacts whose timestamp columns are the correct signature of real mutual calendar meetings and directional message history, backed by evidence rows (`calendar_event`, `comms_message`, `phone_call`, interactions).

PR #738 already fixed the mechanism-level defect: the toolkit can no longer seed `last_contacted` directly (`ContactSpec` has no such field; compile-time guarantee pinned by test). What remains is the population-level gap this spec addresses: after a post-#738 reseed, the catalog bulk (~150 contacts in `prod-shaped`) has *no* interaction history at all, while prod's most common contacted state (mutual meeting-backed) barely exists on staging — only a handful of per-source showcase contacts get replayed source history today.

## Guiding principles (settled in design discussion)

1. **Seed the cause, not the endpoint** (existing toolkit contract, now applied to the whole population): contact interaction state is never written directly; it *emerges* from fake sync-source payloads replayed through the real ingestion pipeline (provider normalize → match → dedup → event bus → River consumers). Staging deliberately has no live sync sources (privacy); fake payloads never touch external APIs. This makes distribution drift structurally impossible when app semantics change — the seed cannot express states the app cannot produce.
2. **Functional coverage, not statistical fidelity.** The seed's job is qualitative: enough samples of each behavior-relevant archetype, at varied points along the relevant distributions, for application functionality to be visible and testable. No prod-mirroring; no committed prod statistics; no machinery to track prod's shape.
3. **All dates relative.** Archetype definitions are expressed purely in durations and frequencies relative to the seed anchor (`g.at(offset)` convention) — "meets every ~30d, most recent 8d ago", never calendar positions. Every reseed re-anchors the population to "now", so dashboards always show currently-relevant state and nothing breaks as the anchor moves. (Inherently calendar-positional data — birthdays — keeps its existing guarded handling; relative offsets cannot fully protect year-wrap cases.)
4. **Generator gives texture, fixtures give guarantees.** Any state a tour, trap, or E2E test *depends on* stays pinned to stable, named, hand-authored fixtures. The archetype generator provides population texture, not test contracts.

## Design

### Archetype catalog

Each archetype = a source mix + frequency/direction/recency parameters, all relative, with per-contact jitter (seeded PRNG) so samples are not clones. Initial catalog (~7):

| Archetype | History shape | What it exercises |
|---|---|---|
| `mutual-regular` | Recurring attended calendar meetings + occasional two-way email/chat, recent | Prod's dominant contacted state; the mutual/all-four-equal timestamp path |
| `mutual-drifting` | Was regular; last meeting ~10–16 weeks ago | Staleness/overdue with real history behind it |
| `outbound-heavy` | User reaches out (email/telegram), rarely or never answered | `last_outreach_at` ≠ `last_contacted` semantics (the #737/#738 split) |
| `inbound-only` | They write; user hasn't responded | Response-pending surfaces |
| `dormant` | Genuine mixed history ending ~4–8 months ago | Deep-overdue, reconnect suggestions |
| `burst-then-quiet` | Interactions clustered in a short window | Messaging aggregation windows |
| `never-contacted` | No source history | Large slice; a real and behavior-relevant state |

Counts per archetype: qualitative — enough samples with varied jitter to populate the relevant UI surfaces and sort/filter ranges; exact numbers chosen at implementation time per profile.

### Generation mechanics

- Per-contact timeline generated from archetype parameters via the toolkit's seeded PRNG. **New PRNG draws append after all existing draws** (the established discipline: mid-sequence draws shift the shared sequence and break id/handle stability of earlier replays).
- Timelines are emitted as source payloads through the existing replay adapters and settled via the existing two-gate settle (domain-terminal predicate + River-jobs-finalized).
- Direction/mutuality is expressed in source terms (who sent which message, calendar attendance), never as interaction-row fields — the interaction model classifies.

### Layering onto existing profiles

- The archetype population replaces the current bare catalog bulk as the *sole* source of interaction state: catalog contacts keep their identity fixtures (names, birthdays, cadences, how-met, lives-in) but interaction state only ever comes from replayed history. Each catalog contact is assigned an archetype.
- Hand-authored fixture cohorts stay unchanged and explicitly labeled: 1900-sentinel birthdays, unicode/descender names, no-method slot, pending/unmatched candidates, follow-up loop, soft-deleted, merged pairs, import candidates, and the per-source showcase contacts.
- **Both `prod-shaped` and `dev` profiles get archetypes** — dev with smaller counts. `minimal-scoped` (golden-pinned) is untouched.

### Constraints

- Determinism: population is a pure function of (seed, namespace, anchor), consistent with existing factory rules.
- Reseed duration: no hard ceiling (reseeds are rare and non-blocking), but the reseed should log its duration and payload volumes so growth is observable.
- Todoist is out of scope (feeds tasks, not interactions). The three documented direct-write exceptions (relationship signals, follow-up loop, import candidates) remain as-is.
- Namespace isolation, cleanup, and `SeedAllowed` env-gating are unchanged.

## Non-goals

- Mirroring prod's exact distribution, or committing measured prod statistics.
- Wiring any live sync source into staging (permanently out for privacy).
- Migrating/repairing existing staging rows: a full reseed on a post-#738 image supersedes them. (Operational note: staging's current clock-start rows persist until a reseed actually runs — the auto-reseed skips its wipe when a sync account is connected (`--require-oauth-empty`), so a one-time forced `make staging-reset` may be needed.)
- Changing the E2E `/test/seed/*` HTTP fixture path (its `last_contacted = now` default is a separate deferred #738 item).

## Open questions for the implementation plan

1. **Replay-adapter inventory vs archetype needs:** which sources have replay adapters today, and is calendar (critical for `mutual-regular`/`mutual-drifting` attended meetings) among them? Any archetype requiring a missing adapter either gets the adapter built or its source mix adjusted.
2. **Existing catalog cohort migration:** the current overdue/recent/never-contacted index cohorts predate #738 — determine what (if anything) still makes them overdue post-#738, and how they map onto archetypes without breaking tours that select contacts by stable index.
3. **Tour/trap dependency audit:** enumerate which staging states tours and traps actually depend on, so those get pinned fixtures before the population underneath them changes.
4. **Reseed runtime measurement:** measure the settled reseed wall-clock at implementation scale; no ceiling, but the number informs payload sizing.
