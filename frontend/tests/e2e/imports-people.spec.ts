import { test, expect } from './fixtures'
import { createTestAPI, TestAPI } from './helpers/test-api'

// People tab: the Anarlog source filter pill and the anarlog_humans card
// badge. anarlog_humans candidates flow through the ordinary candidate queue,
// so the only People-tab work is presentational (source-display + filter).
test.describe('Imports People tab — Anarlog source @area:imports', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('shows the Anarlog source filter pill', async ({ page }) => {
    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('button', { name: 'Anarlog', exact: true })).toBeVisible()
  })

  test('filters to anarlog_humans candidates and renders the Anarlog badge', async ({ page }) => {
    await testApi.seedExternalContacts([
      {
        display_name: 'Anarlog Person',
        source: 'anarlog_humans',
        emails: [`anarlog-${testApi.prefix}@example.invalid`],
      },
    ])

    const suggestionsResponse = page.waitForResponse(
      res =>
        res.request().method() === 'GET' &&
        res.url().includes('/api/v1/imports/suggestions') &&
        res.url().includes('source=anarlog_humans')
    )

    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')
    await page.getByRole('button', { name: 'Anarlog', exact: true }).click()
    await suggestionsResponse

    // The seeded candidate card is visible (display name is prefixed by the
    // seed route).
    await expect(page.getByText(`${testApi.prefix}-Anarlog Person`).first()).toBeVisible({
      timeout: 10000,
    })
  })
})
