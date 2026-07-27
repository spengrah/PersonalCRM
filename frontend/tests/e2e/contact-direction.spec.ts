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
    // spec: CAD-029.last-outreach-time-shown, CAD-029.last-response-time-shown
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

    // Direction signals section should appear with outreach and response
    // TIMES, not just the labels — the value after the colon must be
    // non-empty (formatRelativeTime renders e.g. "3 days ago"; a blank
    // value would render "Last outreach: " and fail the \S+ match).
    await expect(page.getByText(/Last outreach: \S+/)).toBeVisible({ timeout: 5000 })
    await expect(page.getByText(/Last response: \S+/)).toBeVisible()
  })

  test('shows outreach but not response after outbound-only interaction', async ({
    page,
    request,
  }) => {
    // spec: CAD-029.last-outreach-time-shown, CAD-029.last-response-time-shown
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

    // Should show the outreach TIME (non-empty value after the label)
    await expect(page.getByText(/Last outreach: \S+/)).toBeVisible({ timeout: 5000 })

    // Shown-IFF-exists: with no inbound/mutual interaction there is no
    // response timestamp, so the response line must be ABSENT.
    await expect(page.getByText('Last response:')).not.toBeVisible()
  })

  test('shows awaiting-reply indicator while a follow-up pends', async ({ page, request }) => {
    // spec: CAD-029.awaiting-reply-indicator-shown
    const { ids } = await testApi.seedContacts([
      { full_name: 'Awaiting Reply Test', cadence: 'monthly' },
    ])
    const contactId = ids[0]

    // A real outbound interaction populates last_outreach_at so the
    // recent-activity block renders from real data.
    const pastDate = new Date(Date.now() - 24 * 60 * 60 * 1000).toISOString()
    await request.post(`${API_BASE_URL}/api/v1/contacts/${contactId}/interactions`, {
      headers: API_HEADERS,
      data: { occurred_at: pastDate, direction: 'outbound' },
    })

    // has_pending_followup is provider-driven (the follow-up loop needs a
    // Todoist provider), so it is unreachable by real seeding here.
    // FETCH-AND-PATCH: fetch the real detail response and flip ONLY
    // has_pending_followup — the rest of the detail stays real. Route
    // installed BEFORE goto.
    await page.route(`**/api/v1/contacts/${contactId}`, async route => {
      const response = await route.fetch()
      const json = await response.json()
      json.data.has_pending_followup = true
      await route.fulfill({ response, json })
    })

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: 'Awaiting Reply Test' })).toBeVisible({
      timeout: 15000,
    })

    // The awaiting-reply indicator renders while the follow-up pends. Target
    // the indicator by its title attribute — the seeded contact name contains
    // "Awaiting Reply", so a substring getByText would also match the name
    // heading/definition (strict-mode violation).
    await expect(page.getByTitle('Awaiting reply')).toBeVisible()
    await expect(page.getByText(/Last outreach: \S+/)).toBeVisible()
  })

  test('shows explicit no-recent-activity state with no direction signals', async ({ page }) => {
    // spec: CAD-029.explicit-no-recent-activity
    // A freshly seeded contact has no interactions and no pending follow-up,
    // which guarantees the no-recent-activity branch (no vacuous fallback).
    const { ids } = await testApi.seedContacts([
      { full_name: 'No Activity Test', cadence: 'monthly' },
    ])

    await page.goto(`/contacts/${ids[0]}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: 'No Activity Test' })).toBeVisible({
      timeout: 15000,
    })

    await expect(page.getByText('No recent activity')).toBeVisible()
    await expect(page.getByText('Last outreach:')).not.toBeVisible()
    await expect(page.getByText('Last response:')).not.toBeVisible()
    await expect(page.getByTitle('Awaiting reply')).not.toBeVisible()
  })

  // The interaction/contact wire-shape checks (direction field on POST/list,
  // direction timestamp fields on GET) that used to live here involved no page
  // and are owned by the Go API suite: TestInteractionAPI_DirectionInResponse,
  // TestContactAPI_DirectionTimestamps, and TestContactAPI_HasPendingFollowup
  // in backend/tests/api/direction_api_test.go.
})
