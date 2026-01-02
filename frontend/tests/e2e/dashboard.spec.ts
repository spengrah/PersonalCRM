import { test, expect } from '@playwright/test'

// TEMPORARY: Deliberate failure to verify CI artifact uploads work
// DELETE THIS TEST AFTER VERIFICATION
test('CI artifact upload verification - DELIBERATE FAILURE', async ({ page }) => {
  await page.goto('/dashboard')
  // This will fail and trigger trace/screenshot uploads
  await expect(page.getByText('THIS TEXT DOES NOT EXIST ON PURPOSE')).toBeVisible({
    timeout: 5000,
  })
})

test.describe('Dashboard', () => {
  test('should display dashboard with navigation', async ({ page }) => {
    await page.goto('/')

    // Should redirect to dashboard
    await expect(page).toHaveURL('/dashboard')

    // Wait for page to fully load
    await page.waitForLoadState('networkidle')

    // Should have correct title
    await expect(page).toHaveTitle(/Personal CRM/)

    // Should show navigation with links (use exact: true to avoid matching "View All Contacts")
    await expect(page.getByRole('link', { name: 'Dashboard', exact: true })).toBeVisible()
    await expect(page.getByRole('link', { name: 'Contacts', exact: true })).toBeVisible()

    // Should show "Action Required" heading (the main h2 heading)
    await expect(page.getByRole('heading', { name: 'Action Required', level: 2 })).toBeVisible()
  })

  test('should navigate to contacts from dashboard', async ({ page }) => {
    await page.goto('/dashboard')

    // Click on contacts navigation
    await page.getByRole('link', { name: 'Contacts' }).click()

    // Should navigate to contacts page
    await expect(page).toHaveURL('/contacts')
    // Use level: 2 to target the main h2 heading, not the h3 "No contacts"
    await expect(page.getByRole('heading', { name: 'Contacts', level: 2 })).toBeVisible()
  })

  test('should show dashboard content when loaded', async ({ page }) => {
    await page.goto('/dashboard')

    // Wait for content to load
    await page.waitForLoadState('networkidle')

    // Should show status message (either overdue count or "all caught up")
    const hasOverdue = await page
      .getByText('contacts need your attention')
      .isVisible()
      .catch(() => false)
    const hasCaughtUp = await page
      .getByText("You're all caught up")
      .isVisible()
      .catch(() => false)

    expect(hasOverdue || hasCaughtUp).toBeTruthy()
  })
})
