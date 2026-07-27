import { test, expect } from './fixtures'
import { createTestAPI, TestAPI } from './helpers/test-api'
import {
  expectModalCandidate,
  findCandidateByName,
  candidateCardByName,
} from './helpers/imports-helpers'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

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

    // spec: IMP-026.people-tab-default-holds
    test('paginates the candidate list', async ({ page }) => {
      await page.goto('/imports')
      await page.waitForLoadState('domcontentloaded')

      // Pagination controls render once the queue exceeds a page.
      const nextButton = page.getByRole('button', { name: 'Next', exact: true })
      await expect(page.getByRole('button', { name: 'Previous', exact: true })).toBeVisible()
      await expect(nextButton).toBeVisible()

      // Clicking Next refetches the suggestions feed with the page-2 param
      // and the second page is non-empty (21 seeded > one page of 20).
      const page2Response = page.waitForResponse(
        res =>
          res.request().method() === 'GET' &&
          res.url().includes('/api/v1/imports/suggestions') &&
          res.url().includes('page=2')
      )
      await nextButton.click()
      const res = await page2Response
      expect(res.ok()).toBe(true)
      const body = await res.json()
      expect((body?.data ?? []).length).toBeGreaterThan(0)

      // Route/UI state reflects being past the first page.
      await expect(page.getByRole('button', { name: 'Previous', exact: true })).toBeEnabled()
    })
  })

  test.describe('Suggested Matches', () => {
    // These tests verify the suggested-match affordance with deterministic
    // seeded data.

    let testApi: TestAPI

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    // spec: IMP-029.without-suggestion-link-action
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

      // Find our seeded candidate (may need to paginate)
      const displayName = `${testApi.prefix}-Unique Nomatch Person`
      await findCandidateByName(page, displayName)

      // The seeded candidate should show "Link (select)" since there's no matching CRM contact
      const candidateCard = candidateCardByName(page, displayName)
      await expect(candidateCard.getByRole('button', { name: 'Link (select)' })).toBeVisible()
    })

    // spec: IMP-029.link-action-names-suggested
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

      // Find our seeded candidate (may need to paginate)
      const displayName = `${testApi.prefix}-Matching Contact Person`
      await findCandidateByName(page, displayName)

      // The link action names the suggested contact with its confidence
      // percentage: "Link to [Name] (XX%)".
      const candidateCard = candidateCardByName(page, displayName)
      const linkButton = candidateCard.getByRole('button', { name: /Link to/ })
      await expect(linkButton).toBeVisible()
      await expect(linkButton).toContainText(displayName)
      await expect(linkButton).toContainText('%')
    })

    // spec: IMP-028.suggested-match-when-present
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

      // Find our seeded candidate (may need to paginate)
      const displayName = `${testApi.prefix}-Preselect Test Contact`
      await findCandidateByName(page, displayName)

      // Click the Link button (which should show "Link to [Name] (XX%)")
      await candidateCardByName(page, displayName)
        .getByRole('button', { name: /Link to/ })
        .click()

      // Verify modal opens with mode toggle
      await expect(page.getByRole('button', { name: 'Link to Existing' })).toBeVisible()

      await expectModalCandidate(page, displayName)

      // The suggested contact is pre-selected: the ContactSelector shows the
      // selection (no search placeholder) and the Link action is enabled
      // (it is disabled when no contact is selected).
      const dialog = page.getByRole('dialog', { name: 'Resolve import candidate' })
      await expect(dialog.getByText('Search for a contact...')).toHaveCount(0)
      await expect(page.getByRole('button', { name: /Link Contact/i })).toBeEnabled()
    })
  })

  test.describe('Telegram @username display', () => {
    let testApi: TestAPI

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    // spec: IMP-036.username-appears-chip-deep
    test('shows @username chip on Telegram candidate card', async ({ page }) => {
      // Use a prefix-scoped handle so parallel test runs don't collide on the
      // link selector and so we can scope assertions to our card.
      const handle = `@dale_${testApi.prefix.replace(/-/g, '_')}`
      const telegramPath = handle.replace(/^@/, '')

      await testApi.seedExternalContacts([
        {
          source: 'telegram',
          display_name: 'Dale Dobeck',
          metadata: { username: handle },
        },
      ])

      await page.goto('/imports')
      await page.waitForLoadState('domcontentloaded')

      // Filter to Telegram so only our seeded card is visible even on a busy DB.
      await page.getByRole('button', { name: 'Telegram', exact: true }).click()

      const displayName = `${testApi.prefix}-Dale Dobeck`
      await findCandidateByName(page, displayName)

      // Heading shows the display_name; chip shows the handle and links to t.me
      await expect(page.getByRole('heading', { name: displayName })).toBeVisible()
      const handleLink = page.getByRole('link', { name: handle })
      await expect(handleLink).toBeVisible()
      await expect(handleLink).toHaveAttribute('href', `https://t.me/${telegramPath}`)
    })

    // spec: IMP-036.no-name-fields-username-used
    test('falls back to @username when no name is set on Telegram candidate', async ({ page }) => {
      const handle = `@dale_${testApi.prefix.replace(/-/g, '_')}`

      await testApi.seedExternalContacts([
        {
          source: 'telegram',
          // No display_name, no first/last
          metadata: { username: handle },
        },
      ])

      await page.goto('/imports')
      await page.waitForLoadState('domcontentloaded')

      await page.getByRole('button', { name: 'Telegram', exact: true }).click()

      // @username becomes the primary heading
      await expect(page.getByRole('heading', { name: handle })).toBeVisible()
      // Chip is suppressed when the handle is already the heading. Scoping
      // by the unique prefix-based handle avoids clashing with other seeded
      // rows on a shared DB.
      const handleLinks = page.getByRole('link', { name: handle })
      await expect(handleLinks).toHaveCount(0)
    })

    // A candidate with no source name fields must still be importable without
    // editing the name — the frontend sends the @handle as `name` explicitly,
    // because the backend can't derive it from display_name/first_name/last_name.
    // spec: IMP-012.crm-contact-created-normal-path, IMP-012.methods-come-from-selection
    test('imports a handle-only Telegram candidate without requiring a name edit', async ({
      page,
      request,
    }) => {
      const handle = `@dale_${testApi.prefix.replace(/-/g, '_')}`

      await testApi.seedExternalContacts([
        {
          source: 'telegram',
          metadata: { username: handle },
        },
      ])

      await page.goto('/imports')
      await page.waitForLoadState('domcontentloaded')
      await page.getByRole('button', { name: 'Telegram', exact: true }).click()

      // The candidate card heading shows the handle (display-name fallback).
      await expect(page.getByRole('heading', { name: handle })).toBeVisible()

      // Open the Import action for this candidate.
      await candidateCardByName(page, handle)
        .getByRole('button', { name: /Import/i })
        .click()

      // Modal opens in import mode (mode toggle is visible).
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

      await expectModalCandidate(page, handle)

      // Contact Methods section shows the @handle as a selectable method row —
      // NOT "No contact methods available". The row carries the handle as its
      // visible value and defaults to selected.
      const dialog = page.getByRole('dialog', { name: 'Resolve import candidate' })
      await expect(dialog.getByText('No contact methods available')).not.toBeVisible()
      const methodRow = dialog.locator('div.border', { hasText: handle }).last()
      await expect(methodRow.getByRole('button', { name: 'Deselect method' })).toHaveAttribute(
        'aria-pressed',
        'true'
      )

      // Import without editing the name; the created contact carries the
      // handle as its name and the telegram method from the external record.
      const importResponsePromise = page.waitForResponse(
        res => res.request().method() === 'POST' && /\/imports\/.+\/import$/.test(res.url())
      )
      await page.getByRole('button', { name: 'Import as New Contact', exact: true }).click()

      const importResponse = await importResponsePromise
      expect(importResponse.status()).toBe(201)
      const importBody = await importResponse.json()
      const createdContactId: string = importBody?.data?.contact?.id
      expect(createdContactId).toBeTruthy()

      const contactRes = await request.get(`${API_BASE_URL}/api/v1/contacts/${createdContactId}`, {
        headers: API_HEADERS,
      })
      expect(contactRes.ok()).toBe(true)
      const contactBody = await contactRes.json()
      expect(contactBody?.data?.full_name).toBe(handle)
      const methods: Array<{ type: string; value: string }> = contactBody?.data?.methods ?? []
      expect(methods.some(m => m.type === 'telegram')).toBe(true)
    })

    // spec: IMP-027.methods-selectable-one-primary
    test('shows @username method in Link to Existing modal', async ({ page }) => {
      const handle = `@dale_${testApi.prefix.replace(/-/g, '_')}`

      // Seed a CRM contact to link to
      await testApi.seedContacts([
        {
          full_name: 'Link Target For Telegram',
        },
      ])

      // Seed a Telegram candidate with no name fields
      await testApi.seedExternalContacts([
        {
          source: 'telegram',
          metadata: { username: handle },
        },
      ])

      await page.goto('/imports')
      await page.waitForLoadState('domcontentloaded')
      await page.getByRole('button', { name: 'Telegram', exact: true }).click()

      await candidateCardByName(page, handle).getByRole('button', { name: /Link/i }).click()

      await expectModalCandidate(page, handle)

      // Switch to Link mode
      await page.getByRole('button', { name: 'Link to Existing', exact: true }).click()

      // Select the CRM contact
      const dialog = page.getByRole('dialog', { name: 'Resolve import candidate' })
      await dialog.getByText('Search for a contact...').click()
      await page.getByText(`${testApi.prefix}-Link Target For Telegram`).click()

      // Link mode groups the handle under the to-add bucket ("Will be
      // added") rather than "No contact methods available".
      await expect(dialog.getByText('No contact methods available')).not.toBeVisible()
      await expect(dialog.getByText('Will be added')).toBeVisible()
      const methodRow = dialog.locator('div.border', { hasText: handle }).last()
      await expect(
        methodRow.getByRole('button', { name: /Select method|Deselect method/ })
      ).toBeVisible()
    })
  })
})
