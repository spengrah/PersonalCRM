import { test, expect, type Page, type Route } from '@playwright/test'

// The provider sections talk to the app's own account/status endpoints, and
// the sandbox/CI deployment has no GOOGLE_*/TODOIST_* credentials — so every
// provider-state test here route-mocks those endpoints (the sanctioned
// technique) to force each state deterministically, independent of env.
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

const notFoundBody = { success: false, error: { code: 'NOT_FOUND', message: 'not found' } }

// Force a provider into its unconfigured/not-connected state: its account
// routes 404 exactly as they do on a deployment without the provider's
// credentials (SET-005).
async function mockGoogleAccounts(page: Page, accounts: unknown[] | null) {
  await page.route('**/api/v1/auth/google/accounts', route =>
    accounts === null
      ? corsFulfill(route, notFoundBody, 404)
      : corsFulfill(route, { success: true, data: accounts })
  )
}

async function mockTodoistAccounts(page: Page, accounts: unknown[] | null) {
  await page.route('**/api/v1/auth/todoist/accounts', route =>
    accounts === null
      ? corsFulfill(route, notFoundBody, 404)
      : corsFulfill(route, { success: true, data: accounts })
  )
}

async function mockSyncStates(page: Page, states: unknown[]) {
  await page.route('**/api/v1/sync/status', route =>
    corsFulfill(route, { success: true, data: states })
  )
}

const googleRegion = (page: Page) => page.getByRole('region', { name: 'Google Accounts' })
const todoistRegion = (page: Page) => page.getByRole('region', { name: 'Todoist' })

const GMAIL_SCOPE = 'https://www.googleapis.com/auth/gmail.readonly'
const CALENDAR_SCOPE = 'https://www.googleapis.com/auth/calendar.readonly'
const CONTACTS_SCOPE = 'https://www.googleapis.com/auth/contacts.readonly'
const CHAT_SCOPES = [
  'https://www.googleapis.com/auth/chat.spaces.readonly',
  'https://www.googleapis.com/auth/chat.messages.readonly',
  'https://www.googleapis.com/auth/chat.memberships.readonly',
]

function googleAccount(accountId: string, overrides: Record<string, unknown> = {}) {
  return {
    id: accountId,
    account_id: accountId,
    created_at: '2026-01-05T12:00:00Z',
    updated_at: '2026-01-05T12:00:00Z',
    scopes: [] as string[],
    ...overrides,
  }
}

