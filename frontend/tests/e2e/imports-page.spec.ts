import { test, expect } from './fixtures'
import { createTestAPI, TestAPI } from './helpers/test-api'
import { findCandidateByName } from './helpers/imports-helpers'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

test.describe('Imports Page @area:imports', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  // spec: IMP-026.people-tab-default-holds
  test('renders candidates confidence-ranked on the People tab', async ({ page, request }) => {
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

    // Scope the list to the ranked pair's own source. Google Contacts rather than
    // Calendar: an unmatched Calendar candidate cannot hold a contact's email (the
    // sync matches it, and the calendar rematch handler flips any stored row), so
    // the high-confidence half of this ladder is only producible on the address
    // book.
    await page.getByRole('button', { name: 'Google Contacts', exact: true }).click()

    // Both rows really do render on the People tab. findCandidateByName paginates,
    // so neither anchor assumes the two are on the same page.
    await findCandidateByName(page, highName)
    await findCandidateByName(page, medName)

    // The ORDER is compared over the whole ranked list rather than over one
    // rendered page. Google Contacts is the busiest source in the shared E2E
    // database, so sibling workers' high-confidence rows can sit between ours and
    // push one of them onto a later page — which would fail a page-local
    // comparison while the global ranking was perfectly correct.
    const rankedRes = await request.get(
      `${API_BASE_URL}/api/v1/imports/suggestions?source=gcontacts&limit=1000`,
      { headers: API_HEADERS }
    )
    expect(rankedRes.ok()).toBe(true)
    const ranked: Array<{ kind: string; candidate?: { display_name?: string } }> =
      (await rankedRes.json())?.data ?? []
    const rankedNames = ranked
      .filter(item => item.kind === 'contact')
      .map(item => item.candidate?.display_name ?? '')
    const highIdx = rankedNames.indexOf(highName)
    const medIdx = rankedNames.indexOf(medName)

    // Both must have been FOUND before their positions mean anything — an empty
    // or truncated list would otherwise satisfy the comparison below vacuously.
    expect(
      highIdx,
      'the high-confidence candidate must appear in the ranked list'
    ).toBeGreaterThanOrEqual(0)
    expect(
      medIdx,
      'the medium-confidence candidate must appear in the ranked list'
    ).toBeGreaterThanOrEqual(0)
    expect(highIdx).toBeLessThan(medIdx)
  })

  // spec: IMP-026.people-tab-default-holds
  test('source filters expose selection state and scope the candidate request', async ({
    page,
  }) => {
    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')

    // The source filter pills exist.
    const allFilter = page.getByRole('button', { name: 'All', exact: true })
    const googleContacts = page.getByRole('button', { name: 'Google Contacts', exact: true })
    const calendar = page.getByRole('button', { name: 'Calendar', exact: true })
    const whatsApp = page.getByRole('button', { name: 'WhatsApp', exact: true })
    await expect(allFilter).toBeVisible()
    await expect(googleContacts).toBeVisible()
    await expect(calendar).toBeVisible()
    await expect(whatsApp).toBeVisible()

    // All is the default selection.
    await expect(allFilter).toHaveAttribute('aria-pressed', 'true')

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
    await expect(allFilter).toHaveAttribute('aria-pressed', 'false')

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
    // The triggers live in the candidate list's empty state, so force an empty
    // list rather than depending on whatever the shared E2E database holds.
    await page.route('**/api/v1/imports/suggestions*', route =>
      corsFulfill(route, {
        success: true,
        data: [],
        meta: { pagination: { total: 0, page: 1, limit: 20, pages: 0 } },
      })
    )

    // The triggers act only once the account list has loaded — wait for it
    // before clicking, else the click lands on the no-accounts branch.
    const accountsLoaded = page.waitForResponse(
      res => res.url().includes('/api/v1/auth/google/accounts') && res.request().method() === 'GET'
    )
    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')
    await accountsLoaded

    // Both per-source triggers are offered, in the candidate list's empty state.
    const syncContacts = page.getByRole('button', { name: /Sync Contacts/i })
    const syncCalendar = page.getByRole('button', { name: /Sync Calendar/i })
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
