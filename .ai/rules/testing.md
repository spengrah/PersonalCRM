# Testing Rules

## Before Pushing

**Always run the required test suite locally before pushing:**

```bash
make test         # All backend tests (unit + integration)
make test-e2e-diff # Diff-selected Playwright E2E tests (core + impacted)
```

CI runs the full E2E suite; local runs use diff selection for speed.

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

See [`.ai/patterns/synthetic-seed-toolkit.md`](../patterns/synthetic-seed-toolkit.md) for the factory/replay/profile how-to.

## Backend Integration-Test Parallelism

New backend integration tests run with `t.Parallel()` by default. The suite was converted to within-run parallelism across #430 (`backend/tests`), #438 (`backend/tests/api`), and #428 (the river-heavy core); a new serial test silently widens the suite's serial prefix (Go runs the entire non-`t.Parallel()` cohort to completion before the parallel cohort starts), so default to parallel and only opt out for the documented serial cases below.

**Know which DB model your test uses — it decides how you make it parallel-safe:**

- **Shared package DB** (`backend/tests` and `backend/tests/api`, via `newSharedTestDB` / `newAPISharedTestDB`): one `MaxConns=8` pool per package, shared across that package's parallel tests. The parallel-safety lever here is **namespace scoping** — scope every read/assertion to your own namespace (`syntheticNS(t)` / `synthetic.NewHarnessForNamespace`; see Build State With the Synthetic Toolkit). Never assert over a global/DB-wide count, or compare counts across two queries — a sibling test can change rows between them.
- **Per-test ephemeral clone** (`testdb.NewEphemeralClone`, surfaced through the `newIsolatedRiverTestDB` helper): each test mints its own clone DB (copied from the content-hashed, pre-migrated template) plus its own pool + River client, dropped on `t.Cleanup`. Use this when a test needs true isolation (see the River rule). A clone is pre-migrated — do NOT call `db.RunMigrations` against it.

**River-touching tests → per-test clone.** A live River client (`client.Start(ctx)`) draining `river_job` on a shared DB steals sibling tests' jobs, and DB-wide `river_job` count/delete assertions collide. Any test that starts a worker, asserts DB-wide over `river_job`, or relies on fixed-ID fixtures (fixed `external_task_id`, fixed chat/message IDs) must isolate via `newIsolatedRiverTestDB` before flipping to `t.Parallel()` — the private clone makes those collisions impossible by construction (no ID renaming needed). Enqueue-only tests (build a `TestOnly` client but never `Start` it) can stay on the shared DB.

**Stays serial — the documented exceptions:**

- Singleton-table tests (`mac_host_*` auth in `tests/api`): the auth tables are global singletons; per-test cloning was tried and correctly reverted. Keep serial.
- Migration-subject tests (e.g. `TestRunMigrations_River_Integration`): the migration runner is the subject under test, so a pre-migrated clone is meaningless. Keep serial.

