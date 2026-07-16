# Agentic UX QA — judge, grader, report + corpus

The consumer half of the tours harness. It reads the §1 capture records the tours (`contacts.tour.ts`, `dashboard.tour.ts`, `cadence-followup.tour.ts`) produce (`../support/types` `Capture`) and grades each behavior's spec `then`-items. **Advisory only** — it files no issues and gates no CI beyond its own offline tests.

## The grader (judge residue only)

Each behavior's `then`-items are classified in `grader/classification.ts`, keyed by `(behavior_id, then_index)`. The deterministic **verifier** lane — one pure function per then-item over structured evidence — migrated to cited Playwright E2E specs (`frontend/tests/e2e/*.spec.ts`, see the `// spec:` citations); what remains here is only the **judge** residue: the LLM (`adapter/`) owns the two semantic then-items no deterministic check can prove — **CON-042[0]** ("warns cannot be undone") and **DSH-004[2]** (error-reason faithfulness). (A former third residue item, DSH-001[1] on the redirect's interim presentation, was retired by the maintainer at the first label session — the interim is imperceptibly brief; interim quality stays judgeable holistically under the DSH-011 intent.) Item-judge prompts render per-capture `CAPTURE[n]` sections (in-flight vs settled states stay distinguishable) and attach screenshots all-or-nothing on the codex-exec adapter, mirroring the intent pass.

Aggregation: any item `fail` → behavior `fail`; all `pass` → `pass`; else `unsure`. The **grounding rule** downgrades an uncited `fail` to `unsure` (`grader/grade.ts`).

## The intent pass (judged experience goals)

Sibling of the item-judge residue: one judge call per `type: intent` behavior in the SSOT (`intent-catalog.ts`, a transcription kept YAML-synced by `intent-catalog.test.ts`). Evidence binds via the inverted `serves:` edges — captures tagged with the intent's ID or any serving behavior, deduped, capped at 8 with the dropped count surfaced (`intent-input.ts`). The prompt renders an INTENT block + per-capture `CAPTURE[n]` sections; a `fail` must cite the capture index + node/path, and the preamble forbids failing goals for aria-invisible visual qualities (abstain instead — screenshots are the PR3 follow-up). Verdicts land in the report's **Intents** section: a `current` intent failing is a regression signal, a `proposed` intent passing is a progress signal. The pass runs only under the report CLI's `--judge` (`make qa-report … JUDGE=1`), defaults to a stronger model than the cheap item judge **on the codex-exec adapter only** (`QA_INTENT_MODEL`/`QA_INTENT_EFFORT`, default gpt-5.5/medium; other adapters keep their own model config unless `QA_INTENT_MODEL` is explicitly set), and never blocks — it is advisory. `corpus/intent-cases/*.json` carry self-labeled hypothesis verdicts (see `DEFERRED.md` — labels pending).

**Screenshots (live-only).** Tours record a best-effort viewport screenshot per capture point into the gitignored run dir (`TOURS_SCREENSHOTS=0` disables); the report CLI attaches them as model images (`codex exec -i`) — all-or-nothing per intent (any bound capture missing its screenshot drops ALL images to keep the CAPTURE[n] mapping honest) and codex-exec only — which flips the intent prompt from the aria-only visual caution to visual-grounding-allowed. Intents flagged `visual: true` in the catalog carry an explicit EVIDENCE CAVEAT in the report when judged aria-only. **The committed corpus stays aria-only** — the PII audit can grep JSON, not pixels.

## Rendering the advisory report

The deterministic **verifier merge gate has retired** with the verifier lane — that coverage now lives in the cited Playwright E2E specs (`frontend/tests/e2e/*.spec.ts`, `// spec:` citations). What remains is the advisory report over a tours run dir:

```bash
make qa-report RUNDIR=<run dir>              # advisory report (offline; judge items render as "pending labels")
make qa-report RUNDIR=<run dir> JUDGE=1      # + the live judge over residue items (advisory, needs codex quota)
```

`make qa-report` groups a run's captures by behavior, grades the judge residue, and renders the markdown roll-up + coverage + skip-list. It files no issues; the judge-layer + fail-precision metrics print `N/A — pending human labels` (see `DEFERRED.md`).

**Live exports are scrubbed at the boundary.** When a run ships spans to Langfuse (`make qa-export`, opt-in on `LANGFUSE_*`), the export path scrubs every free-form / env-sourced string (prompt, response, `metadata.error`, `metadata.model`, and every label-trace field below) through `judge/scrub.ts` before the HTTP call — the single live→Langfuse PII chokepoint. The whole `input`/`output` of each trace body is deep-walked, so any new free-form field placed inside them is covered; a new field placed elsewhere without routing through the scrubber is a leak.

