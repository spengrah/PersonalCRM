import { test, expect } from './fixtures'
import { createTestAPI, TestAPI } from './helpers/test-api'
import {
  expectModalCandidate,
  findCandidateByName,
  candidateCardByName,
  resolverDialog,
  selectContactIfNeeded,
} from './helpers/imports-helpers'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'

/**
 * Walk the ALREADY-OPEN modal to a candidate using its own Prev/Next pager.
 * Opening from a card is keyed by id and never needs this — it exists solely
 * for in-session flows where the modal advanced in place onto whatever the
 * global queue put at that position (possibly another worker's candidate)
 * and the test must reach its own next candidate without closing the modal.
 * Bounded scan: back to the start, then forward, asserting the target at the
 * end so a miss fails loudly.
 */
async function walkModalToCandidate(
  page: import('@playwright/test').Page,
  displayName: string,
  maxSteps = 30
): Promise<void> {
  const dialog = resolverDialog(page)
  const heading = dialog.getByRole('heading', { level: 3 })
  const target = dialog.getByRole('heading', { level: 3, name: displayName })
  const prev = dialog.getByRole('button', { name: 'Previous candidate' })
  const next = dialog.getByRole('button', { name: 'Next candidate' })
  for (const button of [prev, next]) {
    for (let i = 0; i < maxSteps; i++) {
      if (await target.isVisible().catch(() => false)) return
      if (!(await button.isEnabled().catch(() => false))) break
      const before = await heading.textContent()
      await button.click()
      if (before) {
        // Wait out the pager transition (heading settles on the neighbor).
        await expect(heading)
          .not.toHaveText(before, { timeout: 2000 })
          .catch(() => {})
      }
    }
  }
  await expect(target).toBeVisible({ timeout: 2000 })
}
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

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

    // spec: IMP-028.position-pager-arrow-keys
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

      // Wait for the modal, open on our candidate
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()
      await expectModalCandidate(page, displayName)

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

    // spec: IMP-027.cadence-chosen-or-prefilled
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
      await expectModalCandidate(page, candidateName)

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

    // spec: IMP-027.cadence-chosen-or-prefilled
    test('should update cadence when linking contact', async ({ page, request }) => {
      await testApi.seedExternalContacts([
        {
          display_name: 'Link Cadence Update Test',
          emails: ['link-update@example.com'],
        },
      ])

      const { ids: targetIds } = await testApi.seedOverdueContacts([
        {
          full_name: 'Link Cadence Target',
          email: 'link-target@example.com',
          cadence: 'monthly', // Use monthly, will change to weekly
          days_overdue: 1,
        },
      ])
      const targetContactId = targetIds[0]

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      const displayName = `${testApi.prefix}-Link Cadence Update Test`

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // Open link modal
      await candidateCardByName(page, displayName).getByRole('button', { name: /Link/i }).click()

      // Wait for modal
      await expect(page.getByRole('button', { name: 'Link to Existing' })).toBeVisible()
      await expectModalCandidate(page, displayName)

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

      // Verify the cadence actually changed on the linked contact. The
      // network-param assert above proves what the UI SENT; this proves the
      // stored effect, via the same API the contact surfaces render from —
      // a UI walk to the detail page here re-tests navigation, not linking.
      const contactRes = await request.get(`${API_BASE_URL}/api/v1/contacts/${targetContactId}`, {
        headers: { 'X-API-Key': API_KEY },
      })
      expect(contactRes.ok()).toBe(true)
      const contactBody = await contactRes.json()
      expect(contactBody?.data?.cadence).toBe('weekly')
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

    // spec: IMP-027.name-editable-empty-blocks
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

      await expectModalCandidate(page, displayName)

      // Click on the name heading within the modal to enter edit mode
      const modal = resolverDialog(page)
      await modal.getByRole('heading', { level: 3 }).first().click()

      // Verify input field appears with the name value
      const nameInput = modal.getByRole('textbox').first()
      await expect(nameInput).toBeVisible({ timeout: 5000 })
      await expect(nameInput).toHaveValue(new RegExp(displayName))
    })

    // spec: IMP-027.name-editable-empty-blocks, IMP-012.crm-contact-created-normal-path, IMP-031.item-leaves-queue-counts-update
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

      await expectModalCandidate(page, displayName)

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

    // spec: IMP-027.methods-selectable-one-primary
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

      await expectModalCandidate(page, displayName)

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

    // spec: IMP-027.cadence-chosen-or-prefilled, IMP-031.item-leaves-queue-counts-update
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

      // Wait for the modal, open on our candidate
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()
      await expectModalCandidate(page, displayName)

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

  test.describe('Resolution Guards', () => {
    let testApi: TestAPI

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    // spec: IMP-027.name-editable-empty-blocks
    test('an empty name blocks resolution', async ({ page }) => {
      const { ids } = await testApi.seedExternalContacts([
        {
          display_name: 'Empty Name Guard',
          emails: ['empty-name-guard@example.com'],
        },
      ])
      const externalId = ids[0]
      const displayName = `${testApi.prefix}-Empty Name Guard`

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')
      await findCandidateByName(page, displayName)

      await candidateCardByName(page, displayName)
        .getByRole('button', { name: /Import/i })
        .click()
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()
      await expectModalCandidate(page, displayName)

      const modal = resolverDialog(page)

      // Count import POSTs for this candidate: the blocked attempt must not
      // add one, so after the subsequent successful import exactly ONE has
      // fired (no sleep needed — the success anchors the negative proof).
      let importPosts = 0
      page.on('request', req => {
        if (req.method() === 'POST' && req.url().includes(`/imports/${externalId}/import`)) {
          importPosts++
        }
      })

      // Clear the name, then attempt to import — blocked, modal stays open.
      await modal.getByRole('heading', { level: 3 }).first().click()
      const nameInput = modal.getByRole('textbox').first()
      await expect(nameInput).toBeVisible({ timeout: 5000 })
      await nameInput.fill('')
      await page.getByRole('button', { name: 'Import as New Contact', exact: true }).click()
      await expect(modal).toBeVisible()

      // Restore a valid name and import — this one goes through, proving the
      // earlier click was processed and rejected (else the count would be 2).
      await nameInput.fill(`${testApi.prefix}-Renamed After Block`)
      await nameInput.press('Enter')
      const importResponsePromise = page.waitForResponse(
        res =>
          res.request().method() === 'POST' && res.url().includes(`/imports/${externalId}/import`)
      )
      await page.getByRole('button', { name: 'Import as New Contact', exact: true }).click()
      const importResponse = await importResponsePromise
      expect(importResponse.status()).toBe(201)
      expect(importPosts).toBe(1)
    })

    // spec: IMP-027.name-editable-empty-blocks
    test('an unresolved telegram peer requires a name edit before import', async ({ page }) => {
      // An unresolved peer: telegram source with no name fields, no
      // username, and no methods. Hidden by default behind the opt-in
      // "Show unresolved" toggle. Every unresolved peer displays as
      // "Unknown", so a unique organization marker (which does NOT affect
      // the unresolved classification) binds the CARD to this worker's row.
      const orgMarker = `${testApi.prefix}-tg-org`
      const { ids } = await testApi.seedExternalContacts([
        { source: 'telegram', organization: orgMarker },
      ])
      const externalId = ids[0]

      await page.goto('/imports')
      await page.waitForLoadState('domcontentloaded')
      await page.getByRole('button', { name: 'Telegram', exact: true }).click()

      // Opt in to unresolved peers, then open OUR card (bound by the org
      // marker, not the shared "Unknown" display name).
      const unresolvedToggle = page.getByRole('switch')
      await expect(unresolvedToggle).toBeVisible({ timeout: 10000 })
      await unresolvedToggle.click()
      const ourCard = page.locator('div.border', { hasText: orgMarker }).last()
      await expect(ourCard).toBeVisible({ timeout: 10000 })
      await ourCard.getByRole('button', { name: /Import/i }).click()
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()
      // The modal opens on the clicked card's peer (keyed by id); every
      // unresolved peer displays as "Unknown", so the import POST below is
      // additionally pinned to OUR external id and fails loudly on a mixup.
      // spec: IMP-028.card-opens-that-candidate
      await expectModalCandidate(page, 'Unknown')

      const modal = resolverDialog(page)

      // Count import POSTs for this candidate: the blocked attempt must not
      // add one, so after the subsequent successful import exactly ONE has
      // fired (no sleep needed — the success anchors the negative proof).
      let importPosts = 0
      page.on('request', req => {
        if (req.method() === 'POST' && req.url().includes(`/imports/${externalId}/import`)) {
          importPosts++
        }
      })

      // Importing without a name edit is blocked: the modal stays open.
      await page.getByRole('button', { name: 'Import as New Contact', exact: true }).click()
      await expect(modal).toBeVisible()

      // Editing the name unblocks the import.
      const editedName = `${testApi.prefix}-Named Telegram Peer`
      await modal.getByRole('heading', { level: 3 }).first().click()
      const nameInput = modal.getByRole('textbox').first()
      await expect(nameInput).toBeVisible({ timeout: 5000 })
      await nameInput.fill(editedName)
      await nameInput.press('Enter')

      const importResponsePromise = page.waitForResponse(
        res =>
          res.request().method() === 'POST' && res.url().includes(`/imports/${externalId}/import`)
      )
      await page.getByRole('button', { name: 'Import as New Contact', exact: true }).click()
      const importResponse = await importResponsePromise
      expect(importResponse.status()).toBe(201)
      expect(importPosts).toBe(1)
    })

    // spec: IMP-027.methods-selectable-one-primary
    test('link mode disables already-present methods and offers additions', async ({ page }) => {
      // The CRM contact already carries email X; the candidate carries the
      // identical X, a same-type different-value email Z, and a phone Y.
      const emailX = `x-${testApi.prefix}@example.invalid`
      const emailZ = `z-${testApi.prefix}@example.invalid`
      const phoneY = '+15550001234'
      await testApi.seedContacts([
        {
          full_name: 'Method Buckets Target',
          methods: [{ type: 'email', value: emailX }],
        },
      ])
      await testApi.seedExternalContacts([
        {
          display_name: 'Method Buckets Target',
          source: 'icloud_contacts',
          emails: [emailX, emailZ],
          phones: [phoneY],
        },
      ])

      await page.goto('/imports')
      await page.waitForLoadState('domcontentloaded')

      const cardName = `${testApi.prefix}-Method Buckets Target`
      await findCandidateByName(page, cardName)
      await candidateCardByName(page, cardName).getByRole('button', { name: /Link/i }).click()
      await expect(page.getByRole('button', { name: 'Link to Existing' })).toBeVisible()
      await expectModalCandidate(page, cardName)

      // Ensure the same-named CRM contact is selected (trigram usually
      // pre-selects it; select explicitly if not).
      const dialog = resolverDialog(page)
      await selectContactIfNeeded(page, dialog, 'Method Buckets Target', cardName)
      await expect(page.getByRole('button', { name: /Link Contact/i })).toBeEnabled()

      const methodRow = (value: string) => dialog.locator('div.border', { hasText: value }).last()

      // The identical email X cannot be re-selected: its toggle is disabled.
      const rowX = methodRow(emailX)
      await expect(rowX).toBeVisible({ timeout: 10000 })
      await expect(
        rowX.getByRole('button', { name: /Select method|Deselect method/ })
      ).toBeDisabled()

      // The additions are offered under the to-add grouping with live toggles:
      // the phone Y and the same-type different-value email Z (alongside the
      // CRM value, which stays visible as X's row).
      await expect(dialog.getByText('Will be added')).toBeVisible()
      await expect(methodRow(phoneY).getByRole('button', { name: 'Deselect method' })).toBeEnabled()
      await expect(methodRow(emailZ).getByRole('button', { name: 'Deselect method' })).toBeEnabled()
    })
  })

  test.describe('Resolve Advance', () => {
    let testApi: TestAPI

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    // spec: IMP-028.modal-advances-to-next
    test('advances to the next candidate after resolving, closing only when the queue is exhausted', async ({
      page,
    }) => {
      const { ids } = await testApi.seedExternalContacts([
        {
          display_name: 'Advance Test One',
          source: 'icloud_contacts',
          emails: [`advance-one-${testApi.prefix}@example.invalid`],
        },
        {
          display_name: 'Advance Test Two',
          source: 'icloud_contacts',
          emails: [`advance-two-${testApi.prefix}@example.invalid`],
        },
      ])
      const [idOne, idTwo] = ids
      const nameOne = `${testApi.prefix}-Advance Test One`
      const nameTwo = `${testApi.prefix}-Advance Test Two`

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')
      await findCandidateByName(page, nameOne)

      await candidateCardByName(page, nameOne)
        .getByRole('button', { name: /Import/i })
        .click()
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()
      await expectModalCandidate(page, nameOne)

      const dialog = resolverDialog(page)
      const heading = dialog.getByRole('heading', { level: 3 }).first()

      // Resolve the first candidate; the invalidation-triggered queue refetch
      // follows the successful import.
      const importOne = page.waitForResponse(
        res => res.request().method() === 'POST' && res.url().includes(`/imports/${idOne}/import`)
      )
      const refetchAfterOne = page.waitForResponse(
        res =>
          res.request().method() === 'GET' &&
          res.url().includes('/api/v1/imports/candidates') &&
          res.url().includes('limit=1000')
      )
      await page.getByRole('button', { name: 'Import as New Contact', exact: true }).click()
      expect((await importOne).status()).toBe(201)
      await refetchAfterOne

      // Our second candidate is still queued, so the modal must NOT close —
      // it advances off the resolved candidate.
      await expect(dialog).toBeVisible()
      await expect(heading).not.toHaveText(nameOne, { timeout: 10000 })

      // Resolve the second candidate WITHOUT leaving the modal: the in-place
      // advance may have landed on another worker's candidate, so walk the
      // modal's own pager to ours. Staying in-session proves an
      // advanced-in-place modal is still actionable end-to-end (per-candidate
      // state re-initialized, action wired to the newly displayed candidate)
      // — the import POST below is pinned to OUR second external id.
      await walkModalToCandidate(page, nameTwo)
      const importTwo = page.waitForResponse(
        res => res.request().method() === 'POST' && res.url().includes(`/imports/${idTwo}/import`)
      )
      await page.getByRole('button', { name: 'Import as New Contact', exact: true }).click()
      expect((await importTwo).status()).toBe(201)

      // With OUR queue exhausted the modal must reach a terminal state:
      // closed (nothing left anywhere) or advanced onto another worker's
      // candidate (shared queue non-empty). Which arm applies depends on the
      // shared queue at action time, so poll for either — never for the
      // resolved candidate lingering. The close-only-when-exhausted precision
      // is deterministically covered by the first resolve above (a queued
      // candidate of OURS proved the modal did not close early).
      await expect
        .poll(
          async () => {
            const open = await dialog.isVisible().catch(() => false)
            if (!open) return 'closed'
            // Short-timeout read: if the dialog closes between the visibility
            // check and this read, the locator must fail fast (not auto-wait
            // past the poll budget) so the next iteration sees it closed.
            const text = (await heading.textContent({ timeout: 500 }).catch(() => '')) ?? ''
            return text.includes(nameTwo) ? 'pending' : 'advanced'
          },
          { timeout: 15000 }
        )
        .not.toBe('pending')
    })
  })

  test.describe('Dismissal', () => {
    let testApi: TestAPI

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    // spec: IMP-039.pressing-escape-closes-modal, IMP-039.clicking-backdrop-closes-modal, IMP-039.cancel-action-closes-modal, IMP-039.dismissal-resolves-nothing
    test('Escape, backdrop, and Cancel dismiss without resolving', async ({ page, request }) => {
      const { ids } = await testApi.seedExternalContacts([
        {
          display_name: 'Dismiss Modal Test',
          emails: ['dismiss-modal@example.com'],
        },
      ])
      const externalId = ids[0]
      const displayName = `${testApi.prefix}-Dismiss Modal Test`

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')
      await findCandidateByName(page, displayName)

      const dialog = resolverDialog(page)
      const openModal = async () => {
        await candidateCardByName(page, displayName)
          .getByRole('button', { name: /Import/i })
          .click()
        await expect(dialog).toBeVisible()
        await expectModalCandidate(page, displayName)
      }

      await openModal()

      // An uncommitted name edit is discarded by Escape: the ORIGINAL name
      // persists and the modal stays open (input Escape cancels the edit,
      // not the dialog).
      await dialog.getByRole('heading', { level: 3 }).first().click()
      const nameInput = dialog.getByRole('textbox').first()
      await expect(nameInput).toBeVisible({ timeout: 5000 })
      await nameInput.fill('Should Not Save')
      await nameInput.press('Escape')
      await expect(
        dialog.getByRole('heading', { level: 3 }).filter({ hasText: displayName })
      ).toBeVisible()
      await expect(dialog).toBeVisible()

      // Escape (outside an input) closes the modal.
      await page.keyboard.press('Escape')
      await expect(dialog).not.toBeVisible()

      // Reopen: the discarded edit did not stick to the candidate.
      await openModal()
      await expect(
        dialog.getByRole('heading', { level: 3 }).filter({ hasText: displayName })
      ).toBeVisible()

      // Clicking the backdrop (the overlay outside the panel) closes it.
      await dialog.locator('xpath=..').click({ position: { x: 10, y: 10 } })
      await expect(dialog).not.toBeVisible()

      // The Cancel action closes it.
      await openModal()
      await dialog.getByRole('button', { name: 'Cancel', exact: true }).click()
      await expect(dialog).not.toBeVisible()

      // Nothing was resolved: the candidate is still unmatched and still in
      // its queue.
      await expect(candidateCardByName(page, displayName)).toBeVisible()
      const candidateRes = await request.get(`${API_BASE_URL}/api/v1/imports/${externalId}`, {
        headers: API_HEADERS,
      })
      expect(candidateRes.ok()).toBe(true)
      const candidateBody = await candidateRes.json()
      expect(candidateBody?.data?.match_status).toBe('unmatched')
    })
  })
})
