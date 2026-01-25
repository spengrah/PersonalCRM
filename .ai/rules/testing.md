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

## Test File Locations

```
backend/tests/
  ├── unit/           # Fast, isolated tests
  ├── integration/    # Database tests
  └── api/            # HTTP endpoint tests

frontend/tests/e2e/   # Playwright browser tests
```

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
