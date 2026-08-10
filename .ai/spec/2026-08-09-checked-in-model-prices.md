# Checked-in model prices — retire the Langfuse price reconciler

Date: 2026-08-09
Status: PROPOSED
Implements: [#761](https://github.com/spengrah/PersonalCRM/issues/761). Supersedes the price-sync half (D10–D17, work items 5–6, and the model-prices test plan) of `2026-07-22-langfuse-usage-cost-tracking.md`; that spec's observation/cost half (D1–D9) shipped and is untouched.

## Context & problem

The judge harness keeps Langfuse model prices current via a reconciler that mirrors Langfuse's own price-resolution semantics in TypeScript — regex-flavor translation, tier precedence, managed-vs-override analysis, drift diffing, a >5x delta guard — pinned to a verified Langfuse version and driven by a five-flag mode matrix. Mirroring another system's internals is inherently version-fragile, but the decisive problem is narrower: the reconciler's whole job is converging our overrides toward Langfuse's *managed* price rows, and Langfuse's managed list is a hosted-provider list that does not contain the open-weights models the LLM extraction program (#379 SP3) will run via Venice. For every model that program actually cares about, there is nothing to reconcile against — we sit permanently in override mode and the reconciliation machinery is dead code by construction. Alongside it, a ~1,000-line manual smoke tool (`obs-smoke`) exists to prove the observability export round-trips, with no automated execution path and no test.

This work replaces all of it with a reviewed, checked-in price file and two small commands, as part of the broader effort to lighten the verification layer before the extraction work begins.

## What matters (priority order)

1. **Shrink the verification layer.** The reconciler pair, the smoke tool, their second-order artifacts (the price-sync provenance merger and its fixtures), the nightly-round sync step and its test block, and the now-dead `activeModels()` all go — roughly 5,800 lines replaced by ~250. The reconciler's test file runs in the standard vitest lane today, so the deletion also shrinks every pre-push and CI run.
2. **Preserve the one real capability: per-model cost attribution in Langfuse.** #379 SP3 specifies per-job model selection; balancing cost against capability requires cost-per-job attribution that is comparable across the Venice extractors and the hosted judge, from one source of truth.
3. **Simplicity over automation.** Human review of every drift PR is the control — the weekly workflow only fetches and proposes; nothing merges or applies without a person having seen the diff. No upstream mirroring, no runtime guards, no reconciliation. Overrides are permanent by choice — price correctness must not depend on Langfuse image upgrades. (Unattended merging is a possible later addition, coupled to an escalation guard — see Goals.)
4. **Self-healing operation.** No remember-to-run step between merging a price change and Langfuse reflecting it.

## Goals

- One checked-in `model-prices.json` as the single source of truth for every model whose cost we attribute. Every row declares its price source: `source: "venice"` (open-weights models called through Venice) or `source: "open-router"` (closed-weights models — today the judge's OpenAI models on the Codex path, priced from OpenRouter's quotes as a proxy for provider list prices). The tag names *price provenance*, not model kind — later changes may price open-weights rows from OpenRouter or closed-weights rows from Venice or new sources without restructuring, because collisions are impossible by construction: each row's model string identifies a distinct usage stream (the judge reports `gpt-5.6-luna`; a Venice call would report Venice's own id), and no two rows may match the same string.
- `make model-prices-sync`: refresh every declared row's prices from its declared source — Venice's models API for `venice` rows (per-**million**-token USD, ÷1e6; validate each is a text model), OpenRouter's models API for `open-router` rows (already per-token, but quoted as decimal *strings* to parse). Both APIs are anonymous. Rewrite the file deterministically sorted; the diff is the review surface. Sync never adds or removes rows (see Hard constraints).
- `make model-prices-apply`: converge Langfuse's project-scoped model definitions to the file. Idempotent — identical rows produce zero writes; changed rows are replaced delete-then-create.
- Auto-apply at the start of the nightly QA round, before the export step, so every observation the round produces is priced from the current file by construction. Fail-open: warn loudly, emit a manifest field, never block the round.
- A light scheduled workflow (weekly cron + manual dispatch), entirely within GitHub, that runs sync and opens a PR only when prices drifted. Every drift PR is held for human review and merged by hand — no auto-merge in this scope. No vendor secrets — both price-source endpoints are anonymous — but the PR-creating step still needs a repo-scoped fine-grained PAT, because PRs created with the default `GITHUB_TOKEN` do not trigger CI, and develop requires green CI to merge.
- **Deliberately deferred as a package:** unattended merging. If the loop proves trustworthy, auto-merge may be added later — but only together with a sync-time escalation rule (quiet bounded price changes auto-merge; a model appearing/disappearing, an out-of-bound delta, or a schema anomaly leaves the PR held for review). Neither half lands without the other: auto-merge without the classifier removes the design's only guard against upstream garbage.
- Replace obs-smoke with one assertion inside the nightly round: the round's own freshly-exported generation observation carries non-zero cost. Warn-only, same fail-open policy.
- Amend `2026-07-22-langfuse-usage-cost-tracking.md` in the same PR: mark the shipped observation half implemented and the price-sync half superseded by this spec.

