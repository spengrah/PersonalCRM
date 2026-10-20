# Core Rules

These rules apply to all AI agents working on this project.

## Absolute Rules (Never Violate)

1. **Never use `time.Now()`** → Use `accelerated.GetCurrentTime()`
2. **Never write raw SQL in Go** → Use sqlc-generated queries (`make sqlc`). Applies to ALL Go code: production code, integration tests, test fixtures, and helper scripts — add a test-only sqlc query + repository wrapper rather than inlining `pool.Exec(ctx, "INSERT ...", ...)` in a test file.
3. **A layer exists where it earns its keep** → handlers never call sqlc (see rule 5); anything with business logic, multi-repository orchestration, or a transaction goes in a service; a handler may call a repository directly when the call adds nothing but a rename. See `.agents/guides/feature-development.md` "When NOT to create a service".
4. **Never use npm/npx** → Use bun/bunx
5. **Never call queries from handlers** → Go through repository
6. **Always sign commits** → `git commit -S -m "..."`
7. **Test changed behavior and meaningful regression risks** → Add or update tests in proportion to the change. Documentation, formatting, and mechanical edits do not automatically require new tests.

## Code Quality

- Keep solutions simple and direct
- Prefer boring, readable code over clever abstractions
- Do not over-engineer or add unrequested features
- Run lint/format after code changes
- Run tests to verify changes work during development

### Second-order verification

Seed, fixture, and harness code is verification layer. A test of it is second-order and presumed unnecessary.

One thing overcomes the presumption: a silent failure there would make first-order tests pass vacuously. Diagnostics never count.

1. If this fixture broke silently, would any currently-passing test become meaningless?
2. No → delete it. The consuming test is the verification.
3. Yes → add the smallest assertion that catches the lie, in the lane that gates a merge.
4. Never add a guard whose own correctness requires a further guard.

A second-order test that does not gate a merge is deleted — unless it qualifies above, in which case move it into the gating lane.

### Ephemeral second-order verification

When adding or changing a gate or guard, prove it rejects bad input, then delete
the temporary proof. This does not require mutation testing for every application change.

Write the second-order test — the injected defect, the mutation, the probe — run it, confirm it turns red, discard it. Keep mutants outside the repository (`/tmp`, `go test -overlay`) so nothing is committed by accident.

Record in the PR body: the defect injected, the command run, and the exit code observed.

Commit nothing.

### Proportionality

Before adding a permanent gate (lint rule, CI step, hook phase, guard script):

1. Name the concrete failure it prevents.
2. Determine whether that failure is already impossible — a type, an enum's `default:` arm, a compile error, a runtime `unknown X` error.
3. If the only remaining risk is stale documentation or comments, do a one-time audit instead: record the hit list and per-occurrence justification in the PR body, and commit no machinery. A one-time audit needs no test.
4. If the gate plus its test exceeds the code it guards, drop the gate.
5. Measure runtime and state the required tools, services, and environment. For a pre-push gate, explain why the check must block a push rather than a merge, and how it fits the warm-checkout time budget. Record this evidence in the PR description; do not add another reporting system.

A gate whose self-test needs its own falsification is the wrong tool.

## Testing Requirements

- Run focused tests appropriate to the changed behavior and available environment; broaden for shared or high-risk changes. Report verification deferred to CI. Required CI suites gate merging.
- Do not rerun successful checks unless relevant source, test, configuration, dependency, or environment inputs changed, or new evidence warrants it. Reuse relevant results already obtained in the session; this does not waive required CI checks for the revision being merged.
- Run `make test-e2e` only for the full suite (CI or when explicitly requested)
- Agents may choose focused E2E runs with `make test-e2e-local PLAYWRIGHT_GREP='...'` for the affected behavior; no user-supplied grep is required.
- Unit tests for business logic, integration tests for DB operations, E2E for user flows

### Writing tests

These rules govern new and substantially rewritten tests. An existing test converts only when a change already rewrites it. Each rule serves one of the suite's goals and names what enforces it; reviewer judgment means the rule is enforced at code review ([code-review.md](code-review.md)).

The goals:

