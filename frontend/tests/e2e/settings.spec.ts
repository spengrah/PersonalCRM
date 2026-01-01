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
})
