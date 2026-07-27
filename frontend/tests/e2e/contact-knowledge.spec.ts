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

async function createContact(
  request: APIRequestContext,
  data: { full_name: string; location?: string; birthday?: string }
): Promise<string> {
  const response = await request.post(`${API_BASE_URL}/api/v1/contacts`, {
    headers: API_HEADERS,
    data,
  })
  expect(response.ok()).toBe(true)
  return ((await response.json()).data as { id: string }).id
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

    const location = `${testApi.prefix}-Lisbon`
    const locatedId = await createContact(request, {
      full_name: `${testApi.prefix}-Located Contact`,
      location,
    })
    const unlocatedId = await createContact(request, {
      full_name: `${testApi.prefix}-Unlocated Contact`,
    })

    // Known location: a labeled row carrying the seeded value
    // (30s: this can be the suite's first page load, paying next dev's
    // on-demand route compilation on top of the data fetch)
    // spec: KNW-034.known-location-appears-labeled
    await page.goto(`/contacts/${locatedId}`)
    await expect(
      page.getByRole('heading', { name: `${testApi.prefix}-Located Contact` })
    ).toBeVisible({ timeout: 30000 })
    await expect(detailRow(page, 'Location')).toBeVisible()
    await expect(detailRow(page, 'Location')).toContainText(location)

    // Unknown location: no row renders at all
    // spec: KNW-034.known-location-appears-labeled
    await page.goto(`/contacts/${unlocatedId}`)
    await expect(
      page.getByRole('heading', { name: `${testApi.prefix}-Unlocated Contact` })
    ).toBeVisible({ timeout: 15000 })
    await expect(detailRow(page, 'Location')).toHaveCount(0)
  })

  test('shows a birthday row only when the birthday is known', async ({ page, request }) => {
    const birthday = '1990-04-12'
    // Mirror the component's rendering exactly: parseDateOnly builds a LOCAL
    // date from the Y-M-D parts (no timezone conversion), and formatBirthday
    // formats it with year/month/day options in the (pinned en-US) locale.
    const [year, month, day] = birthday.split('-').map(Number)
    const expectedBirthday = new Date(year, month - 1, day).toLocaleDateString('en-US', {
      year: 'numeric',
      month: 'short',
      day: 'numeric',
    })

    const birthdayId = await createContact(request, {
      full_name: `${testApi.prefix}-Birthday Contact`,
      birthday,
    })
    const plainId = await createContact(request, {
      full_name: `${testApi.prefix}-No Birthday Contact`,
    })

    // Known birthday: a labeled row with the human-readable date
    // spec: KNW-034.known-birthday-appears-labeled
    await page.goto(`/contacts/${birthdayId}`)
    await expect(
      page.getByRole('heading', { name: `${testApi.prefix}-Birthday Contact` })
    ).toBeVisible({ timeout: 15000 })
    await expect(detailRow(page, 'Birthday')).toBeVisible()
    await expect(detailRow(page, 'Birthday')).toContainText(expectedBirthday)

    // Unknown birthday: no row renders
    // spec: KNW-034.known-birthday-appears-labeled
    await page.goto(`/contacts/${plainId}`)
    await expect(
      page.getByRole('heading', { name: `${testApi.prefix}-No Birthday Contact` })
    ).toBeVisible({ timeout: 15000 })
    await expect(detailRow(page, 'Birthday')).toHaveCount(0)
  })
})
