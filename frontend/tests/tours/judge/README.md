# Agentic UX QA — judge, hybrid grader, eval + corpus

The consumer half of the tours harness. It reads the §1 capture records the tours (`contacts.tour.ts`, `dashboard.tour.ts`, `cadence-followup.tour.ts`) produce (`../support/types` `Capture`) and grades each behavior's spec `then`-items. **Advisory only** — it files no issues and gates no CI beyond its own offline tests.

## The grader (judge residue only)

Each behavior's `then`-items are classified in `grader/classification.ts`, keyed by `(behavior_id, then_index)`. The deterministic **verifier** lane — one pure function per then-item over structured evidence — migrated to cited Playwright E2E specs (`frontend/tests/e2e/*.spec.ts`, see the `// spec:` citations); what remains here is only the **judge** residue: the LLM (`adapter/`) owns the two semantic then-items no deterministic check can prove — **CON-042[0]** ("warns cannot be undone") and **DSH-004[2]** (error-reason faithfulness). (A former third residue item, DSH-001[1] on the redirect's interim presentation, was retired by the maintainer at the first label session — the interim is imperceptibly brief; interim quality stays judgeable holistically under the DSH-011 intent.) Item-judge prompts render per-capture `CAPTURE[n]` sections (in-flight vs settled states stay distinguishable) and attach screenshots all-or-nothing on the codex-exec adapter, mirroring the intent pass.

Aggregation: any item `fail` → behavior `fail`; all `pass` → `pass`; else `unsure`. The **grounding rule** downgrades an uncited `fail` to `unsure` (`grader/grade.ts`).

## The intent pass (judged experience goals)

Sibling of the item-judge residue: one judge call per `type: intent` behavior in the SSOT (`intent-catalog.ts`, a transcription kept YAML-synced by `intent-catalog.test.ts`). Evidence binds via the inverted `serves:` edges — captures tagged with the intent's ID or any serving behavior, deduped, capped at 8 with the dropped count surfaced (`intent-input.ts`). The prompt renders an INTENT block + per-capture `CAPTURE[n]` sections; a `fail` must cite the capture index + node/path, and the preamble forbids failing goals for aria-invisible visual qualities (abstain instead — screenshots are the PR3 follow-up). Verdicts land in the report's **Intents** section: a `current` intent failing is a regression signal, a `proposed` intent passing is a progress signal. The pass runs only under `--judge` (report CLI `JUDGE=1` / eval `--judge`), defaults to a stronger model than the cheap item judge **on the codex-exec adapter only** (`QA_INTENT_MODEL`/`QA_INTENT_EFFORT`, default gpt-5.5/medium; other adapters keep their own model config unless `QA_INTENT_MODEL` is explicitly set), and never touches the offline merge gate. `corpus/intent-cases/*.json` carry self-labeled hypothesis verdicts (see `DEFERRED.md` — labels pending).

**Screenshots (live-only).** Tours record a best-effort viewport screenshot per capture point into the gitignored run dir (`TOURS_SCREENSHOTS=0` disables); the report CLI attaches them as model images (`codex exec -i`) — all-or-nothing per intent (any bound capture missing its screenshot drops ALL images to keep the CAPTURE[n] mapping honest) and codex-exec only — which flips the intent prompt from the aria-only visual caution to visual-grounding-allowed. Intents flagged `visual: true` in the catalog carry an explicit EVIDENCE CAVEAT in the report when judged aria-only. **The committed corpus stays aria-only** — the PII audit can grep JSON, not pixels.

## Rendering the advisory report

The deterministic **verifier merge gate has retired** with the verifier lane — that coverage now lives in the cited Playwright E2E specs (`frontend/tests/e2e/*.spec.ts`, `// spec:` citations). What remains is the advisory report over a tours run dir:

```bash
make qa-report RUNDIR=<run dir>              # advisory report (offline; judge items render as "pending labels")
make qa-report RUNDIR=<run dir> JUDGE=1      # + the live judge over residue items (advisory, needs codex quota)
```

`make qa-report` groups a run's captures by behavior, grades the judge residue, and renders the markdown roll-up + coverage + skip-list. It files no issues; the judge-layer + fail-precision metrics print `N/A — pending human labels` (see `DEFERRED.md`).

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

Captures land in `frontend/tests/tours/.runs/<runId>/captures/{contacts,dashboard,cadence-followup}/` (gitignored). Curate the relevant ones into `corpus/captures/<tour>/`, refresh the affected `corpus/cases/*.json` + `PROVENANCE.json`, then run the PII audit (`bun run tests/tours/judge/corpus/pii-audit.ts corpus`). Regeneration is intentionally NOT byte-stable (accelerated timestamps + first-seen id ordinals leak through); the grader keys on semantics, so regenerate rarely and review the diff by eye. Curation step: drop the birthdays capture's incidental `GET /contacts?limit=1000` body — con045 reads the compact `fields.birthdayContacts` projection, not that body, so the full contact list is dead weight (and its truncated `data` vs `meta.pagination.total` would be self-inconsistent). Same treatment applies to the delete-flow after-accept capture's unread `GET /api/v1/contacts` list body (con042 reads only the probe GET, the DELETE, and the url; an unread list body would otherwise inflate every CON-042 judge/labeler prompt, which serializes full capture evidence). The aria tree is NOT trimmed — it is the honest rendered page state.

## Adding a doctored case

With the verifier lane (and its offline merge gate) retired, doctored cases now only shape the **labeler's** draft evidence — there is no longer an automated pass/fail over them, and the advisory report (`make qa-report`) grades live tours run dirs, not the corpus. Pick a **judge**-residue item (the residue is CON-042[0] / DSH-004[2]), add a single-point mutation to a new `corpus/cases/*.json` (`op: inject_query | delete_endpoint | set_aria_disabled | reorder_ids | blank_dialog | remove_aria_subtree | set_field | set_json_field`), and set its `then_index` expected verdict to `fail` (others unchanged). `remove_aria_subtree` drops an aria-rendered node (by role + name/text), `set_field` overwrites an aria-invisible `fields` value (skeleton count, tier class, nav position), and `set_json_field` overwrites a body JSON path. The labeler (`bun run tests/tours/judge/label.ts`) then drafts over the mutated evidence (via `resolveCaseCaptures`), so the draft describes the doctored world.

## Correcting draft labels (deferred, cheap)

Edit `corpus/labels/*.draft.json` in place → `*.labeled.json`, flipping `status: draft` → `human-confirmed` and correcting the verdict/critique. Re-run nothing. See `DEFERRED.md`.

## The mechanical-vs-deferred split

**Mergeable now (zero human labels):** the grader + machinery unit tests, the doctoring tool, the labeling CLI's machinery (mocked drafter), the advisory report, the PII audit. **Deferred (labels/quota):** fail-precision over a held-out set, the error-analysis taxonomy, the real stronger-model draft-fill, the codex-SDK impl, the promptfoo spike. All deferred items are flagged in `DEFERRED.md` — never GitHub issues.