## Non-Goals

- No first-party OpenAI price source. OpenAI publishes no pricing API, so closed-weights rows refresh from OpenRouter's quotes — a proxy for provider list prices, accepted with eyes open: on the day this was ruled, four sources disagreed on `gpt-5.6-luna` by up to 10x (Langfuse managed $1.00/M, LiteLLM $0.20, OpenRouter $0.10, Venice $0.267). The reviewed weekly diff is where a wrong number gets caught, and a subscription-priced judge is where absolute per-token accuracy matters least — comparability across models is the actual requirement.
- No dependence on Langfuse image upgrades for price freshness. An override freezes the model at the file's price until the next sync-driven change; that is the point.
- No pricing-tier support in the file. Rows are flat per-token prices; OpenRouter's large-context override tiers are deliberately dropped at sync time. Accepted approximation: models with a >272K conditional tier collapse to their standard tier — judge inputs run far below the threshold, and Venice quotes no tiers. Revisit only if either fact changes.
- No general Venice model-shape parser. Text models only; a non-text model in use fails loudly.
- No backfill or repricing of history. Cost is computed at ingestion and stored; nothing here recomputes it (see trade-offs).
- No change to scores, triage queues, trace granularity, deep links, or the surviving export/backfill/setup tooling and its shared Langfuse credential plumbing.
- No relocation of the judge export tooling out of its current directory. The new tooling lives in `infra/langfuse/` and imports nothing from the judge tree; moving that tree is a separate decision for a separate arc, not a rider on a deletion PR.
- No shared "LLM platform" code directory. The QA judge (TS) and the future extraction engine (Go, #379 SP3) share the Langfuse instance and its conventions, not code; the checked-in price file is the only genuinely shared artifact.

## Relation to existing & planned work

- **#761** — this spec is that issue, sharpened by exploration. Two corrections to its accounting: `obs-smoke.test.ts` does not exist (the tool is ~1,021 untested lines, not 2,032), and there is no GitHub workflow or cron invoking the round or the price sync — the round orchestrator is invoked externally, so the deletion touches the round script and its tests, not workflow files. The issue's quoted Venice payload shape is also outdated: the live shape is `pricing: {input: {usd, diem}, output: {usd, diem}, cache_input?: {…}}` (per-field objects, USD per million tokens), not `pricing.usd = {input, output}`. And the issue's design evolved in two ways during exploration: sync is narrowed from a catalog import ("filter to text models → write the file") to refreshing declared rows, because Venice's catalog also carries proxied hosted models; and the issue's hand-entered non-Venice rows became `source: "open-router"` rows refreshed from OpenRouter's anonymous API, so closed-weights prices stay current through the same reviewed loop instead of by hand.
- **#379 SP3 (LLM extraction)** — the consumer this exists for. Per-job model selection needs per-model cost attribution; the extraction engine's own Venice endpoint/key configuration is SP3's scope, not this spec's.
- **`2026-07-22-langfuse-usage-cost-tracking.md`** — superseded in its price-sync half; its verified Langfuse API facts are carried forward below rather than re-derived.
- **#723 (breaker-tripped rounds and the watermark)** — adjacent nightly-round machinery; the auto-apply step and cost assertion must not entangle with the cadence gate or watermark logic.

## Prior art & external constraints (verified facts)

Facts below were verified live or against deployed-server source; do not re-derive, and re-verify the Langfuse ones on any Langfuse upgrade.

- **Venice `GET /api/v1/models?type=text` requires no authentication** (probed 2026-08-09, HTTP 200 anonymous, pricing included). No Venice credential exists anywhere in this design. Prices are USD per **million** tokens.
- **Venice's catalog is not open-weights-only**: it includes proxied hosted models (`openai-gpt-56-luna`, `claude-opus-5`, `gemini-3-6-flash`, …) at Venice's own rates — roughly list + 25% on comparable models, though not uniformly (probed 2026-08-09: mostly a 15–25% premium over OpenRouter across 40 matched models, with outliers in both directions). This is why sync refreshes declared rows rather than importing a catalog, and why a human picks each row's source at declaration time.
- **OpenRouter `GET /api/v1/models` requires no authentication** (probed 2026-08-09): ~400 models, 95 `openai/*`, prices already per-token but quoted as decimal strings, including `input_cache_read`/`input_cache_write` and large-context override tiers. Prices are what OpenRouter charges for the route — usually provider list price, not guaranteed to be.
- **Naming conventions differ per catalog for the same underlying model** — `gpt-5.6-luna` (Codex/judge) vs `openai/gpt-5.6-luna` (OpenRouter) vs `openai-gpt-56-luna` (Venice). This is why each row carries both its usage-stream model string and a source-side lookup id, and why distinct usage paths cannot collide in Langfuse matching.
- **Candidate price sources genuinely disagree** (2026-08-09, `gpt-5.6-luna` input $/M): Langfuse managed 1.00, LiteLLM 0.20, OpenRouter 0.10, Venice 0.267 — likely a provider price cut absorbed at different speeds. Recorded so nobody later treats any single source as ground truth; the reviewed diff is the arbiter.
- **Langfuse stores prices per token** (managed rows show `7.5e-7` for $0.75/M). Venice quotes per **million** tokens, so sync divides its rows by 1e6; OpenRouter already quotes per token (as decimal strings). Getting the conversion wrong yields costs off by a factor of a million while looking structurally fine — the checked-in JSON diff is the guard (`7.5e-7` vs `0.75` is unmissable in review), replacing the deleted runtime heuristic.
- **`POST /api/public/models` is create-only**: a second POST for an existing `(projectId, modelName)` 400s; there is no PUT/PATCH; project-scoped rows are DELETE-able (managed rows are not). Hence delete-then-create for changed rows. Both operations clear the model-match cache.
- **Cost resolution happens at ingestion time and is stored.** Replacing a model definition never touches already-priced observations; past runs keep their numbers. A re-export recomputes at current prices — a pre-existing, documented hazard, unchanged here.
- **Resolution order is custom-over-managed, then newest `startDate`** — so a project-scoped override wholly shadows the managed row for as long as it exists. This design accepts that permanently (Non-Goals).
- **`matchPattern` is a Postgres-flavored regex**: the `(?i)` inline-flag prefix is valid there and a SyntaxError in JavaScript. Patterns are emitted, never evaluated, by our tooling.

## Hard constraints

- **Apply never reads upstream and never decides anything.** It converges Langfuse to the file: per row, skip when the existing project-scoped definition matches, delete-then-create when it differs. This is Langfuse-vs-our-own-reviewed-file convergence — any logic that consults Venice, Langfuse's managed rows, or price deltas inside apply is the deleted reconciler growing back.
- **Sync refreshes declared rows; it never imports a catalog.** The file declares every model whose cost we attribute. A row carries: the model string its usage stream reports to Langfuse (the judge's Codex path reports bare OpenAI names like `gpt-5.6-luna`; a Venice call reports Venice's id), the `source` naming which catalog refreshes its prices, and the source-side lookup id (OpenRouter keys differ from the model string: `openai/gpt-5.6-luna`). Sync updates prices in place for each row from its source and never adds or removes rows — declaring a model is a hand edit, which is the moment a human picks the source whose price is true for that stream's path. Loud failures, never silent: a declared row absent from its source's catalog (never keep stale prices silently, never delete), and any two rows whose emitted matchPatterns match the same model string (a collision makes cost attribution ambiguous by construction). All rows ride the same apply — that is the one-source-of-truth goal. Initial contents: the four judge models (`gpt-5.4-mini`, `gpt-5.5`, `gpt-5.6-luna`, `gpt-5.6-terra`) as `open-router` rows; `venice` rows arrive when #379 SP3 declares extraction models.
- **`matchPattern` is emitted mechanically from the row's model string, never authored** — for Venice rows `(?i)^(venice\/)?<escaped-model-id>$`; for other rows an escaped exact match of the model string (exact emission per source is the planner's to pin; the constraint is mechanical derivation plus the no-two-rows-match-one-string validation).
- **Deterministic file output**: stable sort, stable formatting, so sync diffs are reviewable and a no-drift sync is byte-identical.
- **Fail-open in the round, loud everywhere**: auto-apply and the cost assertion warn and emit manifest fields but never block a round; stale prices make cost approximate, a skipped round makes QA absent. The manual targets fail with non-zero exit.
- **No new credentials.** Neither price source needs any; apply uses the Langfuse write credentials the round environment already carries (shared with the surviving export tooling — do not unwind them).
- **Manual apply must not run while an export is in flight** — the delete-then-create gap would price a concurrently-ingested observation at zero. Auto-apply is sequenced before the round's export step, which satisfies this by construction; the constraint binds only ad-hoc human runs.

