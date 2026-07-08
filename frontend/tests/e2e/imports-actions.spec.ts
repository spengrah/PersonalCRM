import { test, expect } from './fixtures'
import { createTestAPI, TestAPI } from './helpers/test-api'
import { navigateModalToCandidate, findCandidateByName } from './helpers/imports-helpers'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
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
    let externalId: string

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)

      // Seed a candidate for import testing
      const { ids } = await testApi.seedExternalContacts([
        {
          display_name: 'Import Test Contact',
          emails: ['import-test@example.com'],
        },
      ])
      externalId = ids[0]
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    test('should import candidate and show success notification', async ({ page, request }) => {
      // spec: IMP-012
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

      // Capture the import POST before triggering it.
      const importResponsePromise = page.waitForResponse(
        res =>
          res.url() === `${API_BASE_URL}/api/v1/imports/${externalId}/import` &&
          res.request().method() === 'POST'
      )

      // Click the "Import as New Contact" button in the modal
      await page.getByRole('button', { name: 'Import as New Contact', exact: true }).click()

      const importResponse = await importResponsePromise
      expect(importResponse.status()).toBe(201)
      const importBody = await importResponse.json()
      const createdContactId: string = importBody?.data?.contact?.id
      expect(createdContactId, 'import response should carry the created contact id').toBeTruthy()
      expect(
        importBody?.data?.rematch_job_id,
        'import response should carry a rematch job id'
      ).toBeTruthy()

      // DOM precondition: the candidate card leaves the list (modal/list advanced).
      await expect(card).not.toBeVisible({ timeout: 15000 })

      // Persisted-state proof: the external row is marked imported and linked
      // to the new contact.
      const candidateRes = await request.get(`${API_BASE_URL}/api/v1/imports/${externalId}`, {
        headers: API_HEADERS,
      })
      expect(candidateRes.ok()).toBe(true)
      const candidateBody = await candidateRes.json()
      expect(candidateBody?.data?.match_status).toBe('imported')
      expect(candidateBody?.data?.crm_contact_id).toBe(createdContactId)

      // The new contact was created through the normal creation path, with
      // methods copied from the external record.
      const contactRes = await request.get(`${API_BASE_URL}/api/v1/contacts/${createdContactId}`, {
        headers: API_HEADERS,
      })
      expect(contactRes.ok()).toBe(true)
      const contactBody = await contactRes.json()
      expect(contactBody?.data?.full_name).toBe(displayName)
      const methods: Array<{ type: string; value: string }> = contactBody?.data?.methods ?? []
      expect(methods.some(m => m.type === 'email' && m.value === 'import-test@example.com')).toBe(
        true
      )
    })
  })

  test.describe('Ignore Action', () => {
    let testApi: TestAPI
    let externalId: string

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)

      // Seed a candidate for ignore testing
      const { ids } = await testApi.seedExternalContacts([
        {
          display_name: 'Ignore Test Contact',
          emails: ['ignore-test@example.com'],
        },
      ])
      externalId = ids[0]
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    test('should ignore candidate and show notification', async ({ page, request }) => {
      // spec: IMP-007
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

      const ignoreResponsePromise = page.waitForResponse(
        res =>
          res.url() === `${API_BASE_URL}/api/v1/imports/${externalId}/ignore` &&
          res.request().method() === 'POST'
      )
      await ignoreButton.click()
      const ignoreResponse = await ignoreResponsePromise
      expect(ignoreResponse.ok()).toBe(true)

      // DOM precondition: the ignore affordance leaves the list (this contact,
      // not another worker's, was ignored).
      await expect(ignoreButton).not.toBeVisible({ timeout: 15000 })

      // Persisted-state proof: the row is marked ignored.
      const candidateRes = await request.get(`${API_BASE_URL}/api/v1/imports/${externalId}`, {
        headers: API_HEADERS,
      })
      expect(candidateRes.ok()).toBe(true)
      const candidateBody = await candidateRes.json()
      expect(candidateBody?.data?.match_status).toBe('ignored')

      // Sticky proof: the row never resurfaces in the candidate queue.
      const candidatesRes = await request.get(
        `${API_BASE_URL}/api/v1/imports/candidates?limit=10000`,
        { headers: API_HEADERS }
      )
      expect(candidatesRes.ok()).toBe(true)
      const candidatesBody = await candidatesRes.json()
      const candidates: Array<{ id: string }> = candidatesBody?.data ?? []
      expect(candidates.some(c => c.id === externalId)).toBe(false)
    })
  })

  test.describe('Link Action', () => {
    let testApi: TestAPI
    let externalId: string
    let targetContactId: string

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)

      // Seed a candidate for link testing
      const { ids } = await testApi.seedExternalContacts([
        {
          display_name: 'Link Test Contact',
          emails: ['link-test@example.com'],
        },
      ])
      externalId = ids[0]

      // Seed a contact to link to
      const { ids: targetIds } = await testApi.seedOverdueContacts([
        {
          full_name: 'Link Target Contact',
          cadence: 'monthly',
          days_overdue: 1,
          email: 'link-target@example.com',
        },
      ])
      targetContactId = targetIds[0]
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    test('should link candidate to existing contact', async ({ page, request }) => {
      // spec: IMP-013
      await page.goto('/imports')
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

      // Capture the link POST before triggering it.
      const linkResponsePromise = page.waitForResponse(
        res =>
          res.url() === `${API_BASE_URL}/api/v1/imports/${externalId}/link` &&
          res.request().method() === 'POST'
      )

      // Click Link Contact button
      await page.getByRole('button', { name: /Link Contact/i }).click()

      const linkResponse = await linkResponsePromise
      expect(linkResponse.ok()).toBe(true)
      const linkBody = await linkResponse.json()
      expect(linkBody?.data?.external_contact?.match_status).toBe('imported')
      expect(linkBody?.data?.external_contact?.crm_contact_id).toBe(targetContactId)

      // DOM precondition: the candidate card leaves the list (this candidate,
      // not another worker's, was linked).
      await expect(candidateCard).not.toBeVisible({ timeout: 15000 })

      // Re-read confirms the same persisted state (curated link -> imported,
      // not the bare-link matched branch).
      const candidateRes = await request.get(`${API_BASE_URL}/api/v1/imports/${externalId}`, {
        headers: API_HEADERS,
      })
      expect(candidateRes.ok()).toBe(true)
      const candidateBody = await candidateRes.json()
      expect(candidateBody?.data?.match_status).toBe('imported')
      expect(candidateBody?.data?.crm_contact_id).toBe(targetContactId)
    })
  })
})
