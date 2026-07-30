# Codex SDK judge transport + judge follow-up backlog

Date: 2026-07-19
Status: BUILT (codex-sdk transport, 2026-07-19) + backlog (residual items)

This doc is the SSOT for (a) the planned **Codex SDK judge-transport** work and (b) the residual judge follow-ups. It **replaces `frontend/tests/tours/judge/DEFERRED.md`**, which was deleted: that flat "deferred" list conflated *done*, *decided*, *reframed*, and *never-started* items into one bucket and actively misled readers (it read as if the codex-SDK transport and the luna swap were still open questions when the first is wanted and the second is decided). Items that were already **done** or **decided** are recorded as such here so they are not re-litigated; only genuinely-open work stays in the backlog.

## The judge, grounded (what exists today)

The UXQA judge is **not** an autonomous agent and this work does not make it one. It is a two-stage pipeline: deterministic Playwright **tours** (`run-tours.sh`) drive the deployed staging app and capture a frozen evidence bundle (aria/DOM + network + screenshots per capture point); then `make qa-report JUDGE=1` runs the **judge** — one LLM call per residue item / per intent, evidence in the prompt, returning a structured verdict — plus the intent pass and the trap self-test. The brain sits behind a stable `Judge` interface (`adapter/types.ts`: `Judge = (input: JudgeInput) => Promise<PerItemVerdict[]>`) with adapters `codex-exec` (default, spawns the `codex` CLI) and an `http` stub. Verdicts flow through `qa-export` to Langfuse (GenAI spans + `verdict`-as-score + enqueue into the `qa-triage` annotation queue).

## Planned work: Codex SDK judge transport

**Goal.** Add `@openai/codex-sdk` as a third `Judge` adapter (`adapter/codex-sdk.ts`) and enable `QA_JUDGE=codex-sdk`. It is the LLM **brain/kernel** of the harness. This is a **like-for-like transport swap** of the `codex-exec` adapter — single-shot judgment, **zero grader change** — NOT an agentic re-architecture.

**Explicit non-goals (scope guard).** No multi-turn agent loop. No browser actuation (navigate/click/type) — tours still drive the browser; the judge still only reads captures. No grader/prompt/schema change. The SDK is chosen because it is the programmatic door to the same Codex engine and the substrate we could *grow into* tool-use/agency later **if** needed — that optionality is a free property, not scope being built now.

**Parity points — verified against the TS SDK source (`github.com/openai/codex` `sdk/typescript`), not assumed:**
- **Structured output → supported.** `thread.run(prompt, { outputSchema })` takes a plain JSON-Schema object. Maps directly to `OUTPUT_SCHEMA` (replaces the exec adapter's `--output-schema <file>`).
- **Image attachment → supported.** `thread.run([{ type: "text", text }, { type: "local_image", path }])`. Maps directly to the intent pass's screenshots (replaces `codex exec -i <file>` per image).
- **Tool-use / D4 pure-criticism rule → supported.** The run result exposes `turn.items`; detect tool/command items with the same `TOOL_EVENT_MARKERS` logic the exec parser uses (`codex-exec.ts` `eventUsedTool`), re-run once, else all-unsure. Sandbox pinned read-only via Codex `config`.
- **Model / reasoning-effort / usage → via Codex `config` + result usage.** Same engine keys as the CLI's `-c` (e.g. `model_reasoning_effort`); usage from streamed `event.usage` / the turn result. **Pin the exact field/option names against the installed package's `.d.ts` at build time — do not guess them.**

**Build steps (all landed 2026-07-19; `@openai/codex-sdk@0.144.6`).** Live smoke passed — the real SDK returned a schema-constrained, grounded verdict on the default cheap model in ~8s.
1. `bun add -d @openai/codex-sdk` (devDep — test-harness only), version pinned. Adding the dep is what unblocks `tsc` (the sole reason this was ever deferred: a hard import of a missing dep failed the typecheck).
2. New `adapter/codex-sdk.ts` → `makeCodexSdkJudge(opts): Judge`, **reusing every pure piece** from the exec path: `buildPrompt`, `OUTPUT_SCHEMA`, `parseVerdicts`, `allUnsure`, the verdict-normalization map, and `buildGenAiSpan`/`appendSpan` + `buildScenario`/`buildGradedEvidence`. Only the transport differs: `thread.run(entries, { outputSchema })` instead of the `spawn`, and `impl: 'codex-sdk'` on the span.
3. Flip the `throw` in `adapter/index.ts` → `case 'codex-sdk': return makeCodexSdkJudge(model ? { model } : {})`.
4. Tests mirroring `codex-exec`'s injectable-`run` seam: canned turn results exercise parse / tool-rejection / span logic as pure unit tests; the live SDK call stays a thin wrapper (manual smoke, like exec's).
5. Update the `codex-sdk` deferral test (`adapter/index.test.ts`) to assert the built behavior instead of the throw.

