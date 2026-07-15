import { test, expect } from '@playwright/test'
import type { APIRequestContext, Page } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

// Freeze the client-side accelerated-time frame to a fixed instant by mocking
// GET /system/time BEFORE the page loads. `acceleration_factor: 0` pins
// currentTime at base_time (a non-zero factor collapses to the wall clock), and
// the body is the full apiClient envelope (apiClient unwraps `data`, so a bare
// inner object would leave the frame undefined). Per-page + pre-navigation, so
// it is parallel-safe and does not touch the process-wide acceleration state.
// NOTE: page.route does NOT intercept the test's own request.get('/system/time'),
// so birthdays seeded for these tests use fixed dates chosen against the mocked
// frame, not the real frame.
async function mockFrozenSystemTime(page: Page, isoInstant: string): Promise<void> {
  await page.route('**/api/v1/system/time', async route => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        success: true,
        data: {
          current_time: isoInstant,
          base_time: isoInstant,
          is_accelerated: true,
          acceleration_factor: 0,
          environment: 'testing',
        },
      }),
    })
  })
}

async function getCurrentBirthdayDate(request: APIRequestContext): Promise<string> {
  const response = await request.get(`${API_BASE_URL}/api/v1/system/time`, {
    headers: API_HEADERS,
  })
  expect(response.ok()).toBeTruthy()

  const body = await response.json()
  const systemTime = body.data as { current_time: string; is_accelerated: boolean }
  const currentTime = systemTime.is_accelerated ? new Date(systemTime.current_time) : new Date()
  const month = String(currentTime.getMonth() + 1).padStart(2, '0')
  const day = String(currentTime.getDate()).padStart(2, '0')

  return `1900-${month}-${day}`
}

async function createContactWithBirthday(
  request: APIRequestContext,
  testApi: TestAPI,
  input: { fullName: string; birthday: string }
): Promise<string> {
  const response = await request.post(`${API_BASE_URL}/api/v1/contacts`, {
    headers: API_HEADERS,
    data: {
      full_name: `${testApi.prefix}-${input.fullName}`,
      birthday: input.birthday,
    },
  })
  expect(response.ok()).toBeTruthy()

  const body = await response.json()
  return (body.data as { id: string }).id
}

