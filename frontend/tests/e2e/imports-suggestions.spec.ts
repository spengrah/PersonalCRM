import { test, expect } from './fixtures'
import { createTestAPI, TestAPI } from './helpers/test-api'
import { navigateModalToCandidate } from './helpers/imports-helpers'

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

  test('§4 residual: deselect-all link removes the candidate', async ({ page }) => {
    // Seed a CRM contact + a same-named gcontacts candidate so the modal
    // opens with the contact auto-selected (suggested match).
    await testApi.seedContacts([{ full_name: 'Deselect Target' }])
    await testApi.seedExternalContacts([
      {
        display_name: 'Deselect Target',
        source: 'gcontacts',
        emails: [`deselect-${testApi.prefix}@example.invalid`],
      },
    ])

    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')

    const cardName = `${testApi.prefix}-Deselect Target`
    const candidateCard = (): import('@playwright/test').Locator =>
      page.locator('div.border', { has: page.getByRole('heading', { name: cardName }) })
    await expect(candidateCard()).toHaveCount(1, { timeout: 10000 })

    // Open the modal via Link.
    await candidateCard().getByRole('button', { name: /Link/i }).click()
    await expect(page.getByRole('button', { name: /Link to Existing/i })).toBeVisible()

    // The modal refetches ALL candidates (parallel workers included) and
    // navigates by index, so steer it to THIS worker's candidate first.
    await navigateModalToCandidate(page, cardName)

    // Ensure a contact is selected. The trigram suggested-match usually
    // pre-selects the same-named CRM contact (ContactSelector then shows the
    // name, not the search input); if it did not, select explicitly.
    const search = page.locator('input[placeholder="Search for a contact..."]')
    if (await search.isVisible().catch(() => false)) {
      await search.fill('Deselect Target')
      await page.getByText(cardName, { exact: true }).last().click()
    }
    // The Link button is enabled only once a contact is selected.
    await expect(page.getByRole('button', { name: /Link Contact/i })).toBeEnabled()

    // Deselect every method (the modal rendered the method-selection UI).
    let remaining = await page.getByRole('button', { name: 'Deselect method' }).count()
    while (remaining > 0) {
      // Re-query each iteration since toggling relabels the button.
      await page.getByRole('button', { name: 'Deselect method' }).first().click()
      remaining = await page.getByRole('button', { name: 'Deselect method' }).count()
    }

    // Link with no methods selected. The §4 frontend behavior: methods_curated
    // is sent true (the modal offered method curation) with an empty
    // selected_methods — proving the deselect-all path. Capture the request
    // and assert the link succeeds (200).
    const linkRequest = page.waitForRequest(
      req => req.url().includes('/methods/') === false && /\/imports\/.+\/link$/.test(req.url())
    )
    const linkResponse = page.waitForResponse(res => /\/imports\/.+\/link$/.test(res.url()))
    await page.getByRole('button', { name: /Link Contact/i }).click()

    const req = await linkRequest
    const body = req.postDataJSON()
    expect(body.methods_curated).toBe(true)
    expect(body.selected_methods ?? []).toEqual([])

    const res = await linkResponse
    expect(res.status()).toBe(200)

    // The modal closes (or advances) and the unmatched candidate leaves the
    // People list. Close the modal, then assert the candidate card is gone
    // from the list (the same-named CRM contact still exists elsewhere, so
    // scope to the list card via its heading). The toHaveCount assertion
    // retries on the live UI, so no networkidle wait is needed.
    await page.keyboard.press('Escape')
    await expect(
      page.locator('div.border', { has: page.getByRole('heading', { name: cardName }) })
    ).toHaveCount(0, { timeout: 10000 })
  })
})
