import { test, expect, type APIRequestContext } from '@playwright/test'
import { acquireGlobalLock } from './helpers/global-lock'

// Cross-file mutex: imports-interactions.spec.ts also resets/reseeds the
// mac_host singleton, and nothing else stops the two files landing in
// different workers and nuking each other's host mid-test. The lock is
// held for the WHOLE file (beforeAll → afterAll): per-test cycling lets
// this worker instantly re-acquire between its serial tests, starving the
// other file's waiter. afterAll has its own timeout slot, and if the
// worker dies without running it the renew heartbeat stops and the lease
// lapses at the arbiter, freeing the lock.
let releaseMacHostLock: (() => Promise<void>) | null = null

test.beforeAll(async () => {
  // The contending file may hold the lock for its entire serial run.
  test.setTimeout(360_000)
  releaseMacHostLock = await acquireGlobalLock('mac-host')
})

test.afterAll(async () => {
  await releaseMacHostLock?.()
  releaseMacHostLock = null
})

// API_KEY is injected by the make target via NEXT_PUBLIC_API_KEY.
// We re-use it here for the test-only seed endpoints under
// /api/v1/test/seed/* which expect the global API key.
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || ''
const API_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'

async function seedMacHost(
  request: APIRequestContext,
  body: Record<string, unknown>
): Promise<string> {
  const resp = await request.post(`${API_URL}/api/v1/test/seed/mac-hosts`, {
    headers: { 'X-API-Key': API_KEY, 'Content-Type': 'application/json' },
    data: body,
  })
  if (!resp.ok()) {
    throw new Error(`seed mac host failed: ${resp.status()} ${await resp.text()}`)
  }
  const json = (await resp.json()) as { data: { host_id: string } }
  return json.data.host_id
}

async function deleteAllMacHosts(request: APIRequestContext): Promise<void> {
  // Best-effort cleanup before/after each paired-host test so the
  // singleton index is empty for the next test. The admin DELETE
  // cascades push-cursor rows.
  const resp = await request.get(`${API_URL}/api/v1/host`, {
    headers: { 'X-API-Key': API_KEY },
  })
  if (!resp.ok()) return
  const json = (await resp.json()) as { data: Array<{ id: string }> }
  for (const host of json.data ?? []) {
    await request.delete(`${API_URL}/api/v1/host/${host.id}`, {
      headers: { 'X-API-Key': API_KEY },
    })
  }
}