### Label-trace contract (Langfuse export)

A shipped trace is self-sufficient for adjudication (spec lines 51–53): a reviewer opening the annotation queue sees the full scenario and evidence without leaving Langfuse. The export **fans one judge span out to ONE trace per graded item** (`judge-<behavior>-<span_id>-item<index>`), each carrying:

- **`input.scenario_item`** — the behavior's `behavior_id`/`behavior_title`/`given`/`when` + THIS item's singular `then_text` + `all_then` (the full then-list for context); or, for an intent trace, the `intent_id`/`title`/`statement`/`status` in place of GWT.
- **`input.graded_evidence`** — the graded captures in the prompt's own `CAPTURE[n]` order, each entry carrying its REAL `capture_file` (threaded from the loader via `CaptureSection.captureFile`) and its OWN `screenshot` media token (attributed by index — the flat media gallery is orderless, so a per-index token is the only faithful attribution). All-or-nothing: when the run degraded to aria-only, every entry's `screenshot` is absent — honest, not a bug.
- **`input.mutation` + `input.screenshot_caveat`** — present ONLY when the evidence was doctored (the trap self-test path, PR3). The caveat is DERIVED from the mutation's presence, never carried independently, so a doctored trace has both and a real trace has neither.
- **`output`** — THAT item's verdict; **`input.prompt`** is kept for fidelity; operational fields (tokens, model, status, `screenshots_expected`/`screenshots_attached`) live in `metadata`.

Both QA adapters (`codex-exec`, `http`) emit this content — `http` is text-only, so its `graded_evidence` carries no screenshots. Ingestion event ids carry a per-submission nonce (trace ids stay stable) so a re-export overwrites rather than being silently dropped.

## Detection self-test (trap-as-transformation)

Every JUDGED round (`make qa-report … JUDGE=1`) runs a built-in detection self-test over the round's OWN fresh captures — the trap is the mutation, not a frozen fixture, so there is zero fixture rot. For each committed trap (`judge/trap-config.ts`): apply a single-point `Mutation` to the round's captures, judge the doctored evidence via a RAW judge, and assert a grounded `fail`. A missed trap, a non-executable trap (its target behavior/item is absent, or the mutation is invisible to the judge-rendered prompt), or a per-trap exception sets a non-zero **exit** (a hard signal, distinct from the advisory verdicts, which never gate). The self-test's notion of "detected" matches production exactly: an UNCITED fail is downgraded to `unsure` by the same grounding rule, so it counts as a MISS, not a catch.

A **liveness guard** compares the judge-RENDERED PROMPT before vs after the mutation (`buildPrompt(base) !== buildPrompt(mutated)`), NOT the raw captures — a mutation the judge lane never projects (e.g. a `set_field`, which the prompt omits) leaves the graded evidence identical and fails LOUDLY as a no-op `error`, never a silent pass.

**Adding / adjusting a trap:** edit `judge/trap-config.ts` — a `{ id, targetBehavior, targetItem, mutation, note }` list, kept to 2–3 to bound per-round judge quota. Two hard constraints (both guarded by `trap-config.test.ts`): the `{targetBehavior, targetItem}` MUST be a judge-graded residue item (`judgeItemsFor` — the residue is `CON-042[0]` / `DSH-004[2]`), and the `mutation.op` MUST be one the judge lane PROJECTS (`blank_dialog` / `set_json_field` / `remove_aria_subtree`, never `set_field`). Validate a new trap against a real tours run dir: `QA_JUDGE_TRACE=/tmp/t.jsonl make qa-report RUNDIR=<dir> JUDGE=1` should show it CAUGHT.

**On a miss, ship the doctored trace as an INDEPENDENT step** — `make qa-report … JUDGE=1 ; make qa-export TRACE=/tmp/t.jsonl` (a `;` sequence, NEVER `&&`): the non-zero exit would short-circuit an `&&` chain and skip the export of the very doctored trace a reviewer needs to diagnose the miss. The doctored spans carry `mutation` + the derived `screenshot_caveat` (the label-trace contract above), so Langfuse shows the doctoring is JSON-only and the pixels show the undoctored world.

## The corpus

