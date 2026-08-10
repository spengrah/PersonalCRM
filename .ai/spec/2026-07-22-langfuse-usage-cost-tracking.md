# Langfuse token + cost tracking for judge traces

Date: 2026-07-22
Status: The observation/cost half (D1–D9) is IMPLEMENTED. The price-sync half (D10–D17, work items 5–6, and the model-prices test plan) is SUPERSEDED by `2026-08-09-checked-in-model-prices.md`.

This doc is the SSOT for wiring **token usage and USD cost** into the ux-qa judge's Langfuse export, and for **keeping the instance's model prices current** so those costs stay true. It exists because the judge's spend is currently invisible in Langfuse, and because the upcoming `gpt-5.6-luna` / `gpt-5.6-terra` judge-model comparison (queued in `2026-07-19-codex-sdk-judge-transport.md`) is a cost/quality tradeoff that cannot be evaluated without the cost half.

## The problem, grounded

The adapters **do** capture usage. `adapter/codex-sdk.ts` reads `turn.usage.input_tokens`/`output_tokens` and `adapter/span.ts:85-86` writes them to `gen_ai.usage.input_tokens`/`gen_ai.usage.output_tokens` on the span. The numbers reach the JSONL artifact intact.

`export/langfuse.ts` then puts them somewhere Langfuse cannot use: `langfuse.ts:140-141` copies them into the trace's `metadata` object, and **every** ingestion event in the export path is `type: 'trace-create'` (plus one `score-create`). There is no `observation-create` anywhere in the exporter.

Langfuse only computes usage and cost on **observations** of type `generation` or `embedding`; trace-level totals are aggregations over observations, and `metadata` is opaque display-only key/value that neither the dashboards nor the Metrics API read. So the export is working exactly as written and the answer is still zero.

Verified live against the self-hosted instance (`$LANGFUSE_HOST`, project `qa-harness`) on 2026-07-22:

```
GET /api/public/observations                    → totalItems: 0     (project-wide)
GET /api/public/traces/judge-DSH-004-<span>-item2
    observations: []      totalCost: 0
    metadata: {… "model":"gpt-5.4-mini","input_tokens":13331,"output_tokens":229 …}
```

Whole-project scale for context: 86 judge calls carrying usage, 3.44M input tokens, 28.5k output tokens. At `gpt-5.4-mini` fresh-input pricing that is ~$2.71; if ~70% of input is cache-read it is ~$1.00. **That gap is the single biggest reason to capture cached tokens rather than only the totals we have today.**

## Measured facts (do not re-derive; several contradict the obvious guess)

Each of these was measured on 2026-07-22, not inferred. They are recorded here because at least three of them are the opposite of the reasonable assumption.

1. **The models are already priced. No custom model definitions are needed.** The instance carries 170 model definitions across **two** pages — a `?limit=100` query returns only page 1 and makes `gpt-5*` look absent. All four judge models in play are present with cached-input prices:

   | model | input | cached input | output |
   |---|---|---|---|
   | `gpt-5.4-mini` | $0.75/M | $0.075/M | $4.50/M |
   | `gpt-5.5` | $5.00/M | $0.50/M | $30.00/M |
   | `gpt-5.6-luna` | $1.00/M | $0.10/M | $6.00/M |
   | `gpt-5.6-terra` | $2.50/M | $0.25/M | $15.00/M |

   Cached input is **10× cheaper** across the board. Cost inference therefore works on day one for the 5.6 experiment with zero Langfuse-side setup, and there is no ingestion-time price-ordering hazard to plan around.

2. **`gpt-5.4-mini` has no `output_reasoning_tokens` price.** `gpt-5.4` / `gpt-5.5` / `gpt-5.6-*` do, and in every entry it is identical to the plain `output` price. Splitting reasoning into its own usage bucket therefore buys nothing on the models that price it and leaves the bucket **unpriced** on `gpt-5.4-mini`, silently undercounting the ux pass.

3. **Codex SDK 0.144.6 emits five usage fields at runtime, but its `.d.ts` declares four.** Probed with a live two-turn thread:

   ```
   turn1: input 18584  cached  4480  cache_write 0  output 25  reasoning 18
   turn2: input 37186  cached 22784  cache_write 0  output 52  reasoning 37
   ```

   `cache_write_input_tokens` is absent from the published `Usage` type (`node_modules/@openai/codex-sdk/dist/index.d.ts:119-129`). Read usage defensively — do not trust the type to be exhaustive.

