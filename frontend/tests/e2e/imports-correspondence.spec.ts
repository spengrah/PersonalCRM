import { test, expect } from './fixtures'
import { createTestAPI, TestAPI } from './helpers/test-api'
import { navigateModalToCandidate } from './helpers/imports-helpers'

// gmail_correspondence source (C): a link-only candidate carries an evidence
// badge (co-occurring contact + message count) and links to an existing
// contact, adding the email as a contact method. Data is seeded per-worker for
// parallel isolation. The link-only Import-hidden policy is B's; this spec
// covers C's net-new evidence badge + the end-to-end link.
test.describe('Imports gmail_correspondence evidence @area:imports', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('evidence badge renders, Import is hidden, and link adds the method', async ({ page }) => {
    // Seed the CRM contact the candidate should suggest (same name so the
    // trigram suggested-match pre-selects it), then the gmail_correspondence
    // candidate with evidence metadata. The seed route prefixes display names,
    // so the candidate's effective name matches the prefixed contact name.
    await testApi.seedContacts([{ full_name: 'Correspondence Person' }])
    await testApi.seedExternalContacts([
      {
        display_name: 'Correspondence Person',
        source: 'gmail_correspondence',
        emails: [`correspondence-${testApi.prefix}@example.invalid`],
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

    const card = page
      .locator('div.border', { has: page.getByRole('heading', { name: cardName }) })
      .first()

    // C's net-new UI: the evidence badge (co-occurring contact + message count).
    await expect(card.getByText(`Seen with ${testApi.prefix}-Correspondence Person`)).toBeVisible()
    await expect(card.getByText('4 messages')).toBeVisible()

    // Link-only: no Import button on the card.
    await expect(card.getByRole('button', { name: 'Import' })).toHaveCount(0)

    // Open the modal via Link, steer it to THIS worker's candidate, and link.
    await card.getByRole('button', { name: /Link/i }).click()
    await expect(page.getByRole('button', { name: /Link to Existing/i })).toBeVisible()
    await navigateModalToCandidate(page, cardName)

    // Ensure a contact is selected (the trigram match usually pre-selects it).
    const search = page.locator('input[placeholder="Search for a contact..."]')
    if (await search.isVisible().catch(() => false)) {
      await search.fill('Correspondence Person')
      await page.getByText(cardName, { exact: true }).last().click()
    }
    await expect(page.getByRole('button', { name: /Link Contact/i })).toBeEnabled()

    // Link adds the email method to the existing contact (200 response).
    const linkResponse = page.waitForResponse(res => /\/imports\/.+\/link$/.test(res.url()))
    await page.getByRole('button', { name: /Link Contact/i }).click()
    const res = await linkResponse
    expect(res.status()).toBe(200)

    // The candidate leaves the People list once linked.
    await page.keyboard.press('Escape')
    await expect(
      page.locator('div.border', { has: page.getByRole('heading', { name: cardName }) })
    ).toHaveCount(0, { timeout: 10000 })
  })
})
