# D — Synthetic Seed Generator — Design

**Date:** 2026-06-07
**Status:** Design — filled out this session (grounded by a code-explorer pass over the sync/ingestion architecture). Implementation-detail threads remain.
**Author:** spengrah (brainstormed with Claude)
**Parent:** `2026-06-07-deploy-and-staging-overview-design.md`

> **Superseded on the seed profile (2026-07-30, gh #759).** The `dev` and `prod-shaped` catalog profiles — and the whole invented-distribution layer behind them (bands, quotas, archetypes, margins) — are deleted. There are now exactly two worlds: the declared `standard` world (the default for local dev, staging, the automated staging reseed, and the QA tours) and `minimal-scoped` (an explicit operator override). Historical measurements below were taken against the world that existed at the time and are left as recorded; operational commands and provenance assumptions have been updated to `standard` / `synth-standard-`. See `.ai/patterns/synthetic-seed-toolkit.md` for the current story.

## Scope

A generator that produces **production-shaped, PII-free, deterministic** synthetic data for the CRM. It is two things at once:

1. **A data source** for staging (spec C) and a richer local `make dev`.
2. **A shared synthetic-data toolkit** reused across unit, integration, and E2E tests.

Its defining capability — and the hard part — is that it doesn't just write terminal domain rows; it can inject **synthetic sync-source inputs** (fake Gmail/GChat/Telegram/Calendar/Mac-Contacts/iMessage/Todoist data) and **replay them through the real ingestion pipeline** (provider normalization → matching → dedup → event bus → consumers → downstream graph), so the full sync flow is exercised without live credentials.

## Architecture: a library-first, layered toolkit

The single biggest design decision: D is a **library first, entrypoints second**. The core lives in a Go package (e.g. `backend/internal/synthetic`) callable directly from Go tests; `crm-admin --seed`, the existing `/seed/*` routes, and the E2E `TestAPI` helpers all become thin wrappers over it. This supersedes/absorbs today's `/seed/*` + `test-api.ts` prior art rather than living beside it.

Four layers, and each test level reuses a *different* one:

| Layer | What it is | Reused by |
|---|---|---|
| **Factories** | deterministic constructors for each entity *and* each synthetic source payload | **unit** (factories only — replay is too heavy here), integration, E2E, seed |
| **Replay adapters** | per-source injectors that feed synthetic input through the *real* ingestion pipeline | **integration** (this *is* the sync integration-test harness), seed, staging |
| **Scenario catalog** | named bundles = edge cases + production-shaped distributions | **E2E** (named scenarios), staging, QA harness (#380) |
| **Entrypoints** | `crm-admin --seed`, `/seed/*` routes, Go test helpers | staging/local, CI |

There is **no single uniform injector** — the grounding pass found three ingestion topologies (scheduler-pull, HTTP-push, persistent-MTProto), so the replay layer is a small **per-source adapter set**, each wrapping that source's existing seam.

## Three data shapes — from one mechanism

The dataset must contain three states, and the key finding is that they fall out of **whether a synthetic input's sender matches a seeded contact** — not three separate code paths:

1. **Settled** — fully-processed domain graph. Either replayed (sender matches a seeded contact → matched interaction) or direct-written for bulk volume.
2. **Flowed-through** — synthetic source inputs replayed through the real pipeline; you see the real pipeline's *output* and have tested it.
3. **Pending / unprocessed** — replay a synthetic input from an **unknown** sender and you land naturally in the pending states that drive major UX surfaces (Imports queue, rematch, needs-review). No hand-construction required.

## Replay-default, direct-write as a fast-path profile

- **Replay is the default** (for staging + showcase fidelity). It derives `last_contacted` / `contact_by` / cadence / interactions *correctly* via the real consumers, and exercises matching + the sync flow. Cost: slower (River jobs, drained synchronously) — fine for a one-time staging seed.
- **Direct-write is an optional "volume padding" profile** (local/CI, or bulk filler in staging) — fast, but must hand-replicate the consumers' derived fields, so it carries drift risk and is used only where exact derived fields matter less.
- Same toolkit, selected by **profile** (this generalizes the volume/distribution parameter): e.g. `prod-shaped` (replay-heavy, hundreds of contacts) for staging; `minimal-scoped` (small, namespaced) for CI/E2E; `dev` for local. *(As designed. The volume/distribution parameter is what #759 removed: `standard` replaced the three-profile split and is defined by declaration + adversarial-edge registries, not by counts.)*

## Per-source injection (provider-fetch boundary wherever not too expensive)

| Source | Topology | Injection point | Effort |
|---|---|---|---|
| Gmail | scheduler-pull | fake fetcher via `SetFetcherFactoryForTest` + `FakeGmailFetcherFuncs` (exist) → real `Sync` | **zero** |
| GChat | scheduler-pull | fake fetcher via `NewFakeChatFetcherFactoryForTest` (exists) | **zero** |
| Todoist | pull (writer) | fake `Client.Sync` returning synthetic `SyncItem`s via `SetTodoistClientFactory` (exists) | **zero** |
| GCal | scheduler-pull | **extract a `calendarFetcher` interface** (~50 LOC, same pattern as Gmail/GChat) | **small** |
| Mac Contacts | push (daemon) | in-process `IngestService.IngestBatch` with synthetic `external_contact.upserted` envelopes | **zero** |
| iMessage | push (daemon) | in-process `IngestService.IngestBatch` with synthetic `raw_message` envelopes | **zero** |
| Telegram | persistent MTProto | **Approach 1**: construct synthetic `tg.Message` + `tg.Entities`, drive `MessageHandler.HandleNewMessage` (keeps `PeerMatcher` matching fidelity) | **medium** |

The replay adapters compose the providers' existing exported fakes (shippable, non-`_test.go` symbols), call `provider.Sync(...)` / `IngestBatch(...)` / `HandleNewMessage(...)` against a real DB, and drain River so the consumers run.

## Decisions locked (from brainstorm)

- **Synthetic seed is the data source** for staging — not a sanitized prod snapshot (free-text notes can't be reliably scrubbed; PII-on-VPS regression) and not always-on live test accounts (reserved on-demand for integration dev only).
- **Library-first, four-layer toolkit** (factories → replay adapters → scenario catalog → entrypoints); reused across unit/integration/E2E/staging.
- **Generate through the service/repository layer** (and through the real ingestion pipeline for replay), never raw SQL.
- **Replay through the real pipeline is the default; direct-write is a fast volume-padding profile.**
- **Per-source replay adapters at the provider-fetch boundary** for all sources where feasible (Gmail/GChat/Todoist zero-cost; GCal small interface extraction; Mac Contacts/iMessage via in-process `IngestBatch` at the daemon's real boundary; **Telegram via Approach 1** for matching fidelity).
- **Three data shapes (settled / flowed-through / pending) emerge from one replay mechanism** by varying whether the synthetic sender matches a seeded contact.
- **Deterministic:** fixed PRNG seed; timestamps anchored to `accelerated.GetCurrentTime()`; **stable synthetic source-ids** so re-seeding is idempotent.
- **Idempotent re-seed** (confirmed): every write path is an upsert and the event bus dedups on `(source, source_id)`, so replaying the same deterministic input converges — no double-create. `make staging-reset` re-runs are safe.
- **Gated `CRM_ENV != production`** — the replay path invokes shippable fake-fetcher scaffolding, so the seed entrypoint must refuse to run in prod.
- **Dual-use:** the same generator enriches local `make dev` and is the QA harness's deterministic world.
- **Dev tier (Mac + VPS sandbox):** the seed runs through the service layer against the **local Postgres-16 endpoint**, no containers required (see overview's three-tier parity model).

## Test & CI scope

**Test suite — in scope.** D adds its own comprehensive tests (factories, replay adapters, the GCal seam, scenario assembly) per the project test rules, AND includes the test-infra work below. Two constraints to honor throughout: per-test **namespacing** (unique prefixes) so toolkit use can't collide on the shared test DB, and routing heavy **replay** integration tests (River-draining = slow) into the existing slow-test suite (`backend-slow-tests.yml` / LONG_TESTS gating) rather than the fast path.

In scope (see "Sequencing within D" for ordering):
- **New coverage the seams unlock:** GCal + Telegram **sync-flow integration tests** (mirroring `gmail_provider_integration_test.go`) — these close a real gap (both flows are currently un-integration-testable) and validate the new seams.
- **Deterministic golden-scenario regression test** for the seed (assert a named scenario yields a stable per-table graph + key invariants) — catches silent seed drift inside the normal test run; keeps staging/QA trustworthy.
- **Test-importable helper API:** factories + replay adapters must be ergonomically callable from `backend/tests/`, not buried in the seed orchestrator.
- **`/seed/*` + `test-api.ts` refactor** onto the shared core.
- **Suite-wide migration of the existing integration suite onto the factories** — sequenced **last** (a final PR or two), exemplar-first (convert one test as the canonical pattern, then the rest), onto the by-then-proven API. This is where the now-redundant determinism workarounds get removed.

**CI — essentially no workflow changes.** New tests are auto-discovered by the existing `make test` / `test-integration` / E2E jobs; the Postgres + River-drain infrastructure they need already exists in CI, and D's tests exercise the **library directly** (not the `crm-admin` CLI), so CI does **not** need to build the `crm-admin` binary — building/shipping `crm-admin` is A0/A's concern (the image build), not D's. All D changes live under `backend/**`, so the existing `path-filters.yml` `backend` group already triggers the right jobs; no path-filter or workflow edits. The only judgment call is fast-vs-slow placement of replay tests (above), which uses existing machinery.

## Project rules & documentation (in scope)

D doesn't just add a toolkit — it makes that toolkit the **standard**, enforced by convention, and retires the workarounds it obsoletes.

**Forward-looking conventions (land once the API + entrypoints exist):**
- **New features write their own seeding.** When a feature adds an entity, a sync source, or a downstream record, it must add the matching factory + replay/seed coverage to the synthetic toolkit. Add to the "When You Change X, Check Y" table (`AGENTS.md` / `.ai/rules/core.md`) and `.ai/guides/feature-development.md`.
- **New tests use the toolkit.** New tests build state via the synthetic factories/replay/scenarios — not hand-rolled fixtures or raw inserts. Add to `.ai/rules/testing.md`.
- **A new `.ai/patterns/` doc** for the synthetic toolkit (how to build factories, replay a source, request a scenario, scope for isolation), referenced from the rules above.

**Retire the obsoleted gotchas (gated on the suite migration).** Once tests seed through scoped factories, a cluster of shared-DB / fixture gotchas can be simplified or removed — candidates:
- "Raw SQL in integration test setup → add a test-only sqlc query" → "use the synthetic toolkit factories."
- "Integration sub-test reuses identifying names → randomized per-subtest suffixes" → superseded by toolkit namespacing.
- "Limit-based assertions fail / DB accumulates state → run `make e2e-db`" and "assert per-query invariants across count queries" → reduced by scoped seeding + scoped queries.
- E2E "non-unique `getByText` → `.first()`" and "parallel workers see each other's import/link candidates → `navigateModalToCandidate`" → reduced by deterministic, per-worker-namespaced scenarios.

Do **not** delete a gotcha while any unmigrated test still relies on the old pattern — the retirement rides with (or just after) the migration PRs. (`AGENTS.md` is the symlink target for `.claude/CLAUDE.md` — stage `AGENTS.md`.)

## QA harness (#380) support

D as scoped supports the agentic UX QA harness. The contract is clean: **D provides the world, #380 provides the tour.**
- **Deterministic, production-shaped world** with the full downstream graph → every UI surface has content (today: the `standard` profile).
- **Programmatic reset** (`make staging-reset` / callable reseed) → a known baseline before each tour.
- **All surfaces + states covered**, including the pending-state UX (Imports queue, rematch, needs-review) and the edge-case catalog → nothing the harness tours is empty. The **coverage check** (open thread) is the load-bearing hook — it guarantees the harness has something to assert on every surface, so it stays in (lightweight).
- **Deterministic timestamps** anchored to `accelerated.GetCurrentTime()` → cadence/overdue states are reproducible given staging's time config, so tours are comparable across runs.
- **PII-free** synthetic data → safe to screenshot.

#380 owns touring, scheduling (nightly ~3am per B), and visual/UX assertions, and may **contribute additional named scenarios** it needs; D provides the catalog mechanism + a baseline set. The seam is (reset + scenarios).

**Bidirectional coupling (from reading the #380 spec, `2026-05-31-agentic-ux-qa-and-behavior-ssot-design.md`):** #380's Track A (API-level tests) and Track B (tours) currently build on the same `test-api.ts` + `/seed/*` substrate D supersedes — so they must build on **D's toolkit**, and D's `/seed` refactor must preserve what they need. D's **coverage check can be SSOT-driven** (a fixture per `ux` / `data` behavior ID), which makes it concretely measurable and feeds #380 Piece 3's anti-drift traceability — align the two rather than building parallel coverage mechanisms.

## Sequencing within D (for the planner)

1. **Core toolkit** — factories + per-source replay adapters + GCal `calendarFetcher` extraction + Telegram synthetic-input builder + the test-importable helper API.
2. **New coverage** — GCal + Telegram sync-flow integration tests; deterministic golden-scenario regression test.
3. **Entrypoints** — `crm-admin --seed` + `/seed/*` + `test-api.ts` refactored onto the core.
4. **Forward-looking rules/docs** — new-features-seed + new-tests-use-toolkit conventions; new `.ai/patterns/` doc (can land here, once the API exists).
5. **Suite-wide migration** onto the factories (exemplar-first, then the rest) — final PR(s).
6. **Retire obsoleted gotchas** — rides with/after step 5.

## Edge-case catalog (fixtures to bake in)

Domain edge cases:
- 1900-birthday sentinel (month/day-only); names with descenders (clipping bug); unicode names; very long notes; contacts at every cadence state (overdue / never-contacted / mutual-only / outbound-only); contacts with and without methods/notes/birthdays; fuzzy-match near-collisions.

Pending/unprocessed states (each produced naturally by replaying an unknown-sender input; documented as intentional fixtures):
- `external_contact.match_status = 'unmatched'` → Imports page candidate queue.
- `messages_message.matched_contact_id IS NULL` → stranded iMessage awaiting rematch (`crm-admin --messages-rematch-stranded`).
- `telegram_message.matched_contact_id IS NULL` → stranded Telegram awaiting rematch.
- `comms_message` without `interaction_id` → pre-aggregation state.
- `calendar_event` with `matched_contact_ids = '{}'` → needs-review attendees.
- `meeting_note.linkage_state` ∈ {`conflict_pending`, `orphan_needs_review`} → meeting-note linkage review.
- weak title-candidate discovery rows → title-candidate review flow.

## Open threads / TODO (fill out here)

- [ ] **Package layout.** Confirm `backend/internal/synthetic` (or similar); how factories/replay-adapters/scenarios are organized; how `crm-admin --seed`, `/seed/*`, and Go test helpers wrap the core without import cycles (seed is a top-level orchestrator importing `service`/`google`/`telegram`/`todoist`).
- [ ] **GCal `calendarFetcher` extraction.** The one new seam: interface wrapping `calendar.Service.Events.List/Watch`, a `newFetcher` factory field on `CalendarSyncProvider`, and `SetFetcherFactoryForTest` — mirroring Gmail/GChat. Lands as its own small change (standing testability win).
- [ ] **Telegram synthetic-input builder.** Helper to construct `tg.Message` + `tg.Entities` and drive `HandleNewMessage` (handler uses the API client only for group-participant counts — basic messages work with a nil/stub client). Reuse the pure `ParseMessage` where useful.
- [ ] **River draining in the seed context.** Replay enqueues River jobs (interactions, cadence, aggregation, Todoist). The seed must drain them synchronously and wait for completion (mirror the integration-test harness) so the graph is settled when the seed returns. Confirm the drain mechanism + that no consumer is leader-gated in a way that blocks the CLI context.
- [ ] **Profiles + volume/distribution params.** `prod-shaped` / `minimal-scoped` / `dev`; configurable counts and distributions; per-test **namespacing** (unique prefixes/ids) so integration/E2E reuse can't collide on the shared DB (the cross-test-pollution class the determinism arc fights). *(Shipped, then partly reversed: namespacing stands; the configurable counts/distributions and the three-profile split were deleted by #759 in favour of `standard` + `minimal-scoped`.)*
- [ ] **Isolation primitive / parallelization readiness.** Make the namespacing primitive an explicit design decision and build it to support **both** modes: (a) **prefix-scoping within one DB** (lightweight tests) and (b) a fast full-seed suitable for **DB/schema-per-worker** isolation (heavy replay tests, CI sharding). This keeps D parallelization-ready so the follow-on test-parallelization project (`2026-06-07-test-parallelization-design.md`, gh #413) is a cheap successor rather than a rework. **D builds the foundation only** — the actual parallel flip (`t.Parallel()`, per-worker DB provisioning, CI shards) is out of D's scope.
- [ ] **Determinism details.** PRNG seeding strategy; mapping generated timestamps onto accelerated time so cadence/overdue states are reproducible; stable synthetic source-ids/guids for idempotent replay.
- [ ] **Reset mechanism (`make staging-reset`).** Truncate-respecting-FK vs drop/migrate/reseed (soft-delete does NOT cascade → a true wipe is a hard reset). Must be **programmatically callable** by the QA harness, not just interactive. Pairs with C.
- [ ] **Entrypoint refactor.** Fold the existing `TestHandler` seed endpoints (`backend/internal/api/handlers/test.go`) and `test-api.ts` onto the shared core; keep the HTTP `/seed/*` surface for E2E while the in-process path serves `crm-admin --seed`.
- [ ] **Local-dev integration (dev tier).** How `make dev` opts into loading the seed (flag/target); default off vs on. Runs through the service layer against the local Postgres-16 endpoint — same on the Mac and in the VPS sandbox. Postgres packaging is irrelevant; only the major version matters.
- [ ] **Naming/PII hygiene.** Emit obviously-synthetic names/emails/handles (no resemblance to real contacts); document the data source (curated synthetic list or faker lib).
- [ ] **Coverage check (load-bearing for #380 — keep it).** Ensure the seed exercises every list filter / sort / view the UI and QA harness depend on, so "rich enough to verify UI" is verifiable, not assumed. This is the hook that guarantees the QA harness has content on every surface it tours.

## Existing primitives to build on

- **Provider seams (the load-bearing find):** Gmail `SetFetcherFactoryForTest` / `FakeGmailFetcherFuncs` (`backend/internal/google/gmail.go`); GChat `NewFakeChatFetcherFactoryForTest` (`backend/internal/google/gchat.go` + `gchat_helpers.go`); Todoist `Client` interface + `SetTodoistClientFactory` (`backend/internal/todoist/`).
- **Ingestion entrypoints:** `SyncService.RunAccountSync` (`service/sync.go`), `IngestService.IngestBatch` (`service/ingest.go`), `MessageHandler.HandleNewMessage` (`telegram/handlers.go`), `IdentityService.MatchOrCreateTx` (`service/identity.go`).
- **Canonical injection prior art (integration tests):** `backend/tests/gmail_provider_integration_test.go`, `gchat_provider_integration_test.go`, `contact_task_service_test.go`, `api/ingest_raw_message_test.go`, `api/external_contact_ingest_test.go`, `email_interaction_integration_test.go`.
- **Seed prior art:** `TestHandler` routes (`backend/internal/api/handlers/test.go`), `frontend/tests/e2e/helpers/test-api.ts`.
- `crm-admin` operator CLI (subcommand host); `accelerated.GetCurrentTime()`; repository/service + sqlc (the sanctioned write path).

## Dependencies

- **Independent of the infra tracks** (A0/A/B) — pure service-layer + pipeline code, no containerization dependency. Build it **early / in parallel**.
- **Pairs with C** (staging consumes it) and **helps local dev** immediately.
- **Consumed by the #380 QA harness** (needs a rich, deterministic world to tour).
- Carries one small in-repo change of its own: the GCal `calendarFetcher` extraction.