4. **`cached_input_tokens` is inclusive of `input_tokens`.** Turn 2's cached count (22784) exceeds turn 1's entire input (18584); the numbers are only coherent under inclusive accounting. Langfuse requires **mutually exclusive** usage buckets to derive a correct total, so `input` must be ingested net of cached.

5. **`turn.usage` accumulates across a thread — but that is moot here.** Turn 2's `input_tokens` is exactly turn 1's plus turn 2's own. `defaultRun` calls `new Codex().startThread(...)` per judge call (`adapter/codex-sdk.ts:90`), so every recorded usage is a single turn. This is a live landmine only if the transport is ever changed to reuse a thread.

6. **The per-item fan-out is a latent double-count, not an active one.** `buildTraceBody` fans one span out to one trace per graded item (`langfuse.ts:152`), each carrying that span's single token count. But `judge-runner.ts:20-24` invokes the judge once per behavior over its residue items, and every behavior in every round so far has had exactly **one** residue item: all 86 exporter-produced judge traces have a unique span id. Attaching usage per trace would be correct today and would multiply spend by the item count the first time a behavior carries 2+ judge-tagged then-items.

7. **Intent traces are indistinguishable from ux traces by tag or name.** An intent trace is named `judge DSH-012` and tagged `behavior:DSH-012` exactly like a ux trace; the only discriminator is `input.scenario_item.intent_id`, which is not filterable in a dashboard. Today model doubles as the pass proxy (`gpt-5.4-mini` = ux, `gpt-5.5` = intent) — **the 5.6 experiment destroys that proxy**, which is precisely why the dimension has to become explicit before the experiment runs rather than after.

8. **There is prior art for a model tag.** 22 traces from an earlier hand-rolled experiment carry `qa-experiment` + `model:gpt-5.6-luna` and names like `intent DSH-011 (gpt-5.6-luna r2)` — the dimension was already needed once and was added outside the exporter.

## Decisions

**D1 — One `generation` observation per SPAN, attached to that span's lowest-`itemIndex` trace.** Not one per trace (double-counts under fact 6), and not a restructure to one-span-one-trace.

Rejected: **collapsing the fan-out to one trace per span.** It is a migration, not a shape tweak — per-item `verdict` scores would have to become observation-scoped, `backfill.ts:57-62,156-170` explicitly rejects non-trace score subjects and joins on `subject.id`, the standing `qa-triage` queue items would change `objectType`, the 31 existing human `ANNOTATION` scores (16 `ground_truth`, 9 `verdict`, 6 `disposition`) are keyed to per-item trace ids, and `spec` pins per-item granularity so it would be a retire-and-mint. Decisively: **it would not deliver cost anyway** — a trace has no usage of its own, so a generation observation is required either way. It is a large refactor that buys tidiness, not the feature.

Rejected: **dividing usage across sibling item-traces.** Fractional tokens, and it makes a single judge call unreadable as a unit when debugging.

Consequence to accept: on a future multi-item behavior, sibling traces read $0 while the primary carries the whole call. Round-, session-, and model-level totals stay exactly right, which is the granularity every question here is asked at. Sibling traces get `usage_attributed: false` in metadata so a reader is never left guessing why.

**D2 — Usage buckets mirror the price keys, with `input` net of cached and reasoning folded into `output`.**

```
usageDetails: {
  input:               input_tokens - cached_input_tokens,   // cached is INCLUSIVE (fact 4)
  input_cached_tokens: cached_input_tokens,
  output:              output_tokens,                        // reasoning stays inside (fact 2)
}
```

This is exactly the shape the Langfuse docs' own worked example uses (`17903` gross input with `17817` cached → `input: 86` + `input_cached_tokens: 17817`). Flat usage keys are stored verbatim with no normalization, the server derives `total` as the bucket sum, and price lookup is **exact string equality** against the model definition's price keys — so a bucket named anything else prices at zero rather than erroring.

