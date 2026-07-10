# Deferred work (in-repo flags — NOT GitHub issues)

This advisory arc (Piece 4 · Track B) files **no** GitHub issues (arc §4.4). The items below are wired-but-deferred; the `N/A` stubs in the eval + report reference this file. None of these block PR2 merge.

## Label-gated (need the maintainer's corrected ground-truth labels)

The user is unavailable, so PR2 is engineered to be mergeable with **zero** human labels. These metrics wait for labels and for the deferred **PR4** (issue-mode) to gate on them:

- **Fail-precision over a labeled held-out set (the north star).** Measured over the judge's residual `fail`s. Deferred: needs a human-labeled held-out split.
- **Error-analysis-first failure taxonomy over real captures.** The first advisory sweep seeds candidate critiques; the taxonomy is built from the maintainer's corrected labels.
- **Judge-layer precision/recall vs human ground truth.** Same dependency.

**How to un-defer (cheap, later):**

1. Run the labeling CLI to draft labels with a **stronger** model than the runtime judge (breaks the "no LLM-generated ground truth" circularity):
   `QA_LABELER=<stronger-profile> bun run tests/tours/judge/label.ts` → writes `corpus/labels/*.draft.json`.
2. The maintainer edits each `*.draft.json` in place into `*.labeled.json` (flip `status: draft` → `human-confirmed`, correct the verdict/critique). **Re-run nothing** — the corrected file is the ground truth.
3. PR4 wires the held-out fail-precision bar + the issue-mode flip on top of those labels.

## Manual authoring (needs codex quota — not a CI gate)

- **Real stronger-model draft labels.** `corpus/labels/CON-042.draft.json` is a structural placeholder. The genuine model-drafted critiques come from the manual `label.ts` run above and are committed like the corpus captures. The merge gate is only the CLI's **machinery** (unit-tested with a mocked drafter). **Chosen drafter: Claude** — a _different model family_ from the cheap codex judge (`gpt-5.4-mini`), which maximally breaks the "no LLM-generated ground truth" circularity (a codex judge is never graded against codex-drafted labels). The adapter LANDED as `adapter/claude.ts` — a `claude -p --output-format json` CLI transport (like codex-exec; the Claude Code CLI is already authenticated on the dev hosts, so no API key lands in `.env`), selected via `QA_LABELER=claude` (now the label.ts default; `QA_LABELER_MODEL` picks the tier, default `claude-opus-4-8`). The CLI covers item cases AND intent cases. Cost note: `claude -p` bills the metered programmatic pool (June 2026 billing split) — drafting is a deliberate, small batch (~a dozen calls per corpus regen), never CI.
- **Live judge smoke.** `QA_JUDGE=codex-exec bun run tests/tours/judge/eval/run.ts --judge --limit 2` exercises the live `codex exec` adapter over judge-tagged cases; `--repeat N` measures self-consistency. Advisory, never a merge gate.

## Tooling follow-ups

- **`@openai/codex-sdk` judge implementation (D2 ordering inversion).** The arc names the SDK as the eventual primary; PR2 ships `codex-exec` (present in this env) + the `Judge` interface + an `http` stub, but NOT a `codex-sdk.ts` (a hard `@openai/codex-sdk` import would fail `tsc`, and the dep is unvetted). Follow-up: add the devDep + a `codex-sdk.ts` behind the identical `Judge` interface; select via `QA_JUDGE=codex-sdk`. Zero grader change.
- **promptfoo spike (D5).** PR2 ships a ~200-line bun runner (the arc's sanctioned fallback). A later spike decides whether promptfoo fits the per-`then`-item verdict schema without contortion and plausibly serves #379's Venice-provider extractor evals. The corpus/artifact conventions are kept tool-neutral so the spike inherits them.

## Deferred: the v0 human-validated corpus freeze (label-gated)

PR3 grows the corpus toward the v0 size **mechanically** (new tours' synthetic captures + doctored self-labeled + clean cases) and lands the verifiers-only eval end-to-end, but the **full v0 human-validated freeze stays deferred** — exactly like the label-gated metrics above. It needs the maintainer's corrected ground-truth labels for the real/clean/ambiguous cases, the 70/30 frozen dev/held-out split, and the fail-precision-over-held-out bar. Un-defer via the same recipe: draft with a stronger model (`*.draft.json`), the maintainer corrects in place (`*.labeled.json`), and PR4 wires the bar + issue-mode flip. **No human-labeling ask is surfaced by PR3** — everything it ships is mergeable with zero human labels.

_(The DSH-001 in-flight corpus fixture lands at the next curated regen — the live tours already record it; until then the corpus judge hypothesis for DSH-001[1] grades the settled state only. The PR2 capture-coverage caveats CON-038[0] / CON-040[0] are now toured — the bare-`/contacts` and last-contact-boundary captures land in `contacts.tour.ts` (PR3 follow-up 3), so those then-items are proven, not abstained.)_

## Intent-pass labels (label-gated, same recipe)

The intent pass (`intent-runner.ts`, one judge call per `type: intent` SSOT behavior) is advisory and label-gated exactly like the item-judge residue:

- **Intent fail-precision joins the north star**: fail-precision is measured over the judge's residual item fails AND intent fails, over a human-labeled held-out set. Deferred with the same dependency.
- **Intent-case expectations are hypotheses.** `corpus/intent-cases/*.json` carry `expected_hypothesis` — self-labeled guesses printed for eyeballing under `--judge`, never trusted ground truth and never a merge gate. The maintainer's corrected labels supersede them via the label.ts draft → `*.labeled.json` recipe above (the Claude drafter covers intents too).
- **Live intent smoke**: `QA_JUDGE=codex-exec bun run tests/tours/judge/eval/run.ts --judge` now also prints intent verdicts vs hypotheses; `QA_INTENT_MODEL`/`QA_INTENT_EFFORT` override the pass's stronger-model default (`gpt-5.5`/`medium` — codex-exec adapter only; other adapters keep their own model config — revalidate the model id against the pinned Codex CLI's model list at first live run, and confirm the CLI's `-i` image attachment reaches the model on that account tier).
