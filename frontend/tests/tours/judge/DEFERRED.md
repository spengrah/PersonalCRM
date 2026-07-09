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

- **Real stronger-model draft labels.** `corpus/labels/CON-042.draft.json` is a structural placeholder. The genuine model-drafted critiques come from the manual `label.ts` run above and are committed like the corpus captures. The merge gate is only the CLI's **machinery** (unit-tested with a mocked drafter).
- **Live judge smoke.** `QA_JUDGE=codex-exec bun run tests/tours/judge/eval/run.ts --judge --limit 2` exercises the live `codex exec` adapter over judge-tagged cases; `--repeat N` measures self-consistency. Advisory, never a merge gate.

## Tooling follow-ups

- **`@openai/codex-sdk` judge implementation (D2 ordering inversion).** The arc names the SDK as the eventual primary; PR2 ships `codex-exec` (present in this env) + the `Judge` interface + an `http` stub, but NOT a `codex-sdk.ts` (a hard `@openai/codex-sdk` import would fail `tsc`, and the dep is unvetted). Follow-up: add the devDep + a `codex-sdk.ts` behind the identical `Judge` interface; select via `QA_JUDGE=codex-sdk`. Zero grader change.
- **promptfoo spike (D5).** PR2 ships a ~200-line bun runner (the arc's sanctioned fallback). A later spike decides whether promptfoo fits the per-`then`-item verdict schema without contortion and plausibly serves #379's Venice-provider extractor evals. The corpus/artifact conventions are kept tool-neutral so the spike inherits them.

## Capture-coverage caveats (tour follow-ups, surfaced in the advisory report)

The merged `contacts.tour.ts` leaves two then-items only partially captured; the verifiers **abstain** (`unsure`) rather than claim them proven:

- **CON-038[0]** — the tour opens `/contacts?sort=cadence&order=desc` (explicit), proving cadence-ordering in the default-equivalent context but NOT the _implicit_ no-sort default. A bare-`/contacts` capture is the follow-up.
- **CON-040[0]** — only the FIRST boundary (`Previous` disabled) is captured; the last boundary (`Next` disabled at the last contact) is not. A last-contact capture is the follow-up.
