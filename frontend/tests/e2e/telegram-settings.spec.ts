import { test, expect, type Page } from '@playwright/test'

// The telegram section talks to the app's own /api/v1/telegram/* endpoints,
// which cannot run live in E2E (MTProto needs real credentials and a real
// account). Every test here route-mocks that boundary — the sanctioned
// technique — and asserts the real surface branches the mocked states drive.

async function mockStatus(page: Page, data: Record<string, unknown>) {
  await page.route('**/api/v1/telegram/auth/status', route =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ success: true, data }),
    })
  )
}

const telegramRegion = (page: Page) => page.getByRole('region', { name: 'Telegram' })

test.describe('Telegram Settings @area:settings', () => {
  // spec: SET-027[0]
  test('shows Telegram section on settings page', async ({ page }) => {
    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    // Telegram section should be visible
    await expect(page.getByRole('heading', { name: 'Telegram', exact: true })).toBeVisible({
      timeout: 10000,
    })
  })

  // spec: SET-027[1]
  test('not-configured backend collapses to configuration guidance', async ({ page }) => {
    // Force the not-configured branch deterministically: the section keys it
    // off a 404 from the status endpoint (telegram routes are not registered
    // when the feature is off, per SET-005).
    await page.route('**/api/v1/telegram/auth/status', route =>
      route.fulfill({
        status: 404,
        contentType: 'application/json',
        body: JSON.stringify({
          success: false,
          error: { code: 'NOT_FOUND', message: 'not found' },
        }),
      })
    )

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    const section = telegramRegion(page)
    await expect(section.getByText('Configuration Required')).toBeVisible({ timeout: 10000 })
    // The behavior mandates naming the configuration the deployment must
    // supply; match the keys loosely so copy rewording survives.
    await expect(section.getByText(/ENABLE_TELEGRAM_SYNC/)).toBeVisible()
    await expect(section.getByText('TELEGRAM_API_ID')).toBeVisible()
    await expect(section.getByText('TELEGRAM_API_HASH')).toBeVisible()
  })

  // spec: TGM-038[0]
  test('auth flow: phone input shows on Connect click', async ({ page }) => {
    await mockStatus(page, { status: 'disconnected', connected: false })

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    await expect(page.getByRole('heading', { name: 'Telegram', exact: true })).toBeVisible({
      timeout: 10000,
    })

    // Click Connect
    await page.getByRole('button', { name: /Connect Telegram/i }).click()

    // Phone input should appear
    await expect(page.getByLabel('Phone Number')).toBeVisible()
    await expect(page.getByRole('button', { name: 'Send Code' })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Cancel' })).toBeVisible()
  })

  // spec: TGM-038[1]
  test('auth flow: phone input → code input transition', async ({ page }) => {
    await mockStatus(page, { status: 'disconnected', connected: false })

    // Mock start auth
    await page.route('**/api/v1/telegram/auth/start', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: {
            auth_token: 'mock-token',
            status: 'awaiting_code',
            code_type: 'app',
            expires_in: 300,
          },
        }),
      })
    )

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    await expect(page.getByRole('heading', { name: 'Telegram', exact: true })).toBeVisible({
      timeout: 10000,
    })

    // Start auth flow, asserting the start request carries the entered phone
    // (network-param proof that the input is wired to the API).
    await page.getByRole('button', { name: /Connect Telegram/i }).click()
    await page.getByLabel('Phone Number').fill('+15551234567')
    const startRequest = page.waitForRequest(
      req => req.url().includes('/api/v1/telegram/auth/start') && req.method() === 'POST'
    )
    await page.getByRole('button', { name: 'Send Code' }).click()
    const req = await startRequest
    expect(req.postDataJSON()).toMatchObject({ phone_number: '+15551234567' })

    // Should show code input, reflecting the mocked delivery channel (app).
    await expect(page.getByText(/code was sent to your Telegram app/i)).toBeVisible({
      timeout: 5000,
    })
    await expect(page.getByLabel('Verification Code')).toBeVisible()
  })

  // spec: TGM-038[2]
  test('auth flow: code → connected transition', async ({ page }) => {
    await mockStatus(page, { status: 'disconnected', connected: false })

    // Mock start auth
    await page.route('**/api/v1/telegram/auth/start', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: {
            auth_token: 'mock-token',
            status: 'awaiting_code',
            code_type: 'app',
            expires_in: 300,
          },
        }),
      })
    )

    // Mock verify code → connected
    await page.route('**/api/v1/telegram/auth/verify-code', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: { status: 'connected', username: 'testuser', user_id: 12345 },
        }),
      })
    )

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    await expect(page.getByRole('heading', { name: 'Telegram', exact: true })).toBeVisible({
      timeout: 10000,
    })

    // Complete auth flow
    await page.getByRole('button', { name: /Connect Telegram/i }).click()
    await page.getByLabel('Phone Number').fill('+15551234567')
    await page.getByRole('button', { name: 'Send Code' }).click()

    await expect(page.getByLabel('Verification Code')).toBeVisible({ timeout: 5000 })
    await page.getByLabel('Verification Code').fill('12345')
    await page.getByRole('button', { name: 'Verify' }).click()

    // A valid code connects: the success indication carries the verify
    // response's username, and the view switches to the connected state
    // (its disconnect affordance appears). The connected VIEW's username
    // display is TGM-038[4], proven by the pre-connected-status test.
    await expect(page.getByText(/Connected.*@testuser/)).toBeVisible({ timeout: 5000 })
    await expect(page.getByRole('button', { name: /Disconnect/i })).toBeVisible()
  })

  // spec: TGM-038[2], TGM-038[3]
  test('auth flow: code → 2FA → connected transition', async ({ page }) => {
    await mockStatus(page, { status: 'disconnected', connected: false })

    await page.route('**/api/v1/telegram/auth/start', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: {
            auth_token: 'mock-token',
            status: 'awaiting_code',
            code_type: 'app',
            expires_in: 300,
          },
        }),
      })
    )

    // Mock verify code → needs 2FA
    await page.route('**/api/v1/telegram/auth/verify-code', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: { status: 'awaiting_password' },
        }),
      })
    )

    // Mock verify password → connected
    await page.route('**/api/v1/telegram/auth/verify-password', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: { status: 'connected', username: 'testuser2fa', user_id: 67890 },
        }),
      })
    )

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    await expect(page.getByRole('heading', { name: 'Telegram', exact: true })).toBeVisible({
      timeout: 10000,
    })

    // Start and enter code
    await page.getByRole('button', { name: /Connect Telegram/i }).click()
    await page.getByLabel('Phone Number').fill('+15551234567')
    await page.getByRole('button', { name: 'Send Code' }).click()
    await expect(page.getByLabel('Verification Code')).toBeVisible({ timeout: 5000 })
    await page.getByLabel('Verification Code').fill('12345')
    await page.getByRole('button', { name: 'Verify' }).click()

    // Should show 2FA password step (the awaiting_password branch)
    await expect(page.getByLabel('2FA Password')).toBeVisible({ timeout: 5000 })

    // Enter password
    await page.getByLabel('2FA Password').fill('mypassword')
    await page.getByRole('button', { name: 'Verify' }).click()

    // A valid password connects: success indication plus the view's
    // disconnect affordance.
    await expect(page.getByText(/Connected.*@testuser2fa/)).toBeVisible({ timeout: 5000 })
    await expect(page.getByRole('button', { name: /Disconnect/i })).toBeVisible()
  })

  // spec: TGM-038[4], TGM-039[0], TGM-039[2]
  test('shows connected state with username', async ({ page }) => {
    // Mock status as connected
    await mockStatus(page, {
      status: 'connected',
      connected: true,
      username: 'existinguser',
      phone_number: '+15559876543',
    })

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    await expect(page.getByRole('heading', { name: 'Telegram', exact: true })).toBeVisible({
      timeout: 10000,
    })

    // Identity from the mocked status is reflected in the section.
    await expect(page.getByText(/Connected.*@existinguser/)).toBeVisible({ timeout: 5000 })
    await expect(page.getByText('+15559876543')).toBeVisible()
    await expect(page.getByRole('button', { name: /Disconnect/i })).toBeVisible()
  })

  // spec: TGM-039[2]
  test('disconnect returns the section to the disconnected state', async ({ page }) => {
    // One handler covers DELETE /auth and GET /auth/status: the glob matches
    // both, so branch on method + path and route.fallback() anything else.
    let disconnected = false
    await page.route('**/api/v1/telegram/auth**', route => {
      const req = route.request()
      const path = new URL(req.url()).pathname
      if (req.method() === 'DELETE' && path.endsWith('/telegram/auth')) {
        disconnected = true
        return route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({ success: true, data: { status: 'disconnected' } }),
        })
      }
      if (req.method() === 'GET' && path.endsWith('/auth/status')) {
        return route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({
            success: true,
            data: disconnected
              ? { status: 'disconnected', connected: false }
              : {
                  status: 'connected',
                  connected: true,
                  username: 'disconnectme',
                  phone_number: '+15550001111',
                },
          }),
        })
      }
      return route.fallback()
    })
    await page.route('**/api/v1/telegram/chats', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ success: true, data: [] }),
      })
    )

    // The disconnect affordance confirms via a native dialog.
    page.on('dialog', dialog => dialog.accept())

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByText(/Connected.*@disconnectme/)).toBeVisible({ timeout: 10000 })

    const deleteRequest = page.waitForRequest(
      req => req.method() === 'DELETE' && req.url().includes('/api/v1/telegram/auth')
    )
    await page.getByRole('button', { name: /Disconnect/i }).click()
    await deleteRequest

    // The section returns to the disconnected state (connect affordance back).
    await expect(page.getByRole('button', { name: /Connect Telegram/i })).toBeVisible({
      timeout: 5000,
    })
  })

  // spec: TGM-040[0], TGM-040[1]
  test('shows error on invalid code', async ({ page }) => {
    await mockStatus(page, { status: 'disconnected', connected: false })

    await page.route('**/api/v1/telegram/auth/start', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: {
            auth_token: 'mock-token',
            status: 'awaiting_code',
            code_type: 'app',
            expires_in: 300,
          },
        }),
      })
    )

    // Mock verify code → 401 error
    await page.route('**/api/v1/telegram/auth/verify-code', route =>
      route.fulfill({
        status: 401,
        contentType: 'application/json',
        body: JSON.stringify({
          success: false,
          error: { code: 'UNAUTHORIZED', message: 'Invalid verification code' },
        }),
      })
    )

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    await expect(page.getByRole('heading', { name: 'Telegram', exact: true })).toBeVisible({
      timeout: 10000,
    })

    await page.getByRole('button', { name: /Connect Telegram/i }).click()
    await page.getByLabel('Phone Number').fill('+15551234567')
    await page.getByRole('button', { name: 'Send Code' }).click()

    await expect(page.getByLabel('Verification Code')).toBeVisible({ timeout: 5000 })
    await page.getByLabel('Verification Code').fill('99999')
    await page.getByRole('button', { name: 'Verify' }).click()

    // The rejection reason (the mocked error payload's message) is surfaced,
    // and the flow stays on the code step so the user can retry.
    await expect(page.getByText(/Invalid verification code/i)).toBeVisible({ timeout: 5000 })
    await expect(page.getByLabel('Verification Code')).toBeVisible()
  })

  // spec: TGM-041[0]
  test('group chat management: shows chat list', async ({ page }) => {
    await mockStatus(page, { status: 'connected', connected: true, username: 'testuser' })

    await page.route('**/api/v1/telegram/chats', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: [
            {
              telegram_chat_id: -100111,
              chat_title: 'Close Friends',
              chat_type: 'group',
              member_count: 5,
              status: 'auto',
              effective_tracked: true,
            },
            {
              telegram_chat_id: -100222,
              chat_title: 'Work Team',
              chat_type: 'group',
              member_count: 25,
              status: 'auto',
              effective_tracked: false,
            },
          ],
        }),
      })
    )

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    const section = telegramRegion(page)
    await expect(section.getByText('Close Friends')).toBeVisible({ timeout: 10000 })
    await expect(section.getByText('Work Team')).toBeVisible()
    await expect(section.getByText('5 members').first()).toBeVisible()
    await expect(section.getByText('25 members').first()).toBeVisible()
  })

  // spec: TGM-041[1]
  test('group chat management: empty state', async ({ page }) => {
    await mockStatus(page, { status: 'connected', connected: true, username: 'testuser' })

    await page.route('**/api/v1/telegram/chats', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ success: true, data: [] }),
      })
    )

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    await expect(telegramRegion(page).getByText(/No group chats/i)).toBeVisible({
      timeout: 10000,
    })
  })

  // spec: TGM-039[1]
  test('group chat management: backfill progress', async ({ page }) => {
    await mockStatus(page, {
      status: 'connected',
      connected: true,
      username: 'testuser',
      backfill_in_progress: true,
      backfill_total: 42,
      backfill_completed: 15,
    })

    await page.route('**/api/v1/telegram/chats', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ success: true, data: [] }),
      })
    )

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    // The 15/42 figures are the mocked backfill progress reflected in the UI.
    await expect(page.getByText(/Syncing messages.*15\/42/)).toBeVisible({ timeout: 10000 })
  })

  // spec: TGM-041[2]
  test('group chat management: toggle auto to ignored', async ({ page }) => {
    let currentStatus = 'auto'
    let currentTracked = true

    await mockStatus(page, { status: 'connected', connected: true, username: 'testuser' })

    await page.route('**/api/v1/telegram/chats**', route => {
      if (route.request().method() === 'GET') {
        return route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({
            success: true,
            data: [
              {
                telegram_chat_id: -100111,
                chat_title: 'Toggle Test Group',
                chat_type: 'group',
                member_count: 5,
                status: currentStatus,
                effective_tracked: currentTracked,
              },
            ],
          }),
        })
      }
      // PATCH — update mock state
      const body = route.request().postDataJSON()
      currentStatus = body.status
      currentTracked = body.status !== 'ignored'
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: {
            telegram_chat_id: -100111,
            chat_title: 'Toggle Test Group',
            chat_type: 'group',
            member_count: 5,
            status: currentStatus,
            effective_tracked: currentTracked,
          },
        }),
      })
    })

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    await expect(page.getByText('Toggle Test Group')).toBeVisible({ timeout: 10000 })

    const select = page.getByRole('combobox', { name: 'Tracking for Toggle Test Group' })
    await expect(select).toHaveValue('auto')

    // Toggle to "Ignored", asserting the persistence request carries the choice.
    const patchRequest = page.waitForRequest(
      req => req.method() === 'PATCH' && req.url().includes('/api/v1/telegram/chats/')
    )
    await select.selectOption('ignored')
    expect((await patchRequest).postDataJSON()).toMatchObject({ status: 'ignored' })

    // After the mutation + refetch the list reflects the persisted choice.
    await expect(select).toHaveValue('ignored', { timeout: 5000 })
  })

  // spec: TGM-041[2]
  test('group chat management: toggle auto to tracked', async ({ page }) => {
    let currentStatus = 'auto'

    await mockStatus(page, { status: 'connected', connected: true, username: 'testuser' })

    await page.route('**/api/v1/telegram/chats**', route => {
      if (route.request().method() === 'GET') {
        return route.fulfill({
          status: 200,
          contentType: 'application/json',
          body: JSON.stringify({
            success: true,
            data: [
              {
                telegram_chat_id: -100333,
                chat_title: 'Large Work Group',
                chat_type: 'group',
                member_count: 50,
                status: currentStatus,
                effective_tracked: currentStatus === 'tracked',
              },
            ],
          }),
        })
      }
      const body = route.request().postDataJSON()
      currentStatus = body.status
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: {
            telegram_chat_id: -100333,
            chat_title: 'Large Work Group',
            chat_type: 'group',
            member_count: 50,
            status: currentStatus,
            effective_tracked: currentStatus === 'tracked',
          },
        }),
      })
    })

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    await expect(page.getByText('Large Work Group')).toBeVisible({ timeout: 10000 })

    const select = page.getByRole('combobox', { name: 'Tracking for Large Work Group' })
    await expect(select).toHaveValue('auto')

    // Toggle to "Tracked", asserting the persistence request carries the choice.
    const patchRequest = page.waitForRequest(
      req => req.method() === 'PATCH' && req.url().includes('/api/v1/telegram/chats/')
    )
    await select.selectOption('tracked')
    expect((await patchRequest).postDataJSON()).toMatchObject({ status: 'tracked' })

    // After mutation + refetch, the list reflects the persisted choice.
    await expect(select).toHaveValue('tracked', { timeout: 5000 })
  })
})
