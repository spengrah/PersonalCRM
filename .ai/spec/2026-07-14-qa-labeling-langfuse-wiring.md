# QA agent end-state: Langfuse-native labeling, corpus retirement, experiments

Status: SETTLED DESIGN (discussion of 2026-07-14; supersedes the same-day first draft of this doc — superseded decisions are kept below, marked, because the reasoning that moved them is part of the design). Owner: maintainer.

## Framing decisions (the load-bearing ones)

1. **This is an advisory QA agent, not a production agent.** Occasional false positives are acceptable — a human reads the report, and even in a future autonomous issue-filing mode, an issue is not a fix: implementers exercise discretion to "not fix". Calibration infrastructure sized for a zero-FP production gate (trap families, held-out splits, prompt tuning against traps, fail-precision bars) is overengineering for this system and is dropped.
2. **The verifier lane leaves the QA agent entirely.** Deterministic checks are CI's job: verifier rows migrate to E2E per-domain, in the same commit as their row deletion (standing direction; re-affirmed). The QA agent is judge-only.
3. **Langfuse is the source of truth for judge-related items** — evidence archives, ground truth, experiment baselines. Git keeps code and specs, not evidence.
4. **North star reframed.** The old north star (fail-precision over a frozen labeled held-out set) existed to let the judge earn issue-filing rights. The new north star for an advisory agent: **of the report's fails, how many did the human act on** — accumulated continuously and for free by triaging live fails in a Langfuse annotation queue, not through dedicated label sessions over frozen fixtures.

## End state

- **Git holds:** tours, judge machinery + mocked unit tests, the SSOT behavior/intent specs, and a small trap-mutation config (a few lines: mutation op + target behavior). No capture fixtures, no case files, no label files.
- **E2E/CI holds:** every deterministic behavior check (the migrated verifier lane).
- **Each judge round:** tours produce fresh live captures (gitignored run dir) → judge grades them → report + traces ship to Langfuse (with evidence embedded per the label-trace contract below) → **detection self-test**: N trap mutations applied on-the-fly to the round's own fresh captures, judged, must-fail — "the trap is the mutation, not the fixture," so detection checks ride every round over always-current evidence with zero fixture rot (the frozen-trap `itemIndex` silent no-op found 2026-07-14 is the failure mode this eliminates).
- **Ground truth accumulates as a byproduct:** the maintainer triages live fails in an annotation queue (real / noise, with a why); every triage becomes a scored trace. Unsure adjudications carry a reason that converts to a backlog item (e.g. "blocked on #642") rather than polluting any gate.
- **False-positive traps: deleted** (DSH-011-doctored retires). False-negative (detection) traps survive as transformations only.

## Experiments in the end state

Prototyped 2026-07-14 by the luna experiment (dataset `qa-intent-eval-20260714`, 4 runs; results in the session summary — headline: hold `DEFAULT_INTENT_MODEL=gpt-5.5`; both models 0/2 vs human labels, tied 8/11 self-consistency, 3/11 units flip within-model between repeats, luna ~5× cheaper on identical token volume; the instability finding outweighs the model question).

- **Mint:** snapshot a live run's evidence into a Langfuse dataset on demand (items carry intent + full evidence bundle + screenshots as media — proven by the label-session trace contract). Pinned per experiment, not permanently: mint fresh when freshness matters; old datasets persist for reproducing old conclusions.
- **Ground truth is inherited, not sessioned:** items matching already-triaged traces carry those verdicts as expected output (grows with every report read); trap-transformation items carry constructed must-fail ground truth for free; unlabeled items still serve agreement/stability metrics.
- **Arms** run the real judge path through the raw dataset-run harness (SDK pattern at high fidelity; adopt the `@langfuse/client` experiment runner when productized): one dataset run per configuration — model swap, `luna-3vote` (task fn = 3 calls + majority), prompt variant pinned by git SHA in run metadata. Resumable legs; item scores + run-level scores.
- **Human cost = triaging disagreements only:** items where arms disagree are queued into an annotation queue; adjudications immediately join the ground-truth pool.
- **Decision rule (relaxed):** agreement with baseline + maintainer's call on the disagreement queue + stability across repeats + cost/latency. No precision bar over a tiny frozen set.
- Queued experiment candidates, deliberately AFTER #642 (comparing judges over a monoculture world partly measures known noise): luna 3-vote majority (~$0.85/run vs $1.42 single-shot 5.5 — possibly cheaper AND more stable), gpt-5.6-terra arm (probe the model id on the pinned codex CLI first).

## Sequencing

