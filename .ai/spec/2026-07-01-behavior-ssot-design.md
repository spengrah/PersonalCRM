# Behavior SSOT — Design (Piece 1 of #380)

**Date:** 2026-07-01
**Status:** Approved design, ready for planning
**Author:** spengrah (brainstormed with Claude)
**Parent:** `.ai/spec/2026-05-31-agentic-ux-qa-and-behavior-ssot-design.md` (umbrella), GH issue #380

## Purpose

This is the detailed design for Piece 1 of the Agentic UX QA program: the single source of truth (SSOT) of intended behavior. The umbrella spec locked the architecture (durable, DOM-free intent specs that deterministic tests, the QA judge, the MCP server, and docs all reference); this spec settles the artifact format, schema, evolution rules, tooling, and the derivation/curation pipeline, and scopes the first implementation cycle.

Naming note: the behavior corpus lives at top-level `spec/` (a first-class product artifact). It is distinct from `.ai/spec/`, which holds design documents like this one.

## Scope of this cycle: exemplar-first

Deliver the schema, `spec/README.md`, the linter + wiring, the maintenance rules, and **three exemplar domains derived and curated to `reviewed`**: `contacts`, `cadence-followup`, `imports-matching`. These three stress the schema across all behavior types and feed Tracks A/B best (contacts: invariants/data; cadence-followup: business logic; imports-matching: the heaviest ux surface). The remaining domains are backfilled in later cycles with a proven template.

## The artifact

One YAML file per domain at `spec/<domain>.yaml`, plus `spec/README.md` as the human index (taxonomy + prefix table, schema reference, ID lifecycle rules, curation rules). Structured YAML because the corpus has three machine consumers — Piece 3 traceability/CI, seed-scenario indexing, the Track B judge — plus humans; YAML is trivially parseable by all of them with no bespoke parser, and the repo has precedent for YAML corpora (`.ai/log/learnings/`).

File-level fields:

```yaml
domain: contacts
prefix: CON
maturity: reviewed   # draft | reviewed | ratified
behaviors: [...]
```

Maturity semantics: `draft` = agent-derived, untrusted, consumers MUST ignore; `reviewed` = human-curated behavior-by-behavior, consumers may act on it; `ratified` = proven through consumption (consumers may hard-gate on it). Nothing flips to `ratified` in this cycle — the flip criteria belong to Pieces 3/4. Maturity is per-file, so the new paradigm switches on domain-by-domain.

## Per-behavior schema

```yaml
- id: CON-001            # <PREFIX>-NNN, stable forever
  title: Soft-deleting a contact cascades to its dependents
  type: business-logic    # business-logic | api | ux | invariant | data
  status: current         # current | proposed | retired
  given: a contact with methods, notes, and a person node
  when: the contact is deleted
  then:
    - the contact is soft-deleted
    - its contact methods are soft-deleted
    - its notes are soft-deleted
    - its person node is soft-deleted, dropping its assertions from graph reads
    - graph assertions themselves are retained
  provenance: [ContactService.DeleteContact, contact_merge_node_integration_test.go]
  notes: optional free text — rationale, known edge cases
```

Field rules:

- **`id`** — `<PREFIX>-NNN` (prefix from the file, number zero-padded to 3 digits, growing naturally past 999). Monotonically assigned per domain. Never reused, never renumbered; gaps left by retirement are fine.
- **`title`** — one short line naming the durable intent.
- **`type`** — what kind of consumer cares:
  - `business-logic` — domain rules observable through outcomes (state machines, computations).
  - `api` — HTTP contract (status codes, validation, response shapes). Track A's handler tests.
  - `ux` — what the user can see/do at a surface, expressed DOM-free (no selectors, no copy). Track B's judge.
  - `invariant` — always-true cross-cutting property (e.g. soft-delete filtering, deterministic ordering).
  - `data` — persistence/derived-data correctness (cascades, dedup, derived cache columns).
