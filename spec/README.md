# Behavior Specs — the intended-behavior SSOT

This directory is the single source of truth (SSOT) for the application's intended behavior: durable, DOM-free statements of what the system is supposed to do, independent of how any test or UI currently expresses it. Consumers: deterministic tests (which cite behavior IDs — see [Test → behavior citations](#test--behavior-citations)), and the traceability/coverage scanner (`make spec-coverage`). The Track B agentic QA judge consumes the corpus *indirectly* — it grades against hand-maintained verbatim transcriptions (`judge/spec-catalog.ts`, `judge/intent-catalog.ts`), and the only judge code that parses `spec/*.yaml` is the offline sync guard `intent-catalog.test.ts`. (An MCP server was planned as a further consumer; none reads the corpus today.) Humans read it as the behavior index.

It is deliberately distinct from `.ai/spec/`, which holds design documents. This directory holds product behavior, one YAML file per domain at `spec/<domain>.yaml`. The `.yaml` extension is required — the linter globs `*.yaml` only (this README, subdirectories, and `.yml` files are ignored).

## File format

```yaml
domain: contacts
prefix: CON
maturity: reviewed   # draft | reviewed | ratified
settled: [ui, api]   # optional; surfaces whose orphans block instead of warn (ui | api).
                     # All twelve domains currently list BOTH.
behaviors: [...]
```

Maturity semantics (per-file, so the paradigm switches on domain-by-domain):

- `draft` — agent-derived, untrusted. Consumers MUST ignore.
- `reviewed` — human-curated behavior-by-behavior. Consumers may act on it.
- `ratified` — proven through consumption. Consumers may hard-gate on it. **No file has ever been flipped to `ratified`** — all twelve are `reviewed` — and the criteria for flipping one have not been written.

## Per-behavior schema

```yaml
- id: XXX-001            # <PREFIX>-NNN, taken from the file's own prefix, stable forever.
                         # XXX stands in for that prefix: this block is a format template,
                         # not a corpus entry, so its id resolves to no real behavior.
  title: Soft-deleting a contact cascades to its dependents
  type: business-logic    # business-logic | api | ux | invariant | data | intent
  status: current         # current | proposed | retired
  surface: none           # ui | api | none — which deterministic-test layer owns proving it
  given: a contact with methods and notes
  when: the contact is deleted
  then:
    - the contact is soft-deleted                   # uncited item — stays a plain string
    - key: methods-soft-deleted                     # cited or waived — carries a permanent key,
      text: its contact methods are soft-deleted    #   so a test cites it as <ID>.methods-soft-deleted
    - its notes are soft-deleted
  provenance: [ContactService.DeleteContact, contact_delete_integration_test.go]
  notes: optional free text — rationale, known edge cases
```

Field rules:

- **`id`** — `<PREFIX>-NNN` (prefix from the file's `prefix` field; number zero-padded to 3 digits, growing naturally past 999 with no leading zero: `CON-001` … `CON-999`, then `CON-1000`). Monotonically assigned per domain. Never reused, never renumbered; gaps in the sequence are fine. (Retirement does *not* create one — a retired behavior keeps its row as a tombstone. The corpus's existing gaps are IDs that were drafted and dropped before ever being committed.)
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
- **`then`** — observable outcome(s); a string, or a list whose items are each **either** a plain string **or** a `{key, text}` mapping. Each item is an independently checkable fact (a deterministic test maps items to assertions; the judge verifies item-by-item), and schema evolution becomes one-line diffs. If a list item feels like it deserves its own ID, it is a separate behavior.

  An item's **`key`** is its permanent citation handle: lowercase-kebab (`^[a-z0-9]+(-[a-z0-9]+)*$`), unique within the behavior, never the reserved word `statement`. A key is minted the moment something first cites the item **by key** or waives it, and is **never regenerated, renumbered, or renamed by tooling** — an item stays a plain string until then. Note a bare `ID` citation claims every then-item without keying any of them, so an item can be cited and covered while still carrying no key. Nothing ever withdraws a key either, so an item keeps its handle after its last citation goes away (it just becomes an orphan). Because a citation binds to the key rather than to a position, reordering, inserting, and deleting *sibling* items are no-ops for citations. Renaming or deleting a key is a deliberate human act, and it must update every citing test **and** every waiver naming it in the same PR (see [Citing an item that has no key yet](#citing-an-item-that-has-no-key-yet)).
