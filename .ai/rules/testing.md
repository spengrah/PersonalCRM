# Testing Rules

## Before Pushing

**Always run the full test suite locally before pushing:**

```bash
make test         # All backend tests (unit + integration)
make test-e2e     # Playwright E2E tests
```

This catches issues that CI would flag, avoiding multiple push-fix-push cycles.

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
make test-e2e          # Playwright E2E tests
make test              # All backend tests
```

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

### Key Principles

1. **Test edge cases** - not just happy path
2. **Verify unrelated data is unaffected** - ensure operations are scoped correctly
3. **Use descriptive test names** - `TestContactRepository_SoftDelete_DoesNotAffectOtherContacts`
4. **Clean up after tests** - use defer or afterEach hooks
