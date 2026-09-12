# Testing Rules

## Before Pushing

Run focused verification appropriate to the changed behavior: affected unit tests
for logic, integration tests for database changes, and E2E tests for user flows
when the environment supports them. Broaden testing for shared or high-risk changes.
Report any verification deferred to CI; do not provision unrelated runtimes just
to push a branch.

Pre-push runs path-selected static checks, including spec drift. Required CI
suites gate merging. Use `make test` and `make test-e2e-local` explicitly when
broader local verification is useful; neither is mandatory for every push.

Agents may select focused E2E tests with
`make test-e2e-local PLAYWRIGHT_GREP='...'` without waiting for the user to
provide a grep. Match the selection to the affected behavior.

Reuse successful verification from the session when its relevant inputs have
not changed. Rerun when source, tests, configuration, dependencies, environment,
or new evidence make the previous result insufficient. Required CI checks must
still pass for the revision being merged.

## Test Pyramid

```
        E2E Tests (Playwright)
       - Full user workflows
      - Browser automation
     - Slowest, run pre-deploy

       Integration Tests
      - DB + Repository layer
     - Real PostgreSQL
    - Run in CI

      Unit Tests
     - Pure functions
    - Mocked dependencies
   - Fastest, run frequently
```

See [Layered Architecture](../guides/architecture.md#why-layered) for how these layers interact.

## When to Write What

**Unit Tests:**
- Business logic calculations
- Validation logic
- Utility functions
- Handler response formatting

**Integration Tests:**
- Repository CRUD operations
- Database constraints
- Transaction handling
- Migration correctness

**E2E Tests:**
- Critical user flows (create → view → delete)
- Navigation
- Form submissions
- Error states

## Integration Tests vs Unit Tests

**Integration tests suffice for unit tests** when unit tests would require heavy mock infrastructure. If the codebase doesn't have mock interfaces for repositories, write integration tests that exercise the real code path rather than creating mock infrastructure for a single test file.

## Build State With the Synthetic Toolkit

**New tests build state via the synthetic factories/replay/scenarios — not hand-rolled fixtures or raw inserts.** Unit tests use `synthetic/factory` directly; integration tests use the `synthetic.NewHarness` replay harness (which seeds through the real service/repository layer, replays source input through the real pipeline, and tears down by tracked id). This gives every new test deterministic, namespace-isolated data on the shared test DB for free — instead of re-deriving the determinism/isolation workarounds by hand.

- **Don't** open-code a `CreateContact` fixture, a raw `pool.Exec(ctx, "INSERT ...")`, or a hand-built source payload — call `gen.Contact(...)` / `gen.GmailMessage(...)` / `h.SeedContact(...)` / `h.ReplayGmail(...)` etc.
- **Namespace isolation** is automatic: give each sub-test a unique namespace via `synthetic.NewHarnessForNamespace` so shared-test-DB reuse can't collide (supersedes the manual randomized-suffix pattern).
- **Heavy replay tests are slow** (River-draining): call `testsupport.RequireLongTests(t)` and name them with the `TestSynthetic` prefix so they route onto the slow suite (see Slow-test routing in `.ai/patterns/synthetic-seed-toolkit.md`).

- **E2E specs that need a seeded world** post `POST /api/v1/test/seed/declared` with a `behavior_id`, and the fixture shape is DECLARED in `backend/internal/synthetic/declare/<domain>.go` beside its spec behavior. This is the only provisioning path — no bespoke `/api/v1/test/seed/*` endpoint exists. A spec that creates rows through the PRODUCT (a contact POST, a note PUT) builds their identifying strings from `testApi.prefix` so `cleanup()` can find them.

See [`.ai/patterns/synthetic-seed-toolkit.md`](../patterns/synthetic-seed-toolkit.md) for the factory/replay/declare how-to.

## Backend Integration-Test Parallelism

New backend integration tests run with `t.Parallel()` by default. The suite was converted to within-run parallelism across #430 (`backend/tests`), #438 (`backend/tests/api`), and #428 (the river-heavy core); a new serial test silently widens the suite's serial prefix (Go runs the entire non-`t.Parallel()` cohort to completion before the parallel cohort starts), so default to parallel and only opt out for the documented serial cases below.

**Know which DB model your test uses — it decides how you make it parallel-safe:**

- **Shared package DB** (`backend/tests` and `backend/tests/api`, via `newSharedTestDB` / `newAPISharedTestDB`): one `MaxConns=8` pool per package, shared across that package's parallel tests. The parallel-safety lever here is **namespace scoping** — scope every read/assertion to your own namespace (`syntheticNS(t)` / `synthetic.NewHarnessForNamespace`; see Build State With the Synthetic Toolkit). Never assert over a global/DB-wide count, or compare counts across two queries — a sibling test can change rows between them.
- **Per-test ephemeral clone** (`testdb.NewEphemeralClone`, surfaced through the `newIsolatedRiverTestDB` helper): each test mints its own clone DB (copied from the content-hashed, pre-migrated template) plus its own pool + River client, dropped on `t.Cleanup`. Use this when a test needs true isolation (see the River rule). A clone is pre-migrated — do NOT call `db.RunMigrations` against it.

**River-touching tests → per-test clone.** A live River client (`client.Start(ctx)`) draining `river_job` on a shared DB steals sibling tests' jobs, and DB-wide `river_job` count/delete assertions collide. (A synthetic replay harness is the exception to the first half — it fetches only its own private queue, `replay.SyntheticQueueName` — but not to the second, so the clone rule stands for it too.) Any test that starts a worker, asserts DB-wide over `river_job`, or relies on fixed-ID fixtures (fixed `external_task_id`, fixed chat/message IDs) must isolate via `newIsolatedRiverTestDB` before flipping to `t.Parallel()` — the private clone makes those collisions impossible by construction (no ID renaming needed). Enqueue-only tests (build a `TestOnly` client but never `Start` it) can stay on the shared DB.

**Stays serial — the documented exceptions:**

- Singleton-table tests (`mac_host_*` auth in `tests/api`): the auth tables are global singletons; per-test cloning was tried and correctly reverted. Keep serial.
- Migration-subject tests (e.g. `TestRunMigrations_River_Integration`): the migration runner is the subject under test, so a pre-migrated clone is meaningless. Keep serial.

**Connection budget.** Concurrent clones/pools share one Postgres (`max_connections=200` on CI and recreated-local; 100 on a stock/un-recreated local container). Each shared-DB pool ≈ 8 conns; each isolated-river clone ≈ 7 (pool 6 + River's LISTEN conn). `-p`/`-parallel` are computed by `scripts/test-parallelism.sh` against the live ceiling — don't raise a test pool's `MaxConns` without reason, and if a new file mints many concurrent clones, sample `pg_stat_activity` during a run to confirm headroom.

**For changes to shared-state concurrency, test parallelism, or database isolation:** verify the affected package under `-race -count=10 -shuffle=on` at the local `-p`/`-parallel` with a confirmed non-empty `TEST_DATABASE_URL` (an empty DSN makes the package self-skip into a false green). Fixed-ID collisions surface only under `-count>=2`. See [`.ai/patterns/test-parallelism.md`](../patterns/test-parallelism.md) for the clone recipe, gotchas, and the full validation matrix.

## Test File Locations

```
backend/tests/
  ├── unit/           # Fast, isolated tests (external package: `package unit`)
  ├── integration/    # Database tests
  └── api/            # HTTP endpoint tests

backend/internal/<pkg>/*_test.go  # Same-package tests (access unexported symbols)
frontend/tests/e2e/               # Playwright browser tests
```

**Same-package vs external-package tests:** Exercise production code, not a copy of its implementation. Test unexported functions in the same package, or cover their behavior through an exported entry point. External-package tests can only access exported symbols; do not duplicate unexported logic to work around that boundary.

## Running Tests

```bash
make test-unit         # Backend unit tests (fast, no DB)
make test-integration  # Backend integration tests (needs DB)
make test-frontend     # Frontend unit tests
make test-e2e          # Full Playwright E2E tests
make test-e2e-local    # Playwright E2E tests (honors PLAYWRIGHT_GREP)
make test              # All backend tests
```

## E2E Area Tags

Specs tag their `describe` blocks with an `@area:` tag naming the user-facing surface they verify (`test.describe('Navigation @area:navigation', …)`), plus `@smoke` on the core flows. A focused local run selects by tag: `make test-e2e-local PLAYWRIGHT_GREP='@area:navigation'`. Tag new specs by the functionality they verify, not the implementation they touch.

## E2E Test Parallelism

E2E tests support parallel execution via Playwright workers. See [`.ai/patterns/e2e-parallelism.md`](../patterns/e2e-parallelism.md) for the prefix contract, the scoping rules, and the global-lock fixture for unscopable singletons.

### Test Isolation with TestAPI

Tests that create/modify data should use the `TestAPI` helper:

```typescript
import { test, expect } from '@playwright/test'
import { createTestAPI, TestAPI, type SeedBehaviorResult } from './helpers/test-api'

test.describe('My Feature', () => {
  let testApi: TestAPI
  let seeded: SeedBehaviorResult

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
    // Declared seeding: name a SPEC BEHAVIOR and read the manifest back.
    seeded = await testApi.seedBehavior('IMP-007')
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should do something', async ({ page }) => {
    // Assertions read names and ids from seeded.entities, never re-derived strings
  })
})
```

### When to Use Serial Mode

If a test creates data **without** using `TestAPI` (e.g., via UI clicks), mark it as serial:

```typescript
test.describe.configure({ mode: 'serial' })
```

**Rule of thumb:**
- Uses `TestAPI` for data → can run in parallel
- Read-only test → can run in parallel
- Creates data via UI without cleanup → mark as serial

### Available TestAPI Methods

| Method | Purpose |
|--------|---------|
| `seedBehavior()` | Seed the fixture DECLARED for a spec behavior and return its manifest — the only provisioning path; there is no bespoke seed endpoint left |
| `seedContactNote()` | Write a contact's notepad note through the product's own notes API |
| `cleanup()` | Two independent sweeps, both always run: hard-delete the contacts this test created through the PRODUCT (matched on this test's name prefix), and remove every declared namespace it seeded |

### Parallel E2E Testing Gotchas

**Shared Database**: All workers share the same database. Tests can see other workers' data, causing pagination and unexpected elements.

**No page.reload() needed**: Import tests use a custom fixture (`./fixtures`) that sets `window.__PLAYWRIGHT__`, which tells React Query to use `staleTime: 0`. This ensures tests always get fresh data after seeding—no reload workarounds needed.

```typescript
// Import from fixtures instead of @playwright/test
import { test, expect } from './fixtures'

// Then just navigate and wait - React Query fetches fresh data automatically
await page.goto('/imports', { waitUntil: 'domcontentloaded' })
await findCandidateByName(page, displayName)  // waits + paginates
```

**Pagination handling**: Other workers' data can push your contact to page 2. Use `findCandidateByName()` helper to paginate until found.

**Modal opens on the clicked candidate**: The resolver modal is keyed by candidate id, so clicking your own card deterministically opens your own candidate even under parallel workers. After opening, assert it with the `expectModalCandidate()` helper so a regression fails loudly instead of acting on the wrong candidate.

**Target specific elements**: Never scope a candidate card by a substring `hasText` filter—it can also match a foreign worker's card whose heading merely contains your name. Use the `candidateCardByName()` helper (`helpers/imports-helpers.ts`), which matches the card's heading with `exact: true`:
```typescript
// ❌ WRONG - substring match can span another worker's card
const card = page.locator('div.border').filter({ hasText: displayName })

// ✅ CORRECT - exact heading match, scoped to your prefixed data
const card = candidateCardByName(page, displayName)
await card.getByRole('button', { name: /Import/i }).click()
```

**Wait for visibility**: Always wait before interacting with seeded data:
```typescript
await expect(candidateCard).toBeVisible({ timeout: 10000 })
await candidateCard.getByRole('button', { name: /Import/i }).click()
```

**Modal stays open**: ImportLinkModal only closes when `candidates.length <= 1`. After importing, verify the card disappeared—don't wait for modal to close.

## Citing behaviors in tests

Follow the canonical [test-to-behavior citation rules](../../spec/README.md#test--behavior-citations)
and [maintenance rule](../../spec/README.md#maintenance-rule). They define citation
placement, stable keys, key minting, coverage, waivers, and same-PR obligations.
Cite the behavior actually exercised by the assertions.

## Writing Good Tests

### Integration Test Template

Use `//go:build integration_testdb` for database-backed test files. Reuse the
package's existing fixture and choose the shared or cloned DB model described
[above](#backend-integration-test-parallelism); do not add per-test migrations to
an already migrated fixture. Build test data with the synthetic harness/factories.

For a real pipeline test with namespace isolation, see
[TestInteractionVenue_LivePath](../../backend/tests/interaction_venue_integration_test.go).
It is a long-running replay example; ordinary repository tests should use the
lighter existing fixture appropriate to their package. Keep migration-subject
tests separate, since their purpose is to exercise migrations themselves.

### Frontend Unit Test Pattern (Vitest)

```typescript
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'

describe('MyFunction', () => {
  beforeEach(() => {
    global.fetch = vi.fn()
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('handles valid input', () => {
    const result = myFunction('input')
    expect(result).toBe(expectedValue)
  })
})
```

### Frontend Component Test Pattern (React Testing Library)

```typescript
import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

describe('Button', () => {
  it('calls onClick when clicked', async () => {
    const user = userEvent.setup()
    const handleClick = vi.fn()

    render(<Button onClick={handleClick}>Click me</Button>)
    await user.click(screen.getByRole('button'))

    expect(handleClick).toHaveBeenCalledOnce()
  })
})
```

### Backend API Response Parsing

All API responses are wrapped in `api.APIResponse`:

```go
// ❌ WRONG - direct unmarshaling fails
var contact Contact
json.Unmarshal(w.Body.Bytes(), &contact)  // contact.ID will be empty

// ✅ CORRECT - unwrap from api.APIResponse first
var resp api.APIResponse
json.Unmarshal(w.Body.Bytes(), &resp)
require.True(t, resp.Success)
data := resp.Data.(map[string]interface{})
contactID := data["id"].(string)  // now works
```

This applies to all test helpers that call API endpoints.

### Key Principles

1. **Test edge cases** - not just happy path
2. **Verify unrelated data is unaffected** - ensure operations are scoped correctly
3. **Use descriptive test names** - `TestContactRepository_SoftDelete_DoesNotAffectOtherContacts`
4. **Clean up after tests** - use defer or afterEach hooks

## Service Layer Testing with External APIs

When testing services that call external APIs (Todoist, OAuth, etc.), use the client factory pattern:

### Pattern: Client Factory Injection

```go
// 1. Define interface matching the external client
type Client interface {
    QuickAdd(ctx context.Context, text string) (*Task, error)
    Sync(ctx context.Context, token string, commands []Command) error
}

// 2. Add factory to service struct
type ContactTaskService struct {
    // ... other deps ...
    todoistClientFunc ClientFactory
    testAccessToken   string  // bypasses OAuth in tests
}

// 3. Create test constructor that bypasses OAuth
func NewContactTaskServiceForTest(deps..., testToken string) *ContactTaskService {
    return &ContactTaskService{
        // ... deps ...
        todoistClientFunc: DefaultClientFactory,
        testAccessToken:   testToken,
    }
}

// 4. Allow test to override the factory
func (s *Service) SetClientFactory(factory ClientFactory) {
    s.clientFunc = factory
}
```

### Pattern: Mock with Call History

```go
type mockClient struct {
    quickAddCalls []quickAddCall  // track calls for assertions
    syncCalls     []syncCall
    quickAddFunc  func(ctx, text, note) (*Task, error)  // custom behavior
}

func (m *mockClient) QuickAdd(ctx, text, note) (*Task, error) {
    m.quickAddCalls = append(m.quickAddCalls, quickAddCall{text, note})
    if m.quickAddFunc != nil {
        return m.quickAddFunc(ctx, text, note)
    }
    // Generate unique ID to avoid constraint violations across subtests
    taskID := "test-task-" + uuid.New().String()[:8]
    return &Task{ID: taskID}, nil
}
```

### Key Rules

1. **Generate unique IDs** - Use UUIDs, not hardcoded values. Each subtest creates a new mock with counter reset, causing duplicate key violations.

2. **Capture values for assertions** - Don't hardcode expected IDs:
   ```go
   // ❌ WRONG - breaks when mock generates dynamic IDs
   assert.Equal(t, "test-task-id", cmd.Args["id"])

   // ✅ CORRECT - capture from mock
   assert.Equal(t, capturedTaskID, cmd.Args["id"])
   ```

3. **Clean up test data proactively** - Delete by account ID before creating:
   ```go
   _ = syncRepo.DeleteSyncStatesByAccountID(ctx, "test-account-123")
   syncState, err := syncRepo.CreateSyncState(ctx, ...)
   ```

4. **Fail and clean up on API failure** - If step 2 fails after step 1 succeeds, clean up step 1's side effects:
   ```go
   if err != nil {
       deleteCmd := NewItemDeleteCommand(task.ID)
       _, _ = client.Sync(ctx, "*", []string{}, deleteCmd)
       return nil, fmt.Errorf("update failed: %w", err)
   }
   ```

## E2E Date Testing

When testing date display in E2E tests, use UTC date components if the backend stores UTC timestamps:

```typescript
// ❌ WRONG - fails late at night when UTC has rolled to next day
const today = new Date().toLocaleDateString()

// ✅ CORRECT - matches how formatDateOnly extracts UTC date portion
const now = new Date()
const today = `${now.getUTCMonth() + 1}/${now.getUTCDate()}/${now.getUTCFullYear()}`
```

The `formatDateOnly` utility extracts the UTC date portion from ISO strings (e.g., `2026-01-20T06:00:00Z` → `1/20/2026`), so displayed dates show UTC dates, not local dates. Tests using local date methods like `toLocaleDateString()` will fail when run late at night and UTC has already rolled over to the next day.
