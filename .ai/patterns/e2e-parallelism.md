# E2E Parallelism — writing parallel-safe Playwright tests

Every worker in the Playwright suite runs against the same Postgres DB, backend, and frontend. Tests stay safe under parallelism by never observing another worker's data, not by hoping workers don't collide. See `.ai/spec/2026-07-16-e2e-parallelization-design.md` for the audit and full rationale.

## The prefix contract

`createTestAPI(request, testInfo)` mints a per-worker prefix (`getTestPrefix` → `w{workerIndex}-{timestamp}`) once at construction, exposed as `testApi.prefix`. Every seed helper (`seedContacts`, `seedExternalContacts`, `seedMacHost`, …) sends this prefix to the backend, which prepends it to the entity's name/display value server-side — so a contact named `Foo` in a test renders as `${testApi.prefix}-Foo`. Always build assertion strings from `testApi.prefix`, never hardcode the seeded name. `test.afterEach` calls `testApi.cleanup()`, which deletes only rows matching this test's prefix — never a wildcard.

## The four scoping rules

1. **Scope list reads via `search=${prefix}`.** The contacts list is the model citizen: fill the search box (or navigate with a `?search=` param) before asserting on rows, then assert relative order/membership among your own rows only. Never index into or count an unscoped `tbody tr` — other workers' rows are present and rendered in a nondeterministic order.
2. **Global widgets get invariant assertions, never absolute counts.** A dashboard badge or queue counter is shared by every worker's seeded data. Assert internal invariants (header count == rendered cards, count `>=` your seeded floor) instead of an exact number.
3. **Empty states are always `page.route` mocks, never real-DB emptiness.** A table is never actually empty in a shared DB under parallel load; mock the endpoint to return `[]` to exercise the empty-state branch.
4. **Provider/settings boundaries are route-mocked.** External-provider calls (OAuth, Todoist, etc.) are mocked at the network boundary in every settings/provider spec — this is also what keeps those tests parallel-safe, since they never depend on real external state.

When a surface genuinely can't be scoped or mocked — a DB-level singleton like `mac_host`, which only allows one non-revoked row at a time — reach for the global-lock fixture below instead of a workaround.

## Global-lock fixture

`frontend/tests/e2e/helpers/global-lock.ts` exports `acquireGlobalLock(name): Promise<() => void>`, an OS-level mutex (atomic `mkdir` spin loop, 5-minute stale-lock break, keyed by `E2E_DATABASE_NAME` so isolated lanes don't contend). Use it for unscopable global singletons that multiple spec files mutate: register a `test.beforeEach` that acquires the lock *before* any other `beforeEach` that touches the singleton, and a `test.afterEach` that releases it *after* the existing cleanup `afterEach` — hook execution order matches registration order for hooks at the same describe level (verify empirically with a scratch run if unsure, don't assume). The release must be called unconditionally (`releaseLock?.()`), since Playwright still runs every registered `afterEach` even when an earlier hook throws.

Reach for this only when scoping/mocking doesn't apply — it serializes across every worker holding the same lock name, which is a throughput cost. `settings-mac.spec.ts` and `imports-interactions.spec.ts` both hold the `mac-host` lock for this reason.