- **`status`** — carries the is/ought split. `current` = faithfully describes today's behavior. `proposed` = desired behavior that does not (fully) hold today. If curation reveals today's behavior is a bug, the intended behavior is written as `proposed` (optionally with a filed bug) — a bug is never enshrined as `current` intent. `retired` = tombstone; the row stays intact so the ID is never reused, with a pointer in `notes` if superseded.
- **`given`** — precondition; string or list of strings. Optional: omit when there is no meaningful precondition rather than writing filler.
- **`when`** — the trigger; strictly a single string. A behavior needing two `when`s is two behaviors.
- **`then`** — observable outcome(s); string or list of strings. Each list item is an independently checkable fact (a deterministic test maps items to assertions; the judge verifies item-by-item), and schema evolution becomes one-line diffs. Items get no IDs or keys — coverage and citation stay at behavior granularity. If a list item feels like it deserves its own ID, it is a separate behavior.
- **`statement`** — for `invariant`-type behaviors only, a single string that replaces `given`/`when`/`then` entirely (forcing "all queries filter soft-deleted rows" into GWT produces noise). Mutually exclusive with GWT; other types must use GWT.
- **`provenance`** — set-once list of source references (code symbols, test files, design docs) recording what the behavior was derived from. A historical record: non-load-bearing, allowed to rot, nothing parses it for enforcement.
- **`notes`** — optional free text.

## Evolution rules (no versioning field)

Behaviors are edited **in place**; git history is the version record. There is deliberately no `version:` or `updated:` field — manually-bumped metadata is discipline-dependent (the exact rot vector the umbrella warns about) and duplicates what `git log -p spec/<domain>.yaml` already records with author, date, and PR.

ID lifecycle rule: if an edit **refines or extends the same intent** (a cascade set grows, wording is clarified), edit in place — the ID names the durable intent, and existing test citations stay valid. If an edit **reverses or replaces** the intent (e.g. "deletes are soft" → "deletes are hard"), retire the old ID and mint a new one — editing a reversal in place would silently invalidate every citation pointing at the old meaning. This rule lives in `spec/README.md`.

The change-detection question ("did a behavior change without its covering tests changing?") is answered mechanically from PR diffs, not from metadata — a Piece 3 forward hook: warn when a PR edits behaviors whose citing tests are untouched.

## Traceability direction

All pointers point INTO the SSOT; nothing in the SSOT points at files that move. Tests cite behavior IDs (annotation format defined in Piece 2); Piece 3 computes coverage by scanning tests and flags orphans/dead IDs. There is **no `coverage` field** in spec files — a spec-side mapping would rot on every test rename and force bidirectional checking. The same principle pre-positions the seed-scenario coupling: when D's scenario catalog indexes by behavior ID (Piece 3 alignment), scenarios will cite IDs, not the reverse; the schema needs no change for it.

## Domain taxonomy and prefixes

Fixed in `spec/README.md`. Refreshed from the umbrella's list (which predates Gmail/GChat comms, the knowledge graph, and enrichment). Only the starred three are derived this cycle; the rest are named now so prefixes are reserved and future files slot in without renaming:

| Domain | Prefix | Covers |
|---|---|---|
| contacts ★ | `CON` | CRUD, merge + graph tombstoning, soft-delete cascade, list filter/sort/navigation, birthdays surface |
| cadence-followup ★ | `CAD` | cadence state machine, overdue/contact_by, followup tasks, staleness |
| imports-matching ★ | `IMP` | external contacts, import queue, candidates, matching/rematch, suggestions, needs-review |
| ingest | `ING` | message/call/meeting-note pipelines, comms aggregation, dedup windows |
| calendar | `CAL` | GCal sync, event→interaction, attendee matching |
| telegram | `TGM` | MTProto ingest, enablement |
| todoist | `TDS` | task sync, reconciliation, markers, close-on-outreach |
| knowledge | `KNW` | assertions, nodes/edges, derived cache columns, enrichment suggestions |
| notes-meetings | `NTS` | notes CRUD, meeting-note linkage states |
| mac-host | `MAC` | daemon endpoints, auth, address-book reconcile, anarlog |
| settings | `SET` | settings surfaces, OAuth flows, provider enablement |
| dashboard | `DSH` | dashboard surfaces, search |