test.describe('Settings Page @area:settings', () => {
  // spec: SET-019[0], SET-023[0], SET-023[1]
  test('unconfigured providers collapse to a not-connected state with setup guidance', async ({
    page,
  }) => {
    await mockGoogleAccounts(page, null)
    await mockTodoistAccounts(page, null)

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    // Each provider has its own section…
    const google = googleRegion(page)
    const todoist = todoistRegion(page)
    await expect(google).toBeVisible({ timeout: 10_000 })
    await expect(todoist).toBeVisible()

    // …showing a not-connected empty state (connect affordance, no error
    // stack) rather than surfacing the 404.
    await expect(google.getByRole('button', { name: /Connect Google Account/i })).toBeVisible({
      timeout: 10_000,
    })
    await expect(todoist.getByRole('button', { name: /Connect Todoist Account/i })).toBeVisible({
      timeout: 10_000,
    })

    // …and listing the configuration the deployment must supply.
    await expect(google.getByText('GOOGLE_CLIENT_ID')).toBeVisible()
    await expect(google.getByText('GOOGLE_CLIENT_SECRET')).toBeVisible()
    await expect(google.getByText('TOKEN_ENCRYPTION_KEY')).toBeVisible()
    await expect(todoist.getByText('TODOIST_CLIENT_ID')).toBeVisible()
    await expect(todoist.getByText('TODOIST_CLIENT_SECRET')).toBeVisible()
  })

  // spec: SET-019[1], SET-019[2]
  test('a connected account shows its identity, connect date, and manage affordances', async ({
    page,
  }) => {
    await mockGoogleAccounts(page, [googleAccount('e2e-google-user')])
    await mockSyncStates(page, [])

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    const google = googleRegion(page)
    // The account identity and its connected date (mocked created_at
    // 2026-01-05, rendered by formatDate) appear on the card.
    await expect(google.getByText('e2e-google-user')).toBeVisible({ timeout: 10_000 })
    await expect(google.getByText(/Connected Jan 5, 2026/)).toBeVisible()

    // The section offers connect and per-account disconnect affordances.
    await expect(google.getByRole('button', { name: 'Add Account' })).toBeVisible()
    await expect(google.getByRole('button', { name: 'Disconnect e2e-google-user' })).toBeVisible()
  })

  // spec: SET-020[0]
  test('connecting a provider fetches the auth URL and navigates to it', async ({ page }) => {
    await mockGoogleAccounts(page, null)
    // Stub the provider consent screen with a same-origin URL so the
    // whole-page navigation is observable without a live provider.
    await page.route('**/api/v1/auth/google', route =>
      corsFulfill(route, {
        success: true,
        data: { url: '/settings?consent-stub=1', state: 'e2e-state' },
      })
    )

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    const connect = googleRegion(page).getByRole('button', { name: /Connect Google Account/i })
    await expect(connect).toBeVisible({ timeout: 10_000 })

    const authUrlRequest = page.waitForRequest(
      req => new URL(req.url()).pathname.endsWith('/api/v1/auth/google') && req.method() === 'GET'
    )
    await connect.click()
    await authUrlRequest

    // The app navigates the whole page to the returned URL.
    await page.waitForURL('**/settings?consent-stub=1', { timeout: 10_000 })
  })

  // spec: SET-021[0], SET-021[2]
  test('a success outcome shows a notification, refreshes accounts, and strips params', async ({
    page,
  }) => {
    let accountListGets = 0
    await page.route('**/api/v1/auth/google/accounts', route => {
      if (route.request().method() === 'GET') accountListGets++
      return corsFulfill(route, { success: true, data: [googleAccount('e2e-google-user')] })
    })
    await mockSyncStates(page, [])

    await page.goto('/settings?auth=success&provider=google')
    await page.waitForLoadState('domcontentloaded')

    // Success indication renders…
    await expect(googleRegion(page).getByText(/connected successfully/i)).toBeVisible({
      timeout: 10_000,
    })
    // …the account list is refetched (initial load + the success refetch)…
    await expect.poll(() => accountListGets, { timeout: 10_000 }).toBeGreaterThanOrEqual(2)
    // …and the one-time params are stripped so refresh does not re-trigger.
    await expect(page).toHaveURL('/settings', { timeout: 10_000 })
  })

  // spec: SET-021[1], SET-021[2]
  test('an error outcome surfaces the failure reason and strips params', async ({ page }) => {
    await mockGoogleAccounts(page, [])
    await mockSyncStates(page, [])

    await page.goto('/settings?auth=error&provider=google&message=exchange_failed')
    await page.waitForLoadState('domcontentloaded')

    // The reason is derived from the redirect's message param — data.
    await expect(googleRegion(page).getByText(/exchange failed/i)).toBeVisible({
      timeout: 10_000,
    })
    await expect(page).toHaveURL('/settings', { timeout: 10_000 })
  })

  // spec: SET-022[0], SET-022[1]
  test('disconnect asks for confirmation and dismissing revokes nothing', async ({ page }) => {
    await mockGoogleAccounts(page, [googleAccount('e2e-google-user')])
    await mockSyncStates(page, [])
    let revokeCalled = false
    await page.route('**/api/v1/auth/google/accounts/*/revoke', route => {
      if (route.request().method() !== 'OPTIONS') revokeCalled = true
      return corsFulfill(route, { success: true, data: { message: 'revoked' } })
    })

    const dialogMessages: string[] = []
    page.on('dialog', dialog => {
      dialogMessages.push(dialog.message())
      return dialog.dismiss()
    })

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    const google = googleRegion(page)
    const disconnect = google.getByRole('button', { name: 'Disconnect e2e-google-user' })
    await expect(disconnect).toBeVisible({ timeout: 10_000 })
    await disconnect.click()

    // The confirmation identifies the account and warns what is revoked.
    await expect.poll(() => dialogMessages.length).toBeGreaterThanOrEqual(1)
    expect(dialogMessages[0]).toContain('e2e-google-user')
    expect(dialogMessages[0]).toMatch(/revoke/i)

    // Dismissed → no revoke request, account still listed.
    await expect(google.getByText('e2e-google-user')).toBeVisible()
    expect(revokeCalled).toBe(false)
  })

  // spec: SET-022[2]
  test('confirming a disconnect revokes the account and the list reflects removal', async ({
    page,
  }) => {
    let accounts: unknown[] = [googleAccount('e2e-google-user')]
    await page.route('**/api/v1/auth/google/accounts', route =>
      corsFulfill(route, { success: true, data: accounts })
    )
    await mockSyncStates(page, [])
    const revokePosts: string[] = []
    await page.route('**/api/v1/auth/google/accounts/*/revoke', route => {
      if (route.request().method() !== 'OPTIONS') {
        revokePosts.push(route.request().url())
        accounts = []
      }
      return corsFulfill(route, { success: true, data: { message: 'revoked' } })
    })

    page.on('dialog', dialog => dialog.accept())

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    const google = googleRegion(page)
    const disconnect = google.getByRole('button', { name: 'Disconnect e2e-google-user' })
    await expect(disconnect).toBeVisible({ timeout: 10_000 })
    await disconnect.click()

    // The revoke request fires for that account…
    await expect
      .poll(() => revokePosts.some(u => u.includes('/accounts/e2e-google-user/revoke')))
      .toBe(true)
    // …the outcome is reported, and the list reflects the removal.
    await expect(google.getByText(/Disconnected e2e-google-user/)).toBeVisible({
      timeout: 10_000,
    })
    await expect(google.getByRole('button', { name: /Connect Google Account/i })).toBeVisible({
      timeout: 10_000,
    })
  })

  // spec: SET-024[0]
  test('per-source sync affordances follow the account scopes', async ({ page }) => {
    // Gmail + Calendar granted, Contacts omitted.
    await mockGoogleAccounts(page, [
      googleAccount('e2e-scoped-user', { scopes: [GMAIL_SCOPE, CALENDAR_SCOPE] }),
    ])
    await mockSyncStates(page, [])

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    const google = googleRegion(page)
    await expect(google.getByText('e2e-scoped-user')).toBeVisible({ timeout: 10_000 })

    await expect(google.getByRole('button', { name: 'Sync Gmail' })).toBeVisible()
    await expect(google.getByRole('button', { name: 'Sync Calendar' })).toBeVisible()
    // The scope the account does not hold gets no affordance.
    await expect(google.getByRole('button', { name: 'Sync Contacts' })).toHaveCount(0)
  })

  // spec: SET-024[1]
  test('the Chat affordance appears when all chat scopes are held', async ({ page }) => {
    await mockGoogleAccounts(page, [
      googleAccount('e2e-chat-user', { scopes: [GMAIL_SCOPE, ...CHAT_SCOPES] }),
    ])
    await mockSyncStates(page, [])

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    const google = googleRegion(page)
    await expect(google.getByText('e2e-chat-user')).toBeVisible({ timeout: 10_000 })
    await expect(google.getByRole('button', { name: 'Sync Chat' })).toBeVisible()
    await expect(google.getByRole('button', { name: /Chat — reconnect required/ })).toHaveCount(0)
  })

  // spec: SET-024[1]
  test('a partial chat grant shows a reconnect prompt instead of the Chat affordance', async ({
    page,
  }) => {
    await mockGoogleAccounts(page, [
      googleAccount('e2e-partial-chat-user', { scopes: [GMAIL_SCOPE, CHAT_SCOPES[0]] }),
    ])
    await mockSyncStates(page, [])

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    const google = googleRegion(page)
    await expect(google.getByText('e2e-partial-chat-user')).toBeVisible({ timeout: 10_000 })
    await expect(google.getByRole('button', { name: /Chat — reconnect required/ })).toBeVisible()
    await expect(google.getByRole('button', { name: 'Sync Chat' })).toHaveCount(0)
  })

  // spec: SET-025[0], SET-025[1]
  test('an auth-errored sync surfaces a reconnect prompt only while the credential is stale', async ({
    page,
  }) => {
    // Account A: sync auth-error NEWER than the credential → reconnect.
    // Account B: credential refreshed AFTER the same error → suppressed.
    await mockGoogleAccounts(page, [
      googleAccount('e2e-stale-user', { updated_at: '2026-01-01T00:00:00Z' }),
      googleAccount('e2e-fresh-user', { updated_at: '2026-06-15T00:00:00Z' }),
    ])
    const errorState = (accountId: string) => ({
      id: `sync-${accountId}`,
      source: 'email',
      account_id: accountId,
      enabled: true,
      status: 'error',
      sync_cursor: null,
      last_sync_at: null,
      last_successful_sync_at: null,
      next_sync_at: null,
      error_count: 1,
      error_message: 'oauth token error: invalid_grant',
      created_at: '2026-06-01T00:00:00Z',
      updated_at: '2026-06-01T00:00:00Z',
    })
    await mockSyncStates(page, [errorState('e2e-stale-user'), errorState('e2e-fresh-user')])

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    const google = googleRegion(page)
    await expect(google.getByText('e2e-stale-user')).toBeVisible({ timeout: 10_000 })
    await expect(google.getByText('e2e-fresh-user')).toBeVisible()

    // Exactly one Reconnect affordance: present for the stale account,
    // suppressed for the refreshed one. (exact: the per-account chat
    // "reconnect required" hint would otherwise substring-match.)
    await expect(google.getByRole('button', { name: 'Reconnect', exact: true })).toHaveCount(1)
  })

  // spec: SET-026[0], SET-026[1], SET-026[2]
  test('the Todoist configuration guides selecting a project and label', async ({ page }) => {
    await mockTodoistAccounts(page, [googleAccount('e2e-todoist-account')])
    await mockSyncStates(page, [])

    let settings: Record<string, string> = {}
    const settingsPatches: Record<string, unknown>[] = []
    await page.route('**/api/v1/todoist/settings', route => {
      if (route.request().method() === 'PATCH') {
        const body = route.request().postDataJSON() as Record<string, string>
        settingsPatches.push(body)
        settings = { ...settings, ...body }
      }
      return corsFulfill(route, { success: true, data: settings })
    })
    await page.route('**/api/v1/todoist/projects', route =>
      corsFulfill(route, {
        success: true,
        data: [
          { id: 'proj-1', name: 'CRM Tasks' },
          { id: 'proj-2', name: 'Inbox' },
        ],
      })
    )
    await page.route('**/api/v1/todoist/labels', route =>
      corsFulfill(route, {
        success: true,
        data: [
          { id: 'label-1', name: 'crm-people' },
          { id: 'label-2', name: 'someday' },
        ],
      })
    )

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    const todoist = todoistRegion(page)
    await expect(todoist.getByText('e2e-todoist-account')).toBeVisible({ timeout: 10_000 })

    // Pickers are populated from the live provider lists.
    const projectSelect = todoist.getByRole('combobox', { name: 'Project' })
    const labelSelect = todoist.getByRole('combobox', { name: 'Label' })
    await expect(projectSelect.locator('option', { hasText: 'CRM Tasks' })).toHaveCount(1)
    await expect(projectSelect.locator('option', { hasText: 'Inbox' })).toHaveCount(1)
    await expect(labelSelect.locator('option', { hasText: 'crm-people' })).toHaveCount(1)

    // Both-required note shows while the pair is incomplete.
    const bothRequired = todoist.getByText(/both a project and label/i)
    await expect(bothRequired).toBeVisible()

    // Selecting a project persists the choice and reports the outcome.
    await projectSelect.selectOption('proj-1')
    await expect.poll(() => settingsPatches.length).toBeGreaterThanOrEqual(1)
    expect(settingsPatches[0]).toMatchObject({ project_id: 'proj-1' })
    await expect(todoist.getByText(/Project updated/i)).toBeVisible({ timeout: 10_000 })

    // Completing the pair clears the both-required note.
    await labelSelect.selectOption('label-1')
    await expect.poll(() => settingsPatches.some(p => p.label_id === 'label-1')).toBe(true)
    await expect(bothRequired).toHaveCount(0, { timeout: 10_000 })
  })

  // spec: TDS-034[0], TDS-034[1]
  test('a manual Todoist task sync can be triggered with its outcome indicated', async ({
    page,
  }) => {
    await mockTodoistAccounts(page, [
      googleAccount('e2e-todoist-account', { scopes: ['data:read_write'] }),
    ])
    await mockSyncStates(page, [])
    await page.route('**/api/v1/todoist/settings', route =>
      corsFulfill(route, { success: true, data: {} })
    )
    await page.route('**/api/v1/todoist/projects', route =>
      corsFulfill(route, { success: true, data: [] })
    )
    await page.route('**/api/v1/todoist/labels', route =>
      corsFulfill(route, { success: true, data: [] })
    )

    let failMode = false
    const triggerBodies: Record<string, unknown>[] = []
    await page.route('**/api/v1/sync/todoist/trigger', route => {
      if (route.request().method() === 'OPTIONS') {
        return route.fulfill({ status: 204, headers: corsHeaders })
      }
      triggerBodies.push(route.request().postDataJSON() as Record<string, unknown>)
      if (failMode) {
        return corsFulfill(
          route,
          { success: false, error: { code: 'INTERNAL', message: 'boom' } },
          500
        )
      }
      return corsFulfill(route, { success: true, data: null })
    })

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    const todoist = todoistRegion(page)
    await expect(todoist.getByText('e2e-todoist-account')).toBeVisible({ timeout: 10_000 })

    // The account's read/write permission state is visible (data-derived
    // from the mocked scopes).
    await expect(todoist.getByText('Read & Write')).toBeVisible()

    // Trigger the sync: the request targets the todoist source with the
    // account id, and success is indicated.
    const syncTasks = todoist.getByRole('button', { name: 'Sync Tasks' })
    await syncTasks.click()
    await expect.poll(() => triggerBodies.length).toBeGreaterThanOrEqual(1)
    expect(triggerBodies[0]).toMatchObject({ account_id: 'e2e-todoist-account' })
    await expect(todoist.getByText(/Todoist sync started/i)).toBeVisible({ timeout: 10_000 })

    // A failing trigger is indicated as a failure.
    failMode = true
    await expect(syncTasks).toBeEnabled()
    await syncTasks.click()
    await expect(todoist.getByText(/Failed to start sync/i)).toBeVisible({ timeout: 10_000 })
  })

  // spec: SET-028[0], SET-028[1], SET-028[2]
  test('the backup surface exports a JSON download and validates an uploaded backup', async ({
    page,
  }) => {
    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    // Export: the affordance streams a JSON backup download (live endpoint).
    const exportResponse = page.waitForResponse(
      resp => resp.url().includes('/api/v1/export') && resp.request().method() === 'POST'
    )
    const downloadPromise = page.waitForEvent('download')
    await page.getByRole('button', { name: /Download Backup/i }).click()
    expect((await exportResponse).status()).toBe(200)
    const download = await downloadPromise
    expect(download.suggestedFilename()).toMatch(/\.json$/)

    // Import: the affordance accepts a backup file…
    const fileInput = page.locator('input[type="file"]')
    await expect(fileInput).toHaveAttribute('accept', '.json')
    await fileInput.setInputFiles({
      name: 'e2e-backup.json',
      mimeType: 'application/json',
      buffer: Buffer.from(JSON.stringify({ version: '1.0', contacts: [] })),
    })

    // …and submitting it fires the import request (result alert dismissed).
    page.on('dialog', dialog => dialog.accept())
    const importRequest = page.waitForRequest(
      req => req.url().includes('/api/v1/import') && req.method() === 'POST'
    )
    await page.getByRole('button', { name: /Validate/i }).click()
    await importRequest

    // The surface communicates that import does not yet modify stored data.
    await expect(page.getByText(/without making changes/i)).toBeVisible()
  })
})
