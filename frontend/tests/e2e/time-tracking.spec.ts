import { test, expect } from '@playwright/test'

test.describe('Time Tracking Page', () => {
  test('should display time tracking page', async ({ page }) => {
    await page.goto('/time-tracking')

    // Check page loads correctly
    await expect(page.getByRole('heading', { name: 'Time Tracking' })).toBeVisible()

    // Check action buttons are visible
    await expect(page.getByRole('button', { name: /Start New Timer/i })).toBeVisible()
    await expect(page.getByRole('button', { name: /Add Manual Entry/i })).toBeVisible()
  })

  test('should show timer form with consistent styling', async ({ page }) => {
    await page.goto('/time-tracking')

    // Click start new timer
    await page.getByRole('button', { name: /Start New Timer/i }).click()

    // Check form appears
    const descriptionLabel = page.getByText('Description *', { exact: true })
    await expect(descriptionLabel).toBeVisible()

    // Check description input has consistent styling
    const descriptionInput = page.locator('#timer-description')
    await expect(descriptionInput).toBeVisible()
    await expect(descriptionInput).toHaveClass(/rounded-md/)
    await expect(descriptionInput).toHaveClass(/border/)
    await expect(descriptionInput).toHaveClass(/shadow-sm/)
    await expect(descriptionInput).toHaveClass(/transition-colors/)

    // Check project input
    const projectInput = page.locator('#timer-project')
    await expect(projectInput).toBeVisible()

    // Check cancel button works
    await page.getByRole('button', { name: 'Cancel' }).click()
    await expect(descriptionInput).not.toBeVisible()
  })

  test('should show manual entry form with consistent styling', async ({ page }) => {
    await page.goto('/time-tracking')

    // Click add manual entry
    await page.getByRole('button', { name: /Add Manual Entry/i }).click()

    // Check form appears with all expected fields
    await expect(page.getByText('Description *', { exact: true }).first()).toBeVisible()

    // Check inputs exist and have consistent styling
    const descriptionInput = page.locator('#manual-description')
    await expect(descriptionInput).toBeVisible()
    await expect(descriptionInput).toHaveClass(/rounded-md/)
    await expect(descriptionInput).toHaveClass(/transition-colors/)

    // Check date/time inputs
    const startDateInput = page.locator('#start-date')
    await expect(startDateInput).toBeVisible()
    await expect(startDateInput).toHaveClass(/rounded-md/)
    await expect(startDateInput).toHaveClass(/transition-colors/)

    const startTimeInput = page.locator('#start-time')
    await expect(startTimeInput).toBeVisible()

    // Check duration toggle checkbox
    const durationCheckbox = page.locator('#use-duration')
    await expect(durationCheckbox).toBeVisible()

    // Toggle to duration mode
    await durationCheckbox.check()
    await expect(page.locator('#duration-hours')).toBeVisible()
    await expect(page.locator('#duration-minutes')).toBeVisible()

    // Toggle back to end time mode
    await durationCheckbox.uncheck()
    await expect(page.locator('#end-date')).toBeVisible()
    await expect(page.locator('#end-time')).toBeVisible()
  })
})
