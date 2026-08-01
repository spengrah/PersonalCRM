import { test, expect } from './fixtures'
import { createTestAPI, TestAPI } from './helpers/test-api'
import { findCandidateByName } from './helpers/imports-helpers'

test.describe('Imports Page @area:imports', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  // spec: IMP-026.people-tab-default-holds
  test('renders candidates confidence-ranked on the People tab', async ({ page }) => {
    // IMP-029's declared fixture: two CRM contacts, where one candidate matches
    // its contact on name AND email (high confidence) and the other matches on
    // name only (lower confidence). The declaration lists the low-confidence
    // candidate FIRST — entity order is seed order — so a seed-order artifact
    // cannot pass this.
    const seeded = await testApi.seedBehavior('IMP-029')
    const highName = seeded.entities['matching'].name
    const medName = seeded.entities['name-collide'].name

    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')

    // Scope the list to the ranked pair's own source to keep the page small,
    // then find both of our rows. Google Contacts rather than Calendar: an
    // unmatched Calendar candidate cannot hold a contact's email (the sync
    // matches it, and the calendar rematch handler flips any stored row), so the
    // high-confidence half of this ladder is only producible on the address book.
    await page.getByRole('button', { name: 'Google Contacts', exact: true }).click()
    await findCandidateByName(page, highName)
    await expect(page.getByRole('heading', { name: medName })).toBeVisible({ timeout: 10000 })

    // Relative render order among OUR rows: higher confidence first.
    const headings = await page.getByRole('heading', { level: 3 }).allTextContents()
    const highIdx = headings.findIndex(t => t.includes(highName))
    const medIdx = headings.findIndex(t => t.includes(medName))
    expect(highIdx).toBeGreaterThanOrEqual(0)
    expect(medIdx).toBeGreaterThanOrEqual(0)
    expect(highIdx).toBeLessThan(medIdx)
  })

  // spec: IMP-026.people-tab-default-holds
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

  // spec: IMP-026.manual-sync-triggers-offered
  test('manual sync triggers fire per-source sync requests', async ({ page }) => {
    // The sync buttons act on connected Google accounts — an external-provider
    // dependency — so mock the account list (sanctioned route-mock technique)
    // and absorb the trigger POST itself; the deterministic claim is that the
    // page offers both triggers and each fires the right per-source request.
    // The app calls the API cross-origin (frontend :3000 → API :8080) with an
    // X-API-Key header, so fulfilled responses must answer the CORS preflight
    // and carry Access-Control-Allow-Origin, else the browser drops them.
    const corsHeaders = {
      'Access-Control-Allow-Origin': '*',
      'Access-Control-Allow-Methods': 'GET,POST,OPTIONS',
      'Access-Control-Allow-Headers': 'Content-Type,X-API-Key',
    }
    const corsFulfill = (route: import('@playwright/test').Route, body: unknown) => {
      if (route.request().method() === 'OPTIONS') {
        return route.fulfill({ status: 204, headers: corsHeaders })
      }
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        headers: { ...corsHeaders, 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      })
    }
    await page.route('**/api/v1/auth/google/accounts', route =>
      corsFulfill(route, {
        success: true,
        data: [
          {
            id: 'e2e-mock-google-account',
            account_id: 'e2e-mock-google-account',
            created_at: new Date().toISOString(),
            updated_at: new Date().toISOString(),
          },
        ],
      })
    )
    const syncRequests: string[] = []
    await page.route('**/api/v1/sync/*/trigger', route => {
      if (route.request().method() !== 'OPTIONS') {
        syncRequests.push(route.request().url())
      }
      return corsFulfill(route, { success: true, data: null })
    })

    // The triggers act only once the account list has loaded — wait for it
    // before clicking, else the click lands on the no-accounts branch.
    const accountsLoaded = page.waitForResponse(
      res => res.url().includes('/api/v1/auth/google/accounts') && res.request().method() === 'GET'
    )
    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')
    await accountsLoaded

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
