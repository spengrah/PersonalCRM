# Deferred work (in-repo flags — NOT GitHub issues)

This advisory arc files **no** GitHub issues. The items below are wired-but-deferred; the `N/A` stubs in the report reference this file.

> **Status update (corpus retirement complete).** The deterministic verifier lane and its offline merge gate retired first — that coverage moved to cited Playwright E2E specs (`frontend/tests/e2e/*.spec.ts`, `// spec:` citations). The frozen git corpus (captures/cases/labels/intent-cases) and the `label.ts` drafter CLI have now been **deleted** — labels are Langfuse-native (the judge's own live verdict is the draft, confirmed or rejected in the annotation queue), detection is the live trap self-test, and the export scrubber is the sole PII chokepoint. What survives here is the advisory judge over the residue (`CON-042[0]` + `DSH-004[2]`) plus the intent pass, rendered by `make qa-report`. The held-out fail-precision bar + issue-mode flip the items below describe as "later" are still deferred — they now sit on the Langfuse-labeled set, not a git corpus.

> **Detection coverage — live, not frozen.** Detection is the trap-as-transformation self-test (`judge/trap-selftest.ts` + `judge/trap-config.ts`, on every `JUDGE=1` round): the trap is a single-point `Mutation` (`judge/mutation.ts` + `doctor.ts`) applied to the round's OWN fresh captures, not a stored fixture, so there is zero fixture rot and the silent-no-op a frozen trap could hide (a mutation the judge never projected) fails loudly via the rendered-prompt liveness guard. To add detection coverage, edit `judge/trap-config.ts` — there is no corpus doctored case any more.

## Label-gated (need the maintainer's corrected ground-truth labels)

These metrics wait for confirmed labels in the Langfuse annotation queue and for a later issue-mode step to gate on them:

- **Fail-precision over a labeled held-out set (the north star).** Measured over the judge's residual `fail`s. Deferred: needs a human-confirmed held-out split.
- **Error-analysis-first failure taxonomy over real captures.** The advisory sweep seeds candidate critiques; the taxonomy is built from the maintainer's confirmed labels.
- **Judge-layer precision/recall vs human ground truth.** Same dependency.

**How to un-defer (cheap, later):**

1. Run `make qa-report JUDGE=1 RUNDIR=<a tours run dir>` with `QA_JUDGE_TRACE` set, then `make qa-export` to ship the judge spans to Langfuse. Each graded item's verdict rides the label-trace contract (scenario + graded evidence + per-capture `capture_file` + screenshot token) — self-sufficient for adjudication in the annotation queue.
2. The maintainer confirms or rejects each verdict in the Langfuse queue. That annotation IS the ground truth — git holds no labels.
3. A later step wires the held-out fail-precision bar + the issue-mode flip on top of those annotations.

## Manual authoring (needs codex quota — not a CI gate)

- **Live judge smoke.** `make qa-report JUDGE=1 RUNDIR=<a tours run dir>` exercises the live `codex exec` adapter over the judge residue items on a real tours run. Advisory, never blocking.
- **The first label session (historical, 2026-07-10).** The maintainer held a label session over `codex-exec:gpt-5.5` drafts; those adjudications reshaped the judge residue and remain the rationale for today's shape: DSH-001's interim-presentation clause was retired (the interim is imperceptibly brief; interim quality stays judgeable holistically under the DSH-011 intent), DSH-004's error heading was reclassified binding-vehicle, DSH-010's wall-uniformity fail was overridden to pass, and DSH-011-doctored was confirmed as a deliberate false-positive check. Those decisions are baked into `grader/classification.ts` + `intent-catalog.ts`; the git label files that once recorded them are gone (labels are Langfuse-native now).

## Tooling follow-ups

- **`@openai/codex-sdk` judge implementation.** The eventual primary transport; today the harness ships `codex-exec` (present in this env) + the `Judge` interface + an `http` stub, but NOT a `codex-sdk.ts` (a hard `@openai/codex-sdk` import would fail `tsc`, and the dep is unvetted). Follow-up: add the devDep + a `codex-sdk.ts` behind the identical `Judge` interface; select via `QA_JUDGE=codex-sdk`. Zero grader change.
- **Intent-model swap experiment: `gpt-5.6-luna` (maintainer-approved, sequenced AFTER labels land).** The intent pass is ~94% of per-run token cost (measured 2026-07-10: gpt-5.5 intents ≈ $1.42/run API-equivalent vs ≈ $0.09 for the gpt-5.4-mini item judge); luna's pricing cuts the intent pass ~5× (≈ $0.28/run). Protocol once a labeled held-out set exists in Langfuse: (1) upgrade the pinned Codex CLI (luna needs it — see codex-exec.ts) and validate the model id live; (2) the held-out comparison — intent fail-precision + verdict agreement of `QA_INTENT_MODEL=gpt-5.6-luna` vs the gpt-5.5 baseline — is part of the deferred labeling milestone, NOT runnable via `make qa-report` today (qa-report renders a live tours run; it cannot score a labeled held-out set); (3) flip `DEFAULT_INTENT_MODEL` only if luna is non-inferior on fail-precision (the gpt-5.5 tier is what caught the CAD-036 class of finding — don't trade that for $1/run without evidence).
- **Name-shape audit over shipped label prose.** The judge's `critique`/`citation` free text ships to Langfuse; the export scrubber (`judge/scrub.ts`) covers email/phone patterns but not name bigrams — a real full-name narrated in critique prose over synthetic data would not be caught. Harmless today (the judge quotes only `synth-prodshaped-`-prefixed evidence) but the channel is live: extend the scrubber with a name-bigram pass at its next touch.
- **promptfoo spike.** The harness ships a ~200-line bun runner. A later spike decides whether promptfoo fits the per-`then`-item verdict schema without contortion and plausibly serves #379's Venice-provider extractor evals. The adapter conventions are kept tool-neutral so the spike inherits them.

## Intent-pass labels (label-gated, same recipe)

The intent pass (`intent-runner.ts`, one judge call per `type: intent` SSOT behavior) is advisory and label-gated exactly like the item-judge residue:

- **Intent fail-precision joins the north star**: fail-precision is measured over the judge's residual item fails AND intent fails, over a human-confirmed held-out set. Deferred with the same dependency.
- **Live intent smoke**: `make qa-report JUDGE=1 RUNDIR=<a tours run dir>` also runs the intent pass over the run's captures; `QA_INTENT_MODEL`/`QA_INTENT_EFFORT` override the pass's stronger-model default (`gpt-5.5`/`medium` — codex-exec adapter only; other adapters keep their own model config). VALIDATED at the 2026-07-10 first live run (codex-cli 0.142.4): `gpt-5.5` is accepted as a model id, and `-i` image attachment reaches the model — intent verdicts cited visual evidence (screenshot-only qualities like tier colors and a blank mid-redirect frame).
