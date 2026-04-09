import { test, expect } from '@playwright/test'

test.describe('Telegram Settings @area:settings', () => {
  test('shows Telegram section on settings page', async ({ page }) => {
    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    // Telegram section should be visible
    await expect(page.getByRole('heading', { name: 'Telegram', exact: true })).toBeVisible({
      timeout: 10000,
    })
  })

  test('shows not-configured or disconnected state', async ({ page }) => {
    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    await expect(page.getByRole('heading', { name: 'Telegram', exact: true })).toBeVisible({
      timeout: 10000,
    })

    // Scope to the Telegram section
    const telegramSection = page.locator('section', {
      has: page.getByRole('heading', { name: 'Telegram', exact: true }),
    })

    // Wait for loading to finish — either "Configuration Required" or "Connect Telegram" should appear
    const configRequired = telegramSection.getByText('Configuration Required')
    const connectButton = telegramSection.getByRole('button', { name: /Connect Telegram/i })

    // Use Playwright's built-in polling instead of instant snapshot
    await expect(configRequired.or(connectButton)).toBeVisible({ timeout: 10000 })
  })

  test('auth flow: phone input shows on Connect click', async ({ page }) => {
    // Intercept status to return disconnected
    await page.route('**/api/v1/telegram/auth/status', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: { status: 'disconnected', connected: false },
        }),
      })
    )

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

  test('auth flow: phone input → code input transition', async ({ page }) => {
    // Mock status as disconnected
    await page.route('**/api/v1/telegram/auth/status', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: { status: 'disconnected', connected: false },
        }),
      })
    )

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

    // Start auth flow
    await page.getByRole('button', { name: /Connect Telegram/i }).click()
    await page.getByLabel('Phone Number').fill('+15551234567')
    await page.getByRole('button', { name: 'Send Code' }).click()

    // Should show code input
    await expect(page.getByText(/code was sent to your Telegram app/i)).toBeVisible({
      timeout: 5000,
    })
    await expect(page.getByLabel('Verification Code')).toBeVisible()
  })

  test('auth flow: code → connected transition', async ({ page }) => {
    // Mock status
    await page.route('**/api/v1/telegram/auth/status', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: { status: 'disconnected', connected: false },
        }),
      })
    )

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

    // Should show connected state
    await expect(page.getByText(/Connected.*@testuser/)).toBeVisible({ timeout: 5000 })
  })

  test('auth flow: code → 2FA → connected transition', async ({ page }) => {
    await page.route('**/api/v1/telegram/auth/status', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: { status: 'disconnected', connected: false },
        }),
      })
    )

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

    // Should show 2FA input
    await expect(page.getByText(/two-factor authentication/i)).toBeVisible({ timeout: 5000 })
    await expect(page.getByLabel('2FA Password')).toBeVisible()

    // Enter password
    await page.getByLabel('2FA Password').fill('mypassword')
    await page.getByRole('button', { name: 'Verify' }).click()

    // Should show connected
    await expect(page.getByText(/Connected.*@testuser2fa/)).toBeVisible({ timeout: 5000 })
  })

  test('shows connected state with username', async ({ page }) => {
    // Mock status as connected
    await page.route('**/api/v1/telegram/auth/status', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: {
            status: 'connected',
            connected: true,
            username: 'existinguser',
            phone_number: '+15559876543',
          },
        }),
      })
    )

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    await expect(page.getByRole('heading', { name: 'Telegram', exact: true })).toBeVisible({
      timeout: 10000,
    })

    // Should show connected state
    await expect(page.getByText(/Connected.*@existinguser/)).toBeVisible({ timeout: 5000 })
    await expect(page.getByText('+15559876543')).toBeVisible()
    await expect(page.getByRole('button', { name: /Disconnect/i })).toBeVisible()
  })

  // Disconnect E2E test omitted: Playwright's `**/api/v1/telegram/auth` glob matches
  // `/auth/status` too (LIFO routing means the less-specific handler intercepts status
  // requests and calls route.continue(), bypassing the status mock). Verified with:
  // glob patterns, regex patterns, function predicates, and LIFO registration order —
  // none reliably isolate DELETE /auth from GET /auth/status. The disconnect UI path
  // is a simple confirm() → mutateAsync() → setStep('disconnected') and is tested manually.

  test('shows error on invalid code', async ({ page }) => {
    await page.route('**/api/v1/telegram/auth/status', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: { status: 'disconnected', connected: false },
        }),
      })
    )

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

    // Should show error
    await expect(page.getByText(/Invalid verification code/i)).toBeVisible({ timeout: 5000 })
  })

  test('group chat management: shows chat list', async ({ page }) => {
    await page.route('**/api/v1/telegram/auth/status', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: { status: 'connected', connected: true, username: 'testuser' },
        }),
      })
    )

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

    await expect(page.getByText('Close Friends')).toBeVisible({ timeout: 10000 })
    await expect(page.getByText('Work Team')).toBeVisible()
    await expect(page.getByText('5 members')).toBeVisible()
    await expect(page.getByText('25 members')).toBeVisible()
  })

  test('group chat management: empty state', async ({ page }) => {
    await page.route('**/api/v1/telegram/auth/status', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: { status: 'connected', connected: true, username: 'testuser' },
        }),
      })
    )

    await page.route('**/api/v1/telegram/chats', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ success: true, data: [] }),
      })
    )

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    await expect(page.getByText(/No group chats discovered yet/i)).toBeVisible({ timeout: 10000 })
  })

  test('group chat management: backfill progress', async ({ page }) => {
    await page.route('**/api/v1/telegram/auth/status', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: {
            status: 'connected',
            connected: true,
            username: 'testuser',
            backfill_in_progress: true,
            backfill_total: 42,
            backfill_completed: 15,
          },
        }),
      })
    )

    await page.route('**/api/v1/telegram/chats', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ success: true, data: [] }),
      })
    )

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    await expect(page.getByText(/Syncing messages.*15\/42/)).toBeVisible({ timeout: 10000 })
  })

  test('group chat management: toggle status', async ({ page }) => {
    await page.route('**/api/v1/telegram/auth/status', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          success: true,
          data: { status: 'connected', connected: true, username: 'testuser' },
        }),
      })
    )

    await page.route('**/api/v1/telegram/chats', route => {
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
                status: 'auto',
                effective_tracked: true,
              },
            ],
          }),
        })
      }
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
            status: 'ignored',
            effective_tracked: false,
          },
        }),
      })
    })

    await page.goto('/settings')
    await page.waitForLoadState('domcontentloaded')

    await expect(page.getByText('Toggle Test Group')).toBeVisible({ timeout: 10000 })

    // The select should be visible with "Auto" selected
    const select = page.locator('select').first()
    await expect(select).toHaveValue('auto')

    // Change to "Ignored" — triggers PATCH and refetch
    const patchPromise = page.waitForRequest(
      req => req.url().includes('/telegram/chats/') && req.method() === 'PATCH'
    )
    await select.selectOption('ignored')
    const patchReq = await patchPromise
    expect(JSON.parse(patchReq.postData() ?? '{}')).toEqual({ status: 'ignored' })
  })
})
