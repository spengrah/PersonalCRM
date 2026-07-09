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

- **Real stronger-model draft labels.** `corpus/labels/CON-042.draft.json` is a structural placeholder. The genuine model-drafted critiques come from the manual `label.ts` run above and are committed like the corpus captures. The merge gate is only the CLI's **machinery** (unit-tested with a mocked drafter). **Chosen drafter: Claude** — a _different model family_ from the cheap codex judge (`gpt-5.4-mini`), which maximally breaks the "no LLM-generated ground truth" circularity (a codex judge is never graded against codex-drafted labels). Needs a `Judge`-interface adapter (Anthropic endpoint, selected via `QA_LABELER=claude`); until it lands, labeling stays deferred (residue is 3 items).
- **Live judge smoke.** `QA_JUDGE=codex-exec bun run tests/tours/judge/eval/run.ts --judge --limit 2` exercises the live `codex exec` adapter over judge-tagged cases; `--repeat N` measures self-consistency. Advisory, never a merge gate.

## Tooling follow-ups

- **`@openai/codex-sdk` judge implementation (D2 ordering inversion).** The arc names the SDK as the eventual primary; PR2 ships `codex-exec` (present in this env) + the `Judge` interface + an `http` stub, but NOT a `codex-sdk.ts` (a hard `@openai/codex-sdk` import would fail `tsc`, and the dep is unvetted). Follow-up: add the devDep + a `codex-sdk.ts` behind the identical `Judge` interface; select via `QA_JUDGE=codex-sdk`. Zero grader change.
- **promptfoo spike (D5).** PR2 ships a ~200-line bun runner (the arc's sanctioned fallback). A later spike decides whether promptfoo fits the per-`then`-item verdict schema without contortion and plausibly serves #379's Venice-provider extractor evals. The corpus/artifact conventions are kept tool-neutral so the spike inherits them.

## Deferred: the v0 human-validated corpus freeze (label-gated)

PR3 grows the corpus toward the v0 size **mechanically** (new tours' synthetic captures + doctored self-labeled + clean cases) and lands the verifiers-only eval end-to-end, but the **full v0 human-validated freeze stays deferred** — exactly like the label-gated metrics above. It needs the maintainer's corrected ground-truth labels for the real/clean/ambiguous cases, the 70/30 frozen dev/held-out split, and the fail-precision-over-held-out bar. Un-defer via the same recipe: draft with a stronger model (`*.draft.json`), the maintainer corrects in place (`*.labeled.json`), and PR4 wires the bar + issue-mode flip. **No human-labeling ask is surfaced by PR3** — everything it ships is mergeable with zero human labels.

_(The PR2 capture-coverage caveats CON-038[0] / CON-040[0] are now toured — the bare-`/contacts` and last-contact-boundary captures land in `contacts.tour.ts` (PR3 follow-up 3), so those then-items are proven, not abstained.)_
