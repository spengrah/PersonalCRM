# Piece 4 · Track B — Agentic UX QA Harness — Design

**Date:** 2026-07-08
**Status:** Designed; implementation not started
> **Superseded on the seed profile (2026-07-30, gh #759).** The `dev` and `prod-shaped` catalog profiles — and the whole invented-distribution layer behind them (bands, quotas, archetypes, margins) — are deleted. There are now exactly two worlds: the declared `standard` world (the default for local dev, staging, the automated staging reseed, and the QA tours) and `minimal-scoped` (an explicit operator override). Historical measurements below were taken against the world that existed at the time and are left as recorded; operational commands and provenance assumptions have been updated to `standard` / `synth-standard-`. See `.ai/patterns/synthetic-seed-toolkit.md` for the current story.

**Author:** spengrah (brainstormed with Claude)
**Parent:** #380 umbrella (`.ai/spec/2026-05-31-agentic-ux-qa-and-behavior-ssot-design.md`), Piece 4

## Problem / Goal

The umbrella design split UX quality assurance into a deterministic part (tours) and a judgement part (a model judge), connected by the behavior SSOT. This sub-spec makes Piece 4 concrete: assertion-free Playwright tours capture accessibility-tree snapshots plus recorded API responses at annotated points; a cheap LLM judge evaluates each captured state against `type: ux` behaviors from `spec/*.yaml`; confirmed failures become GitHub issues (never fix-PRs). The harness must be trustworthy before it is autonomous — a noisy judge erodes trust faster than no judge — so the design centers an eval-driven prove/improvement cycle for the judge itself.

## Current state this design builds on

- **SSOT is ready.** All 12 domains at `maturity: reviewed` (consumers may act), ~394 behaviors, of which 56 are `type: ux` — Track B's targets. The schema was written for this consumer: `then` items are independently checkable facts the judge verifies item-by-item.
- **Staging shipped** (#556 deploy plumbing, #566/#569 auto-reseed, #570 host provisioning) — the umbrella's "no target to tour" gap is closed. `scripts/staging-reset.sh` explicitly documents this harness as its programmatic caller: stop → `crm-admin --reset-and-seed` off the pinned image digest → start.
- **Seed substrate is ready.** The synthetic toolkit (spec D) provides the deterministic, production-shaped world tours walk through.
- **Piece 2 completed while this design was in review (#596–#604); Piece 3 is unstarted — neither blocks this work.** Piece 4 depends only on the SSOT. Structurally, Piece 3 consumes coverage links that Pieces 2 and 4 produce, so building Piece 4 first gives Piece 3's scanner a second producer to index.

## Decisions (this brainstorm)

- **Separate tour suite**, not the umbrella's "tours = relaxed E2E doing double duty." A dedicated assertion-free suite decouples the tour suite from the E2E suite's lifecycle (Piece 2's relaxation was still in flight at design time) and gives capture its own design space; navigation-code duplication is mitigated by reusing E2E helpers.
- **Explicit behavior annotations.** Each capture declares the behavior IDs it exercises, mirroring Track A's `// spec:` citation convention. Authoring a tour requires reading the domain's spec file — which is the point.
- **Staging is the primary target from day one**; the harness stays host-agnostic ("a seeded app instance at a URL") so any host can run it.
- **Prove phase runs from the dev sandbox**, which requires un-deferring the sandbox→staging network path (ops prerequisite, see Environment).
- **First cut: contacts + dashboard + cadence-followup** (~23 ux behaviors) — richest purely-web surfaces, fully tourable against the seeded world, no external-service entanglement.
- **Eval-driven prove cycle**: the judge ships with its own eval harness and golden corpus; issue-filing turns on only when eval metrics clear a bar.
- **Judge invoker: `@openai/codex-sdk`** (TypeScript, subscription-covered) primary, `codex exec --json --output-schema` as degraded mode; an OpenAI-compatible adapter seam keeps the brain swappable.
- **Instrumentation is designed as shared tooling with the LLM extraction program (#379)**: same span conventions, same eval-runner pattern, same (future) self-hosted observability platform. Consistency lives in the instrumentation layer, not the brain.

## Architecture

```
staging-reset ──► tours (Playwright, 0 model quota) ──► run dir (captures + manifest)
                                                             │
                                     judge (codex SDK, cheap model) ──► verdicts.jsonl
                                                             │
                                     reporter (dedup + issue author) ──► GitHub issues
```

Five components, decoupled at file-artifact seams so each can be swapped or replayed independently. The seams are the design's load-bearing property: because captures and verdicts are plain text artifacts on disk, the judge is evaluable offline (no staging, no browser) and every run is replayable.

### Tour suite

`frontend/tests/tours/*.tour.ts`, a separate Playwright project (`tours`) in `playwright.config.ts`. Assertion-free: a tour navigates a domain's surfaces and performs actions, calling `capture()` at annotated points. It deliberately drives each ux behavior's `when` (captures bracket the action where the behavior needs before/after evidence). Targets `TOURS_BASE_URL`; reuses E2E helpers where useful but has no dependency on the E2E spec files. UI restyles cannot break a tour that asserts nothing — navigation breakage (a renamed route, a removed button) surfaces as a tour error, which is itself signal.

### Capture contract

`capture(page, { behaviors: ['CON-012'], note: 'after deleting contact A' })` records, per capture:

- the page's **aria snapshot** (Playwright `ariaSnapshot()` — text, not pixels),
- the **`/api/v1` network responses** since the previous capture (context-level response listener),
- current URL/route state, and the authored note.

Captures are **normalized deterministically** before storage: volatile noise stripped, repeated nodes/responses capped. This is a judge-bias mitigation (verbosity/salience bias — more text must not read as stronger evidence) as much as a diff-stability measure. Each run directory carries a manifest: git SHA, staging image digest, seed profile, capture-generator version, timestamp. The judge's framing follows from the contract: **network responses are ground truth; the aria snapshot is what the user sees**; the judge checks that the surface faithfully expresses the data per the behavior's intent.

### Behavior pairing and coverage

After each run the harness diffs captured behavior IDs against the scoped domains' `type: ux, status: current` behaviors and lists the **untoured remainder** in the run report. This is harness-local observability — never a CI gate, never a repo-wide scanner; Piece 3 later absorbs tour annotations as one more coverage source. `status: proposed` behaviors are skipped (they describe what does not hold yet). Genuinely untourable behaviors go on an explicit skip-list with reasons, not silence.

### Judge

**Invoker.** All ChatGPT-subscription inference routes through the Codex agent runtime (there is no subscription path to the raw API). Primary invoker is **`@openai/codex-sdk`** (TypeScript, programmatic threads, typed event stream); degraded mode is `codex exec --json --output-schema <file> --ephemeral` — schema-constrained verdicts and a JSONL event stream from the CLI directly. The harness defines a narrow adapter interface (`judge(input) → verdict`); codex-SDK, codex-exec, and OpenAI-compatible-HTTP (Venice or a metered key) are interchangeable implementations. The adapter is the instrumentation point (see Instrumentation) and the policy hedge: if subscription-programmatic access is ever re-priced, the brain swaps by config with zero harness changes. Model routing per the umbrella (cheap model judges, stronger model authors issues; named profiles, exact models revalidated at build time). Judge runs use a read-only sandbox and no tools — the model is criticism, not agency.

**Prompt protocol.** Evidence is presented in stable labeled blocks (spec / aria snapshot / API responses) to mitigate position bias. The judge verdicts **each `then` item separately** (`pass | fail | unsure`) and aggregates: any item fail → behavior fail. Verdicts are categorical — no numeric scores, no Likert. **Grounding rule: a `fail` must cite the exact aria-tree node label or JSON path that contradicts the item; no citation ⇒ the verdict is downgraded to `unsure`.** `unsure` is abstention, not a middle grade: unsures route to human review and are never issue-eligible. Few-shot examples are 4–8 promoted human critiques (see evals), order pinned, with a shuffled-shot smoke check before prompt changes.

### Judge evals (the prove/improvement cycle)

The judge is treated as a small owned evaluation product; prompts and rubrics are versioned in-repo, and evals are to the judge what tests are to code. Derived from a sourced survey of LLM-as-judge practice (BenchFlow PATTERNS.md, Hamel Husain's judge/eval guides, Eugene Yan's evaluator-bias survey, AgentRewardBench; working memo in local scratch).

- **Golden corpus** — labeled cases of `(behavior, captures, expected per-item verdicts, one-line critique)`. Solo-maintainer labeling (critique-shadowing); no committee, no LLM-generated ground truth. Sources: real sweep captures labeled by hand, plus **doctored captures** — deterministic mutations of real ones (remove an affordance from the aria snapshot, alter a JSON field, stale response, aria↔API mismatch) manufacturing known-fail cases, each human-validated.
- **v0 sizing (23 first-cut behaviors):** ~70–100 cases — one clean pass per behavior, 1–2 doctored fails per behavior, plus 20–30 targeted ambiguous/empty-state/collateral cases. Mix ≈ 45% pass / 40% fail / 15% unsure. Split ~70/30 dev/held-out, stratified, **frozen once**; prompt tuning never touches held-out labels.
- **Metrics** — confusion matrix; per-verdict precision/recall; **fail-precision is the north star** (a false fail costs more than a false pass); abstention rate tracked as a calibration signal.
- **Gating** — `make qa-eval` runs the judge over the corpus; any prompt/rubric/model change gates on: held-out fail-precision no-regression, zero regression on previously-confirmed-fail cases, no new issue-eligible false positives. Runs tagged with model, prompt hash, corpus version, git SHA, capture-generator version.
- **Regression rule** — every human-confirmed judge error (false fail, false pass, bad unsure) becomes a new labeled case before the prompt changes again.

### Reporter

**Advisory mode first**: verdicts roll up into a human-readable run report; every fail is human-reviewed; no issues are filed. **Issue mode** turns on when the eval bar is met: held-out fail-precision above a threshold set after the first labeling pass, plus N consecutive live sweeps where every fail was human-confirmed (default N=3; both parameters set when the corpus exists, recorded in the eval config). In issue mode, confirmed fails become GitHub issues labeled `ux-qa`, deduped by a fingerprint (behavior ID + normalized failure signature) embedded as an HTML comment in the issue body and checked via search before filing. A recurring fail on an already-open issue files nothing; the reporter never auto-closes. Issue bodies carry the behavior GWT, observed-vs-expected with the judge's cited evidence, and run metadata. Captures contain only synthetic staging data, so issues stay PII-clean by construction; the issue template states this as a guard.

## Instrumentation & tooling (shared with #379)

The LLM extraction program (#379) and this harness are structurally twins — both need evals, tracing, and dataset/labeling workflow — and their brains differ **by design** (judge = codex under subscription economics; extractors = Venice under privacy requirements). Consistency therefore lives in the instrumentation layer:

- **Tier 1 (in this design's critical path, no new infra):** git-diffable JSONL run artifacts + manifest + markdown report; Playwright traces for tour debugging; the judge adapter emits **OTel GenAI-convention spans** fed from the codex SDK/`--json` event stream. Field naming follows the OTel GenAI semantic conventions so any OTLP backend can ingest runs later without rework.
- **Tier 2 (decided by a spike in PR2): promptfoo as the eval runner** — MIT, local, git-diffable YAML cases, `exec:`/HTTP providers, JSON output post-processed into the confusion matrix and gates. Acceptance criteria: (a) fits the per-`then`-item verdict schema without contortion; (b) plausibly serves #379 extractor evals too (Venice is directly reachable as an OpenAI-compatible HTTP provider). Fallback: a ~200-line bun runner sharing the same corpus/artifact conventions so #379 inherits them either way.
- **Tier 3 (separate follow-up issue, after the harness works end-to-end): self-hosted Langfuse** (or Phoenix if stack weight wins) on the VPS as shared #379/#380 infra — OTLP trace ingest, datasets, annotation queues (the critique-shadowing labeling workflow), and prompt versioning (which matters more for #379's in-product extractor prompts than for the judge). **Self-hosting is a hard requirement, not an ethos preference**: extraction traces will contain real message content, and a hosted trace platform is a logging endpoint — excluded by #379's no-logging/no-retention principle. This also excludes Braintrust/Weave/Raindrop-cloud as shared tooling.
- **Deferred option — OpenAI-compatible HTTP shim** over the codex SDK (local `/v1/chat/completions`, read-only sandbox, no tools), which would make every SDK-shaped tool (promptfoo's openai provider, Langfuse wrappers, Raindrop's OSS Workshop debugger) see a standard endpoint and collapse brain choice to a base URL. Build it only when a tool that demands an HTTP endpoint earns it. Judge-only: extraction content never transits the shim (real PII through a ChatGPT account is what Venice exists to avoid; judge inputs are synthetic staging data).
- **Workshop** (Raindrop's MIT local trace debugger, local SQLite, standalone) is a candidate dev-loop viewer for either program once the SDK/shim exists — optional, never a dependency; `--json`/SDK events already provide the same trace data to our own spans.

## Environment & prerequisites

- **Target:** staging (auto-deploys `develop`, deterministic reseed of the `standard` world, reset contract shipped). Config via env: `TOURS_BASE_URL` + staging API key, never committed.
- **Ops prerequisite (personal-ops repo, not this repo):** un-defer the sandbox→staging path — the deferred "observe path" covered only a read-only PG role; touring needs staging's HTTP endpoint plus a reset trigger reachable from the sandbox.
- **Nightly autonomous wiring is out of scope for the first cut** — it lands after the prove phase demonstrates judge precision, reusing the umbrella's cadence/host decisions.

## Sequencing & relationship to other pieces

- **Piece 2 (Track A):** independent; the separate-tour-suite decision removes the umbrella's coupling to Playwright relaxation. Tours and relaxed E2E may later share page drivers if duplication warrants it.
- **Piece 3 (anti-drift):** deliberately built after Piece 4 despite the umbrella's ordering — the untoured-remainder report here is scoped as harness-local so it does not preempt Piece 3's scanner, and tour annotations become a second coverage producer for it to index.
- **#379 (extraction program):** shares the instrumentation layer per above; the Tier-3 platform issue is sequenced after this harness works end-to-end and before SP3's extractor iteration begins.
- **#477 (VPS + staging):** superseded by shipped work (#556/#566/#569/#570); closed with a pointer to this spec's remaining ops prerequisite.

## Implementation shape (~4 PRs + ops task)

1. **PR1** — tours Playwright project + capture harness (normalization, run dir, manifest) + `contacts.tour.ts`. Zero model involvement; verified by inspecting captures. Developable against a local stack while the ops prerequisite lands.
2. **PR2** — judge: adapter (codex SDK primary, exec fallback) + prompt protocol + verdict schema + **eval harness with seed golden corpus** (promptfoo spike decided here) + advisory run report, proven on the contacts captures.
3. **PR3** — `dashboard.tour.ts` + `cadence-followup.tour.ts` + untoured-behavior/skip-list reporting; golden corpus grows to v0 target.
4. **PR4** — reporter issue mode (fingerprint dedup, issue authoring), enabled only after the eval bar is met.
- **Ops (parallel):** sandbox→staging network exception.
- **Follow-up issues to file:** self-hosted observability platform standup (Tier 3, shared #379/#380); nightly autonomous wiring.

## Non-goals (first cut)

Vision/screenshot escalation, free-form agentic exploration, per-PR diff-scoped runs, nightly cron wiring, changes to the E2E suite (Piece 2), CI gates (Piece 3), and any auto-fixing (permanent, per the umbrella).

## Risks

- **Judge precision is the whole game.** Mitigated by the eval-driven prove cycle, the grounding rule, abstention routing, and advisory mode with an eval-defined exit — the harness cannot file an issue until it has earned the right to.
- **Subscription-programmatic access is a moving policy target.** Mitigated by the adapter seam; a metered key or Venice is a config swap.
- **Tour rot.** Assertion-free tours can silently stop exercising a behavior's `when` after a UI flow change. Mitigated by the untoured-remainder report and by tour errors being treated as signal, not flake.
- **Corpus overfitting.** A solo-labeled corpus this small can overfit the judge to one person's reading of the specs. Accepted for a single-user product; the frozen held-out split and the regression rule are the guardrails.

## SSOT note

One authoring-guidance spillover, no schema change: where relevant, ux behaviors benefit from negative/collateral `then` items ("no unrelated destructive action is visible/enabled") — the judge checks unexpected side effects only if the spec names them. Apply opportunistically during future curation; no bulk edit.

## Addendum — 2026-07-09: arc refinement & implementation decisions

This addendum extends the design in place (it does not supersede the narrative above) with the decisions taken when scoping the implementation as a plan-and-ship arc. Where a decision refines an earlier section, it is noted.

### Arc shape

The implementation ships as **three PRs (PR1 → PR2 → PR3), terminating in advisory mode**; **PR4 (reporter issue-mode) is deferred to a separate later arc**. This refines "Implementation shape (~4 PRs)" above. Rationale: the prove phase that gates issue-mode (held-out fail-precision bar + N consecutive human-confirmed clean live sweeps) is inherently time-based and human-gated, so an automated arc cannot merge its way through it. The arc therefore terminates with the harness running in advisory mode across the first cut — which is precisely the substrate the prove phase then runs on. Planning PR4 becomes sensible only once a v0 corpus exists and live sweeps are accumulating. Two human-in-the-loop gates remain: (1) corpus labeling mid-arc, (2) the prove-phase → issue-mode flip in the deferred PR4.

The first-cut inventory, stated exactly (current `ux` only; `proposed` skipped): **20 current behaviors** — PR1 `contacts.tour.ts` covers 7 (CON-038/040/041/042/043/044/045; CON-046 is `proposed`); PR3 covers 13 (`dashboard.tour.ts`: DSH-001/002/003/004/005/007, with DSH-006/009 `proposed`; `cadence-followup.tour.ts`: CAD-026/027/028/029/030/031/033).

### Environment

The **sandbox→staging network exception is complete** (the ops prerequisite in "Environment & prerequisites"); staging is the PR1 target from day one, no local-stack workaround required. The sandbox has a Playwright chromium build available; what it lacks is docker (the app-under-test's stack manager), not the browser — which does not matter when tours point at staging over the network.

### Decisions

- **Reset-before-run per sweep.** Each tour sweep first invokes `scripts/staging-reset.sh` (ssh mode) to obtain a known `standard` world, then runs all tours against it. This makes the destructive `when`s in the contacts scope (CON-042 delete, CON-043 merge, CON-044 mark-contacted) repeatable, and the run manifest pins the resulting staging image digest.
- **Hybrid grader (refines "Judge").** Each `then` item is classified as **deterministic-verifiable** (checkable in code from captured evidence — e.g. CON-041 URL-param stripped via regex, CON-044 interaction logged via a `/api/v1` JSON-path, "arrows inert" via aria-node presence) or **judge-only** (semantic — e.g. CON-042 "warns the action cannot be undone"). Deterministic items are checked by verifiers with ordinary unit tests; the LLM judge owns only the semantic residue, and **fail-precision (the north star) is measured over the judge's residual items**. This follows the surveyed "verifiers before judges" rule and directly lifts judge precision by removing mechanical noise. Consequence for the capture contract: captures must be stored **parseably** (queryable aria structure, `/api/v1` responses keyed by endpoint, URL as a field), not as opaque text blobs.
- **Server-time frame per capture (refines "Capture contract").** Accelerated time cannot be frozen — `accelerated.GetCurrentTime()` returns `TIME_BASE + wall_elapsed × TIME_ACCELERATION`, always advancing. Rather than perturb staging's clock, each capture records the app's current accelerated `now` plus the acceleration factor (read from the system-time endpoint); the judge and verifiers evaluate time-dependent `then` items (CON-045 birthdays, CAD due-dates, DSH widgets) **in that recorded frame**, and time-dependent before/after brackets are kept tight enough not to straddle a day boundary.
- **Before/after state-delta captures (refines "Capture contract").** Behaviors whose `when` mutates state are captured as an explicit before/after **pair** the grader can diff, rather than two independent snapshots — the "grade what changed in the world" discipline.
- **Corpus labeling = agent-drafted, human-corrected (refines "Judge evals").** Doctored fails remain self-labeled by construction (a deterministic single-point mutation of a clean capture manufactures a known fail on exactly one item; the human validates the mutation). For the real/clean/ambiguous cases that need genuine ground truth, a labeling CLI pre-fills **draft** per-item verdicts + critiques using a **different/stronger model than the judge** (avoiding the circularity the "no LLM-generated ground truth" rule guards against); the maintainer reviews and corrects, and the corrected labels are the ground truth. PR2 lands the deterministic doctoring tool and this labeling CLI alongside the seed corpus.

### Eval-guide fold-ins

A practitioner synthesis of the BenchFlow `awesome-evals` corpus (the same sources this design already drew on) largely validated the plan; because this judge is a read-only, no-tools *criticism* classifier rather than an action agent, the survey's trajectory/world-state/RL material is out of scope. Four items were folded in: the verifier layer (→ hybrid grader above); before/after state-delta captures (→ capture contract above); a **judge self-consistency metric** (repeat-verdict stability on a fixed capture) added to the PR2 eval harness; and **error-analysis-first sequencing** — deriving the judge's failure taxonomy from an open-coding pass over the first advisory sweep's real captures before finalizing the prompt and few-shots.