Bucket names match the model definitions' price keys exactly, and the three are mutually exclusive so Langfuse's derived total is correct. `reasoning_output_tokens` and `cache_write_input_tokens` go in the observation's `metadata` — visible for analysis, not priced, not double-counted. Clamp the `input` subtraction at zero defensively: a provider that ever reports exclusive counts would otherwise ingest a negative bucket.

**D3 — Emit `pass:ux` / `pass:intent` and `model:<model>` trace tags.**

`pass` is derived from `scenario.kind`, which `buildTraceBody` already holds (`langfuse.ts:114`). It is the dimension that exists nowhere else (fact 7).

`model` is redundant with the observation for **cost** — Langfuse groups generations by model natively — but not for **quality**: `verdict` and `ground_truth` scores are trace-scoped while model is observation-scoped, so without the tag, accuracy-by-model and cost-by-model cannot be put in one view. That view is the entire deliverable of a 5.4-mini vs luna vs terra bakeoff. It also matches the convention the existing luna corpus already set (fact 8).

**D4 — Accumulate usage across retry attempts.** `adapter/codex-sdk.ts:120-124` re-runs once on a tool-using turn and keeps only the last attempt's `result`, so the discarded attempt's tokens vanish from the span though they were really spent. If luna and terra trip the tool guard at different rates, that is a systematic bias in exactly the comparison this work exists to enable. Sum usage across attempts; keep verdicts from the accepted attempt only.

**D5 — Ship the observation as its own non-fatal step, after the trace lands.** Same shape as the existing `score-create` (`langfuse.ts:693-723`): separate ingestion request, own `try`/`catch`, never co-batched. A rejected observation must never couple to or drop an already-shipped trace (INV-A).

**D6 — Stable observation id `obs-${spanTraceId}-gen`.** Re-export upserts rather than duplicating, matching the trace ids' existing stability contract (INV-6). A re-export also **recomputes** inferred cost at the new ingestion time, which is the backfill path for the existing traces if we want history priced.

**D7 — Use the `generation-create` event, not `observation-create`.** Both work against the deployed server (v3.212.0, confirmed via `/api/public/health`) — its `LegacyObservationBody` Zod schema does accept `usageDetails` and `IngestionService` merges it into `provided_usage_details`. But in the published OpenAPI, `observation-create`'s body carries only the **deprecated** `usage: {input, output, total, unit}` object; `usageDetails`/`costDetails` are documented solely on `generation-create`/`generation-update`. The server's own name for the event we would be riding is `legacyObservationCreateEvent`. An upgrade that realigns the Zod schema with the published one would silently drop the field — precisely the silent no-op this whole spec exists to eliminate. `generation-create` takes the identical body minus the `type: 'GENERATION'` discriminator (the event type implies it) and is the documented carrier. Zero-cost swap; take the documented path.

**D8 — Set `startTime`/`endTime` from the span, and a `name`.** Every ingestion body field is optional and cost computes at ingestion with no completion event required — but when `startTime` is absent the server falls back to the event envelope timestamp, i.e. **export time**. Time-bucketed cost views and the Metrics API would then stamp a backfill's entire history on the day it was backfilled, and a D6 re-export would drag historic observations' start time forward. Derive both from `span.start_time_unix_nano` / `end_time_unix_nano`, the same discipline the existing `score-create` already applies to its envelope (`langfuse.ts:700`). Name the generation `judge <behaviorId>` to match its trace.

**D9 — The observation's `model` routes through `scrub`, like the trace's.** `langfuse.ts:89-97` states the policy: every env-sourced string must be scrubbed, and `model` is env-sourced (`QA_JUDGE_MODEL`). The trace metadata already ships `scrub(model)` (`langfuse.ts:111`). Ship the same scrubbed value on the observation — harmless in practice (model names match no PII pattern, so regex price-matching is unaffected) and it keeps one value across trace and observation instead of two that can diverge.

## Scope addition: keeping the instance's model prices current

Cost inference is only as good as the price table it reads. The instance's 170 model definitions are **Langfuse-managed** (`project_id IS NULL`), baked into the worker image at build time by `worker/src/scripts/upsertDefaultModelPrices.ts` and applied on worker start — there is no runtime fetch and no phone-home, so **an image upgrade is the only built-in way prices ever change**. Upstream maintains them in `worker/src/constants/default-model-prices.json` via a nightly audit workflow that opens `chore(pricing): update default model prices` PRs. Between image upgrades, this instance's prices drift silently, and a judge-model bakeoff decided on stale prices is decided on fiction.

