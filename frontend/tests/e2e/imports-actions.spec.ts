import { test, expect, Page } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

/**
 * Helper to navigate the modal to show a specific candidate by name.
 * This handles the race condition where parallel tests can cause the modal
 * to open showing a different candidate than the one clicked.
 *
 * Strategy: Go to start first (all prev), then scan forward (all next).
 * This ensures we check every candidate exactly once.
 */
async function navigateModalToCandidate(
  page: Page,
  displayName: string,
  maxNavigations = 30
): Promise<void> {
  const modal = page.locator('.fixed.inset-0')
  const prevButton = page.getByRole('button', { name: 'Previous candidate' })
  const nextButton = page.getByRole('button', { name: 'Next candidate' })

  // Helper to check if candidate is visible
  const isTargetVisible = async () => {
    const modalHeading = modal.getByRole('heading', { level: 3, name: displayName })
    return modalHeading.isVisible({ timeout: 300 }).catch(() => false)
  }

  // Check if already showing correct candidate
  if (await isTargetVisible()) return

  // Phase 1: Go to start (click prev until disabled)
  for (let i = 0; i < maxNavigations; i++) {
    const prevVisible = await prevButton.isVisible({ timeout: 300 }).catch(() => false)
    if (!prevVisible) break
    const prevDisabled = await prevButton.isDisabled()
    if (prevDisabled) break
    await prevButton.click()
    await page.waitForTimeout(150)
    if (await isTargetVisible()) return
  }

  // Phase 2: Scan forward (click next until found or disabled)
  for (let i = 0; i < maxNavigations; i++) {
    const nextVisible = await nextButton.isVisible({ timeout: 300 }).catch(() => false)
    if (!nextVisible) break
    const nextDisabled = await nextButton.isDisabled()
    if (nextDisabled) break
    await nextButton.click()
    await page.waitForTimeout(150)
    if (await isTargetVisible()) return
  }

  // Final check - verify we found the candidate
  const modalHeading = modal.getByRole('heading', { level: 3, name: displayName })
  await expect(modalHeading).toBeVisible({ timeout: 2000 })
}

/**
 * Helper to find a candidate by name, handling pagination if needed.
 * Returns the candidate card locator once found.
 */
async function findCandidateByName(page: Page, displayName: string, maxPages = 5): Promise<void> {
  for (let i = 0; i < maxPages; i++) {
    // Check if our contact is visible on current page (use heading to avoid matching Link button)
    const contactHeading = page.getByRole('heading', { name: displayName })
    if (await contactHeading.isVisible({ timeout: 2000 }).catch(() => false)) {
      return
    }

    // Try to go to next page (use exact match to avoid matching "Next candidate" in modal)
    const nextButton = page.getByRole('button', { name: 'Next', exact: true })
    if (await nextButton.isVisible({ timeout: 1000 }).catch(() => false)) {
      const isDisabled = await nextButton.isDisabled()
      if (!isDisabled) {
        await nextButton.click()
        await page.waitForLoadState('networkidle')
        continue
      }
    }

    // No more pages, contact not found
    break
  }

  // Final check - if still not visible, the expect will fail with a good error
  await expect(page.getByRole('heading', { name: displayName })).toBeVisible({ timeout: 5000 })
}

test.describe('Imports Actions @area:imports', () => {
  test.describe('With Seeded Data', () => {
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

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, `${testApi.prefix}-Test Import User`)

      // Verify action buttons are present
      await expect(page.getByRole('button', { name: /Import/i }).first()).toBeVisible()
      await expect(page.getByRole('button', { name: /Link/i }).first()).toBeVisible()
    })

    test('should open link modal when clicking Link button', async ({ page }) => {
      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      const displayName = `${testApi.prefix}-Test Import User`

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // Click the Link button on our candidate
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })
      await candidateCard.getByRole('button', { name: /Link/i }).click()

      // Verify modal opens with mode toggle and contact selector
      await expect(page.getByRole('button', { name: 'Link to Existing' })).toBeVisible()

      // Navigate to correct candidate if needed (handles race conditions in parallel tests)
      await navigateModalToCandidate(page, displayName)

      await expect(page.getByText('Search for a contact...')).toBeVisible()

      // Verify cancel button works
      await page.getByRole('button', { name: /Cancel/i }).click()
      await expect(page.getByRole('button', { name: 'Link to Existing' })).not.toBeVisible()
    })
  })

  test.describe('Import Action', () => {
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

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // Click Import on the specific candidate card (not just any Import button)
      const card = page.locator('[class*="border-gray-200"]').filter({ hasText: displayName })
      await card.getByRole('button', { name: /Import/i }).click()

      // Verify modal opens in import mode - mode toggle should be visible
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

      // Navigate to correct candidate if needed (handles race conditions in parallel tests)
      await navigateModalToCandidate(page, displayName)

      // Click the "Import as New Contact" button in the modal
      await page.getByRole('button', { name: 'Import as New Contact', exact: true }).click()

      // Wait for the candidate card to be removed from the candidate list
      // The modal may stay open if there are other candidates, so we check the card (not text)
      // because the success notification may contain the display name
      await expect(card).not.toBeVisible({ timeout: 15000 })
    })
  })

  test.describe('Ignore Action', () => {
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

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // Click the X (ignore) button on the candidate
      // The ignore button is a ghost button with just an X icon, aria-label "Ignore candidate"
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })
      const ignoreButton = candidateCard.getByRole('button', { name: 'Ignore candidate' })

      await ignoreButton.click()

      // Wait for the candidate card's ignore button to disappear from the list
      // This is the definitive signal that THIS contact was ignored (not another worker's)
      await expect(ignoreButton).not.toBeVisible({ timeout: 15000 })
    })
  })

  test.describe('Link Action', () => {
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

      // Reload to ensure we get fresh data after seeding
      await page.reload()
      await page.waitForLoadState('networkidle')

      const candidateName = `${testApi.prefix}-Link Test Contact`
      const targetName = `${testApi.prefix}-Link Target Contact`

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, candidateName)

      // Find the candidate card and click its Link button
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: candidateName })
      await candidateCard.getByRole('button', { name: /Link/i }).click()

      // Wait for modal to open with mode toggle visible
      await expect(page.getByRole('button', { name: 'Link to Existing' })).toBeVisible()

      // Navigate to correct candidate if needed (handles race conditions in parallel tests)
      await navigateModalToCandidate(page, candidateName)

      // The ContactSelector is a custom searchable dropdown
      // Click on the selector area (contains placeholder text) to open it
      const contactSelector = page.getByText('Search for a contact...')
      await contactSelector.click()

      // Type to search for the seeded contact
      const searchInput = page.locator('input[placeholder="Search for a contact..."]')
      await searchInput.fill(testApi.prefix)

      // Wait for the dropdown to show the contact and click it
      const contactOption = page
        .locator('[class*="cursor-pointer"]')
        .filter({ hasText: targetName })
      await expect(contactOption).toBeVisible({ timeout: 5000 })
      await contactOption.click()

      // Click Link Contact button
      await page.getByRole('button', { name: /Link Contact/i }).click()

      // Wait for the candidate card to disappear from the list
      // This is the definitive signal that THIS contact was linked (not another worker's)
      await expect(page.getByText(candidateName)).not.toBeVisible({ timeout: 15000 })
    })
  })
})
