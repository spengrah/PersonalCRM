# Langfuse v4 Phase 3: QA harness on v4 APIs

Date: 2026-10-08
Status: Draft for arc planning. Implements Phase 3 of the upgrade plan.

## Source of truth

The upgrade plan lives in personal-ops at `.ai/log/plan/langfuse-v4-upgrade.md` (gitignored there). Its Phase 3 section owns the detailed break table, pagination rules, backfill and cost-assertion steps, OTLP exporter rewrite, tests and docs lists, and the parity gate. This spec states the behavior the change must hold, with stable IDs, and does not copy that detail. The identity and usage decisions D1 to D9 of `.ai/spec/2026-07-22-langfuse-usage-cost-tracking.md` still hold and are cited by ID.

## Intent & appetite (delegation charter)

**What the artifact is for.** The QA nightly round (`scripts/ci/qa-nightly-round.sh`) ships judge traces, scores and triage queue items to the obs Langfuse instance, backfills false-negative candidates, and asserts that every judge generation carries cost. The obs instance moves from Langfuse 4.54.0 `dual` write mode to `events_only` (Phase 4, run from personal-ops). Under `events_only` the three legacy paths this repo uses stop working. This spec moves those paths to the v4 APIs and proves parity against the live `dual` instance so the cutover breaks nothing.

**Who attends it and how often.** The nightly round runs unattended at 11:00 Europe/Berlin. Spencer reads its summary line and the queue. Failures in the nightly surface as a round that is not clean, which is the signal to watch.

**What failure costs.** A silent failure mode is the expensive one: a round that exports cleanly but splits traces, prices generations at zero, or drops queue identities. A loud failure (non-zero exit, counted in `FAILED`) is cheap and acceptable. Judge-model errors are unrelated and must not be conflated with Langfuse failures.

**Priorities, in order.**
1. No nightly round breaks while the old exporter still runs during `dual`. The legacy-export guard protects this.
2. The parity gate passes on the live `dual` instance (see Parity below).
3. Cost stays correct per bucket, not only in total.

Where two priorities collide, the higher one wins. Where a choice is cheaper but weakens priority 1 or 3, the choice is rejected.

**Appetite.** This phase is worth the three features, their tests and their docs, delivered in roughly five to six PRs. Hardening beyond the plan's listed tests is disproportionate. So is polish on the QA harness UI and any refactor of the judge that the three features do not need.

**Rabbit holes.**
- The Codex model error `The 'gpt-5.4-mini' model is not supported when using Codex with a ChatGPT account`. It is unrelated to Langfuse. Do not fix it here and do not count it as a Langfuse failure.
- Langfuse behavior beyond what the plan's answered questions and the v4.54.0 source already establish. Probe the live instance for the two open questions only.
- Phase 3b (personal-ops sandbox edge, the Caddy allowlist) and Phase 4 (cutover). Both are out of scope.
- Changing the nightly script's summary format, the clean-round predicate, or the `advance` gate. Every round has ended `round=incomplete` since 2026-10-04 for judge-trap misses, which are not Langfuse failures. Do not gate on `advance=true`.

**Call-me-when triggers.** Bring these to the user instead of judging silently:
- An open question answers badly. Either a re-sent root span leaves two rows that never merge, or the annotation queue UI does not open v4-only traces. Either blocks Phase 4 and changes the design.
- A live write to the `qa-harness` project fails in a way that needs personal-ops infrastructure, including the Phase 3b edge allowlist for `POST /api/public/otel/v1/traces`.
- Test traces accumulate beyond a small volume in the real `qa-harness` project. Tag every test trace `test:`.
- A parity round fails and the cause is not clearly Langfuse, the judge, or a harness defect.
- Any change needed in personal-ops, including the obs infrastructure or the sandbox edge.

**User-reserved decisions.**
- Promotion to `main` (`make promote`). Production promotion always needs the user.
- The Phase 4 cutover to `events_only`, and anything gated on langfuse/langfuse#17506.
- Edits to the engine catalog, which is the user's.
- Anything that spends the user's Codex quota beyond one arc, if a Codex account reports exhaustion, which is a swap decision for the user.

**Merges (user-authorized).** The user has authorized autonomous merges into `develop` once required CI and the approval gate pass for each PR, and once the arc's holistic review passes. Each merge still runs through the arc's normal gates. The user is not asked to approve merges into `develop`.

**Tradeoff rules for a delegate.** When fidelity to the existing judge export contract collides with a cleaner v4 shape, keep the existing contract (D1 to D9, the summary line, score ids, scrubbing) and change the shape underneath it. When a live probe contradicts the v4.54.0 source, trust the live probe and record the discrepancy in the arc.

## Engines

```json engines
{
  "authorFamily": "claude",
  "arcPlanner": "claude-opus-5-5:high",
  "arcReview": "gpt-6.1-sol:high"
}
```

