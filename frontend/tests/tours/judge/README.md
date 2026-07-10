# Agentic UX QA — judge, hybrid grader, eval + corpus

The consumer half of the tours harness. It reads the §1 capture records the tours (`contacts.tour.ts`, `dashboard.tour.ts`, `cadence-followup.tour.ts`) produce (`../support/types` `Capture`) and grades each behavior's spec `then`-items. **Advisory only** — it files no issues and gates no CI beyond its own offline tests.

## The hybrid grader (verifiers before judges)

Each behavior's `then`-items are classified in `grader/classification.ts`, keyed by `(behavior_id, then_index)` (60 items across the 20 current first-cut `ux` behaviors — 7 contacts, 6 dashboard, 7 cadence-followup):

- **verifier** — a pure function over structured evidence (`url` / `apiResponses` / `aria` / `serverTime` / `dialogs` / `fields`). Returns `unsure` (never `fail`) when its evidence is absent, so a missing capture never manufactures a false fail. A **required** mutation absent from a _present_ bracket (e.g. no interaction POST after mark-as-contacted) IS a `fail`. Aria-invisible visual state (loading skeletons, urgency tier, active-nav mark, nav stickiness, redirect spinner) binds to targeted `fields` reads the tour records.
- **judge** — the LLM (`adapter/`) owns only the semantic residue: exactly **CON-042[0]** ("warns cannot be undone"), plus **CON-043[5]** and **DSH-004[1]** as fallbacks when the verifier can't bind (the success wording / the error-reason faithfulness).

Aggregation: any item `fail` → behavior `fail`; all `pass` → `pass`; else `unsure`. The **grounding rule** downgrades an uncited `fail` to `unsure` (`grader/grade.ts`).

## The intent pass (judged experience goals)

Sibling of the item-judge residue: one judge call per `type: intent` behavior in the SSOT (`intent-catalog.ts`, a transcription kept YAML-synced by `intent-catalog.test.ts`). Evidence binds via the inverted `serves:` edges — captures tagged with the intent's ID or any serving behavior, deduped, capped at 8 with the dropped count surfaced (`intent-input.ts`). The prompt renders an INTENT block + per-capture `CAPTURE[n]` sections; a `fail` must cite the capture index + node/path, and the preamble forbids failing goals for aria-invisible visual qualities (abstain instead — screenshots are the PR3 follow-up). Verdicts land in the report's **Intents** section: a `current` intent failing is a regression signal, a `proposed` intent passing is a progress signal. The pass runs only under `--judge` (report CLI `JUDGE=1` / eval `--judge`), defaults to a stronger model than the cheap item judge **on the codex-exec adapter only** (`QA_INTENT_MODEL`/`QA_INTENT_EFFORT`, default gpt-5.5/medium; other adapters keep their own model config unless `QA_INTENT_MODEL` is explicitly set), and never touches the offline merge gate. `corpus/intent-cases/*.json` carry self-labeled hypothesis verdicts (see `DEFERRED.md` — labels pending).

**Screenshots (live-only).** Tours record a best-effort viewport screenshot per capture point into the gitignored run dir (`TOURS_SCREENSHOTS=0` disables); the report CLI attaches existing ones as model images (`codex exec -i`), which flips the intent prompt from the aria-only visual caution to visual-grounding-allowed. Intents flagged `visual: true` in the catalog carry an explicit EVIDENCE CAVEAT in the report when judged aria-only. **The committed corpus stays aria-only** — the PII audit can grep JSON, not pixels.

## Running the eval

```bash
make qa-eval                       # verifiers-only (OFFLINE, deterministic) — the MERGE GATE
QA_JUDGE=codex-exec bun run tests/tours/judge/eval/run.ts --judge --limit 2   # + live judge (advisory, needs quota)
bun run tests/tours/judge/eval/run.ts --judge --repeat 5                       # judge self-consistency
```

`make qa-eval` loads the corpus, applies each doctored case's single-point mutation, runs the grader, and exits non-zero **only** on a verifier regression (a self-labeled doctored `fail` not caught, or collateral on a clean case). It prints the confusion matrix + per-verdict precision/recall + abstention rate over the deterministic classifier. The judge-layer + fail-precision metrics print `N/A — pending human labels` (see `DEFERRED.md`).

