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

    await page.getByRole('button', { name: 'Pair new Mac' }).click()

    // Modal is rendered as a dialog with the matching aria-label.
    const dialog = page.getByRole('dialog', { name: 'Pair new Mac' })
    await expect(dialog).toBeVisible({ timeout: 5_000 })

    // The token is shown in a code block (or "Generating..." if the
    // request is in-flight; we wait for the code element to appear).
    const tokenCode = page.getByTestId('pairing-token-value')
    await expect(tokenCode).toBeVisible({ timeout: 10_000 })
    await expect(tokenCode).not.toBeEmpty()

    // Close button dismisses the modal.
    await dialog.getByRole('button', { name: 'Close' }).click()
    await expect(dialog).not.toBeVisible()
  })
})