**Model note.** Item judge = `gpt-5.4-mini`/`low` (cheap, per spec: "cheap model judges"); intent pass = `gpt-5.5`/`medium`. `gpt-5.6` is **not** adopted (see backlog: luna). The SDK adapter keeps these same defaults.

## Residual backlog (migrated from DEFERRED.md, correctly classified)

**Label-gated metrics — substrate BUILT + DEPLOYED; blocked only on triage volume.** The three metrics still print `N/A` in the report, but every input now exists (arc #684/#685/#688, provisioned live against obs 2026-07-19): `verdict` emitted as a bound Langfuse score, `ground_truth`/`disposition` score configs, the standing `qa-triage` queue, enqueue + salt-passes, and the `qa-fn-backfill` recall CLI. What remains is the small "read the labels back and divide" step + the Phase-1 issue-mode flip gated on it:
- Fail-precision over the labeled set (the north star — note: reframed from a *frozen held-out split* to *disposition-based triage usefulness* per `2026-07-14-qa-labeling-langfuse-wiring.md` decision 4).
- Judge-layer precision/recall vs human ground truth (both operands already emitted trace-by-trace; only aggregation left).
- Intent fail-precision (same substrate; already covers intents uniformly).

**Failure taxonomy — deliberately emergent, not blocked-by-tooling.** `failure_mode` is intentionally kept as free-text comments until categories stabilize over ~20–30 whys, then formalized into a categorical score. Waits on triage volume, by design.

**luna intent-model swap — DECIDED (hold gpt-5.5), not open.** The experiment ran 2026-07-14 (`qa-intent-eval-20260714`): luna ~5× cheaper but less self-consistent; verdict = **hold `DEFAULT_INTENT_MODEL=gpt-5.5`**. Future arms (`luna-3vote` majority-vote, `gpt-5.6-terra`) are queued as candidates, sequenced after #642 — revisit only against a labeled held-out comparison, do not flip without evidence (gpt-5.5 is what caught the CAD-036 class of finding).

**Scrubber name-bigram pass — OPEN (low urgency).** `judge/scrub.ts` scrubs email/phone (`pii-patterns.ts`) but not name bigrams. Harmless today (the judge quotes only `synth-<namespace>-`-prefixed synthetic evidence) but the channel is live; extend the scrubber with a name-bigram pass at its next touch.

**promptfoo spike — OPEN, likely superseded.** Nothing built. Direction has shifted toward Langfuse dataset-runs (`2026-07-14-qa-labeling-langfuse-wiring.md`) as the eval-harness plan; the adapter conventions are kept tool-neutral so either could inherit them. Low priority.

**`http` judge adapter — interface stub, retained.** `adapter/http.ts` is a text-only stub behind the `Judge` interface (no `QA_JUDGE_HTTP_URL` default). Kept as the non-codex reference implementation.

## Done — do NOT re-litigate

- **Live judge smoke** (`make qa-report JUDGE=1`) — works; run live end-to-end 2026-07-16 (run `20260716T194058Z`).
- **First label session** (2026-07-10, #631/#632/#634) — baked into `grader/classification.ts` (residue = CON-042[0] + DSH-004[2]) + `intent-catalog.ts`.
- **Live intent smoke** — validated 2026-07-10 (codex-cli 0.142.4, gpt-5.5, `-i` images reach the model); re-run 2026-07-16.
- **The whole export/triage substrate** — `qa-langfuse-setup` (score configs + `qa-triage` queue), `qa-export` (spans + verdict-score + enqueue + salt), `qa-fn-backfill` — built, merged, and provisioned live against obs.
- **Corpus retirement / verifier→E2E migration / trap-as-transformation self-test** — complete.

## Sequencing

Per maintainer decision (2026-07-19): the Codex SDK transport is built **before** the personal-ops QA-runner-tenant provisioning (#53 host glue) — it is the kernel of intelligence the provisioned runner will execute.

## References

- `frontend/tests/tours/judge/` — adapters (`codex-exec.ts`, `http.ts`, `index.ts`, `types.ts`, `prompt.ts`, `span.ts`), `report/render.ts`, `README.md`.
- `.ai/spec/2026-07-14-qa-labeling-langfuse-wiring.md` — settled advisory-agent + label-trace-contract design (luna result, north-star reframe, transport orthogonality).
- `.ai/log/plan/judge-as-tenant-automation-brief.md` — settled provisioning/roadmap decisions (Phase 0 advisory → Phase 1 auto-issues → Phase 2 auto-fix).
- `.ai/log/progress/qa-nightly-runner.md` — runner + #53 progress log.
