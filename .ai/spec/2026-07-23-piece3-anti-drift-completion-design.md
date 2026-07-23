# Piece 3 completion — `api`-surface coverage + behavior-drift hook

Status: DESIGN (2026-07-23). Owner: maintainer. Parent: #380 (Piece 3 · anti-drift). Depends on: the E2E-settlement arc (#667–#678), which shipped the traceability scanner this design extends. Design-reviewed by a Fable pass (2026-07-23); its refinements are folded in below.

## Why now — Piece 3 is mostly already shipped

The umbrella (#380) charters Piece 3 as "anti-drift — traceability + CI coverage checks: behavior IDs ↔ tests; flag orphan behaviors and dead IDs." The headline deliverable already exists, landed opportunistically inside the E2E-settlement arc (#667–#678, filed under #380 as "Piece 3 + residual Track A"):

- `make spec-coverage` (`backend/cmd/spec-coverage` + `backend/internal/spec/coverage.go`) — the traceability scanner: extracts `// spec:` citations from the deterministic test surfaces (backend `*_test.go`, `frontend/tests/e2e/*.spec.ts`), validates them against the corpus, and derives per-then-item coverage.
- Dead IDs, out-of-range indexes, malformed/inline markers, and citations of intent / proposed / retired behaviors → hard failures (rotted references).
- `ui`-surface orphan then-items → warn, or **block** when the domain's spec file declares `e2e_settled: true`.
- Stale-waiver warnings; E2E-cites-non-ui warnings.
- Wired into CI (`.github/workflows/ci.yml` spec-lint job) and pre-push (`.ai/pre-push.json`).
- All 12 domains are `e2e_settled: true`; the scanner is green today.

So "flag orphan behaviors and dead IDs" is **done for the `ui` surface.** This design closes the two genuine remainders.

## The gap

1. **Coverage detection stops at `ui`.** The scanner counts `api`- and `none`-surface behaviors but never flags one that no test cites — orphan detection runs only for `surface: ui` (via E2E citations). Go-test `// spec:` markers are collected and checked for dead IDs, but **never credited as coverage.** An `api`-surface behavior can therefore rot with zero covering test, invisibly. There are ~62 `api` behaviors — the surface the forthcoming MCP server consumes, so the one most worth hardening.

2. **No behavior-changed-without-test-change signal.** The classic silent-drift case — a PR edits what a behavior asserts but touches none of its covering tests — is explicitly parked as future work (`spec/README.md` line ~153; `2026-07-01-behavior-ssot-design.md` line ~76). Nothing catches it today.

## Scope decision: `api` now, `none` later

`api` only. `none`-surface behaviors (backend-internal invariants owned by unit/integration tests) are the higher-stakes correctness surface, but orphan-checking them now has low value-per-effort:

- The `none` surface is covered by ~280 mostly-**uncited** Go unit/integration tests (only ~19 Go `// spec:` citations exist tree-wide today). Enabling `none` orphan detection means either a ~275-item noise list or a large low-signal citation backfill before any domain can flip to block — exactly the retrofit the SSOT design deferred ("cite-on-write now; Piece 3-driven backfill later").
- `none` coverage mapping is fuzzier: a `none` invariant often maps to a *cluster* of integration tests with ambiguous ownership, versus a clean one-contract-test-per-`api`-behavior mapping.

`api` is ~62 behaviors, a clean citation target, and has an active downstream consumer (MCP). The mechanism `none` needs later is *identical in shape* to what A builds — Go-citation credit keyed on `Citation.E2E == false`, plus a settled entry and a backfill — so enabling `none` is a bounded follow-up, not a scanner redesign: ship the small `none`-orphan **emission** code (this piece emits only ui/api coverage items), drop the `settled` `none`-rejection (§A amendment below), and backfill citations. It is **not** a data-only change — the emission code must land before any `none` settlement can mean anything (2026-07-23 consistency fix; earlier drafts said "data change" here, contradicting the amendment). The fuzzy cluster-ownership problem is a backfill-effort problem (which tests get markers), not a scanner-design problem; the covered/waived/orphan item model is unchanged.

## A — `api`-surface orphan/coverage detection

**Coverage credit is keyed on the behavior's `surface` tag, deliberately.** `surface` is defined as "which deterministic-test layer *owns proving* the behavior" (`spec/README.md`), and enforcement gates on that ownership:

- An **E2E** citation (`Citation.E2E == true`) credits a `ui`-surface behavior (unchanged).
- A **Go-test** citation (`Citation.E2E == false`, already collected but currently unused for coverage) credits an `api`-surface behavior. Bare (`ID`) and indexed (`ID[n]`) Go citations behave exactly as their E2E counterparts do for `ui`, including the statement-behavior case: an `api` invariant is coverable only by a bare Go cite and waivable at index 0 (the Go-cite equivalent of today's `e2eBare` map feeds the same `itemState` path).
- An **E2E citation of an `api`** behavior keeps its existing "should this be `surface: ui`?" warning and grants **no** api coverage; a **Go citation of a `ui`** behavior is defense-in-depth and grants no ui coverage (the ui pressure valve for a genuinely browser-unprovable item is a waiver, not a Go cite).

**Stated asymmetry (do not "fix" it):** there is an E2E-cites-non-ui warning but there is deliberately **no** Go-cites-non-api warning. Go tests legitimately cite `ui` and `none` behaviors all over the tree today (`MAC-018`, `IMP-007`, `CAD-039`, `CON-023`, …), and the `none` cites are precisely the future `none`-coverage corpus — a symmetric warning would fire on every one of them. This asymmetry is correct and intentional; record it so a later implementer does not add the mirror check.

**Orphan emission.** `ComputeCoverage` emits `ItemCoverage` for `api`-surface, `status: current` behaviors, not just `ui`. Each item carries its surface so the two orphan populations are counted and gated independently. `DomainCoverage` exposes per-surface orphan counts.

**Enforcement — a per-surface `settled` list (replaces the `e2e_settled` boolean).** The domain-settlement flag is generalized from one boolean to a per-surface list: `settled: [ui]`, `settled: [ui, api]`, etc. — the set of surfaces whose orphans **block** rather than warn. Rationale (Fable): the design already knows the axis is *surface* (it names the future `none_settled`), so three booleans (`e2e_settled` names a harness, `api_settled`/`none_settled` name surfaces — incoherent) is the wrong primitive; a list keyed on the surface enum that keys coverage credit means one linter rule and a blocked-computation that loops over surfaces instead of growing a boolean expression.

**`settled ⊆ {ui, api}` today; `none` is RESERVED (2026-07-23 amendment).** `none`-orphan *emission* is not built in this piece (only ui/api coverage items are emitted), so a `settled: [none]` would be an inert, silent no-op. Fail-closed, the linter therefore **rejects `none` in `settled`** with a forward-looking message ("not yet supported — none-orphan detection is future work") rather than accepting a currently-inert settlement. So the membership rule is "every element ∈ {ui, api}", not `{ui, api, none}`; the `validSurface` enum still governs the `surface` *tag* as `{ui, api, none}` — only the `settled` list is narrowed. Duplicate `settled` entries are also rejected (canonical, one representation per set). Enabling `none` later = ship the emission code + drop this rejection + the citation backfill — a bounded follow-up, not a redesign.

- Schema: `File.Settled` becomes a surface set (replacing `E2ESettled bool`); the hand-walked parser reads a `settled` sequence (a scalar is rejected — fail-closed, not normalized); `validate.go` checks membership against `{ui, api}` (see the amendment above).
- Migration: the 12 existing `e2e_settled: true` lines become `settled: [ui]` **in this PR**. No legacy alias — the linter makes stragglers impossible, and an alias is cruft.
- Block rule in `cmd/spec-coverage/main.go`: for each domain, for each surface `s` in `d.Settled`, block if that surface's orphan count > 0. Default (surface absent from the list) = warn.
- Every domain ships with `settled: [ui]` (its current ui-blocking state preserved). No domain adds `api` to the list in this piece → **api orphans warn, never block** until a domain is backfilled and flipped. This design lands green and non-disruptive; the backfill + per-domain `api` additions are explicit follow-up (mirroring the ui backfill/settlement children of #667–#678).

**Waivers.** Today waivers are legal only on `ui`-surface behaviors. A extends waiver legality to `api`-surface behaviors so an api then-item that no deterministic test can prove can be waived (with a reason) exactly like a ui one — otherwise a genuinely-unprovable api item could never leave the orphan list and would permanently block an `api` settlement flip. `validate.go`'s `checkWaivers` placement rule changes from "surface must be `ui`" to "surface must be `ui` or `api`" (still never `none`, never intents). `itemState`'s stale-waiver logic generalizes for free.

**Report.** The per-domain summary line adds api figures alongside ui, and the settled-state annotation becomes per-surface (e.g. `settled: ui` vs `settled: ui,api`). The exact new summary-line format is pinned in Testing below because pre-push log-scrapers (agents) and `main_test.go` assert on it.

## B — behavior-changed-without-test-change (drift hook)

**New CLI `backend/cmd/spec-drift <repo-root> <base-ref>`,** a thin edge over a **pure** core function in `internal/spec`:

```
SpecDrift(base, head []*File, cites []Citation, changedFiles map[string]bool) []Violation
```

The CLI's only jobs are materializing the base corpus, computing the changed-file set, normalizing paths, and printing. The comparison logic lives in the pure function and is table-driven with in-memory fixtures — the same seam `ComputeCoverage` already uses (pure over `[]*File` + `[]Citation`, thin CLI wrapper). This keeps every interesting drift case out of git scaffolding.

**Algorithm:**

1. **Materialize the base corpus.** `git archive <base-ref> spec/ | tar -x` into a scratch dir (or per-file `git show`), then run the existing `spec.Lint` on it — reusing the lint-validity gate for free and adding no parse-from-bytes API (none exists; `Lint(dir)` is directory-based). Parse `HEAD` = the pushed commit tree (**not** the working tree — uncommitted scratch is not what drifts).
2. **Compute changed-content IDs.** Diff parsed behaviors by ID; flag IDs whose **assertable content** changed — `given` / `when` / `then` / `statement` (order-sensitive, so a then-item reorder counts — this partially closes the reorder hole `spec/README.md` documents the coverage scanner cannot see). Title / notes / provenance / type / status / surface / serves / waivers changes do **not** count. A newly-added ID does not warn (nothing to drift from); a removed/renamed ID is the coverage scanner's dead-ID job.
3. **Changed-file set.** `git diff --name-only <base-ref>...HEAD`, paths normalized to match `CollectCitations`' path rooting (see the path-normalization note below — this is a fail-open trap if skipped).
4. **Warn.** For each changed-content ID, resolve its citations via `CollectCitations` on the HEAD tree. If the ID has ≥1 citation but **none** of its citing files is in the changed set → **warn**: "behavior `X`'s assertions changed but none of its N citing test(s) were touched (`file:line`, …)". An ID with **zero** citations is A's orphan job, not B's — no warn here (clean partition, no double-report). Coarseness accepted: "citing *file* changed" is satisfied by any edit to that file, a false-negative not worth test-function-level attribution for an advisory check.

**Exit contract.** Exit **0** whether or not drift warnings were emitted (a reworded-but-unchanged-meaning then-clause is legitimate, so B never blocks). Exit **2** on operational error — unresolvable base ref, unreadable tree, or a git failure — mirroring `spec-coverage`'s operational-error contract. "Always exit 0" must NOT swallow git failures: a shallow checkout with no merge-base that silently computes nothing and exits green is a gate that cannot fail — the exact anti-pattern the repo's CLAUDE.md rule warns about.

**Unclean base corpus** (mid-refactor stack, or a base predating a schema change): treat base behaviors that don't lint as **absent** (⇒ their HEAD counterparts read as newly-added, no warn) and print a notice. Do **not** exit 2 for it, or stacked-PR workflows get spurious failures.

**Visibility.** Under GitHub Actions, emit workflow warning commands (`::warning file=spec/<domain>.yaml,line=N::…`) so drift surfaces as PR annotations in the checks UI and Files-Changed view — a plain warning inside a green CI log is read by no one, which would just relocate the silence B exists to break. In pre-push the plain WARN line is sufficient (the repo's agent workflow surfaces pre-push WARN lines by convention). The `Violation` already carries `Path`/`Line`.

## Testing

- **`internal/spec` unit tests** (table-driven, extending `coverage_test.go`): Go citation credited to an api behavior; api orphan emitted; ui and api orphan populations counted and gated separately; api waiver accepted, `none` waiver still rejected; E2E-cite-of-api still warns and grants no api coverage; Go-cite-of-ui grants no ui coverage and emits no warning; api statement-behavior covered by a bare Go cite / waived at index 0.
- **`SpecDrift` unit tests** (table-driven, in-memory `[]*File`): then changed + no citing file in changed set → warn; then changed + a citing file changed → silent; title-only edit → silent; then-item reorder + no citing change → warn; newly-added ID → silent; zero-citation changed behavior → silent (A's job); base-behavior-absent → silent. No git in these.
- **CLI plumbing tests** (`cmd/spec-coverage/main_test.go`, `cmd/spec-drift/main_test.go`): exit 1 when a domain's `settled` includes `api` and an api behavior is uncited; exit 0 (warn) when `api` is absent from `settled` with the same orphan; existing ui exit-code cases unchanged after the `e2e_settled`→`settled:[ui]` migration; drift exit 0 with/without warnings, exit 2 on an unresolvable base ref. **Exactly one** thin temp-repo fixture test drives spec-drift's real git + path-normalization path end-to-end (the path-normalization code must run for real, not be hand-fed matching paths — else the fail-open trap is untested).
- **Prove the gates fail on bad input** (CLAUDE.md rule): inject an uncited api behavior under a domain with `settled: [ui, api]`, confirm exit 1 directly (`cmd; echo $?`); inject a drifted-behavior fixture through the real path normalization, confirm the warn line appears. A gate that cannot fail is indistinguishable from one that always passes.
- **Report-string assertions** in `main_test.go` that pin today's ui summary substrings are rewritten to the new per-surface format; the new format is named here so agents' log-scrapers have a stable target.

## Path normalization (the fail-open trap, called out explicitly)

`CollectCitations` returns paths rooted at the CLI's `<repo-root>` argument (absolute — the Makefile passes `$(shell git rev-parse --show-toplevel)`), while `git diff --name-only` emits repo-relative paths. Without a `filepath.Rel` normalization the "citing file changed" set never intersects the changed-file set, and B silently finds no drift **forever** — fail-open, invisible. The normalization is part of the design, and the one temp-repo fixture test must exercise it.

## Rollout

- Ships green: every domain migrates to `settled: [ui]` (identical ui-blocking behavior), no domain adds `api`, so `make spec-coverage` stays exit-0 in CI. `spec-drift` is warn-only. No behavior's `given/when/then/statement` changes.
- Wiring: `make spec-drift` target; a step in the existing CI spec-lint job (base = merge-base with `develop`) **with `fetch-depth: 0`** (or a targeted fetch) on that job — `actions/checkout` defaults to depth 1 and `git merge-base` would otherwise fail; a new-branch push (remote SHA all-zeros) falls back to merge-base with `develop`. Pre-push entry in `.ai/pre-push.json` (warn lane). `settled` (and the retired `e2e_settled`) documented in `spec/README.md`.
- Follow-up (separate issue, not this piece): backfill `api` citations onto the handler/API tests domain-by-domain and add `api` to each domain's `settled` list — the api analogue of the E2E-settlement children.

## Non-goals

- `none`-surface orphan detection or its citation backfill (enabling it later = the small emission code + dropping the `settled` `none`-rejection + the citation backfill — a bounded follow-up per §A's 2026-07-23 amendment, not a data-only change).
- The `api`-citation backfill / per-domain `api` settlement flips (follow-up issue).
- A blocking drift check, or any waiver mechanism for drift.
- New spec *types* or SSOT schema changes beyond the `e2e_settled`→per-surface `settled` migration and the api-waiver placement relaxation.
- Any change to the Track B judge, tours, or the ui coverage semantics that already ship.

## Issue hygiene

Piece 3 currently has **no** sub-issue under #380 (the checkbox is unchecked with no link). This work files that issue and closes it on merge; #380's Piece 3 line is ticked with the closing PR(s).