1. **#642 seed-distribution realism arc** (next code work): varied overdue ages / cadence mix / last-contact spread via the synthetic generators. Kills the judge's loudest recurring noise (DSH-010's monoculture fail every round), makes urgency-tier UI actually exercisable, and un-blocks DSH-010-clean's question. Retire DSH-010-clean from the corpus pending this (its human `unsure` = "blocked on #642").
2. **Verifier→E2E migration arcs, per domain** (same-commit row-deletion rule). The frozen corpus stays load-bearing for the merge gate until this completes.
3. **Hardening prerequisites — land BEFORE labels become Langfuse-only:** backup.sh on a systemd timer; obs tenant graduated into personal-ops IaC (`setup-vps.sh` TENANTS); durable reach (Tailscale Serve / Caddy) including a same-origin answer for the MinIO media URLs (the :9090 coupling breaks any reach story that only fronts :3000).
4. **Corpus retirement stroke:** trap-as-transformation self-test in the judge round; email/phone scrub moves from curation-time into the Langfuse export path (same seam #379's input-minimization needs); delete corpus captures/cases/labels; labels become Langfuse-only.
5. **Judge-as-tenant** (optional, separate decision): a `qa` tenant on stovepipes owning the scheduled reseed→tours→judge→export loop. Auth answer: dedicated codex account/slot, one interactive login at provision, rotation continues tenant-side (never sync `auth.json` from the Mac — two consumers of one rotating refresh token = the known "refresh token already used" burn). Transport (CLI vs codex-sdk) is orthogonal: the SDK uses the same auth store, so subscription auth works through either. Remaining costs are provisioning mechanics (Playwright/chromium on ARM, tenant secrets, timer). Revisit alongside the vps-dev-sandbox contents work.

## Superseded decisions (kept for the record)

- **Hybrid ground truth (Langfuse authors, git snapshot via label-sync)** — decided earlier on 2026-07-14, superseded the same day: the git snapshot existed for the offline eval's label-gated metrics, which are exactly what moves to Langfuse-native accumulation. `label-sync` dies un-productized (the one-off ran once, for the 2026-07-14 session).
- **`label.ts --langfuse` drafter sink as a permanent integration** — the drafter concept dissolves with the corpus: in the end state the judge's own live verdict IS the draft, confirmed or rejected in the triage queue. The drafter machinery stays only until corpus retirement (step 4).
- **Fail-precision over a frozen held-out set as the north star** — replaced by triage-derived usefulness (framing decision 4). The 2026-07-14 experiment demonstrated why: N=2 labeled units (one of them human-`unsure`) cannot gate anything, and `unsure` labels measure evidence quality, not judge quality (all three verdict categories appeared on DSH-010-clean across humans and models). Where ground truth IS used (experiments), `unsure` adjudications are excluded from match denominators.
- **False-positive trap program / prompt calibration (the old "experiment #0")** — dropped per framing decision 1. The observed over-fail on cosmetic doctoring (3 of 4 runtime legs bit DSH-011-doctored) is acknowledged, and the cheaper levers if report noise ever becomes an attention problem are majority-vote arms and verdict-confidence presentation in the report — not trap-tuned prompts.

## What we consciously give up

- Byte-stable evidence in git history (7k-line capture JSONs were ceremony, not value). Longitudinal comparisons now depend on Langfuse dataset persistence — hence the hardening prerequisites in step 3.
- Offline-ness of judge-lane tooling (the deterministic merge gate lives in CI/E2E, which needs no evidence fixtures; everything judge-side may assume Langfuse reachable).

## Label-trace contract (carried forward — applies to live-fail triage traces and experiment items)

The trace/item input must carry the full scenario (`behavior_title`/`given`/`when`/`then_text`/`all_then` or the intent goal), the mutation when doctored, a `screenshot_caveat` on doctored evidence (screenshots always show the undoctored world), and `graded_evidence` — the mutation-applied captures in the prompt's own CAPTURE[n] structure, each entry carrying its `capture_file` name and its own `screenshot` media token (the media gallery does NOT preserve order; a flat screenshots array is unattributable) — so verdicts are verifiable without leaving Langfuse.

## Operational learnings (2026-07-14 sessions — keep)

- Seed with the backend STOPPED — assertion-driven caches (birthday etc.) silently vanish under the River race, and the seed still exits 0.
- `TIME_ACCELERATION` without `TIME_BASE` is a silent no-op server-side but reports `is_accelerated: true` with empty `base_time`, which the frontend renders as "Invalid Date" — set both, base as Unix seconds.
- Langfuse ingestion dedups by EVENT id (not trace id): re-submitting a trace-create with a previously-used event id is silently dropped. Any upserting writer must generate unique event ids per submission. Ingestion is processed async by a worker: verification reads need a settle delay.
- Langfuse media = MinIO presigned URLs on :9090; any reach/tunnel story must cover it or every media op fails with an unhelpful generic connection error.
- Run-level scores (scores with `datasetRunId`) are accepted by the v3.212 API — verify UI rendering; if absent, attach aggregates as run metadata until upgrade.
- The local dev port 3000 collides with the habitual Langfuse forward — any local tours stack should use a non-3000 frontend port.
- Doctored-mutation liveness: applying a mutation MUST change the evidence, else fail loudly (out-of-range `itemIndex` no-ops silently). Made structural by trap-as-transformation over live evidence, but any residual mutation application should keep the deep-inequality guard.
