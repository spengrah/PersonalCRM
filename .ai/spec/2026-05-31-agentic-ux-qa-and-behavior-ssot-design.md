# Agentic UX QA + Behavior SSOT — Umbrella Design

**Date:** 2026-05-31
**Status:** Umbrella design (architecture-level). Detailed sub-specs deferred to their own brainstorms.
**Author:** spengrah (brainstormed with Claude)

## Problem

The deterministic Playwright E2E suite is brittle. ~70% of its assertions check literal copy, CSS classes, DOM structure, element counts, and ordering by visual position — exactly what changes when the UI is improved. This makes continuous UX improvement expensive: every restyle forces a test rewrite, even when the underlying behavior is unchanged. The current suite conflates two different jobs — *is the business logic correct?* and *has the UI not regressed?* — and the coupling makes both worse.

At the same time there is no automated judgement of *UX quality* itself. The suite can confirm a button exists; it cannot tell you the empty state is confusing or the flow feels broken.

## Goal

Rebalance the testing pyramid into two tracks with clean responsibilities:

1. **Deterministic tests own business-logic correctness** — stable, indifferent to how the UI looks. Re-oriented toward API-level / data-asserted verification. This also hardens the API surface for the **forthcoming MCP server**, which is just another API consumer.
2. **An agentic layer owns UX quality** — judging the experience against intended behavior in a way that *survives UI change by construction*, because it asserts on intent and observable outcomes rather than DOM specifics.

The connective tissue between both tracks is a **single source of truth (SSOT) of intended behavior**: durable, DOM-free intent specifications that deterministic tests, the QA agent, the MCP server, and documentation all reference.

## Non-Goals

- Pixel-perfect visual-regression testing. The oracle is *behavioral*, not visual. Vision is an optional escalation for genuinely visual defects, never the default path.
- Auto-fixing. The QA loop **detects and reports**; it does not open fix-PRs (see "Issues, not fix-PRs").
- Replacing the Go test suite. The Go suite is already a strong business-logic safety net (~465 test functions; ingest→match→interaction→cadence pipelines, imports, and matching covered end-to-end without a browser). This work leans on it and extends it at the HTTP/handler layer.
- Deleting all UI coverage. A thin layer of resilient, data-asserting E2E remains for flows that genuinely need the frontend wired in.

## Background: current-state findings

These observations motivated the design and should be re-verified before each sub-project begins (code changes).

- **Go suite is a strong logic net, weak at the HTTP edge.** End-to-end-without-browser coverage is strong for ingest pipelines (messages, phone calls, meeting notes, external contacts), contact matching/enrichment, cadence/followup state machines, import→contact flows, and mac-host auth. The gap: ~13 of 18 Gin handlers lack dedicated API-level tests (rematch, sync, calendar, todoist, search). That gap matters more now because the MCP server will exercise the same API surface.
- **Playwright suite is ~70% brittle.** ~21 specs, ~130 tests, ~5,763 LOC. Roughly 70% of assertions are UI-regression (literal text, `toHaveClass`, DOM structure, counts in labels); ~25% mixed; ~5% genuinely resilient (API/data/route assertions). Only ~8 test blocks verify business logic that nothing else covers (e.g. rematch end-to-end, overdue cross-view consistency, a few import state transitions).
- **Resilient infrastructure already exists to build on.** `frontend/tests/e2e/helpers/test-api.ts` (TestAPI seed/read helpers), backend test-only seed routes (`/seed/contacts`, `/seed/external-contacts`, `/seed/meeting-notes`, …) gated by `CRM_ENV`, `page.waitForResponse` patterns, role-based queries, `test-map.json` tag-based diff selection, and the `personal_crm_test` DB lifecycle.

## Architecture: two tracks, four pieces

```
                    ┌─────────────────────────────────┐
                    │   SSOT: durable intent specs     │
                    │   (spec/, DOM-free, IDed)        │  ← single source of truth
                    └─────────────────────────────────┘
                       ▲            ▲            ▲
        references     │            │            │   references
        ┌──────────────┘            │            └──────────────┐
        │                           │                           │
┌───────────────────┐   ┌───────────────────────┐   ┌──────────────────────┐
│ Track A:          │   │ Anti-drift:           │   │ Track B:             │
│ deterministic     │   │ traceability + CI     │   │ agentic UX QA harness│
│ tests (API-level/ │   │ coverage checks       │   │ (tours + judge →     │
│ data-asserted)    │   │ (behavior IDs ↔ tests)│   │  GitHub issues)      │
└───────────────────┘   └───────────────────────┘   └──────────────────────┘
```

The four implementable pieces:

