import { test, expect, APIRequestContext } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'
import { getTodayUTCShort } from './helpers/date-utils'
import { waitForOverdueListSettled } from './helpers/dashboard'

/**
 * E2E coverage for overdue-contact state changes.
 *
 * The dashboard-card "Mark as Contacted" halves (server-timestamped mutual
 * interaction + immediate removal, CAD-028.mutual-interaction-logged-timestamped + .contact-leaves-overdue-list) are cited and proven in
 * dashboard.spec.ts; this file owns the detail-page Log Interaction modal
 * variant (CON-053), the cross-view consistency leg (CAD-028.change-consistent-across-dashboard), and the
 * overdue endpoint's reporting contract (CAD-023).
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

  test('clears the contact from the overdue list when logged from Contact Detail', async ({
    page,
    request,
  }) => {
    // The modal's mechanics (direction picker, backdating, close-on-success)
    // are owned by contacts.spec.ts; this variant proves the same default-
    // mutual submission from a SEEDED-OVERDUE fixture, plus the outcome
    // unique to this entry point: the logged interaction clears the
    // contact's overdue state.
    // spec: CON-053.direction-chosen-outbound-inbound, CON-053.interaction-posted-chosen-direction
    const contactName = `${testApi.prefix}-Overdue Test Contact`

    // Seeded-data precondition: the contact renders as overdue on the
    // dashboard before the action.
    await page.goto('/dashboard')
    await expect(page.getByRole('heading', { name: contactName })).toBeVisible()

    // Go to contact detail and log a mutual interaction via the modal.
    // The header button is "Log Interaction" (not "Mark as Contacted")
    // and posts to POST /interactions instead of the legacy PATCH
    // /last-contacted endpoint.
    await page.goto(`/contacts/${contactId}`)
    await expect(page.getByRole('heading', { name: contactName, level: 2 })).toBeVisible()

    const responsePromise = page.waitForResponse(
      resp =>
        resp.url().includes(`/api/v1/contacts/${contactId}/interactions`) &&
        resp.request().method() === 'POST'
    )
    await page.getByRole('button', { name: /Log Interaction/i }).click()
    await expect(page.getByRole('dialog')).toBeVisible()
    // Submit without touching the direction picker: the modal's default
    // direction (mutual) is what must land in the POST body.
    await page.getByRole('button', { name: 'Log', exact: true }).click()
    const interactionResponse = await responsePromise
    expect(interactionResponse.ok()).toBeTruthy()
    expect(interactionResponse.request().postDataJSON()).toMatchObject({ direction: 'mutual' })

    // The modal closes on success.
    await expect(page.getByRole('dialog')).not.toBeVisible()

    // Verify via API the contact is no longer overdue (mutual + cadence
    // recompute clears last_contacted to today, contact_by advances).
    const stillOverdue = await isContactOverdue(request, contactId)
    expect(stillOverdue).toBe(false)
  })

  test('all views should show consistent state after marking as contacted', async ({
    page,
    request,
  }) => {
    // spec: CAD-028.change-consistent-across-dashboard, CAD-029.last-response-time-shown
    const contactName = `${testApi.prefix}-Overdue Test Contact`

    // A SENTINEL that stays overdue: its rendered card is the data-derived
    // settle signal on the dashboard ("Action Required" renders even while
    // loading, so the heading alone cannot prove the list rendered FROM
    // DATA).
    const { ids: sentinelIds } = await testApi.seedOverdueContacts([
      {
        full_name: 'Consistency Sentinel',
        cadence: 'weekly',
        days_overdue: 4,
      },
    ])
    const sentinelId = sentinelIds[0]
    const sentinelName = `${testApi.prefix}-Consistency Sentinel`

    // Log a mutual interaction via the API (replaces the deleted PATCH
    // /last-contacted endpoint). All "Mark as Contacted" surfaces
    // route through POST /interactions {direction:"mutual"}.
    const interactionResponse = await request.post(
      `${API_BASE_URL}/api/v1/contacts/${contactId}/interactions`,
      {
        headers: API_HEADERS,
        data: { direction: 'mutual' },
      }
    )
    expect(interactionResponse.ok()).toBeTruthy()

    // 1. Dashboard: the marked contact is GONE from the overdue list while
    // the sentinel still renders. Content-predicate wait registered BEFORE
    // goto, then the sentinel card proves the list rendered from that data —
    // not a loading frame — before the absence assertion.
    const overdueSettled = waitForOverdueListSettled(page, {
      absentIds: [contactId],
      presentIds: [sentinelId],
    })
    await page.goto('/dashboard')
    await overdueSettled
    await expect(page.getByRole('heading', { name: sentinelName })).toBeVisible()
    await expect(page.getByRole('heading', { name: contactName })).not.toBeVisible()

    // 2. Contacts List: today's date appears in the target row's SPECIFIC
    // "Last response" column (a mutual interaction sets last_response_at),
    // not just anywhere in the row. The column index is derived from the
    // table header rather than hardcoded, so a column reorder cannot
    // silently point this at the wrong cell. Filter the list by the test
    // prefix so another worker's contacts cannot push the target off the
    // 20-row first page.
    await page.goto(`/contacts?search=${encodeURIComponent(testApi.prefix)}`)
    const table = page.getByRole('table')
    const contactRow = table.getByRole('row').filter({ hasText: contactName })
    await expect(contactRow).toBeVisible()
    // innerText is RENDERED text — the header row is CSS-uppercased, so the
    // match must be case-insensitive.
    const headerTexts = await table.getByRole('columnheader').allInnerTexts()
    const lastResponseColumn = headerTexts.findIndex(text =>
      text.toLowerCase().includes('last response')
    )
    expect(lastResponseColumn).toBeGreaterThanOrEqual(0)
    // getTodayUTCShort matches the table's UTC-date rendering (formatDateOnly
    // extracts the UTC date portion), so the assertion is timezone-stable.
    const todayShort = getTodayUTCShort()
    await expect(contactRow.getByRole('cell').nth(lastResponseColumn)).toHaveText(todayShort)

    // 3. Contact Detail: the recent-activity block reflects the new mutual
    // interaction — the "Last response" row is rendered only when a
    // response timestamp exists (CAD-029.last-response-time-shown), and it must carry a VALUE,
    // not just the label.
    await page.goto(`/contacts/${contactId}`)
    await expect(page.getByRole('heading', { name: contactName })).toBeVisible()
    await expect(page.getByText(/Last response: \S+/)).toBeVisible()

    // 4. API: backend-state cross-check — no longer overdue.
    const stillOverdue = await isContactOverdue(request, contactId)
    expect(stillOverdue).toBe(false)
  })
})

test.describe('Overdue Contact Updates - Multiple Contacts @area:overdue', () => {
  let testApi: TestAPI
  let firstId: string
  let secondId: string

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)

    // Seed multiple overdue contacts
    const { ids } = await testApi.seedOverdueContacts([
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
    firstId = ids[0]
    secondId = ids[1]
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should show multiple overdue contacts on dashboard', async ({ page, request }) => {
    await page.goto('/dashboard')
    await page.waitForLoadState('domcontentloaded')

    // Both contacts should be visible (DOM precondition: the dashboard rendered
    // the seeded cards).
    await expect(
      page.getByRole('heading', { name: `${testApi.prefix}-First Overdue` })
    ).toBeVisible()
    await expect(
      page.getByRole('heading', { name: `${testApi.prefix}-Second Overdue` })
    ).toBeVisible()

    // CAD-023.list-bounded-1000-truncation (the 1000-entry bound + most-overdue-retained truncation)
    // is waived in spec/cadence-followup.yaml: not E2E-seedable.
    // spec: CAD-023.each-entry-carries-contact, CAD-023.entries-ordered-most-overdue
    const overdueRes = await request.get(`${API_BASE_URL}/api/v1/contacts/overdue`, {
      headers: API_HEADERS,
    })
    expect(overdueRes.ok()).toBe(true)
    const overdueBody = await overdueRes.json()
    const entries: Array<{
      id: string
      days_overdue: number
      next_due_date: string
      suggested_action: string
    }> = overdueBody?.data ?? []

    // Membership: both our seeded contacts are in the overdue list.
    const firstEntry = entries.find(e => e.id === firstId)
    const secondEntry = entries.find(e => e.id === secondId)
    expect(firstEntry, 'first overdue contact should be in the list').toBeTruthy()
    expect(secondEntry, 'second overdue contact should be in the list').toBeTruthy()

    // Entry metadata: each entry carries days overdue, next due date, and a
    // suggested action.
    for (const entry of [firstEntry, secondEntry]) {
      expect(entry!.days_overdue).toBeGreaterThanOrEqual(1)
      expect(entry!.next_due_date).toBeTruthy()
      expect(typeof entry!.suggested_action).toBe('string')
      expect(entry!.suggested_action.length).toBeGreaterThan(0)
    }

    // Relative ordering, scoped to our own two rows: the 10-days-overdue
    // contact ranks before the 3-days-overdue one.
    const ourIds = entries.map(e => e.id).filter(id => id === firstId || id === secondId)
    expect(ourIds.indexOf(secondId)).toBeLessThan(ourIds.indexOf(firstId))
  })
})
