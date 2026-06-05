import { test, expect } from '@playwright/test'

test.describe('Settings Page @area:settings', () => {
  test('should display Todoist accounts section', async ({ page }) => {
    await page.goto('/settings', { waitUntil: 'networkidle' })
    await page.reload({ waitUntil: 'networkidle' })

    // Check Todoist section heading is visible (in the card header)
    await expect(page.getByRole('heading', { name: 'Todoist', exact: true })).toBeVisible()

    // Check for either Connect button or configuration instructions
    // (depends on whether backend has TODOIST_* env vars configured)
    const connectButton = page.getByRole('button', { name: /Connect Todoist/i })
    const configMessage = page.getByText(/configure your Todoist OAuth credentials/i)

    // One of these should be visible
    const hasConnectButton = await connectButton.isVisible().catch(() => false)
    const hasConfigMessage = await configMessage.isVisible().catch(() => false)

    expect(hasConnectButton || hasConfigMessage).toBe(true)
  })

  test('should display Google Accounts section with optional Gmail sync badge', async ({
    page,
  }) => {
    await page.goto('/settings', { waitUntil: 'networkidle' })
    await page.reload({ waitUntil: 'networkidle' })

    // Google Accounts section heading should always render
    await expect(page.getByRole('heading', { name: 'Google Accounts', exact: true })).toBeVisible()

    // The Gmail sync badge only appears when an account with the gmail.readonly
    // scope is connected (depends on backend GOOGLE_* env vars + a real account).
    // If present, its refresh button must be accessible and not permanently disabled.
    const gmailBadge = page.locator('div').filter({ hasText: /^Gmail/ })
    const hasGmailBadge = await gmailBadge
      .first()
      .isVisible()
      .catch(() => false)

    if (hasGmailBadge) {
      const refreshButton = gmailBadge.first().getByRole('button')
      await expect(refreshButton).toBeVisible()
      // Button may be transiently disabled while syncing, but not absent/hidden.
      await expect(refreshButton).toBeEnabled({ timeout: 5000 })
    }
  })

  test('should display optional Google Chat sync badge or reconnect hint', async ({ page }) => {
    await page.goto('/settings', { waitUntil: 'networkidle' })
    await page.reload({ waitUntil: 'networkidle' })

    await expect(page.getByRole('heading', { name: 'Google Accounts', exact: true })).toBeVisible()

    // Per connected Google account, the Chat surface is conditional: a "Chat"
    // SyncBadge when the account carries chat.spaces.readonly, OR a
    // "Chat — reconnect required" hint when it does not. Both depend on backend
    // GOOGLE_* env vars + a real connected account, so this is a
    // conditional-presence test (like the Gmail-badge test above).
    const chatBadge = page.locator('div').filter({ hasText: /^Chat/ })
    const hasChatBadge = await chatBadge
      .first()
      .isVisible()
      .catch(() => false)

    if (hasChatBadge) {
      const refreshButton = chatBadge.first().getByRole('button')
      await expect(refreshButton).toBeVisible()
      await expect(refreshButton).toBeEnabled({ timeout: 5000 })
    } else {
      // No scoped account: if any Google account is connected, the reconnect
      // hint may be present. Its visibility is account-dependent, so only assert
      // it is clickable WHEN present (never required).
      const reconnectHint = page.getByRole('button', { name: /Chat — reconnect required/ })
      const hasHint = await reconnectHint
        .first()
        .isVisible()
        .catch(() => false)
      if (hasHint) {
        await expect(reconnectHint.first()).toBeEnabled({ timeout: 5000 })
      }
    }
  })

  test('should display settings page with export and import sections', async ({ page }) => {
    await page.goto('/settings')

    // Check page loads correctly
    await expect(page.getByRole('heading', { name: 'Settings' })).toBeVisible()

    // Check export section is visible
    await expect(page.getByRole('heading', { name: 'Export Data' })).toBeVisible()
    await expect(page.getByRole('button', { name: /Download Backup/i })).toBeVisible()

    // Check import section is visible
    await expect(page.getByRole('heading', { name: 'Import Data' })).toBeVisible()

    // Check file input is present
    const fileInput = page.locator('input[type="file"]')
    await expect(fileInput).toBeVisible()
    await expect(fileInput).toHaveAttribute('accept', '.json')
  })

  test('should have consistent form field styling', async ({ page }) => {
    await page.goto('/settings')

    // File input should have consistent styling classes
    const fileInput = page.locator('input[type="file"]')
    await expect(fileInput).toHaveClass(/rounded-md/)
    await expect(fileInput).toHaveClass(/border/)
    await expect(fileInput).toHaveClass(/shadow-sm/)
  })

  test('should display system information section with version info', async ({ page }) => {
    await page.goto('/settings')

    // Check System Information section is visible
    await expect(page.getByRole('heading', { name: 'System Information' })).toBeVisible()

    // Check Version field exists
    await expect(page.getByText('Version')).toBeVisible()

    // In test environment without NEXT_PUBLIC_BUILD_VERSION, should show "Personal CRM (dev)"
    // Note: Testing with a valid version/commit hash is not practical in E2E because:
    // 1. The env var is baked in at build time, not runtime
    // 2. We'd need to rebuild the app with specific values for each test run
    // The fallback logic is verified here.
    const versionValue = page.locator('p.text-gray-600:has-text("Version")').locator('+ p')
    await expect(versionValue).toHaveText('Personal CRM (dev)')

    // Verify no GitHub link is rendered when there's no commit hash
    await expect(versionValue.locator('a')).not.toBeVisible()
  })
})
