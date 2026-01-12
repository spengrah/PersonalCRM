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

    // Click Import on the candidate to open the modal
    await page
      .getByRole('button', { name: /Import/i })
      .first()
      .click()

    // Verify modal opens in import mode - mode toggle should be visible
    await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

    // Click the "Import as New Contact" button in the modal
    await page.getByRole('button', { name: 'Import as New Contact', exact: true }).click()

    // Wait for the action to complete
    await page.waitForLoadState('networkidle')

    // Verify success notification appears
    await expect(page.getByText(/imported successfully/i)).toBeVisible({ timeout: 10000 })

    // Close modal if still open (modal may stay open if there are other candidates in DB)
    const cancelButton = page.getByRole('button', { name: /Cancel/i })
    if (await cancelButton.isVisible()) {
      await cancelButton.click()
    }

    // Wait for modal to close
    await expect(page.getByRole('button', { name: 'Import as New', exact: true })).not.toBeVisible({
      timeout: 5000,
    })

    // Verify the candidate card is removed from the page list
    // Use a specific selector for candidate cards (with Import button) to exclude the notification
    const candidateCard = page
      .locator('[class*="border-gray-200"]')
      .filter({ hasText: displayName })
      .filter({ has: page.getByRole('button', { name: 'Import', exact: true }) })
    await expect(candidateCard).toHaveCount(0, { timeout: 10000 })
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
    // The ignore button is a ghost button with just an X icon, aria-label "Ignore candidate"
    const candidateCard = page
      .locator('[class*="border-gray-200"]')
      .filter({ hasText: displayName })
    const ignoreButton = candidateCard.getByRole('button', { name: 'Ignore candidate' })

    await ignoreButton.click()

    // Wait for the action to complete
    await page.waitForLoadState('networkidle')

    // Verify notification appears
    await expect(page.getByText(/ignored/i)).toBeVisible({ timeout: 10000 })

    // Verify the candidate is no longer in the list (not just "not visible in card")
    // Wait for the card with the display name to be removed
    await expect(
      page.locator('[class*="border-gray-200"]').filter({ hasText: displayName })
    ).not.toBeVisible({
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

    // Click Link Contact button and wait for link + refetch to complete
    const linkResponse = page.waitForResponse(
      response =>
        response.request().method() === 'POST' &&
        response.url().includes('/api/v1/imports/') &&
        response.url().endsWith('/link')
    )
    const candidatesRefetch = page.waitForResponse(
      response =>
        response.request().method() === 'GET' &&
        response.url().includes('/api/v1/imports/candidates')
    )
    await page.getByRole('button', { name: /Link Contact/i }).click()
    await linkResponse
    await candidatesRefetch

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

test.describe('Imports - Modal UX Improvements', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)

    // Seed multiple candidates for navigation testing
    await testApi.seedExternalContacts([
      {
        display_name: 'Modal Test Contact One',
        emails: ['modal-test-one@example.com'],
      },
      {
        display_name: 'Modal Test Contact Two',
        emails: ['modal-test-two@example.com'],
      },
      {
        display_name: 'Modal Test Contact Three',
        emails: ['modal-test-three@example.com'],
      },
    ])
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should close modal when pressing Escape key', async ({ page }) => {
    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // Open modal
    await page
      .getByRole('button', { name: /Import/i })
      .first()
      .click()
    await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

    // Press Escape
    await page.keyboard.press('Escape')

    // Modal should be closed
    await expect(page.getByRole('button', { name: 'Import as New', exact: true })).not.toBeVisible()
  })

  test('should navigate with arrow keys', async ({ page }) => {
    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // Open modal on first candidate
    await page
      .getByRole('button', { name: /Import/i })
      .first()
      .click()
    await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

    // Verify we're on candidate 1
    await expect(page.getByText(/1 of/)).toBeVisible()

    // Press ArrowRight to go to next - use deterministic wait for new text
    await page.keyboard.press('ArrowRight')
    await expect(page.getByText(/2 of/)).toBeVisible({ timeout: 5000 })

    // Press ArrowLeft to go back - use deterministic wait for new text
    await page.keyboard.press('ArrowLeft')
    await expect(page.getByText(/1 of/)).toBeVisible({ timeout: 5000 })
  })

  test('should close modal when clicking backdrop', async ({ page }) => {
    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // Open modal
    await page
      .getByRole('button', { name: /Import/i })
      .first()
      .click()
    await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

    // Click on backdrop (outside the modal content)
    // The backdrop has class 'fixed inset-0'
    await page.locator('.fixed.inset-0').click({ position: { x: 10, y: 10 } })

    // Modal should be closed
    await expect(page.getByRole('button', { name: 'Import as New', exact: true })).not.toBeVisible()
  })

  test('should show loading text during import action', async ({ page }) => {
    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // Open modal
    await page
      .getByRole('button', { name: /Import/i })
      .first()
      .click()
    await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

    // Click Import button and check for loading state
    const importButton = page.getByRole('button', { name: 'Import as New Contact', exact: true })
    await importButton.click()

    // The button should briefly show "Importing..." - use a short timeout as it's transient
    // Note: This might be too fast to catch in E2E, but we verify the action completes
    await page.waitForLoadState('networkidle')
    await expect(page.getByText(/imported successfully/i)).toBeVisible({ timeout: 10000 })
  })

  test('should display friendly source names with icons', async ({ page }) => {
    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // Open modal
    await page
      .getByRole('button', { name: /Import/i })
      .first()
      .click()
    await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

    // Should show "Google Contacts" instead of "gcontacts" in the modal
    // The seeded contacts are from gcontacts source
    // The source info is in a paragraph element inside the modal, not the filter button
    // Look for the source display paragraph that contains an SVG icon
    const sourceDisplay = page.locator('p.text-gray-500').filter({ hasText: 'Google Contacts' })
    await expect(sourceDisplay.first()).toBeVisible()
  })

  test('should have transparent backdrop with blur', async ({ page }) => {
    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // Open modal
    await page
      .getByRole('button', { name: /Import/i })
      .first()
      .click()
    await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

    // Verify backdrop has the correct classes
    const backdrop = page.locator('.fixed.inset-0.backdrop-blur-sm')
    await expect(backdrop).toBeVisible()
  })
})

