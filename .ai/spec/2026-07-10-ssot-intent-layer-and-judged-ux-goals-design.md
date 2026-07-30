# SSOT Intent Layer + Judged UX Goals — Design

**Date:** 2026-07-10
**Status:** Designed; implementation not started
**Author:** spengrah (brainstormed with Claude)
**Parent:** #380 umbrella (`.ai/spec/2026-05-31-agentic-ux-qa-and-behavior-ssot-design.md`); extends Piece 1 (`.ai/spec/2026-07-01-behavior-ssot-design.md`) and Piece 4 (`.ai/spec/2026-07-08-piece4-track-b-agentic-qa-harness-design.md`)

## Problem / Goal

The Piece 4 harness landed with its judgment layer effectively vestigial. Across the 20 toured `ux` behaviors there are 60 `then`-items; the LLM judge owns exactly one outright (CON-042[0]) plus two fallback slots (CON-043[5], DSH-004[1]), and roughly a dozen more items are permanent abstains (provider-seeded state, untourable timing). The judge model was correspondingly pinned to cheap+low (#621) because the residue doesn't warrant more. The root cause is a spec-writing convention, not a harness defect: the SSOT requires every `then`-item to be an independently checkable fact, so for `ux` behaviors all the judgment is spent at spec-writing time — the author decomposes an experience goal into mechanical observables, and "verifiers before judges" then correctly claims everything. Nothing grades the goal the facts serve: whether the surface achieves what it is for, whether the experience regressed.

A second-order cost: the verifiers have re-acquired the coupling Track B was meant to escape. They bind to exact copy and styling (`findByRoleName(aria, 'heading', 'Action Required')`, `'Merge Contacts'`, `fields.activeNavClass`, `tierClass`, `navPosition === 'sticky'`). The spec text is DOM-free but the grader is not — a copy tweak or restyle breaks graders that live far from the UI code, which is the old Playwright brittleness relocated.

This design adds a **judged intent layer**: experience goals as first-class SSOT items, a per-intent judge pass in the harness, screenshot evidence for visual judgment, a re-audit of mechanism-pinning items, and a generalized unbound→judge fallback that restores redesign-survival.

## Current state this design builds on

- **SSOT** at `maturity: reviewed` across 12 domains; schema per `spec/README.md` (types `business-logic | api | ux | invariant | data`; GWT xor `statement`, statement currently invariant-only); Go parser/validator in `backend/internal/spec` + `cmd/spec-lint`.
- **Harness** (#615/#616/#618/#620/#621, all merged): assertion-free tours (`contacts`, `dashboard`, `cadence-followup`) producing capture records (`Capture.behaviors: string[]` tags each capture with the behavior IDs it evidences); hybrid grader (`grader/classification.ts` keyed by `(behavior_id, then_index)`, 60 items); judge adapters (`codex-exec` primary, cheap+low default); advisory report; offline verifiers-only eval as the merge gate (`make qa-eval`); labeling deferred per `judge/DEFERRED.md` (drafter: Claude — different model family from the codex judge).
- **Design-session convention**: design work mints `proposed` ux behaviors; the implementation PR flips them `current`. Intents extend this flow upward in grain.

## Decisions

- **D1 — Intents live in the SSOT as first-class items.** New behavior `type: intent`: a judged experience goal. Uses `statement:` (single string) instead of GWT — the type rule becomes "GWT xor statement; statement for `invariant | intent`". `title` stays the short name; `statement` carries the judgeable claim. IDs come from the domain's normal sequence (no new prefix, no separate numbering). Rationale for SSOT placement: design sessions can mint spec items at multiple grains — start with an intent, add granular behaviors as planning progresses — and the spec items ARE the feature specification.
- **D2 — Linkage via `serves:`.** New optional field, a list of intent IDs, allowed on `ux` behaviors and on intents themselves (a finer intent may serve a broader one — multi-grain refinement). Cross-domain references are legal and immediately necessary (the dashboard's at-a-glance intent in `dashboard.yaml` is served by CAD-026/027/028 in `cadence-followup.yaml`). Lint: every `serves` target must resolve corpus-wide to an existing behavior of `type: intent`.
- **D3 — Status semantics carry over.** `proposed` = aspirational goal not yet achieved (what a design session mints); `current` = the surface achieves this today (judging it is regression detection); `retired` = tombstone. Extend-in-place vs retire-and-mint unchanged: rewording a goal is in-place, reversing it is retire-and-mint.
- **D4 — The judge grades declared intents only.** No open-ended "anything confusing here?" critique pass: every verdict stays anchored to a reviewed SSOT ID, keeping fail-precision measurable. Unknown-unknowns get caught the day someone writes the intent for them — and writing intents is now part of design sessions. (An occasional "intent-mining" pass that drafts proposed intents for human review was considered and deferred; it feeds the SSOT, not the verdict report.)
- **D5 — Harness: a third grader layer with per-intent calls and derived evidence binding.** An `INTENT_CATALOG` transcription (id, title, statement, `servedBy` = the corpus-wide inversion of `serves:`) beside `SPEC_CATALOG`, with a unit-test guard that parses the spec YAML and asserts the transcription matches (IDs, statements, inverted edges). Evidence for intent I = captures whose `behaviors` tag names I directly or names any behavior in `servedBy(I)`, deduped, ordered by tour/seq. `Capture.behaviors` is a free string list, so direct intent tagging already works with zero capture-schema change. One judge call per intent (~10/run at backfill size). Per-intent capture cap (default 8) with the dropped count logged in the report — no silent truncation. Zero bound captures → `unsure` ("no evidence bound"): a freshly minted intent is visibly unjudgeable, not silently absent.
- **D6 — Prompt protocol.** Reuses the labeled-block convention: an INTENT block (id, title, statement) replaces SPEC; evidence renders as `CAPTURE[n]` sections, each carrying the capture's note plus its existing URL/ARIA/API/SERVER_TIME/DIALOGS blocks. Same read-only preamble, categorical output (one verdict: `pass|fail|unsure` + citation + critique), same grounding rule with one addition: a `fail` must cite the capture index along with the aria label or JSON path.
- **D7 — Proposed vs current framing in the report.** Both are graded; a `current` intent failing is a regression signal, a `proposed` intent passing is a progress signal ("consider flipping it current"). The judge is useful on both sides of the is/ought split.
- **D8 — Model knob.** The cheap+low default stays for the item-judge residue. The intent pass gets its own env override beside `QA_JUDGE`, defaulting to a stronger model/effort — semantic goal judgment is the hard task and the call count is small. `--repeat N` self-consistency applies unchanged and matters more here.
- **D9 — Eval and labels.** The offline merge gate (`make qa-eval`) is untouched; intents never enter it. Doctored cases may target intent-relevant evidence (wipe the suggested actions across cards, drop the count header) and run under `--judge` only, with expected verdicts recorded as self-labeled hypotheses, explicitly not trusted ground truth. Real ground truth rides the deferred labeling path (`DEFERRED.md` gains an intents entry; the stronger-model Claude drafter covers intents too). The fail-precision north star becomes: measured over the judge's residual item fails AND intent fails.
- **D10 — Screenshots, live-only in this arc.** Aria+API is a good proxy for information architecture but not for visual experience: aria happily reports elements that are white-on-white, overlapping, clipped, or off-screen, and the harness's `fields:` hacks (`tierClass`, `navPosition`, skeleton counts, `rootSpinnerSeen`) exist precisely because visually-salient state is aria-invisible. Tours therefore capture an optional screenshot artifact per capture point (Playwright, zero model quota) into the run dir; the intent pass attaches screenshots when the adapter supports images (Claude labeler: yes; `codex exec` image attachment: verify at implementation; aria-only fallback otherwise). Intents whose judgment is inherently visual are flagged in the catalog (`visual: true`); an aria-only verdict on a visual intent carries an explicit evidence caveat in the report. **The committed corpus stays aria-only**: the pii-audit can grep JSON, not pixels, so screenshots feed the live judge and the labeling workflow but do not enter git. Live-sweep screenshots are PII-safe by construction (prod-guard + the `synth-<namespace>-` seed prefix — `synth-standard-` on the shipping world).
- **D11 — Re-audit of mechanism-pinning items.** One criterion per item: enumerates genuinely mechanical facts → stays verifier; pins mechanism where the durable intent is looser → reworded at intent level (extend-in-place) and reclassified judge-owned. Known dispositions: DSH-001[1] "a brief loading indicator shows" → "the redirect does not present as broken" (judge-owned, judgeable once screenshots exist; the `rootSpinnerSeen` hack retires). DSH-004[1] splits: error-state presence stays verifier; reason-faithfulness becomes judge-primary (no longer fallback). CAD-026[1] survives as verifier (element presence is mechanical); its at-a-glance quality is owned by the new dashboard intent. The PR sweeps all 60 items against the criterion; only these three are expected to move.
- **D12 — Verifier three-outcome contract.** Verifiers move from pass/fail(/unsure) to `pass | fail | unbound`: `unbound` = anchor not found (structurally absent evidence), distinct from bound-but-contradicted. `unbound` routes the item to the item-judge with the same evidence, replacing the per-item `judgeFallback` flag and most abstentions. This is the redesign-survival fix: when "Add Contact" is renamed, the verifier unbinds and the judge answers "is there an add-contact affordance in the header?" instead of the item going dark. Items where judge routing is meaningless (a missing mutation bracket — no semantics recovers an absent POST) keep an explicit abstain opt-out. Invariants preserved: fallback verdicts obey the grounding rule; the merge gate counts only deterministic verifier outcomes, so `make qa-eval` semantics do not change.

## Schema (spec/README.md changes)

```yaml
- id: DSH-010
  title: The dashboard tells the user who to reach out to, at a glance
  type: intent
  status: current
  statement: A user opening the dashboard can decide who to contact next and how, without clicking into any contact — the surface is scannable, not a wall.
  provenance: [design session 2026-07-10]

- id: CAD-026            # existing ux behavior gains a back-ref
  type: ux
  serves: [DSH-010]
  ...
```

README additions: the `intent` type row (consumer: the agentic judge only); `statement` allowed for `invariant | intent`; the `serves:` field rules (optional; `ux` and `intent` items; targets must be intents; cross-domain legal); one consumer rule — **deterministic tests never cite intent IDs** (`// spec:` markers assert provable facts; intents are judge-only). Mechanical enforcement of that citation rule belongs to Piece 3's scanner; until then it is a documented rule. The maintenance rule needs no change — intents are behaviors in `spec/<domain>.yaml`, so behavior-affecting PRs already owe them updates.

Go changes: `backend/internal/spec` parser + validator (new type enum value, `serves` field, statement-for-intent rule, corpus-wide `serves` resolution) and `cmd/spec-lint` wiring. Piece 3's scanner gains one future obligation (reject citations of intent IDs), recorded there, not built here.

## Backfill (exemplar content, 3 domains)

Roughly 3–4 intents per toured domain (~10 total), drafted in PR 1 and pushed through the same adversarial curation review as the rest of the corpus (Codex at xhigh or a Claude reviewer). Flavor: dashboard — "decide who to contact next and how, without clicking into any contact"; "the dashboard never dead-ends — there is always a next action". Contacts — "destructive actions are deliberate; nothing important is lost by accident"; "moving through contacts keeps list context — the user never loses their place". Cadence-followup — "a contact's page answers 'where do we stand with this person?' without digging"; "managing tasks from the CRM is safe — remote task state is respected". Serving `ux` behaviors gain `serves:` back-refs in the same PR.

## PR sequencing

| PR | Content | Gate |
|----|---------|------|
| 1 | SSOT schema (`type: intent`, `serves:`, parser/validate/lint, README) + backfilled intents for the 3 toured domains | `make spec-lint`, parser unit tests |
| 2 | Harness intent pass, aria-only: `INTENT_CATALOG` + YAML-sync guard, binding, prompt, report Intents section, eval `--judge` intent cases, `DEFERRED.md` entry | vitest offline, `make qa-eval` unchanged |
| 3 | Screenshots: capture-side artifact, multimodal attach, `visual` flags + aria-only caveats | offline tests + live smoke |
| 4 | Re-audit sweep (spec rewording + classification/verifier updates) | `make qa-eval` + spec-lint |
| 5 | Verifier three-outcome contract + unbound→judge routing | vitest + `make qa-eval` |

PR 2 depends on PR 1; PRs 3–5 depend on PR 2; PRs 4 and 5 are independent of each other. Each PR is independently green and mergeable.

## Non-goals

- Open-ended surface critique / intent mining (deferred; would feed the SSOT as drafts, not the verdict report).
- Committed screenshot corpus (blocked on a pixel-capable PII gate; the offline eval stays aria-based and deterministic).
- Deterministic-test citation enforcement for intents (Piece 3 scanner scope).
- Narrow-viewport (mobile) tours — the known mobile nav gap stays a bug outside this arc.
- Issue-mode / autonomous filing — unchanged from Piece 4: advisory until the deferred label-gated bar clears (PR4 of that arc).
