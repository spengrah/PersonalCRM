# Gmail Participant Discovery — Propose New Contacts from Trusted Senders' Mail

**Status:** Draft v1 — user-confirmed exploration, ready for planning (spec-pinned TDD workflow)
**Date:** 2026-08-11
**GH:** #789 (primary), folds in #803; #390 closed as stale during grounding; #391 narrowed (see Relations)
**Behavior items:** IMP-042 … IMP-049 in `spec/imports-matching.yaml` (all `status: current`)

## Context & problem

Email is the only major sync source that cannot propose a **new** contact. Gmail's single producer, `gmail_correspondence`, is deliberately link-only: it surfaces an observed address only when its display name trigram-matches an existing contact (sim ≥ 0.60, ≥2-token name). On a real group thread started by an existing contact, three participants who were genuinely new people were silently dropped — nothing in the product ever showed them. The pipeline works as specified; the gap is structural: no mechanism exists to propose a brand-new contact from an email participant.

Loosening the existing gate is not an option: every fetched message already touches a known contact by construction of the fetch query, so "co-occurs with a known contact" is nearly free and would admit newsletters, vendors, recruiters, and cold outreach — burying the imports tab.

The idea (user's conviction): **filter on who is already trusted, not on properties of the unknown address.** A participant qualifies only when the message's sender is already trusted — an existing CRM contact, or the user themself.

## What matters (priority order)

1. **Discovery must never break sync.** Errors stay best-effort and logged; they never rewind the cursor, fail a sweep, or fail the rematch backfill.
2. **No real person silently missed** within the ruled gates. This is why the recipient cap applies to the create path only.
3. **No firehose.** The acceptance bar from production measurement: on the order of 40 cards over the whole backfill window, zero automated-looking senders.
4. **One card per address.** Duplicate cards for the same address are a low-medium defect.
5. **Misclassification (link vs create) is cheap.** The user picks the action in the UI; no migration machinery is warranted.

## Severity grading context (for every reviewer)

Calibrate P-levels to this feature's actual cost structure, not generic severity:

- **Worst:** discovery breaking the sync path — a cursor rewind, a failed sweep, or blocked message ingest. Silent and compounding.
- **Expensive:** a qualifying real person never surfaced (silent over-suppression). The user cannot know what they never saw.
- **Bad (the acceptance bar):** noise/firehose — visible and recoverable via ignore, but defeats the feature's purpose.
- **Low-medium:** a duplicate card for one address.
- **Cheap:** link-vs-create misclassification; evidence/observability gaps. Not worth complexity to prevent.

## Goals

- A second Gmail candidate source, `gmail_participant`, that proposes **create** candidates from To ∪ Cc participants of trust-anchored messages, surfaced through the existing imports queue with import / link / ignore.
- Nameless (address-only) candidates are first-class: they surface, and can be imported after the user types a name.
- Candidate evidence (trusted sender, anchor message subject, message count) is stored and rendered — and the queue's evidence line is generalized across all discovery sources (#803).
- The historical backlog is harvested via the existing one-time cursor-reset runbook step; steady state is automatic thereafter.

## Non-Goals

- No auto-import — every proposal remains a manual review.
- No change to bystander interaction attribution (separate issue territory).
- No Bcc recovery (structurally impossible from Gmail's stored copies).
- No re-classification migration: first classification is sticky until the user resolves the card.
- No extension of the own-domain rule beyond discovery (message storage / inbound-outbound classification unchanged).
- Not the full #391 form-state-hygiene refactor — only the narrow guarantee this feature needs (see Relations).
- No new Gmail fetching and no History API work (#399 is separate).
- No backfill beyond what the cursor-reset replay covers.

## Ruled behavior (user-confirmed)

| Rule | Decision |
| --- | --- |
| Trust anchor | `From` is an existing contact, **or** one of the user's own accounts, **or** an address at a configured **own domain** (see below) |
| Own-domain rule | Per-account configuration listing domains that count as "me" (the user runs several aliases on a personal domain). Applies to **discovery only**: anchors trust, and excludes those addresses from the candidate pool. The actual domain value is deployment configuration — never hardcoded in code, spec, or tests (repo PII rule) |
| Participant pool | To ∪ Cc; Bcc excluded |
| Recipient cap | To ∪ Cc > 20 → no **create** candidates from that message; link discovery for the same message is unaffected (it stays uncapped, gated by its trigram match as today) |
| Display name | Not required; address-only participants qualify |
| Repetition | First sighting proposes; no minimum message count |
| Exclusions | Known addresses (any contact method), own addresses (incl. own-domain), sticky-ignored addresses |
| Precedence | Link gate evaluated first: strong name match (≥ 0.60) → `gmail_correspondence` link candidate as today; otherwise trust anchor → `gmail_participant` create candidate |
| Mutual exclusion | One card per address: an address with an existing candidate row under either Gmail source is never proposed under the other; first classification sticky |
| Actions | `gmail_participant` is **not** in `linkOnlySources` → import / link / ignore; frontend allowed-actions helper mirrored in lock-step |
| Evidence | Trusted-sender identity, an anchor message subject, and message count ride the existing candidate metadata channel |
| Nameless UX | Card/modal heading falls back to the address (never the literal "Unknown"); editable name field seeds **empty** (nothing to delete); Import disabled until a name is typed; a typed name survives a background refetch |
| Seams | Both discovery seams produce identically: the live sync window loop and the rematch backfill scan |
| Idempotency | Re-syncing the same mail produces no duplicate candidates |

## Relation to existing & planned work

- **`.ai/spec/2026-06-03-gmail-correspondence-discovery-on-fetched-set.md`** — established the in-sync discovery hook, aggregate fold, best-effort error posture, and the cursor-reset runbook. This feature extends that machinery (new gate + flag on the aggregate), it does not add a parallel path.
- **#803 (folded in):** the queue's evidence line is hard-gated to `gmail_correspondence`; we must touch that gate anyway for `gmail_participant`, so it is generalized to render each discovery source's evidence metadata (message count, recency, counterpart) — frontend-only, per the issue. Its motivating failure (indistinguishable rows → mis-link → hand SQL against prod) aligns with this spec's severity ranking.
- **#390 (closed as stale during grounding):** written against a modal that no longer exists; the current modal already tracks candidates by stable id.
- **#391 (narrowed):** the full refetch-hygiene refactor stays out; the one guarantee this feature needs — a typed name for a nameless candidate survives a background refetch — is in scope (IMP-047).
- **IMP-014** (link-only invariant) is unchanged and still true: `gmail_correspondence` stays link-only; `gmail_participant` is deliberately not link-only.
- **IMP-006 / IMP-007** (per-source dedup, sticky ignore) are relied on; the cross-source one-card-per-address rule is *new* and minted as IMP-045.
- **IMP-023** is the chat-side analog (repetition-gated where this is sender-gated); no interaction.
- **#399 (History API)** is orthogonal; this feature must not assume anything beyond the current windowed fetch.

## Hard constraints

- Discovery remains best-effort: errors logged, never returned to the sync/backfill paths.
- No new Gmail API calls; piggyback the existing fetch exactly as the current discovery does.
- No raw SQL in Go; sqlc only. Handler → Service → Repository layering. `accelerated.GetCurrentTime()`.
- The Gmail message **fakes used in tests must present headers the way real Gmail does** — address-only participants, missing display names, absent Subject — so the no-name and no-subject paths are genuinely exercised (workflow rule: fakes must reject/omit what the real system rejects/omits). No external write endpoints exist in this feature, so the verbatim-write-schema rule does not apply.
- Same-PR repo obligations: synthetic factory + replay + profile coverage for the new source; `spec/imports-matching.yaml` proposed items flipped to `current` with citing tests (ui + api surfaces are both settled — E2E cites for `ui`, Go tests for `api`); test-map entries for any new frontend files; `make api-types` / `make api-docs` if DTOs or annotations change.
- Repo PII rule: the user's real domain/addresses never appear in committed code, fixtures, spec text, or tests; the own-domain list is runtime configuration.

## Architectural direction

- **Constraint — extend, don't fork:** the new gate rides the existing fold machinery (`foldDiscovery` / `aggregateParticipants` / the per-address aggregate), adding a per-address "trust-anchored message seen" flag plus sender/subject evidence capture. No parallel discovery pipeline.
- **Constraint — discovery-only own-domain rule:** the own-domain set must not leak into the storage gate's inbound/outbound classification.
- **Constraint — server is the source of truth for allowed actions** (existing `AllowedActionsForSource` pattern), frontend mirrors.
- **Leaning — anchor subject choice:** most-recent trust-anchored message's subject seems right; the planner may pick first-seen if materially simpler. Either is acceptable (evidence gaps are cheap).
- **Leaning — own-domain config mechanism:** per-account configuration (shape left to planning — settings column, env, or config file), so long as it is not hardcoded and works for exactly-one-account setups without ceremony.

## Success criteria

- All eight acceptance criteria on #789 hold (they map onto IMP-042 … IMP-047).
- After deploy plus the one-time `crm-admin --reset-gmail-backfill-cursors` runbook step, the backfill replay surfaces on the order of the measured volume (~tens of cards, not hundreds), with no automated-looking senders — verified against the live instance, per the workflow's live-verification discipline.
- The imports queue renders evidence for chat-source candidates (#803's table), not just Gmail's.
- Re-running sync over the same mail adds nothing.

## Desired behavior

Normative statements live as behavior items IMP-042 … IMP-049 (`spec/imports-matching.yaml`, `status: current`), minted in this session:

- IMP-042 — trusted-sender participants become create candidates (incl. nameless, evidence, exclusions)
- IMP-043 — untrusted senders never produce create candidates (negative)
- IMP-044 — the recipient cap suppresses create proposals only (negative + boundary)
- IMP-045 — link precedence and one-card-per-address mutual exclusion
- IMP-046 — the server declares import / link / ignore for `gmail_participant`
- IMP-047 — nameless candidates are importable after naming (UI)
- IMP-048 — the evidence line renders for every discovery source (UI, #803)
- IMP-049 — own-domain addresses count as the user, in discovery only

## Assumptions & deferred questions

- Volume figures (30 + ~9 addresses over the backfill window) were measured against production before this spec; treated as the calibration for the firehose bar, not re-verified here.
- The own-domain configuration mechanism and the exact anchor-subject choice are left to planning (leanings above).
- Whether the evidence line's generalized rendering needs per-source field mapping beyond message count / recency / counterpart is left to planning within #803's table.
- Single PR vs small arc (e.g. #803 rendering as its own PR) is a planning decision; the spec is indifferent.
