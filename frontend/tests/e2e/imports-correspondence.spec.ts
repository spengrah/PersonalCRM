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
// adding the email as a contact method. Data is seeded per-worker for parallel
// isolation. The link-only Import-hidden policy is enforced by the shared
// import-suggestions surface; this spec covers the evidence badge + the
// end-to-end link.
test.describe('Imports gmail_correspondence evidence @area:imports', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  // spec: IMP-037[0], IMP-029[2], IMP-031[0]
  test('evidence badge renders, Import is hidden, and link adds the method', async ({
    page,
    request,
  }) => {
    // Seed the CRM contact the candidate should suggest (same name so the
    // trigram suggested-match pre-selects it), then the gmail_correspondence
    // candidate with evidence metadata. The seed route prefixes display names,
    // so the candidate's effective name matches the prefixed contact name.
    const correspondenceEmail = `correspondence-${testApi.prefix}@example.invalid`
    const { ids: contactIds } = await testApi.seedContacts([{ full_name: 'Correspondence Person' }])
    const targetContactId = contactIds[0]
    await testApi.seedExternalContacts([
      {
        display_name: 'Correspondence Person',
        source: 'gmail_correspondence',
        emails: [correspondenceEmail],
        metadata: {
          display_names_seen: ['Correspondence Person'],
          message_count: 4,
          co_occurring_contact: { id: '', name: `${testApi.prefix}-Correspondence Person` },
        },
      },
    ])

    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')

    const cardName = `${testApi.prefix}-Correspondence Person`
    await expect(page.getByText(cardName).first()).toBeVisible({ timeout: 10000 })

    const card = candidateCardByName(page, cardName)

    // The evidence badge: the seeded co-occurring contact name + the seeded
    // message count (both metadata-driven data, not static copy).
    await expect(card.getByText(`Seen with ${testApi.prefix}-Correspondence Person`)).toBeVisible()
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
    await selectContactIfNeeded(page, dialog, 'Correspondence Person', cardName)
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
