# Synthetic Seed Toolkit

The synthetic-seed toolkit produces **production-shaped, PII-free, deterministic** synthetic data by replaying fake source input through the *real* ingestion pipeline. It is one library reused four ways: unit tests use the factories directly; integration tests use the replay harness; staging and `make dev-seed` use the profile entrypoints; the QA harness (#380) tours the declared `standard` world. Code lives under `backend/internal/synthetic/`.

The defining capability is that it doesn't just write terminal domain rows — it injects synthetic sync-source inputs (e.g. fake Gmail, GChat, GCal, Telegram, iMessage, Mac-Contacts, or Todoist payloads) and drives them through provider normalization → matching → dedup → event bus → River consumers → downstream graph, so the full sync flow is exercised without live credentials.

## Why the seed drives services and provider replay, not the HTTP API

A natural question is whether seeding should go through the public HTTP API instead, so data propagates via "the application's actual logic." That principle — seed the cause, not the endpoint — is exactly what the toolkit enforces, but deliberately one layer below the handlers. First, most prod data has no API surface: interactions, external contacts, sync state, and enrichment assertions originate from sync pipelines in production, so for that data the app's actual logic *is* the provider replay path, and routing it through invented API endpoints would fake the sync result rather than reproduce its cause. Second, a production-shaped world requires things the API correctly forbids — contacts created months ago, interactions backdated across cadence windows, deterministic PRNG, per-test namespacing; in prod those shapes come from time passing, which can't be replayed through a public API, so the seed injects timestamps at the service/repo layer while still driving the real causal chain from there down. Third, the layer skipped (handler → service) is the thinnest one — DTO validation and auth — and it is validated where it matters: the E2E suite and QA tours exercise the real API against the seeded world. The corollary trade-off: a bug living only in a handler won't corrupt the seeded world and won't be caught by seeding; it surfaces in E2E/tours instead. Bugs in service-layer logic, by contrast, do surface here because seeding runs the real services — the post-seed coherence gates then assert the world contains only prod-reachable states.

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
- **Contacts are additionally recovered by RECORDED OWNERSHIP, not just by name.** A generator-derived token only works while the row still carries it, and `contact.full_name` is user-editable — the update path rewrites `node.canonical_label` with it, so a renamed seeded contact vanishes from every name-derived sweep at once. `declare` therefore records `(namespace, kind, entity_id)` in `synthetic_namespace_entity` at seed time and cleanup unions that with the prefix sweep. Those records are dropped LAST, with the host marker and only on a clean run, so a partial sweep leaves the namespace both discoverable and recoverable. Any new declarable entity class whose recovery token is user-editable owes the same record.
- **The requested and effective namespace grammars are asymmetric, on purpose.** A REQUESTED namespace is 1-57 characters and may not end in `-sN`; the EFFECTIVE one the manifest returns may be up to 60 and may carry that suffix, because construction re-salts on a numeric-band collision and never revalidates the result. Those three reserved characters are what keep a re-salted world's own token acceptable to cleanup — fill all 60 and the seed succeeds while its cleanup request is rejected outright, stranding the rows in the shared E2E database.
- **The E2E client polls retriable cleanup outcomes for as long as a run can hold the namespace.** A seed whose response was lost keeps executing server-side (detached, ~90s run budget plus settle/teardown), holding the reservation that answers `busy`, so `cleanup()` polls rather than retrying once — and raises the Playwright test timeout, since an `afterEach` shares the test's timeout slot and would otherwise be killed mid-poll. Outcomes accumulate ACROSS attempts (a retry names only the retriable namespaces), so one namespace's `error` can never be masked by another's later success.
- **Every harness owns a PRIVATE River queue** (`replay.SyntheticQueueName(namespace)`, derived so a later cleanup can find it). River's fetch has no kind or owner filter, so a harness sharing `default` with the live application damages three separate classes: a no-op kind is finalized without its work (a nil `Work` result FINALIZES the job), a worker wired for replay semantics (`followup_manager` in off mode) finalizes a production job having skipped the work, and a kind the harness does not register at all is fetched and failed as unknown. The client fetches only its own queue and a River insert hook rewrites every job it enqueues onto that queue, so neither side can see the other's work — including harness-versus-harness on a shared test database. Cleanup drops the queue's orphaned unfinalized jobs (its client is provably gone: the reservation is held) and the `river_queue` row itself. Adding a worker to the harness needs no ownership check; do not reintroduce one.
- **A generator never mints the same display name twice — but uniqueness is all it gives you.** Names are drawn WITH REPLACEMENT from a 16×10 pool, so a three-contact fixture repeated one ~1.8% of runs (measured) — enough to break any selector that resolves a contact BY NAME, which is how a manifest-driven spec addresses its fixtures (Playwright's strict mode fails outright on two matching headings). A repeat carries its contact sequence number as a SUFFIX, asserted over a pool-exhausting run in `TestContact_DisplayNamesStayUniquePastThePool`. It is disambiguated rather than redrawn on purpose: a redraw would consume extra rng values and shift every later draw, so a namespace that does NOT collide stays byte-identical. The suffix settles EQUALITY only: the shorter name stays a PREFIX of the disambiguated one, so a spec that resolves a declared fixture by name must match it EXACTLY (`getByText(name, { exact: true })`, or a `filter({ has: … })` over the name element) — a substring match on the shorter name resolves both rows and the mis-resolution reads as a legitimate hit.
- **A declaration spends from the composed world's BUDGETS.** `declare.World()` runs every registered declaration and edge into ONE namespace, so registering fixtures grows the `standard` world for everybody. Two bounds are real and neither is checked by a Go test: the tours resolve their fixtures inside a `limit=500` contact window, and the overdue-bearing tour captures are sliced at `synthetic.TourOverdueCaptureCap` (`frontend/tests/tours/support/pinned-fixtures.ts` refuses to tour a world past it, naming the count). Drawn synthetic phones PANIC on exhaustion at 100 per namespace. Occupancy measured at the contacts-domain migration: **contacts 184/500, overdue 66/96, phones 2/100**. A PR that grows the world reads its own numbers off a tour run, not off a test. Raising a cap is a deliberate two-sided change (the constant AND the slicing it feeds), never a relaxed assertion.
- **`ExplicitName` pins a literal, and its uniqueness is a PER-LIST property.** It is exempt from the display-name dedupe on purpose, so `validateEntityOrder` rejects a repeated literal inside one entity list — the scope that matters, because an E2E spec seeds ONE declaration into its own namespace. A cross-list collision is not checked: nothing resolves a COMPOSED-world contact by a pinned literal (the tours resolve by marker), so no test can be made to pass vacuously by one. `ExplicitName` is also mutually exclusive with `SameNameAs` and `NameEdge`, both of which would render something other than the pinned literal. Its read-path oracle is the citing E2E spec: a pinned literal that failed to lower renders a drawn name, and the spec's locator finds nothing.
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

There are exactly **TWO** profiles (`synthetic/profiles.go` holds the dispatch core; `synthetic/standard.go` holds the declared world):

- `synthetic.ProfileStandard` (`"standard"`) — **the default** for `make dev-seed`, `make staging-reset`, the automated staging reseed, and the QA tours. The DECLARED world: every registered `declare` declaration (behavior-id sorted), then every adversarial edge (`declare.Edges()`, catalog registration order), then the pinned tour fixtures LAST. It carries **no `Counts`** — it is exactly what the two registries say it is, which is why its content is asserted as named states rather than as statistics. Adding a declaration or an edge joins it automatically, and spends from the world's budgets (see the declaration bullet above).
- `synthetic.ProfileMinimalScoped` (`"minimal-scoped"`) — the smallest viable world (a few contacts, one settled interaction per source). The CI/E2E namespacing baseline. It is an **explicit operator override**, never a default: a world this small has no content for a UI surface or a QA tour to exercise.

An earlier generation of profiles (`dev`, `prod-shaped`) sized a contact catalog by volume knobs and then graded it against an invented distribution — bands, quotas, archetype assignments, margins. That whole layer is **deleted**, not renamed: a distribution the seed invented is not evidence about the product, and the states the tours and the judge actually need are now named declarations and adversarial edges instead. `ProfileParams` refuses both retired names with a loud "unknown profile" error, so an un-updated script fails rather than silently building a different world.
- `declare.World` does not trust its tail's reported manifest for completeness. It compares every reported contact id with the exact contact ledger owned by `Harness.SeedContact`; the integration test then checks the tail suffix's marker order. Together those checks reject both a reported row after the pinned fixtures and a created-but-unreported row. On failure, `WorldResult.Steps` remains the completed prefix, `WorldResult.Current` names the actual failing step/final phase, and completed partial entities remain in `Order` so profile summaries report truthful partial counts.

Two ways to request data:

```go
// (a) the convenience full-seed shape — a few contacts + a settled message per source:
res, _ := synthetic.SeedAll(ctx, h, synthetic.DefaultParams())

// (b) a named profile (the entrypoints' path) — pin the namespace + seed so the world is reproducible:
params, _ := synthetic.ProfileParams(synthetic.ProfileStandard)  // override Namespace/Seed if needed
h, teardown, _ := synthetic.NewHarnessWithDBForNamespace(ctx, db, params.Namespace, params.Seed)
result, _ := synthetic.RunProfile(ctx, h, params)           // counts-only ProfileResult, no PII
```

`SeedParams` carries `Namespace`, `Seed` (PRNG), `Profile`, and `Counts` (which only `minimal-scoped` reads — `standard` has no volume knobs at all). The harness must be constructed for the **same** namespace + seed as `params` so the run's identifiers and cleanup scope line up. Contact creation is not upsert-idempotent (re-running `SeedAll` seeds a fresh set); source-message replay **is** idempotent (stable source-ids; the event bus dedups on `(source, source_id)`), so an idempotency assertion re-replays a captured `GmailReplayProbe`, not a second `SeedAll`.

Some pending states have **no toolkit producer yet**: `conflict_pending` meeting_note, title-candidate review rows, `comms_message` without `interaction_id`. Adding one means adding the factory/replay producer plus a declaration or an adversarial edge that asserts it through a read path.

## Asserting the seeded population's shape — read what the product reads

Assert what the seed produced through the same read the application performs, not through a recomputation of the same concept from the underlying columns. The two are different quantities whenever a derived/cached column can lag its inputs, and a gate on the recomputation passes while the deployed world disagrees — silently, because both numbers look correct.

The concrete case (gh #751): a population band was asserted by recomputing overdue-ness from `(cadence, last_contacted, created_at)`, while `GET /contacts/overdue` filters on the persisted `contact_by` column. `contact_by` is written FORWARD-ONLY, so a history whose newest two-way interaction predates its contact's `created_at` moves `last_contacted` backwards past `created_at` and leaves `contact_by` at the creation-time value — the recompute says overdue, the endpoint does not return it. Deployed staging was nine contacts short of what the gate measured and the gate could not see it.

That whole class of statistical gate is now gone with the catalog profiles, and the rule it taught is what replaced it: `standard`'s declarations and adversarial edges are asserted by the E2E specs that cite them, as named states rather than as percentages.

Rules of thumb when adding one of these assertions:

- Assert MEMBERSHIP, ORDERING, or an exact id set through the endpoint the product uses — never a percentage band, and never an exact rendered day count (a source's fixed real-wall-clock lag is additive with the declared age, so day counts are floors).
- If a derived column is involved, assert its precondition rather than assuming it. `contact_by` holds `base + AMBIENT cadence period`, so anything reading it must pin which cadence table is active; under a compressed `CRM_ENV` that column is minutes wide. (The Go integration suite runs with `CRM_ENV` unset — i.e. production durations — because no `make test-integration*` target sets it; only the frontend E2E lane sets `CRM_ENV=testing`.)
- Prefer a test-only sqlc query that COPIES the production predicate (namespace-scoped and unbounded — the production `LIMIT` would let an accumulated shared test DB truncate the window) over re-deriving the concept in Go.

## Entrypoints

| Entrypoint | What it does |
|---|---|
| `crm-admin --seed [--profile P] [--prng-seed S] --yes` | Additive seed of the selected profile world. Refused in production (`synthetic.SeedAllowed` rejects `CRM_ENV` ∈ {production, prod}). Requires `--yes`. |
| `crm-admin --reset-and-seed [--profile P] --yes` | Hard-wipe every live data table (preserving `schema_migrations` + River internals + the migration-seeded curated catalog `predicate`/`entity_type` — only their provisional rows are cleared), then reseed (default `standard`). Refused in production. Requires `--yes`. |
| `make dev-seed` | Stops the detached backend so the seed harness owns the River queue, migrates, then `crm-admin --seed --profile standard --yes`. Opt-in; plain `make dev` is unchanged. Knobs: `DEV_SEED_PROFILE` picks the world, `DEV_SEED_RESET=1` selects `--reset-and-seed` over the additive `--seed` (anything MEASURING a world has to reset, or it grades a mixture of whatever was there before), and `CRM_ENV` selects the cadence semantics the world is computed under. `CRM_ENV` precedence is explicit — caller → `.env` → `testing` — because both this script and `start-backend.sh` `set -a; source .env`, so a naive default evaluated afterwards would be silently beaten by a `CRM_ENV` line in `.env`. A world seeded under one cadence table must be SERVED under the same one, so `start-backend.sh` honors the same precedence at both of its pin sites. |
| `make staging-reset` | Drives the rootless `staging` tenant (ssh `STAGING_HOST`; `--local` on-box): stops `personalcrm-backend`, runs `crm-admin --reset-and-seed --profile "$STAGING_RESET_PROFILE" --yes` (default `standard`) from an ephemeral container off the deployed backend image (the unit's pinned `Image=`, never `:latest`/`podman exec`), restarts. `STAGING_RESET_PROFILE` is the profile knob — the script's argument parser exits 2 on any flag it does not know, so there is deliberately no `--profile` flag. The AUTOMATED path (`deploy-staging.yml` → root-owned `staging-reseed.sh`) does not rely on that default: the wrapper `export`s `STAGING_RESET_PROFILE=standard` itself, so which world the auto-reseed builds sits inside the same runner-immutable trust seam as the tenant identity, and a caller-supplied value cannot leak through. Reads `CRM_ENV` from the deployed staging `.env`; fail-closed refuse if it is a production alias OR empty/unset. Note the profile must exist in the DEPLOYED image, so a newly-added profile is only reachable on staging after its PR merges and deploys. |
| `scripts/check-tour-markers.sh` | Asserts every pinned tour-fixture marker resolves to EXACTLY ONE contact in the world currently served by `TOURS_API_URL`, using the same predicate `resolveFixture` uses and reading the marker tokens from the Go SSOT. Exits 1 naming every offending marker. Run it against a FRESH world BEFORE the tours; it is deliberately not wired into `run-tours.sh`, because the contacts tour CONSUMES `fxdeletevictim` and a second `TOURS_SKIP_RESET=1` iteration would legitimately find zero. |
| `/test/seed/*` + `/cleanup` routes | The E2E HTTP surface, behind `service.TestSeedService`. The handler validates HTTP input then calls the service (no handler→queries layer violation). It does NOT import the synthetic package — the profile/replay world is CLI-only — to avoid a `service → synthetic → replay → service` cycle. |

## Slow-test routing

Replay integration tests are River-draining and therefore slow, so they are gated out of the fast suite. Call `testsupport.RequireLongTests(t)` at the top of any replay test — it skips unless `LONG_TESTS` is set (and short mode is off). The slow workflow (`make test-integration-slow`, run by `.github/workflows/backend-slow-tests.yml`) sets `LONG_TESTS=1` and selects by `BACKEND_SLOW_TESTS_REGEX` (Makefile), which includes `TestSynthetic` — so name synthetic replay tests with the `TestSynthetic` prefix to route them onto the slow path. Both are load-bearing and they do different jobs: the prefix is the only thing the slow lane SELECTS on, and the `RequireLongTests` call is the only thing that makes the fast lane SKIP. A test that would only ever run on the nightly cron gates nothing: delete it rather than gate it. Factory-only unit tests (no replay) need neither mechanism.
