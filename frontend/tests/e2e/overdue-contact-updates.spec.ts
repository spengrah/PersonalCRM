import { test, expect, APIRequestContext } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'
import { getTodayUTC, getTodayUTCShort } from './helpers/date-utils'

/**
 * Comprehensive E2E tests for overdue contact updates.
 *
 * Test Matrix:
 * - Actions: Mark as Contacted (Dashboard, Contact Detail, Contacts List), Complete Reminder
 * - Views: Dashboard (list & count), Contact Detail (last_contacted), Contacts List (column)
 *
 * Each action should:
 * 1. Update last_contacted timestamp with full precision (not just date)
 * 2. Remove contact from overdue list immediately
 * 3. Reflect changes in all UI views after navigation
 *
 * Note: In testing mode (CRM_ENV=testing), weekly cadence = 2 minutes.
 * Contacts become overdue 2 minutes after last_contacted.
 */

// API configuration for E2E tests
const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

// Helper to verify contact is not in overdue list via API
async function isContactOverdue(request: APIRequestContext, contactId: string): Promise<boolean> {
  const response = await request.get(`${API_BASE_URL}/api/v1/contacts/overdue`, {
    headers: API_HEADERS,
  })
  const data = await response.json()
  return data.data.some((c: { id: string }) => c.id === contactId)
}

// Helper to get contact's last_contacted via API
async function getLastContacted(request: APIRequestContext, contactId: string): Promise<string> {
  const response = await request.get(`${API_BASE_URL}/api/v1/contacts/${contactId}`, {
    headers: API_HEADERS,
  })
  const data = await response.json()
  return data.data.last_contacted
}

