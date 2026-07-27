import { test, expect } from './fixtures'
import { createTestAPI, TestAPI } from './helpers/test-api'

// People tab: the Anarlog source filter pill scopes the candidate query.
// anarlog_humans candidates flow through the ordinary candidate queue.
test.describe('Imports People tab — Anarlog source @area:imports', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  // spec: IMP-026.people-tab-default-holds
  test('filters to anarlog_humans candidates via the Anarlog pill', async ({ page }) => {
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
    const anarlogPill = page.getByRole('button', { name: 'Anarlog', exact: true })
    await expect(anarlogPill).toBeVisible()
    await anarlogPill.click()
    await suggestionsResponse
    await expect(anarlogPill).toHaveAttribute('aria-pressed', 'true')

    // The seeded candidate card is visible (display name is prefixed by the
    // seed route).
    await expect(page.getByText(`${testApi.prefix}-Anarlog Person`).first()).toBeVisible({
      timeout: 10000,
    })
  })
})
