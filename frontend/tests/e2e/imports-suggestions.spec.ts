import { test, expect } from './fixtures'
import { createTestAPI, TestAPI } from './helpers/test-api'

// Unified People-tab suggestions surface: the method-suggestion group
// (above the confidence-ranked candidates) + the link-only candidate case.
// All data is seeded with a per-worker prefix for parallel isolation.
test.describe('Imports suggestions surface @area:imports', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('method-suggestion card appears at the top; Review confirms and clears it', async ({
    page,
  }) => {
    const email = `suggest-${testApi.prefix}@example.invalid`
    await testApi.seedMethodSuggestions({
      contact_name: 'Suggest Target',
      pending: [{ type: 'email', value: email }],
    })

    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')

    // The card renders "‹contact› — N new methods" (contact name is prefixed
    // by the seed route). Scope to THIS worker's card to stay parallel-safe.
    const cardText = `${testApi.prefix}-Suggest Target — 1 new method`
    const card = page.locator('div.border', { hasText: cardText }).first()
    await expect(card).toBeVisible({ timeout: 10000 })

    // Review opens the enrich-locked body: fixed contact header, NO
    // ContactSelector, NO Import.
    await card.getByRole('button', { name: 'Review' }).click()
    await expect(page.getByText(`Adding to ${testApi.prefix}-Suggest Target`)).toBeVisible()
    await expect(page.getByRole('button', { name: /Import as New/i })).toHaveCount(0)
    await expect(page.getByPlaceholder(/Search for a contact/i)).toHaveCount(0)

    // Confirm adds the method; the card disappears from the list.
    await page.getByRole('button', { name: 'Confirm' }).click()
    await expect(page.getByText(cardText)).toHaveCount(0, {
      timeout: 10000,
    })
  })

  test('Dismiss removes the card and it does not return after reload', async ({ page }) => {
    const email = `dismiss-${testApi.prefix}@example.invalid`
    await testApi.seedMethodSuggestions({
      contact_name: 'Dismiss Target',
      pending: [{ type: 'email', value: email }],
    })

    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')

    const cardText = `${testApi.prefix}-Dismiss Target — 1 new method`
    const card = page.locator('div.border', { hasText: cardText }).first()
    await expect(card).toBeVisible({ timeout: 10000 })

    await card.getByRole('button', { name: 'Dismiss' }).click()
    await expect(page.getByText(cardText)).toHaveCount(0, { timeout: 10000 })

    // Reload: the dismissed suggestion stays gone (sticky).
    await page.reload()
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByText(cardText)).toHaveCount(0, { timeout: 10000 })
  })

  test('link-only source hides Import on the card and in the modal', async ({ page }) => {
    // Seed a gmail_correspondence unmatched candidate (link-only source).
    await testApi.seedExternalContacts([
      {
        display_name: 'LinkOnly Person',
        source: 'gmail_correspondence',
        emails: [`linkonly-${testApi.prefix}@example.invalid`],
      },
    ])

    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')

    const cardName = `${testApi.prefix}-LinkOnly Person`
    await expect(page.getByText(cardName).first()).toBeVisible({ timeout: 10000 })

    // Scope to the candidate card via its heading, then walk up to the card
    // container (the bordered wrapper holding the action buttons).
    const card = page
      .locator('div.border', { has: page.getByRole('heading', { name: cardName }) })
      .first()

    // The card has no Import button (link-only source) — only Link + ignore.
    await expect(card.getByRole('button', { name: 'Import' })).toHaveCount(0)

    // Open the modal via the Link button (no suggested match → "Link (select)").
    await card.getByRole('button', { name: /Link/i }).click()

    // The modal is locked to link mode — the Import-mode toggle is absent.
    await expect(page.getByRole('button', { name: /Import as New/i })).toHaveCount(0)
    await expect(page.getByRole('button', { name: /Link to Existing/i })).toHaveCount(0)
  })
})
