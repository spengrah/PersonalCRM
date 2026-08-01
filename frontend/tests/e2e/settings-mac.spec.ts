import { test, expect, type APIRequestContext, type Page } from '@playwright/test'
import { acquireGlobalLock } from './helpers/global-lock'
import { createTestAPI, TestAPI, type SeedBehaviorResult } from './helpers/test-api'

// Cross-file mutex on the mac_host SINGLETON. The resource is a database-wide
// partial unique index that permits one non-revoked host, not a spec convention:
// a declared world pairs a LIVE host through the real pairing service, so a
// second world attempting it while this one holds the slot fails its SEED
// outright rather than waiting. Only this file pairs one today, and the lock
// still has to stay — with it, a future second declaring spec queues; without it,
// that spec's seed errors.
//
// It is held for the WHOLE file (beforeAll → afterAll): per-test cycling lets
// this worker instantly re-acquire between its serial tests, starving another
// file's waiter. afterAll has its own timeout slot, and if the worker dies
// without running it the renew heartbeat stops and the lease lapses at the
// arbiter, freeing the lock.
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
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || ''
const API_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'

interface AdminHost {
  id: string
  hostname: string
  source_health: Record<string, { pushed_cursor?: string | number; backfill_complete?: boolean }>
}

// The admin list is where a test learns what the daemon reported, so a cursor or
// a flag is read from the API rather than restated as a literal here.
async function fetchHost(request: APIRequestContext, hostname: string): Promise<AdminHost> {
  const response = await request.get(`${API_URL}/api/v1/host`, {
    headers: { 'X-API-Key': API_KEY },
  })
  expect(response.ok()).toBe(true)
  const hosts = ((await response.json()) as { data: AdminHost[] }).data ?? []
  const host = hosts.find(h => h.hostname === hostname)
  expect(host, `the declared host ${hostname} should be in the live host list`).toBeTruthy()
  return host!
}

// Every host row read is filtered to THIS world's hostname first. The row list is
// global, so `.first()` would be a coin flip against a parallel worker's host.
const hostRow = (page: Page, hostname: string) =>
  page.getByTestId('mac-host-row').filter({ hasText: hostname })

// The source-health table renders one real <tr> per source, so a per-source cell
// is addressed through its row. With two sources on one host an unscoped
// cursor-cell locator is strict-mode ambiguous.
const sourceRow = (page: Page, hostname: string, label: string) =>
  hostRow(page, hostname).getByTestId('source-health').getByRole('row').filter({ hasText: label })

const declaredHostname = (seeded: SeedBehaviorResult): string => seeded.entities['host'].name
const declaredHostId = (seeded: SeedBehaviorResult): string => seeded.entities['host'].id

async function waitForHostList(page: Page): Promise<void> {
  const listPromise = page.waitForResponse(
    resp => resp.url().endsWith('/api/v1/host') && resp.request().method() === 'GET',
    { timeout: 15_000 }
  )
  await page.goto('/settings/mac', { waitUntil: 'domcontentloaded' })
  await listPromise
}

