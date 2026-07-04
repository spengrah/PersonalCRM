# Piece 2 · Track A — Test → behavior citations (design)

Design for the foundational half of Piece 2 of the Agentic UX QA program (#380): the convention by which deterministic tests cite the behavior IDs they cover, plus the coverage strategy and scope boundaries that surround it. This is the design deliverable; writing the actual handler tests and relaxed E2E flows is implementation that applies this convention and is planned separately.

## Context

Piece 1 landed the behavior SSOT: `spec/*.yaml` (12 domains, 393 behaviors at `maturity: reviewed`) plus the `backend/internal/spec` parser/linter that reads it. The Piece 1 design deliberately deferred one thing to Piece 2 — *"tests cite behavior IDs (annotation format defined in Piece 2)"*. This document defines that annotation format.

The traceability direction is already settled and is not reopened here: all pointers point **into** the SSOT; nothing in the SSOT points back at tests (there is no `coverage` field in spec files). Tests carry the citation; Piece 3's scanner computes coverage by reading tests and diffing against the corpus. This design produces the citation format; it does not build the scanner.

Piece 2 as a whole bundles three workstreams: (A) this citation convention, (B) new API-level handler tests for the ~5 uncovered handlers, and (C) "Playwright relaxation" of the thin E2E layer. (A) is the reusable foundation both (B) and (C) apply. This document designs (A) in full and sets the scope/strategy for (B) and (C); the concrete test-writing in (B)/(C) is implementation planning.

## The citation convention

### Marker

A citation is a line comment of the form:

```
// spec: <ID>[, <ID> ...]
```

- The token is literally `// spec:` — `//`, a single space, `spec:`. Chosen because a line comment is the one carrier that is byte-identical across every test surface in the repo (Go `testing`, Playwright, Vitest, and any future MCP tests), is inert (it cannot break compilation, a test run, or be coupled to a framework's annotation API/version), and imposes zero dependency from test code onto the spec package.
- IDs are comma-separated. A test covering many behaviors may stack several `// spec:` lines rather than writing one long line.
- The same token is used on all surfaces. There is exactly one convention to learn and one marker string for the scanner to find (it still parses per-language, but it looks for the same token).

### Placement / attribution

Two canonical placements, covering the two granularities:

- **Function level** — the marker sits on the line(s) immediately preceding a test declaration, separated from it only by blank lines or other comment lines: a `func TestXxx(t *testing.T)` in Go, or a `test(...)` / `it(...)` / `test.describe(...)` in Playwright/Vitest. It binds to that whole test.
- **Subtest level** — the marker is the first statement line(s) inside a `t.Run("name", func(t *testing.T) { ... })` body. It binds to that subtest.

The rule in one sentence: **put the citation next to the assertions that prove the behavior.** Both levels are allowed because this repo's subtests are frequently the real behavior boundary (a `CreateWithInvalidDirection_Returns400` subtest maps to one specific `api` behavior), so subtest-level citations keep the mapping precise. Attributing a comment to its enclosing `t.Run` is a one-time cost in the Piece 3 scanner, not an author burden.

### Granularity and cardinality

- **Behavior-granular only.** Citations name behavior IDs (`CON-001`), never then-item positions — the SSOT has no sub-IDs, and coverage is defined at behavior granularity.
- **Free N:M.** One test/subtest may cite several behaviors; one behavior may be cited by many tests. No uniqueness constraint. Piece 3 reports orphans (behaviors with no citation) and dead IDs (citations naming no behavior); it does not require 1:1.

### Worked examples

```go
// Go — function-level (backend/tests/api/rematch_test.go)
// spec: IMP-019, IMP-020
func TestRematchAPI_TriggersFanout(t *testing.T) {
    // Go — subtest-level
    t.Run("enqueues one job per stale candidate", func(t *testing.T) {
        // spec: IMP-021
        ...
    })
}
```

```ts
// Playwright — function-level (frontend/tests/e2e/overdue.spec.ts)
// spec: DSH-005
test('overdue list refreshes after logging an interaction', async ({ page }) => {
    ...
})
```

## What gets cited, and the coverage strategy

### Cite-on-write

Every test Piece 2 authors or relaxes carries citations. Piece 2 does **not** retrofit the existing 334-file suite (280 Go + 29 Vitest + 25 Playwright). Those behaviors will read as orphans in Piece 3's report, and that report then drives an incremental backfill (likely an agent-driven mechanical sweep) as its own effort. This keeps Piece 2 focused on the high-value gap and avoids a large, low-signal annotation slog blocking the handler work.

Going forward, cite-on-write becomes the soft norm for all new/changed tests (a natural extension of the spec maintenance rule); Piece 3 later makes it enforceable.

### The two producers of new citations

- **New API-level handler tests** for the ~5 uncovered Gin handlers (rematch, sync, calendar, todoist, search — the ~13-of-18 handlers lacking dedicated API tests, narrowed to the highest-value five). Each cites the `api` / `business-logic` behaviors it asserts. This is the bulk of Track A and the reason the gap matters: the MCP server will exercise the same API surface.
- **Relaxed keeper E2E flows** — the handful of E2E flows that genuinely need the browser wired in (e.g. rematch job→poll→refresh, overdue cross-view consistency), rewritten to assert on API responses / network params / DB state via role-based drivers — never DOM layout or copy — each citing its `ux` behaviors.

### Discovery of spec gaps

Writing a handler's tests will occasionally surface a behavior that *should* exist as `api` but is not yet in the corpus. This is the "audit as a side effect" the umbrella design predicted. Handle it via the standing maintenance rule — extend `spec/<domain>.yaml` in the same PR (extend-in-place vs retire-and-mint per `spec/README.md`), run `make spec-lint` — not as a blocker.

### Rollout — vertical-slice-first

Do **rematch end-to-end first**: its handler tests, the `// spec:` citations into the IMP behaviors, and a check that the marker parses cleanly as expected. This validates the convention against real code before fanning out to sync / calendar / todoist / search. Playwright relaxation runs as a parallel track on its own cadence. Each handler (and each relaxed flow) is an independently shippable PR.

## Scanner-readiness contract

Piece 2 does not build the scanner, but pins the format so Piece 3 has a deterministic target:

- **Marker grammar.** After `//` and optional whitespace, a citation line begins with `spec:`, followed by one or more IDs separated by commas. Each ID matches the same `<PREFIX>-NNN` shape the spec linter already enforces (uppercase prefix, number growing past 999 without a leading zero). Text that does not match is not a citation.
- **Valid-ID source of truth.** The canonical set of behavior IDs is `spec.ParseDir("spec/")` → `[]Behavior` → their `.ID` values (the existing `backend/internal/spec` package, unchanged by Piece 2). A citation is *dead* iff its ID is not in that set; a behavior is an *orphan* iff no citation names it.
- **Attribution rule.** A marker binds to the test construct it immediately precedes (function level) or opens (first line inside a `t.Run` body). Piece 3 implements this as a Go AST walk (comment-position → enclosing/following node) and a Playwright/Vitest source grep.

No tooling ships in Piece 2 — no scanner, no runtime validator, no pre-push check. Citation *validity* is Piece 3's chartered job, and the cite-on-write volume keeps pre-Piece-3 rot negligible.

## Scope boundaries

- **Playwright relaxation is narrow, and defers to Piece 4.** Piece 2 relaxes + cites only the keeper flows that pair with the handler tests it writes. It does not attempt a full E2E purge or tour conversion: the umbrella design's assertion-free a11y-snapshot **tours are Piece 4 (Track B)**, layered *on top of these same relaxed flows* (double duty). Split: Piece 2 makes keeper flows data-asserting + cited; Piece 4 adds snapshot capture + model judge. Deleting a brittle pure-UI spec is fine when a relaxed flow supersedes it, but is not a mandate.
- **The MCP server is out of scope — it is a coupling, not a deliverable.** "Interleaves with the MCP-server work" means the handler tests harden the same API edge the MCP server will consume; the practical effect on Piece 2 is *ordering* (prioritize the endpoints the MCP server most relies on). Building the MCP server stays its own project.
- **Todoist proceeds now.** The "todoist" uncovered handler is the HTTP `TodoistHandler` (settings CRUD, projects/labels, sync trigger) — stable, and above the reconciler internals that the paused todoist-reconciler arc (PR2–4) churns. Those internals are covered by the arc's own op-worker tests, not by handler API tests. So todoist / sync / calendar handler tests proceed now, citing the stable handler behaviors. If a paused-arc PR later touches a cited behavior, it updates spec + tests in-PR per the maintenance rule.

## Explicitly out of scope

- The traceability scanner, orphan/dead-ID detection, behavior-changed-without-test-change detection — Piece 3.
- Any runtime citation validator or pre-push/CI coverage gate — Piece 3, once enough behaviors carry citations to be worth gating.
- Any `spec/*.yaml` schema change (no `coverage` field).
- Retrofitting citations onto the existing passing suite — cite-on-write now; Piece 3-driven backfill later.
- Assertion-free tours, accessibility-tree capture, network-recording, the model judge — Piece 4 / Track B.
- Building the MCP server.

## Deliverables

1. The `// spec:` convention documented for contributors — a short section in `spec/README.md` (traceability) and/or `.ai/rules/testing.md`, with the marker, placement, granularity, and an example on each surface.
2. New API-level handler test suites for rematch, sync, calendar, todoist, search — cited, rematch first as the validating vertical slice.
3. The keeper E2E flows relaxed to data/network/DB assertions and cited.
4. Any spec gaps discovered during (2)/(3) closed in-PR per the maintenance rule.

## Success criteria

- The convention is documented and applied uniformly across at least the rematch slice on both a Go handler test and a relaxed Playwright flow.
- All five handler suites exist and cite behavior IDs; the keeper E2E flows are relaxed and cited.
- The format is confirmed machine-parseable against `backend/internal/spec` (a marker's IDs resolve to real behaviors) — validated by inspection/one-off grep, since the enforcing scanner is Piece 3.
- No `spec/*.yaml` schema change; no scanner or CI gate introduced.

## Open items (for planning, not blocking)

- Exact home + wording of the contributor-facing convention doc (`spec/README.md` vs `.ai/rules/testing.md` vs both).
- The precise five-handler priority order beyond "rematch first."
- Which keeper E2E flows make the relaxation cut (candidate set: rematch end-to-end, overdue cross-view; finalized during implementation planning).
