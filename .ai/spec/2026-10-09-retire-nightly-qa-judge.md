# Retire the nightly QA judge; keep intents as the UX spec

## Context & problem

The agentic UX QA system tours staging every night, grades the captures with an LLM judge in two passes (a per-item ux pass with detection traps, and an intent pass over experience goals), exports every verdict to Langfuse, and queues fails and salted passes for human triage. A value audit on 2026-10-09 covered every round since 2026-08-11 and the labeled July rounds. It found that only two intents ever led to an app change, CAD-036 and CON-051, both from July verdicts. Nothing judged since 2026-07-24 produced one. The ux pass judges two items that have never failed on live evidence. It was broken for 30 consecutive rounds without anyone noticing. The triage queue grew to 278 unread items. The harness, its round runner, the Langfuse wiring and their design docs total roughly 25k lines that agents load into context and the maintainer maintains. The judge also flips verdicts on identical evidence, so each finding needs an investigation before anyone can act on it.

What did pay off was judging whole journeys against a stated goal. The intents are worth keeping as the app's high-level UX spec. Grading them every night is not.

## What matters

The maintainer weighs these by size, not rank:

- **No harm to UX.** The deterministic suites (Playwright E2E and Go) are the regression net, and stay exactly as strong.
- **Less code.** Less for the maintainer and agents to maintain and to load into context. This is the largest gain.
- **Freed compute on the VPS.** The nightly round container and judge calls go away. This is a small gain.

## Intent & appetite (delegation charter)

**What the artifact is for.** After this arc, the QA system is a periodic sanity check: the maintainer runs it by hand when they want a pulse on the app, not on a schedule. Nobody attends it between runs. Its output is a local, advisory report. Failure costs little: a missed regression is caught by the deterministic suites or by the maintainer's own daily use, which is how most UX gaps are found today.

**Appetite.** Modest. This is a removal arc with one small keep (the on-demand sanity check) and a small corpus edit. It is worth a few PRs, not a redesign. Retirement is the default for anything nightly-specific. When in doubt about a QA-specific piece, delete it: git history is the archive.

**Not worth investing in:**
- Re-engineering the intent pass. It runs as it does today, minus the nightly-only pieces.
- Scrubbing history. Superseded design docs get a pointer, not a rewrite.
- Building the future computer-use review. Only keep the door open (see Architectural direction).
- Migrating, curating or deleting QA history in Langfuse. It stays as is.

**Rabbit holes:**
- Calibrating the judge: rubric tuning, vote counts, stability work. Instability is accepted for an occasional advisory check.
- Rebuilding traps or a detection self-test in a new form.
- Generalizing the QA Langfuse exporter into a library for extraction before extraction needs it. Keep what is already general; don't design for a consumer that doesn't exist yet.
- Writing tests that prove the removed machinery is gone. A deleted file needs no test. The spec gates (spec-lint, spec-coverage) and the existing suites passing are the proof.

**Call-me-when triggers.** Stop and bring it to the maintainer before:
- changing the synthetic seed or its declared fixtures;
- deleting or weakening any E2E or Go test, or leaving any `ui`/`api` then-item uncovered that was covered before;
- deleting any data, in Langfuse or anywhere else;
- deleting the tours or their support code;
- changing what an intent means beyond what this spec states;
- finding that a piece this spec calls nightly-only is used by something outside the QA system.

**User-reserved decisions:** the classes above, and the tours' fate after the first sanity-check run. When the stated goals collide, use magnitude: a large reduction in code beats a small compute saving, and nothing beats a deterministic test.

**Unattended resolution.** The arc may settle the questions listed under **Deferred to the arc** without asking, by citing this spec, and must record each ruling.

## Goals

- No scheduled QA round runs anywhere, and the repository carries none of the nightly machinery.
- One on-demand command runs the sanity check: fresh tours, then the intent pass, then a local report.
- The spec corpus treats intents as the high-level UX spec, with the trivial and untested ones retired.

## Non-goals

- Fixing the gaps the audits found: #842, #843, #470, #892, and the dashboard-staleness gap.
- Adding new intents. The 2026-10-09 intent-gap audit's candidates belong to the work that builds or fixes their features.
- Changing personal-ops. The qa tenant, its timer, volumes, Codex login and secrets are removed there by its own owner, from the hand-off below.
- Deciding the tours' long-term fate, or building a live-app review agent.
- Any change to Langfuse data, projects or the obs tenant.

## Relation to existing & planned work

- **Supersedes** the nightly design in `.ai/spec/2026-07-19-codex-sdk-judge-transport.md`, `.ai/spec/2026-07-14-qa-labeling-langfuse-wiring.md`, the judge-as-tenant and corpus-retirement work, and the QA parts of `.ai/spec/2026-10-08-langfuse-v4-phase3.md`. That spec's owed post-landing parity check (ruling R6) was met by the 2026-10-09 round (`export_exit=0`, 12 traces, 6 of 6 queued) and the same day's `make qa-cost-acceptance` pass on events_only. No later nightly will run it.
- **Narrows #380** (agentic UX QA umbrella) to an on-demand check. #663 (expand QA coverage to more domains) no longer applies in its nightly form.
- **Keeps a seam for #379** (LLM extraction): `.ai/spec/llm-extraction-program.md` plans to reuse the shared instrumentation (OTel GenAI spans, self-hosted Langfuse). General-purpose pieces stay.

## Hard constraints

- `make spec-lint` and `make spec-coverage` pass. No `ui` or `api` then-item that was covered becomes uncovered.
- Deterministic tests never cite intents. That rule stays.
- Retired behaviors stay as tombstones with their IDs, per `spec/README.md`'s ID lifecycle.
- No new intents, spec domains or prefixes.
- The sanity check needs no Langfuse and no triage queue to run.
- No data is deleted anywhere.

