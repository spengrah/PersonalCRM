import { test, expect } from './fixtures'
import { createTestAPI, TestAPI } from './helpers/test-api'

// People-tab name candidates: grouped anarlog_title tokens lifted from session
// titles. Each (token, session) pair is one external_contact row; the Imports
// UI groups them by normalized token and resolves the whole group in one call.
test.describe('Imports name candidates (anarlog_title) @area:imports', () => {
  let testApi: TestAPI
  // A per-run unique token so parallel workers don't collide on grouping.
  let token: string
  let display: string

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
    // The display name carries testApi.prefix verbatim so the CRM contact
    // created by the import path is cleaned up by DeleteContactsByNamePrefix
    // in afterEach (the prefix would otherwise be lost on the imported name).
    display = `${testApi.prefix}-Lena`
    token = display.toLowerCase()
    // Two sessions surfacing the same token → evidence_count = 2.
    await testApi.seedExternalContacts([
      {
        source: 'anarlog_title',
        metadata: {
          token_normalized: token,
          token_display: display,
          session_uuid: crypto.randomUUID(),
        },
      },
      {
        source: 'anarlog_title',
        metadata: {
          token_normalized: token,
          token_display: display,
          session_uuid: crypto.randomUUID(),
        },
      },
    ])
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  // spec: IMP-026.low-confidence-names-section
  test('renders the name-candidate section with the grouped token and evidence count', async ({
    page,
  }) => {
    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')

    // The section appears when title-derived tokens exist; the heading is
    // the region's accessible locator.
    await expect(page.getByRole('heading', { name: 'Names found in session titles' })).toBeVisible({
      timeout: 10000,
    })
    await expect(page.getByText(display, { exact: true }).first()).toBeVisible()
    // Evidence count reflects the two seeded (token, session) rows.
    await expect(page.getByText(/Seen in 2 session titles/).first()).toBeVisible()
  })

  // Scope to THIS test's exact name-candidate row by its token test id. A
  // hasText filter matches ancestor containers that wrap OTHER workers' rows
  // too, so `.first()` there could open the modal on a sibling worker's
  // candidate; the test id pins the precise row element.
  const myRow = (page: import('@playwright/test').Page) =>
    page.getByTestId(`name-candidate-row-${token}`)

  // Match only THIS worker's resolve POST (the body carries normalized_token).
  // Without scoping, parallel workers' resolve responses cross-match and a
  // sibling worker's response can be awaited here.
  const myResolve = (page: import('@playwright/test').Page) =>
    page.waitForResponse(res => {
      if (
        res.request().method() !== 'POST' ||
        !res.url().includes('/api/v1/imports/anarlog-title/resolve')
      ) {
        return false
      }
      try {
        return (
          (res.request().postDataJSON() as { normalized_token?: string }).normalized_token === token
        )
      } catch {
        return false
      }
    })

  // spec: IMP-031.item-leaves-queue-counts-update
  test('imports the whole token group as a new contact', async ({ page }) => {
    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByText(display, { exact: true }).first()).toBeVisible({ timeout: 10000 })

    await myRow(page)
      .getByRole('button', { name: /Create contact/ })
      .first()
      .click()

    // The modal opens in the name-only branch: no contact-methods section,
    // just name + cadence + the info note. The pager opens at this token.
    const dialog = page.getByRole('dialog', { name: /Create contact from name candidate/ })
    await expect(dialog.getByText(/No contact methods/)).toBeVisible()
    await expect(dialog.getByLabel('Name')).toHaveValue(display)

    const resolved = myResolve(page)
    await dialog.getByRole('button', { name: 'Create contact', exact: true }).click()
    const res = await resolved
    expect(res.ok()).toBeTruthy()

    // The token group leaves the name-candidate list after resolution.
    await expect(page.getByText(display, { exact: true })).toHaveCount(0, { timeout: 10000 })
  })

  // spec: IMP-031.item-leaves-queue-counts-update
  test('links the token group to an existing contact', async ({ page }) => {
    const seeded = await testApi.seedContacts([{ full_name: 'Link Target Person' }])
    const targetName = `${testApi.prefix}-Link Target Person`
    expect(seeded.created).toBe(1)

    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByText(display, { exact: true }).first()).toBeVisible({ timeout: 10000 })

    await myRow(page)
      .getByRole('button', { name: /Create contact/ })
      .first()
      .click()
    const dialog = page.getByRole('dialog', { name: /Create contact from name candidate/ })
    await expect(dialog.getByText(/No contact methods/)).toBeVisible()

    // Toggle to link mode and pick the seeded contact.
    await dialog.getByRole('button', { name: 'Link to existing' }).click()
    await dialog.getByText('Search contacts...').click()
    await page.getByText(targetName).first().click()

    const resolved = myResolve(page)
    await dialog.getByRole('button', { name: 'Link contact', exact: true }).click()
    const res = await resolved
    expect(res.ok()).toBeTruthy()

    await expect(page.getByText(display, { exact: true })).toHaveCount(0, { timeout: 10000 })
  })

  // spec: IMP-031.item-leaves-queue-counts-update
  test('ignores the token group via "Not a person"', async ({ page }) => {
    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByText(display, { exact: true }).first()).toBeVisible({ timeout: 10000 })

    const resolved = myResolve(page)
    // The row-level "Not a person" ignores the group directly (no modal).
    await myRow(page)
      .getByRole('button', { name: /Not a person/ })
      .first()
      .click()
    const res = await resolved
    expect(res.ok()).toBeTruthy()

    await expect(page.getByText(display, { exact: true })).toHaveCount(0, { timeout: 10000 })
  })
})
