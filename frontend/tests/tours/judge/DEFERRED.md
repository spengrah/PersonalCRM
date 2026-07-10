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

- **Real stronger-model draft labels.** `corpus/labels/CON-042.draft.json` is a structural placeholder. The genuine model-drafted critiques come from the manual `label.ts` run above and are committed like the corpus captures. The merge gate is only the CLI's **machinery** (unit-tested with a mocked drafter). **Chosen drafter: codex-exec with a stronger tier** — `label.ts` defaults the drafter to `gpt-5.5`/`medium` (the intent pass's live-validated tier), never the cheap judge defaults; `QA_LABELER`/`QA_LABELER_MODEL`/`QA_LABELER_EFFORT` override (`resolveLabelerDrafter`). The original cross-family choice (a Claude drafter, maximally breaking the "no LLM-generated ground truth" circularity) was considered, prototyped, and consciously **waived by the maintainer (2026-07-10)**: the human correction pass is the ground-truth step, so same-family drafts only pre-fill what the maintainer verifies — and the codex CLI rides the ChatGPT-subscription quota rather than a metered API pool. If the drafts ever start gating anything WITHOUT human correction, revisit the waiver. When the deferred `codex-sdk` transport (below) lands, `QA_LABELER=codex-sdk` slots into the same seam with zero machinery change.
- **Live judge smoke.** `QA_JUDGE=codex-exec bun run tests/tours/judge/eval/run.ts --judge --limit 2` exercises the live `codex exec` adapter over judge-tagged cases; `--repeat N` measures self-consistency. Advisory, never a merge gate.

## Tooling follow-ups

- **`@openai/codex-sdk` judge implementation (D2 ordering inversion).** The arc names the SDK as the eventual primary; PR2 ships `codex-exec` (present in this env) + the `Judge` interface + an `http` stub, but NOT a `codex-sdk.ts` (a hard `@openai/codex-sdk` import would fail `tsc`, and the dep is unvetted). Follow-up: add the devDep + a `codex-sdk.ts` behind the identical `Judge` interface; select via `QA_JUDGE=codex-sdk`. Zero grader change.
- **promptfoo spike (D5).** PR2 ships a ~200-line bun runner (the arc's sanctioned fallback). A later spike decides whether promptfoo fits the per-`then`-item verdict schema without contortion and plausibly serves #379's Venice-provider extractor evals. The corpus/artifact conventions are kept tool-neutral so the spike inherits them.

## Deferred: the v0 human-validated corpus freeze (label-gated)

PR3 grows the corpus toward the v0 size **mechanically** (new tours' synthetic captures + doctored self-labeled + clean cases) and lands the verifiers-only eval end-to-end, but the **full v0 human-validated freeze stays deferred** — exactly like the label-gated metrics above. It needs the maintainer's corrected ground-truth labels for the real/clean/ambiguous cases, the 70/30 frozen dev/held-out split, and the fail-precision-over-held-out bar. Un-defer via the same recipe: draft with a stronger model (`*.draft.json`), the maintainer corrects in place (`*.labeled.json`), and PR4 wires the bar + issue-mode flip. **No human-labeling ask is surfaced by PR3** — everything it ships is mergeable with zero human labels.

_(The DSH-001 in-flight corpus fixture LANDED at the 2026-07-10 curated regen (sourceRun local-20260710T153937Z): DSH-001-clean now carries the mid-redirect capture, so the judge item grades the in-flight state. Note the first live run's judge FAILED DSH-001[1] on that evidence (a bare spinner on an otherwise blank surface) while the case hypothesis stays `pass` — the maintainer's label adjudicates. The PR2 capture-coverage caveats CON-038[0] / CON-040[0] are now toured — the bare-`/contacts` and last-contact-boundary captures land in `contacts.tour.ts` (PR3 follow-up 3), so those then-items are proven, not abstained.)_

## Intent-pass labels (label-gated, same recipe)

The intent pass (`intent-runner.ts`, one judge call per `type: intent` SSOT behavior) is advisory and label-gated exactly like the item-judge residue:

- **Intent fail-precision joins the north star**: fail-precision is measured over the judge's residual item fails AND intent fails, over a human-labeled held-out set. Deferred with the same dependency.
- **Intent-case expectations are hypotheses.** `corpus/intent-cases/*.json` carry `expected_hypothesis` — self-labeled guesses printed for eyeballing under `--judge`, never trusted ground truth and never a merge gate. The maintainer's corrected labels supersede them via the label.ts draft → `*.labeled.json` recipe above (the Claude drafter covers intents too).
- **Live intent smoke**: `QA_JUDGE=codex-exec bun run tests/tours/judge/eval/run.ts --judge` now also prints intent verdicts vs hypotheses; `QA_INTENT_MODEL`/`QA_INTENT_EFFORT` override the pass's stronger-model default (`gpt-5.5`/`medium` — codex-exec adapter only; other adapters keep their own model config). VALIDATED at the 2026-07-10 first live run (codex-cli 0.142.4): `gpt-5.5` is accepted as a model id, and `-i` image attachment reaches the model — intent verdicts cited visual evidence (screenshot-only qualities like tier colors and a blank mid-redirect frame).