Guidance on engine choice for the arc: the arc planner and the contract reviewer are set above. Other engines are the arc planner's to choose from the catalog. The holistic reviewer must be a non-Claude family, since the spec's author family is Claude. Use the cheaper Codex model only where a downstream reviewer checks its mechanical output.

## Behavior

Each claim below is a spec item the arc must cover with at least one acceptance criterion, or hold in a fog entry until its node lands.

**P3-1: Trace and generation ship over OTLP.** `make qa-export` ships each judge trace and its generation through `POST /api/public/otel/v1/traces` with `x-langfuse-ingestion-version: 4`. `score-create` stays on `/api/public/ingestion`. Media, score-configs and annotation queues are unchanged.

**P3-2: Identity is deterministic.** Each judge string id maps to one hex trace id, one root span id and one generation span id, the same on every export. Start and end times come from the judge span, never from export time. The string id travels as `langfuse.trace.metadata.label_id`. (Derivations are in the plan.)

**P3-3: One root send per trace.** Media registers against the hex trace id before the root span. The root span is sent once, with its tokens spliced in, all-or-nothing as today.

**P3-4: Usage keys are exact.** A generation's `langfuse.observation.usage_details` has exactly three keys: `input` (net of cached), `input_cached_tokens` and `output`, matching the price keys in `infra/langfuse/model-prices.json`. Any other key prices at zero and is a defect. Generation eligibility keeps `spanCarriesUsage`, and the generation's failure stays isolated from its trace (D5).

**P3-5: Legacy-exported traces are refused.** A judge trace the old exporter already shipped is found by its string id in v2 or by the legacy trace endpoint, and is refused, not re-exported under a hex id. Each refusal and each failed lookup counts in `FAILED`, so a round with a refusal never reports clean. Any lookup response other than found or not-found counts as a failed lookup.

**P3-6: Backfill candidates are unchanged.** `make qa-fn-backfill` reads root observations through v2, keys candidates by trace id and never by observation id, and decodes output on the client. For a historic round and a new round it produces the same candidates, queue identities and deep links that the backfill's behavior requires. Score-over-output precedence and ambiguity handling are unchanged.

**P3-7: Cost assertion reads v2 usage.** `make qa-cost-assert` reads generations through v2 with the `usage` field group, and asserts non-zero cost per generation as today. Its pagination ends on an empty `meta` and never requires a limit. A repeated cursor fails rather than loops.

**P3-8: Cost math is proven per bucket.** A live acceptance test exports a fixture generation with known input, cached and output counts, reads it back, and compares each bucket's cost and the total to the prices `model-prices-apply` set. It must fail when the cached key is renamed. This test runs once for the migration and does not gate nightly.

**P3-9: The summary line is unchanged.** `run.ts` keeps its output format, so `scripts/ci/qa-nightly-round.sh`'s summary regex and clean-round predicate need no change.

**P3-10: Scrubbing holds.** Every new attribute carrying free-form text goes through `judge/scrub.ts`, as today (D9).

**P3-11: Re-export is safe.** Re-exporting a trace under the same hex ids replaces its rows once merged, and its start times do not move. Question 1 in the plan settles the merge window. Until then, a re-export is never counted as a second trace.

## Parity

Done means the following, all against the live `dual` instance on project `qa-harness`:

- One full nightly round on the new exporter exports with `export_exit=0`, `ship_failed=0`, `observations_failed=0`, traces above zero, enqueue N/N with zero failed, and `cost_check=ok`. This round runs after the change is merged and deployed to staging, or from the Mac if the user prefers, which the user decides at the call-me-when stage.
- The cost acceptance test (P3-8) passes, and fails under its injected wrong key.
- The user can open the round's queue items as v4 traces in the UI. The arc reports which items.
- `qa-fn-backfill` produces correct candidates, queue identities and deep links for a historic round and a new round.

Verification of Langfuse-backed targets runs through `~/.config/langfuse/qa-make.sh <target>` from the repo root. Test writes use a `test:` tag.

## Infrastructure needs (personal-ops, not in this arc)

- Phase 3b: the sandbox edge (`10.100.0.2`) must allowlist `POST /api/public/otel/v1/traces` before the nightly round can run from the sandbox. Until then, OTLP work runs from the Mac.
- The plan's Phase 3b doc and probe updates are personal-ops work.

## Assumptions & deferred questions

**Assumed:** the `dual` instance stays available for the parity window, and the plan's pre-flight facts hold.

**Deferred to the arc:**
- Whether a re-sent root span replaces the earlier row once merged, and whether reads before the merge show one row or two (plan open question 1). Resolution: a live probe. If it shows two rows that never merge, the dedupe by `(traceId, id)` in backfill is the fallback, and the arc records the decision.
- Whether the annotation queue UI opens a trace that exists only in v4 tables (plan open question 2). Resolution: a live check on a test trace. If it does not open, stop and bring it to the user under the call-me-when trigger.
