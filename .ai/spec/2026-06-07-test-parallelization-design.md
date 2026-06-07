# Test Suite Parallelization — Design (Placeholder)

**Date:** 2026-06-07
**Status:** Placeholder skeleton — direction sketched during the synthetic-seed (D) brainstorm; not yet a full design. Gated on D.
**Author:** spengrah (brainstormed with Claude)
**Tracking:** gh issue #413
**Prerequisite:** `2026-06-07-synthetic-seed-generator-design.md` (D) — the synthetic seed toolkit + its suite migration.

## Scope

Parallelize the backend integration (and E2E) test suite to cut wall-clock time, building on the synthetic seed toolkit (D). Successor to the determinism→speed arc (#360 / #361 / #362), which achieved determinism + a long-pole package split; parallelism is the next speed lever.

This is a **placeholder** — it captures the direction discovered while scoping D so it isn't lost. Flesh out into a full design (its own brainstorm) when the project starts.

## Why this is now possible (the D enabler)

The blocker to parallelism today is the **shared mutable test DB + global-state assertions** — tests assert against totals ("contact X is in the list," "count = N"), so they collide when run concurrently (the whole pollution-gotcha cluster: accumulated state, trigram collisions, list-bounded assertions, the E2E cross-worker import/link candidate problem). D removes that root cause: deterministic, idempotent, fast, **namespaced** fixtures, built to support both isolation modes below.

## Direction (sketched, from the D brainstorm)

Two models, likely both, chosen per test class:

1. **Scoped-shared-DB** — namespaced fixtures + *scoped* assertions (query/filter by the namespace marker, not totals) → safe within-package `t.Parallel()` on one DB. Fits lightweight unit/integration tests. Requires D's suite migration to have converted assertions to scoped form.
2. **DB/schema-per-worker** — D's fast deterministic seed makes N isolated seeded DBs/schemas cheap → true isolation, zero cross-talk. The right fit for **heavy replay / River-draining tests** (which interleave badly on a shared DB) and for **CI sharding**.

D is being built **parallelization-ready**: its isolation primitive supports both prefix-scoping and full per-DB/schema seeding, so this project is a cheap successor rather than a rework.

## Open threads (flesh out when this project starts)

- [ ] Model choice per test class (lightweight → scoped-shared-DB; heavy replay/River → DB-per-worker).
- [ ] DB/schema-per-worker provisioning mechanism (template-DB clone, schema-per-worker `search_path`, or containerized DB-per-shard).
- [ ] River-in-parallel handling (queue/DB isolation per worker).
- [ ] `t.Parallel()` rollout + the assertion-scoping it requires (rides on D's suite migration).
- [ ] CI sharding config (Go test shards across runners; Playwright shard alignment).
- [ ] Resource ceiling (Postgres connection limits / DB instances per box; CI runner sizing).
- [ ] Retire the remaining shared-DB parallelism gotchas (E2E cross-worker candidate collisions, etc.) — overlaps D's gotcha-retirement set.

## Relationship to #380 (QA pyramid rebalance)

#380 (agentic UX QA + behavior SSOT, `2026-05-31-agentic-ux-qa-and-behavior-ssot-design.md`) reshapes the exact surface this project parallelizes — coordinate the two:

- **#380 Track A relaxes/rewrites the brittle Playwright assertions** (~70% are UI-regression brittle) and moves correctness to **API-level / Go tests** (new handler tests for rematch, sync, calendar, todoist, search), thinning E2E to ~8 data-asserting flows. So **this project's parallelization target is mostly the Go integration/API suite Track A *grows*, not the brittle E2E assertions Track A *thins*.**
- **Sequencing:** do the E2E-scoping work **after** Track A's relaxation, so we don't scope assertions #380 is about to delete. The Go/API DB-per-worker parallelization is the durable, independent win and can proceed alongside.
- **Build Track A's new tests parallelization-ready:** the API-level handler tests Track A adds should be D-backed + scoped from the start, so they don't need re-isolation later.
- **Shared convention (D + this + #380):** deterministic seeded scenarios (D) + scoped / data-asserted assertions (here + Track A) that cite **SSOT behavior IDs** (#380 Piece 1). One philosophy, three consumers.

## Dependencies

- **Gated on D** (the synthetic seed toolkit + the migration of the existing integration suite onto its factories).
- Successor to the determinism→speed arc (#360 / #361 / #362).
- Coordinated with **#380** (above).