So: a standalone price-sync step, run by the nightly round when the round will actually run, and runnable by hand before an ad-hoc experiment.

**Verified API facts.** `DELETE /api/public/models/{id}` **cannot** remove a managed model — the documented override path is "create your own definition with the same modelName". `CreateModelRequest` carries full `pricingTiers` fidelity (`conditions`, `isDefault`, `priority`, per-tier price maps) plus `startDate`, `tokenizerId`/`tokenizerConfig`, `unit`. Tiers and flat prices (`inputPrice`/`outputPrice`/`totalPrice`) are mutually exclusive, and a tier array must contain exactly one default (`isDefault: true`, `priority: 0`, `conditions: []`). Resolution order is **(1) custom over built-in, (2) newest `startDate` where `model.startDate < observation.startTime`**.

**D10 — Its own script, invoked two ways.** `export/model-prices.ts`, following `backfill.ts`'s shape (pure functions + a `main()` CLI), reusing `configFromEnv`/`api`/`apiGetAllPages` from `langfuse.ts` rather than growing a second HTTP seam. Makefile target `qa-model-prices` with `MODELS=` and `DRY_RUN=1`. The nightly calls it; a human calls it before pointing the judge at luna or terra.

**D11 — Select models by regex match, not name equality — and the patterns are not JS regexes.** The judge sends a model *string*; Langfuse matches it against `matchPattern`. These are not the same thing: `gpt-5.5`'s definition has `modelName: "gpt-5.5-2026-04-23"` and pattern `(?i)^(openai/)?(gpt-5.5(-2026-04-23)?)$`. Selecting upstream entries by `modelName === 'gpt-5.5'` finds nothing and the sync silently no-ops on the intent pass's model.

Every relevant pattern begins with the inline flag `(?i)`, which is valid in Postgres but a **`SyntaxError` in JavaScript** — `new RegExp("(?i)^…")` throws "unrecognized character after (?" (verified). A naive port crashes, and a defensively `try`/`catch`-ed one selects nothing and reports success. Strip the `(?i)` prefix and apply the `i` flag. The test for this must use a **verbatim** upstream pattern, not a hand-written fixture, or the trap survives review and reaches production.