test.describe('Overdue Contact Updates - With Seeded Data @area:overdue', () => {
  let testApi: TestAPI
  let contactId: string

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)

    // Seed an overdue contact for testing
    const result = await testApi.seedOverdueContacts([
      {
        full_name: 'Overdue Test Contact',
        cadence: 'weekly',
        days_overdue: 5, // 5 days overdue
        email: 'overdue-test@example.com',
      },
    ])

    contactId = result.ids[0]
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should remove contact from dashboard when marked as contacted', async ({
    page,
    request,
  }) => {
    const contactName = `${testApi.prefix}-Overdue Test Contact`
    const beforeMark = new Date()

    // Go to dashboard
    await page.goto('/dashboard')
    await expect(page.getByRole('heading', { name: 'Action Required', level: 2 })).toBeVisible()

    // Verify contact appears in overdue list
    await expect(page.getByRole('heading', { name: contactName })).toBeVisible()

    // Click "Mark as Contacted" for our contact
    const contactCard = page.locator('div.rounded-lg').filter({ hasText: contactName })
    await contactCard.getByRole('button', { name: /Mark as Contacted/i }).click()

    const afterMark = new Date()

    // Contact should disappear from dashboard
    await expect(page.getByRole('heading', { name: contactName })).not.toBeVisible({
      timeout: 5000,
    })

    // Verify via API that contact is no longer overdue
    const stillOverdue = await isContactOverdue(request, contactId)
    expect(stillOverdue).toBe(false)

    // Verify last_contacted has full timestamp precision
    const lastContacted = await getLastContacted(request, contactId)
    const lastContactedDate = new Date(lastContacted)

    // Should be between before and after the click
    expect(lastContactedDate.getTime()).toBeGreaterThanOrEqual(beforeMark.getTime() - 1000)
    expect(lastContactedDate.getTime()).toBeLessThanOrEqual(afterMark.getTime() + 1000)

    // Should NOT be midnight (the old bug)
    expect(lastContacted).not.toMatch(/T00:00:00(\.0+)?Z$/)
  })

  test('should reflect in Contact Detail page after navigation', async ({ page }) => {
    const contactName = `${testApi.prefix}-Overdue Test Contact`

    await page.goto('/dashboard')
    await expect(page.getByRole('heading', { name: 'Action Required', level: 2 })).toBeVisible()

    // Mark as contacted from dashboard
    const contactCard = page.locator('div.rounded-lg').filter({ hasText: contactName })
    await contactCard.getByRole('button', { name: /Mark as Contacted/i }).click()
    await expect(page.getByRole('heading', { name: contactName })).not.toBeVisible({
      timeout: 5000,
    })

    // Navigate to contact detail page
    await page.goto(`/contacts/${contactId}`)
    await expect(page.getByRole('heading', { name: contactName, level: 2 })).toBeVisible()

    // Last contacted should show today's date (use first() as date may appear multiple times)
    const today = getTodayUTC()
    await expect(page.getByText(today).first()).toBeVisible()
  })

  test('should update last_contacted and remove from dashboard when marked from Contact Detail', async ({
    page,
    request,
  }) => {
    const contactName = `${testApi.prefix}-Overdue Test Contact`

    // Verify contact is initially overdue on dashboard
    await page.goto('/dashboard')
    await expect(page.getByRole('heading', { name: 'Action Required', level: 2 })).toBeVisible()
    await expect(page.getByRole('heading', { name: contactName })).toBeVisible()

    // Go to contact detail and mark as contacted
    await page.goto(`/contacts/${contactId}`)
    await expect(page.getByRole('heading', { name: contactName, level: 2 })).toBeVisible()

    // Wait for the API call to complete when clicking Mark as Contacted
    const responsePromise = page.waitForResponse(
      resp => resp.url().includes('/last-contacted') && resp.request().method() === 'PATCH'
    )
    await page.getByRole('button', { name: /Mark as Contacted/i }).click()
    await responsePromise

    // Last contacted should update to today (use first() as date may appear multiple times)
    const today = getTodayUTC()
    await expect(page.getByText(today).first()).toBeVisible()

    // Verify via API
    const stillOverdue = await isContactOverdue(request, contactId)
    expect(stillOverdue).toBe(false)
  })

  test('all views should show consistent state after marking as contacted', async ({
    page,
    request,
  }) => {
    const contactName = `${testApi.prefix}-Overdue Test Contact`

    // Mark as contacted via API to avoid extra UI setup
    const lastContactedResponse = await request.patch(
      `${API_BASE_URL}/api/v1/contacts/${contactId}/last-contacted`,
      { headers: API_HEADERS }
    )
    expect(lastContactedResponse.ok()).toBeTruthy()

    // 1. Contact Detail: should show today's date (use first() as date may appear multiple times)
    await page.goto(`/contacts/${contactId}`)
    await expect(page.getByRole('heading', { name: contactName })).toBeVisible()
    const today = getTodayUTC()
    await expect(page.getByText(today).first()).toBeVisible()

    // 2. Contacts List: should show today's date in the Next Contact column
    // (weekly cadence + just-marked = contact_by is today). The Last response
    // column may render "Never" because PATCH /last-contacted does not bump
    // last_response_at. Use first() to match any matching cell in the row.
    await page.goto('/contacts')
    await expect(page.getByRole('heading', { name: 'Contacts', level: 2 })).toBeVisible()
    const contactRow = page.locator('tr').filter({ hasText: contactName })
    const todayShort = getTodayUTCShort()
    await expect(contactRow.getByText(todayShort).first()).toBeVisible()

    // 3. API: should confirm not overdue
    const stillOverdue = await isContactOverdue(request, contactId)
    expect(stillOverdue).toBe(false)
  })

  test('last_contacted should have full timestamp precision, not just date', async ({
    page,
    request,
  }) => {
    const contactName = `${testApi.prefix}-Overdue Test Contact`

    await page.goto('/dashboard')
    await expect(page.getByRole('heading', { name: 'Action Required', level: 2 })).toBeVisible()

    const beforeMark = new Date()

    const contactCard = page.locator('div.rounded-lg').filter({ hasText: contactName })
    await contactCard.getByRole('button', { name: /Mark as Contacted/i }).click()
    await expect(page.getByRole('heading', { name: contactName })).not.toBeVisible({
      timeout: 5000,
    })

    const afterMark = new Date()

    // Get the updated last_contacted via API
    const lastContacted = await getLastContacted(request, contactId)
    const lastContactedDate = new Date(lastContacted)

    // Verify timestamp is within expected range
    expect(lastContactedDate.getTime()).toBeGreaterThanOrEqual(beforeMark.getTime() - 1000)
    expect(lastContactedDate.getTime()).toBeLessThanOrEqual(afterMark.getTime() + 1000)

    // The old bug would set time to midnight - verify this is NOT the case
    // A timestamp at exactly midnight would match this pattern
    expect(lastContacted).not.toMatch(/T00:00:00(\.0+)?Z$/)

    // Verify the timestamp includes sub-second precision (our fix stores full timestamp)
    expect(lastContacted).toMatch(/T\d{2}:\d{2}:\d{2}\.\d+Z$/)
  })
})

test.describe('Overdue Contact Updates - Multiple Contacts @area:overdue', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)

    // Seed multiple overdue contacts
    await testApi.seedOverdueContacts([
      {
        full_name: 'First Overdue',
        cadence: 'weekly',
        days_overdue: 3,
      },
      {
        full_name: 'Second Overdue',
        cadence: 'monthly',
        days_overdue: 10,
      },
    ])
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should show multiple overdue contacts on dashboard', async ({ page }) => {
    await page.goto('/dashboard')
    await page.waitForLoadState('domcontentloaded')

    // Both contacts should be visible
    await expect(
      page.getByRole('heading', { name: `${testApi.prefix}-First Overdue` })
    ).toBeVisible()
    await expect(
      page.getByRole('heading', { name: `${testApi.prefix}-Second Overdue` })
    ).toBeVisible()

    // Status should show correct count
    await expect(page.getByText(/contacts need your attention/)).toBeVisible()
  })
})
