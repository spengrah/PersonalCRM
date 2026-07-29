# Synthetic Seed Toolkit

The synthetic-seed toolkit produces **prod-shaped, PII-free, deterministic** synthetic data by replaying fake source input through the *real* ingestion pipeline. It is one library reused four ways: unit tests use the factories directly; integration tests use the replay harness; staging and `make dev-seed` use the profile entrypoints; the QA harness (#380) tours the prod-shaped world. Code lives under `backend/internal/synthetic/`.

The defining capability is that it doesn't just write terminal domain rows — it injects synthetic sync-source inputs (e.g. fake Gmail, GChat, GCal, Telegram, iMessage, Mac-Contacts, or Todoist payloads) and drives them through provider normalization → matching → dedup → event bus → River consumers → downstream graph, so the full sync flow is exercised without live credentials.

## Why the seed drives services and provider replay, not the HTTP API

A natural question is whether seeding should go through the public HTTP API instead, so data propagates via "the application's actual logic." That principle — seed the cause, not the endpoint — is exactly what the toolkit enforces, but deliberately one layer below the handlers. First, most prod data has no API surface: interactions, external contacts, sync state, and enrichment assertions originate from sync pipelines in production, so for that data the app's actual logic *is* the provider replay path, and routing it through invented API endpoints would fake the sync result rather than reproduce its cause. Second, a prod-shaped world requires things the API correctly forbids — contacts created months ago, interactions backdated across cadence windows, deterministic PRNG, per-test namespacing; in prod those shapes come from time passing, which can't be replayed through a public API, so the seed injects timestamps at the service/repo layer while still driving the real causal chain from there down. Third, the layer skipped (handler → service) is the thinnest one — DTO validation and auth — and it is validated where it matters: the E2E suite and QA tours exercise the real API against the seeded world. The corollary trade-off: a bug living only in a handler won't corrupt the seeded world and won't be caught by seeding; it surfaces in E2E/tours instead. Bugs in service-layer logic, by contrast, do surface here because seeding runs the real services — the post-seed coherence gates then assert the world contains only prod-reachable states.

## Layers

| Layer | Package | What it is |
|---|---|---|
| **Factories** | `synthetic/factory` | Deterministic, dependency-light constructors for each entity (`ContactSpec`, `NoteSpec`) and each source payload (`GmailMessageSpec`, `TelegramMessageSpec`, ...). Imports only leaf/type packages — never `service`/`provider` — so it can't form an import cycle. |
| **Replay adapters** | `synthetic/replay` | The `Harness` plus per-source `Replay*` methods that feed a factory payload through the real pipeline and drain River. |
| **Helper API + profiles** | `synthetic` | The package-root re-export (`synthetic.NewHarness`, `SeedAll`, `RunProfile`, `SeedParams`, `Profile`) — the ergonomic surface `backend/tests/` and the entrypoints call. |
| **Repository support** | `repository/synthetic_support.go` | The `SyntheticSupportRepository`: every sqlc-backed read/delete the harness needs (settle counts, ID-tracked cleanup deletes, collision-band checks). No raw SQL anywhere. |

## The three data shapes — from one mechanism

The dataset must contain three states, and they fall out of **whether a synthetic input's sender matches a seeded contact** — not three code paths. A payload factory takes a `factory.MatchIntent`:

- `factory.MatchSeeded` — the payload addresses a seeded contact's exact identifier → a **settled** / flowed-through graph (matched interaction, derived `last_contacted`/cadence).
- `factory.MatchUnknown` — the payload addresses an identifier no seeded contact owns → a **pending / unprocessed** row that drives a UX surface (Imports queue, rematch, needs-review), or **match-only** where the source has no pending equivalent (Gmail/GChat write no row for an unknown sender).

## Quickstart (a test)

```go
ctx := context.Background()                 // the test's long-lived base ctx — NOT a timeout ctx
h := synthetic.NewHarness(t, ctx, database) // ctx is MANDATORY (client.Start uses it)
gen := h.Generator()                        // seeded, namespaced, live anchor

alice, _ := h.SeedContact(ctx, gen.Contact(factory.WithEmail()))
res, _ := h.ReplayGmail(ctx, alice.ID, gen.GmailMessage( /* target spec */, factory.MatchSeeded))
// the graph is settled here — Settle ran inside ReplayGmail; assert via h.InteractionRepo() etc.
```

Heavy replay tests are LONG-gated and self-skip in the fast suite (see Slow-test routing). The harness's `t.Cleanup` deletes everything the run created.

## Declared seeding (`synthetic/declare`) — the E2E provisioning path

`backend/internal/synthetic/declare` is a layer ON TOP of the toolkit: a fixture is stated in DOMAIN terms and keyed by the SPEC BEHAVIOR ID it provisions, and the E2E suite asks for it by that id. It replaces per-test bespoke seed endpoints (arc #759) rather than replacing anything in the toolkit — a declaration lowers onto the same factory + harness primitives.

```go
// declare/dashboard.go — one registration file per spec domain, run at init.
Register(Declaration{
    Behavior: "CAD-026",
    Entities: []Entity{
        Contact("card-a", Cadence("weekly"), OverdueBy(Days(3))),
        Contact("card-b", Cadence("weekly"), OverdueBy(Days(4))),
    },
})
RegisterNone("DSH-002", "static navigation surface; nothing to seed")
```

```ts
// the spec asks for the behavior, and reads names/ids from the manifest
const seeded = await testApi.seedBehavior('CAD-026')
await expect(page.getByRole('heading', { name: seeded.entities['card-a'].name })).toBeVisible()
// testApi.cleanup() removes the declared worlds along with the prefix ones
```

Load-bearing properties, none of them optional:

- **Every ui-surface spec behavior is resolved exactly once** — Declaration, `RegisterNone(reason)`, or a waiver in `declare/waivers.go`. A unit test in the existing `make test-unit` lane fails on anything unresolved, double-resolved, or naming a behavior that no longer exists. Each domain PR deletes its waiver lines.
- **Fixture math is stated LOCALLY** (`declare/facts.go`), never read from `internal/cadence`. If the app's cadence math regresses, a fixture derived from it would track the regression silently; a locally stated one fails loudly. A tripwire test asserts the local tables equal the deployed ones per environment, and a recursive import guard (proven against a testdata fixture) fails on any import of `internal/cadence` under `declare/`.
- **`OverdueBy` lowers to REPLAYED history, never a column write** — a backdated inbound email, plus a backdated creation. The creation backdating is load-bearing, not cosmetic: the app's due date only ever moves FORWARD on an automated interaction, so a contact created now stays not-overdue no matter how old the history you give it.
- **Cleanup is stateless.** The seed and the cleanup are different requests, so the harness's in-memory id ledger is gone; every id set is rebuilt from the namespace's generator-derived tokens. It refuses rather than guesses: `busy` while a run holds the namespace, `pending` while an unfinalized River job still references the rows, `error` (naming them) when a live namespace is nested underneath.
- **The requested and effective namespace grammars are asymmetric, on purpose.** A REQUESTED namespace is 1-57 characters and may not end in `-sN`; the EFFECTIVE one the manifest returns may be up to 60 and may carry that suffix, because construction re-salts on a numeric-band collision and never revalidates the result. Those three reserved characters are what keep a re-salted world's own token acceptable to cleanup — fill all 60 and the seed succeeds while its cleanup request is rejected outright, stranding the rows in the shared E2E database.
- **The E2E client polls retriable cleanup outcomes for as long as a run can hold the namespace.** A seed whose response was lost keeps executing server-side (detached, ~90s run budget plus settle/teardown), holding the reservation that answers `busy`, so `cleanup()` polls rather than retrying once — and raises the Playwright test timeout, since an `afterEach` shares the test's timeout slot and would otherwise be killed mid-poll. Outcomes accumulate ACROSS attempts (a retry names only the retriable namespaces), so one namespace's `error` can never be masked by another's later success.
- **A harness's no-op workers are namespace-scoped.** The harness runs a real River client on the shared `default` queue, River's fetch has no kind or owner filter, and a nil `Work` result FINALIZES the job — so an unconditional no-op worker silently swallows whatever another process enqueued. Both no-op kinds (`rematch_dispatcher`, `knowledge_cache_updater`) complete only jobs they can prove belong to their namespace and `river.JobSnooze` everything else back. Integration tests get this for free from per-test clones; the E2E lane has one database and needs it. Any no-op worker added to the harness owes the same check.
- **Growth rule**: every entity class a declaration can create MUST get a namespace-recoverable cleanup step in the same PR that makes it declarable.

Endpoints (test-only, same `CRM_ENV` gate as the bespoke ones): `POST /api/v1/test/seed/declared` and the `namespaces` shape of `POST /api/v1/test/cleanup`. The handler drives `declare` directly under a documented layering exception — `service` cannot import `synthetic` without a cycle, and the toolkit writes through the real services one level down.

## Adding a factory for a new entity or source payload

Factories are pure functions of `(seed, namespace)` for everything except timestamps. Read `synthetic/factory/factory.go` — its package doc spells out the two determinism claims (wall-clock-independent identifiers vs anchor-relative timestamps) and the PII / isolation rules.

**A new domain-entity spec** (like `ContactSpec`/`NoteSpec`): add the spec struct + a `(*Generator)` builder in `synthetic/factory/domain.go`. Namespace-prefix any identifying string (`g.Prefix()` → `synth-<ns>-`) so the prefix cleanup backstop finds it. Use `g.givenName()`/`g.surname()` (curated synthetic corpus) for names; never an external faker.

**A new source payload** (like `GmailMessageSpec`): add the spec struct + a `(*Generator)` builder in `synthetic/factory/sources.go`. Each source builder takes the target `ContactSpec` + a `MatchIntent` and addresses the target's exact identifier for `MatchSeeded`, an unowned synthetic identifier for `MatchUnknown`. Required rules:

- **String identifiers** carry the `g.Prefix()` token (e.g. `gmail.externalID`, `gcal_event_id`, message `guid`) — the prefix is both the obvious-synthetic marker and the cleanup key.
- **Numeric identifiers** can't be string-prefixed, so draw them from this namespace's reserved disjoint sub-block: telegram peer ids via `g.nextPeerUserID()`, message ids via `g.nextTelegramMessageID()`, group chat ids via `g.nextGroupChatID()`, phones via the per-namespace area code (`g.phoneFor()`). Isolation matters because identity matching keys on the exact normalized value **DB-wide with no namespace scoping** — two namespaces sharing a phone or peer would cross-match.
- **Timestamps** are `g.at(offset)` (anchor + a deterministic offset), never `time.Now()`. The anchor defaults to `accelerated.GetCurrentTime()` and is injectable via `NewGeneratorAt` for determinism tests.
- **Emit obviously-synthetic data:** the RFC-2606 `.example` TLD for emails, the reserved 555-01XX fictional range for phones.

## Replaying a source through the real pipeline

Each source has a `Harness.Replay*` adapter in `synthetic/replay/` that composes the provider's existing **exported** fake-fetcher seam (shippable, non-`_test.go` symbols), calls the real `provider.Sync(...)` / `IngestBatch(...)` / `HandleNewMessage(...)` against the real DB, drains River, then `Settle`s and tracks created ids for cleanup.

| Source | Adapter | Real seam it drives |
|---|---|---|
| Gmail | `ReplayGmail` | `GmailSyncProvider.Sync` via `SetFetcherFactoryForTest` + `FakeGmailFetcherFuncs` |
| GChat | `ReplayGChat` | the real chat provider sweep via `SetFetcherFactoryForTest` + `FakeChatFetcherFuncs` |
| GCal | `ReplayGCal` | `CalendarSyncProvider.Sync` via the extracted `calendarFetcher` seam (`SetFetcherFactoryForTest` + `FakeCalendarFetcherFuncs`) |
| Telegram (private) | `ReplayTelegram` | `MessageHandler.HandleNewMessage` (nil api client; keeps `PeerMatcher` fidelity) |
| Telegram (group) | `ReplayTelegramGroup` / `ReplayTelegramGroupMessages` | `HandleNewMessage` with a group `tg.Message` |
| iMessage | `ReplayIMessage` | `IngestService.IngestBatch` (`raw_message.received` envelope, revoked host) |
| Mac Contacts | `ReplayMacContacts` | `IngestService.IngestBatch` (`external_contact.upserted` envelope) |
| Todoist | `ReplayTodoist` | fake `Client.Sync` returning synthetic `SyncItem`s |

**To add a replay adapter for a NEW source:** add a `synthetic/replay/<source>.go` with a `(h *Harness) Replay<Source>(ctx, contactID, spec)` method that (1) tracks the synthetic source via `h.track(func(c *created){ c.addDirectSource(...) })` so cleanup captures its root event, (2) drives the source's real seam, (3) calls `h.Settle(ctx, gateA, aggregateSource)`, and (4) records created ids/peers into the ledger. If the source needs new repository reads/deletes, add them to `SyntheticSupportRepository` (a new sqlc query + wrapper) — **never inline raw SQL**. If the source needs a fake-fetcher seam the provider doesn't yet expose, extract one mirroring Gmail's `SetFetcherFactoryForTest` (the GCal `calendarFetcher` extraction is the worked example).

### The namespace / isolation primitive

`ValidateNamespace` enforces the charset `^[a-z0-9-]+$` (rejecting the SQL `LIKE` metacharacters `_` and `%`), so the prefix-based cleanup deletes (`LIKE 'synth-<ns>-%'`) can never over-match another namespace. Each namespace gets **disjoint numeric bands** (telegram peer block, telegram message-id block, phone area code) keyed by a hash of the namespace; `resolveNamespace` collision-checks those bands at harness setup and re-salts on collision. The guarantee is "probabilistically disjoint + detected at setup," not a hard mathematical one.

**Hard rule — no DB-wide enumerate-then-write.** No replay path may scan a table DB-wide and then mutate (e.g. "list all unmatched external_contacts, then reconcile them"). On the shared test DB that silently reaches across namespaces and corrupts a parallel run's data — this was the isolation defect class fixed during the build. Every settle read, cleanup delete, and reconcile must be scoped to *this* run's tracked ids or this namespace's prefix/band. The cleanup ledger (`created` in `replay.go`) exists precisely so deletes go by exact id, never by a DB-wide source/kind value.

### The two-gate Settle + ID-tracked Cleanup (the failure-path contract)

After each `Replay*`, the harness `Settle`s through two per-replay-scoped gates (`replay.go`):

- **Gate A** — a domain terminal predicate: a sqlc read scoped to *this* replay's exact identifiers that returns true once the replay's domain rows have landed (e.g. "this Gmail message is linked to an interaction"). The adapter supplies it.
- **Gate B** — *this* replay's River jobs finalized: `CountUnfinalizedRiverJobsForEventsByContacts` (plus the messaging-aggregate companion) for the run's contact ids reaches zero.

Both budgets are **real wall-clock** (`context.WithTimeout`, monotonic clock), not accelerated time — settle latency is infrastructure timing, and an accelerated budget would collapse under a high `TIME_ACCELERATION` and spuriously time out. A gate timeout names the unmet gate and indicates a real wiring regression, not normal latency.

Cleanup is **ID-tracked and FK-ordered** (`harness_cleanup.go`): the run accumulates created contact/interaction/event ids (+ telegram peers/chat ids + todoist task delta) into the ledger; teardown deletes them in FK order by exact id, or by namespace prefix for the genuinely prefixed columns. `river_job` rows are **never** deleted here — finalized jobs are reclaimed by River retention / a DB reset.

The **failure-path contract** governs the unsettled case: teardown stops *this* harness's River client, bounded-waits Gate B, and gates the **entire** cleanup on Gate B == 0. If Gate B does not clear, teardown **skips all deletes** and leaves the namespaced (inert, obviously-synthetic) dataset intact — a follow-up DB reset reclaims it. Rationale: a retained unfinalized `river_job`, picked up later by a shared default-queue River client, dereferences this replay's contact / comms_message / staging / calendar / event rows; deleting any of them while a job is still live can fault that future worker. For non-test entrypoints the success path calls `Harness.Quiesce` (seed-and-leave: stop client + bounded-wait, no deletes) and the error path calls the full teardown closure (stop + clean the partial world). Either way the River client is always stopped, never leaked.

## Requesting a scenario / profile

A **profile** selects which synthetic world to build (`synthetic/profiles.go`):

- `synthetic.ProfileMinimalScoped` (`"minimal-scoped"`) — the smallest viable world (a few contacts, one settled interaction per source). The CI/E2E namespacing baseline; the golden test pins its shape, so it MUST stay byte-stable.
- `synthetic.ProfileDev` (`"dev"`) — a richer-but-fast local world: the contact edge-case catalog + a settled interaction per source + a handful of pending states, so every local UI surface has content.
- `synthetic.ProfileProdShaped` (`"prod-shaped"`) — the staging / #380 world: ~150 contacts, the full edge-case catalog, and a representative slice of producible pending states. Replay-heavy; slow is fine (one-time staging seed).

Two ways to request data:

```go
// (a) the convenience full-seed shape — a few contacts + a settled message per source:
res, _ := synthetic.SeedAll(ctx, h, synthetic.DefaultParams())

// (b) a named profile (the entrypoints' path) — pin the namespace + seed so the world is reproducible:
params, _ := synthetic.ProfileParams(synthetic.ProfileDev)  // override Namespace/Seed/Counts if needed
h, teardown, _ := synthetic.NewHarnessWithDBForNamespace(ctx, db, params.Namespace, params.Seed)
result, _ := synthetic.RunProfile(ctx, h, params)           // counts-only ProfileResult, no PII
```

`SeedParams` carries `Namespace`, `Seed` (PRNG), `Profile`, and `Counts` (the per-entity volume knobs a profile consumes — there is no distribution DSL). The harness must be constructed for the **same** namespace + seed as `params` so the run's identifiers and cleanup scope line up. Contact creation is not upsert-idempotent (re-running `SeedAll` seeds a fresh set); source-message replay **is** idempotent (stable source-ids; the event bus dedups on `(source, source_id)`), so an idempotency assertion re-replays a captured `GmailReplayProbe`, not a second `SeedAll`.

Some pending states have **no toolkit producer yet** (documented, asserted-absent by the coverage check, tracked for a follow-on): `conflict_pending` meeting_note, title-candidate review rows, `comms_message` without `interaction_id`. See the comment block on `runCatalogProfile` in `profiles.go`.

## Asserting the seeded population's shape — read what the product reads

A population-shape gate ("the seed leaves 12–22% of live contacts overdue") is a claim about what the application SHOWS. Assert it through the same read the application performs, not through a recomputation of the same concept from the underlying columns. The two are different quantities whenever a derived/cached column can lag its inputs, and a gate on the recomputation passes while the deployed world disagrees — silently, because both numbers look correct.

The concrete case (gh #751): the overdue band was asserted by recomputing overdue-ness from `(cadence, last_contacted, created_at)` via `synthetic.OverdueAtProduction`, while `GET /contacts/overdue` filters on the persisted `contact_by` column. `contact_by` is written FORWARD-ONLY, so an archetype whose newest two-way interaction predates its contact's `created_at` moves `last_contacted` backwards past `created_at` and leaves `contact_by` at the creation-time value — the recompute says overdue, the endpoint does not return it. Deployed staging was nine contacts short of what the gate measured and the gate could not see it. The fix was to measure both, model both (`PredictedCatalogOverdue` vs `PredictedCatalogOverduePersisted`), and put the product-facing band on the endpoint's own predicate (`SyntheticSupportRepository.ListOverdueContactIdsByNamePrefix`, a deliberate namespace-scoped copy of the production query).

Rules of thumb when adding one of these gates:

- Prefer a test-only sqlc query that COPIES the production predicate (namespace-scoped and unbounded — the production `LIMIT` would let an accumulated shared test DB truncate the window) over re-deriving the concept in Go.
- If a derived column is involved, assert its precondition rather than assuming it. `contact_by` holds `base + AMBIENT cadence period`, so `assertOverdueBand` requires `cadence.GetCadenceConfig() == cadence.ProductionCadenceConfig()` outright; under a compressed `CRM_ENV` that column is minutes wide and the band would be grading nonsense. (The Go integration suite runs with `CRM_ENV` unset — i.e. production durations — because no `make test-integration*` target sets it; only the frontend E2E lane sets `CRM_ENV=testing`.)
- Keep the recomputation too, as a separate assertion. It is what the seed's assignment can actually steer, and the two measurements bracketing each other (endpoint set ⊆ recomputed set) is itself a regression detector.

## Entrypoints

| Entrypoint | What it does |
|---|---|
| `crm-admin --seed [--profile P] [--prng-seed S] --yes` | Additive seed of the selected profile world. Refused in production (`synthetic.SeedAllowed` rejects `CRM_ENV` ∈ {production, prod}). Requires `--yes`. |
| `crm-admin --reset-and-seed [--profile P] --yes` | Hard-wipe every live data table (preserving `schema_migrations` + River internals + the migration-seeded curated catalog `predicate`/`entity_type` — only their provisional rows are cleared), then reseed (default `prod-shaped`). Refused in production. Requires `--yes`. |
| `make dev-seed` | Stops the detached backend so the seed harness owns the River queue, migrates, then `crm-admin --seed --profile dev --yes`. Opt-in; plain `make dev` is unchanged. |
| `make staging-reset` | Drives the rootless `staging` tenant (ssh `STAGING_HOST`; `--local` on-box): stops `personalcrm-backend`, runs `crm-admin --reset-and-seed --profile prod-shaped --yes` from an ephemeral container off the deployed backend image (the unit's pinned `Image=`, never `:latest`/`podman exec`), restarts. Reads `CRM_ENV` from the deployed staging `.env`; fail-closed refuse if it is a production alias OR empty/unset. |
| `/test/seed/*` + `/cleanup` routes | The E2E HTTP surface, behind `service.TestSeedService`. The handler validates HTTP input then calls the service (no handler→queries layer violation). It does NOT import the synthetic package — the profile/replay world is CLI-only — to avoid a `service → synthetic → replay → service` cycle. |

## Golden-scenario regression

`backend/tests/synthetic_golden_scenario_integration_test.go` pins the `minimal-scoped`/`SeedAll` shape against a committed snapshot at `backend/tests/testdata/golden_stream.txt` (fixed seed `"golden"`, fixed anchor). The stream records only NON-timestamp fields, so it catches silent factory/seed drift inside the normal test run. Regenerate intentionally with `go test ... -run TestSyntheticGolden... -update`; an un-intended change FAILS loudly.

## Slow-test routing

Replay integration tests are River-draining and therefore slow, so they are gated out of the fast suite. Call `testsupport.RequireLongTests(t)` at the top of any replay test — it skips unless `LONG_TESTS` is set (and short mode is off). The slow workflow (`make test-integration-slow`, run by `.github/workflows/backend-slow-tests.yml`) sets `LONG_TESTS=1` and selects by `BACKEND_SLOW_TESTS_REGEX` (Makefile), which includes `TestSynthetic` — so name synthetic replay tests with the `TestSynthetic` prefix to route them onto the slow path. Factory-only unit tests (no replay) need neither.
