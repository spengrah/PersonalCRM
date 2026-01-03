import { test, expect } from '@playwright/test'

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

  test('marking contact as contacted updates dashboard immediately without navigation', async ({
    page,
  }) => {
    // Navigate to dashboard
    await page.goto('/dashboard')
    await page.waitForLoadState('networkidle')

    // Check if there are any overdue contacts
    const hasOverdueContacts = await page
      .getByText(/contacts need your attention/)
      .isVisible()
      .catch(() => false)

    // Skip test if no overdue contacts exist (nothing to test)
    test.skip(!hasOverdueContacts, 'No overdue contacts available to test')

    // Get the initial overdue count
    const statusText = page.getByText(/contacts need your attention/)
    const initialText = await statusText.textContent()
    const initialCount = parseInt(initialText?.match(/(\d+)/)?.[1] || '0', 10)

    // Find the first "Mark as Contacted" button on the dashboard
    const markContactedButton = page.getByRole('button', { name: /Mark as Contacted/i }).first()
    await expect(markContactedButton).toBeVisible()

    // Get the contact name from the card (for verification later)
    const contactCard = page.locator('div.rounded-lg.shadow-sm.border').first()
    const contactNameElement = contactCard.locator('h3').first()
    const contactName = await contactNameElement.textContent()

    // Click "Mark as Contacted"
    await markContactedButton.click()

    // Wait for the mutation to complete and the card to disappear
    // The contact should no longer be overdue, so it should vanish from the dashboard
    if (contactName) {
      await expect(page.getByRole('heading', { name: contactName })).not.toBeVisible({
        timeout: 5000,
      })
    }

    // Verify the dashboard updated (count decreased or showing "all caught up")
    const hasAllCaughtUp = await page
      .getByText("You're all caught up")
      .isVisible()
      .catch(() => false)

    if (!hasAllCaughtUp) {
      // Count should have decreased
      const newStatusText = page.getByText(/contacts need your attention/)
      const newText = await newStatusText.textContent()
      const newCount = parseInt(newText?.match(/(\d+)/)?.[1] || '0', 10)
      expect(newCount).toBeLessThan(initialCount)
    }

    // Test passed: the dashboard updated immediately without page navigation
    // This verifies the cross-domain query invalidation is working
  })
})
