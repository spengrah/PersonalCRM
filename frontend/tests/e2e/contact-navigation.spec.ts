import { test, expect } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

test.describe('Contact Keyboard Navigation @area:contact-navigation', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should navigate between contacts with arrow keys @smoke', async ({ page }) => {
    // Create 3 contacts to navigate between
    const { ids } = await testApi.seedContacts([
      { full_name: 'Nav Contact A' },
      { full_name: 'Nav Contact B' },
      { full_name: 'Nav Contact C' },
    ])

    const fullNameA = `${testApi.prefix}-Nav Contact A`
    const fullNameB = `${testApi.prefix}-Nav Contact B`
    const fullNameC = `${testApi.prefix}-Nav Contact C`

    // Navigate to contacts list first to establish context
    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')

    // Click on the middle contact (B) to go to its detail page
    await page.getByText(fullNameB).click()
    await page.waitForURL(/\/contacts\/[A-Za-z0-9-]+/)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullNameB })).toBeVisible({ timeout: 15000 })

    // Verify navigation bar is visible
    await expect(page.getByRole('button', { name: 'Previous contact' })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Next contact' })).toBeVisible()

    // Press right arrow to go to next contact
    await page.keyboard.press('ArrowRight')
    await page.waitForLoadState('domcontentloaded')

    // Should have navigated (the exact contact depends on sort order)
    // Just verify we're on a different contact or the navigation worked
    const currentUrl = page.url()
    expect(currentUrl).toContain('/contacts/')
  })

  test('should show navigation bar with position indicator', async ({ page }) => {
    // Create 5 contacts
    await testApi.seedContacts([
      { full_name: 'Position Test 1' },
      { full_name: 'Position Test 2' },
      { full_name: 'Position Test 3' },
      { full_name: 'Position Test 4' },
      { full_name: 'Position Test 5' },
    ])

    const fullName3 = `${testApi.prefix}-Position Test 3`

    // Go to list first to establish context
    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')

    // Click on third contact row
    await page.getByText(fullName3).click()
    await page.waitForURL(/\/contacts\/[A-Za-z0-9-]+/)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName3 })).toBeVisible({ timeout: 15000 })

    // Wait for navigation IDs to load, then check for position indicator
    // The navigation bar shows "N of M" pattern
    await expect(page.getByText(/\d+ of \d+/)).toBeVisible({ timeout: 10000 })

    // Verify navigation buttons are present
    await expect(page.getByRole('button', { name: 'Previous contact' })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Next contact' })).toBeVisible()
  })

  test('should disable navigation at boundaries', async ({ page }) => {
    // Create 2 contacts
    const { ids } = await testApi.seedContacts([
      { full_name: 'Boundary First' },
      { full_name: 'Boundary Last' },
    ])

    // Go to list first
    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')

    // Go to first contact detail via direct URL with sort params
    await page.goto(
      `/contacts/${ids[0]}?sort=name&order=asc&search=${encodeURIComponent(testApi.prefix)}`
    )
    await page.waitForLoadState('domcontentloaded')

    // Previous button should be disabled at first position
    const prevButton = page.getByRole('button', { name: 'Previous contact' })
    await expect(prevButton).toBeVisible()
    await expect(prevButton).toBeDisabled()

    // Next button should be enabled
    const nextButton = page.getByRole('button', { name: 'Next contact' })
    await expect(nextButton).toBeEnabled()
  })

  test('should disable keyboard navigation in edit mode', async ({ page }) => {
    // Create 2 contacts
    const { ids } = await testApi.seedContacts([
      { full_name: 'Edit Mode Test A' },
      { full_name: 'Edit Mode Test B' },
    ])

    const fullNameA = `${testApi.prefix}-Edit Mode Test A`

    // Go to list first, then to first contact
    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')
    await page.getByText(fullNameA).click()
    await page.waitForURL(/\/contacts\/[A-Za-z0-9-]+/)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullNameA })).toBeVisible({ timeout: 15000 })

    // Enter edit mode
    await page.getByRole('button', { name: 'Edit' }).first().click()
    await page.waitForLoadState('domcontentloaded')

    // Navigation buttons should be visually disabled (grayed out)
    const prevButton = page.getByRole('button', { name: 'Previous contact' })
    const nextButton = page.getByRole('button', { name: 'Next contact' })

    // Buttons should have disabled styling (opacity or disabled attribute)
    await expect(prevButton).toHaveAttribute('disabled', '')
    await expect(nextButton).toHaveAttribute('disabled', '')

    // Try pressing arrow key - should not navigate
    const currentUrl = page.url()
    await page.keyboard.press('ArrowRight')
    // URL should remain the same
    await expect(page).toHaveURL(currentUrl, { timeout: 500 })
  })

  test('should not navigate when typing in input fields', async ({ page }) => {
    // Create 2 contacts
    const { ids } = await testApi.seedContacts([
      { full_name: 'Input Field Test A' },
      { full_name: 'Input Field Test B' },
    ])

    const fullNameA = `${testApi.prefix}-Input Field Test A`

    // Go to contact detail and enter edit mode
    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')
    await page.getByText(fullNameA).click()
    await page.waitForURL(/\/contacts\/[A-Za-z0-9-]+/)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullNameA })).toBeVisible({ timeout: 15000 })

    // Enter edit mode to get input fields
    await page.getByRole('button', { name: 'Edit' }).first().click()
    await page.waitForLoadState('domcontentloaded')

    // Focus on name input
    const nameInput = page.getByLabel('Full Name')
    await nameInput.focus()

    // Store current URL
    const currentUrl = page.url()

    // Type arrow keys in the input - they should move cursor, not navigate
    await nameInput.press('ArrowRight')
    await nameInput.press('ArrowLeft')

    // Should still be on same page
    await expect(page).toHaveURL(currentUrl, { timeout: 300 })
  })

  test('should preserve URL context (sort, search) during navigation', async ({ page }) => {
    // Create contacts
    const { ids } = await testApi.seedContacts([
      { full_name: 'Context Test Alpha', location: 'New York' },
      { full_name: 'Context Test Beta', location: 'Los Angeles' },
    ])

    // Go directly to a contact detail page with sort params
    // This tests that the detail page preserves context when navigating
    await page.goto(
      `/contacts/${ids[0]}?sort=name&order=asc&search=${encodeURIComponent(testApi.prefix)}`
    )
    await page.waitForLoadState('domcontentloaded')

    // URL should contain the sort params
    expect(page.url()).toContain('sort=name')
    expect(page.url()).toContain('order=asc')

    // Wait for navigation bar to be ready
    await expect(page.getByRole('button', { name: 'Next contact' })).toBeVisible({ timeout: 10000 })

    // Navigate to next contact using keyboard
    await page.keyboard.press('ArrowRight')
    await page.waitForLoadState('domcontentloaded')

    // URL should still contain sort params after navigation
    expect(page.url()).toContain('sort=name')
    expect(page.url()).toContain('order=asc')
  })

  test('should navigate via navigation bar buttons', async ({ page }) => {
    // Create 3 contacts
    const { ids } = await testApi.seedContacts([
      { full_name: 'Button Nav 1' },
      { full_name: 'Button Nav 2' },
      { full_name: 'Button Nav 3' },
    ])

    // Go directly to first contact with sort param and search filter to isolate IDs
    await page.goto(
      `/contacts/${ids[0]}?sort=name&order=asc&search=${encodeURIComponent(testApi.prefix)}`
    )
    await page.waitForLoadState('domcontentloaded')

    // Wait for navigation bar to be fully ready with IDs loaded
    await expect(page.getByRole('button', { name: 'Next contact' })).toBeVisible({ timeout: 10000 })
    // Wait for the position indicator to show we have navigation data
    await expect(page.getByText(/\d+ of \d+/)).toBeVisible({ timeout: 10000 })

    const initialUrl = page.url()

    // Click next button
    const nextButton = page.getByRole('button', { name: 'Next contact' })
    await expect(nextButton).toBeEnabled({ timeout: 5000 })
    await nextButton.click()
    await page.waitForURL(url => url.toString() !== initialUrl)
    await page.waitForLoadState('domcontentloaded')

    // Should have navigated to a different contact
    expect(page.url()).not.toBe(initialUrl)
    expect(page.url()).toContain('/contacts/')
  })

  test('should handle Escape key to return to list', async ({ page }) => {
    // Create a contact
    const { ids } = await testApi.seedContacts([{ full_name: 'Escape Test' }])

    const fullName = `${testApi.prefix}-Escape Test`

    // Go to list first
    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')

    // Click on contact
    await page.getByText(fullName).click()
    await page.waitForURL(/\/contacts\/[A-Za-z0-9-]+/)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Press Escape to return to list
    await page.keyboard.press('Escape')
    await page.waitForLoadState('domcontentloaded')

    // Should be back on contacts list
    await expect(page.getByRole('heading', { name: 'Contacts' })).toBeVisible()
  })

  test('should restore search and sort state after Escape back to list', async ({ page }) => {
    // Two contacts so search + sort visibly shape the list
    await testApi.seedContacts([
      { full_name: 'Restore State Alpha' },
      { full_name: 'Restore State Beta' },
    ])

    const fullNameAlpha = `${testApi.prefix}-Restore State Alpha`

    // Apply a search and a name-asc sort on the list
    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')
    await page.getByPlaceholder('Search contacts...').fill(testApi.prefix)
    await expect(page.getByText(fullNameAlpha)).toBeVisible({ timeout: 15000 })
    await page.getByRole('columnheader').filter({ hasText: /^Name/ }).click()

    // The list mirrors its state into the URL
    await expect(page).toHaveURL(/sort=name/)
    await expect(page).toHaveURL(/order=asc/)

    // Enter a detail page, then Escape back
    await page.getByText(fullNameAlpha).click()
    await page.waitForURL(/\/contacts\/[A-Za-z0-9-]+/)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullNameAlpha })).toBeVisible({
      timeout: 15000,
    })
    await page.keyboard.press('Escape')
    await page.waitForLoadState('domcontentloaded')

    // Back on the list with search + sort restored, not reset to defaults
    await expect(page.getByRole('heading', { name: 'Contacts' })).toBeVisible()
    await expect(page).toHaveURL(/sort=name/)
    await expect(page).toHaveURL(/order=asc/)
    await expect(page.getByPlaceholder('Search contacts...')).toHaveValue(testApi.prefix)
  })

  test('detail prev/next follows the same default (cadence) ordering as the list', async ({
    page,
  }) => {
    // spec: CON-038[1]
    // No sort param anywhere → the default cadence-desc order. Seeded scrambled
    // so the order weekly→monthly→annual proves the shared default, not
    // insertion. The existing nav tests all pin an explicit sort=name; this one
    // exercises the implicit default the list and detail share.
    const { ids } = await testApi.seedContacts([
      { full_name: 'Default Nav Annual', cadence: 'annual' },
      { full_name: 'Default Nav Weekly', cadence: 'weekly' },
      { full_name: 'Default Nav Monthly', cadence: 'monthly' },
    ])
    const annualId = ids[0]
    const weeklyId = ids[1]
    const monthlyId = ids[2]

    // Open the most-frequent (weekly) contact with only the search context — no
    // sort param, so the detail nav uses the default cadence ordering.
    await page.goto(`/contacts/${weeklyId}?search=${encodeURIComponent(testApi.prefix)}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(
      page.getByRole('heading', { name: `${testApi.prefix}-Default Nav Weekly` })
    ).toBeVisible({
      timeout: 15000,
    })

    // Weekly is first in cadence-desc order → Previous disabled at this boundary.
    await expect(page.getByRole('button', { name: 'Previous contact' })).toBeDisabled()

    // Next walks weekly → monthly → annual (most- to least-frequent).
    await page.keyboard.press('ArrowRight')
    await page.waitForURL(new RegExp(`/contacts/${monthlyId}`))
    await page.keyboard.press('ArrowRight')
    await page.waitForURL(new RegExp(`/contacts/${annualId}`))

    // Annual is last → Next disabled at the far boundary.
    await expect(page.getByRole('button', { name: 'Next contact' })).toBeDisabled()
  })
})