1. **Proof:** every behavior the product promises has a test that fails when it breaks, cited to its spec ID.
2. **Gating:** regressions are caught before merge.
3. **Fidelity:** the repository's own code runs against real PostgreSQL; fakes stand in only for what cannot run, and for injected failures.
4. **Independence:** tests are parallel-safe and deterministic.
5. **Speed:** the gating lanes stay cheap enough to run on every PR.
6. **One way:** each kind of test plumbing has one home, so an agent copying the nearest example copies the right one.

The rules:

- **Cite the behavior a test proves** (Proof). Follow the [test-to-behavior citation rules](../../spec/README.md#test--behavior-citations), and cite the behavior the assertions exercise. Enforced by `make spec-lint` and `make spec-coverage` in pre-push and CI.
- **The proving test gates a merge** (Gating). The test that proves a spec behavior runs in a lane that `.github/workflows/ci.yml` runs on every pull request. The nightly slow lane (`make test-integration-slow`) holds only extra coverage whose measured cost outweighs catching its regression a day late; cost alone never moves a behavior's last proving test there. A second-order test that guards against vacuous passes also gates a merge, as [Second-order verification](#second-order-verification) requires. Enforced by reviewer judgment: `make spec-coverage` records no lane. To route a test to the slow lane, follow "Slow-test routing" in [synthetic-seed-toolkit.md](../patterns/synthetic-seed-toolkit.md#slow-test-routing).
- **Fake only what a test cannot run** (Fidelity). Fake third-party APIs and their clients, the network, the clock, and a failure the real dependency cannot produce on demand. The repository's own repositories and services run against real PostgreSQL; a fake of the repository's own code exists only to inject a failure. When a failure matters, assert its outcome (retried, skipped, rolled back, or mapped to a status), not only that an error came back. Enforced by reviewer judgment.
- **Test production code, not a copy of it** (Fidelity). Reach unexported logic from a same-package test or through an exported entry point; never copy it into a test. Enforced by reviewer judgment.
- **Build domain state with the synthetic toolkit** (One way). When a test needs reusable domain state, build it with `synthetic/factory` in a unit test or the replay harness (`synthetic.NewHarnessForNamespace`) in an integration test, and extend the toolkit when it cannot express that state. A small repository fixture and a same-package test of unexported logic stay as they are: the harness cannot be imported from the packages it depends on. Enforced by reviewer judgment. How-to: [synthetic-seed-toolkit.md](../patterns/synthetic-seed-toolkit.md).
- **Isolate by namespace, or by a clone when the database is the state** (Independence). On a shared package database, scope every read and assertion to the test's own namespace, never to a database-wide count. Open a per-test clone (`newIsolatedRiverTestDB`) when correctness depends on database-wide state: a database-wide count or delete, a fixed ID, a singleton table, or a started River client. A synthetic-harness test opens nothing: the harness runs on a clone of the test's own, shared by every harness the test builds. Enforced by the harness constructors for harness tests (`backend/internal/synthetic/replay/harness_testdb.go`) and by reviewer judgment for the rest. Recipe and validation: [test-parallelism.md](../patterns/test-parallelism.md).
- **Parallel by default** (Speed). A backend integration test calls `t.Parallel()`: Go finishes every serial test before the parallel ones start, so each serial test lengthens the run. A test whose subject is the migration runner stays serial. Enforced by reviewer judgment.
- **Isolate E2E specs by prefix and declared seeds** (Independence). An E2E spec seeds through `testApi.seedBehavior` and builds the identifying strings of rows it creates through the product from `testApi.prefix`, so its cleanup finds them and its assertions read only its own rows. Use serial mode (`test.describe.configure({ mode: 'serial' })`) only to order the tests of one file when a sibling test changes shared state the file reads, and the global lock (`acquireGlobalLock`) for a singleton that several files change. Enforced by reviewer judgment. How-to: [e2e-parallelism.md](../patterns/e2e-parallelism.md) and `frontend/tests/e2e/helpers/test-api.ts`.
- **Tag E2E specs by area** (Speed). Tag each spec's `describe` block `@area:<surface>` after the functionality it verifies, so a focused run (`PLAYWRIGHT_GREP='@area:<surface>'`) selects it. Enforced by reviewer judgment.

## Pre-push Hooks

Git pre-push hooks run automatically and may block push:

- **Static checks only**: Backend lint; frontend lint, types, and formatting; repo hygiene; spec lint, coverage, and drift; generated API types/docs and contact-query drift. Checks are selected by changed paths using `.agents/pre-push.json` and `path-filters.yml`. Repo hygiene always runs.
- **Spec drift**: Detected drift warns; operational failures block the push. The local remote development ref must be available; the hook does not fetch.
- **Frontend formatting**: Pre-push checks changed, existing files with Prettier. Formatting configuration or frontend package/lockfile changes trigger a full formatting check. ESLint and TypeScript still check the whole frontend; CI retains the full `bun run lint` command.
- **Tests**: Unit, integration, E2E, Swift, deploy-script, and hook suites run explicitly during development and in CI, not in pre-push. Backend-only and docs-only pushes do not require frontend dependencies.
- Aim for a roughly 30-second warm-checkout hook budget. Measure slow checks before deciding whether they belong in CI.

## Code Review Approval Criteria

Use [code-review.md](code-review.md) as the authoritative review policy, including
blocking criteria, proportional fixes, follow-up reviews, and verdict format.

## Git Practices

- Use conventional commits (feat:, fix:, docs:, refactor:, test:, chore:)
- First line under 72 characters
- Commit logical units of work, not partial changes
- Use conventional branches (feat/, fix/, refactor/, docs/, test/, chore/), unless the workflow or tool creating the branch prescribes its own names

### Branch model: `develop` (default) + `main` (prod)

- The default branch is `develop`. ALL PRs target `develop` (protected: requires a PR + green CI).
- Merge into `develop` autonomously once required CI passes and the workflow that produced the PR calls for the merge, such as an arc's landing after its gates. Production promotion requires human approval; never promote autonomously.
- `main` is prod and is fast-forward-only from `develop` — never commit or open PRs directly against it.
- Promote with `make promote` (`git push origin develop:main`), which triggers the self-hosted-runner prod deploy via `deploy-prod.yml`.
- Pushes to `main` skip the local pre-push checks: the content was already reviewed + CI-gated on `develop`, and `deploy-prod.yml` re-verifies CI for the SHA before deploying.

## Layered Architecture

See [Request Flow Diagram](../guides/architecture.md#why-layered) for the write-path sequence (`Handler → Service → Repository → sqlc → PostgreSQL`). Read-heavy handlers may bind directly to a repository surface when the service layer would only forward (rule: a layer exists where it earns its keep).

## Task Scope and Context

Search for related defects to understand impact. Fix those within the task's scope;
report unrelated cleanup separately. Do not expand a narrow fix into a repository-wide refactor.

Read the relevant entries in the [change checklist](../guides/change-checklist.md)
when changing shared contracts, database semantics, generators, or verification
tooling. Domain entry files link their troubleshooting guides; load them for the
affected area rather than reading every guide upfront.

When changing product behavior, read `spec/README.md` and the relevant domain spec.
Follow the [maintenance rule](../../spec/README.md#maintenance-rule) and
[test citation rules](../../spec/README.md#test--behavior-citations) there.

## Error Handling

```go
// Proper error wrapping
if err != nil {
    return fmt.Errorf("create contact: %w", err)
}

// Proper error comparison
if errors.Is(err, db.ErrNotFound) {
    api.SendNotFound(c, "Contact")
    return
}
```

## Soft Deletes

All queries must filter `WHERE deleted_at IS NULL`. This is enforced in sqlc queries.

A child-table read that joins `contact` only to enforce `deleted_at IS NULL` (no other contact column needed, or contact columns needed alongside the liveness filter) selects from the `live_contact` view instead of hand-copying the predicate. `contact.sql`'s own queries keep filtering `contact` directly — they read the contact table as the subject, not joining it for liveness.

**Important:** Soft-delete (`UPDATE deleted_at = NOW()`) does NOT trigger FK cascades. When soft-deleting a parent record (e.g., contact), you must explicitly delete or reassign related records (e.g., contact_methods, notes) first. The ON DELETE CASCADE constraint only fires on actual DELETE statements.