test.describe('Birthdays - Placeholder Years @area:birthdays', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('shows placeholder-year birthdays without age and keeps today at the top', async ({
    page,
    request,
  }) => {
    // spec: CON-045[3]
    const birthday = await getCurrentBirthdayDate(request)
    const birthdayDate = new Date(`${birthday}T12:00:00`)
    const expectedListDate = `${birthdayDate.getMonth() + 1}/${birthdayDate.getDate()}`
    const expectedDetailDate = birthdayDate.toLocaleDateString('en-US', {
      month: 'short',
      day: 'numeric',
    })
    const expectedBirthdayPageDate = birthdayDate.toLocaleDateString('en-US', {
      month: 'long',
      day: 'numeric',
    })

    const contactId = await createContactWithBirthday(request, testApi, {
      fullName: 'Placeholder Birthday Contact',
      birthday,
    })
    const fullName = `${testApi.prefix}-Placeholder Birthday Contact`

    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')
    await page.getByPlaceholder('Search contacts...').fill(fullName)
    await page.getByPlaceholder('Search contacts...').press('Enter')
    const row = page.locator('tr', { has: page.getByText(fullName) })
    await expect(row).toBeVisible({ timeout: 15000 })
    await expect(row).toContainText(expectedListDate)
    await expect(row).not.toContainText('/00')
    await expect(row).not.toContainText('1900')

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })
    await expect(page.getByText(expectedDetailDate, { exact: true })).toBeVisible()
    await expect(page.getByText('1900')).not.toBeVisible()

    await page.goto('/birthdays')
    await page.waitForLoadState('domcontentloaded')
    const todaySection = page.locator('section', {
      has: page.getByRole('heading', { name: /Today's Birthdays/ }),
    })
    await expect(todaySection).toBeVisible({ timeout: 15000 })

    const card = todaySection.getByTestId('birthday-card').filter({ hasText: fullName })
    await expect(card).toBeVisible()
    await expect(card).toContainText(expectedBirthdayPageDate)
    await expect(card).toContainText('Today!')
    await expect(card).not.toContainText(/Turning|Turned/)
  })

  test('groups birthdays into today, upcoming, and already-celebrated', async ({
    page,
    request,
  }) => {
    // spec: CON-045[0]
    // Freeze the frame mid-year so the three groups are deterministic and
    // parallel-safe (the same per-page frame-mock idiom used for CON-045[1]).
    await mockFrozenSystemTime(page, '2026-06-15T12:00:00Z')
    const todayName = `${testApi.prefix}-Group Today`
    const upcomingName = `${testApi.prefix}-Group Upcoming`
    const celebratedName = `${testApi.prefix}-Group Celebrated`
    await createContactWithBirthday(request, testApi, {
      fullName: 'Group Today',
      birthday: '1990-06-15',
    })
    await createContactWithBirthday(request, testApi, {
      fullName: 'Group Upcoming',
      birthday: '1990-06-20',
    })
    await createContactWithBirthday(request, testApi, {
      fullName: 'Group Celebrated',
      birthday: '1990-03-10',
    })

    await page.goto('/birthdays')
    await page.waitForLoadState('domcontentloaded')

    const todaySection = page.locator('section', {
      has: page.getByRole('heading', { name: /Today's Birthdays/ }),
    })
    const upcomingSection = page.locator('section', {
      has: page.getByRole('heading', { name: /Upcoming Birthdays/ }),
    })
    const celebratedSection = page.locator('section', {
      has: page.getByRole('heading', { name: /Already Celebrated This Year/ }),
    })

    await expect(todaySection).toBeVisible({ timeout: 15000 })
    await expect(upcomingSection).toBeVisible()
    await expect(celebratedSection).toBeVisible()

    // Each seeded contact lands in the correct group.
    await expect(todaySection.getByText(todayName)).toBeVisible()
    await expect(upcomingSection.getByText(upcomingName)).toBeVisible()
    await expect(celebratedSection.getByText(celebratedName)).toBeVisible()
  })

  test('sorts upcoming birthdays soonest-first and sinks celebrated to the end', async ({
    page,
    request,
  }) => {
    // spec: CON-045[2]
    await mockFrozenSystemTime(page, '2026-06-15T12:00:00Z')
    const soonName = `${testApi.prefix}-Sort Soon` // 3 days out
    const laterName = `${testApi.prefix}-Sort Later` // 10 days out
    const celebratedName = `${testApi.prefix}-Sort Celebrated`
    await createContactWithBirthday(request, testApi, {
      fullName: 'Sort Soon',
      birthday: '1990-06-18',
    })
    await createContactWithBirthday(request, testApi, {
      fullName: 'Sort Later',
      birthday: '1990-06-25',
    })
    await createContactWithBirthday(request, testApi, {
      fullName: 'Sort Celebrated',
      birthday: '1990-03-10',
    })

    await page.goto('/birthdays')
    await page.waitForLoadState('domcontentloaded')

    const upcomingSection = page.locator('section', {
      has: page.getByRole('heading', { name: /Upcoming Birthdays/ }),
    })
    const celebratedSection = page.locator('section', {
      has: page.getByRole('heading', { name: /Already Celebrated This Year/ }),
    })
    await expect(upcomingSection).toBeVisible({ timeout: 15000 })
    await expect(celebratedSection).toBeVisible()

    // Within upcoming, the sooner birthday (3 days) precedes the later (10 days)
    // in DOM order — prefix-scoped card names, not viewport coordinates.
    await expect(upcomingSection.getByText(soonName)).toBeVisible()
    await expect(upcomingSection.getByText(laterName)).toBeVisible()
    const upcomingCards = await upcomingSection.getByTestId('birthday-card').allTextContents()
    const soonIdx = upcomingCards.findIndex(t => t.includes(soonName))
    const laterIdx = upcomingCards.findIndex(t => t.includes(laterName))
    expect(soonIdx).toBeGreaterThanOrEqual(0)
    expect(laterIdx).toBeGreaterThan(soonIdx)

    // The seeded celebrated contact sits in the celebrated section, which renders
    // AFTER the upcoming section (section headings compared in DOM order).
    await expect(celebratedSection.getByText(celebratedName)).toBeVisible()
    const headings = await page.getByRole('heading', { level: 2 }).allTextContents()
    const upcomingHeadingIdx = headings.findIndex(h => /Upcoming Birthdays/.test(h))
    const celebratedHeadingIdx = headings.findIndex(h => /Already Celebrated This Year/.test(h))
    expect(upcomingHeadingIdx).toBeGreaterThanOrEqual(0)
    expect(celebratedHeadingIdx).toBeGreaterThan(upcomingHeadingIdx)
  })

  test('the birthdays page date header follows the server accelerated frame', async ({ page }) => {
    // spec: CON-045[4]
    // Freeze the frame to a fixed, non-wall-clock date and assert the page
    // header renders THAT date — proving it follows the server frame rather than
    // the wall clock (a real-frame assertion would pass trivially when the
    // backend reports is_accelerated=false and the frame equals the wall clock).
    await mockFrozenSystemTime(page, '2026-09-03T12:00:00Z')
    await page.goto('/birthdays')
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: 'Birthday Tracker' })).toBeVisible({
      timeout: 15000,
    })
    await expect(page.getByText(/September 3, 2026/)).toBeVisible()
  })

  test('shows the gift-planning section near year end', async ({ page, request }) => {
    // spec: CON-045[1]
    // December frame → the page surfaces early-next-year (Jan-Mar) birthdays.
    await mockFrozenSystemTime(page, '2026-12-15T12:00:00Z')
    const febName = `${testApi.prefix}-Gift Feb`
    await createContactWithBirthday(request, testApi, {
      fullName: 'Gift Feb',
      birthday: '1990-02-14',
    })

    await page.goto('/birthdays')
    await page.waitForLoadState('domcontentloaded')
    // Scope to the seeded contact INSIDE the gift-planning section — another
    // worker's Jan-Mar birthday could otherwise satisfy the bare heading.
    const giftSection = page.locator('section', {
      has: page.getByRole('heading', { name: /Gift Planning/ }),
    })
    await expect(giftSection).toBeVisible({ timeout: 15000 })
    await expect(giftSection.getByText(febName)).toBeVisible()
  })

  test('hides the gift-planning section away from year end', async ({ page, request }) => {
    // spec: CON-045[1]
    // June frame → no gift-planning section, even with a Jan-Mar birthday.
    await mockFrozenSystemTime(page, '2026-06-15T12:00:00Z')
    const febName = `${testApi.prefix}-Gift Feb`
    await createContactWithBirthday(request, testApi, {
      fullName: 'Gift Feb',
      birthday: '1990-02-14',
    })

    await page.goto('/birthdays')
    await page.waitForLoadState('domcontentloaded')
    // Wait for the sections to render (the Feb contact shows as celebrated),
    // then assert the gift-planning section is absent.
    await expect(page.getByText(febName)).toBeVisible({ timeout: 15000 })
    await expect(page.getByRole('heading', { name: /Gift Planning/ })).toHaveCount(0)
  })
})
