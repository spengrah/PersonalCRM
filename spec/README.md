# Behavior Specs — the intended-behavior SSOT

This directory is the single source of truth (SSOT) for the application's intended behavior: durable, DOM-free statements of what the system is supposed to do, independent of how any test or UI currently expresses it. Consumers: deterministic tests (which cite behavior IDs — annotation format arrives with Piece 2 of #380), the Piece 3 traceability/coverage scanner, the Track B agentic QA judge, and the MCP server. Humans read it as the behavior index.

It is deliberately distinct from `.ai/spec/`, which holds design documents. This directory holds product behavior, one YAML file per domain at `spec/<domain>.yaml`. The `.yaml` extension is required — the linter globs `*.yaml` only (this README, subdirectories, and `.yml` files are ignored).

## File format

```yaml
domain: contacts
prefix: CON
maturity: reviewed   # draft | reviewed | ratified
behaviors: [...]
```

Maturity semantics (per-file, so the paradigm switches on domain-by-domain):

- `draft` — agent-derived, untrusted. Consumers MUST ignore.
- `reviewed` — human-curated behavior-by-behavior. Consumers may act on it.
- `ratified` — proven through consumption. Consumers may hard-gate on it. Nothing flips to `ratified` in the current cycle; the flip criteria belong to Pieces 3/4.

## Per-behavior schema

```yaml
- id: CON-001            # <PREFIX>-NNN, stable forever
  title: Soft-deleting a contact cascades to its dependents
  type: business-logic    # business-logic | api | ux | invariant | data
  status: current         # current | proposed | retired
  given: a contact with methods and notes
  when: the contact is deleted
  then:
    - the contact is soft-deleted
    - its contact methods are soft-deleted
    - its notes are soft-deleted
  provenance: [ContactService.DeleteContact, contact_delete_integration_test.go]
  notes: optional free text — rationale, known edge cases
```

Field rules:

- **`id`** — `<PREFIX>-NNN` (prefix from the file's `prefix` field; number zero-padded to 3 digits, growing naturally past 999 with no leading zero: `CON-001` … `CON-999`, then `CON-1000`). Monotonically assigned per domain. Never reused, never renumbered; gaps left by retirement are fine.
- **`title`** — one short line naming the durable intent.
- **`type`** — what kind of consumer cares:
  - `business-logic` — domain rules observable through outcomes (state machines, computations).
  - `api` — HTTP contract (status codes, validation, response shapes). Track A's handler tests.
  - `ux` — what the user can see/do at a surface, expressed DOM-free (no selectors, no copy). Track B's judge.
  - `invariant` — always-true cross-cutting property (e.g. soft-delete filtering, deterministic ordering).
  - `data` — persistence/derived-data correctness (cascades, dedup, derived cache columns).
- **`status`** — carries the is/ought split. `current` = faithfully describes today's behavior. `proposed` = desired behavior that does not (fully) hold today. If curation reveals today's behavior is a bug, the intended behavior is written as `proposed` (optionally with a filed bug) — **a bug is never enshrined as `current` intent**. `retired` = tombstone; the row stays intact so the ID is never reused, with a pointer in `notes` if superseded.
- **`given`** — precondition; string or list of strings. Optional: omit when there is no meaningful precondition rather than writing filler.
- **`when`** — the trigger; strictly a single string. A behavior needing two `when`s is two behaviors.
- **`then`** — observable outcome(s); string or list of strings. Each list item is an independently checkable fact (a deterministic test maps items to assertions; the judge verifies item-by-item), and schema evolution becomes one-line diffs. Items get no IDs or keys — coverage and citation stay at behavior granularity. If a list item feels like it deserves its own ID, it is a separate behavior.
- **`statement`** — for `invariant`-type behaviors only, a single string that replaces `given`/`when`/`then` entirely (forcing "all queries filter soft-deleted rows" into GWT produces noise). Mutually exclusive with GWT; other types must use GWT.
- **`provenance`** — set-once list of source references (code symbols, test files, design docs) recording what the behavior was derived from. A historical record: non-load-bearing, allowed to rot, nothing parses it for enforcement.
- **`notes`** — optional free text.

## ID lifecycle

IDs are monotonically assigned per domain, zero-padded to 3 digits, growing past 999 without a leading zero. Never reused, never renumbered; gaps are fine. There are no `version:`/`updated:` fields — behaviors are edited in place and **git history is the version record** (`git log -p spec/<domain>.yaml` carries author, date, and PR).

**Extend-in-place vs retire-and-mint:**

- If an edit **refines or extends the same intent** (a cascade set grows, wording is clarified), edit in place — the ID names the durable intent, and existing test citations stay valid.
- If an edit **reverses or replaces** the intent (e.g. "deletes are soft" → "deletes are hard"), retire the old ID and mint a new one — editing a reversal in place would silently invalidate every citation pointing at the old meaning.

A behavior that moves domains gets a new ID; the old one is retired with a pointer in `notes`. Retired behaviors keep their full row (tombstone) and must stay schema-valid.

## Domain taxonomy and prefixes

★ marks the exemplar-cycle domains (derived and curated first). All prefixes are reserved now so future files slot in without renaming; non-derived domains may still be split/merged at their own derivation time — a README table edit plus new prefixes, with the retirement rules covering any moves.

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

## Curation rules

- **One domain per PR.** Derivation is an interactive session: the agent drafts behaviors with `type`/`status`/`provenance`, then an in-session chunked walkthrough (~10 behaviors per chunk) happens **before the PR opens** — the human accepts / edits / rejects / reclassifies each behavior (`current` → `proposed` for anything that is actually a bug or an aspiration).
- The PR lands the file at `maturity: reviewed`, and the PR description records what the walkthrough rejected or rewrote — durable evidence that curation happened.
- **Granularity:** one behavior = one durable intent, one `when`; `then` lists enumerate facets of that single intent. Rough expectation: 20–50 behaviors per domain. Derivation is not transcription — write at the intent level (survives any UI redesign), not one-per-test-assertion.
- **PII:** behaviors are generic statements of intent. No real contact data, UUIDs, or hostnames appear in spec files (standard repo privacy rule).

## Linting

`make spec-lint` validates the corpus (wired into the pre-push LINT lane and CI's `spec` path group). Checks: YAML parses; required fields present (`domain`, `prefix`, `maturity`, `behaviors`; per-behavior `id`, `title`, `type`, `status`); enums valid (`type`, `status`, `maturity`); `id` matches `<PREFIX>-NNN` against the file's declared `prefix`; prefixes unique across files; IDs unique within a file and globally; `when` is a singular string; GWT xor `statement` per the type rules; `given`/`then` list items are non-empty strings.

The linter is minimal by design — parse + validate only. Coverage/orphan/dead-ID checks and behavior-changed-without-test-change detection are Piece 3.

## Maintenance rule

Behavior-affecting app changes in a domain that has a `spec/<domain>.yaml` file must update the affected behaviors **in the same PR** (extend-in-place vs retire-and-mint per the ID lifecycle above) and run `make spec-lint`. Domains without a spec file impose no obligation (backfill period).
