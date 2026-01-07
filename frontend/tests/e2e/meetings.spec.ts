import { test, expect } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

// API configuration for E2E tests
const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

test.describe('Meetings Component', () => {
  let testApi: TestAPI
  let contactId: string

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)

    // Create a contact first
    const contactResponse = await request.post(`${API_BASE_URL}/api/v1/contacts`, {
      headers: API_HEADERS,
      data: {
        full_name: `${testApi.prefix}-Meeting Test Contact`,
      },
    })
    expect(contactResponse.ok()).toBe(true)
    const contactData = await contactResponse.json()
    contactId = contactData.data.id
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should display meetings section with upcoming and past events', async ({ page }) => {
    // Seed calendar events for the contact
    await testApi.seedCalendarEvents(contactId, [
      { title: 'Upcoming Meeting 1', is_past: false, days_ahead: 3 },
      { title: 'Upcoming Meeting 2', is_past: false, days_ahead: 10 },
      { title: 'Past Meeting 1', is_past: true, days_ago: 5 },
      { title: 'Past Meeting 2', is_past: true, days_ago: 14 },
    ])

    // Navigate to contact page
    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('networkidle')

    // Verify Meetings section exists
    await expect(page.getByRole('heading', { name: /Meetings/i })).toBeVisible()

    // Verify filter tabs exist with correct counts
    await expect(page.getByRole('button', { name: /All \(4\)/i })).toBeVisible()
    await expect(page.getByRole('button', { name: /Upcoming \(2\)/i })).toBeVisible()
    await expect(page.getByRole('button', { name: /Past \(2\)/i })).toBeVisible()

    // By default (Upcoming tab), only upcoming events should be visible
    await expect(page.getByText(`${testApi.prefix}-Upcoming Meeting 1`)).toBeVisible()
    await expect(page.getByText(`${testApi.prefix}-Upcoming Meeting 2`)).toBeVisible()
    await expect(page.getByText(`${testApi.prefix}-Past Meeting 1`)).not.toBeVisible()
    await expect(page.getByText(`${testApi.prefix}-Past Meeting 2`)).not.toBeVisible()

    // Click All filter to see all events
    await page.getByRole('button', { name: /All \(4\)/i }).click()
    await expect(page.getByText(`${testApi.prefix}-Upcoming Meeting 1`)).toBeVisible()
    await expect(page.getByText(`${testApi.prefix}-Upcoming Meeting 2`)).toBeVisible()
    await expect(page.getByText(`${testApi.prefix}-Past Meeting 1`)).toBeVisible()
    await expect(page.getByText(`${testApi.prefix}-Past Meeting 2`)).toBeVisible()
  })

  test('should filter by upcoming events', async ({ page }) => {
    // Seed calendar events
    await testApi.seedCalendarEvents(contactId, [
      { title: 'Upcoming Event', is_past: false, days_ahead: 5 },
      { title: 'Past Event', is_past: true, days_ago: 5 },
    ])

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('networkidle')

    // Click Upcoming filter
    await page.getByRole('button', { name: /Upcoming \(1\)/i }).click()

    // Should show only upcoming event
    await expect(page.getByText(`${testApi.prefix}-Upcoming Event`)).toBeVisible()
    await expect(page.getByText(`${testApi.prefix}-Past Event`)).not.toBeVisible()
  })

  test('should filter by past events', async ({ page }) => {
    // Seed calendar events
    await testApi.seedCalendarEvents(contactId, [
      { title: 'Upcoming Event', is_past: false, days_ahead: 5 },
      { title: 'Past Event', is_past: true, days_ago: 5 },
    ])

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('networkidle')

    // Click Past filter
    await page.getByRole('button', { name: /Past \(1\)/i }).click()

    // Should show only past event
    await expect(page.getByText(`${testApi.prefix}-Past Event`)).toBeVisible()
    await expect(page.getByText(`${testApi.prefix}-Upcoming Event`)).not.toBeVisible()

    // Past events should have "Past" badge
    await expect(page.getByText('Past', { exact: true })).toBeVisible()
  })

  test('should display html_link as clickable external link', async ({ page }) => {
    // Seed event with html_link
    await testApi.seedCalendarEvents(contactId, [
      {
        title: 'Meeting With Link',
        is_past: false,
        days_ahead: 3,
        html_link: 'https://calendar.google.com/calendar/event?eid=test123',
      },
    ])

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('networkidle')

    // Find the meeting link - it should be a link with the title text
    const meetingLink = page.getByRole('link', {
      name: new RegExp(`${testApi.prefix}-Meeting With Link`),
    })
    await expect(meetingLink).toBeVisible()

    // Verify it has target="_blank" and correct href
    await expect(meetingLink).toHaveAttribute('target', '_blank')
    await expect(meetingLink).toHaveAttribute(
      'href',
      'https://calendar.google.com/calendar/event?eid=test123'
    )
  })

  test('should not show meetings section when no events exist', async ({ page }) => {
    // Don't seed any events - just navigate to the contact
    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('networkidle')

    // Meetings section should not be visible
    await expect(page.getByRole('heading', { name: /Meetings/i })).not.toBeVisible()
  })

  test('should show load more button when many events exist', async ({ page }) => {
    // Seed more than 10 events (the default display limit)
    const events = Array.from({ length: 15 }, (_, i) => ({
      title: `Event ${i + 1}`,
      is_past: false,
      days_ahead: i + 1,
    }))

    await testApi.seedCalendarEvents(contactId, events)

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('networkidle')

    // Should show "Load more" button
    await expect(page.getByRole('button', { name: /Load more/i })).toBeVisible()

    // Click load more
    await page.getByRole('button', { name: /Load more/i }).click()

    // After loading more, all 15 events should be visible
    // (or the button should update to show fewer remaining)
    await expect(page.getByRole('button', { name: /Load more/i })).not.toBeVisible()
  })
})