1. **The SSOT artifact + derivation** (the foundation everything consumes).
2. **Track A — re-orient deterministic tests** toward API-level / data-asserted verification.
3. **Anti-drift — traceability + CI coverage checks** keeping specs and tests in sync.
4. **Track B — the agentic UX QA harness** (deterministic tours + model judge → GitHub issues).

### Piece 1 — The SSOT (behavior intent specs)

The oracle is a version-controlled corpus of **intended behaviors**, deliberately DOM-free so UI restyles never invalidate it.

- **Organization:** one file per **domain**, drawn from the existing test/handler grouping — `contacts`, `interactions`, `cadence-followup`, `imports-matching`, `ingest`, `calendar-gcal`, `telegram`, `todoist`, `mac-host`, `notes`, `dashboard`.
- **Per-behavior schema:** stable **ID** (e.g. `INT-003`); **type** (`business-logic` / `api` / `ux` / `invariant` / `data`) so each consumer filters to what it cares about; intent-level **Given/When/Then** (no selectors, no copy); **status** (`current` = faithfully describes today vs `proposed` = desired change); **coverage** (test references, the traceability seed); **provenance** (sources it was derived from).
- **File maturity states:** `draft` (derived) → `reviewed` (curated) → `ratified` (trusted). The new paradigm switches on **per domain** as each file matures, rather than all-or-nothing.
- **Location:** top-level **`spec/`** with a `spec/README.md` index — signals a first-class product artifact, not agent scratch.

Each behavior survives any UI redesign, tells a deterministic test exactly what to assert, tells the QA agent what outcome to look for, and reads as documentation. The same corpus later informs the MCP server's contract and user-facing docs.

**Build strategy:** derive draft behaviors from existing Go/E2E tests + handlers (agent drafts, human curates), then backfill domain-by-domain by priority. This grounds the SSOT in real current behavior, is the cheapest path, and audits existing coverage as a side effect.

### Piece 2 — Track A: re-orient deterministic tests

Shift business-logic verification off the DOM:

- **Lean on the Go suite** as the primary logic net; close the HTTP-edge gap by adding API-level tests for the handlers that lack them (rematch, sync, calendar, todoist, search). This doubles as MCP-server hardening.
- **Relax brittle Playwright** — strip `toHaveClass`, literal-copy, and DOM-structure assertions that only re-check, through the UI, what a backend test already covers.
- **Keep a thin data-asserting E2E layer** for the ~8 flows where the browser genuinely adds coverage (e.g. rematch end-to-end job→poll→refresh, overdue cross-view consistency). Rewrite these to assert on API responses / network params / DB state via TestAPI, using role-based drivers — never layout or copy.
- Lean (per the user) toward **API-level E2E and data-asserted** styles over DOM assertions.

This track and the SSOT are mutually reinforcing: specs say what to assert; tests cite the behavior IDs they cover.

### Piece 3 — Anti-drift: traceability + CI coverage checks

Keep the SSOT honest without hard-blocking unrelated work:

- Tests declare the **behavior IDs** they cover.
- CI flags **orphan behaviors** (a `current` behavior with no covering test) and **dead IDs** (a test referencing a removed behavior).
- A PR-review expectation that behavior changes update the relevant spec.

This gives measurable drift detection rather than relying on discipline (the classic way SSOTs rot), while avoiding the noise/gaming of a hard "touch a spec file or CI fails" gate.

### Piece 4 — Track B: the agentic UX QA harness

The harness splits the job into a part machines do deterministically and a part that needs judgement — which is what makes it both reliable and cheap.

- **Tours (deterministic, zero model quota):** assertion-free Playwright that walks each flow through its key states, seeding via the existing test routes, and **captures the accessibility tree (text, not pixels) plus recorded network responses** at each state. The tour navigates; it asserts nothing, so UI restyles don't break it. The tour *is* the relaxed E2E suite from Track A, doing double duty.
- **Judge (model, high-volume, cheap):** for each relevant `(behavior × captured state)` pair, one small model call reads `intent-spec + a11y-snapshot (+ network)` and returns `pass | fail | unsure`, with observed-vs-expected and a draft issue on fail. This is *criticism, not agency* — the model never decides where to click — which is why a cheap model is sufficient.
- **Reporter:** confirmed failures become **GitHub issues** (see below).
- **Optional escalation:** vision (screenshots) only for genuinely visual behaviors (overlap, broken layout, contrast). Not the default.
- **Optional add-on (deferred):** a sparse (e.g. weekly) free-form *agentic exploration* pass to hunt unknown-unknowns the scripted tours don't cover.

#### Issues, not fix-PRs

