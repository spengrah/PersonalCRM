import { test, expect } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

test.describe('Contact Direction Signals @area:contacts', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('shows direction signal timestamps after mutual interaction', async ({ page, request }) => {
    const { ids } = await testApi.seedContacts([
      { full_name: 'Direction Signal Test', cadence: 'monthly' },
    ])
    const contactId = ids[0]

    // Record a mutual interaction via API
    const pastDate = new Date(Date.now() - 3 * 24 * 60 * 60 * 1000).toISOString()
    await request.post(`${API_BASE_URL}/api/v1/contacts/${contactId}/interactions`, {
      headers: API_HEADERS,
      data: { occurred_at: pastDate, direction: 'mutual' },
    })

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')

    // Wait for contact data to load
    await expect(page.getByRole('heading', { name: 'Direction Signal Test' })).toBeVisible({
      timeout: 15000,
    })

    // Direction signals section should appear with outreach and response timestamps
    await expect(page.getByText('Last outreach:')).toBeVisible({ timeout: 5000 })
    await expect(page.getByText('Last response:')).toBeVisible()
  })

  test('shows outreach but not response after outbound-only interaction', async ({
    page,
    request,
  }) => {
    // Create a fresh contact with no prior interactions
    const { ids } = await testApi.seedContacts([
      { full_name: 'Outbound Only Test', cadence: 'monthly' },
    ])
    const contactId = ids[0]

    // Record an outbound interaction
    const pastDate = new Date(Date.now() - 2 * 24 * 60 * 60 * 1000).toISOString()
    await request.post(`${API_BASE_URL}/api/v1/contacts/${contactId}/interactions`, {
      headers: API_HEADERS,
      data: { occurred_at: pastDate, direction: 'outbound' },
    })

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')

    await expect(page.getByRole('heading', { name: 'Outbound Only Test' })).toBeVisible({
      timeout: 15000,
    })

    // Should show outreach timestamp
    await expect(page.getByText('Last outreach:')).toBeVisible({ timeout: 5000 })
  })

  test('interaction API response includes direction field', async ({ request }) => {
    const { ids } = await testApi.seedContacts([{ full_name: 'Direction API E2E Test' }])
    const contactId = ids[0]

    // Create interaction with direction
    const pastDate = new Date(Date.now() - 60 * 60 * 1000).toISOString()
    const createResp = await request.post(
      `${API_BASE_URL}/api/v1/contacts/${contactId}/interactions`,
      {
        headers: API_HEADERS,
        data: { occurred_at: pastDate, direction: 'inbound' },
      }
    )
    expect(createResp.ok()).toBeTruthy()
    const createBody = await createResp.json()
    expect(createBody.data.direction).toBe('inbound')

    // List interactions — should include direction
    const listResp = await request.get(
      `${API_BASE_URL}/api/v1/contacts/${contactId}/interactions`,
      { headers: API_HEADERS }
    )
    expect(listResp.ok()).toBeTruthy()
    const listBody = await listResp.json()
    expect(listBody.data.length).toBeGreaterThan(0)
    expect(listBody.data[0].direction).toBe('inbound')
  })

  test('contact API response includes new direction timestamp fields', async ({ request }) => {
    const { ids } = await testApi.seedContacts([
      { full_name: 'Timestamps API E2E Test', cadence: 'monthly' },
    ])
    const contactId = ids[0]

    // Record a mutual interaction to populate timestamps
    const pastDate = new Date(Date.now() - 60 * 60 * 1000).toISOString()
    await request.post(`${API_BASE_URL}/api/v1/contacts/${contactId}/interactions`, {
      headers: API_HEADERS,
      data: { occurred_at: pastDate, direction: 'mutual' },
    })

    const resp = await request.get(`${API_BASE_URL}/api/v1/contacts/${contactId}`, {
      headers: API_HEADERS,
    })
    expect(resp.ok()).toBeTruthy()
    const body = await resp.json()

    expect(body.data).toHaveProperty('last_interaction_at')
    expect(body.data).toHaveProperty('last_outreach_at')
    expect(body.data).toHaveProperty('last_response_at')
    expect(body.data).toHaveProperty('has_pending_followup')
    expect(body.data.has_pending_followup).toBe(false)
  })
})
