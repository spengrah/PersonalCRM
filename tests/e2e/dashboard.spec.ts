import { test, expect } from '@playwright/test'

test.describe('Dashboard', () => {
  test('should display dashboard with navigation', async ({ page }) => {
    await page.goto('/')
    
    // Should redirect to dashboard
    await expect(page).toHaveURL('/dashboard')
    
    // Should have correct title
    await expect(page).toHaveTitle(/Personal CRM/)
    
    // Should show navigation
    await expect(page.getByText('Personal CRM')).toBeVisible()
    await expect(page.getByText('Dashboard')).toBeVisible()
    
    // Should show dashboard content
    await expect(page.getByText('Dashboard')).toBeVisible()
    await expect(page.getByText('Your personal CRM overview and reminders')).toBeVisible()
  })

  test('should navigate to contacts from dashboard', async ({ page }) => {
    await page.goto('/dashboard')
    
    // Click on contacts navigation
    await page.getByRole('link', { name: 'Contacts' }).click()
    
    // Should navigate to contacts page
    await expect(page).toHaveURL('/contacts')
    await expect(page.getByText('Contacts')).toBeVisible()
  })

  test('should show stats cards when data is available', async ({ page }) => {
    await page.goto('/dashboard')
    
    // Wait for potential stats to load
    await page.waitForTimeout(1000)
    
    // Should show some form of content (either stats or empty state)
    const hasStats = await page.getByText('Total Reminders').isVisible().catch(() => false)
    const hasEmptyState = await page.getByText('All caught up!').isVisible().catch(() => false)
    
    expect(hasStats || hasEmptyState).toBeTruthy()
  })
})