test.describe('Settings — Mac Daemon @area:settings', () => {
  // Run the Mac Daemon settings tests serially within this file: the
  // mac_host table has a partial unique index that allows only one
  // non-revoked host at a time. Parallel workers would race on
  // seed/delete and the empty-state / paired-state tests would
  // interfere with each other.
  test.describe.configure({ mode: 'serial' })
  // Headroom for slow singleton cleanup on a loaded machine.
  test.setTimeout(60_000)

  test('renders empty-state when no Mac hosts are paired', async ({ page }) => {
    // The zero-host rendering branch, driven by mocking the list endpoint:
    // the shared mac_host singleton table is seeded/reset by parallel
    // workers (imports-interactions.spec.ts), so asserting real-API global
    // emptiness is racy and would mean deleting hosts other tests own. The
    // real-API list facet (MAC-018[0]) is covered by the seeded-host and
    // uninstall tests below; this test is uncited (the mock replaces the
    // endpoint the behavior describes).
    await page.route('**/api/v1/host', route =>
      route.request().method() === 'GET'
        ? route.fulfill({
            status: 200,
            contentType: 'application/json',
            body: JSON.stringify({ success: true, data: [] }),
          })
        : route.fallback()
    )

    await page.goto('/settings/mac', { waitUntil: 'domcontentloaded' })

    await expect(page.getByText('No Mac hosts paired')).toBeVisible({ timeout: 10_000 })
    await expect(page.getByTestId('mac-host-row')).toHaveCount(0)
  })

  // spec: MAC-018[3]
  test('opens pairing modal with a token when Pair new Mac is clicked', async ({ page }) => {
    // No host seeding or reset needed: the pairing modal works regardless
    // of how many hosts are listed (parallel workers may own one).
    // Wait for the initial GET /host response BEFORE clicking — this
    // proves React Query has mounted on the page and hydration is
    // complete, otherwise the click handler can fire against an
    // unhydrated tree and silently drop the event.
    const initialListPromise = page.waitForResponse(
      resp => resp.url().endsWith('/api/v1/host') && resp.request().method() === 'GET',
      { timeout: 15_000 }
    )
    await page.goto('/settings/mac', { waitUntil: 'domcontentloaded' })
    await initialListPromise

    const pairButton = page.getByRole('button', { name: 'Pair new Mac' })
    await expect(pairButton).toBeVisible()

    const tokenResponsePromise = page.waitForResponse(
      resp =>
        resp.url().includes('/api/v1/host/pairing-token') && resp.request().method() === 'POST',
      { timeout: 10_000 }
    )
    await pairButton.click()
    const resp = await tokenResponsePromise
    expect(resp.status()).toBe(200)

    // Modal is rendered as a dialog with the matching aria-label.
    const dialog = page.getByRole('dialog', { name: 'Pair new Mac' })
    await expect(dialog).toBeVisible({ timeout: 5_000 })

    const tokenCode = page.getByTestId('pairing-token-value')
    await expect(tokenCode).toBeVisible({ timeout: 10_000 })
    await expect(tokenCode).not.toBeEmpty()

    // Close button dismisses the modal.
    await dialog.getByRole('button', { name: 'Close' }).click()
    await expect(dialog).not.toBeVisible()
  })

  // spec: MAC-018[0]
  test('renders paired host with permissions and source-health badges', async ({
    page,
    request,
  }) => {
    await deleteAllMacHosts(request)
    const hostId = await seedMacHost(request, {
      hostname: 'e2e-paired-host',
      daemon_version: '0.1.2',
      protocol_version: 1,
      permissions: { fda: true, contacts: false },
      // Sync-staleness watchdog coupling: this last_pushed_at is month-stale, so adding
      // `enabled: true` here would open a push_stale breach and surface the
      // staleness banner mid-run. Keep `enabled` absent (or last_pushed_at fresh).
      source_health: {
        messages: { last_pushed_at: '2026-05-01T00:00:00Z', pushed_cursor: 'abc-123' },
      },
    })

    // Sanity check via the admin API that the host is really there
    // before navigating — if this fails the front-end test will never
    // pass and the failure mode is more obvious.
    const verify = await request.get(`${API_URL}/api/v1/host`, {
      headers: { 'X-API-Key': API_KEY },
    })
    const verifyBody = (await verify.json()) as { data: Array<{ hostname: string }> }
    expect(verifyBody.data.some(h => h.hostname === 'e2e-paired-host')).toBe(true)

    // Wait for the page's own list query to resolve so we know the
    // render below is reacting to a non-empty response.
    const listPromise = page.waitForResponse(
      resp => resp.url().endsWith('/api/v1/host') && resp.request().method() === 'GET',
      { timeout: 15_000 }
    )
    await page.goto('/settings/mac', { waitUntil: 'domcontentloaded' })
    await listPromise

    const row = page.getByTestId('mac-host-row').first()
    await expect(row).toBeVisible({ timeout: 10_000 })
    await expect(row.getByText('e2e-paired-host')).toBeVisible()
    await expect(row.getByText(hostId, { exact: false })).toBeVisible()

    // Permissions badges
    const permissions = row.getByTestId('permissions-badges')
    await expect(permissions).toBeVisible()
    await expect(permissions.getByText(/Full Disk Access: granted/)).toBeVisible()
    await expect(permissions.getByText(/Contacts: denied/)).toBeVisible()

    // Source-health table
    const sourceHealth = row.getByTestId('source-health')
    await expect(sourceHealth).toBeVisible()
    await expect(sourceHealth.getByText('messages')).toBeVisible()
    await expect(sourceHealth.getByText('abc-123')).toBeVisible()

    await deleteAllMacHosts(request)
  })

  // spec: MAC-046[0]
  test('renders icloud_contacts contact count when backfill_complete (#327)', async ({
    page,
    request,
  }) => {
    await deleteAllMacHosts(request)
    const hostId = await seedMacHost(request, {
      hostname: 'e2e-icloud-host',
      daemon_version: '0.1.2',
      protocol_version: 2,
      // Sync-staleness watchdog coupling: this last_pushed_at is month-stale, so adding
      // `enabled: true` here would open a push_stale breach and surface the
      // staleness banner mid-run. Keep `enabled` absent (or last_pushed_at fresh).
      source_health: {
        icloud_contacts: {
          last_pushed_at: '2026-05-01T00:00:00Z',
          backfill_complete: true,
        },
      },
    })

    // Seed external_contact rows for the host so the count is non-zero.
    const seedPrefix = `e2e-icloud-${Date.now()}`
    const seedResp = await request.post(`${API_URL}/api/v1/test/seed/external-contacts`, {
      headers: { 'X-API-Key': API_KEY, 'Content-Type': 'application/json' },
      data: {
        prefix: seedPrefix,
        host_id: hostId,
        contacts: [
          { display_name: 'iCloud A', source: 'icloud_contacts', emails: ['a@example.com'] },
          { display_name: 'iCloud B', source: 'icloud_contacts', emails: ['b@example.com'] },
          { display_name: 'iCloud C', source: 'icloud_contacts', emails: ['c@example.com'] },
        ],
      },
    })
    expect(seedResp.ok()).toBe(true)

    const listPromise = page.waitForResponse(
      resp => resp.url().endsWith('/api/v1/host') && resp.request().method() === 'GET',
      { timeout: 15_000 }
    )
    const countsPromise = page.waitForResponse(
      resp =>
        resp.url().includes('/api/v1/host/' + hostId + '/source-counts') &&
        resp.request().method() === 'GET',
      { timeout: 15_000 }
    )
    await page.goto('/settings/mac', { waitUntil: 'domcontentloaded' })
    await listPromise
    await countsPromise

    const row = page.getByTestId('mac-host-row').first()
    await expect(row).toBeVisible({ timeout: 10_000 })
    const sourceHealth = row.getByTestId('source-health')
    await expect(sourceHealth).toBeVisible()

    // The Cursor cell substitutes the live contact count (3 seeded rows)
    // for the misleading change-token / dash. The checkmark glyph is
    // presentation — assert the count and the cell's state instead.
    const cursorCell = sourceHealth.getByTestId('cursor-cell')
    await expect(cursorCell).toHaveText(/3 contacts/)
    await expect(cursorCell).toHaveAttribute('data-state', 'count')

    // Cleanup.
    await deleteAllMacHosts(request)
  })

  // spec: MAC-046[1]
  test('renders dash for icloud_contacts when backfill_complete is false (#327)', async ({
    page,
    request,
  }) => {
    await deleteAllMacHosts(request)
    await seedMacHost(request, {
      hostname: 'e2e-icloud-host-incomplete',
      daemon_version: '0.1.2',
      protocol_version: 2,
      // Sync-staleness watchdog coupling: this last_pushed_at is month-stale, so adding
      // `enabled: true` here would open a push_stale breach and surface the
      // staleness banner mid-run. Keep `enabled` absent (or last_pushed_at fresh).
      source_health: {
        icloud_contacts: {
          last_pushed_at: '2026-05-01T00:00:00Z',
          backfill_complete: false,
          // A pushed change-token is the NORMAL mid-backfill state — the
          // cell must show the placeholder, never this opaque token.
          pushed_cursor: 'e2e-change-token-xyz',
        },
      },
    })

    const listPromise = page.waitForResponse(
      resp => resp.url().endsWith('/api/v1/host') && resp.request().method() === 'GET',
      { timeout: 15_000 }
    )
    await page.goto('/settings/mac', { waitUntil: 'domcontentloaded' })
    await listPromise

    const row = page.getByTestId('mac-host-row').first()
    const sourceHealth = row.getByTestId('source-health')
    await expect(sourceHealth).toBeVisible({ timeout: 10_000 })

    // While backfill is in progress, the cell stays in its no-count state:
    // no numeric substitution, and the raw change-token is never shown.
    await expect(sourceHealth.getByTestId('cursor-cell')).toHaveAttribute('data-state', 'pending')
    await expect(sourceHealth.getByText(/\d+ contacts/)).toHaveCount(0)
    await expect(sourceHealth.getByText('e2e-change-token-xyz')).toHaveCount(0)

    await deleteAllMacHosts(request)
  })

  // spec: MAC-018[3]
  test('opens rotate-key modal with templated CLI command when Rotate Key is clicked', async ({
    page,
    request,
    context,
  }) => {
    await deleteAllMacHosts(request)
    await seedMacHost(request, { hostname: 'rotate-test-host', protocol_version: 1 })

    // Grant clipboard permissions so navigator.clipboard.readText
    // works for the Copy assertion below.
    await context.grantPermissions(['clipboard-read', 'clipboard-write'])

    const listPromise = page.waitForResponse(
      resp => resp.url().endsWith('/api/v1/host') && resp.request().method() === 'GET',
      { timeout: 15_000 }
    )
    await page.goto('/settings/mac', { waitUntil: 'domcontentloaded' })
    await listPromise

    const row = page.getByTestId('mac-host-row').first()
    await expect(row.getByText('rotate-test-host')).toBeVisible({ timeout: 10_000 })

    const rotateButton = row.getByRole('button', {
      name: /Rotate pair-key for rotate-test-host/i,
    })
    await expect(rotateButton).toBeVisible()

    const tokenResponsePromise = page.waitForResponse(
      resp =>
        resp.url().includes('/api/v1/host/pairing-token') && resp.request().method() === 'POST',
      { timeout: 10_000 }
    )
    await rotateButton.click()
    const resp = await tokenResponsePromise
    expect(resp.status()).toBe(200)

    const dialog = page.getByRole('dialog', { name: /Rotate pair-key for rotate-test-host/i })
    await expect(dialog).toBeVisible({ timeout: 5_000 })

    // The displayed command is the full templated re-pair invocation —
    // operator can copy and paste directly into a Mac terminal. The
    // token is base64url-encoded 24 bytes = 32 chars.
    const commandEl = page.getByTestId('rotate-key-command')
    await expect(commandEl).toBeVisible({ timeout: 10_000 })
    const commandText = (await commandEl.textContent()) ?? ''
    expect(commandText).toMatch(/^crm-mac install --re-pair --pair [A-Za-z0-9_-]{32,}$/)

    // Copy button copies the FULL command (not just the token).
    await dialog.getByRole('button', { name: /Copy/i }).click()
    await expect(dialog.getByRole('button', { name: /Copied/i })).toBeVisible({ timeout: 5_000 })
    const clipboardText = await page.evaluate(() => navigator.clipboard.readText())
    expect(clipboardText).toBe(commandText)

    await dialog.getByRole('button', { name: 'Close' }).click()
    await expect(dialog).not.toBeVisible()

    await deleteAllMacHosts(request)
  })

  // spec: MAC-018[3], MAC-018[0]
  test('uninstall flow removes a paired host', async ({ page, request }) => {
    await deleteAllMacHosts(request)
    await seedMacHost(request, { hostname: 'e2e-uninstall-me', protocol_version: 1 })

    const listPromise = page.waitForResponse(
      resp => resp.url().endsWith('/api/v1/host') && resp.request().method() === 'GET',
      { timeout: 15_000 }
    )
    await page.goto('/settings/mac', { waitUntil: 'domcontentloaded' })
    await listPromise
    const row = page.getByTestId('mac-host-row').first()
    await expect(row.getByText('e2e-uninstall-me')).toBeVisible({ timeout: 10_000 })

    // Click the row's Uninstall button (aria-label scoped to hostname).
    await row.getByRole('button', { name: /Uninstall e2e-uninstall-me/i }).click()
    const confirmDialog = page.getByRole('dialog', { name: 'Uninstall Mac host' })
    await expect(confirmDialog).toBeVisible()
    const deleteResponsePromise = page.waitForResponse(
      resp => resp.url().includes('/api/v1/host/') && resp.request().method() === 'DELETE',
      { timeout: 10_000 }
    )
    await confirmDialog.getByRole('button', { name: 'Uninstall', exact: true }).click()
    const deleteResp = await deleteResponsePromise
    expect(deleteResp.status()).toBe(200)

    // The removal is reflected in the live list: this test's host row is
    // gone (scoped to the seeded hostname — a parallel worker may own an
    // unrelated host at this moment).
    await expect(
      page.getByTestId('mac-host-row').filter({ hasText: 'e2e-uninstall-me' })
    ).toHaveCount(0, { timeout: 10_000 })
  })
})
