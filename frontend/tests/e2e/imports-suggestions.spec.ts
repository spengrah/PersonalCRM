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

  // spec: IMP-026[0], IMP-030[0], IMP-030[2], IMP-031[0], IMP-031[1]
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
    const dialog = page.getByRole('dialog', { name: 'Review method suggestions' })
    await expect(dialog.getByText(`Adding to ${testApi.prefix}-Suggest Target`)).toBeVisible()
    await expect(dialog.getByRole('button', { name: /Import as New/i })).toHaveCount(0)
    await expect(dialog.getByPlaceholder(/Search for a contact/i)).toHaveCount(0)

    // Confirming requires at least one selected method: deselect the sole
    // method and Confirm disables; re-select and it enables again.
    const confirmButton = dialog.getByRole('button', { name: 'Confirm' })
    await dialog.getByRole('button', { name: 'Deselect method' }).click()
    await expect(confirmButton).toBeDisabled()
    await dialog.getByRole('button', { name: 'Select method' }).click()
    await expect(confirmButton).toBeEnabled()

    // Confirm adds the method; the queue surface refreshes through query
    // invalidation (a fresh suggestions GET fires without a reload) and the
    // card disappears from the list.
    const resolvePost = page.waitForResponse(
      res => res.request().method() === 'POST' && res.url().includes('/methods/resolve')
    )
    const invalidationRefetch = page.waitForResponse(
      res => res.request().method() === 'GET' && res.url().includes('/api/v1/imports/suggestions')
    )
    await confirmButton.click()
    expect((await resolvePost).ok()).toBe(true)
    await invalidationRefetch
    await expect(page.getByText(cardText)).toHaveCount(0, {
      timeout: 10000,
    })
  })

  // spec: IMP-030[2], IMP-031[0]
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

  // spec: IMP-029[2], IMP-027[1]
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

    // The modal refetches ALL candidates and navigates by index, so steer it
    // to THIS worker's link-only candidate before asserting on its controls.
    await navigateModalToCandidate(page, cardName)

    // The modal is locked to link mode — the Import-mode toggle is absent.
    await expect(page.getByRole('button', { name: /Import as New/i })).toHaveCount(0)
    await expect(page.getByRole('button', { name: /Link to Existing/i })).toHaveCount(0)
  })

  // spec: IMP-013[0], IMP-027[3], IMP-031[0]
  test('deselect-all link removes the candidate', async ({ page }) => {
    // Seed a CRM contact + a same-named candidate so the modal opens with the
    // contact auto-selected (suggested match). Use icloud_contacts (not
    // gcontacts) so this candidate does not pollute gcontacts-scoped tests
    // under parallel runs.
    await testApi.seedContacts([{ full_name: 'Deselect Target' }])
    await testApi.seedExternalContacts([
      {
        display_name: 'Deselect Target',
        source: 'icloud_contacts',
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
    const dialog = page.getByRole('dialog', { name: 'Resolve import candidate' })
    const search = dialog.getByPlaceholder('Search for a contact...')
    if (await search.isVisible().catch(() => false)) {
      await search.fill('Deselect Target')
      await page.getByText(cardName, { exact: true }).last().click()
    }
    // The Link button is enabled only once a contact is selected.
    await expect(page.getByRole('button', { name: /Link Contact/i })).toBeEnabled()

    // Methods are individually selectable: deselect every method.
    let remaining = await dialog.getByRole('button', { name: 'Deselect method' }).count()
    expect(remaining).toBeGreaterThan(0)
    while (remaining > 0) {
      // Re-query each iteration since toggling relabels the button.
      await dialog.getByRole('button', { name: 'Deselect method' }).first().click()
      remaining = await dialog.getByRole('button', { name: 'Deselect method' }).count()
    }
    await expect(dialog.locator('button[aria-label="Deselect method"]')).toHaveCount(0)

    // Link with no methods selected. The frontend sends methods_curated true
    // (the modal offered method curation) with an empty selected_methods —
    // the curation signal that classifies the link as imported. Capture the
    // request and assert the link succeeds (200).
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
