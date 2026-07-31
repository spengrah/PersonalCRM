import { test, expect } from './fixtures'
import { createTestAPI, TestAPI } from './helpers/test-api'
import {
  candidateCardByName,
  expectModalCandidate,
  resolverDialog,
  selectContactIfNeeded,
} from './helpers/imports-helpers'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

// gmail_correspondence source: a link-only candidate carries an evidence badge
// (co-occurring contact + message count) and links to an existing contact,
// adding the email as a contact method. The link-only Import-hidden policy is
// enforced by the shared import-suggestions surface; this spec covers the
// evidence badge + the end-to-end link.
test.describe('Imports gmail_correspondence evidence @area:imports', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  // spec: IMP-037.evidence-badge-names-cooccurring, IMP-029.import-not-offered-link, IMP-031.item-leaves-queue-counts-update
  test('evidence badge renders, Import is hidden, and link adds the method', async ({
    page,
    request,
  }) => {
    // IMP-037's fixture is the CRM contact the candidate should suggest (same
    // name, so the trigram suggested-match pre-selects it) plus the
    // gmail_correspondence candidate carrying its co-occurrence evidence.
    const seeded = await testApi.seedBehavior('IMP-037')
    const targetContactId = seeded.entities['cooccur'].id
    const cardName = seeded.entities['corr'].name

    // The candidate's email is generator-derived, so it is read back from the
    // API rather than restated here — this is the value the link must copy onto
    // the contact.
    const candidateRes = await request.get(
      `${API_BASE_URL}/api/v1/imports/${seeded.entities['corr'].id}`,
      { headers: API_HEADERS }
    )
    expect(candidateRes.ok()).toBe(true)
    const candidateBody = await candidateRes.json()
    const correspondenceEmail: string = candidateBody?.data?.emails?.[0]?.value
    expect(correspondenceEmail, 'the seeded candidate must carry an email to link').toBeTruthy()

    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')

    await expect(page.getByText(cardName).first()).toBeVisible({ timeout: 10000 })

    const card = candidateCardByName(page, cardName)

    // The evidence badge: the seeded co-occurring contact name + the declared
    // message count (both metadata-driven data, not static copy). The count is
    // literal because it MIRRORS the declaration's own
    // CorrespondenceEvidence(4, …) rather than a generated value.
    await expect(card.getByText(`Seen with ${seeded.entities['cooccur'].name}`)).toBeVisible()
    await expect(card.getByText('4 messages')).toBeVisible()

    // Link-only: no Import button on the card.
    await expect(card.getByRole('button', { name: 'Import' })).toHaveCount(0)

    // Open the modal via Link. This is a link-only source, so the modal is
    // locked to link mode (no Import/Link toggle buttons) — wait for the
    // always-present Link Contact submit button instead.
    await card.getByRole('button', { name: /Link/i }).click()
    await expect(page.getByRole('button', { name: /Link Contact/i })).toBeVisible()
    await expectModalCandidate(page, cardName)

    // Ensure a contact is selected (the trigram match usually pre-selects it).
    const dialog = resolverDialog(page)
    await selectContactIfNeeded(page, dialog, cardName, cardName)
    await expect(page.getByRole('button', { name: /Link Contact/i })).toBeEnabled()

    // Link succeeds (200 response).
    const linkResponse = page.waitForResponse(res => /\/imports\/.+\/link$/.test(res.url()))
    await page.getByRole('button', { name: /Link Contact/i }).click()
    const res = await linkResponse
    expect(res.status()).toBe(200)

    // The link added the correspondence email to the existing contact.
    const contactRes = await request.get(`${API_BASE_URL}/api/v1/contacts/${targetContactId}`, {
      headers: API_HEADERS,
    })
    expect(contactRes.ok()).toBe(true)
    const contactBody = await contactRes.json()
    const methods: Array<{ type: string; value: string }> = contactBody?.data?.methods ?? []
    expect(methods.some(m => m.type === 'email' && m.value === correspondenceEmail)).toBe(true)

    // The candidate leaves the People list once linked.
    await page.keyboard.press('Escape')
    await expect(candidateCardByName(page, cardName)).toHaveCount(0, { timeout: 10000 })
  })
})
