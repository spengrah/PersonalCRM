import { test, expect, APIRequestContext } from '@playwright/test'

/**
 * Comprehensive E2E tests for overdue contact updates.
 *
 * Test Matrix:
 * - Actions: Mark as Contacted (Dashboard, Contact Detail, Contacts List), Complete Reminder
 * - Views: Dashboard (list & count), Contact Detail (last_contacted), Contacts List (column)
 *
 * Each action should:
 * 1. Update last_contacted timestamp with full precision (not just date)
 * 2. Remove contact from overdue list immediately
 * 3. Reflect changes in all UI views after navigation
 *
 * Note: In testing mode (CRM_ENV=testing), weekly cadence = 2 minutes.
 * Contacts become overdue 2 minutes after last_contacted.
 */

// API configuration for E2E tests
const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

// Helper to get or create an overdue contact
async function getOrCreateOverdueContact(request: APIRequestContext) {
  // First check for existing overdue contacts
  const overdueResp = await request.get(`${API_BASE_URL}/api/v1/contacts/overdue`, {
    headers: API_HEADERS,
  })
  const overdueData = await overdueResp.json()

  if (overdueData.data && overdueData.data.length > 0) {
    // Use existing overdue contact
    return { contact: overdueData.data[0], needsCleanup: false }
  }

  // Create a new contact - it will need time to become overdue
  const suffix = Date.now()
  const response = await request.post(`${API_BASE_URL}/api/v1/contacts`, {
    headers: API_HEADERS,
    data: {
      full_name: `Overdue Test ${suffix}`,
      cadence: 'weekly',
    },
  })
  expect(response.ok()).toBeTruthy()
  const data = await response.json()

  return { contact: data.data, needsCleanup: true, needsWait: true }
}

// Helper to delete a contact via API
async function deleteContact(request: APIRequestContext, contactId: string) {
  await request.delete(`${API_BASE_URL}/api/v1/contacts/${contactId}`, {
    headers: API_HEADERS,
  })
}

// Helper to verify contact is not in overdue list via API
async function isContactOverdue(request: APIRequestContext, contactId: string): Promise<boolean> {
  const response = await request.get(`${API_BASE_URL}/api/v1/contacts/overdue`, {
    headers: API_HEADERS,
  })
  const data = await response.json()
  return data.data.some((c: { id: string }) => c.id === contactId)
}

// Helper to get contact's last_contacted via API
async function getLastContacted(request: APIRequestContext, contactId: string): Promise<string> {
  const response = await request.get(`${API_BASE_URL}/api/v1/contacts/${contactId}`, {
    headers: API_HEADERS,
  })
  const data = await response.json()
  return data.data.last_contacted
}

