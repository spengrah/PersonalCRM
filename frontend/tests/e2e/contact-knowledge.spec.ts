import { test, expect, type Page } from '@playwright/test'
import type { APIRequestContext } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

// API configuration for E2E tests
const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

// A labeled row in the contact-detail definition list, located by its label.
const detailRow = (page: Page, label: string) =>
  page.locator('dl > div').filter({ has: page.getByText(label, { exact: true }) })

// The knowledge values the declared fixture stores are not derivable from the
// manifest: a declared location is namespace-PREFIXED (the auto-created place
// node's label has to carry the prefix for teardown to find it) and a declared
// birthday resolves an undisclosed leap-safe birth year. Both are read back from
// the detail API and the RENDERED string is asserted against what was stored.
async function readKnowledge(
  request: APIRequestContext,
  contactId: string
): Promise<{ location?: string; birthday?: string }> {
  const response = await request.get(`${API_BASE_URL}/api/v1/contacts/${contactId}`, {
    headers: API_HEADERS,
  })
  expect(response.ok()).toBe(true)
  return (await response.json()).data as { location?: string; birthday?: string }
}

test.describe('Contact knowledge rows @area:contacts', () => {
  // The birthday row renders via formatBirthday → toLocaleDateString with the
  // browser's default locale — pin it so the expectation computed below (with
  // an explicit 'en-US') matches the browser's rendering.
  test.use({ locale: 'en-US' })

  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('shows a location row only when the location is known', async ({ page, request }) => {
    // Often the suite's first page load: next dev's on-demand compilation of
    // the contact-detail route can eat most of the default 30s test budget
    // before the app even renders — triple it.
    test.slow()

    const seeded = await testApi.seedBehavior('KNW-034')
    const located = seeded.entities['located']
    const unlocated = seeded.entities['unlocated']
    const { location } = await readKnowledge(request, located.id)
    expect(location).toBeTruthy()

    // Known location: a labeled row carrying the seeded value
    // (30s: this can be the suite's first page load, paying next dev's
    // on-demand route compilation on top of the data fetch)
    // spec: KNW-034.known-location-appears-labeled
    await page.goto(`/contacts/${located.id}`)
    await expect(page.getByRole('heading', { name: located.name })).toBeVisible({ timeout: 30000 })
    await expect(detailRow(page, 'Location')).toBeVisible()
    await expect(detailRow(page, 'Location')).toContainText(location as string)

    // Unknown location: no row renders at all
    // spec: KNW-034.known-location-appears-labeled
    await page.goto(`/contacts/${unlocated.id}`)
    await expect(page.getByRole('heading', { name: unlocated.name })).toBeVisible({
      timeout: 15000,
    })
    await expect(detailRow(page, 'Location')).toHaveCount(0)
  })

  test('shows a birthday row only when the birthday is known', async ({ page, request }) => {
    const seeded = await testApi.seedBehavior('KNW-034')
    const withBirthday = seeded.entities['birthday']
    const plain = seeded.entities['plain']

    // The declaration pins the month and day (April 12) but resolves the YEAR
    // itself, so the stored date is read back rather than restated.
    const { birthday } = await readKnowledge(request, withBirthday.id)
    expect(birthday).toBeTruthy()
    // Mirror the component's rendering exactly: parseDateOnly builds a LOCAL
    // date from the Y-M-D parts (no timezone conversion), and formatBirthday
    // formats it with year/month/day options in the (pinned en-US) locale.
    const [year, month, day] = (birthday as string).split('T')[0].split('-').map(Number)
    const expectedBirthday = new Date(year, month - 1, day).toLocaleDateString('en-US', {
      year: 'numeric',
      month: 'short',
      day: 'numeric',
    })

    // Known birthday: a labeled row with the human-readable date
    // spec: KNW-034.known-birthday-appears-labeled
    await page.goto(`/contacts/${withBirthday.id}`)
    await expect(page.getByRole('heading', { name: withBirthday.name })).toBeVisible({
      timeout: 15000,
    })
    await expect(detailRow(page, 'Birthday')).toBeVisible()
    await expect(detailRow(page, 'Birthday')).toContainText(expectedBirthday)

    // Unknown birthday: no row renders
    // spec: KNW-034.known-birthday-appears-labeled
    await page.goto(`/contacts/${plain.id}`)
    await expect(page.getByRole('heading', { name: plain.name })).toBeVisible({ timeout: 15000 })
    await expect(detailRow(page, 'Birthday')).toHaveCount(0)
  })
})
