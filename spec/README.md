# Behavior Specs — the intended-behavior SSOT

This directory is the single source of truth (SSOT) for the application's intended behavior: durable, DOM-free statements of what the system is supposed to do, independent of how any test or UI currently expresses it. Consumers: deterministic tests (which cite behavior IDs — see [Test → behavior citations](#test--behavior-citations)), the Piece 3 traceability/coverage scanner, the Track B agentic QA judge, and the MCP server. Humans read it as the behavior index.

It is deliberately distinct from `.ai/spec/`, which holds design documents. This directory holds product behavior, one YAML file per domain at `spec/<domain>.yaml`. The `.yaml` extension is required — the linter globs `*.yaml` only (this README, subdirectories, and `.yml` files are ignored).

## File format

```yaml
domain: contacts
prefix: CON
maturity: reviewed   # draft | reviewed | ratified
e2e_settled: false   # optional; true flips the coverage scanner from warn to block for this domain
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
  surface: none           # ui | api | none — which deterministic-test layer owns proving it
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
  - `ux` — what the user can see/do at a surface, expressed DOM-free (no selectors, no copy). Track B's judge. `ux` behaviors are also produced *forward* by design sessions (including AI design work): a design mints its intended surface behavior as `status: proposed`, the implementation PR flips it to `current` under the maintenance rule, and a design that reverses an existing surface follows retire-and-mint.
  - `invariant` — always-true cross-cutting property (e.g. soft-delete filtering, deterministic ordering).
  - `data` — persistence/derived-data correctness (cascades, dedup, derived cache columns).
  - `intent` — a judged experience goal: the durable purpose its serving `ux` behaviors exist for (e.g. "the dashboard tells the user who to reach out to, at a glance"), expressed as a single `statement`. Consumed ONLY by the Track B agentic judge — an intent is by construction not provable by a deterministic test, so **deterministic tests never cite intent IDs** (the coverage scanner rejects such citations as invalid). Intents are the top of the design-session grain ladder: a design mints its goal as a `proposed` intent first, granular `ux` behaviors follow as planning progresses, and the implementation flips both `current`. The judge grades a `current` intent as regression detection and a `proposed` intent as progress detection.
- **`surface`** — which deterministic-test layer owns proving the behavior; drives the Piece 3 coverage scanner. `ui` = a browser-driven E2E test must cite it (every then-item covered or waived); `api` = the HTTP contract is owned by the Go API/handler tests; `none` = backend-internal, owned by unit/integration tests. Required on every non-intent, non-retired behavior; forbidden on intents (judge-only). Classification rule: a browser-facing `type: ux` behavior is always `ui`; a `ux` behavior of a non-browser surface (mac-daemon notifications, CLI flows) is `none` — no browser test can ever reach it; other types are `ui` only if a then-clause asserts something a user can see in the browser.
- **`status`** — carries the is/ought split. `current` = faithfully describes today's behavior. `proposed` = desired behavior that does not (fully) hold today. If curation reveals today's behavior is a bug, the intended behavior is written as `proposed` (optionally with a filed bug) — **a bug is never enshrined as `current` intent**. `retired` = tombstone; the row stays intact so the ID is never reused, with a pointer in `notes` if superseded.
- **`given`** — precondition; string or list of strings. Optional: omit when there is no meaningful precondition rather than writing filler.
- **`when`** — the trigger; strictly a single string. A behavior needing two `when`s is two behaviors.
- **`then`** — observable outcome(s); string or list of strings. Each list item is an independently checkable fact (a deterministic test maps items to assertions; the judge verifies item-by-item), and schema evolution becomes one-line diffs. Items get no IDs or keys — tests address them positionally (`ID[n]`, 0-based file position; see the citation rules below), which is why a then-list edit must update citing indexes in the same PR. If a list item feels like it deserves its own ID, it is a separate behavior.
- **`statement`** — for `invariant`- and `intent`-type behaviors only, a single string that replaces `given`/`when`/`then` entirely (forcing "all queries filter soft-deleted rows" — or an experience goal — into GWT produces noise). Mutually exclusive with GWT; other types must use GWT.
- **`serves`** — optional, `ux` and `intent` behaviors only: a list of intent IDs this behavior contributes evidence toward. Cross-domain references are legal and expected (a cadence-domain behavior may serve a dashboard-domain intent — the linter resolves corpus-wide); an intent may serve a broader intent, giving the multi-grain refinement ladder. Every target must exist and be `type: intent`. The judge inverts these edges to bind captured evidence to each intent, so an unserved intent is judgeable only by captures tagged with its ID directly.
- **`waivers`** — ui-surface behaviors only: a list of `{then: <0-based index>, reason: <text>}` entries recording the deliberate decision (the relaxation rubric's DROP verdict) that a then-item is neither deterministically E2E-provable nor worth a judge intent. The coverage scanner reports waived items loudly (with the reason) instead of counting them as orphans, and flags a stale waiver when a waived item gains a citing test.
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

- **Curation is an adversarial-review pass, not an interactive walkthrough.** The agent drafts behaviors with `type`/`status`/`provenance`, then a fresh-context reviewer (Codex at xhigh, or a Claude reviewer when Codex is quota-blocked) audits the whole file for completeness (missing in-scope behaviors) and correctness (fidelity to code, `current`-vs-`proposed` classification, type/consumer-harness fit) until it passes with zero blocking findings; a final cross-corpus consistency pass over the whole SSOT catches inter-file duplication, contradiction, and boundary drift. Every derived behavior is verified against code before it is enshrined, and a bug is written as `proposed`, never `current`. (The earlier interactive human-by-behavior walkthrough is superseded by this pass.)
- **PR granularity is flexible.** One domain per PR is the default and keeps git history clean, but a batched multi-domain PR is fine when the whole set went through the review + consistency pass together (the exemplar backfill landed nine domains in one PR). Either way the PR lands the file(s) at `maturity: reviewed`, and the PR description records what review rejected or rewrote — durable evidence that curation happened.
- **Granularity:** one behavior = one durable intent, one `when`; `then` lists enumerate facets of that single intent. Rough expectation: 20–50 behaviors per domain. Derivation is not transcription — write at the intent level (survives any UI redesign), not one-per-test-assertion.
- **PII:** behaviors are generic statements of intent. No real contact data, UUIDs, or hostnames appear in spec files (standard repo privacy rule).

## Test → behavior citations

Deterministic tests cite the behavior IDs they cover with a source-comment marker. The pointer points **into** the SSOT (a test names the behavior it proves); the SSOT never points back at tests — there is no `coverage` field. Piece 3's traceability scanner reads these markers and diffs them against the corpus; Piece 2 ships only the format and its first hand-applied uses, no tooling.

**Marker.** A citation is a line comment of the exact form `// spec: <ref>[, <ref> ...]` — literally `//`, one space, `spec:`, then one or more references separated by commas, where each reference is a behavior ID (`IMP-021`) or a then-item reference (`IMP-021[2]`, 0-based index into the behavior's `then` list). The marker must be the only content on its line — in a scanned test file, a `// spec:` that trails other content (e.g. after an assertion) is reported as an invalid citation rather than silently ignored, so a mis-placed marker can never rot unseen. To cite many references, either comma-separate them on one line or stack multiple `// spec:` lines. The same marker string is used on every test surface (Go `testing`, Playwright, Vitest, and any future MCP tests): a line comment is byte-identical across surfaces, inert (it cannot break compilation or a test run), and couples test code to no framework annotation API.

