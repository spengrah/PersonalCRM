import { test, expect, type APIRequestContext } from '@playwright/test'

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

  test('renders empty-state when no Mac hosts are paired', async ({ page, request }) => {
    await deleteAllMacHosts(request)
    await page.goto('/settings/mac', { waitUntil: 'domcontentloaded' })

    await expect(page.getByRole('heading', { name: 'Mac Daemon' })).toBeVisible({
      timeout: 10_000,
    })
    // Pair-new-Mac CTA + back link are always rendered.
    await expect(page.getByRole('button', { name: 'Pair new Mac' })).toBeVisible()
    await expect(page.getByRole('link', { name: /Back to Settings/i })).toBeVisible()

    // Empty-state messaging.
    await expect(page.getByText('No Mac hosts paired')).toBeVisible()
  })

  test('opens pairing modal with a token when Pair new Mac is clicked', async ({
    page,
    request,
  }) => {
    await deleteAllMacHosts(request)
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

    // Wait for the empty-state to confirm the list query resolved + render
    // pass completed.
    await expect(page.getByText('No Mac hosts paired')).toBeVisible({ timeout: 10_000 })

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

  test('renders icloud_contacts contact count when backfill_complete (#327)', async ({
    page,
    request,
  }) => {
    await deleteAllMacHosts(request)
    const hostId = await seedMacHost(request, {
      hostname: 'e2e-icloud-host',
      daemon_version: '0.1.2',
      protocol_version: 2,
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

    // The Cursor cell renders the contact count + checkmark instead of
    // the misleading change-token / dash.
    await expect(sourceHealth.getByText('3 contacts ✓')).toBeVisible()

    // Cleanup.
    await deleteAllMacHosts(request)
  })

  test('renders dash for icloud_contacts when backfill_complete is false (#327)', async ({
    page,
    request,
  }) => {
    await deleteAllMacHosts(request)
    await seedMacHost(request, {
      hostname: 'e2e-icloud-host-incomplete',
      daemon_version: '0.1.2',
      protocol_version: 2,
      source_health: {
        icloud_contacts: {
          last_pushed_at: '2026-05-01T00:00:00Z',
          backfill_complete: false,
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

    // While backfill is in progress, the cell stays as a dash; no
    // numeric substitution happens.
    const rows = sourceHealth.locator('tbody tr')
    const icloudRow = rows.first()
    await expect(icloudRow.locator('td').nth(2)).toHaveText('—')

    await deleteAllMacHosts(request)
  })

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

    // Row disappears, empty-state returns.
    await expect(page.getByText('No Mac hosts paired')).toBeVisible({ timeout: 10_000 })
  })
})