The loop opens **GitHub issues**, not fix-PRs. This is a deliberate modular seam: it decouples *detection* from *remediation*. Issues can be picked up manually, triaged, or later fed to a separate agentic remediation pipeline — your choice, wired up independently. It also avoids the worst failure mode of auto-fix bots: confidently-wrong PRs that cost more to review than they save.

## Execution & cost model

The hard constraint: **no metered Claude API pricing.** As of 2026-06-15, *any* programmatic Claude (`claude -p`, Agent SDK, GitHub Actions, third-party harnesses) leaves the subscription pool and bills at full API rates from a small non-rollover credit. Interactive terminal Claude is unaffected. This forks the project by brain:

- **Build/curate phase → interactive Claude (now, $0).** Deriving the SSOT, relaxing tests, and building the harness skeleton are human-in-the-loop work — exactly right while the specs are unproven.
- **Autonomous phase → `codex exec` ($0 at the margin, quota-bound).** Unlike Claude, `codex exec` runs under the **ChatGPT subscription**. Its cost is **quota** (token-based, rolling 5-hour + weekly windows) which is *shared with interactive coding* — not dollars. So the design minimizes quota burn.

**Why the tour+judge split is quota-frugal:** tours navigate at **zero model quota** (deterministic Playwright). Quota is spent only on judging (high-volume, but small per-call and routed to the cheapest capable model) and issue authoring (low-volume, higher model).

**Brain routing (all within one Codex subscription):**

| Step | Model / effort | Rationale |
|---|---|---|
| Tour navigation | none (deterministic) | 0 quota |
| Judge (high-volume) | **gpt-5.4-mini @ low** | ~30% of standard's quota → work lasts ~3.3× longer; judging is easy (read a11y tree, match one sentence, pass/fail). The ~4pt SWE-bench gap vs 5.5 is irrelevant for this task. |
| Issue authoring (low-volume) | **gpt-5.5 @ medium** | Reasoning quality matters when writing a human-worthy issue; rare enough to afford. |

Configured via named profiles in `config.toml` (or per-invocation `--model`/effort flags). *Note:* `gpt-5.3-codex` is the only model of the set supporting Codex **Cloud Tasks / Code Review** — keep documented as the option if the loop ever offloads to Codex Cloud instead of the VPS.

**Cadence:** nightly full sweep (~3am) + optional per-PR diff-scoped run. A 3am sweep almost never overlaps interactive coding or a dev-sandbox session, so quota contention is minimal in practice.

**Host:** prove the loop on the Pi under interactive Claude ($0), then graduate the autonomous `codex exec` loop to a **Hetzner CAX21** (4 vCPU / 8 GB ARM, ~€6.49/mo). The box doubles as general agent infra (dev-sandbox, experiments); since QA runs nightly and dev-sandbox runs daytime, they're temporally staggered and a small box suffices. Resize to **CAX31** (8 vCPU / 16 GB, ~€15.99/mo) on demand (Hetzner bills hourly with a monthly cap) if dev-sandbox sessions run hot. ARM matches the existing stack so images/scripts port cleanly from the Pi. Keep workloads in **separate containers** for hygiene — a hung 3am QA run shouldn't greet the morning's sandbox session with junk. The harness is **host-agnostic** ("a box with the repo, a seeded app, and Codex auth"), so the Pi-vs-VPS choice never leaks into its design.

## Sequencing

The SSOT is the foundation everything else consumes, so it goes first. Each piece below gets its own brainstorm → spec → plan → implementation cycle.

1. **SSOT** — artifact schema, derivation pipeline, first domains derived + curated to `reviewed`.
2. **Track A** — re-orient deterministic tests (API-level handler tests + Playwright relaxation), citing behavior IDs. Naturally interleaves with the MCP-server work.
3. **Anti-drift** — traceability + CI coverage checks, once enough behaviors carry coverage links to be worth enforcing.
4. **Track B** — the QA harness, proved on the Pi under interactive Claude, then graduated to `codex exec` on the VPS.

## Relationship to the seed + parallelization work (D, #413)

Added 2026-06-07 — the deploy/staging design surfaced concrete couplings between this work and two of its sub-projects:

