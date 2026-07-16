import { test, expect, type Page, type Route } from '@playwright/test'

// The calendar sync control and staleness banner depend on provider state
// (a connected Google account, watchdog breaches) that the sandbox/CI
// deployment cannot produce, so these tests route-mock the app's own
// account/status endpoints — the sanctioned technique — and drive the real
// settings UI against those states.
//
// The app calls the API cross-origin (frontend :3000 → API :8080) with an
// X-API-Key header, so fulfilled responses answer the CORS preflight and
// carry Access-Control-Allow-Origin.
const corsHeaders = {
  'Access-Control-Allow-Origin': '*',
  'Access-Control-Allow-Methods': 'GET,POST,PATCH,DELETE,OPTIONS',
  'Access-Control-Allow-Headers': 'Content-Type,X-API-Key',
}

function corsFulfill(route: Route, body: unknown, status = 200) {
  if (route.request().method() === 'OPTIONS') {
    return route.fulfill({ status: 204, headers: corsHeaders })
  }
  return route.fulfill({
    status,
    headers: { ...corsHeaders, 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
}

const CALENDAR_SCOPE = 'https://www.googleapis.com/auth/calendar.readonly'
const ACCOUNT_ID = 'e2e-calendar-user'

const googleRegion = (page: Page) => page.getByRole('region', { name: 'Google Accounts' })

function calendarAccount() {
  return {
    id: ACCOUNT_ID,
    account_id: ACCOUNT_ID,
    created_at: '2026-01-05T12:00:00Z',
    updated_at: '2026-01-05T12:00:00Z',
    scopes: [CALENDAR_SCOPE],
  }
}

function gcalSyncState(overrides: Record<string, unknown> = {}) {
  return {
    id: 'e2e-gcal-sync-state',
    source: 'gcal',
    account_id: ACCOUNT_ID,
    enabled: true,
    status: 'idle',
    sync_cursor: null,
    last_sync_at: '2026-01-01T00:00:00Z',
    last_successful_sync_at: '2026-01-01T00:00:00Z',
    next_sync_at: null,
    error_count: 0,
    error_message: null,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...overrides,
  }
}

async function mockCalendarAccount(page: Page) {
  await page.route('**/api/v1/auth/google/accounts', route =>
    corsFulfill(route, { success: true, data: [calendarAccount()] })
  )
}

async function mockStaleness(page: Page, breaches: unknown[]) {
  await page.route('**/api/v1/sync/staleness', route =>
    corsFulfill(route, { success: true, data: breaches })
  )
}

test.describe('Settings calendar sync @area:settings', () => {
  // spec: CAL-029[0]
  test('the calendar badge shows the sync state and triggers a sync for the account', async ({
    page,
  }) => {
    await mockCalendarAccount(page)
    await page.route('**/api/v1/sync/status', route =>
      corsFulfill(route, { success: true, data: [gcalSyncState()] })
    )
    await page.route('**/api/v1/sync/gcal/trigger', route =>
      corsFulfill(route, { success: true, data: null }, 202)
    )

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    const google = googleRegion(page)
    await expect(google.getByText(ACCOUNT_ID)).toBeVisible({ timeout: 10_000 })

    // The badge reports the state derived from the mocked
    // last_successful_sync_at (2026-01-01 → "Jan 1" via formatSyncTime).
    await expect(google.getByText('Calendar', { exact: true })).toBeVisible()
    await expect(google.getByText('Jan 1', { exact: true })).toBeVisible()

    // Triggering the sync posts to the gcal trigger endpoint with the
    // account the badge belongs to.
    const syncButton = google.getByRole('button', { name: 'Sync Calendar' })
    const [triggerRequest] = await Promise.all([
      page.waitForRequest(
        r => r.url().includes('/api/v1/sync/gcal/trigger') && r.method() === 'POST'
      ),
      syncButton.click(),
    ])
    expect(triggerRequest.postDataJSON()).toMatchObject({ account_id: ACCOUNT_ID })
  })

  // spec: CAL-029[1]
  test('a triggered sync reports it started and the badge reflects the polled progress', async ({
    page,
  }) => {
    await mockCalendarAccount(page)

    // The status mock flips to syncing once the trigger lands, so the
    // post-trigger refetch observes the in-progress state.
    let syncing = false
    await page.route('**/api/v1/sync/status', route =>
      corsFulfill(route, {
        success: true,
        data: [gcalSyncState(syncing ? { status: 'syncing' } : {})],
      })
    )
    await page.route('**/api/v1/sync/gcal/trigger', route => {
      syncing = true
      return corsFulfill(route, { success: true, data: null }, 202)
    })

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    const google = googleRegion(page)
    const syncButton = google.getByRole('button', { name: 'Sync Calendar' })
    await expect(syncButton).toBeVisible({ timeout: 10_000 })
    await syncButton.click()

    // The section reports the sync started…
    await expect(google.getByText('Calendar sync started!')).toBeVisible({ timeout: 10_000 })

    // …and once the state poll returns syncing, the badge reflects it and
    // the trigger control is held disabled.
    await expect(google.getByText('Syncing...')).toBeVisible({ timeout: 10_000 })
    await expect(syncButton).toBeDisabled()
  })

  // spec: CAL-030[0]
  test('a stalled calendar sync is named on the staleness banner', async ({ page }) => {
    await mockCalendarAccount(page)
    await page.route('**/api/v1/sync/status', route =>
      corsFulfill(route, { success: true, data: [gcalSyncState()] })
    )
    await mockStaleness(page, [
      {
        id: 'e2e-gcal-breach',
        source: 'gcal',
        account_id: ACCOUNT_ID,
        breach_type: 'sync_stale',
        stale_since: '2026-01-01T00:00:00Z',
        threshold_seconds: 3600,
        details: 'no successful sync in 2h',
        detected_at: '2026-01-01T02:00:00Z',
        last_observed_at: '2026-01-01T02:00:00Z',
      },
    ])

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    // The banner names Google Calendar (label derived from the breach's
    // source datum) and carries the watchdog's details.
    const banner = page.getByTestId('sync-staleness-banner')
    await expect(banner).toBeVisible({ timeout: 10_000 })
    await expect(banner).toHaveAttribute('role', 'status')
    await expect(banner).toContainText('Google Calendar')
    await expect(banner).toContainText('no successful sync in 2h')
  })

  // spec: CAL-030[1]
  test('the staleness banner stays quiet when nothing is stalled', async ({ page }) => {
    await mockCalendarAccount(page)
    await page.route('**/api/v1/sync/status', route =>
      corsFulfill(route, { success: true, data: [gcalSyncState()] })
    )
    await mockStaleness(page, [])

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    // Wait for the page to settle (the account section has rendered its
    // data) before asserting the banner's settled-state absence.
    await expect(googleRegion(page).getByText(ACCOUNT_ID)).toBeVisible({ timeout: 10_000 })
    await expect(page.getByTestId('sync-staleness-banner')).toHaveCount(0)
  })
})