A behavior that moves domains gets a new ID; the old one is retired with a pointer in `notes`. Non-derived domains may still be split/merged at their own derivation time — a README table edit plus new prefixes, with retirement rules covering any moves.

## Tooling: the spec linter

Minimal by design — parse + validate only. Coverage/orphan/dead-ID checks are Piece 3 and out of scope here.

- **Implementation:** Go parser package `backend/internal/spec` (typed schema structs) + thin CLI `backend/cmd/spec-lint` (precedent: `crm-admin`) taking the spec directory as an argument. Go because YAML parsing + cross-file validation in shell is misery, and Piece 3's traceability scanner will import this same parser package.
- **Checks:** YAML parses; required fields present; enums valid (`type`, `status`, `maturity`); `id` matches `<PREFIX>-NNN` and the file's declared `prefix`; prefixes unique across files; IDs unique within file and globally; `when` is a singular string; GWT xor `statement` per the type rules; `given`/`then` list items are non-empty strings.
- **Wiring:** `make spec-lint` target; pre-push via the existing LINT lane in `.ai/pre-push.json` (no new phase; the lane classifier and phase guard test get the standard once-over per the core.md row); CI via a new `spec` group in `path-filters.yml` (`'spec/**'`, flat glob per the LCD rule) driving a small lint job in `ci.yml` — dorny step, pre-push group parser, and filter-test-harness assertions updated together per the When-You-Change-X row. Linter code lives under `backend/**`, so changes to the linter itself already trigger backend CI.
- **Tests:** unit tests with fixture YAML files (valid corpus, each violation class) per the comprehensive-tests rule.

## Derivation & curation playbook

