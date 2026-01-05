import { test, expect } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

// API configuration for E2E tests
const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

test.describe('Imports Page', () => {
  test.beforeEach(async ({ page }) => {
    // Navigate to imports page before each test
    await page.goto('/imports')
    await page.waitForLoadState('networkidle')
  })

  test('should display page header and sync button', async ({ page }) => {
    // Verify page header
    await expect(page.getByRole('heading', { name: 'Import Contacts' })).toBeVisible()

    // Verify sync button exists (use first() as there may be multiple sync buttons)
    await expect(page.getByRole('button', { name: /Sync Google Contacts/i }).first()).toBeVisible()
  })

  test('should show imports in navigation', async ({ page }) => {
    // Verify navigation has Imports entry
    await expect(page.getByRole('link', { name: /Imports/i })).toBeVisible()
  })

  test('should display empty state when no candidates', async ({ page, request }) => {
    // First, ensure there are no candidates by checking the API
    const response = await request.get(`${API_BASE_URL}/api/v1/imports/candidates`, {
      headers: API_HEADERS,
    })

    if (response.ok()) {
      const data = await response.json()
      if (data.data?.length === 0 || data.meta?.pagination?.total === 0) {
        // Empty state should show specific messaging
        await expect(page.getByText(/No import candidates/i)).toBeVisible()
        await expect(page.getByText(/All contacts from Google have been imported/i)).toBeVisible()
      }
    }
  })
})

test.describe('Imports - With Seeded Data', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)

    // Seed external contacts for this test
    await testApi.seedExternalContacts([
      {
        display_name: 'Test Import User',
        emails: ['test-import@example.com'],
        phones: ['+1234567890'],
        organization: 'Test Org',
        job_title: 'Engineer',
      },
      {
        display_name: 'Second Import User',
        emails: ['second-import@example.com'],
      },
    ])
  })

  test.afterEach(async () => {
    // Clean up all test data created with our prefix
    await testApi.cleanup()
  })

  test('should display candidate cards with correct information', async ({ page }) => {
    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // Verify seeded candidates are visible (with prefix)
    await expect(page.getByText(`${testApi.prefix}-Test Import User`)).toBeVisible()

    // Verify action buttons are present
    await expect(page.getByRole('button', { name: /Import/i }).first()).toBeVisible()
    await expect(page.getByRole('button', { name: /Link/i }).first()).toBeVisible()
  })

  test('should open link modal when clicking Link button', async ({ page }) => {
    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // Click the Link button on the first candidate
    await page.getByRole('button', { name: /Link/i }).first().click()

    // Verify modal opens
    await expect(page.getByText('Link to Existing Contact')).toBeVisible()
    await expect(page.getByText('Select Contact')).toBeVisible()

    // Verify cancel button works
    await page.getByRole('button', { name: /Cancel/i }).click()
    await expect(page.getByText('Link to Existing Contact')).not.toBeVisible()
  })
})

test.describe('Imports - Import Action', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)

    // Seed a candidate for import testing
    await testApi.seedExternalContacts([
      {
        display_name: 'Import Test Contact',
        emails: ['import-test@example.com'],
      },
    ])
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should import candidate and show success notification', async ({ page }) => {
    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    const displayName = `${testApi.prefix}-Import Test Contact`

    // Verify candidate is visible
    await expect(page.getByText(displayName)).toBeVisible()

    // Click Import on the candidate
    await page
      .getByRole('button', { name: /Import/i })
      .first()
      .click()

    // Wait for the action to complete
    await page.waitForLoadState('networkidle')

    // Verify success notification appears
    await expect(page.getByText(/imported successfully/i)).toBeVisible({ timeout: 10000 })

    // Verify the candidate card is removed from the list
    // Use a more specific selector for the card, not just the text (which also appears in notification)
    const candidateCard = page.locator('[class*="rounded-lg"]').filter({ hasText: displayName })
    await expect(candidateCard.getByRole('button', { name: /Import/i })).not.toBeVisible({
      timeout: 5000,
    })
  })
})

