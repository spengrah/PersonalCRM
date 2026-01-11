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
    await expect(page.getByRole('button', { name: /Sync Contacts/i }).first()).toBeVisible()
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

    // Verify modal opens with mode toggle and contact selector
    await expect(page.getByRole('button', { name: 'Link to Existing' })).toBeVisible()
    await expect(page.getByText('Search for a contact...')).toBeVisible()

    // Verify cancel button works
    await page.getByRole('button', { name: /Cancel/i }).click()
    await expect(page.getByRole('button', { name: 'Link to Existing' })).not.toBeVisible()
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

    // Find the candidate card and click its Link button
    const candidateCard = page.locator('[class*="rounded-lg"]').filter({ hasText: candidateName })
    await candidateCard.getByRole('button', { name: /Link/i }).click()

    // Wait for modal to open with mode toggle visible
    await expect(page.getByRole('button', { name: 'Link to Existing' })).toBeVisible()

    // The ContactSelector is a custom searchable dropdown
    // Click on the selector area (contains placeholder text) to open it
    const contactSelector = page.getByText('Search for a contact...')
    await contactSelector.click()

    // Type to search for the seeded contact
    const searchInput = page.locator('input[placeholder="Search for a contact..."]')
    await searchInput.fill(testApi.prefix)

    // Wait for the dropdown to show the contact and click it
    const contactOption = page.locator('[class*="cursor-pointer"]').filter({ hasText: targetName })
    await expect(contactOption).toBeVisible({ timeout: 5000 })
    await contactOption.click()

    // Click Link Contact button
    await page.getByRole('button', { name: /Link Contact/i }).click()

    // Wait for action to complete
    await page.waitForLoadState('networkidle')

    // Verify success notification
    await expect(page.getByText(/linked successfully/i)).toBeVisible({ timeout: 10000 })

    // Verify the candidate card is removed from the list
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
      .getByRole('button', { name: /Sync Contacts/i })
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

test.describe('Imports - Suggested Matches', () => {
  // These tests verify the suggested matches functionality from PR #93.
  // We seed deterministic data to ensure consistent test results.

  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should show "Link (select)" when no suggested match', async ({ page }) => {
    // Seed an external contact with a unique name that won't match any CRM contact
    await testApi.seedExternalContacts([
      {
        display_name: 'Unique Nomatch Person',
        emails: ['unique-nomatch@example.com'],
      },
    ])

    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // The seeded candidate should show "Link (select)" since there's no matching CRM contact
    const candidateCard = page
      .locator('[class*="rounded-lg"]')
      .filter({ hasText: `${testApi.prefix}-Unique Nomatch Person` })
    await expect(candidateCard.getByRole('button', { name: 'Link (select)' })).toBeVisible()
  })

  test('should show suggested match with confidence percentage when present', async ({ page }) => {
    // First seed a CRM contact
    await testApi.seedOverdueContacts([
      {
        full_name: 'Matching Contact Person',
        email: 'matching-contact@example.com',
        cadence: 'monthly',
        days_overdue: 1,
      },
    ])

    // Then seed an external contact with the SAME name and email
    // This will trigger the fuzzy matching algorithm to find a suggested match
    await testApi.seedExternalContacts([
      {
        display_name: 'Matching Contact Person',
        emails: ['matching-contact@example.com'],
      },
    ])

    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // The external contact should have a suggested match with the CRM contact
    // The button should show "Link to [Name] (XX%)"
    const candidateCard = page
      .locator('[class*="rounded-lg"]')
      .filter({ hasText: `${testApi.prefix}-Matching Contact Person` })

    // Wait for the card to be visible
    await expect(candidateCard).toBeVisible()

    // The Link button should show the matched contact name with confidence
    // Since name and email match exactly, confidence should be high (100%)
    const linkButton = candidateCard.getByRole('button', { name: /Link to/ })
    await expect(linkButton).toBeVisible()

    // Verify it shows the prefixed contact name and a percentage
    await expect(linkButton).toContainText(`${testApi.prefix}-Matching Contact Person`)
    await expect(linkButton).toContainText('%')
  })

  test('should pre-select suggested contact in link modal', async ({ page }) => {
    // Seed matching CRM contact and external contact
    await testApi.seedOverdueContacts([
      {
        full_name: 'Preselect Test Contact',
        email: 'preselect-test@example.com',
        cadence: 'monthly',
        days_overdue: 1,
      },
    ])

    await testApi.seedExternalContacts([
      {
        display_name: 'Preselect Test Contact',
        emails: ['preselect-test@example.com'],
      },
    ])

    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // Find the candidate card and click the Link button
    const candidateCard = page
      .locator('[class*="rounded-lg"]')
      .filter({ hasText: `${testApi.prefix}-Preselect Test Contact` })

    await expect(candidateCard).toBeVisible()

    // Click the Link button (which should show "Link to [Name] (XX%)")
    await candidateCard.getByRole('button', { name: /Link to/ }).click()

    // Verify modal opens with mode toggle
    await expect(page.getByRole('button', { name: 'Link to Existing' })).toBeVisible()

    // The suggested contact should be pre-selected - verify by checking the Link Contact
    // button is enabled (it's disabled when no contact is selected)
    await expect(page.getByRole('button', { name: /Link Contact/i })).toBeEnabled()

    // Close modal
    await page.getByRole('button', { name: /Cancel/i }).click()
    await expect(page.getByRole('button', { name: 'Link to Existing' })).not.toBeVisible()
  })
})