- `corpus/captures/{contacts,dashboard,cadence-followup}/*.json` — base capture fixtures, curated from a **local `prod-shaped` sweep**, UUID-mapped + host-redacted by the normalizer, then scrubbed at curation (`corpus/scrub.ts`'s `scrubCapture`: email/phone → `<email:N>`/`<phone:N>`, sharing the scrubber that now lives in `judge/scrub.ts`). Provably synthetic: every contact name carries the `synth-prodshaped-` factory prefix. (Live-run captures are instead scrubbed in the export path, above.)
- `corpus/cases/*.json` — clean cases + doctored cases (single-point mutations from `doctor.ts`) that the labeler drafts over; the deterministic eval they once fed has retired with the verifier lane.
- `corpus/labels/*.draft.json` — draft judge labels (the real stronger-model fill is manual — see `DEFERRED.md`).
- `corpus/pii-audit.ts` — the mechanical P0 gate over ALL committed artifacts: bans raw UUIDs / real-host URLs / emails / phones / secrets and asserts the `synth-prodshaped-` name prefix. Runs as a vitest (`corpus/pii-audit.test.ts`) over the committed tree AND as a CLI: `bun run tests/tours/judge/corpus/pii-audit.ts corpus`.

## Regenerating the captures (local, provably-synthetic sweep)

The committed captures come from a LOCAL `prod-shaped` sweep (staging is not required). Seed a clean world, run the app in the accelerated `testing` frame, and run the tours:

```bash
crm-admin --reset-and-seed --profile prod-shaped --yes    # synthetic prod-shaped seed (synth-prodshaped- names)
# start native Postgres + `go run ./cmd/crm-api` (CRM_ENV=testing, accelerated TIME_*) + `next dev`
TOURS_SEED_PROFILE=prod-shaped TOURS_SKIP_RESET=1 make tours   # runs ALL *.tour.ts against localhost
```

Captures land in `frontend/tests/tours/.runs/<runId>/captures/{contacts,dashboard,cadence-followup}/` (gitignored). Curate the relevant ones into `corpus/captures/<tour>/`, refresh the affected `corpus/cases/*.json` + `PROVENANCE.json`, then run the PII audit (`bun run tests/tours/judge/corpus/pii-audit.ts corpus`). Regeneration is intentionally NOT byte-stable (accelerated timestamps + first-seen id ordinals leak through); the grader keys on semantics, so regenerate rarely and review the diff by eye. Curation step: drop the birthdays capture's incidental `GET /contacts?limit=1000` body — con045 reads the compact `fields.birthdayContacts` projection, not that body, so the full contact list is dead weight (and its truncated `data` vs `meta.pagination.total` would be self-inconsistent). Same treatment applies to the delete-flow after-accept capture's unread `GET /api/v1/contacts` list body (con042 reads only the probe GET, the DELETE, and the url; an unread list body would otherwise inflate every CON-042 judge/labeler prompt, which serializes full capture evidence). The aria tree is NOT trimmed — it is the honest rendered page state.

## Adding a doctored case

> **Superseded for detection.** The live **Detection self-test** above is now the detection safety net (the `Mutation` machinery relocated to `judge/mutation.ts`; the committed traps live in `judge/trap-config.ts`). The corpus doctored-case recipe below survives only to shape the **labeler's** draft evidence and is removed with the rest of the corpus tree in the corpus-retirement terminal step. To add detection coverage, edit `judge/trap-config.ts`, not a corpus case.

With the verifier lane (and its offline merge gate) retired, doctored cases now only shape the **labeler's** draft evidence — there is no longer an automated pass/fail over them, and the advisory report (`make qa-report`) grades live tours run dirs, not the corpus. Pick a **judge**-residue item (the residue is CON-042[0] / DSH-004[2]), add a single-point mutation to a new `corpus/cases/*.json` (`op: inject_query | delete_endpoint | set_aria_disabled | reorder_ids | blank_dialog | remove_aria_subtree | set_field | set_json_field`), and set its `then_index` expected verdict to `fail` (others unchanged). `remove_aria_subtree` drops an aria-rendered node (by role + name/text), `set_field` overwrites an aria-invisible `fields` value (skeleton count, tier class, nav position), and `set_json_field` overwrites a body JSON path. The labeler (`bun run tests/tours/judge/label.ts`) then drafts over the mutated evidence (via `resolveCaseCaptures`), so the draft describes the doctored world.

## Correcting draft labels (deferred, cheap)

Edit `corpus/labels/*.draft.json` in place → `*.labeled.json`, flipping `status: draft` → `human-confirmed` and correcting the verdict/critique. Re-run nothing. See `DEFERRED.md`.

## The mechanical-vs-deferred split

**Mergeable now (zero human labels):** the grader + machinery unit tests, the doctoring tool, the labeling CLI's machinery (mocked drafter), the advisory report, the PII audit. **Deferred (labels/quota):** fail-precision over a held-out set, the error-analysis taxonomy, the real stronger-model draft-fill, the codex-SDK impl, the promptfoo spike. All deferred items are flagged in `DEFERRED.md` — never GitHub issues.
