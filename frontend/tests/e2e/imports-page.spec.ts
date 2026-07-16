import { test, expect } from './fixtures'

test.describe('Imports Page @area:imports', () => {
  // spec: IMP-026[0]
  test('source filters expose selection state and scope the candidate request', async ({
    page,
  }) => {
    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')

    // The source filter pills exist.
    const allSources = page.getByRole('button', { name: 'All Sources', exact: true })
    const googleContacts = page.getByRole('button', { name: 'Google Contacts', exact: true })
    const calendar = page.getByRole('button', { name: 'Calendar', exact: true })
    await expect(allSources).toBeVisible()
    await expect(googleContacts).toBeVisible()
    await expect(calendar).toBeVisible()

    // All Sources is the default selection.
    await expect(allSources).toHaveAttribute('aria-pressed', 'true')

    // Selecting a source flips the pressed state AND refetches the
    // suggestions feed scoped to that source (network-param proof).
    const gcontactsResponse = page.waitForResponse(
      res =>
        res.request().method() === 'GET' &&
        res.url().includes('/api/v1/imports/suggestions') &&
        res.url().includes('source=gcontacts')
    )
    await googleContacts.click()
    await gcontactsResponse
    await expect(googleContacts).toHaveAttribute('aria-pressed', 'true')
    await expect(allSources).toHaveAttribute('aria-pressed', 'false')

    const gcalResponse = page.waitForResponse(
      res =>
        res.request().method() === 'GET' &&
        res.url().includes('/api/v1/imports/suggestions') &&
        res.url().includes('source=gcal_attendee')
    )
    await calendar.click()
    await gcalResponse
    await expect(calendar).toHaveAttribute('aria-pressed', 'true')
    await expect(googleContacts).toHaveAttribute('aria-pressed', 'false')
  })

  // spec: IMP-026[2]
  test('manual sync triggers fire per-source sync requests', async ({ page }) => {
    // The sync buttons act on connected Google accounts — an external-provider
    // dependency — so mock the account list (sanctioned route-mock technique)
    // and absorb the trigger POST itself; the deterministic claim is that the
    // page offers both triggers and each fires the right per-source request.
    await page.route('**/api/v1/auth/google/accounts', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: [
            {
              id: 'e2e-mock-google-account',
              account_id: 'e2e-mock-google-account',
              created_at: new Date().toISOString(),
              updated_at: new Date().toISOString(),
            },
          ],
        }),
      })
    )
    const syncRequests: string[] = []
    await page.route('**/api/v1/sync/*/trigger', route => {
      syncRequests.push(route.request().url())
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ success: true, data: null }),
      })
    })

    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')

    // Both per-source triggers are offered. (.first(): an empty candidate
    // list renders a second contacts-sync affordance in the empty state.)
    const syncContacts = page.getByRole('button', { name: /Sync Contacts/i }).first()
    const syncCalendar = page.getByRole('button', { name: /Sync Calendar/i }).first()
    await expect(syncContacts).toBeVisible()
    await expect(syncCalendar).toBeVisible()

    await syncContacts.click()
    await expect
      .poll(() => syncRequests.some(u => u.includes('/sync/gcontacts/trigger')))
      .toBe(true)

    await syncCalendar.click()
    await expect.poll(() => syncRequests.some(u => u.includes('/sync/gcal/trigger'))).toBe(true)
  })
})
