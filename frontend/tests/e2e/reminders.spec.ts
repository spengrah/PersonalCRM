import { test, expect } from '@playwright/test'

test.describe('Reminders Page', () => {
  test('should display reminders page with search and filters', async ({ page }) => {
    await page.goto('/reminders')

    // Check page loads correctly
    await expect(page.getByRole('heading', { name: 'Reminders' })).toBeVisible()

    // Check search input is visible and functional
    const searchInput = page.getByPlaceholder('Search reminders...')
    await expect(searchInput).toBeVisible()

    // Test search input accepts text
    await searchInput.fill('test search')
    await expect(searchInput).toHaveValue('test search')

    // Check filter tabs are visible
    await expect(page.getByRole('button', { name: 'All' })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Due' })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Completed' })).toBeVisible()
  })

  test('should have consistent search input styling', async ({ page }) => {
    await page.goto('/reminders')

    // Search input should have consistent styling
    const searchInput = page.getByPlaceholder('Search reminders...')
    await expect(searchInput).toHaveClass(/rounded-md/)
    await expect(searchInput).toHaveClass(/border/)
    await expect(searchInput).toHaveClass(/shadow-sm/)
    await expect(searchInput).toHaveClass(/transition-colors/)
  })

  test('should open new reminder form', async ({ page }) => {
    await page.goto('/reminders')

    // Click new reminder button
    const newReminderButton = page.getByRole('button', { name: /New Reminder/i })
    await expect(newReminderButton).toBeVisible()
    await newReminderButton.click()

    // Check form appears with expected fields
    await expect(page.getByLabel('Title')).toBeVisible()
    await expect(page.getByLabel('Due Date')).toBeVisible()
  })
})