## Architectural direction

- **constraint:** The sanity check is the existing intent pass, run on demand over fresh tour captures, with `gpt-6-luna` as its default model.
- **constraint:** The ux pass (item judge), traps and the trap self-test, the triage queue, labels, salted passes and false-negative backfill, the nightly round script, watermark and cadence gate are removed.
- **leaning:** Intents stay independent of how evidence is gathered, so a computer-use agent on the live app could later consume the same intents. Intent statements and the corpus's definition of an intent carry no tour or capture mechanics. Binding evidence to intents belongs to the sanity check, not the corpus.
- **leaning:** General-purpose code and infra stay: code the extraction program can reuse as is, and infra other work depends on. QA-specific code goes.

## Behavior

**RNQ-1: Nothing runs on a schedule.** The repository contains no nightly round script, watermark, cadence gate or deployed-SHA gate, and no doc, rule or Makefile target describes a scheduled QA round.

**RNQ-2: One command runs the sanity check.** The maintainer runs a single documented command against staging. It runs the tours, then the intent pass over that run's captures, and writes a local, human-readable report. It never gates anything and exits non-zero only when it could not run.

**RNQ-3: No evidence is never a pass.** For each intent, the report gives a verdict with its critique, or says the intent had no bound evidence. An intent with no evidence is never reported as passing.

**RNQ-4: The default model is gpt-6-luna.** The intent pass defaults to `gpt-6-luna` and stays overridable per run.

**RNQ-5: No model grades item behaviors.** The ux pass, its traps and the detection self-test are gone. No `type: ux` behavior is graded by a model.

**RNQ-6: No triage loop remains.** No code enqueues to the annotation queue, writes labels or salts passes, and no command reads the queue. Existing Langfuse data, queues and score configs are left untouched.

**RNQ-7: Deterministic coverage is unchanged.** Every E2E and Go test that existed before the arc still exists and passes, and `make spec-coverage` reports no `ui`/`api` then-item uncovered that was covered before.

**RNQ-8: Intents are the high-level UX spec.** `spec/README.md` defines an intent as a durable, high-level UX goal that briefs the sanity check and feature design, with no reference to a nightly judge. The rest of the corpus documentation matches.

**RNQ-9: DSH-011 and DSH-012 are retired.** Both keep their rows as tombstones. DSH-011's note says it was retired as too trivial to judge. DSH-012's says no tour exercises its goal, so it passed without being tested. No `serves` edge points at a retired intent.

**RNQ-10: Superseded docs point here.** Each `.ai/spec/` design doc that describes the nightly QA system opens with a one-line superseded note linking this spec. Its body is unchanged.

**RNQ-11: No rule describes the nightly.** `AGENTS.md`, `.ai/rules/` and `.ai/guides/` contain no instruction that assumes a nightly judge, a triage queue or traps. Rules about keeping tours and intents in step survive only as far as the sanity check needs them.

## Success criteria

- A fresh agent loading the repository's rules and docs finds no instruction about the nightly QA system.
- The maintainer runs the sanity check once from the documented command and gets a report over every current and proposed intent, each with a verdict or a "no evidence" line.
- The deterministic suites and the spec gates pass with unchanged coverage.
- The repository carries substantially less QA code. The arc's landing summary states the removed line count.

## Desired behavior sketch

The maintainer types one command, waits a few minutes, and reads a short report: one section per intent with pass, fail or unsure and a one-paragraph critique, plus a list of intents with no evidence. Nothing is uploaded, queued or scheduled. If a fail looks real, they file an issue by hand.

## Infrastructure hand-off (personal-ops, not in this arc)

The personal-ops owner removes the qa tenant's scheduled round: its timer (already disabled 2026-10-09), its launch wrapper, state directory, named volumes, Codex login and QA secrets. If the maintainer later runs the sanity check from the VPS rather than the Mac, the minimum the check needs is reach to staging and a Codex login. The obs tenant and Langfuse stay.

## Assumptions & deferred questions

- **Assumed:** the sanity check runs from the maintainer's Mac against staging, as the Langfuse-free replay did on 2026-10-09.
- **Assumed:** removing the nightly needs no change to the synthetic seed. If it does, that is a call-me-when trigger.

**Deferred to the arc:**
- *Which QA code counts as general-purpose.* Candidates: the OTel GenAI span builder, the OTLP/Langfuse exporter's transport, the model-price table with its sync and apply targets, and the cost assertions. If a piece is reusable by the extraction program as is, or is used outside QA, keep it, and move it out of QA-specific paths only if it would otherwise sit inside a deleted directory. If it encodes QA concepts (verdicts, triage, traps, salt, label-trace), delete it. If the price table is kept but no remaining code reads it, keep the table and its targets anyway: Langfuse cost for extraction depends on it.
- *Whether the sanity check keeps any Langfuse export.* Default: none. RNQ-6 and the hard constraints require it to run without Langfuse. If an existing optional export path survives as general-purpose code, the sanity check may leave it unused.
- *What happens to `serves` edges.* Edges that point at kept intents stay. Edges into retired intents are removed. Whether `serves` stays in the corpus schema follows from RNQ-8 and the evidence-independence leaning: keep it if the sanity check still binds evidence through it.

## Engines

```json engines
{
  "authorFamily": "claude",
  "arcPlanner": "claude-opus-5-5:high",
  "arcReview": "gpt-6.1-sol:high"
}
```

The arc is mostly deletion and documentation. Prefer lighter engines for the per-PR work, and save heavier review for the PR that edits `spec/README.md` and the domain files.