test.describe('Overdue Contact Updates', () => {
  test.describe('Mark as Contacted from Dashboard', () => {
    test('should remove contact from dashboard and update with full timestamp', async ({
      page,
      request,
    }) => {
      // Get or create an overdue contact
      const { contact, needsCleanup, needsWait } = await getOrCreateOverdueContact(request)

      // If we created a new contact, we need to wait for it to become overdue
      if (needsWait) {
        test.skip(true, 'New contact created - needs 2+ minutes to become overdue in testing mode')
        return
      }

      try {
        const beforeMark = new Date()

        // Go to dashboard
        await page.goto('/dashboard')
        await page.waitForLoadState('networkidle')

        // Verify contact appears in overdue list
        await expect(page.getByRole('heading', { name: contact.full_name })).toBeVisible()

        // Get initial count
        const countText = await page.getByText(/contacts need your attention/).textContent()
        const initialCount = parseInt(countText?.match(/(\d+)/)?.[1] || '0', 10)
        expect(initialCount).toBeGreaterThan(0)

        // Click "Mark as Contacted" for our contact
        const contactCard = page.locator('div.rounded-lg').filter({ hasText: contact.full_name })
        await contactCard.getByRole('button', { name: /Mark as Contacted/i }).click()

        // Wait for mutation to complete
        await page.waitForTimeout(2000)

        const afterMark = new Date()

        // Contact should disappear from dashboard
        await expect(page.getByRole('heading', { name: contact.full_name })).not.toBeVisible({
          timeout: 5000,
        })

        // Count should decrease or show "all caught up"
        const hasAllCaughtUp = await page
          .getByText("You're all caught up")
          .isVisible()
          .catch(() => false)

        if (!hasAllCaughtUp) {
          const newCountText = await page.getByText(/contacts need your attention/).textContent()
          const newCount = parseInt(newCountText?.match(/(\d+)/)?.[1] || '0', 10)
          expect(newCount).toBeLessThan(initialCount)
        }

        // Verify via API that contact is no longer overdue
        const stillOverdue = await isContactOverdue(request, contact.id)
        expect(stillOverdue).toBe(false)

        // Verify last_contacted has full timestamp precision
        const lastContacted = await getLastContacted(request, contact.id)
        const lastContactedDate = new Date(lastContacted)

        // Should be between before and after the click
        expect(lastContactedDate.getTime()).toBeGreaterThanOrEqual(beforeMark.getTime() - 1000)
        expect(lastContactedDate.getTime()).toBeLessThanOrEqual(afterMark.getTime() + 1000)

        // Should NOT be midnight (the old bug)
        expect(lastContacted).not.toMatch(/T00:00:00(\.0+)?Z$/)
      } finally {
        if (needsCleanup) {
          await deleteContact(request, contact.id)
        }
      }
    })

    test('should reflect in Contact Detail page after navigation', async ({ page, request }) => {
      const { contact, needsCleanup, needsWait } = await getOrCreateOverdueContact(request)

      if (needsWait) {
        test.skip(true, 'New contact created - needs 2+ minutes to become overdue')
        return
      }

      try {
        await page.goto('/dashboard')
        await page.waitForLoadState('networkidle')

        // Mark as contacted from dashboard
        const contactCard = page.locator('div.rounded-lg').filter({ hasText: contact.full_name })
        await contactCard.getByRole('button', { name: /Mark as Contacted/i }).click()
        await page.waitForTimeout(2000)

        // Navigate to contact detail page
        await page.goto(`/contacts/${contact.id}`)
        await page.waitForLoadState('networkidle')

        // Last contacted should show today's date
        const today = new Date().toLocaleDateString()
        await expect(page.getByText(today)).toBeVisible()
      } finally {
        if (needsCleanup) {
          await deleteContact(request, contact.id)
        }
      }
    })
  })

  test.describe('Mark as Contacted from Contact Detail', () => {
    test('should update last_contacted and remove from dashboard', async ({ page, request }) => {
      const { contact, needsCleanup, needsWait } = await getOrCreateOverdueContact(request)

      if (needsWait) {
        test.skip(true, 'New contact created - needs 2+ minutes to become overdue')
        return
      }

      try {
        // Verify contact is initially overdue on dashboard
        await page.goto('/dashboard')
        await page.waitForLoadState('networkidle')
        await expect(page.getByRole('heading', { name: contact.full_name })).toBeVisible()

        // Go to contact detail and mark as contacted
        await page.goto(`/contacts/${contact.id}`)
        await page.waitForLoadState('networkidle')
        await page.getByRole('button', { name: /Mark as Contacted/i }).click()
        await page.waitForTimeout(2000)

        // Last contacted should update to today
        const today = new Date().toLocaleDateString()
        await expect(page.getByText(today)).toBeVisible()

        // Navigate back to dashboard
        await page.goto('/dashboard')
        await page.waitForLoadState('networkidle')

        // Contact should no longer appear
        await expect(page.getByRole('heading', { name: contact.full_name })).not.toBeVisible()

        // Verify via API
        const stillOverdue = await isContactOverdue(request, contact.id)
        expect(stillOverdue).toBe(false)
      } finally {
        if (needsCleanup) {
          await deleteContact(request, contact.id)
        }
      }
    })
  })

  test.describe('Cross-view consistency', () => {
    test('all views should show consistent state after marking as contacted', async ({
      page,
      request,
    }) => {
      const { contact, needsCleanup, needsWait } = await getOrCreateOverdueContact(request)

      if (needsWait) {
        test.skip(true, 'New contact created - needs 2+ minutes to become overdue')
        return
      }

      try {
        // Mark as contacted from dashboard
        await page.goto('/dashboard')
        await page.waitForLoadState('networkidle')

        const contactCard = page.locator('div.rounded-lg').filter({ hasText: contact.full_name })
        await contactCard.getByRole('button', { name: /Mark as Contacted/i }).click()
        await page.waitForTimeout(2000)

        // 1. Dashboard: contact should be gone
        await expect(page.getByRole('heading', { name: contact.full_name })).not.toBeVisible()

        // 2. Contact Detail: should show today's date
        await page.goto(`/contacts/${contact.id}`)
        await page.waitForLoadState('networkidle')
        const today = new Date().toLocaleDateString()
        await expect(page.getByText(today)).toBeVisible()

        // 3. Contacts List: should show today's date in the row
        await page.goto('/contacts')
        await page.waitForLoadState('networkidle')
        const contactRow = page.locator('tr').filter({ hasText: contact.full_name })
        await expect(contactRow.getByText(today)).toBeVisible()

        // 4. API: should confirm not overdue
        const stillOverdue = await isContactOverdue(request, contact.id)
        expect(stillOverdue).toBe(false)
      } finally {
        if (needsCleanup) {
          await deleteContact(request, contact.id)
        }
      }
    })
  })

  test.describe('Timestamp precision', () => {
    test('last_contacted should have full timestamp precision, not just date', async ({
      page,
      request,
    }) => {
      const { contact, needsCleanup, needsWait } = await getOrCreateOverdueContact(request)

      if (needsWait) {
        test.skip(true, 'New contact created - needs 2+ minutes to become overdue')
        return
      }

      try {
        await page.goto('/dashboard')
        await page.waitForLoadState('networkidle')

        const beforeMark = new Date()

        const contactCard = page.locator('div.rounded-lg').filter({ hasText: contact.full_name })
        await contactCard.getByRole('button', { name: /Mark as Contacted/i }).click()
        await page.waitForTimeout(2000)

        const afterMark = new Date()

        // Get the updated last_contacted via API
        const lastContacted = await getLastContacted(request, contact.id)
        const lastContactedDate = new Date(lastContacted)

        // Verify timestamp is within expected range
        expect(lastContactedDate.getTime()).toBeGreaterThanOrEqual(beforeMark.getTime() - 1000)
        expect(lastContactedDate.getTime()).toBeLessThanOrEqual(afterMark.getTime() + 1000)

        // The old bug would set time to midnight - verify this is NOT the case
        // A timestamp at exactly midnight would match this pattern
        expect(lastContacted).not.toMatch(/T00:00:00(\.0+)?Z$/)

        // Verify the timestamp includes sub-second precision (our fix stores full timestamp)
        expect(lastContacted).toMatch(/T\d{2}:\d{2}:\d{2}\.\d+Z$/)
      } finally {
        if (needsCleanup) {
          await deleteContact(request, contact.id)
        }
      }
    })
  })
})
