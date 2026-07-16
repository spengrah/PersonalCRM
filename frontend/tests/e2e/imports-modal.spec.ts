import { test, expect } from './fixtures'
import type { Page } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'
import { navigateModalToCandidate, findCandidateByName } from './helpers/imports-helpers'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

// Candidate card scoped by its heading (never by presentation classes).
const candidateCardByName = (page: Page, displayName: string) =>
  page.locator('div.border', { has: page.getByRole('heading', { name: displayName }) }).first()

// The candidate-resolution modal body.
const resolverDialog = (page: Page) =>
  page.getByRole('dialog', { name: 'Resolve import candidate' })

test.describe('Imports Modal @area:imports', () => {
  test.describe('Queue Navigation', () => {
    let testApi: TestAPI

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)

      // Seed multiple candidates for navigation testing.
      await testApi.seedExternalContacts([
        {
          display_name: 'Modal Test Contact A',
          source: 'gcontacts',
          emails: ['modal-test-a@example.com'],
        },
        {
          display_name: 'Modal Test Contact B',
          source: 'gcontacts',
          emails: ['modal-test-b@example.com'],
        },
        {
          display_name: 'Modal Test Contact C',
          source: 'gcontacts',
          emails: ['modal-test-c@example.com'],
        },
      ])
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    // spec: IMP-028[0]
    test('should navigate with arrow keys and the position pager', async ({ page }) => {
      const displayName = `${testApi.prefix}-Modal Test Contact A`

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // The modal pages the whole queue independent of list pagination:
      // opening it refetches candidates bounded at 1000.
      const modalFetch = page.waitForResponse(
        res =>
          res.request().method() === 'GET' &&
          res.url().includes('/api/v1/imports/candidates') &&
          res.url().includes('limit=1000')
      )

      // Open modal on our own contact
      await candidateCardByName(page, displayName)
        .getByRole('button', { name: /Import/i })
        .click()
      await modalFetch

      // Wait for modal and navigate to correct candidate
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()
      await navigateModalToCandidate(page, displayName)

      const modal = resolverDialog(page)
      const heading = modal.getByRole('heading', { level: 3 }).first()
      const initialName = await heading.textContent()
      expect(initialName).toContain(displayName)

      // Blur any focused element to ensure keyboard events go to the window
      await page.evaluate(() => {
        if (document.activeElement instanceof HTMLElement) {
          document.activeElement.blur()
        }
      })

      // Press ArrowRight to go to next candidate
      await page.evaluate(() => {
        window.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }))
      })

      // Wait for heading to change (could be any candidate, just verify navigation works)
      await expect(heading).not.toHaveText(initialName!, { timeout: 5000 })
      const secondName = await heading.textContent()

      // Press ArrowLeft to go back
      await page.evaluate(() => {
        window.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowLeft', bubbles: true }))
      })

      // Should return to initial contact
      await expect(heading).toHaveText(initialName!, { timeout: 5000 })

      // The position pager buttons drive the same navigation.
      await page.getByRole('button', { name: 'Next candidate' }).click()
      await expect(heading).toHaveText(secondName!, { timeout: 5000 })
      await page.getByRole('button', { name: 'Previous candidate' }).click()
      await expect(heading).toHaveText(initialName!, { timeout: 5000 })

      // Arrow keys are inert while typing: with the name input focused,
      // ArrowRight must not navigate away from the current candidate.
      await heading.click()
      const nameInput = modal.getByRole('textbox').first()
      await expect(nameInput).toBeVisible({ timeout: 5000 })
      await nameInput.press('ArrowRight')
      await expect(nameInput).toHaveValue(new RegExp(displayName))
      await nameInput.press('Escape')
      await expect(heading).toHaveText(initialName!, { timeout: 5000 })
    })
  })

  test.describe('Cadence Selector', () => {
    let testApi: TestAPI

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    // spec: IMP-027[4]
    test('link mode pre-fills the cadence from the existing contact', async ({ page }) => {
      // Seed a candidate and a target contact with an existing cadence
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
      await page.waitForLoadState('domcontentloaded')

      // Open link modal
      const candidateName = `${testApi.prefix}-Cadence Link Candidate`
      const candidateCard = candidateCardByName(page, candidateName)
      await expect(candidateCard).toBeVisible({ timeout: 10000 })
      await candidateCard.getByRole('button', { name: /Link/i }).click()

      // Wait for modal to open in link mode
      await expect(page.getByRole('button', { name: 'Link to Existing' })).toBeVisible()
      await navigateModalToCandidate(page, candidateName)

      // Select a contact to link to
      const dialog = resolverDialog(page)
      await dialog.getByText('Search for a contact...').click()
      const searchInput = dialog.getByPlaceholder('Search for a contact...')
      await searchInput.fill(testApi.prefix)

      const contactOption = dialog.getByText(`${testApi.prefix}-Cadence Link Target`).last()
      await expect(contactOption).toBeVisible({ timeout: 5000 })
      await contactOption.click()

      // The cadence selector pre-fills from the existing contact.
      const cadenceSelect = page.locator('#contact-cadence')
      await expect(cadenceSelect).toHaveValue('quarterly', { timeout: 5000 })
    })

    // spec: IMP-027[4]
    test('should update cadence when linking contact', async ({ page }) => {
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

      const displayName = `${testApi.prefix}-Link Cadence Update Test`

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // Open link modal
      await candidateCardByName(page, displayName).getByRole('button', { name: /Link/i }).click()

      // Wait for modal
      await expect(page.getByRole('button', { name: 'Link to Existing' })).toBeVisible()
      await navigateModalToCandidate(page, displayName)

      // Select the target contact
      const dialog = resolverDialog(page)
      await dialog.getByText('Search for a contact...').click()
      const searchInput = dialog.getByPlaceholder('Search for a contact...')
      await searchInput.fill(testApi.prefix)

      const contactOption = dialog.getByText(`${testApi.prefix}-Link Cadence Target`).last()
      await expect(contactOption).toBeVisible({ timeout: 5000 })
      await contactOption.click()

      // Pre-fill proof, then choose a different cadence.
      const cadenceSelect = page.locator('#contact-cadence')
      await expect(cadenceSelect).toBeVisible({ timeout: 5000 })
      await expect(cadenceSelect).toHaveValue('monthly', { timeout: 5000 })
      await cadenceSelect.selectOption('weekly')
      await expect(cadenceSelect).toHaveValue('weekly')

      // The link request carries the chosen cadence (network-param proof).
      const linkRequestPromise = page.waitForRequest(
        req => req.method() === 'POST' && /\/imports\/.+\/link$/.test(req.url())
      )
      const linkResponsePromise = page.waitForResponse(
        response =>
          response.request().method() === 'POST' &&
          response.url().includes('/api/v1/imports/') &&
          response.url().endsWith('/link')
      )
      await page.getByRole('button', { name: /Link Contact/i }).click()
      const linkRequest = await linkRequestPromise
      expect(linkRequest.postDataJSON()?.cadence).toBe('weekly')
      const linkResponse = await linkResponsePromise
      expect(linkResponse.ok()).toBe(true)

      // Navigate to the contact and verify cadence was updated on the
      // dependent surface.
      await page.goto('/contacts')
      await page.waitForLoadState('domcontentloaded')

      const contactSearchInput = page.getByPlaceholder('Search contacts...')
      await contactSearchInput.fill(`${testApi.prefix}-Link Cadence Target`)
      await page.waitForLoadState('domcontentloaded')

      // Wait for search results and click on the contact
      const contactLink = page.getByText(`${testApi.prefix}-Link Cadence Target`)
      await expect(contactLink).toBeVisible({ timeout: 10000 })
      await contactLink.click()
      await page.waitForLoadState('domcontentloaded')

      // Verify cadence is now weekly - look for it in the contact detail section
      const cadenceValue = page.getByTestId('contact-cadence')
      await expect(cadenceValue).toBeVisible({ timeout: 10000 })
      await expect(cadenceValue).toContainText('weekly', { timeout: 5000 })
    })
  })

  test.describe('Name Editing', () => {
    let testApi: TestAPI

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    // spec: IMP-027[2]
    test('should enter edit mode when clicking name', async ({ page }) => {
      // Seed a candidate
      await testApi.seedExternalContacts([
        {
          display_name: 'Click Edit Test',
          emails: ['click-edit@example.com'],
        },
      ])

      const displayName = `${testApi.prefix}-Click Edit Test`

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // Open import modal
      await candidateCardByName(page, displayName)
        .getByRole('button', { name: /Import/i })
        .click()

      // Wait for modal
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

      // Navigate to correct candidate if needed (handles race conditions in parallel tests)
      await navigateModalToCandidate(page, displayName)

      // Click on the name heading within the modal to enter edit mode
      const modal = resolverDialog(page)
      await modal.getByRole('heading', { level: 3 }).first().click()

      // Verify input field appears with the name value
      const nameInput = modal.getByRole('textbox').first()
      await expect(nameInput).toBeVisible({ timeout: 5000 })
      await expect(nameInput).toHaveValue(new RegExp(displayName))
    })

    // spec: IMP-027[2], IMP-012[0], IMP-031[0]
    test('should edit name and persist on import', async ({ page, request }) => {
      // Seed a candidate
      await testApi.seedExternalContacts([
        {
          display_name: 'Original Import Name',
          emails: ['persist-name@example.com'],
        },
      ])

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      const displayName = `${testApi.prefix}-Original Import Name`
      const newName = `${testApi.prefix}-Edited Name For Import`

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // Open import modal
      await candidateCardByName(page, displayName)
        .getByRole('button', { name: /Import/i })
        .click()

      // Wait for modal
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

      // Navigate to correct candidate if needed (handles race conditions in parallel tests)
      await navigateModalToCandidate(page, displayName)

      // Click on the name to enter edit mode
      const modal = resolverDialog(page)
      await modal.getByRole('heading', { level: 3 }).first().click()

      // Wait for input to appear and edit the name
      const nameInput = modal.getByRole('textbox').first()
      await expect(nameInput).toBeVisible({ timeout: 5000 })

      // Clear and type new name, then press Enter to confirm
      await nameInput.fill(newName)
      await nameInput.press('Enter')

      // Verify the new name is shown in the heading
      await expect(
        modal.getByRole('heading', { level: 3 }).filter({ hasText: newName })
      ).toBeVisible()

      // Capture the import POST, then import.
      const importResponsePromise = page.waitForResponse(
        res => res.request().method() === 'POST' && /\/imports\/.+\/import$/.test(res.url())
      )
      await page.getByRole('button', { name: 'Import as New Contact', exact: true }).click()

      const importResponse = await importResponsePromise
      expect(importResponse.status()).toBe(201)
      const importBody = await importResponse.json()
      const createdContactId: string = importBody?.data?.contact?.id
      expect(createdContactId).toBeTruthy()

      // The candidate leaves the list (it was imported).
      await expect(page.getByText(displayName)).not.toBeVisible({ timeout: 15000 })

      // API-read: the contact was created with the edited name.
      const contactRes = await request.get(`${API_BASE_URL}/api/v1/contacts/${createdContactId}`, {
        headers: API_HEADERS,
      })
      expect(contactRes.ok()).toBe(true)
      const contactBody = await contactRes.json()
      expect(contactBody?.data?.full_name).toBe(newName)
    })
  })

  test.describe('Primary Method Selection', () => {
    let testApi: TestAPI

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    // spec: IMP-027[3]
    test('should only allow one primary method at a time', async ({ page }) => {
      // Seed a candidate with multiple contact methods
      await testApi.seedExternalContacts([
        {
          display_name: 'Single Primary Test',
          emails: ['single1@example.com', 'single2@example.com'],
        },
      ])

      const displayName = `${testApi.prefix}-Single Primary Test`

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      // Open import modal
      const candidateCard = candidateCardByName(page, displayName)
      await expect(candidateCard).toBeVisible({ timeout: 10000 })
      await candidateCard.getByRole('button', { name: /Import/i }).click()

      // Wait for modal
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

      // Navigate to correct candidate if needed (handles race conditions in parallel tests)
      await navigateModalToCandidate(page, displayName)

      const modal = resolverDialog(page)
      const stars = modal.getByRole('button', { name: 'Set as primary' })
      const pressedStars = modal.locator('button[aria-label="Set as primary"][aria-pressed="true"]')

      // Initially no method is primary.
      await expect(stars.first()).toBeVisible()
      await expect(pressedStars).toHaveCount(0)

      // Click the first star: exactly one method is primary.
      await stars.first().click()
      await expect(pressedStars).toHaveCount(1)

      // Click the second star: primary moves — still exactly one.
      await stars.nth(1).click()
      await expect(pressedStars).toHaveCount(1)
      await expect(stars.nth(1)).toHaveAttribute('aria-pressed', 'true')
      await expect(stars.first()).toHaveAttribute('aria-pressed', 'false')
    })
  })

  test.describe('Cadence Import Flow', () => {
    let testApi: TestAPI

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    // spec: IMP-027[4], IMP-031[0]
    test('should import contact with selected cadence', async ({ page, request }) => {
      // Seed a candidate for this isolated test
      await testApi.seedExternalContacts([
        {
          display_name: 'Cadence Import User',
          emails: ['cadence-import-user@example.com'],
        },
      ])

      const displayName = `${testApi.prefix}-Cadence Import User`

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // Open import modal on our contact
      const candidateCard = candidateCardByName(page, displayName)
      await candidateCard.getByRole('button', { name: /Import/i }).click()

      // Wait for modal and navigate to correct candidate
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()
      await navigateModalToCandidate(page, displayName)

      // Import mode defaults to no cadence, then select monthly.
      const cadenceSelect = page.locator('#contact-cadence')
      await expect(cadenceSelect).toBeVisible()
      await expect(cadenceSelect).toHaveValue('')
      await cadenceSelect.selectOption('monthly')

      // The import request carries the chosen cadence.
      const importRequestPromise = page.waitForRequest(
        req => req.method() === 'POST' && /\/imports\/.+\/import$/.test(req.url())
      )
      const importResponsePromise = page.waitForResponse(
        res => res.request().method() === 'POST' && /\/imports\/.+\/import$/.test(res.url())
      )
      await page.getByRole('button', { name: 'Import as New Contact', exact: true }).click()

      const importRequest = await importRequestPromise
      expect(importRequest.postDataJSON()?.cadence).toBe('monthly')
      const importResponse = await importResponsePromise
      expect(importResponse.status()).toBe(201)
      const importBody = await importResponse.json()
      const createdContactId: string = importBody?.data?.contact?.id
      expect(createdContactId).toBeTruthy()

      // The candidate card leaves the list (it was imported).
      await expect(candidateCard).not.toBeVisible({ timeout: 15000 })

      // API-read: the created contact carries the chosen cadence.
      const contactRes = await request.get(`${API_BASE_URL}/api/v1/contacts/${createdContactId}`, {
        headers: API_HEADERS,
      })
      expect(contactRes.ok()).toBe(true)
      const contactBody = await contactRes.json()
      expect(contactBody?.data?.cadence).toBe('monthly')
    })
  })
})