- **`statement`** — for `invariant`- and `intent`-type behaviors only, a single string that replaces `given`/`when`/`then` entirely (forcing "all queries filter soft-deleted rows" — or an experience goal — into GWT produces noise). Mutually exclusive with GWT; other types must use GWT.
- **`serves`** — optional, `ux` and `intent` behaviors only: a list of intent IDs this behavior contributes evidence toward. Cross-domain references are legal and expected (a cadence-domain behavior may serve a dashboard-domain intent — the linter resolves corpus-wide); an intent may serve a broader intent, giving the multi-grain refinement ladder. Every target must exist and be `type: intent`. The judge inverts these edges to bind captured evidence to each intent, so an unserved intent is judgeable only by captures tagged with its ID directly.
- **`waivers`** — ui- or api-surface behaviors only (never `none`, never intents): a list of `{then: <then-item key>, reason: <text>}` entries recording the deliberate decision (the relaxation rubric's DROP verdict) that a then-item is neither deterministically provable by its owning harness (E2E for `ui`, Go for `api`) nor worth a judge intent. A waiver addresses its item by **key**, so an item that is still a plain string must be converted to a `{key, text}` mapping before it can be waived at all; a statement behavior's single implicit item — it has no `then` list — is addressed by the reserved token `then: statement`. The coverage scanner reports waived items loudly (with the reason) instead of counting them as orphans, and flags a stale waiver when a waived item gains a citing test.
- **`provenance`** — set-once list of source references (code symbols, test files, design docs) recording what the behavior was derived from. A historical record: non-load-bearing, allowed to rot, nothing parses it for enforcement.
- **`notes`** — optional free text.

## ID lifecycle

IDs are monotonically assigned per domain, zero-padded to 3 digits, growing past 999 without a leading zero. Never reused, never renumbered; gaps are fine — and, per above, retirement is not what makes them. There are no `version:`/`updated:` fields — behaviors are edited in place and **git history is the version record** (`git log -p spec/<domain>.yaml` carries author, date, and PR).

**Extend-in-place vs retire-and-mint:**

- If an edit **refines or extends the same intent** (a cascade set grows, wording is clarified), edit in place — the ID names the durable intent, and existing test citations stay valid.
- If an edit **reverses or replaces** the intent (e.g. "deletes are soft" → "deletes are hard"), retire the old ID and mint a new one — editing a reversal in place would silently invalidate every citation pointing at the old meaning.

A behavior that moves domains gets a new ID; the old one is retired with a pointer in `notes`. Retired behaviors keep their full row (tombstone) and must stay schema-valid.

## Domain taxonomy and prefixes

★ marks the exemplar-cycle domains (derived and curated first). **The corpus is complete: all fourteen rows have a shipped file at `maturity: reviewed`, and every prefix here is in use — none is held in reserve.** A domain split or merge from here is a README table edit plus new prefixes, with the retirement rules covering any moves.

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
| whatsapp | `WHA` | whatsmeow client lifecycle, pairing, history capture, ingest |
| interactions | `IXN` | contact interactions surface: unified list + drill-down read layer, venue tags, filters |

## Curation rules

- **Curation is an adversarial-review pass, not an interactive walkthrough.** The agent drafts behaviors with `type`/`status`/`provenance`, then a fresh-context reviewer (Codex at xhigh, or a Claude reviewer when Codex is quota-blocked) audits the whole file for completeness (missing in-scope behaviors) and correctness (fidelity to code, `current`-vs-`proposed` classification, type/consumer-harness fit) until it passes with zero blocking findings; a final cross-corpus consistency pass over the whole SSOT catches inter-file duplication, contradiction, and boundary drift. Every derived behavior is verified against code before it is enshrined, and a bug is written as `proposed`, never `current`. (The earlier interactive human-by-behavior walkthrough is superseded by this pass.)
- **PR granularity is flexible.** One domain per PR is the default and keeps git history clean, but a batched multi-domain PR is fine when the whole set went through the review + consistency pass together (the exemplar backfill landed nine domains in one PR). Either way the PR lands the file(s) at `maturity: reviewed`, and the PR description records what review rejected or rewrote — durable evidence that curation happened.
- **Granularity:** one behavior = one durable intent, one `when`; `then` lists enumerate facets of that single intent. Rough expectation: 20–50 behaviors per domain. Derivation is not transcription — write at the intent level (survives any UI redesign), not one-per-test-assertion.
- **PII:** behaviors are generic statements of intent. No real contact data, UUIDs, or hostnames appear in spec files (standard repo privacy rule).

## Test → behavior citations

Deterministic tests cite the behavior IDs they cover with a source-comment marker. The pointer points **into** the SSOT (a test names the behavior it proves); the SSOT never points back at tests — there is no `coverage` field. The scanner reads these markers and checks them against the corpus: `make spec-coverage` for citation validity and per-then-item coverage, `make spec-lint` for the corpus itself, and `make spec-drift` as a warn-only advisory. All three ship and run in CI and the pre-push lint lane.

**Marker.** A citation is a line comment of the exact form `// spec: <ref>[, <ref> ...]` — literally `//`, one space, `spec:`, then one or more references separated by commas, where each reference is a behavior ID (`IMP-021`) or a then-item **key** reference (`CAL-019.contact-appended-each-matching`). The positional `ID[n]` form is **retired**: the scanner rejects it with a targeted *cite the then-item by key* message rather than silently accepting a pointer that can re-point. The marker must be the only content on its line — in a scanned test file, a `// spec:` that trails other content (e.g. after an assertion) is reported as an invalid citation rather than silently ignored, so a mis-placed marker can never rot unseen. To cite many references, either comma-separate them on one line or stack multiple `// spec:` lines. **The marker is only meaningful on the two scanned harnesses — Go `testing` and Playwright E2E.** Those are the only roots `spec-coverage` reads (`backend/**/*_test.go`, `frontend/tests/e2e/**/*.spec.ts`), so a marker written anywhere else — a Vitest unit or component test, say — is validated by **nothing**: its ID can die and its key can be renamed with no gate noticing, which is worse than no marker because it reads as coverage that does not exist. Reference a behavior from an unscanned test in **prose** instead (`the overdue card (CAD-026) must not …`). The marker *form* is deliberately framework-neutral — a line comment is byte-identical across surfaces, inert (it cannot break compilation or a test run), and couples test code to no framework annotation API — so admitting a new harness later needs no new syntax, only a decision about what that harness's citations would credit (`Citation.E2E` is two-valued today: E2E credits `ui`, Go credits `api`).

**Placement.** Put the citation next to the assertions that prove the behavior. Two canonical placements:

- **Function level** — the marker sits on the line(s) immediately preceding the test declaration (`func TestXxx`, `test(...)`, `test.describe(...)`), separated from it only by blank or other comment lines. It binds to the whole test.
- **Subtest level** — the marker is the first statement line(s) inside a `t.Run("name", func(t *testing.T) { ... })` body (or inside a `test(...)` body). It binds to that subtest. Prefer this when only some subtests prove the cited behavior — a function-level marker binds to every subtest, including generic ones that prove no behavior.

**Granularity and cardinality.** E2E tests cite per then-item (`CON-045.placeholder-year-birthdays-no-age`) — the granularity the verifier→E2E migration established — so the coverage scanner can tell which facets of a behavior a test actually proves. A bare behavior-granular citation (`IMP-021`) is also legal and claims the whole behavior (every then-item). **Keyed is the norm on both harnesses** — measured over the shipped tree, E2E citations are 291 keyed / 0 bare, and Go citations of `surface: api` behaviors are 298 keyed / 46 bare (6% bare inside `backend/tests/api/`). Reach for bare when the test really does prove every then-item, and for a statement behavior, which has no then list to key; prefer keyed otherwise, because a bare cite marks facets covered that the test may not actually prove and suppresses the orphan signal for them. A keyed citation binds to the item's **identity**, so reordering, inserting, and deleting sibling items cannot re-point it; a citation naming a key the behavior does not carry is a hard failure whose message **enumerates the behavior's available keys** (or says it has none, when the behavior carries no keyed items at all). Cardinality is free N:M: one test may cite several references, and one then-item may be cited by many tests. No uniqueness constraint. (An earlier rule said citations are behavior-granular only; the per-then-item convention supersedes it. An earlier revision addressed items positionally and warned that the scanner could not detect a reorder — that defect is what stable keys closed.)

### Citing an item that has no key yet

A `then` item carries a key only once something first cites or waives it, so citing one for the first time usually means minting one. Three cases below, plus how to tell when the key already exists and you should not mint at all.

**1. Cite an uncited then-item.** Find it in `spec/<domain>.yaml` and convert it from a plain string to a mapping:

```diff
       then:
-      - the row disappears from the overdue list
+      - key: row-leaves-overdue-list
+        text: the row disappears from the overdue list
```

Choose the key to describe the *claim*, since it is permanent: lowercase-kebab, unique within the behavior, not `statement`, and keep any negation the item turns on (`no`, `not`, `never`, `without`) — a key that reads as the opposite of its item is worse than no key. Then write `// spec: <ID>.row-leaves-overdue-list` (the ID of the behavior you just edited) and run `make spec-lint && make spec-coverage`. Sibling items are untouched and nothing is re-indexed. The diff above is illustrative — every item that anything cites **by key** or waives already carries one, so the plain-string side of it is what an item looks like before that. (Plain strings under a bare-cited behavior are a separate case: covered, but never keyed.)

**2. Waive an uncited then-item.** Same conversion first. A waiver addresses its item by key (`- then: row-leaves-overdue-list`), so a plain-string item cannot be waived at all — and the failure you get instead, `waiver then "…" names no then-item key of this behavior` from **spec-lint**, reads like a typo rather than "convert the item first".

**3. Cite a statement behavior** (`invariant` / `intent`-shaped, no `then` list) — **bare only**, e.g. `// spec: IMP-014` from a Go test (IMP-014 is `surface: api`, so a Go cite credits it). `ID.key` is rejected (`names a then-item key on a statement behavior (no then items)`), and the reserved `then: statement` token is for *waivers*, never for citations.

Starting from a coverage report rather than from the YAML? **Read which form the orphan line prints — the two mean different things.**

- `ORPHAN (blocking) <ID>.some-key: …` — the item **already carries a key**; it is orphaned because nothing currently cites or waives it (a citation was deleted, or renamed and left dangling, or the key was minted for a waiver that has since gone). The printed reference **is** the citable handle: paste it straight into your marker. Do **not** follow case 1 — the item needs no conversion, and minting a second key or renaming the existing one is the one thing keys exist to prevent.
- `ORPHAN (blocking) <ID>[3] (no key yet): …` — the item has **no key yet**, so the report falls back to its position and says so. *That* reference is a **location**, not a handle: pasting `<ID>[3]` into a marker is rejected. Use it to find the item in the YAML, then follow case 1.
- `ORPHAN (blocking) <ID>: …` — bare, no key and no index: this is a **statement behavior**'s single implicit item, which has no `then` list to key or index. Cite it bare (case 3); there is nothing to convert.

A deleted citation, or one left dangling by a half-finished rename, is what produces the first form — so expect it whenever the report and a rename appear together, and just cite what it printed.

**Cite-on-write.** Every new or deliberately-relaxed test **on a scanned harness** (Go, Playwright E2E) carries citations, and for a `ui`- or `api`-surface behavior this is **enforced, not a soft norm**: both surfaces are settled in every domain, so an uncovered then-item blocks (see the coverage scanner below). The historical backfill this section once forecast is **complete** — the suite was retrofitted domain by domain, the corpus currently reports **zero orphans across all twelve domains**, and many long-standing tests carry markers added by that backfill rather than by their original author. A test proving a `none`-surface behavior still carries no obligation.

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
// Playwright — the bare cite above the describe claims the whole behavior;
// the keyed cite inside claims one then-item of another. BOTH name
// surface: ui behaviors, because an E2E citation credits ui coverage only —
// cite a surface: api behavior from Playwright and the scanner warns and
// credits nothing.
// spec: IMP-040
test.describe("A linked candidate's new methods reach the contact", () => {
  test('adding a matching email links a past event', async ({ page, request }) => {
    // spec: CAL-019.contact-appended-each-matching
    ...
  })
})
```

Every reference in both examples is **real and surface-matched**: `IMP-040` and `CAL-019` are both `surface: ui` (so a Playwright cite credits them), `CAL-019` does carry a then item keyed `contact-appended-each-matching`, and the Go example's `IMP-021` is `surface: api` (so a Go cite credits it) with all-unkeyed items, which is why it is cited bare. Nothing scans this README, so keeping these real — and on the right harness — is a deliberate discipline rather than something a gate enforces. Re-check them when you edit this section rather than inventing a plausible-looking handle.

**The coverage scanner (Piece 3).** `make spec-coverage` (backend/cmd/spec-coverage) cross-references citations in the deterministic test surfaces (backend `*_test.go`, `frontend/tests/e2e/*.spec.ts`) against the corpus and reports, per domain:

- **covered** — a then-item of a `status: current` behavior cited by the harness its surface owns: a `surface: ui` behavior by an **E2E** test, a `surface: api` behavior by a **Go** test (a bare cite covers all items; a keyed cite covers the item carrying that key; an invariant's statement is one implicit item, coverable only bare).
- **waived** — a then-item with a `waivers` entry; reported loudly with the reason. A waived item that gains a citing test is flagged as a stale waiver (warning).
- **orphan** — a then-item of a `ui`- or `api`-surface `current` behavior with neither citation nor waiver. The two surfaces' orphan populations are counted independently; a surface's orphans **block** only when the domain lists that surface in its `settled` list, else warn. **Every domain lists BOTH `ui` and `api`** (ui since the E2E surface settlement, api since the api-citation backfill), so a new or changed then-item on **either** surface must land with its citing test — an E2E test for `ui`, a Go test for `api` — or an explicit waiver, in the same PR. There is no warn-only surface left: an orphan on either one blocks. An orphan line renders the item by **key** (`ORPHAN <ID>.some-key: …`) when it already carries one — orphaned and keyed are independent, so an item whose citation was deleted or left dangling keeps its key, and that reference can be cited as printed. An item that has no key yet falls back to its **position**, tagged so the two cannot be confused (`ORPHAN <ID>[3] (no key yet): …`), and *that* reference is a location rather than a citable handle. See [Citing an item that has no key yet](#citing-an-item-that-has-no-key-yet) for which of the two you are looking at and what to do about it.
- **invalid citations** — always a failure, regardless of settlement: a malformed marker, an unknown ID (dead reference), a **retired positional `ID[n]` reference**, a key the behavior does not carry (the message enumerates the ones it does, or says there are none), a keyed cite of a statement behavior (which has no then list), a reference carrying the reserved `@` suffix (see the drift advisory below — the content hash it was held for was declined, and `@` stays rejected so nothing squats it), or a cite of an `intent` (judge-only), `proposed` (a citation asserts truth), or `retired` behavior.

`none`-surface behaviors are exempt from coverage by construction (no orphan is emitted for them yet). Coverage credit is keyed on the behavior's `surface`: an **E2E** citation credits a `ui` behavior, a **Go** citation credits an `api` behavior. An E2E citation of a non-`ui` behavior draws a warning (it usually means the surface tag is wrong) and grants no api coverage; there is deliberately **no** mirror warning for a Go citation of a `ui` or `none` behavior — Go tests legitimately cite those surfaces all over the tree (and the `none` cites are the future `none`-coverage corpus), so a Go cite of a non-`api` behavior is silent and grants no coverage on that surface.

## Linting

`make spec-lint` validates the corpus (wired into the pre-push LINT lane and CI's `spec` path group). Checks: YAML parses; required fields present (`domain`, `prefix`, `maturity`, `behaviors`; per-behavior `id`, `title`, `type`, `status`, and `surface` on non-intent non-retired behaviors); enums valid (`type`, `status`, `maturity`, `surface`); `id` matches `<PREFIX>-NNN` against the file's declared `prefix`; prefixes unique across files; IDs unique within a file and globally; `when` is a singular string; GWT xor `statement` per the type rules; `given`/`then`/`serves` list items are non-empty strings; `serves` appears only on `ux`/`intent` behaviors and every target resolves corpus-wide to an existing `intent` behavior (self-references rejected); `surface` forbidden on intents; `settled`, when present, a non-empty list of surfaces (each ∈ {ui, api}; `none` reserved for later; no duplicates) — an explicit `settled: null` or `settled: []` is rejected, omit the key entirely for a genuinely unsettled domain — and the retired `e2e_settled` key is rejected; then-item keys — a keyed item is exactly `{key, text}`, both non-empty strings, keys matching the lowercase-kebab charset, unique within a behavior among keyed items, and never the reserved `statement`; `waivers` only on ui- or api-surface behaviors, each `then` naming a then-item key of the same behavior (or the reserved `statement` token on a statement behavior), unique, with a non-empty reason.

The linter is minimal by design — parse + validate only. Coverage/orphan/dead-ID checks live in the Piece 3 scanner (`make spec-coverage`, above).

**Behavior-drift advisory (Piece 3).** `make spec-drift` (backend/cmd/spec-drift) catches the classic silent-drift case: a PR edits what a behavior asserts but touches none of its covering tests. It is a **warn-only** advisory — it diffs the assertable content (`given`/`when`/`then`/`statement`, order-sensitive so a `then` reorder counts) of each behavior by ID against `merge-base(HEAD, develop)`, with both corpora and the citations materialized from git (never the working tree, so an uncommitted revert cannot hide a committed change and a locally-deleted citation cannot suppress a warning). When a changed behavior has ≥1 citing test file and none of them was touched in the diff, it warns (`file:line` of each citation); a zero-citation change is the coverage scanner's orphan job, and a change whose citing file was also edited is silent. Under CI it emits `::warning::` annotations so drift surfaces in the checks UI; it is wired into the CI spec-lint job and the pre-push lint lane. Exit **0** whether or not warnings were emitted (a reworded-but-unchanged-meaning assertion is legitimate, so drift never blocks); exit **2** only on a git/operational error — an unresolvable base, an unreadable tree, or a git failure — never fail-open. Then-item **keys are not assertable content**, so minting or renaming a key alone does not drift; a rename cannot happen silently anyway, since spec-coverage hard-fails every dangling citation and spec-lint every dangling waiver.

**Drift is the standing answer here, not a placeholder.** A citation content-hash suffix (`ID.key@a3f2`, pinning which *wording* a citation proves) was designed and declined: it would have turned every reword into a blocking re-confirmation of every citing test, where drift makes the same observation as a warning an author can read and dismiss — and a reworded-but-unchanged-meaning assertion is legitimate often enough that blocking is the wrong default. The `@` character stays rejected so a later reversal would still be a pure addition (`ID.key` keeps its meaning verbatim, no second migration), but nothing is waiting on it.

## Maintenance rule

Behavior-affecting app changes in a domain that has a `spec/<domain>.yaml` file must update the affected behaviors **in the same PR** (extend-in-place vs retire-and-mint per the ID lifecycle above) and run **both** `make spec-lint` (the corpus is well-formed) and `make spec-coverage` (every `ui`/`api` then-item is cited or waived — this is the one that blocks on a new uncovered item). Domains without a spec file impose no obligation (backfill period).
