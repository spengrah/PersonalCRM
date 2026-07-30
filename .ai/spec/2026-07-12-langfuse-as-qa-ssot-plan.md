# Langfuse as SSOT for the QA judge loop — plan of attack

**Issue:** #635 · **Date:** 2026-07-12 · **Status:** plan, not implemented · **Depends on:** [`2026-07-12-qa-architecture-restructuring.md`](./2026-07-12-qa-architecture-restructuring.md) · **Companion to:** [`2026-07-11-llm-observability-platform-spike-decision.md`](./2026-07-11-llm-observability-platform-spike-decision.md)

> **Superseded on the seed profile (2026-07-30, gh #759).** The `dev` and `prod-shaped` catalog profiles — and the whole invented-distribution layer behind them (bands, quotas, archetypes, margins) — are deleted. There are now exactly two worlds: the declared `standard` world (the default for local dev, staging, the automated staging reseed, and the QA tours) and `minimal-scoped` (an explicit operator override). Historical measurements below were taken against the world that existed at the time and are left as recorded; operational commands and provenance assumptions have been updated to `standard` / `synth-standard-`. See `.ai/patterns/synthetic-seed-toolkit.md` for the current story.

The spike proved the round trip end to end: a draft label was annotated in the Langfuse UI and exported back to git as ground truth. That proof deliberately kept **git as SSOT**, because #635 asked for it. This doc plans the inversion — Langfuse becomes the source of truth for the evidence and the human judgment, and the corresponding files leave git tracking.

## Why this is possible now (and was not last week)

The honest argument *against* moving the corpus out of git was CI determinism: `make qa-eval` is a **merge gate**. It loads the committed captures, applies each doctored case's mutation, runs the grader offline, and exits non-zero on a verifier regression. You cannot put a merge gate's fixtures behind an SSH-forwarded VPS.

But that gate exists to protect the **verifier** lane — and the restructuring decision retires the verifier lane, migrating its deterministic assertions into Playwright E2E specs where they belong. What remains in the harness is the **judge**: the item-judge residue and the intent pass, both of which are **advisory by design** (`judge/DEFERRED.md`: "Advisory only — it files no issues and gates no CI"). Advisory work does not need offline determinism at merge time.

So the sequencing is load-bearing: **the verifier→E2E migration is what frees the corpus.** Do not start Phase 2 below before it lands.

## The rule that decides where a thing lives

> **Git owns what a human authors as code. Langfuse owns what the system produces at runtime and what a human judges in a UI.**

Evidence and judgment are runtime artifacts. Graders, renderers, tours, and the spec are code. Applying that rule:

| Artifact | Today | Author | Proposed home | Note |
|---|---|---|---|---|
| `corpus/captures/**` (51 files, 1.1 MB) | git | tour run (machine) | **Langfuse dataset items** | The bulk. Regenerated per run; already documented as "intentionally NOT byte-stable". |
| screenshots | never committed (`.runs/`, gitignored) | tour run | **Langfuse media** | Already proven in the spike (register → presigned PUT → PATCH finalize). Today they are simply *lost* after a run. |
| `corpus/cases/**` (37), `intent-cases/**` (2) | git | machine + `doctor.ts` mutation | **Langfuse dataset items** | Minus the machinery fixtures — see below. |
| `corpus/labels/**` (7) | git | **human**, in a UI | **Langfuse annotations = SSOT** | The annotation queue *is* the authoring surface. Exporting them back to git was the spike's compatibility shim, not the destination. |
| judge prompt prose (preamble, rubric) | git, inside `judge-input.ts` / `intent-input.ts` | human | **Langfuse Prompt Management** | Versioned, and traces link to the version — so "fail-precision by prompt version" becomes a query. |
| prompt *renderers* (`buildJudgeInput`, `buildIntentJudgeInput`, `buildPrompt`) | git | human | **git** | Code. |
| `grader/classification.ts` | git | human | **git** (much smaller) | Shrinks to judge-ownership only once verifiers leave. |
| `intent-catalog.ts` | git | human (transcribed from `spec/*.yaml`) | **git** | The spec is SSOT; this is a synced transcription with a test that enforces it. |
| `doctor.ts`, tours, verifiers-that-survive | git | human | **git** | Code. |

## The four hard problems, and how each is answered

### 1. Machinery tests still need fixtures

`spec-catalog.test.ts`, `doctor.test.ts`, `intent-catalog.test.ts`, `intent-input.test.ts` and friends load corpus files today. They are unit tests of the *machinery* and must stay offline and hermetic.

**Answer: split the two things git is currently conflating.** Keep a tiny, hand-picked **machinery fixture set** in git (2–3 captures, 1 clean case, 1 doctored case — enough to exercise every `doctor.ts` op and the input builders) and move **the eval corpus** — the 51 captures that exist to be *judged* — to Langfuse. These have always been different objects wearing the same directory. The fixture set is code-adjacent and will barely change; the eval corpus is data and changes every regen.

### 2. The PII gate moves from the repo to the wire

`corpus/pii-audit.ts` is the P0 gate: it greps every committed artifact for UUIDs, real hostnames, emails, phones, secrets, and asserts the seed's `synth-<namespace>-` name prefix (`synth-standard-` on the shipping world). It runs as a vitest over the committed tree. If the corpus leaves the tree, **that gate stops protecting anything** — and worse, the destination is a rented VPS.

Two changes, and the second is the one that matters:

- **The audit becomes a pre-upload gate**, not a pre-commit one. The push tool runs `pii-audit` over the payload and refuses to upload on any finding. Same code, new call site.
- **Pixels cannot be grepped.** The README already flags this ("the PII audit can grep JSON, not pixels") — which is exactly why screenshots were never committed. Sending them to Langfuse re-opens the question. The answer cannot be content inspection; it must be **provenance**: refuse to push a run unless it was seeded from a synthetic profile (`TOURS_SEED_PROFILE=standard` against a `crm-admin --reset-and-seed` world), and record that provenance on the dataset. The captures are *provably* synthetic today because every contact name carries the seed's `synth-<namespace>-` factory prefix; the screenshots are pictures of that same synthetic world. **The invariant to enforce mechanically is "this run never saw real data," not "these bytes contain no PII."** A push tool that will happily upload a run taken against the maintainer's real local CRM is the single most dangerous thing in this plan.

### 3. Reproducing a past eval

Langfuse datasets are mutable, and the spike learned the hard way that **dataset-item IDs stay reserved project-wide even after deletion** — a clean reseed needs fresh IDs. So: **one dataset per corpus revision, named by commit sha** (`qa-corpus-495416a8`, exactly as the spike did), plus a committed `corpus.lock.json` carrying the dataset name, item count, and a sha256 over the pulled snapshot. That is a few hundred bytes in git instead of 1.1 MB, it makes drift loud, and it lets any past eval be re-pointed at the dataset it actually ran against.

### 4. A test harness must not depend on a hand-run SSH tunnel

Right now Langfuse is loopback-only on the VPS, reached through a manual `ssh -N -L 3000`, started by a hand-maintained `up.sh`, backed up by a script nobody has put on a timer. That is fine for a spike and unacceptable as a dependency of the QA loop.

**Phase 2 blocks on the decision doc's action items 1, 2 and 5**: the `obs` tenant graduated into `personal-ops` IaC, durable reach (Tailscale Serve / Caddy loopback) replacing the SSH forward, and backups on a timer with an offsite copy. Until those land, this stays a plan.

## What we get that we do not have today

Not just tidiness — three deferred items fall out of the move:

- **Dataset runs = the luna experiment.** `DEFERRED.md` wants an intent-model swap evaluated against the labeled held-out set (gpt-5.5 vs gpt-5.6-luna, ~5× cost difference on the pass that is ~94% of per-run token cost). That is *precisely* a Langfuse **dataset run**: run the same dataset through two judge configurations, compare scores side by side. The comparison harness we would otherwise hand-roll is the platform's core feature.
- **Fail-precision becomes a live number.** The north-star metric currently prints `N/A — pending human labels`. With annotations as SSOT, it is a query over scores, and it updates the moment a label is entered.
- **Screenshots stop being ephemeral.** Today the intent judge's visual evidence is written to a gitignored run dir and then thrown away — so a verdict can never be re-adjudicated against what the model actually saw. Media in the dataset fixes that permanently, and it is the capability Phoenix could not offer.

## Phases

**Phase 0 — this PR.** Docs only: the decision, the Phoenix runbook, the restructuring plan, this plan. No code.

**Phase 1 — the restructuring (blocking).** Verifier assertions migrate to cited E2E specs; the tours grader keeps only the judged layer. `make qa-eval`'s deterministic merge gate retires with the lane it was protecting. See the restructuring doc.

**Phase 2 — dual-write, prove parity.** Land `qa-corpus-push` (provenance gate → PII audit → dataset + media upload → `corpus.lock.json`) and `qa-labels-pull` (annotations → gitignored cache). The corpus stays in git; the eval keeps reading it. Verify that a pull reconstructs byte-equivalent inputs. Blocks on the obs-tenant hardening above.

**Phase 3 — cut over.** The eval reads the dataset and the annotations (through the local cache). Delete `corpus/captures/**`, `corpus/cases/**`, `corpus/intent-cases/**`, `corpus/labels/**` from git, keeping only the machinery fixtures. Keep a `qa-corpus-export` escape hatch that reconstitutes the whole corpus on disk — the exit cost from Langfuse must stay one command.

**Phase 4 — prompts and experiments.** Judge/intent prompt prose into Prompt Management with a committed fallback string (the harness must still run when Langfuse is unreachable — prompts are the one artifact where an outage would otherwise be fatal rather than merely inconvenient). Then the luna comparison as a dataset run.

## Open questions to settle before Phase 2

1. **Does the eval get to require the network?** The proposal says yes for the judge eval (advisory) and no for the machinery tests (hermetic fixtures). If we later want a judge-layer merge gate — `DEFERRED.md`'s PR4 "issue-mode flip" and the held-out fail-precision bar — that gate would then depend on Langfuse being up. Decide deliberately when PR4 comes, not by accident here.
2. **Do labels round-trip to git at all?** Proposal: no, but `qa-corpus-export` can emit them. The alternative — Langfuse authors, git mirrors on every label — keeps a diffable audit trail of ground-truth changes at the cost of the thing we are trying to remove. Weakly held; the audit-trail argument is real.
3. **Screenshot retention.** Every tour run produces ~50 PNGs (4.2 MB per run in the spike). Uploading every run's media grows MinIO without bound. Retain only runs that back a *dataset revision*, and let ad-hoc runs stay ephemeral.