**Placement.** Put the citation next to the assertions that prove the behavior. Two canonical placements:

- **Function level** — the marker sits on the line(s) immediately preceding the test declaration (`func TestXxx`, `test(...)`, `test.describe(...)`), separated from it only by blank or other comment lines. It binds to the whole test.
- **Subtest level** — the marker is the first statement line(s) inside a `t.Run("name", func(t *testing.T) { ... })` body (or inside a `test(...)` body). It binds to that subtest. Prefer this when only some subtests prove the cited behavior — a function-level marker binds to every subtest, including generic ones that prove no behavior.

**Granularity and cardinality.** E2E tests cite per then-item (`CON-045[3]`) — the granularity the verifier→E2E migration established — so the coverage scanner can tell which facets of a behavior a test actually proves. A bare behavior-granular citation (`IMP-021`) is also legal and claims the whole behavior (every then-item); it is the norm for the Go API tests and for statement behaviors, which have no then list to index. Then-item indexes are 0-based file positions, not sub-IDs: reordering or inserting `then` items shifts them, so a PR that edits a cited behavior's `then` list must update the citing tests' indexes in the same PR (the scanner catches truncation via out-of-range indexes, but cannot detect a reorder). Cardinality is free N:M: one test may cite several references, and one then-item may be cited by many tests. No uniqueness constraint. (An earlier rule said citations are behavior-granular only; the per-then-item convention supersedes it.)