## Architectural direction

- **Constraint — file and tooling live together in `infra/langfuse/`**: `model-prices.json`, the sync and apply scripts, and their tests, as one cohesive unit. This is operational machinery — the desired price state of a deployed service and the tools that converge it — shared by both programs, so it belongs to neither program's tree; `infra/` already means "operate the deployment." Concretely: a price-change PR (including every PR the scheduled workflow opens) must read as an ops change and must not sit inside any test-lane path-filter group, so a price-only diff triggers only minimal CI lanes. The judge directory only shrinks in this arc.
- **Constraint — the new tooling is self-contained**: no imports from the judge export tree. It reads the same `LANGFUSE_*` env vars but wraps its own HTTP (likely dependency-free fetch under bun — planner verifies). Duplicating the ~40-line helper is the point, not a smell: importing across `infra/` → `frontend/tests/tours/judge/` would recreate the coupling the factoring removes.
- **Leaning — the contract tests gate merges via an existing lane**: most likely a `bun test infra/langfuse` invocation folded into an existing gating make target, not a new pre-push phase (a new phase drags in the lane classifier and phase-guard test). The planner pins the mechanism; the constraint underneath is that the tests gate a merge.

## End-state assertions (must NOT exist after this ships)

The deletion is only done when all of these are true:

- No upstream (Langfuse `default-model-prices.json`) fetch anywhere.
- No reconcile/diff/precedence logic against Langfuse managed rows; no regex-flavor translation; no tier mirroring.
- No `VERIFIED_LANGFUSE_VERSION` pin.
- No price-delta guard anywhere in this scope (the old >5x runtime heuristic is gone, and no replacement delta logic ships now — every drift PR is human-reviewed instead). If auto-merge is adopted later, its sync-time escalation rule is the one permitted form of delta logic: it compares the new file to the previous file and can only ever route a PR to a human, never block or modify an apply.
- No `DRY_RUN`/`FORCE`/`STRICT`/`RESET`/`UPSTREAM`/`MODELS` flag matrix on any make target.
- No `qa-model-prices` or `qa-obs-smoke` make targets; no `obs-smoke.ts`.
- No price-sync provenance machinery: no `manifest-merge` module or test, no `price_sync*` manifest keys, no upstream-price fixtures, no `QA_MANIFEST_MERGE` seam or its stubs in the round test harness.
- No `activeModels()` in `judge/models.ts` (the model/effort default constants stay — adapters and the intent runner use them).
- The reconciler's test file is gone from the vitest lane (pre-push and CI both shrink).

