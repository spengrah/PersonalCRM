# E2E Parallelism — writing parallel-safe Playwright tests

Every worker in the Playwright suite runs against the same Postgres DB, backend, and frontend. Tests stay safe under parallelism by never observing another worker's data, not by hoping workers don't collide. See `.ai/spec/2026-07-16-e2e-parallelization-design.md` for the audit and full rationale.

## The prefix contract

`createTestAPI(request, testInfo)` mints a per-worker prefix (`getTestPrefix` → `w{workerIndex}-{timestamp}`) once at construction, exposed as `testApi.prefix`. The name-bearing bespoke seed helpers (`seedContacts`, `seedCalendarEvents`) send this prefix to the backend, which prepends it to the entity's name/display value server-side — so a contact named `Foo` in a test renders as `${testApi.prefix}-Foo`. Always build assertion strings from `testApi.prefix`, never hardcode the seeded name. `seedBehavior` isolates differently and more strongly: each call gets its OWN namespace (`${prefix}-c{n}`) and the generator owns every name, so assertions read them out of the returned manifest rather than composing them — see `declaredWorldNamePrefix` / `declaredWorldSearch` for the two scoping terms the UI surfaces need. `test.afterEach` calls `testApi.cleanup()`, which deletes only rows matching this test's prefix — never a wildcard.

## The four scoping rules

1. **Scope list reads via `search=${prefix}`.** The contacts list is the model citizen: fill the search box (or navigate with a `?search=` param) before asserting on rows, then assert relative order/membership among your own rows only. Never index into or count an unscoped `tbody tr` — other workers' rows are present and rendered in a nondeterministic order.
2. **Global widgets get invariant assertions, never absolute counts.** A dashboard badge or queue counter is shared by every worker's seeded data. Assert internal invariants (header count == rendered cards, count `>=` your seeded floor) instead of an exact number.
3. **Empty states are always `page.route` mocks, never real-DB emptiness.** A table is never actually empty in a shared DB under parallel load; mock the endpoint to return `[]` to exercise the empty-state branch.
4. **Provider/settings boundaries are route-mocked.** External-provider calls (OAuth, Todoist, etc.) are mocked at the network boundary in every settings/provider spec — this is also what keeps those tests parallel-safe, since they never depend on real external state.

When a surface genuinely can't be scoped or mocked — a DB-level singleton like `mac_host`, which only allows one non-revoked row at a time — reach for the global-lock fixture below instead of a workaround.

## Global lock

`frontend/tests/e2e/helpers/global-lock.ts` exports `acquireGlobalLock(name, { deadlineMs })`, a named mutex arbitrated by the test-only backend (`POST /api/v1/test/lock`, CRM_ENV=testing). The backend is the one process every Playwright worker already shares, so mutual exclusion is a single in-process decision — no filesystem lock protocol (every one of those shares a stale-break TOCTOU under simultaneous takeover) — and isolated lanes can't contend because each lane runs its own backend. The lease expires unless renewed; the helper heartbeats while the lock is held, so a SIGKILLed worker stops renewing and its lease lapses instead of deadlocking the suite. A waiter that outlives its deadline throws loudly, as does a holder whose lease is lost mid-hold.

Use it for unscopable global singletons that multiple spec files mutate, held for the WHOLE file: acquire in a top-level `test.beforeAll` (with `test.setTimeout` raised inside the hook — the contending file may hold the lock for its entire serial run) and release in `test.afterAll` (its own timeout slot). Do not cycle the lock per test: the releasing worker instantly re-acquires for its next serial test, starving the other file's waiter.

Reach for this only when scoping/mocking doesn't apply — it serializes across every worker holding the same lock name, which is a throughput cost. `settings-mac.spec.ts` and `imports-interactions.spec.ts` both hold the `mac-host` lock for this reason.