Execution context: interactive Claude (the umbrella's $0 build/curate phase).

**Sources per domain**, in rough priority order: handlers + services + repositories; Go integration/unit tests; Playwright specs; synthetic seed scenarios/profiles (they encode which states matter); `.ai/spec/` design docs; the `core.md` gotchas table (many rows are behavior intent in disguise — e.g. NULLS LAST date sorts, forward-only update semantics); git history where intent is ambiguous.

**Process per domain (one PR each):**

1. Inventory the domain's flows and surfaces.
2. Agent drafts behaviors with `type`/`status`/`provenance` per behavior.
3. **In-session walkthrough before the PR opens:** chunks of ~10 behaviors; the human accepts / edits / rejects / reclassifies (`current`→`proposed` for anything that is actually a bug or an aspiration). This is where curation happens.
4. The PR lands the file at `maturity: reviewed`. The PR description records what the walkthrough rejected or rewrote — durable evidence that curation happened.

**Anti-rubber-stamp mechanics** (curation rigor is self-enforced philosophy, supported structurally): one domain per PR; the walkthrough happens before the PR opens, so the PR review is a final read of already-curated content, not the curation itself; every behavior carries provenance so each claim is checkable against its source; the PR description's rejected/rewritten record makes an empty curation pass visible.

**Granularity guidance:** one behavior = one durable intent, one `when`; `then` lists enumerate facets of that single intent. Rough expectation: 20–50 behaviors per exemplar domain. Derivation is not transcription — a behavior is written at the intent level (survives any UI redesign), not one-per-test-assertion.

**PII:** behaviors are generic statements of intent; no real contact data, UUIDs, or hostnames appear in spec files (standard repo privacy rule applies).

## Maintenance rules

- New row in the `AGENTS.md` / `.ai/rules/core.md` "When You Change X, Check Y" table: **when app logic changes behavior in a domain that has a spec file, update the affected `spec/<domain>.yaml` behaviors in the same PR** (extend-in-place vs retire-and-mint per `spec/README.md`) and run `make spec-lint`. Scoped to domains with spec files, so underived domains impose no obligation during the backfill period. (Edit `AGENTS.md`, not the `.claude/CLAUDE.md` symlink.)
- Context Discovery section gets a pointer to `spec/README.md`.
- The full how-to (schema reference, ID lifecycle, curation rules) lives in `spec/README.md`; the rules row stays one discoverable line.
- Mechanical enforcement (orphan behaviors, dead IDs, behavior-changed-without-test-change) is Piece 3, recorded there as forward hooks.

## Sequencing

1. **PR 1 — foundation:** `spec/README.md` (taxonomy + prefix table, schema reference, ID lifecycle, curation rules) + `backend/internal/spec` parser + `backend/cmd/spec-lint` + `make spec-lint` + pre-push/CI wiring + maintenance-rules edits.
2. **PRs 2–4 — exemplar domains:** `contacts`, `cadence-followup`, `imports-matching`, one PR each: derive → walkthrough → land at `reviewed`.
3. File the Piece 1 sub-issue under #380 once this spec is committed (the umbrella asks for sub-issues as each piece is specced).

## Success criteria

- `make spec-lint` green in pre-push and CI.
- Three exemplar domains at `maturity: reviewed`, every behavior having survived the in-session walkthrough.
- Taxonomy and prefixes fixed in `spec/README.md`.
- Maintenance rules landed in `AGENTS.md` / `core.md`.
- Piece 2 can start citing behavior IDs with no further foundation work.

## Non-goals (this piece)

- No test-side ID citations or annotation format (Piece 2).
- No coverage/orphan/dead-ID checks (Piece 3).
- No tours, judge, or issue reporter (Piece 4).
- No seed-scenario↔behavior indexing (D/Piece 3 alignment; the schema is already shaped for it).
- No backfill beyond the three exemplars.
- No generated Markdown render of the corpus (can be added later if ever wanted).

## Risks

- **Rubber-stamping during curation** — the single-PR flow concentrates trust in the walkthrough. Mitigations: chunked walkthrough before the PR, per-behavior provenance, the rejected/rewritten record in the PR description.
- **Derivation codifies bugs as intent** — mitigated by the `current`/`proposed` status rule (bugs become `proposed` intended behavior + optionally a filed issue).
- **Over-derivation** (transcribing tests instead of stating intent) — mitigated by the granularity guidance and the intent-level GWT requirement.
- **Tooling gold-plating** — the linter is parse+validate only; anything smarter waits for Piece 3.

## Decisions locked (this brainstorm)

- Exemplar-first scope: schema + README + linter + three domains (`contacts`, `cadence-followup`, `imports-matching`) to `reviewed` this cycle.
- Structured YAML, one file per domain at top-level `spec/`; `spec/README.md` as index.
- Schema: `id`/`title`/`type`/`status`/GWT/`provenance`/`notes`; `given`/`then` string-or-list, `when` singular, `statement` for invariants; `given` optional.
- Status enum `current`/`proposed`/`retired` carries the is/ought split; retirement is the tombstone; IDs never reused or renumbered.
- No version field: edit in place, git is the record; extend-in-place vs retire-and-mint rule for ID lifecycle.
- Traceability: tests point at the spec; no `coverage` field; `provenance` is set-once and non-load-bearing.
- Linter: Go parser package + `spec-lint` CLI, minimal checks, pre-push LINT lane + CI `spec` path group.
- Curation: single PR per domain, in-session chunked walkthrough before the PR, PR description records curation evidence; consumers trust `reviewed`+ only.
- Maintenance rule in `AGENTS.md`/`core.md`: behavior-affecting app changes update the spec in the same PR, scoped to derived domains.
