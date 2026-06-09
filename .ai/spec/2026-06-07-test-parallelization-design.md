# Backend Test Parallelization — Design

**Date:** 2026-06-07 (fleshed out 2026-06-08)
**Status:** Design ready for planning.
**Author:** spengrah (brainstormed with Claude)
**Tracking:** gh issue #413
**Prerequisite (met):** `2026-06-07-synthetic-seed-generator-design.md` (D) — synthetic seed toolkit + suite migration, all merged.

## Scope

This is **sub-project (i)** of the broader test-parallelization effort: parallelize the backend Go integration suite by enabling within-run `t.Parallel()` in the long-pole `backend/tests` package, and make concurrent worktree-agent test runs safe by removing the single-shared-template contention point.

Explicitly split out:

- **(ii) E2E parallelization** — deferred until after #380 Track A (which thins brittle E2E and grows the Go/API suite this project parallelizes). Its own spec later. Tracked: #425.
- **Cross-run / parallel-implementation isolation beyond the Go template enabler** — connection/instance ceilings + E2E per-worktree ports/DB/process isolation — moved to `2026-06-08-parallel-implementation-isolation-design.md` (placeholder) + #424.
- **CI matrix sharding** — dropped from scope (see Decisions).

## Background: what already exists (and reframes this work)

The original placeholder for this spec assumed "one shared mutable test DB + global-state assertions block all parallelism." Three merged efforts have since moved that line, so the remaining work is narrower and lower-risk than the placeholder implied:

- **#360** — per-package template-clone isolation (`backend/internal/testdb`). Each `go test` package process gets a fresh, migrated, data-empty clone (`CREATE DATABASE … TEMPLATE personal_crm_test_template`), with `DATABASE_URL` rewritten to it. `go test` runs packages in parallel processes, so **cross-package isolation + parallelism already exist today**.
- **#362** — split the monolithic suite into sub-packages (`tests`, `tests/api`, `tests/river`, `tests/scheduler`, `tests/unit`, plus `internal/google`, `internal/todoist`), parallelizing across processes. Pre-push dropped ~76s → ~60s.
- **D (synthetic toolkit)** — namespace-scoped fixtures + scoped assertions, and `testdb.NewEphemeralClone(t)` (a per-test fresh clone). The placeholder's "DB/schema-per-worker provisioning" open thread is effectively already built — it is clone-per-test on the existing template.

**What's left:** the `backend/tests` package is the remaining long pole — ~80 files / ~432 test functions running **serially** within its single package clone. The dominant unused lever is within-package `t.Parallel()`.

## Decisions (from the brainstorm)

1. **Rollout = audit-then-flip.** Classify all fast-suite files first, scope the gaps, then enable `t.Parallel()` in one coordinated change. Highest confidence, clearest done-criteria, lowest residual flake risk.
2. **Leave River/heavy tests serial** (do not parallelize them now). They are `RequireLongTests`-gated, so they already skip the fast suite (pre-push + PR CI) — parallelizing them buys nothing for the inner loop / PR gate and adds ephemeral-clone complexity. `NewEphemeralClone(t)` + `t.Parallel()` is the recorded future lever if the full/slow suite ever becomes the bottleneck.
3. **Fold the Go cross-run enabler (template-by-content-hash) into this spec.** Small change to `testdb`; makes concurrent worktree agents on divergent-migration branches robust. Independent of the `t.Parallel()` rollout.
4. **Drop CI matrix sharding.** `t.Parallel()` already parallelizes `backend/tests` across one runner's cores (`make test-integration-fast` is a single `go test` invocation, bounded by GOMAXPROCS). A matrix only adds value by adding total cores across runners, at real config cost (Postgres service + template build per shard, result aggregation). Revisit only if a single runner proves insufficient after the flip.

## Component 1 — Within-run parallelism (the audit + flip)

### The audit taxonomy

Classify every fast-suite test in `backend/tests` (and confirm the other integration packages are already safe or trivially convertible) into four buckets:

