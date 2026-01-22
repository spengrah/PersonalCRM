import { test, expect } from '@playwright/test'

test.describe('Settings Page @area:settings', () => {
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
