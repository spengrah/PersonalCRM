import { test, expect } from '@playwright/test'
import type { APIRequestContext } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
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
})