- **The synthetic seed toolkit (D, `.ai/spec/2026-06-07-synthetic-seed-generator-design.md`) supersedes the seed substrate this spec builds on.** Background / Track A / Track B here reference `test-api.ts` + the `CRM_ENV`-gated `/seed/*` routes; D refactors those onto a deterministic, namespaced, library-first toolkit. → Track A's new tests and Track B's tours should build on **D's toolkit**, not the legacy routes directly. D's deterministic scenarios are the world Track B tours and Track A asserts against.
- **D's coverage-check and Piece 3 (anti-drift) are the same kind of mechanism.** D can index its scenarios by **SSOT behavior IDs** (a fixture per `ux` / `data` behavior), making "rich enough to tour/assert" concretely measurable and feeding Piece 3's traceability.
- **Test parallelization (#413, `.ai/spec/2026-06-07-test-parallelization-design.md`) parallelizes the post-rebalance suite.** Coordinate so E2E assertions aren't rewritten twice — Track A's Playwright relaxation lands before #413's E2E-scoping — and build Track A's new API-level tests parallelization-ready (D-backed scoping). #413 is gated on D's suite migration.
- **The VPS host this spec sketched is now sub-project B** (`.ai/spec/2026-06-07-vps-and-tailnet-isolation-design.md`); the harness graduates onto the box B provisions, and Track B tours target the staging instance (sub-project C, `.ai/spec/2026-06-07-staging-environment-design.md`).

## Piece 4 sub-spec + current-state corrections (added 2026-07-08)

Piece 4 is now designed: `.ai/spec/2026-07-08-piece4-track-b-agentic-qa-harness-design.md`. Discovery during that brainstorm corrected several assumptions above; the sub-spec supersedes this document on each point:

- **Staging exists** (#556 deploy plumbing, #566/#569 auto-reseed, #570 host provisioning) — tours target staging from day one; the "prove on Pi → graduate to VPS" host arc is obsolete. The prove phase runs from the dev sandbox via an ops network exception; #477 is superseded by the shipped work.
- **Tours are a separate assertion-free suite**, not the relaxed E2E suite doing double duty — decoupling Piece 4 from Piece 2's Playwright-relaxation work (Piece 2 has since completed as #596–#604). Piece 4 proceeds before Piece 3 (it depends only on the SSOT, and tour annotations become a coverage producer for Piece 3's scanner to index).
- **Judge invoker is `@openai/codex-sdk`** (with `codex exec --json --output-schema` as degraded mode) behind an adapter seam that keeps the brain swappable; the economics and model-routing table above still hold.
- **The judge ships with an eval harness** (golden corpus incl. doctored captures, fail-precision-gated advisory→issue-mode transition) — the "prove the judge loop with a human in the loop" mitigation above, made mechanical.
- **Instrumentation is shared tooling with #379** (OTel GenAI spans, common eval-runner pattern, future self-hosted observability platform) — see the sub-spec's Instrumentation section.

## Key risks

- **Spec quality is unproven.** The whole edifice rests on the intent specs being good oracles. Mitigation: prove the judge loop against real specs with a human in the loop ($0 interactive Claude) *before* investing in autonomous infra.
- **Judge false positives/negatives.** A noisy judge erodes trust fast. Mitigation: `unsure` as a first-class verdict; start advisory; tune on a representative sweep before acting on issues automatically.
- **Codex quota contention.** Even nightly, a large sweep could dent the coding bucket. Mitigation: mini@low judging, diff-scoping per-PR runs, deterministic (free) navigation, and measuring real burn before scaling cadence.
- **SSOT drift.** Specs rot if maintenance relies on discipline. Mitigation: Piece 3's mechanical coverage checks.
- **Derivation describes bugs as intent.** Deriving from current tests can codify present behavior as "intended." Mitigation: human curation gate (`draft`→`reviewed`), and the `current` vs `proposed` status to separate "is" from "ought."

## Decisions locked (this brainstorm)

- Two tracks: deterministic tests own business-logic correctness (API-level/data-asserted, also serving the MCP server); agentic layer owns UX quality.
- Oracle = durable, DOM-free intent specs (the SSOT), one file per domain, behaviors with stable IDs + types + maturity states, in `spec/`.
- Build the SSOT by deriving from existing tests/handlers → curate → backfill by priority.
- Anti-drift via traceability + CI coverage checks (behavior IDs ↔ tests; flag orphans/dead IDs).
- QA harness = assertion-free Playwright tours (0 model quota) capturing a11y snapshots + network → model judge → **GitHub issues** (not fix-PRs).
- Brain: interactive Claude while proving it ($0); autonomous loop on `codex exec` under the ChatGPT subscription — gpt-5.4-mini@low judges, gpt-5.5@medium authors issues; agentic-explore deferred.
- Cadence: nightly full sweep + optional per-PR diff-scoped run.
- Host: prove on the Pi → graduate to Hetzner CAX21 (resize to CAX31 on demand); shared agent infra; separate containers; host-agnostic design.
- Deliverable of this brainstorm: this umbrella doc only. Detailed sub-specs deferred to their own brainstorms.