**Connection budget.** Concurrent clones/pools share one Postgres (`max_connections=200` on CI and recreated-local; 100 on a stock/un-recreated local container). Each shared-DB pool ≈ 8 conns; each isolated-river clone ≈ 7 (pool 6 + River's LISTEN conn). `-p`/`-parallel` are computed by `scripts/test-parallelism.sh` against the live ceiling — don't raise a test pool's `MaxConns` without reason, and if a new file mints many concurrent clones, sample `pg_stat_activity` during a run to confirm headroom.

**Prove parallel-safety before claiming done:** run the changed package under `-race -count=10 -shuffle=on` at the local `-p`/`-parallel` with a confirmed non-empty `TEST_DATABASE_URL` (an empty DSN makes the package self-skip into a false green). Fixed-ID collisions surface only under `-count>=2`. See [`.ai/patterns/test-parallelism.md`](../patterns/test-parallelism.md) for the clone recipe, gotchas, and the full validation matrix.

## Test File Locations

```
backend/tests/
  ├── unit/           # Fast, isolated tests (external package: `package unit`)
  ├── integration/    # Database tests
  └── api/            # HTTP endpoint tests

backend/internal/<pkg>/*_test.go  # Same-package tests (access unexported symbols)
frontend/tests/e2e/               # Playwright browser tests
```

**Same-package vs external-package tests:** Tests for unexported methods/functions (lowercase names like `tryRecoverPendingTempID`) must live in `*_test.go` files within the same package directory (e.g., `backend/internal/todoist/provider_test.go` with `package todoist`). Tests in `backend/tests/unit/` use external package naming (`package unit`) and can only access exported symbols. When testing unexported logic from an external package, mirror the logic in the test (see `TestIsPendingTempIDLogic` pattern in `backend/tests/unit/todoist_test.go`).

## Running Tests

```bash
make test-unit         # Backend unit tests (fast, no DB)
make test-integration  # Backend integration tests (needs DB)
make test-frontend     # Frontend unit tests
make test-e2e          # Full Playwright E2E tests
make test-e2e-local    # Playwright E2E tests (honors PLAYWRIGHT_GREP)
make test-e2e-diff     # Diff-selected E2E tests (core + impacted)
make test              # All backend tests
```

## E2E Diff Selection

Local E2E runs use tags and a path-to-tag map to run only tests affected by code changes.

### How It Works

1. `scripts/run-e2e-local.mjs` detects changed files via `git diff`
2. Matches changed paths against patterns in `frontend/tests/e2e/test-map.json`
3. Collects tags from matched entries (e.g., `@area:navigation`)
4. Runs tests whose titles contain those tags

### Test Tags

Tags appear in test `describe` blocks:

```typescript
test.describe('Navigation @area:navigation', () => {
  // All tests here run when @area:navigation is triggered
})
```

Available tags: `@area:dashboard`, `@area:contacts`, `@area:imports`, `@area:navigation`, `@area:settings`, `@area:meetings`, `@area:overdue`, `@area:contact-merge`, `@area:contact-navigation`, `@area:error-boundary`

### test-map.json Structure

Each entry maps a file pattern to tags that should run when that file changes:

```json
{
  "pattern": "^frontend/src/components/layout/",
  "tags": ["@area:navigation", "@area:dashboard"]
}
```

### Adding/Updating Tags

**New tests in existing area:** Add the appropriate `@area:` tag to your `describe` block. No map changes needed.

**New source file in existing area:** Check if an existing pattern covers it. If not, add a pattern entry.

**New feature area:**
1. Create a new `@area:yourfeature` tag
2. Add it to relevant test `describe` blocks
3. Add map entries for source files that affect those tests

### Choosing Tags

Tag tests based on what user-facing functionality they verify, not implementation details. A source file may map to multiple tags if it affects multiple areas.

### Rules

- Always keep a small `@smoke` set for core flows
- Add area tags to new specs
- Update the map when adding new pages/areas

## E2E Test Parallelism

E2E tests support parallel execution via Playwright workers.

### Test Isolation with TestAPI

Tests that create/modify data should use the `TestAPI` helper:

```typescript
import { test, expect } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

test.describe('My Feature', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
    await testApi.seedExternalContacts([
      { display_name: 'Test User', emails: ['test@example.com'] }
    ])
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should do something', async ({ page }) => {
    // Test code - data is isolated per worker
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
| `seedExternalContacts()` | Create import candidates |
| `seedOverdueContacts()` | Create contacts with backdated last_contacted |
| `seedCalendarEvents()` | Create calendar events for a contact |
| `seedContacts()` | Create contacts directly |
| `cleanup()` | Remove all data with this test's prefix |

### Parallel E2E Testing Gotchas

**Shared Database**: All workers share the same database. Tests can see other workers' data, causing pagination and unexpected elements.

**No page.reload() needed**: Import tests use a custom fixture (`./fixtures`) that sets `window.__PLAYWRIGHT__`, which tells React Query to use `staleTime: 0`. This ensures tests always get fresh data after seeding—no reload workarounds needed.

```typescript
// Import from fixtures instead of @playwright/test
import { test, expect } from './fixtures'

// Then just navigate and wait - React Query fetches fresh data automatically
await page.goto('/imports')
await page.waitForLoadState('networkidle')
await findCandidateByName(page, displayName)  // waits + paginates
```

**Pagination handling**: Other workers' data can push your contact to page 2. Use `findCandidateByName()` helper to paginate until found.

**Modal navigation**: When opening a modal, parallel tests may cause it to show another candidate. Use `navigateModalToCandidate()` helper which uses element-based waits (not fixed timeouts) to navigate to your candidate.

**Target specific elements**: Never use `.first()` on candidate cards—target your prefixed contact name:
```typescript
// ❌ WRONG - may click another worker's data
page.getByRole('button', { name: /Import/i }).first().click()

// ✅ CORRECT - targets your prefixed data
const card = page.locator('[class*="border-gray-200"]').filter({ hasText: displayName })
await card.getByRole('button', { name: /Import/i }).click()
```

**Wait for visibility**: Always wait before interacting with seeded data:
```typescript
await expect(candidateCard).toBeVisible({ timeout: 10000 })
await candidateCard.getByRole('button', { name: /Import/i }).click()
```

**Modal stays open**: ImportLinkModal only closes when `candidates.length <= 1`. After importing, verify the card disappeared—don't wait for modal to close.

## Citing behaviors in tests

New and deliberately-relaxed tests cite the `spec/*.yaml` behavior IDs they prove with a line comment: `// spec: <ID>[, <ID> ...]`. Put the marker next to the assertions that prove the behavior — function-level (immediately preceding the `func TestXxx` / `test(...)` / `test.describe(...)`) or subtest-level (first line inside the `t.Run` / `test(...)` body). Cite only `status: current` behaviors the test actually asserts green; generic contracts that no behavior owns carry no marker.

```go
t.Run("rescan with an eligible method returns a pollable job", func(t *testing.T) {
    // spec: IMP-021
    ...
})
```

```ts
// spec: CAL-019
test('adding a matching email links a past event', async ({ page, request }) => { ... })
```

Full grammar, placement rules, cite-on-write policy, and scanner-readiness: `spec/README.md` → [Test → behavior citations](../../spec/README.md#test--behavior-citations).

## Writing Good Tests

### Integration Test Template

```go
func TestNewTableRepository_Integration(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping integration test")
    }

    ctx := context.Background()
    db := setupTestDB(t)
    defer cleanupTestDB(t, db)

    repo := repository.NewRepository(db.Queries)

    t.Run("Create", func(t *testing.T) {
        item, err := repo.Create(ctx, request)
        require.NoError(t, err)
        assert.NotEmpty(t, item.ID)
    })
}
```

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
