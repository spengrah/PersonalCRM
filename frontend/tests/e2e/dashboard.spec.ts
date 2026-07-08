import { test, expect } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

test.describe('Dashboard @area:dashboard', () => {
  test('should display dashboard with navigation @smoke', async ({ page }) => {
    await page.goto('/')

    // Should redirect to dashboard (client-side redirect via useEffect)
    await expect(page).toHaveURL('/dashboard', { timeout: 10000 })

    // Wait for page to fully load
    await page.waitForLoadState('domcontentloaded')

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
    await page.waitForLoadState('domcontentloaded')

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

test.describe('Dashboard - With Seeded Data @area:dashboard @area:overdue', () => {
  let testApi: TestAPI
  let overdueContactId: string

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)

    // Seed an overdue contact for dashboard testing
    const { ids } = await testApi.seedOverdueContacts([
      {
        full_name: 'Dashboard Test Contact',
        cadence: 'weekly',
        days_overdue: 3,
        email: 'dashboard-test@example.com',
      },
    ])
    overdueContactId = ids[0]
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('marking contact as contacted updates dashboard immediately without navigation', async ({
    page,
  }) => {
    // Deliberately un-cited: this test proves only part of CAD-028 (the
    // mutual interaction and the no-reload overdue-list exit). Its other
    // then-items — the accelerated-clock timestamp, the count update, and
    // dashboard/list/detail consistency — are not asserted here, and a
    // partial proof must not mark the behavior covered.
    const contactName = `${testApi.prefix}-Dashboard Test Contact`

    // Navigate to dashboard
    await page.goto('/dashboard')
    await page.waitForLoadState('domcontentloaded')

    // Verify our seeded contact is visible
    await expect(page.getByRole('heading', { name: contactName })).toBeVisible()

    // Find the "Mark as Contacted" button for our contact
    const contactCard = page.locator('div.rounded-lg').filter({ hasText: contactName })
    const markContactedButton = contactCard.getByRole('button', { name: /Mark as Contacted/i })
    await expect(markContactedButton).toBeVisible()

    // Register both listeners BEFORE the click (waitForResponse must be
    // set up before the triggering action, not after).

    // The dashboard "Mark as Contacted" quick action posts to
    // POST /interactions {direction:"mutual"} (the legacy PATCH
    // /last-contacted endpoint was removed).
    const markContactedResponsePromise = page.waitForResponse(
      response =>
        response.request().method() === 'POST' &&
        response.url().includes(`/api/v1/contacts/${overdueContactId}/interactions`)
    )

    // The invalidation-driven refetch: the open dashboard re-fetches the
    // overdue list after the mutation. Content-based predicate (reads the
    // response body) so there is no ordering race against a pre-mutation
    // fetch that still contains our id.
    const overdueRefetchPromise = page.waitForResponse(async response => {
      if (
        response.request().method() !== 'GET' ||
        !response.url().includes('/api/v1/contacts/overdue') ||
        !response.ok()
      ) {
        return false
      }
      const body = await response.json().catch(() => null)
      const entries: Array<{ id: string }> = body?.data ?? []
      return !entries.some(entry => entry.id === overdueContactId)
    })

    // Click "Mark as Contacted"
    await markContactedButton.click()

    // A mutual interaction is logged: the request asks for direction=mutual
    // AND the server persists it as mutual (the response body reflects the
    // stored interaction, not just the request).
    const markContactedResponse = await markContactedResponsePromise
    expect(markContactedResponse.ok()).toBe(true)
    expect(markContactedResponse.request().postDataJSON()?.direction).toBe('mutual')
    const interactionBody = await markContactedResponse.json()
    expect(interactionBody?.data?.direction).toBe('mutual')

    // The contact leaves the overdue list without a page reload: the open
    // dashboard's own refetch no longer includes it.
    await overdueRefetchPromise

    // The count updates: the card vanishes from the live dashboard without navigation.
    await expect(page.getByRole('heading', { name: contactName })).not.toBeVisible({
      timeout: 5000,
    })
  })
})