## Success criteria

- A judge round run against a merged `model-prices.json` produces generation observations whose `totalCost` is non-zero and matches a hand computation from the file's per-token prices — verified live once from an environment with Langfuse write access.
- The round's cost assertion goes green on that same run, and visibly warns (without failing the round) when pointed at a model absent from the file.
- Apply run twice in a row against a converged instance performs zero writes on the second run; editing one price in the file and re-applying performs exactly one delete and one create.
- Sync run against the live sources with no upstream drift produces an empty diff; the scheduled workflow opens no PR that week.
- A drift yields exactly one open PR whose CI checks actually run (the PAT requirement holding), held for human review — nothing merges unattended.
- All end-state assertions hold; net line count across the touched area shrinks by roughly 5,500 lines.

## Desired behavior sketch

Prices flow in one direction with review in the middle: the price sources publish → weekly workflow (or a hand-run `make model-prices-sync`) refreshes every declared row from its source → a PR exists only if something drifted → the human reviews the diff and merges (this is where a unit error, a source disagreement, and upstream garbage all die) → the next nightly round pulls, auto-applies the file to Langfuse, exports, and asserts its own observations carry cost. The dangerous conversions are additionally pinned by contract tests — a known Venice per-million quote and a known OpenRouter decimal-string quote each mapping to their exact per-token output — so neither can regress silently between reviews. Judge-model rows ride the same file, the same sync, and the same apply as extractor rows. `make model-prices-apply` remains available for ad-hoc use — e.g. pricing a new model before an experiment — from any checkout with Langfuse write access (today: the qa tenant).