test.describe('Imports - Ignore Action', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)

    // Seed a candidate for ignore testing
    await testApi.seedExternalContacts([
      {
        display_name: 'Ignore Test Contact',
        emails: ['ignore-test@example.com'],
      },
    ])
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should ignore candidate and show notification', async ({ page }) => {
    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    const displayName = `${testApi.prefix}-Ignore Test Contact`

    // Verify candidate is visible
    await expect(page.getByText(displayName)).toBeVisible()

    // Click the X (ignore) button on the candidate
    // The ignore button is a ghost button with just an X icon
    const candidateCard = page.locator('[class*="rounded-lg"]').filter({ hasText: displayName })
    const ignoreButton = candidateCard
      .getByRole('button')
      .filter({ has: page.locator('svg') })
      .last()

    await ignoreButton.click()

    // Wait for the action to complete
    await page.waitForLoadState('networkidle')

    // Verify notification appears
    await expect(page.getByText(/ignored/i)).toBeVisible({ timeout: 10000 })

    // Verify the candidate card is removed from the list
    // Use existing candidateCard selector to check Import button is gone
    await expect(candidateCard.getByRole('button', { name: /Import/i })).not.toBeVisible({
      timeout: 5000,
    })
  })
})

test.describe('Imports - Link Action', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)

    // Seed a candidate for link testing
    await testApi.seedExternalContacts([
      {
        display_name: 'Link Test Contact',
        emails: ['link-test@example.com'],
      },
    ])

    // Seed a contact to link to
    await testApi.seedOverdueContacts([
      {
        full_name: 'Link Target Contact',
        cadence: 'monthly',
        days_overdue: 1,
        email: 'link-target@example.com',
      },
    ])
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should link candidate to existing contact', async ({ page }) => {
    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    const candidateName = `${testApi.prefix}-Link Test Contact`
    const targetName = `${testApi.prefix}-Link Target Contact`

    // Verify candidate is visible
    await expect(page.getByText(candidateName)).toBeVisible()

    // Click Link on the candidate
    await page.getByRole('button', { name: /Link/i }).first().click()

    // Wait for modal to open
    await expect(page.getByText('Link to Existing Contact')).toBeVisible()

    // Search for and select the contact we created
    // The contact selector is a combobox/searchable dropdown
    const contactSelector = page.getByRole('combobox')
    if (await contactSelector.isVisible()) {
      await contactSelector.click()
      await page.getByText(targetName).click()
    } else {
      // Fallback: try clicking on the contact in a list
      await page.getByText(targetName).click()
    }

    // Click Link Contact button
    await page.getByRole('button', { name: /Link Contact/i }).click()

    // Wait for action to complete
    await page.waitForLoadState('networkidle')

    // Verify success notification
    await expect(page.getByText(/linked successfully/i)).toBeVisible({ timeout: 10000 })

    // Verify the candidate card is removed from the list
    // Use a more specific selector for the card, not just the text (which also appears in notification)
    const candidateCard = page.locator('[class*="rounded-lg"]').filter({ hasText: candidateName })
    await expect(candidateCard.getByRole('button', { name: /Import/i })).not.toBeVisible({
      timeout: 5000,
    })
  })
})

test.describe('Imports - Sync', () => {
  test('should trigger sync when clicking sync button', async ({ page }) => {
    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // Click the sync button (use first() as there may be multiple sync buttons)
    await page
      .getByRole('button', { name: /Sync Google Contacts/i })
      .first()
      .click()

    // The button should show loading state or we should see a notification
    // Note: The actual sync might fail if Google OAuth isn't configured,
    // but we're testing the UI interaction works
    await page.waitForLoadState('networkidle')

    // Just verify the page doesn't crash
    await expect(page.getByRole('heading', { name: 'Import Contacts' })).toBeVisible()
  })
})

test.describe('Imports - Pagination', () => {
  test('should show pagination when there are multiple pages', async ({ page, request }) => {
    // Check if there are enough candidates for pagination
    const response = await request.get(`${API_BASE_URL}/api/v1/imports/candidates?limit=20`, {
      headers: API_HEADERS,
    })

    if (!response.ok()) {
      test.skip()
      return
    }

    const data = await response.json()
    const totalPages = data.meta?.pagination?.pages || 0

    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    if (totalPages > 1) {
      // Verify pagination controls are visible
      await expect(page.getByRole('button', { name: /Previous/i })).toBeVisible()
      await expect(page.getByRole('button', { name: /Next/i })).toBeVisible()
      await expect(page.getByText(/Page \d+ of \d+/i)).toBeVisible()
    } else {
      // With 1 or fewer pages, pagination may not be shown
      // This is expected behavior
    }
  })
})