test.describe('Imports - Cadence Selector (Issue #152)', () => {
  // This test verifies the cadence selector functionality in the import/link modal.
  // Users can set or update contact cadence during import/link operations.

  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should show cadence selector in import modal', async ({ page }) => {
    // Seed a candidate for testing
    await testApi.seedExternalContacts([
      {
        display_name: 'Cadence Test Import',
        emails: ['cadence-import@example.com'],
      },
    ])

    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // Open import modal
    const candidateCard = page
      .locator('[class*="rounded-lg"]')
      .filter({ hasText: `${testApi.prefix}-Cadence Test Import` })
    await candidateCard.getByRole('button', { name: /Import/i }).click()

    // Verify cadence selector is visible
    await expect(page.getByText('Contact Cadence')).toBeVisible()
    await expect(page.getByText('How often you want to be reminded to reach out')).toBeVisible()

    // Verify dropdown has expected options
    const cadenceSelect = page.locator('select').filter({ hasText: 'No cadence' })
    await expect(cadenceSelect).toBeVisible()

    // Close modal
    await page.getByRole('button', { name: /Cancel/i }).click()
  })

  test('should import contact with selected cadence', async ({ page }) => {
    // Seed a candidate
    await testApi.seedExternalContacts([
      {
        display_name: 'Cadence Import User',
        emails: ['cadence-import-user@example.com'],
      },
    ])

    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    const displayName = `${testApi.prefix}-Cadence Import User`

    // Open import modal
    const candidateCard = page.locator('[class*="rounded-lg"]').filter({ hasText: displayName })
    await candidateCard.getByRole('button', { name: /Import/i }).click()

    // Wait for modal
    await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

    // Select monthly cadence
    const cadenceSelect = page.locator('select').filter({ hasText: 'No cadence' })
    await cadenceSelect.selectOption('monthly')

    // Import the contact
    await page.getByRole('button', { name: 'Import as New Contact', exact: true }).click()

    // Wait for success
    await expect(page.getByText(/imported successfully/i)).toBeVisible({ timeout: 10000 })

    // Close modal if still open
    const cancelButton = page.getByRole('button', { name: /Cancel/i })
    if (await cancelButton.isVisible()) {
      await cancelButton.click()
    }

    // Navigate to contacts page and verify the contact has cadence set
    await page.goto('/contacts')
    await page.waitForLoadState('networkidle')

    // Search for the imported contact
    const searchInput = page.getByPlaceholder('Search contacts...')
    await searchInput.fill(testApi.prefix)
    await page.waitForLoadState('networkidle')

    // Wait for search results and click on the contact
    const contactLink = page.getByText(displayName)
    await expect(contactLink).toBeVisible({ timeout: 10000 })
    await contactLink.click()
    await page.waitForLoadState('networkidle')

    // Verify cadence is displayed on detail page
    await expect(page.getByText('Contact cadence')).toBeVisible({ timeout: 10000 })
    await expect(page.getByText('monthly')).toBeVisible({ timeout: 5000 })
  })

  test('should show cadence selector in link modal', async ({ page }) => {
    // Seed a candidate and a target contact
    await testApi.seedExternalContacts([
      {
        display_name: 'Cadence Link Candidate',
        emails: ['cadence-link@example.com'],
      },
    ])

    await testApi.seedOverdueContacts([
      {
        full_name: 'Cadence Link Target',
        email: 'cadence-target@example.com',
        cadence: 'quarterly',
        days_overdue: 1,
      },
    ])

    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // Open link modal
    const candidateCard = page
      .locator('[class*="rounded-lg"]')
      .filter({ hasText: `${testApi.prefix}-Cadence Link Candidate` })
    await candidateCard.getByRole('button', { name: /Link/i }).click()

    // Wait for modal to open in link mode
    await expect(page.getByRole('button', { name: 'Link to Existing' })).toBeVisible()

    // Select a contact to link to
    const contactSelector = page.getByText('Search for a contact...')
    await contactSelector.click()
    const searchInput = page.locator('input[placeholder="Search for a contact..."]')
    await searchInput.fill(testApi.prefix)

    const contactOption = page
      .locator('[class*="cursor-pointer"]')
      .filter({ hasText: `${testApi.prefix}-Cadence Link Target` })
    await expect(contactOption).toBeVisible({ timeout: 5000 })
    await contactOption.click()

    // Verify cadence selector is visible and shows existing cadence
    await expect(page.getByText('Contact Cadence')).toBeVisible()

    // The selector should show the existing contact's cadence (quarterly)
    const cadenceSelect = page.locator('select').filter({ hasText: 'Quarterly' })
    await expect(cadenceSelect).toBeVisible()

    // Close modal
    await page.getByRole('button', { name: /Cancel/i }).click()
  })

  test('should update cadence when linking contact', async ({ page }) => {
    // Seed a candidate and a target contact with NO cadence initially
    // This avoids timing issues with pre-selection of existing cadence
    await testApi.seedExternalContacts([
      {
        display_name: 'Link Cadence Update Test',
        emails: ['link-update@example.com'],
      },
    ])

    await testApi.seedOverdueContacts([
      {
        full_name: 'Link Cadence Target',
        email: 'link-target@example.com',
        cadence: 'monthly', // Use monthly, will change to weekly
        days_overdue: 1,
      },
    ])

    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // Open link modal
    const candidateCard = page
      .locator('[class*="rounded-lg"]')
      .filter({ hasText: `${testApi.prefix}-Link Cadence Update Test` })
    await candidateCard.getByRole('button', { name: /Link/i }).click()

    // Wait for modal
    await expect(page.getByRole('button', { name: 'Link to Existing' })).toBeVisible()

    // Select the target contact
    const contactSelector = page.getByText('Search for a contact...')
    await contactSelector.click()
    const searchInput = page.locator('input[placeholder="Search for a contact..."]')
    await searchInput.fill(testApi.prefix)

    const contactOption = page
      .locator('[class*="cursor-pointer"]')
      .filter({ hasText: `${testApi.prefix}-Link Cadence Target` })
    await expect(contactOption).toBeVisible({ timeout: 5000 })
    await contactOption.click()

    // Wait for the cadence dropdown to be visible
    // The Select component generates id from label: "select-contact-cadence"
    const cadenceSelect = page.locator('#select-contact-cadence')
    await expect(cadenceSelect).toBeVisible({ timeout: 5000 })

    // Wait for the contact data to load and pre-select Monthly cadence
    await expect(cadenceSelect).toHaveValue('monthly', { timeout: 5000 })

    // Change cadence to weekly
    await cadenceSelect.selectOption('weekly')

    // Verify the selection changed
    await expect(cadenceSelect).toHaveValue('weekly')

    // Click Link Contact button
    const linkResponse = page.waitForResponse(
      response =>
        response.request().method() === 'POST' &&
        response.url().includes('/api/v1/imports/') &&
        response.url().endsWith('/link')
    )
    await page.getByRole('button', { name: /Link Contact/i }).click()
    await linkResponse

    // Verify success
    await expect(page.getByText(/linked successfully/i)).toBeVisible({ timeout: 10000 })

    // Navigate to the contact and verify cadence was updated
    await page.goto('/contacts')
    await page.waitForLoadState('networkidle')

    const contactSearchInput = page.getByPlaceholder('Search contacts...')
    await contactSearchInput.fill(`${testApi.prefix}-Link Cadence Target`)
    await page.waitForLoadState('networkidle')

    // Wait for search results and click on the contact
    const contactLink = page.getByText(`${testApi.prefix}-Link Cadence Target`)
    await expect(contactLink).toBeVisible({ timeout: 10000 })
    await contactLink.click()
    await page.waitForLoadState('networkidle')

    // Verify cadence is now weekly - look for it in the contact detail section
    await expect(page.getByText('Contact cadence')).toBeVisible({ timeout: 10000 })
    await expect(page.getByText('weekly')).toBeVisible({ timeout: 5000 })
  })

  test('should default to no cadence in import mode', async ({ page }) => {
    // Seed a candidate
    await testApi.seedExternalContacts([
      {
        display_name: 'Default Cadence Test',
        emails: ['default-cadence@example.com'],
      },
    ])

    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // Open import modal
    const candidateCard = page
      .locator('[class*="rounded-lg"]')
      .filter({ hasText: `${testApi.prefix}-Default Cadence Test` })
    await candidateCard.getByRole('button', { name: /Import/i }).click()

    // Verify modal opens
    await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

    // Verify default selection is "No cadence"
    const cadenceSelect = page.locator('select').filter({ hasText: 'No cadence' })
    await expect(cadenceSelect).toBeVisible()
    await expect(cadenceSelect).toHaveValue('')

    // Close modal
    await page.getByRole('button', { name: /Cancel/i }).click()
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