test.describe('Imports - Confidence Sorting (Issue #122)', () => {
  // This test verifies that import candidates are sorted by confidence score descending.
  // Candidates with higher match confidence should appear before those with lower confidence.
  // This was fixed in PR #128.

  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should sort candidates by confidence score descending', async ({ page }) => {
    // Seed CRM contacts with distinct names that will match with different confidence levels
    await testApi.seedOverdueContacts([
      {
        full_name: 'High Confidence Match',
        email: 'high-confidence@example.com',
        cadence: 'monthly',
        days_overdue: 1,
      },
      {
        full_name: 'Medium Confidence Match',
        email: 'medium-confidence@example.com',
        cadence: 'monthly',
        days_overdue: 1,
      },
    ])

    // Seed external contacts:
    // 1. High confidence: exact name + exact email match → ~100% confidence
    // 2. Medium confidence: exact name only, no email match → ~60% confidence
    // 3. Low/no match: unique name that won't match any CRM contact → no confidence score
    await testApi.seedExternalContacts([
      // This one will NOT have a match (seeded first, but should appear last after sorting)
      {
        display_name: 'Zzz No Match Person',
        emails: ['zzz-nomatch@example.com'],
      },
      // This one will have medium confidence (name match only, ~60%)
      {
        display_name: 'Medium Confidence Match',
        emails: ['different-email@example.com'],
      },
      // This one will have high confidence (name + email match, ~100%)
      {
        display_name: 'High Confidence Match',
        emails: ['high-confidence@example.com'],
      },
    ])

    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // Get all candidate cards in order
    const candidateCards = page.locator('[class*="rounded-lg"]').filter({
      has: page.getByRole('button', { name: /Import/i }),
    })

    // Wait for cards to load
    await expect(candidateCards.first()).toBeVisible()

    // Get the display names in order from the page
    const cardTexts = await candidateCards.allTextContents()

    // Find the indices of our test contacts
    const highConfidenceIdx = cardTexts.findIndex(text =>
      text.includes(`${testApi.prefix}-High Confidence Match`)
    )
    const mediumConfidenceIdx = cardTexts.findIndex(text =>
      text.includes(`${testApi.prefix}-Medium Confidence Match`)
    )
    const noMatchIdx = cardTexts.findIndex(text =>
      text.includes(`${testApi.prefix}-Zzz No Match Person`)
    )

    // Verify all three candidates are found
    expect(highConfidenceIdx).not.toBe(-1)
    expect(mediumConfidenceIdx).not.toBe(-1)
    expect(noMatchIdx).not.toBe(-1)

    // High confidence should appear before medium confidence
    expect(highConfidenceIdx).toBeLessThan(mediumConfidenceIdx)

    // Medium confidence should appear before no match (sorted by confidence, then alphabetically)
    expect(mediumConfidenceIdx).toBeLessThan(noMatchIdx)
  })
})

test.describe('Imports - Source Filter', () => {
  test('should display source filter buttons', async ({ page }) => {
    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

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
    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // Click Google Contacts filter
    await page.getByRole('button', { name: 'Google Contacts', exact: true }).click()
    await page.waitForLoadState('networkidle')

    // Google Contacts button should now be selected
    const googleContactsButton = page.getByRole('button', { name: 'Google Contacts', exact: true })
    await expect(googleContactsButton).toHaveClass(/bg-blue-600/)

    // All Sources should no longer be selected
    const allSourcesButton = page.getByRole('button', { name: 'All Sources', exact: true })
    await expect(allSourcesButton).not.toHaveClass(/bg-blue-600/)

    // Click Calendar filter
    await page.getByRole('button', { name: 'Calendar', exact: true }).click()
    await page.waitForLoadState('networkidle')

    // Calendar button should now be selected
    const calendarButton = page.getByRole('button', { name: 'Calendar', exact: true })
    await expect(calendarButton).toHaveClass(/bg-blue-600/)
  })
})
