# Testing Guide - Personal CRM

This guide covers testing strategies, tools, and best practices for the Personal CRM project.

## Overview

The project uses a comprehensive testing strategy across three layers:

```
        E2E Tests (Playwright)
       - Full user workflows
      - Browser automation
     - Tests entire stack

       Integration Tests (Go)
      - Database operations
     - Repository layer
    - Real PostgreSQL

      Unit Tests
     - Pure functions
    - Individual components
   - Fast execution
```

## Frontend Testing (Vitest + React Testing Library)

### Setup

**Framework**: Vitest (fast, Vite-native test runner)
**Component Testing**: React Testing Library
**DOM Environment**: jsdom

**Test Location**: `frontend/src/**/__tests__/*.test.{ts,tsx}`

### Running Frontend Tests

```bash
# Run all frontend tests once
cd frontend && bun run test

# Watch mode (re-runs tests on changes)
bun run test:watch

# Generate coverage report
bun run test:coverage
```

### What to Test

#### ✅ High Priority

1. **Utility Functions**
   - Date parsing/formatting
   - Data transformations
   - Pure helper functions

2. **API Client**
   - HTTP request/response handling
   - Error handling
   - Timeout behavior

3. **Validation Schemas**
   - Zod schema validation
   - Transform functions
   - Edge cases

4. **Error Boundaries**
   - Error catching
   - Error UI rendering
   - Recovery mechanisms

#### ⚠️ Medium Priority

5. **React Hooks**
   - Custom hooks logic
   - State management
   - Side effects

6. **Forms**
   - Form validation
   - Submit handling
   - Error states

#### ⬇️ Lower Priority

7. **UI Components**
   - Mostly presentational
   - Better tested via E2E
   - Test only critical logic

### Test File Organization

```
frontend/src/
├── lib/
│   ├── __tests__/
│   │   ├── utils.test.ts           # Date utilities
│   │   └── api-client.test.ts      # API client
│   ├── validations/
│   │   └── __tests__/
│   │       └── contact.test.ts     # Zod schemas
│   └── utils.ts
└── components/
    ├── __tests__/
    │   └── error-boundary.test.tsx # React components
    └── error-boundary.tsx
```

### Example Tests

#### Testing Utilities

```typescript
import { describe, it, expect } from 'vitest'
import { parseDateOnly } from '../utils'

describe('parseDateOnly', () => {
  it('parses valid YYYY-MM-DD format', () => {
    const result = parseDateOnly('2024-01-15')
    expect(result).toBeInstanceOf(Date)
    expect(result?.getFullYear()).toBe(2024)
  })

  it('returns null for invalid input', () => {
    expect(parseDateOnly(null)).toBeNull()
    expect(parseDateOnly('')).toBeNull()
  })
})
```

#### Testing API Clients

```typescript
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { apiClient, ApiError } from '../api-client'

describe('ApiClient', () => {
  beforeEach(() => {
    global.fetch = vi.fn()
  })

  it('makes successful GET request', async () => {
    const mockData = { id: '123', name: 'Test' }
    ;(global.fetch as any).mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: async () => ({ success: true, data: mockData })
    })

    const result = await apiClient.get('/api/test')
    expect(result).toEqual(mockData)
  })

  it('throws ApiError on 404', async () => {
    ;(global.fetch as any).mockResolvedValueOnce({
      ok: false,
      status: 404,
      json: async () => ({
        error: { code: 'NOT_FOUND', message: 'Not found' }
      })
    })

    try {
      await apiClient.get('/api/test')
      expect.fail('Should have thrown')
    } catch (error) {
      expect(error).toBeInstanceOf(ApiError)
      expect((error as ApiError).status).toBe(404)
    }
  })
})
```

#### Testing Zod Schemas

```typescript
import { describe, it, expect } from 'vitest'
import { contactSchema } from '../validations/contact'

describe('contactSchema', () => {
  it('validates correct data', () => {
    const result = contactSchema.safeParse({
      full_name: 'John Doe',
      email: 'john@example.com'
    })
    expect(result.success).toBe(true)
  })

  it('rejects invalid email', () => {
    const result = contactSchema.safeParse({
      full_name: 'John Doe',
      email: 'not-an-email'
    })
    expect(result.success).toBe(false)
  })
})
```

#### Testing React Components

```typescript
import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Button } from '../button'

describe('Button', () => {
  it('renders with text', () => {
    render(<Button>Click me</Button>)
    expect(screen.getByRole('button')).toHaveTextContent('Click me')
  })

  it('calls onClick when clicked', async () => {
    const user = userEvent.setup()
    const handleClick = vi.fn()

    render(<Button onClick={handleClick}>Click</Button>)
    await user.click(screen.getByRole('button'))

    expect(handleClick).toHaveBeenCalledOnce()
  })
})
```

### Best Practices

#### ✅ DO

- **Test behavior, not implementation** - Focus on what the code does, not how
- **Use descriptive test names** - `it('returns null for invalid input')`
- **Arrange-Act-Assert pattern** - Set up → Execute → Verify
- **Test edge cases** - null, undefined, empty strings, boundary values
- **Mock external dependencies** - API calls, timers, browser APIs
- **Suppress expected console errors** - Use `vi.fn()` to mock console.error in tests

#### ❌ DON'T

- **Don't test third-party libraries** - Trust that React, Zod, etc. work
- **Don't test implementation details** - Avoid testing internal state
- **Don't write brittle tests** - Avoid relying on exact class names/structure
- **Don't skip cleanup** - Always restore mocks in `afterEach`

## Backend Testing (Go)

### Setup

**Framework**: Go's built-in `testing` package
**Assertions**: `testify/assert` and `testify/require`
**Database**: Real PostgreSQL for integration tests

