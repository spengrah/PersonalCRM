# Parallel-Implementation Isolation — Design (Placeholder)

**Date:** 2026-06-08
**Status:** Placeholder skeleton — direction captured during the test-parallelization (i) brainstorm; not yet a full design.
**Author:** spengrah (brainstormed with Claude)
**Tracking:** gh issue #424
**Related:** `2026-06-07-test-parallelization-design.md` (sub-project i — the within-run sibling; ships the Go template-by-hash enabler this spec assumes), `2026-06-07-deploy-and-staging-overview-design.md`, `2026-05-31-agentic-ux-qa-and-behavior-ssot-design.md` (#380), gh issue #425 (sub-project ii — E2E parallelization).

## Problem

Running **multiple concurrent implementations** — e.g. several Claude agents, each on its own git worktree, often coordinated by a dynamic workflow — each wanting to run the test suite without colliding on shared state. This is the **cross-run** axis (N concurrent test runs not colliding), distinct from sub-project (i)'s **within-run** axis (one run using many cores).

## What already works (no build needed)

- **Go integration tests, same-migration branches:** `testdb.SetupPackage` (#360) mints a uniquely-named random clone per `go test` package process and rewrites that process's `DATABASE_URL` to it. Two worktree agents running `make test-integration-fast` already get independent databases per package — provided both branches share the same migration set.
- **Go integration tests, divergent-migration branches:** handled by sub-project (i)'s **template-by-content-hash** change (each migration-set keeps its own template; agents never contend). That enabler ships in #413, so this spec assumes it.

## Gaps this spec covers

### Thing 2 — one Postgres instance = shared ceilings

All clones live in the single `crm-postgres` instance (`max_connections ≈ 100`; each process opens pools + River clients; template/clone CREATE/DROP is serialized by an advisory lock on the shared maintenance DB). N agents × parallel-packages × pool-size can approach the connection ceiling, and clone-mint throughput is capped by the lock.

Open threads:

- [ ] Per-agent connection budgeting (pool sizing × expected concurrent agents vs `max_connections`).
- [ ] Whether to raise `max_connections` / add a pooler (pgbouncer) for the test instance, or cap concurrent agents.
- [ ] Clone-mint throughput under many concurrent agents (advisory-lock serialization).
- [ ] Per-agent Postgres instance (a container per worktree) vs one shared instance with many clones — tradeoffs.

### Thing 3 — E2E / `make dev` is not isolated at all

E2E runs a real backend (:8080) + frontend (:3000) against a shared DB on fixed ports with a live process. Concurrent agents collide on ports, the DB, and the server process.

Open threads:

- [ ] Per-worktree port allocation (backend / frontend / DB) and process management.
- [ ] Per-worktree E2E database (extend the clone idea to the running app's DB, or a per-worktree compose project).
- [ ] Relationship to the **deploy/staging** design (per-environment isolation is the same shape) and to **(ii) E2E parallelization** (#425, which addresses within-run E2E concurrency after #380) — decide whether thing 3 folds into one of those rather than standing alone.

## Dependencies / sequencing

- The Go enabler (thing 1) ships in sub-project (i) (#413); this spec assumes it.
- Thing 3 likely sequences with or folds into (ii) E2E parallelization (#425, post-#380) and/or the deploy/staging work — resolve when this is picked up.
- Lower priority than (i); flesh out into a full design when parallel-implementation workflows become a routine need.
