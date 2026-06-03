import { test, expect } from './fixtures'

// API configuration for E2E tests
const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

test.describe('Imports Page @area:imports', () => {
  test.beforeEach(async ({ page }) => {
    // Navigate to imports page before each test
    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')
  })

  test('should display page header and sync button', async ({ page }) => {
    // Verify page header
    await expect(page.getByRole('heading', { name: 'Import Contacts' })).toBeVisible()

    // Verify sync button exists (use first() as there may be multiple sync buttons)
    await expect(page.getByRole('button', { name: /Sync Contacts/i }).first()).toBeVisible()
  })

  test('should show imports in navigation', async ({ page }) => {
    // Verify navigation has Imports entry
    await expect(page.getByRole('link', { name: /Imports/i })).toBeVisible()
  })

  test('should display empty state when no Google Contacts candidates', async ({
    page,
    request,
  }) => {
    // Verify Google Contacts source is empty to avoid cross-test interference
    const response = await request.get(
      `${API_BASE_URL}/api/v1/imports/candidates?source=gcontacts`,
      {
        headers: API_HEADERS,
      }
    )

    if (response.ok()) {
      const data = await response.json()
      if (data.data?.length === 0 || data.meta?.pagination?.total === 0) {
        // The People tab loads the unified suggestions endpoint (the
        // candidate list is composed into it), so wait on that.
        const suggestionsResponse = page.waitForResponse(
          res =>
            res.request().method() === 'GET' &&
            res.url().includes('/api/v1/imports/suggestions') &&
            res.url().includes('source=gcontacts')
        )

        await page.getByRole('button', { name: 'Google Contacts' }).click()
        await suggestionsResponse

        // Empty state should show specific messaging
        await expect(page.getByText(/No import candidates/i)).toBeVisible()
        await expect(page.getByText(/All contacts from Google have been imported/i)).toBeVisible()
      }
    }
  })

  test('should display source filter buttons', async ({ page }) => {
    // Verify filter UI is visible
    await expect(page.getByText('Filter:')).toBeVisible()
    await expect(page.getByRole('button', { name: 'All Sources', exact: true })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Google Contacts', exact: true })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Calendar', exact: true })).toBeVisible()

    // All Sources should be selected by default (has blue background)
    const allSourcesButton = page.getByRole('button', { name: 'All Sources', exact: true })
    await expect(allSourcesButton).toHaveClass(/bg-blue-600/)
  })

  test('should filter when clicking filter buttons', async ({ page }) => {
    // Click Google Contacts filter
    await page.getByRole('button', { name: 'Google Contacts', exact: true }).click()
    await page.waitForLoadState('domcontentloaded')

    // Google Contacts button should now be selected
    const googleContactsButton = page.getByRole('button', {
      name: 'Google Contacts',
      exact: true,
    })
    await expect(googleContactsButton).toHaveClass(/bg-blue-600/)

    // All Sources should no longer be selected
    const allSourcesButton = page.getByRole('button', { name: 'All Sources', exact: true })
    await expect(allSourcesButton).not.toHaveClass(/bg-blue-600/)

    // Click Calendar filter
    await page.getByRole('button', { name: 'Calendar', exact: true }).click()
    await page.waitForLoadState('domcontentloaded')

    // Calendar button should now be selected
    const calendarButton = page.getByRole('button', { name: 'Calendar', exact: true })
    await expect(calendarButton).toHaveClass(/bg-blue-600/)
  })

  test('should trigger sync when clicking sync button', async ({ page }) => {
    // Click the sync button (use first() as there may be multiple sync buttons)
    await page
      .getByRole('button', { name: /Sync Contacts/i })
      .first()
      .click()

    // The button should show loading state or we should see a notification
    // Note: The actual sync might fail if Google OAuth isn't configured,
    // but we're testing the UI interaction works
    await page.waitForLoadState('domcontentloaded')

    // Just verify the page doesn't crash
    await expect(page.getByRole('heading', { name: 'Import Contacts' })).toBeVisible()
  })
})