**Test Location**: `backend/tests/{unit,integration,api}/`

### Running Backend Tests

```bash
# Run all backend tests
make test

# Unit tests only (fast)
make test-unit

# Integration tests (requires DB)
make test-integration

# With verbose output
go test -v ./backend/tests/...

# Run specific test
go test -v ./backend/tests/unit -run TestCadenceCalculation
```

### Test Types

#### Unit Tests (`backend/tests/unit/`)

Test individual functions in isolation:
- Business logic calculations
- Validation logic
- Pure functions

```go
func TestCadenceCalculation(t *testing.T) {
    tests := []struct {
        name     string
        input    string
        expected int
    }{
        {"weekly", "weekly", 7},
        {"monthly", "monthly", 30},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result := CalculateCadenceDays(tt.input)
            assert.Equal(t, tt.expected, result)
        })
    }
}
```

#### Integration Tests (`backend/tests/integration/`)

Test database operations with real PostgreSQL:
- Repository CRUD operations
- Database constraints
- Transaction handling

```go
func TestContactRepository_Integration(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping integration test")
    }

    ctx := context.Background()
    db := setupTestDB(t)
    defer cleanupTestDB(t, db)

    repo := repository.NewContactRepository(db.Queries)

    t.Run("CreateAndGet", func(t *testing.T) {
        contact, err := repo.CreateContact(ctx, CreateContactRequest{
            FullName: "Test User",
        })
        require.NoError(t, err)

        fetched, err := repo.GetContact(ctx, contact.ID)
        require.NoError(t, err)
        assert.Equal(t, contact.FullName, fetched.FullName)
    })
}
```

### Best Practices

#### ✅ DO

- **Use table-driven tests** - Test multiple cases efficiently
- **Use `require` for critical assertions** - Stops test on failure
- **Use `assert` for non-critical checks** - Continues test on failure
- **Clean up test data** - Use `defer` to ensure cleanup
- **Test error cases** - Don't just test the happy path

#### ❌ DON'T

- **Don't use `time.Now()` directly** - Use `accelerated.GetCurrentTime()`
- **Don't share state between tests** - Each test should be independent
- **Don't skip integration tests** - They catch real-world issues

## E2E Testing (Playwright)

### Setup

**Framework**: Playwright
**Location**: `frontend/tests/e2e/`
**Config**: `frontend/playwright.config.ts`

The tests run from the frontend directory to resolve `@playwright/test` from `frontend/node_modules`. The Makefile handles this automatically.

### Running E2E Tests

```bash
# Run all E2E tests (recommended)
make test-e2e  # Uses .env.example.testing, starts Docker, syncs DB password

# Run in headed mode (see browser)
cd frontend && bunx playwright test --headed

# Run specific test file
cd frontend && bunx playwright test tests/e2e/dashboard.spec.ts

# Run specific browser only
make test-e2e  # Default: chromium only (faster)
cd frontend && bunx playwright test --project=firefox  # Run Firefox
```

### What to Test

Focus on **critical user workflows**:

1. **Contact Management**
   - Create contact → View in list → Edit → Delete

2. **Reminder System**
   - Create reminder → Mark complete → View stats

3. **Time Tracking**
   - Log time entry → View in dashboard

4. **Error Handling**
   - Trigger ErrorBoundary → Reload page

### Best Practices

#### ✅ DO

- **Test complete workflows** - Full user journeys
- **Use stable selectors** - `role`, `testid`, not CSS classes
- **Wait for elements** - Use Playwright's auto-waiting
- **Test realistic scenarios** - What users actually do

#### ❌ DON'T

- **Don't test every detail** - E2E tests are slow/expensive
- **Don't rely on brittle selectors** - Avoid coupling to DOM structure
- **Don't skip unit/integration tests** - E2E should complement, not replace

## Testing Strategy

### Test Pyramid

```
       E2E (Few)
      - Slow
     - Expensive
    - Full stack

     Integration (Some)
    - Medium speed
   - Database required
  - Layer integration

    Unit (Many)
   - Fast
  - No dependencies
 - Focused
```

**Rule of thumb**:
- 70% Unit tests
- 20% Integration tests
- 10% E2E tests

### When to Write What

| Scenario | Test Type |
|----------|-----------|
| Pure function logic | Unit |
| Database operations | Integration |
| API endpoint behavior | Integration or E2E |
| User workflow | E2E |
| Component rendering | Unit (React Testing Library) |
| Form validation | Unit (Zod schema) |

## Continuous Integration

Tests run automatically on:
- Every commit (unit tests)
- Pull requests (unit + integration)
- Pre-deployment (all tests including E2E)

## Coverage

Coverage is tracked but **not enforced**. Focus on testing:
1. Critical business logic
2. Error-prone code
3. Bug-fix regressions

Don't chase 100% coverage - test what matters.

## Debugging Tests

### Frontend (Vitest)

```bash
# Run tests in watch mode
bun run test:watch

# Debug specific test
# Add `console.log()` in test, or use browser debugger
```

### Backend (Go)

```bash
# Verbose output
go test -v ./backend/tests/unit

# Run specific test
go test -v -run TestSpecificTest

# Print statements
# Use `fmt.Printf()` in tests
```

### E2E (Playwright)

```bash
# Headed mode (see browser)
npm test -- --headed

# Debug mode (pause on failure)
npm test -- --debug

# Trace viewer (after failure)
npx playwright show-trace trace.zip
```

## Resources

- [Vitest Documentation](https://vitest.dev/)
- [React Testing Library](https://testing-library.com/react)
- [Playwright Docs](https://playwright.dev/)
- [Go Testing](https://go.dev/doc/tutorial/add-a-test)

---

*For code examples and patterns, see [`.ai/patterns.md`](.ai/patterns.md)*
