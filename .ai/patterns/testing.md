# Testing Patterns

Code templates and examples for writing tests.

## Go Unit Test Pattern

```go
func TestCadenceCalculation(t *testing.T) {
    tests := []struct {
        name        string
        cadence     reminder.CadenceType
        lastContact time.Time
        checkTime   time.Time
        wantOverdue bool
    }{
        {
            name:        "weekly not overdue",
            cadence:     reminder.CadenceWeekly,
            lastContact: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
            checkTime:   time.Date(2024, 1, 6, 0, 0, 0, 0, time.UTC),
            wantOverdue: false,
        },
        {
            name:        "weekly overdue",
            cadence:     reminder.CadenceWeekly,
            lastContact: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
            checkTime:   time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC),
            wantOverdue: true,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got := reminder.IsOverdue(tt.cadence, &tt.lastContact, tt.checkTime)
            assert.Equal(t, tt.wantOverdue, got)
        })
    }
}
```

## Go Integration Test Pattern

```go
func TestContactRepository_Integration(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping integration test")
    }

    ctx := context.Background()
    db := setupTestDB(t)
    defer cleanupTestDB(t, db)

    repo := repository.NewContactRepository(db.Queries)

    t.Run("Create and Get", func(t *testing.T) {
        // Create
        created, err := repo.CreateContact(ctx, repository.CreateContactRequest{
            FullName: "Test User",
            Email:    stringPtr("test@example.com"),
        })
        require.NoError(t, err)
        assert.NotEmpty(t, created.ID)

        // Get
        fetched, err := repo.GetContact(ctx, created.ID)
        require.NoError(t, err)
        assert.Equal(t, created.FullName, fetched.FullName)
    })

    t.Run("Soft Delete", func(t *testing.T) {
        created, _ := repo.CreateContact(ctx, repository.CreateContactRequest{
            FullName: "To Delete",
        })

        err := repo.SoftDeleteContact(ctx, created.ID)
        require.NoError(t, err)

        // Should not be found
        _, err = repo.GetContact(ctx, created.ID)
        assert.Error(t, err)
    })
}
```

## Frontend Unit Test Pattern (Vitest)

**Testing utilities:**
```typescript
import { describe, it, expect } from 'vitest'

describe('utilityFunction', () => {
  it('handles valid input', () => {
    const result = utilityFunction('valid input')
    expect(result).toBe(expectedValue)
  })

  it('handles null input', () => {
    const result = utilityFunction(null)
    expect(result).toBeNull()
  })
})
```

**Testing API clients with mocks:**
```typescript
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { apiClient, ApiError } from '../api-client'

describe('ApiClient', () => {
  beforeEach(() => {
    global.fetch = vi.fn()
  })

  afterEach(() => {
    vi.restoreAllMocks()
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

  it('handles errors', async () => {
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

**Testing Zod schemas:**
```typescript
import { describe, it, expect } from 'vitest'
import { contactSchema } from '../validations/contact'

describe('contactSchema', () => {
  it('validates correct data', () => {
    const validData = {
      full_name: 'John Doe',
      email: 'john@example.com'
    }

    const result = contactSchema.safeParse(validData)
    expect(result.success).toBe(true)
  })

  it('rejects invalid data', () => {
    const invalidData = {
      full_name: '',
      email: 'not-an-email'
    }

    const result = contactSchema.safeParse(invalidData)
    expect(result.success).toBe(false)
  })
})
```

## Frontend Component Test Pattern (React Testing Library)

**Basic component rendering:**
```typescript
import { describe, it, expect } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MyComponent } from '../my-component'

describe('MyComponent', () => {
  it('renders with props', () => {
    render(<MyComponent title="Test Title" />)
    expect(screen.getByText('Test Title')).toBeInTheDocument()
  })
})
```

**Testing user interactions:**
```typescript
import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Button } from '../button'

describe('Button', () => {
  it('calls onClick handler when clicked', async () => {
    const user = userEvent.setup()
    const handleClick = vi.fn()

    render(<Button onClick={handleClick}>Click me</Button>)

    const button = screen.getByRole('button', { name: /click me/i })
    await user.click(button)

    expect(handleClick).toHaveBeenCalledOnce()
  })

  it('is disabled when disabled prop is true', () => {
    render(<Button disabled>Disabled</Button>)

    const button = screen.getByRole('button')
    expect(button).toBeDisabled()
  })
})
```

## E2E Test Pattern (Playwright)

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
    await page.goto('/contacts')
    await expect(page.getByText('Test User')).toBeVisible()
  })
})
```

## Running Tests

```bash
# Backend
make test-unit         # Fast, isolated tests
make test-integration  # Database tests

# Frontend
bun run test           # All tests
bun run test:watch     # Watch mode
bun run test:coverage  # With coverage

# E2E
make test-e2e          # Playwright tests
```
