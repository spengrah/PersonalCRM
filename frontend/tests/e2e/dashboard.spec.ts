import { test, expect } from '@playwright/test'

// API configuration for E2E tests
const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

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
    request,
  }) => {
    const suffix = Date.now()
    const contactName = `E2E Dashboard Update ${suffix}`

    // Create an overdue contact via API
    // Set last_contacted to 30 days ago with weekly cadence to make it overdue
    const thirtyDaysAgo = new Date(Date.now() - 30 * 24 * 60 * 60 * 1000).toISOString()
    const contactResponse = await request.post(`${API_BASE_URL}/api/v1/contacts`, {
      headers: API_HEADERS,
      data: {
        full_name: contactName,
        cadence: 'weekly',
        last_contacted: thirtyDaysAgo,
      },
    })
    expect(contactResponse.ok()).toBeTruthy()
    const contactData = await contactResponse.json()
    const contactId = contactData.data.id

    try {
      // Navigate to dashboard
      await page.goto('/dashboard')
      await page.waitForLoadState('networkidle')

      // Verify our overdue contact appears on the dashboard
      await expect(page.getByText(contactName)).toBeVisible()

      // Get the initial overdue count from the page text
      const statusText = page.getByText(/contacts need your attention/)
      await expect(statusText).toBeVisible()
      const initialText = await statusText.textContent()
      const initialCount = parseInt(initialText?.match(/(\d+)/)?.[1] || '0', 10)
      expect(initialCount).toBeGreaterThan(0)

      // Click "Mark as Contacted" button on our contact's card
      // The button is within the same card as the contact name
      const contactCard = page.locator('div').filter({ hasText: contactName }).first()
      const markContactedButton = contactCard.getByRole('button', { name: /Mark as Contacted/i })
      await markContactedButton.click()

      // Wait for the mutation to complete (button should stop loading)
      await expect(markContactedButton).not.toBeVisible({ timeout: 5000 })

      // Verify the contact is no longer visible on the dashboard (it's no longer overdue)
      // This tests that the cross-domain invalidation works WITHOUT navigating away
      await expect(page.getByText(contactName)).not.toBeVisible()

      // Verify the count decreased
      // Either the count decreased, or we see "all caught up" if it was the last one
      const hasAllCaughtUp = await page
        .getByText("You're all caught up")
        .isVisible()
        .catch(() => false)

      if (!hasAllCaughtUp) {
        const newStatusText = page.getByText(/contacts need your attention/)
        const newText = await newStatusText.textContent()
        const newCount = parseInt(newText?.match(/(\d+)/)?.[1] || '0', 10)
        expect(newCount).toBeLessThan(initialCount)
      }
    } finally {
      // Cleanup: delete the contact via API
      await request.delete(`${API_BASE_URL}/api/v1/contacts/${contactId}`, {
        headers: API_HEADERS,
      })
    }
  })
})
