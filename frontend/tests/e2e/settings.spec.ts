import { test, expect } from '@playwright/test'

test.describe('Settings Page', () => {
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

  test('should display system information section with build info', async ({ page }) => {
    await page.goto('/settings')

    // Check System Information section is visible
    await expect(page.getByRole('heading', { name: 'System Information' })).toBeVisible()

    // Check Build field exists
    await expect(page.getByText('Build')).toBeVisible()

    // In test environment without NEXT_PUBLIC_COMMIT_HASH, should show "Development"
    // Note: Testing with a valid commit hash is not practical in E2E because:
    // 1. The env var is baked in at build time, not runtime
    // 2. We'd need to rebuild the app with a specific hash for each test run
    // 3. The regex validation logic is simple enough that unit-level verification suffices
    const buildValue = page.locator('p.text-gray-600:has-text("Build")').locator('+ p')
    await expect(buildValue).toContainText('Development')

    // Verify no link is rendered (since no valid hash)
    await expect(buildValue.locator('a')).not.toBeVisible()
  })
})