Ambiguity and absence both fail loudly: more than one upstream match means guessing which price is right (Langfuse's own tiebreak is not ours to assume), and **zero** matches means the model was renamed or removed upstream. Neither may no-op silently — that is the same class of failure this decision exists to prevent.

**D12 — Mirror `pricingTiers` and `matchPattern` verbatim; never flatten.** Copy the upstream tier array through as-is. This is not hypothetical: **luna and terra each carry two tiers upstream** — "Standard" plus "Large Context (>272K)" at 2× the prices. Collapsing them to `inputPrice`/`outputPrice` would silently misprice every generation that hits the conditional tier, and would do so invisibly, since the result is a valid model definition that is simply wrong. (At judge input sizes — 13-40k tokens — the conditional tier cannot fire today, which is exactly why a flattening bug would go unnoticed until it didn't.)

The override must also carry **upstream's `matchPattern`**, since that is what makes the judge's bare model string resolve at all. Two shape notes for the POST: upstream entries have **no** `startDate`, `unit`, or flat prices, while `unit` is **required** by the create schema (managed rows carry `null`) — send `"TOKENS"` and exclude `unit` from the drift diff, or every comparison reports a phantom delta forever.

**D13 — Project-scoped overrides, replaced by delete-then-create. Resolution is at INGESTION time, not observation time.**

The documented contract and the deployed behavior disagree, and the difference matters enough to state which one we build against. The OpenAPI says definitions resolve by "newest according to startTime where `model.startTime < observation.startTime`" and describes `startDate` as "apply only to generations which are newer than this ISO date". **The deployed server does not do this.** In v3.212.0 (and on `main`), `findModelInPostgres` destructures only `{ projectId, model }` — the observation's start time is never passed in — and resolves with:

```sql
WHERE (project_id = $projectId OR project_id IS NULL) AND $model ~ match_pattern
ORDER BY project_id ASC, start_date DESC NULLS LAST LIMIT 1
```

`start_date` is only a tiebreak among same-name rows; there is no date-vs-observation comparison anywhere. Two consequences, both correcting earlier drafts of this spec:

- **Overrides are not non-retroactive.** A new price applies to everything *ingested* after it is written. Already-ingested observations keep their stored cost only because nothing recomputes them — so the D6 re-export path **reprices history at today's prices**. That is a genuine hazard for the backfill open item, not a neutral detail.
- **The real ordering constraint is sync-before-*export*, not sync-before-*judge*.** The placement chosen below still satisfies it (it runs before both), so nothing operationally changes — but the invariant is about ingestion, and recording the wrong reason would leave the next reader defending a constraint the server never checks.

Build against the **deployed** behavior, and re-verify it on any Langfuse upgrade: if the documented rule is ever actually implemented, both bullets above flip.

**Write mechanics.** `POST /api/public/models` is **create-only** and rejects on a `(projectId, modelName)` uniqueness check — `startDate` is *not* part of that key — with a 400 "Model name 'X' already exists in project". There is no `PUT`/`PATCH` on `/api/public/models/{id}`; only `GET` and `DELETE` exist. So "additive dated overrides" works exactly **once** per model: the first drift writes an override, and every subsequent drift — including an upstream *revert* — POSTs into a 400 that D16's fail-open would log forever while prices silently stayed stale. Precisely the failure mode this spec exists to eliminate.

Therefore: when a project-scoped override already exists for a target model, **DELETE it, then POST the replacement**. Project-scoped rows are deletable (only managed ones are protected), and both create and delete call `clearModelCacheForProject`, so the model-match cache cannot serve a stale price to an export that follows. Managed rows are never touched.

**The sync RECONCILES; it does not one-way write.** An override is a precedence claim, not a value claim — a project-scoped row outranks the managed one permanently even when its prices are byte-identical, so a sync that only ever *adds* overrides quietly appoints itself sole price maintainer for every model it touches. Instead, each run drives the target toward one of three states:

| managed row vs upstream | existing override | action |
|---|---|---|
| matches | none | nothing (the common case — see below) |
| matches | present | **DELETE the override** — managed has caught up, hand the model back |
| stale | none | POST an override carrying upstream's prices |
| stale | present, prices differ | DELETE + POST (the second-drift case) |
| stale | present, prices match upstream | nothing |

The third row is what makes the door close by itself: when a Langfuse image upgrade brings the managed row current, the next nightly run removes our override and the model resumes tracking managed prices with no human involved. The override becomes a *transient patch over the gap between image upgrades*, which is exactly what it should be.

Why not update the managed rows directly instead: `upsertDefaultModelPrices` upserts managed rows keyed on `{projectId: null, id}` and skips any whose `updatedAt` already equals the baked JSON's (`isModelUpToDate` compares `getTime()` exactly, plus tier-id set equality). So writing upstream's newer values straight into the managed rows — same id, same `updatedAt`, same tier ids — *would* persist across worker restarts and would need no override at all. Rejected anyway: there is no API for it (the public route always writes project-scoped rows), so it requires direct Postgres access from wherever the sync runs, and it writes to a table owned by another application's migrations. The public API is a contract; the schema is not. A Langfuse schema change would break the writer silently. The reconciling override gets the same end state over HTTP, with no new credentials and no schema coupling.

Note that **today the common case is the first row — nothing to do.** All four judge models on the live instance already match upstream exactly (including luna's and terra's two-tier structures), so a real-model sync run writes nothing. A zero-write run is a **PASS**, not a broken script; anyone reading it as failure will be tempted to "fix" it into a sync that writes unconditionally, which is the ~365-rows/model/year bug D14 exists to prevent.

**D14 — Zero drift means zero writes; the second drift must still write.** Compare the instance's *effective* definition per target model against upstream; identical prices produce no request at all. A sync that writes unconditionally creates a row per model per night, ~365/model/year. But the companion case is the one that actually bites: a **second** drift must delete-then-recreate rather than 400 (see D13). Both are explicit tests.

Resolving "effective" is the script's job, not the API's: among all instance rows whose `matchPattern` matches the target string, pick the one the server would — custom before managed, then newest `startDate` — mirroring `ORDER BY project_id ASC, start_date DESC NULLS LAST`. Diff on `pricingTiers[].prices`, which is a flat `{usageType: number}` map in both upstream and the API. Ignore the response's top-level `prices` field: it is the deprecated flattened view and its shape differs (`{usageType: {price: number}}`).

**D15 — Guard the write path; this is billing-relevant config fed from an unpinned URL.** The fetch is `main` on a public repo with no auth, applied automatically to a config that determines every cost number downstream. Four cheap guards:
- **Strict shape validation** of the fetched JSON — reject the whole run on any unexpected structure rather than partially applying. A truncated response or an HTML error page must fail loudly, never parse into "the price is now zero."
- **Implausible-delta refusal** — any usage-type price moving more than 5× in either direction, or arriving as zero/negative/absent when it previously existed, aborts that model's sync and reports it. A real price change of that size is rare enough to be worth a human look; a corrupted one is exactly this shape. `--force` overrides. The boundary is deliberately *exclusive*: OpenAI's June 2025 o3 cut was exactly 5×, and a legitimate change of that magnitude should pass rather than need forcing.
- **Upstream deletion/rename** — a target matching zero upstream entries (D11) reports loudly and leaves the existing definition alone. Never interpret "not found upstream" as "price is now nothing."
- **Only the models this run will use** — never a blanket sync of all 162 upstream entries. Targets come from `activeModels()` (D17), which resolves **both** passes' overrides: `QA_JUDGE_MODEL` for the ux pass and `QA_INTENT_MODEL` for the intent pass, plus `--models` on the CLI. These are different env vars, and missing the intent one would sync `gpt-5.5` while leaving an experiment's actual model unpriced.
- **Provenance** — record the fetched payload's sha256 (and upstream commit, if obtainable) in the round manifest, so a cost anomaly can be traced back to the exact price set in force.

**D16 — Fail open in the nightly, `--strict` for ad-hoc runs.** A sync failure must not abort a round: stale prices make cost *approximate*, while a skipped round makes the whole night's QA *absent*. Log loudly, emit a manifest field, continue. For a deliberate experiment run, `--strict` exits non-zero so a human bakeoff never starts on prices that failed to update.

**D17 — Move the model/effort defaults out of the retired exec adapter into `judge/models.ts`.** Today `adapter/codex-sdk.ts:15` imports `DEFAULT_JUDGE_MODEL` and `DEFAULT_JUDGE_EFFORT` (`adapter/codex-exec.ts:169-170`) from the transport the harness no longer uses, while the intent pass keeps its own pair in `intent-runner.ts:39-40`. The sync needs a single answer to "which models will this run use", and reading it out of a dead adapter is both fragile and confusing the day exec is deleted.

New `judge/models.ts` owns all four constants plus `activeModels(env)`, which resolves the two passes' distinct overrides (`QA_JUDGE_MODEL`, `QA_INTENT_MODEL`) into the target list the sync consumes. Update the import sites — `adapter/codex-exec.ts`, `adapter/codex-sdk.ts`, `intent-runner.ts`, and the three test files — rather than re-exporting from the old locations; a compatibility shim would leave exactly the ambiguity this removes.

Out of `activeModels()`'s scope by design: the `http` stub adapter resolves its own model (`adapter/http.ts:30`, default `gpt-4o-mini`). It is a non-codex reference implementation that no round runs, so it is deliberately not a sync target — noted because D17 claims to be the single answer to "which models will this run use" and the exclusion should be a decision rather than an oversight.

Residual, deliberately not in scope: `codex-sdk.ts` also imports the pure helpers `allUnsure` and `eventUsedTool` from `codex-exec`. Those are genuinely shared logic, not transport, and moving them is the retirement cleanup's job, not this PR's. Noting it so the exec deletion is not a surprise.

**Placement.** In `scripts/ci/qa-nightly-round.sh`, immediately after the cadence gate's `run_round != true` early-exit (the `emit round skipped` block) and before step 3a — so it runs if and only if the round runs, and always ahead of the judge per D13. The round env already carries `LANGFUSE_HOST`/`PUBLIC_KEY`/`SECRET_KEY` (they are explicitly *stripped* from the judge subprocess's env at the `qa-report` call site), so the sync needs no new credential plumbing.

## Non-goals

- **No `codex-exec` plumbing.** The harness migrated to `codex-sdk`; exec is not in use. The stale `QA_JUDGE` default that still pointed at exec IS corrected here, because it is a cost-correctness defect and not a cleanup: nothing in the nightly or the Makefile sets `QA_JUDGE`, so an unconfigured round ran the transport whose event stream reports no cached-input count — pricing every cached token at the full input rate and publishing the overstatement as the round's authoritative cost. Every `QA_JUDGE` fallback now reads one `DEFAULT_JUDGE_KIND` (`codex-sdk`), and exec — still selectable — warns once per process when it produces usage without a cached count. Teaching the exec parser to read cached tokens is deliberately NOT done: it is a retired transport, and the warning covers anyone who opts back into it. (The 34 exec-impl traces still predate the migration.)
- **No trace-granularity change** (per D1) and no change to scores, the `qa-triage` queue, `backfill.ts`, or deep links.
- **No custom Langfuse model definitions** (fact 1) and no price maintenance in the harness — cost stays inferred from the model string so prices live in one place.
- **No `total` usage bucket.** Langfuse derives it from mutually-exclusive buckets; ingesting one invites drift.

## Build steps

1. **`adapter/codex-sdk.ts`** — widen the local `JudgeTurn['usage']` beyond the SDK's `.d.ts` to `{ input_tokens?, cached_input_tokens?, cache_write_input_tokens?, output_tokens?, reasoning_output_tokens? }`, all optional. Thread the new counts out of `verdictsFromTurn` alongside the existing two. Implement D4 (sum across attempts).
2. **`adapter/span.ts`** — three new optional `SpanParams` fields and their attributes, guarded by the same `!== undefined` pattern as the existing pair (`span.ts:85-86`): `gen_ai.usage.cached_input_tokens`, `gen_ai.usage.reasoning_output_tokens`, `qa.usage.cache_write_input_tokens` (harness-namespaced — not a `gen_ai.*` convention).
3. **`export/langfuse.ts`** —
   - `buildTraceBody`: add `pass` to the returned skeleton (or derive at the ship site) and set `usage_attributed` per item; add the `pass:` / `model:` tags to the tag list built at `langfuse.ts:668-670`.
   - `exportSpans`: after the final trace-create for the span's **lowest-`itemIndex`** body, POST one `generation-create` per D1/D2/D5/D6/D7/D8/D9. Skip entirely when the span carries no `input_tokens` (error runs) — an observation with no usage is noise.
4. **`judge/models.ts`** — the four defaults + `activeModels()` (D17); update the six import sites.
5. **`export/model-prices.ts` + `Makefile` target `qa-model-prices`** — fetch, select (D11), diff, guard (D15), POST (D12/D13). Pure `selectUpstream` / `diffPrices` / `guardDelta` functions with a thin `main()`, mirroring `backfill.ts`.
6. **`scripts/ci/qa-nightly-round.sh`** — invoke the sync after the cadence gate, before step 3a, fail-open with a manifest field (D16).
7. **Live verification — mandatory, not optional.** Neither this spec's author nor its reviewer could prove the payload by POST: the instance is a read-only seam from the sandbox (`only GET/HEAD are permitted`, HTTP 403), so the event shape is verified against the deployed server's source and the published OpenAPI but **not** end-to-end. Re-export one round from an environment that can write and confirm: `observations` non-empty, `totalCost > 0`, the cost matching a hand-computed figure from the three buckets and the model's prices, and `startTime` reflecting the judge run rather than the export.

## Tests

`export/langfuse.test.ts` already has the fake-transport harness that counts ingestion calls by type; extend it rather than inventing a second pattern.

- **Bucket arithmetic** — `input` is net of cached; a cached count exceeding input clamps to 0 rather than going negative; reasoning never appears as its own bucket.
- **One observation per span, on the lowest-`itemIndex` trace** — the regression that matters: a **multi-item** span (the case that does not yet occur in production, fact 6) must yield exactly one observation, on `item0`, with the span's full usage; siblings carry `usage_attributed: false`.
- **No usage → no observation** — an error span with no token attributes ships its trace and zero observations.
- **Timestamps come from the span, not the clock** (D8) — the generation's `startTime` matches `span.start_time_unix_nano`, so a re-export months later does not move it.
- **Non-fatal** — an observation POST rejecting must not decrement `result.traces`, must not raise, and must not suppress the enqueue pass.
- **Stable id across re-export** — two exports of the same span produce the same observation id and differing event ids.
- **Tags** — a behavior scenario yields `pass:ux`, an intent scenario `pass:intent`, both alongside the existing `behavior:` / `runId:` / `gitSha:` tags.
- **`adapter/codex-sdk.test.ts`** — canned turns assert the new counts reach the span, that a missing field degrades to `undefined` rather than `NaN`/`0`, and that a tool-rejected retry sums both attempts' usage (D4).

For `model-prices.ts`, against a fake fetch + fake Langfuse transport:

- **Idempotence** (D14, the one that decides whether this ages well) — upstream identical to the instance produces **zero** POSTs; run twice, still zero.
- **The second drift** (D13/D14, the one that decides whether this works *at all* past week one) — with a project-scoped override already present, a further upstream change must DELETE then POST, not 400. Assert the delete precedes the create.
- **Convergence deletes the override** (D13's reconciliation, the row that closes the one-way door) — managed row now matches upstream while our override is still present ⇒ the run DELETEs the override and creates nothing. This is the case that only ever fires after a real Langfuse image upgrade, so it will never be exercised by accident; it has to be a test.
- **Regex selection** (D11) — the `gpt-5.5` case specifically, using the **verbatim** `(?i)`-prefixed upstream pattern: target string `gpt-5.5` selects the entry whose `modelName` is `gpt-5.5-2026-04-23`. A target matching two upstream entries is refused, not guessed; a target matching zero is reported, not skipped.
- **Effective-definition resolution** (D14) — given a managed row and a custom row both matching the target, the diff compares against the custom one (custom before managed, then newest `startDate`).
- **Tier fidelity** (D12) — luna's real two-tier upstream entry round-trips its full `conditions`/`priority`/`isDefault` structure; nothing flattens to `inputPrice`/`outputPrice`; `unit` is sent but never diffed.
- **Guards** (D15) — a 10× price jump aborts that model and passes under `--force`; an HTML error body, a truncated payload, and a price of `0`/negative/missing each abort without writing.
- **Fail-open vs strict** (D16) — a fetch failure exits 0 by default and non-zero under `--strict`, and in neither case writes a partial update.
- **Scope** — only target models are ever POSTed, never the full upstream list.

## Open items

- **An override suspends managed price updates for that model while it exists.** Custom always wins over managed and resolution ignores dates (D13), so a model with a live override stops receiving image-upgrade prices. The reconciliation rule makes this **transient rather than permanent** — the next run deletes the override once managed catches up. The residual risk is narrow and worth naming: if the sync is removed or silently breaks *while an override is live*, that model freezes at its last synced price while un-overridden models keep tracking upgrades. Mitigation is the loud-failure discipline in D15/D16, not additional machinery.
- **Backfilling existing traces reprices them at today's prices.** Per D13, resolution happens at ingestion, so a re-export does not restore the prices that were in force when a round ran — it applies whatever is current. Combined with the missing cached-token counts on pre-migration spans, a backfill produces a figure that is wrong in two directions at once. Decide deliberately; do not treat re-export as a neutral replay. The ceiling is the **86** exporter-produced judge traces that carry usage (all 86 do, `codex-sdk` and `codex-exec` alike); the other 59 traces in the project are hand-rolled `exp-*` / `label-*` / `corpus-*` artifacts the exporter never produced and a re-export will never touch. Mechanically free (re-export upserts and recomputes cost per D6) but the JSONL artifacts must still exist, and pre-migration spans have no cached counts, so their input would price as 100% fresh — an overstatement, not a neutral one. Decide whether a knowingly-high historical figure is better than none before running it.
- **Trace tags merge by union on re-export.** Re-exporting a span after a model change leaves *both* `model:` tags on the trace. Harmless for the experiment as planned (each round is a fresh export), but D6's upsert framing does not extend to tags — they accumulate.
- **Cost per round in the report.** Once observations exist, `make qa-report` could print a round's spend from the Metrics API. Natural follow-up, not in this scope.
