import { test, expect } from './fixtures'
import type { APIRequestContext } from '@playwright/test'
import { createTestAPI, TestAPI, declaredWorldNamePrefix } from './helpers/test-api'
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

/**
 * The primary email a declared candidate actually carries, read back from the
 * candidate endpoint. The generator owns it, so a test that restated the address
 * would be asserting against a string it invented rather than the seeded one.
 */
async function primaryEmailOf(request: APIRequestContext, candidateId: string): Promise<string> {
  const res = await request.get(`${API_BASE_URL}/api/v1/imports/${candidateId}`, {
    headers: API_HEADERS,
  })
  expect(res.ok()).toBe(true)
  const email: string = (await res.json())?.data?.emails?.[0]?.value
  expect(email, 'the declared candidate must carry an email').toBeTruthy()
  return email
}

test.describe('Imports Actions @area:imports', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  // spec: IMP-007.unmatched-rows-await-review, IMP-027.user-chooses-import-new
  test('should display candidate cards with correct information', async ({ page }) => {
    const seeded = await testApi.seedBehavior('IMP-007')
    const displayName = seeded.entities['cand'].name

    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    // The declared unmatched candidate awaits review in the queue.
    await findCandidateByName(page, displayName)

    // This card offers both resolution choices: import-as-new and link.
    const card = candidateCardByName(page, displayName)
    await expect(card.getByRole('button', { name: /Import/i })).toBeVisible()
    await expect(card.getByRole('button', { name: /Link/i })).toBeVisible()
  })

  // spec: IMP-027.user-chooses-import-new
  test('should open link modal when clicking Link button', async ({ page }) => {
    // Rides IMP-027's resolver fixture for its plain address-book candidate.
    const seeded = await testApi.seedBehavior('IMP-027')
    const displayName = seeded.entities['plain'].name

    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    await findCandidateByName(page, displayName)

    // Click the Link button on our candidate
    await candidateCardByName(page, displayName).getByRole('button', { name: /Link/i }).click()

    // The modal offers the import-vs-link mode choice.
    await expect(page.getByRole('button', { name: 'Link to Existing' })).toBeVisible()

    await expectModalCandidate(page, displayName)

    // Link mode offers contact selection (the searchable selector).
    const dialog = page.getByRole('dialog', { name: 'Resolve import candidate' })
    await dialog.getByText('Search for a contact...').click()
    await expect(dialog.getByPlaceholder('Search for a contact...')).toBeVisible()
  })

  // spec: IMP-012.crm-contact-created-normal-path, IMP-012.methods-come-from-selection, IMP-012.candidate-row-marked-imported, IMP-012.response-reports-new-contact
  // spec: IMP-007.imported-marks-rows-resolved, IMP-031.item-leaves-queue-counts-update, IMP-031.returned-rematch-job-registered
  test('should import candidate and persist the created contact', async ({ page, request }) => {
    const seeded = await testApi.seedBehavior('IMP-012')
    const externalId = seeded.entities['addressbook'].id
    const displayName = seeded.entities['addressbook'].name
    const candidateEmail = await primaryEmailOf(request, externalId)

    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    await findCandidateByName(page, displayName)

    // Click Import on the specific candidate card (not just any Import button)
    const card = candidateCardByName(page, displayName)
    await card.getByRole('button', { name: /Import/i }).click()

    // Verify modal opens in import mode - mode toggle should be visible
    await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

    // The modal opens on the clicked card's candidate (keyed by id); the
    // import POST below is pinned to OUR external id, proving the clicked
    // candidate is the one that gets resolved.
    // spec: IMP-028.card-opens-that-candidate
    await expectModalCandidate(page, displayName)

    // Capture the import POST and the frontend's first rematch-job poll
    // before triggering the action.
    const importResponsePromise = page.waitForResponse(
      res =>
        res.url().includes(`/api/v1/imports/${externalId}/import`) &&
        res.request().method() === 'POST'
    )
    const rematchPollPromise = page.waitForResponse(
      res => res.url().includes('/api/v1/rematch/jobs/') && res.request().method() === 'GET'
    )

    // Click the "Import as New Contact" button in the modal
    await page.getByRole('button', { name: 'Import as New Contact', exact: true }).click()

    const importResponse = await importResponsePromise
    expect(importResponse.status()).toBe(201)
    const importBody = await importResponse.json()
    const createdContactId: string = importBody?.data?.contact?.id
    expect(createdContactId, 'import response should carry the created contact id').toBeTruthy()
    const rematchJobId: string = importBody?.data?.rematch_job_id
    expect(rematchJobId, 'import response should carry a rematch job id').toBeTruthy()

    // The returned rematch job is registered for polling: the frontend's
    // watcher polls exactly that job id.
    const rematchPoll = await rematchPollPromise
    expect(rematchPoll.url()).toContain(rematchJobId)

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
    expect(methods.some(m => m.type === 'email' && m.value === candidateEmail)).toBe(true)
  })

  // spec: IMP-007.ignored-terminal-sticky-row, IMP-031.item-leaves-queue-counts-update
  test('should ignore candidate stickily', async ({ page, request }) => {
    const seeded = await testApi.seedBehavior('IMP-007')
    const externalId = seeded.entities['cand'].id
    const displayName = seeded.entities['cand'].name

    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    await findCandidateByName(page, displayName)

    // Click the X (ignore) button on the candidate
    // The ignore button is a ghost button with just an X icon, aria-label "Ignore candidate"
    const candidateCard = candidateCardByName(page, displayName)
    const ignoreButton = candidateCard.getByRole('button', { name: 'Ignore candidate' })

    const ignoreResponsePromise = page.waitForResponse(
      res =>
        res.url().includes(`/api/v1/imports/${externalId}/ignore`) &&
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

    // Sticky proof: the row never resurfaces in the candidate QUEUE. This
    // is a distinct claim from the match_status write above — the queue
    // read has its own filter, and the DOM check earlier is page-local
    // (a re-sorted global pool could hide a still-listed ignored card on
    // another page). The large-limit API scan is page-independent and
    // parallel-safe: it only asserts our own id is absent.
    const candidatesRes = await request.get(
      `${API_BASE_URL}/api/v1/imports/candidates?limit=10000`,
      { headers: API_HEADERS }
    )
    expect(candidatesRes.ok()).toBe(true)
    const candidatesBody = await candidatesRes.json()
    const candidates: Array<{ id: string }> = candidatesBody?.data ?? []
    expect(candidates.some(c => c.id === externalId)).toBe(false)
  })

  // spec: IMP-013.any-curation-signal-imports, IMP-007.imported-marks-rows-resolved, IMP-031.item-leaves-queue-counts-update
  test('should link candidate to existing contact', async ({ page, request }) => {
    // IMP-013's unrelated pair: the candidate matches no contact, so the link
    // target is chosen explicitly rather than accepted from a suggestion.
    const seeded = await testApi.seedBehavior('IMP-013')
    const externalId = seeded.entities['unmatched'].id
    const candidateName = seeded.entities['unmatched'].name
    const targetContactId = seeded.entities['plain'].id
    const targetName = seeded.entities['plain'].name

    await page.goto('/imports')
    await page.waitForLoadState('networkidle')

    await findCandidateByName(page, candidateName)

    // Find the candidate card and click its Link button
    const candidateCard = candidateCardByName(page, candidateName)
    await candidateCard.getByRole('button', { name: /Link/i }).click()

    // Wait for modal to open with mode toggle visible
    await expect(page.getByRole('button', { name: 'Link to Existing' })).toBeVisible()

    await expectModalCandidate(page, candidateName)

    // The ContactSelector is a custom searchable dropdown filtering client-side
    // by substring, so the declared world's name prefix reaches exactly its own
    // contacts. Click the selector area (the placeholder) to open it.
    const dialog = page.getByRole('dialog', { name: 'Resolve import candidate' })
    await dialog.getByText('Search for a contact...').click()

    const searchInput = dialog.getByPlaceholder('Search for a contact...')
    await searchInput.fill(declaredWorldNamePrefix(seeded))

    // Wait for the dropdown to show the contact and click it
    const contactOption = dialog.getByText(targetName, { exact: true }).last()
    await expect(contactOption).toBeVisible({ timeout: 5000 })
    await contactOption.click()

    // Capture the link POST before triggering it.
    const linkResponsePromise = page.waitForResponse(
      res =>
        res.url().includes(`/api/v1/imports/${externalId}/link`) &&
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
