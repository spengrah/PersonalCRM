# Agentic UX QA — judge, hybrid grader, eval + corpus

The consumer half of the tours harness (Piece 4 · Track B, PR2). It reads the §1 capture records `contacts.tour.ts` produces (`../support/types` `Capture`) and grades each behavior's spec `then`-items. **Advisory only** — it files no issues and gates no CI beyond its own offline tests.

## The hybrid grader (verifiers before judges)

Each behavior's `then`-items are classified in `grader/classification.ts`, keyed by `(behavior_id, then_index)` (23 items across the 7 current contacts `ux` behaviors):

- **verifier** — a pure function over structured evidence (`url` / `apiResponses` / `aria` / `serverTime` / `dialogs`). Returns `unsure` (never `fail`) when its evidence is absent, so a missing capture never manufactures a false fail. A **required** mutation absent from a _present_ bracket (e.g. no interaction POST after mark-as-contacted) IS a `fail`.
- **judge** — the LLM (`adapter/`) owns only the semantic residue: exactly **CON-042[0]** ("warns cannot be undone"), plus **CON-043[5]** as a fallback when the verifier can't bind the success wording.

Aggregation: any item `fail` → behavior `fail`; all `pass` → `pass`; else `unsure`. The **grounding rule** downgrades an uncited `fail` to `unsure` (`grader/grade.ts`).

## Running the eval

```bash
make qa-eval                       # verifiers-only (OFFLINE, deterministic) — the MERGE GATE
QA_JUDGE=codex-exec bun run tests/tours/judge/eval/run.ts --judge --limit 2   # + live judge (advisory, needs quota)
bun run tests/tours/judge/eval/run.ts --judge --repeat 5                       # judge self-consistency
```

`make qa-eval` loads the corpus, applies each doctored case's single-point mutation, runs the grader, and exits non-zero **only** on a verifier regression (a self-labeled doctored `fail` not caught, or collateral on a clean case). It prints the confusion matrix + per-verdict precision/recall + abstention rate over the deterministic classifier. The judge-layer + fail-precision metrics print `N/A — pending human labels` (see `DEFERRED.md`).

## The corpus

- `corpus/captures/contacts/*.json` — base capture fixtures, curated from a **local `prod-shaped` sweep**, UUID-mapped + host-redacted by the normalizer, then scrubbed (`corpus/scrub.ts`: email/phone → `<email:N>`/`<phone:N>`). Provably synthetic: every contact name carries the `synth-prodshaped-` factory prefix.
- `corpus/cases/*.json` — clean cases (self-label the grader's deterministic verdicts) + doctored cases (self-labeled by a single-point mutation from `doctor.ts`).
- `corpus/labels/*.draft.json` — draft judge labels (the real stronger-model fill is manual — see `DEFERRED.md`).
- `corpus/pii-audit.ts` — the mechanical P0 gate over ALL committed artifacts: bans raw UUIDs / real-host URLs / emails / phones / secrets and asserts the `synth-prodshaped-` name prefix. Runs as a vitest (`corpus/pii-audit.test.ts`) over the committed tree AND as a CLI: `bun run tests/tours/judge/corpus/pii-audit.ts corpus`.

## Adding a doctored case

Pick a **verifier**-tagged item, add a single-point mutation to a new `corpus/cases/*.json` (`op: inject_query | delete_endpoint | set_aria_disabled | reorder_ids | blank_dialog`), and set its `then_index` expected verdict to `fail` (others unchanged). Merge-gating doctored cases mutate a verifier item; a `judge`-item mutation (e.g. `blank_dialog`) is exercised only under `--judge`. Run `make qa-eval` to confirm it's caught with zero collateral.

## Correcting draft labels (deferred, cheap)

Edit `corpus/labels/*.draft.json` in place → `*.labeled.json`, flipping `status: draft` → `human-confirmed` and correcting the verdict/critique. Re-run nothing. See `DEFERRED.md`.

## The mechanical-vs-deferred split

**Mergeable now (zero human labels):** verifiers + machinery unit tests, the eval on doctored self-labeled cases, the doctoring tool, the labeling CLI's machinery (mocked drafter), the advisory report, the PII audit. **Deferred (labels/quota):** fail-precision over a held-out set, the error-analysis taxonomy, the real stronger-model draft-fill, the codex-SDK impl, the promptfoo spike. All deferred items are flagged in `DEFERRED.md` — never GitHub issues.
