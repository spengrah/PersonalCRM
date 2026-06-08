# Synthetic Seed Toolkit

The synthetic-seed toolkit produces **prod-shaped, PII-free, deterministic** synthetic data by replaying fake source input through the *real* ingestion pipeline. It is one library reused four ways: unit tests use the factories directly; integration tests use the replay harness; staging and `make dev-seed` use the profile entrypoints; the QA harness (#380) tours the prod-shaped world. Code lives under `backend/internal/synthetic/`.

The defining capability is that it doesn't just write terminal domain rows — it injects synthetic sync-source inputs (e.g. fake Gmail, GChat, GCal, Telegram, iMessage, Mac-Contacts, or Todoist payloads) and drives them through provider normalization → matching → dedup → event bus → River consumers → downstream graph, so the full sync flow is exercised without live credentials.

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

## Entrypoints

| Entrypoint | What it does |
|---|---|
| `crm-admin --seed [--profile P] [--prng-seed S] --yes` | Additive seed of the selected profile world. Refused in production (`synthetic.SeedAllowed` rejects `CRM_ENV` ∈ {production, prod}). Requires `--yes`. |
| `crm-admin --reset-and-seed [--profile P] --yes` | Hard-wipe every live data table (preserving only `schema_migrations` + River internals), then reseed (default `prod-shaped`). Refused in production. Requires `--yes`. |
| `make dev-seed` | Stops the detached backend so the seed harness owns the River queue, migrates, then `crm-admin --seed --profile dev --yes`. Opt-in; plain `make dev` is unchanged. |
| `make staging-reset` | Stops the service, sources the staging env, `crm-admin --reset-and-seed --profile prod-shaped --yes`, restarts. Refuses `CRM_ENV=production`. |
| `/test/seed/*` + `/cleanup` routes | The E2E HTTP surface, behind `service.TestSeedService`. The handler validates HTTP input then calls the service (no handler→queries layer violation). It does NOT import the synthetic package — the profile/replay world is CLI-only — to avoid a `service → synthetic → replay → service` cycle. |

## Golden-scenario regression

`backend/tests/synthetic_golden_scenario_integration_test.go` pins the `minimal-scoped`/`SeedAll` shape against a committed snapshot at `backend/tests/testdata/golden_stream.txt` (fixed seed `"golden"`, fixed anchor). The stream records only NON-timestamp fields, so it catches silent factory/seed drift inside the normal test run. Regenerate intentionally with `go test ... -run TestSyntheticGolden... -update`; an un-intended change FAILS loudly.

## Slow-test routing

Replay integration tests are River-draining and therefore slow, so they are gated out of the fast suite. Call `testsupport.RequireLongTests(t)` at the top of any replay test — it skips unless `LONG_TESTS` is set (and short mode is off). The slow workflow (`make test-integration-slow`, run by `.github/workflows/backend-slow-tests.yml`) sets `LONG_TESTS=1` and selects by `BACKEND_SLOW_TESTS_REGEX` (Makefile), which includes `TestSynthetic` — so name synthetic replay tests with the `TestSynthetic` prefix to route them onto the slow path. Factory-only unit tests (no replay) need neither.
