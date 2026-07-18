---
name: measure-dont-assume
description: "On empirical questions (timing, perf, bottlenecks) measure with instrumentation before claiming — never extrapolate and present as fact"
metadata: 
  node_type: memory
  type: feedback
  originSessionId: 575e4100-b13f-4965-ab74-e34c66b54fce
---

The user caught a repeated pattern (across sessions) of stating empirically-answerable claims — E2E run times, worker scaling, "standalone is ~3min", 1-worker baselines — from extrapolation/assumption rather than measurement. This drove wrong inferences about how E2E testing actually performs and eroded trust ("classic junior dev bullshit... taking shortcuts and not doing the work"). Related: a standing dislike of deferring important/hard work.

**Why:** the whole premise of an effort (e.g. gh #425 E2E parallelization) can rest on unverified performance assumptions; if those are guesses, the direction is wrong.

**How to apply:** When a question is empirically answerable — timing, CPU vs wall-clock, parallelism efficiency, which step is the bottleneck — MEASURE it with real instrumentation (Playwright `--reporter=json` for per-test/per-worker timing, `/usr/bin/time -v` for CPU vs wall, explicit phase timers) before stating any conclusion. Never present an extrapolated number as fact; label estimates as estimates. Don't defer the hard measurement work. The measurement matrix that matters for E2E: environment (sandbox / CI / user's Mac) × server mode (`next dev` vs prebuilt standalone) × worker count (1/2/4/...), broken into phases (build, server-start, db-reset, pure test exec). See [[sandbox-test-environment]].
