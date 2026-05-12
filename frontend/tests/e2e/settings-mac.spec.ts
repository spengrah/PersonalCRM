import { test, expect } from '@playwright/test'

test.describe('Settings — Mac Daemon @area:settings', () => {
  test('renders empty-state when no Mac hosts are paired', async ({ page }) => {
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

  test('opens pairing modal with a token when Pair new Mac is clicked', async ({ page }) => {
    await page.goto('/settings/mac', { waitUntil: 'domcontentloaded' })
    // Wait for the empty-state heading to confirm React has hydrated
    // before clicking — under turbopack dev compile the click handler
    // can miss if dispatched against an unhydrated tree.
    await expect(page.getByRole('heading', { name: 'Mac Daemon' })).toBeVisible({
      timeout: 10_000,
    })
    const pairButton = page.getByRole('button', { name: 'Pair new Mac' })
    await expect(pairButton).toBeVisible()

    // Wait for the pairing-token POST to fire so flakes on hydration
    // races surface as response failures, not silent click misses.
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
})