test.describe('Settings — Mac Daemon @area:settings', () => {
  // Run the Mac Daemon settings tests serially within this file: a declared world
  // pairs a LIVE host, and the mac_host partial unique index permits only one at
  // a time. Each test's afterEach cleanup deletes its host, which is what frees
  // the slot for the next one.
  test.describe.configure({ mode: 'serial' })
  // Headroom for the declared seed (which pairs a host through the real service)
  // plus its cleanup.
  test.setTimeout(60_000)

  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('renders empty-state when no Mac hosts are paired', async ({ page }) => {
    // The zero-host rendering branch, driven by mocking the list endpoint:
    // asserting real-API global emptiness is racy on a shared database and would
    // mean deleting hosts other tests own. The real-API list facet
    // (MAC-018.list-returns-live-hosts) is covered by the declared-host and
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

  // spec: MAC-018.admin-can-mint-fresh
  test('opens pairing modal with a token when Pair new Mac is clicked', async ({ page }) => {
    // No host needed: the pairing affordance renders unconditionally in the page
    // header, whatever the list holds (a parallel worker may own one).
    // Waiting for the initial GET /host response BEFORE clicking proves React
    // Query has mounted and hydration is complete, otherwise the click handler can
    // fire against an unhydrated tree and silently drop the event.
    await waitForHostList(page)

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

  // spec: MAC-018.list-returns-live-hosts
  test('renders paired host with permissions and source-health badges', async ({
    page,
    request,
  }) => {
    // The declared world pairs a live host through the real pairing services and
    // heartbeats the daemon-supplied permissions and source health onto it, so
    // everything asserted below is a value the daemon protocol can actually carry.
    const seeded = await testApi.seedBehavior('MAC-018')
    const hostname = declaredHostname(seeded)
    const hostId = declaredHostId(seeded)

    // The cursor the host reported, read from the admin API rather than restated.
    const host = await fetchHost(request, hostname)
    expect(host.id).toBe(hostId)
    const messagesCursor = String(host.source_health['messages'].pushed_cursor)
    expect(messagesCursor).toBeTruthy()

    await waitForHostList(page)

    const row = hostRow(page, hostname)
    await expect(row).toBeVisible({ timeout: 10_000 })
    await expect(row.getByText(hostname)).toBeVisible()
    await expect(row.getByText(hostId, { exact: false })).toBeVisible()

    // Permissions badges — the declared host reports one granted and one denied,
    // so both states are observable on the same row.
    const permissions = row.getByTestId('permissions-badges')
    await expect(permissions).toBeVisible()
    await expect(permissions.getByText(/Full Disk Access: granted/)).toBeVisible()
    await expect(permissions.getByText(/Contacts: denied/)).toBeVisible()

    // Source-health table: the messages row is labelled and renders its cursor
    // verbatim. The label is matched EXACTLY on its own cell — the cursor value
    // ends in "-cursor-messages", and getByText is a case-insensitive SUBSTRING
    // match, so an unqualified 'Messages' resolves to both cells.
    const messagesRow = sourceRow(page, hostname, 'Messages')
    await expect(row.getByTestId('source-health')).toBeVisible()
    await expect(messagesRow.getByRole('cell', { name: 'Messages', exact: true })).toBeVisible()
    await expect(messagesRow.getByTestId('cursor-cell')).toHaveText(messagesCursor)
  })

  // spec: MAC-046.backfill-complete-shows-count
  test('renders icloud_contacts contact count when backfill_complete (#327)', async ({
    page,
    request,
  }) => {
    // The declared world pairs a host reporting a COMPLETED iCloud backfill and
    // pushes three iCloud contacts through the real ingest pipeline onto that same
    // host — the per-host count route reads host_id, so the candidates have to be
    // owned by the host whose row renders.
    const seeded = await testApi.seedBehavior('MAC-046')
    const hostname = declaredHostname(seeded)
    const hostId = declaredHostId(seeded)

    const host = await fetchHost(request, hostname)
    expect(host.source_health['icloud_contacts'].backfill_complete).toBe(true)

    const countsPromise = page.waitForResponse(
      resp =>
        resp.url().includes('/api/v1/host/' + hostId + '/source-counts') &&
        resp.request().method() === 'GET',
      { timeout: 15_000 }
    )
    await waitForHostList(page)
    await countsPromise

    const row = hostRow(page, hostname)
    await expect(row).toBeVisible({ timeout: 10_000 })

    // The Cursor cell substitutes the live contact count (3 declared candidates)
    // for the misleading change-token. The checkmark glyph is presentation —
    // assert the count and the cell's state instead.
    const cursorCell = sourceRow(page, hostname, 'iCloud Contacts').getByTestId('cursor-cell')
    await expect(cursorCell).toHaveText(/3 contacts/)
    await expect(cursorCell).toHaveAttribute('data-state', 'count')
  })

  // spec: MAC-046.backfill-in-progress-placeholder
  test('renders dash for icloud_contacts when backfill_complete is false (#327)', async ({
    page,
    request,
  }) => {
    // This rides MAC-018's world rather than MAC-046's. The two MAC-046 keys are
    // mutually exclusive states of ONE flag on a SINGLETON host, so no single
    // fixture can hold both — and a freshly paired host reporting an iCloud source
    // mid-backfill is the honest shape for this half, which is exactly what
    // MAC-018's host carries. The citation stays on MAC-046: riding another
    // behavior's fixture never moves a citation.
    const seeded = await testApi.seedBehavior('MAC-018')
    const hostname = declaredHostname(seeded)

    const host = await fetchHost(request, hostname)
    const entry = host.source_health['icloud_contacts']
    expect(entry.backfill_complete).toBe(false)
    // A pushed change-token IS the normal mid-backfill state — the cell must show
    // the placeholder, never this opaque value.
    const opaqueToken = String(entry.pushed_cursor)
    expect(opaqueToken).toBeTruthy()

    await waitForHostList(page)

    const sourceHealth = hostRow(page, hostname).getByTestId('source-health')
    await expect(sourceHealth).toBeVisible({ timeout: 10_000 })

    // While backfill is in progress, the cell stays in its no-count state: no
    // numeric substitution, and the raw change-token is never shown.
    await expect(
      sourceRow(page, hostname, 'iCloud Contacts').getByTestId('cursor-cell')
    ).toHaveAttribute('data-state', 'pending')
    await expect(sourceHealth.getByText(/\d+ contacts/)).toHaveCount(0)
    await expect(sourceHealth.getByText(opaqueToken)).toHaveCount(0)
  })

  // spec: MAC-018.admin-can-mint-fresh
  test('opens rotate-key modal with templated CLI command when Rotate Key is clicked', async ({
    page,
    request,
    context,
  }) => {
    const seeded = await testApi.seedBehavior('MAC-018')
    const hostname = declaredHostname(seeded)
    await fetchHost(request, hostname)

    // Grant clipboard permissions so navigator.clipboard.readText
    // works for the Copy assertion below.
    await context.grantPermissions(['clipboard-read', 'clipboard-write'])

    await waitForHostList(page)

    const row = hostRow(page, hostname)
    await expect(row.getByText(hostname)).toBeVisible({ timeout: 10_000 })

    const rotateButton = row.getByRole('button', {
      name: new RegExp(`Rotate pair-key for ${hostname}`, 'i'),
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

    const dialog = page.getByRole('dialog', {
      name: new RegExp(`Rotate pair-key for ${hostname}`, 'i'),
    })
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
  })

  // spec: MAC-018.admin-can-mint-fresh, MAC-018.list-returns-live-hosts
  test('uninstall flow removes a paired host', async ({ page, request }) => {
    const seeded = await testApi.seedBehavior('MAC-018')
    const hostname = declaredHostname(seeded)
    const hostId = declaredHostId(seeded)
    await fetchHost(request, hostname)

    await waitForHostList(page)
    const row = hostRow(page, hostname)
    await expect(row.getByText(hostname)).toBeVisible({ timeout: 10_000 })

    // Click the row's Uninstall button (aria-label scoped to hostname).
    await row.getByRole('button', { name: new RegExp(`Uninstall ${hostname}`, 'i') }).click()
    const confirmDialog = page.getByRole('dialog', { name: 'Uninstall Mac host' })
    await expect(confirmDialog).toBeVisible()
    // The id is pinned so a parallel worker's DELETE cannot satisfy this wait.
    const deleteResponsePromise = page.waitForResponse(
      resp => resp.url().includes(`/api/v1/host/${hostId}`) && resp.request().method() === 'DELETE',
      { timeout: 10_000 }
    )
    await confirmDialog.getByRole('button', { name: 'Uninstall', exact: true }).click()
    const deleteResp = await deleteResponsePromise
    expect(deleteResp.status()).toBe(200)

    // The removal is reflected in the live list: this test's host row is
    // gone (scoped to the declared hostname — a parallel worker may own an
    // unrelated host at this moment).
    await expect(hostRow(page, hostname)).toHaveCount(0, { timeout: 10_000 })
  })
})