**Cite-on-write.** Every new or deliberately-relaxed test carries citations. The existing suite is **not** retrofitted — untouched tests stay un-cited and read as orphans in Piece 3's report, which drives a later backfill. Going forward, cite-on-write is the soft norm for new and changed tests.

**A citation asserts truth.** `// spec: X` means *this test verifies behavior X holds today*. Cite only `status: current` behaviors your test actually asserts green — never cite a `proposed` behavior as if a passing test proved it (a bug is never enshrined as `current`; see the maintenance rule). Never cite an `intent` behavior: intents are judge-only by construction (a deterministic test cannot prove an experience goal), and the coverage scanner rejects such citations. Not every assertion maps to a behavior: a generic framework-level contract (an unknown-id 404, a malformed-input 400) that no behavior owns simply carries no marker.

**Worked examples.**

```go
// Go — subtest-level, citing the api behavior the subtest proves.
func TestRematchAPI_PollableRescan(t *testing.T) {
    t.Run("rescan with an eligible method returns a pollable job", func(t *testing.T) {
        // spec: IMP-021
        ...
    })
}
```

```ts
// Playwright — function-level above the describe, plus a then-item-level cite inside.
// spec: IMP-021
test.describe('Rematch on add email', () => {
  test('adding a matching email links a past event', async ({ page, request }) => {
    // spec: CAL-019, IMP-021[2]
    ...
  })
})
```

**The coverage scanner (Piece 3).** `make spec-coverage` (backend/cmd/spec-coverage) cross-references citations in the deterministic test surfaces (backend `*_test.go`, `frontend/tests/e2e/*.spec.ts`) against the corpus and reports, per domain:

- **covered** — a then-item of a `surface: ui`, `status: current` behavior cited by an E2E test (bare cite covers all items; indexed cite covers item `n`; a `ui` invariant's statement is one implicit item, coverable only bare).
- **waived** — a then-item with a `waivers` entry; reported loudly with the reason. A waived item that gains a citing test is flagged as a stale waiver (warning).
- **orphan** — a then-item of a `ui`/`current` behavior with neither citation nor waiver. Warn-only by default; hard-fails once the domain's spec file declares `e2e_settled: true`.
- **invalid citations** — always a failure, regardless of settlement: a malformed marker, an unknown ID (dead reference), an out-of-range then-index, an indexed cite of a statement behavior, or a cite of an `intent` (judge-only), `proposed` (a citation asserts truth), or `retired` behavior.

`api`- and `none`-surface behaviors are exempt from E2E coverage by construction; an E2E citation of a non-`ui` behavior draws a warning (it usually means the surface tag is wrong). The Go API tests' citations are validated for deadness but do not count toward ui coverage.

## Linting

`make spec-lint` validates the corpus (wired into the pre-push LINT lane and CI's `spec` path group). Checks: YAML parses; required fields present (`domain`, `prefix`, `maturity`, `behaviors`; per-behavior `id`, `title`, `type`, `status`, and `surface` on non-intent non-retired behaviors); enums valid (`type`, `status`, `maturity`, `surface`); `id` matches `<PREFIX>-NNN` against the file's declared `prefix`; prefixes unique across files; IDs unique within a file and globally; `when` is a singular string; GWT xor `statement` per the type rules; `given`/`then`/`serves` list items are non-empty strings; `serves` appears only on `ux`/`intent` behaviors and every target resolves corpus-wide to an existing `intent` behavior (self-references rejected); `surface` forbidden on intents; `e2e_settled` a boolean; `waivers` only on ui-surface behaviors, with in-range unique then-indexes and non-empty reasons.

The linter is minimal by design — parse + validate only. Coverage/orphan/dead-ID checks live in the Piece 3 scanner (`make spec-coverage`, above); behavior-changed-without-test-change detection remains future work.

## Maintenance rule

Behavior-affecting app changes in a domain that has a `spec/<domain>.yaml` file must update the affected behaviors **in the same PR** (extend-in-place vs retire-and-mint per the ID lifecycle above) and run `make spec-lint`. Domains without a spec file impose no obligation (backfill period).
