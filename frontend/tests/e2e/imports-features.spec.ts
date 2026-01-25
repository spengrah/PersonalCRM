import { test, expect } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'
import { navigateModalToCandidate, findCandidateByName } from './helpers/imports-helpers'

test.describe('Imports Features @area:imports', () => {
  test.describe('Pagination', () => {
    let testApi: TestAPI

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)

      // Seed enough candidates to force pagination (page size is 20)
      const candidates = Array.from({ length: 21 }, (_, index) => ({
        display_name: `Pagination Candidate ${index + 1}`,
        emails: [`pagination-${index + 1}@example.com`],
      }))
      await testApi.seedExternalContacts(candidates)
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    test('should show pagination when there are multiple pages', async ({ page }) => {
      await page.goto('/imports')
      await page.waitForLoadState('domcontentloaded')

      // Verify pagination controls are visible
      await expect(page.getByRole('button', { name: 'Previous', exact: true })).toBeVisible()
      await expect(page.getByRole('button', { name: 'Next', exact: true })).toBeVisible()
      await expect(page.getByText(/Page \d+ of \d+/i)).toBeVisible()
    })
  })

  test.describe('Suggested Matches', () => {
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

      // Reload to ensure we get fresh data after seeding
      await page.reload()
      await page.waitForLoadState('networkidle')

      // Find our seeded candidate (may need to paginate)
      const displayName = `${testApi.prefix}-Unique Nomatch Person`
      await findCandidateByName(page, displayName)

      // The seeded candidate should show "Link (select)" since there's no matching CRM contact
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })
      await expect(candidateCard.getByRole('button', { name: 'Link (select)' })).toBeVisible()
    })

    test('should show suggested match with confidence percentage when present', async ({
      page,
    }) => {
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

      // Reload to ensure we get fresh data after seeding
      await page.reload()
      await page.waitForLoadState('networkidle')

      // Find our seeded candidate (may need to paginate)
      const displayName = `${testApi.prefix}-Matching Contact Person`
      await findCandidateByName(page, displayName)

      // The external contact should have a suggested match with the CRM contact
      // The button should show "Link to [Name] (XX%)"
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })

      // The Link button should show the matched contact name with confidence
      // Since name and email match exactly, confidence should be high (100%)
      const linkButton = candidateCard.getByRole('button', { name: /Link to/ })
      await expect(linkButton).toBeVisible()

      // Verify it shows the prefixed contact name and a percentage
      await expect(linkButton).toContainText(displayName)
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

      // Reload to ensure we get fresh data after seeding
      await page.reload()
      await page.waitForLoadState('networkidle')

      // Find our seeded candidate (may need to paginate)
      const displayName = `${testApi.prefix}-Preselect Test Contact`
      await findCandidateByName(page, displayName)

      // Find the candidate card and click the Link button
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })

      // Click the Link button (which should show "Link to [Name] (XX%)")
      await candidateCard.getByRole('button', { name: /Link to/ }).click()

      // Verify modal opens with mode toggle
      await expect(page.getByRole('button', { name: 'Link to Existing' })).toBeVisible()

      // Navigate to correct candidate if needed (handles race conditions in parallel tests)
      await navigateModalToCandidate(page, displayName)

      // The suggested contact should be pre-selected - verify by checking the Link Contact
      // button is enabled (it's disabled when no contact is selected)
      await expect(page.getByRole('button', { name: /Link Contact/i })).toBeEnabled()

      // Close modal
      await page.getByRole('button', { name: /Cancel/i }).click()
      await expect(page.getByRole('button', { name: 'Link to Existing' })).not.toBeVisible()
    })
  })

  test.describe('Confidence Sorting (Issue #122)', () => {
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
      await page.waitForLoadState('domcontentloaded')

      // Get all candidate cards in order
      const candidateCards = page.locator('[class*="border-gray-200"]').filter({
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
})