## Assumptions & deferred questions

- **Assumed:** the qa tenant's checkout freshness is owned by whatever invokes the round today; a stale checkout applies stale prices and self-heals on the next fresh run. No new machinery for this.
- **Assumed:** Venice's models endpoint stays anonymous. If Venice ever requires auth, sync gains a key and its placement gets decided then (workflow secret + tenant env); nothing else changes.
- **Deferred (user, infra, off critical path):** relaxing the dev sandbox's Langfuse write restriction on stovepipes would allow live apply verification from the dev loop; until then, live verification of apply happens from the qa tenant and dev-loop tests use a fake transport. Resurface when implementation reaches live verification.
- **User-provisioned at implementation time:** a fine-grained PAT (contents + pull-requests, this repo only) as an Actions secret for the workflow's PR step — required because default-token PRs never trigger CI, and develop's protection requires green CI to merge; without it every price PR would need a close/reopen just to get checks running. Resurface when the workflow element is being built.
- **Deferred to #379 SP3:** the extraction engine's runtime Venice configuration, its usage-bucket naming (and therefore whether extractor rows need a `cache_input` price key — the file schema should tolerate an optional cached-input price per row, populated when Venice provides one), and any future need for per-job price granularity beyond per-model.
- **Deferred indefinitely (pre-existing):** whether to ever backfill/re-export history knowing it reprices at current prices. Decide deliberately if ever wanted; nothing here makes it better or worse.
- **If a future price source is ever added** (the `source` tag is designed to admit one): prefer anonymous APIs with plain model keys, and avoid Langfuse's upstream `default-model-prices.json` — its versioned-modelName/regex entry selection is the trap the deleted reconciler existed to navigate.