## The corpus

- `corpus/captures/{contacts,dashboard,cadence-followup}/*.json` — base capture fixtures, curated from a **local `prod-shaped` sweep**, UUID-mapped + host-redacted by the normalizer, then scrubbed (`corpus/scrub.ts`: email/phone → `<email:N>`/`<phone:N>`). Provably synthetic: every contact name carries the `synth-prodshaped-` factory prefix.
- `corpus/cases/*.json` — clean cases (self-label the grader's deterministic verdicts) + doctored cases (self-labeled by a single-point mutation from `doctor.ts`).
- `corpus/labels/*.draft.json` — draft judge labels (the real stronger-model fill is manual — see `DEFERRED.md`).
- `corpus/pii-audit.ts` — the mechanical P0 gate over ALL committed artifacts: bans raw UUIDs / real-host URLs / emails / phones / secrets and asserts the `synth-prodshaped-` name prefix. Runs as a vitest (`corpus/pii-audit.test.ts`) over the committed tree AND as a CLI: `bun run tests/tours/judge/corpus/pii-audit.ts corpus`.

## Regenerating the captures (local, provably-synthetic sweep)

The committed captures come from a LOCAL `prod-shaped` sweep (staging is not required). Seed a clean world, run the app in the accelerated `testing` frame, and run the tours:

```bash
crm-admin --reset-and-seed --profile prod-shaped --yes    # synthetic prod-shaped seed (synth-prodshaped- names)
# start native Postgres + `go run ./cmd/crm-api` (CRM_ENV=testing, accelerated TIME_*) + `next dev`
TOURS_SEED_PROFILE=prod-shaped TOURS_SKIP_RESET=1 make tours   # runs ALL *.tour.ts against localhost
```

Captures land in `frontend/tests/tours/.runs/<runId>/captures/{contacts,dashboard,cadence-followup}/` (gitignored). Curate the relevant ones into `corpus/captures/<tour>/`, refresh the affected `corpus/cases/*.json` + `PROVENANCE.json`, then run the PII audit + `make qa-eval`. Regeneration is intentionally NOT byte-stable (accelerated timestamps + first-seen id ordinals leak through); the grader keys on semantics, so regenerate rarely and review the diff by eye. Curation step: drop the birthdays capture's incidental `GET /contacts?limit=1000` body — con045 reads the compact `fields.birthdayContacts` projection, not that body, so the full contact list is dead weight (and its truncated `data` vs `meta.pagination.total` would be self-inconsistent).

## Adding a doctored case

Pick a **verifier**-tagged item, add a single-point mutation to a new `corpus/cases/*.json` (`op: inject_query | delete_endpoint | set_aria_disabled | reorder_ids | blank_dialog | remove_aria_subtree | set_field | set_json_field`), and set its `then_index` expected verdict to `fail` (others unchanged). `remove_aria_subtree` drops an aria-rendered node (by role + name/text), `set_field` overwrites an aria-invisible `fields` value (skeleton count, tier class, nav position), and `set_json_field` overwrites a body JSON path. Merge-gating doctored cases mutate a verifier item; a `judge`-item mutation (e.g. `blank_dialog`) is exercised only under `--judge`. Run `make qa-eval` to confirm it's caught with zero collateral.

## Correcting draft labels (deferred, cheap)

Edit `corpus/labels/*.draft.json` in place → `*.labeled.json`, flipping `status: draft` → `human-confirmed` and correcting the verdict/critique. Re-run nothing. See `DEFERRED.md`.

## The mechanical-vs-deferred split

**Mergeable now (zero human labels):** verifiers + machinery unit tests, the eval on doctored self-labeled cases, the doctoring tool, the labeling CLI's machinery (mocked drafter), the advisory report, the PII audit. **Deferred (labels/quota):** fail-precision over a held-out set, the error-analysis taxonomy, the real stronger-model draft-fill, the codex-SDK impl, the promptfoo spike. All deferred items are flagged in `DEFERRED.md` — never GitHub issues.