| Bucket | Definition | Action |
|---|---|---|
| **scoped-safe** | No River client; data already scoped by `syntheticNS(t)` / unique ids | Add `t.Parallel()` as-is |
| **needs-scoping** | Safe in principle but asserts on global state (totals, "X in list", unscoped counts) or reuses fixed identifiers | Scope fixtures + convert assertions to scoped form, then `t.Parallel()` |
| **River/heavy** | Starts a River client or drains the queue | Stay serial (Decision 2). Cannot share the package clone's `river_job` table under concurrency — `TestOnly:true` disables leader election, so concurrent clients steal each other's jobs |
| **inherently-serial** | Touches a process-global (env vars, package-level singletons, the accelerated-time global, fixed ports) | Stay serial; no `t.Parallel()` |

The audit output is a checked-in classification table (file → bucket → action) that defines done.

### The flip

Go schedules non-parallel tests first, then runs all `t.Parallel()` tests concurrently — so mixed packages are correct. Add `t.Parallel()` to the scoped-safe + (newly-scoped) needs-scoping buckets. Serial / River / global tests are simply not marked parallel. Because the River/heavy bucket is `RequireLongTests`-gated, the flip delivers parallelism across exactly the set the fast suite runs.

## Component 2 — Cross-run Go enabler (template-by-content-hash)

**Problem:** there is one shared `personal_crm_test_template`. Concurrent agents in different worktrees on **divergent-migration** branches compute different template content-hashes and contend over that single template — each drop+rebuilds it, and the racing loser's clone creation fails loudly with `"template marker mismatch before clone"` (safe, not corruption, but mutually flaky).

**Change:** name the template by its content hash — `personal_crm_test_template_<hash-prefix>` — so each migration-set keeps its own template and they never contend.

Touches `backend/internal/testdb`:

- Derive the template name from `templateHashFromInputs` (use a 32-hex prefix of the sha256; `personal_crm_test_template_` is 27 chars, within Postgres's 63-char identifier limit).
- Extend `dbNamePattern` to allow `template_[0-9a-f]+`.
- The drop-on-mismatch logic in `ensureTemplate` becomes unnecessary (distinct hash = distinct DB name = no contention).
- Add a **template reaper** — distinct migration-sets now leave templates behind; extend `make test-clean-clones` (or add a sibling `test-clean-templates`) to sweep stale `…_template_*`.

**Bonus:** also removes the template rebuild a single developer eats when switching branches across migration sets (previously-built sets are cached by name).

Independent of Component 1 — can land in parallel.

## Resource tuning

With N concurrent tests each opening a pool (+ harness DBs) against Postgres `max_connections ≈ 100`: bound within-package `-parallel`, keep per-test pool `MaxConns` small in `TestConfig`, and ensure every harness `database.Close()` runs in `t.Cleanup`. Choose `-parallel` so `parallel × pool_max` stays comfortably under the ceiling on both a dev box and `ubuntu-latest`. (The cross-agent connection ceiling — many agents at once — is out of scope here; see #424.)

## Success criteria

- **Phase 0 baseline:** measure the fast-suite wall-clock and the `backend/tests`-package time specifically.
- **Target:** a meaningful reduction on `backend/tests` (core-bounded — realistically ~2–4× on a 4-core runner), validated **flake-free under `-race`, `-count=10`, and `-shuffle=on`**.
- **Done:** every file classified + actioned per the audit table; fast + full suites green; template-by-hash + reaper landed.

## Sequencing (phases)

1. **Measure + audit** → baseline numbers + the classification table.
2. **Scope** the needs-scoping bucket → namespaced fixtures + scoped assertions.
3. **Flip** → `t.Parallel()` on scoped-safe + needs-scoping; verify under `-race` / `-count` / `-shuffle`.
4. **Template-by-hash + reaper** → the cross-run Go enabler (parallelizable with phases 1–3).
5. **Tune** → `-parallel` + pool sizing; re-measure against the target.

## Relationships

- **Successor to the determinism→speed arc** (#360 isolation / #361 flake fix / #362 sub-package split).
- **Builds on D** (synthetic seed toolkit; the namespace primitive + `NewEphemeralClone`).
- **Coordinated with #380** (QA pyramid rebalance): #380 Track A relaxes brittle Playwright assertions and grows the Go/API suite — so build Track A's new handler tests D-backed + scoped from the start, and do (ii) E2E parallelization after Track A. The Go within-run parallelization here is the durable, independent win that does not wait on #380.
- **(ii) E2E parallelization** → #425. **Cross-run / parallel-implementation isolation (things 2/3)** → #424 + `2026-06-08-parallel-implementation-isolation-design.md`.
