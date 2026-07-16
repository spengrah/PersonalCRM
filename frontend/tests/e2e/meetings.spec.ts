import { test, expect, type Page } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'
import { fulfillJson } from './helpers/fulfill-json'
import type { CalendarEvent } from '../../src/types/calendar'

// API configuration for E2E tests
const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

// Freeze the client-side accelerated-time frame to a fixed instant by mocking
// GET /system/time BEFORE the page loads. `acceleration_factor: 0` pins
// currentTime at base_time, and the body is the full apiClient envelope.
// Per-page + pre-navigation, so it is parallel-safe and does not touch the
// process-wide acceleration state.
async function mockFrozenSystemTime(page: Page, isoInstant: string): Promise<void> {
  await page.route('**/api/v1/system/time', route =>
    fulfillJson(route, {
      success: true,
      data: {
        current_time: isoInstant,
        base_time: isoInstant,
        is_accelerated: true,
        acceleration_factor: 0,
        environment: 'testing',
      },
    })
  )
}

// Scope every seeded-title lookup to the Meetings region so parallel workers'
// data (and other page text) can never collide with these assertions.
const meetingsRegion = (page: Page) => page.getByRole('region', { name: 'Meetings' })
const meetingCards = (page: Page) => meetingsRegion(page).getByRole('listitem')
const meetingCard = (page: Page, title: string) => meetingCards(page).filter({ hasText: title })

// Expected card strings computed with the same Intl options the component
// uses, from the event timestamps the API actually returned (data-derived,
// not copied from the UI).
function expectedDateTime(iso: string): string {
  return new Intl.DateTimeFormat('en-US', {
    weekday: 'short',
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
  }).format(new Date(iso))
}

function expectedTimeRange(startIso: string, endIso: string): string {
  const fmt = new Intl.DateTimeFormat('en-US', { hour: 'numeric', minute: '2-digit' })
  return `${fmt.format(new Date(startIso))} - ${fmt.format(new Date(endIso))}`
}

test.describe('Meetings Component @area:meetings', () => {
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
    await page.waitForLoadState('domcontentloaded')

    // Verify Meetings section exists
    // spec: CAL-024[0]
    await expect(page.getByRole('heading', { name: /Meetings/i })).toBeVisible()
    const region = meetingsRegion(page)

    // Verify filter tabs exist with live counts derived from the seeded data
    // spec: CAL-025[0]
    await expect(region.getByRole('button', { name: /All \(4\)/i })).toBeVisible()
    await expect(region.getByRole('button', { name: /Upcoming \(2\)/i })).toBeVisible()
    await expect(region.getByRole('button', { name: /Past \(2\)/i })).toBeVisible()

    // By default (Upcoming tab pressed), only upcoming events should be visible
    // spec: CAL-025[1]
    await expect(region.getByRole('button', { name: /Upcoming \(2\)/i })).toHaveAttribute(
      'aria-pressed',
      'true'
    )
    await expect(region.getByText(`${testApi.prefix}-Upcoming Meeting 1`)).toBeVisible()
    await expect(region.getByText(`${testApi.prefix}-Upcoming Meeting 2`)).toBeVisible()
    await expect(region.getByText(`${testApi.prefix}-Past Meeting 1`)).not.toBeVisible()
    await expect(region.getByText(`${testApi.prefix}-Past Meeting 2`)).not.toBeVisible()

    // Click All filter to see all events (no CAL-025 then-item states the
    // combined view; this block just exercises the third filter's activation)
    await region.getByRole('button', { name: /All \(4\)/i }).click()
    await expect(region.getByRole('button', { name: /All \(4\)/i })).toHaveAttribute(
      'aria-pressed',
      'true'
    )
    await expect(region.getByRole('button', { name: /Upcoming \(2\)/i })).toHaveAttribute(
      'aria-pressed',
      'false'
    )
    await expect(region.getByText(`${testApi.prefix}-Upcoming Meeting 1`)).toBeVisible()
    await expect(region.getByText(`${testApi.prefix}-Upcoming Meeting 2`)).toBeVisible()
    await expect(region.getByText(`${testApi.prefix}-Past Meeting 1`)).toBeVisible()
    await expect(region.getByText(`${testApi.prefix}-Past Meeting 2`)).toBeVisible()
  })

  test('should filter between upcoming and past events', async ({ page }) => {
    // Seed calendar events
    await testApi.seedCalendarEvents(contactId, [
      { title: 'Upcoming Event', is_past: false, days_ahead: 5 },
      { title: 'Past Event', is_past: true, days_ago: 5 },
    ])

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    const region = meetingsRegion(page)

    // Click Upcoming filter: shows only the upcoming event
    // spec: CAL-025[0]
    await region.getByRole('button', { name: /Upcoming \(1\)/i }).click()
    await expect(region.getByRole('button', { name: /Upcoming \(1\)/i })).toHaveAttribute(
      'aria-pressed',
      'true'
    )
    await expect(region.getByText(`${testApi.prefix}-Upcoming Event`)).toBeVisible()
    await expect(region.getByText(`${testApi.prefix}-Past Event`)).not.toBeVisible()

    // Click Past filter: only the past-seeded event shows (the end-time
    // classification boundary itself is proven by the in-progress test below)
    // spec: CAL-025[0]
    await region.getByRole('button', { name: /Past \(1\)/i }).click()
    await expect(region.getByRole('button', { name: /Past \(1\)/i })).toHaveAttribute(
      'aria-pressed',
      'true'
    )
    await expect(region.getByText(`${testApi.prefix}-Past Event`)).toBeVisible()
    await expect(region.getByText(`${testApi.prefix}-Upcoming Event`)).not.toBeVisible()

    // The past meeting's card carries the past marker (scoped to that card)
    // spec: CAL-026[3]
    await expect(
      meetingCard(page, `${testApi.prefix}-Past Event`).getByText('Past', { exact: true })
    ).toBeVisible()
  })

  test('should order past meetings most-recent-first', async ({ page }) => {
    await testApi.seedCalendarEvents(contactId, [
      { title: 'Past Oldest', is_past: true, days_ago: 10 },
      { title: 'Past Recent', is_past: true, days_ago: 1 },
      { title: 'Past Middle', is_past: true, days_ago: 5 },
    ])

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    const region = meetingsRegion(page)

    await region.getByRole('button', { name: /Past \(3\)/i }).click()

    // Cards render most-recent-first (days_ago 1, then 5, then 10) — a
    // different order than they were seeded in (10, 1, 5), so this proves
    // the sort rather than echoing insertion order
    // spec: CAL-025[3]
    const cards = meetingCards(page)
    await expect(cards).toHaveCount(3)
    await expect(cards.nth(0)).toContainText(`${testApi.prefix}-Past Recent`)
    await expect(cards.nth(1)).toContainText(`${testApi.prefix}-Past Middle`)
    await expect(cards.nth(2)).toContainText(`${testApi.prefix}-Past Oldest`)
  })

  test('should classify a meeting as past only once its end time has passed', async ({ page }) => {
    // Freeze the app's accelerated clock and mock two events around it: one
    // IN PROGRESS (started before now, ends after now) and one fully ended.
    // Only the ended one may classify as past — an in-progress meeting has a
    // past START time, so this distinguishes end-time classification from
    // start-time classification against the app's (frozen) accelerated now.
    const frozenNow = new Date('2026-06-01T12:00:00.000Z')
    const hour = 60 * 60 * 1000
    const inProgress: CalendarEvent = {
      id: 'e2e-in-progress-event',
      title: 'In Progress Meeting',
      start_time: new Date(frozenNow.getTime() - 1 * hour).toISOString(),
      end_time: new Date(frozenNow.getTime() + 1 * hour).toISOString(),
      status: 'confirmed',
      attendee_count: 0,
    }
    const ended: CalendarEvent = {
      id: 'e2e-ended-event',
      title: 'Ended Meeting',
      start_time: new Date(frozenNow.getTime() - 3 * hour).toISOString(),
      end_time: new Date(frozenNow.getTime() - 2 * hour).toISOString(),
      status: 'confirmed',
      attendee_count: 0,
    }

    await mockFrozenSystemTime(page, frozenNow.toISOString())
    await page.route(`**/api/v1/contacts/${contactId}/events**`, route =>
      fulfillJson(route, { success: true, data: [inProgress, ended] })
    )

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    const region = meetingsRegion(page)

    // The live counts split 1/1: the in-progress meeting stays upcoming, the
    // ended one is past — end time vs the frozen accelerated now decides
    // spec: CAL-025[2]
    await expect(region.getByRole('button', { name: /Upcoming \(1\)/i })).toBeVisible()
    await expect(region.getByRole('button', { name: /Past \(1\)/i })).toBeVisible()

    // Default (Upcoming) view carries the in-progress meeting, unmarked
    await expect(region.getByText('In Progress Meeting')).toBeVisible()
    await expect(region.getByText('Ended Meeting')).not.toBeVisible()
    await expect(
      meetingCard(page, 'In Progress Meeting').getByText('Past', { exact: true })
    ).not.toBeVisible()

    // Past view carries only the ended meeting
    await region.getByRole('button', { name: /Past \(1\)/i }).click()
    await expect(region.getByText('Ended Meeting')).toBeVisible()
    await expect(region.getByText('In Progress Meeting')).not.toBeVisible()
  })

  test('should summarize time, place, and size on the meeting card', async ({ page, request }) => {
    await testApi.seedCalendarEvents(contactId, [
      {
        title: 'Detailed Meeting',
        is_past: false,
        days_ahead: 3,
        location: `${testApi.prefix}-Conference Room B`,
        attendee_emails: [`${testApi.prefix}-a@example.com`, `${testApi.prefix}-b@example.com`],
      },
      { title: 'Bare Meeting', is_past: false, days_ahead: 4 },
    ])

    // Read the seeded events back from the API so the expected date/time
    // strings are derived from the same data the card renders.
    const eventsResponse = await request.get(
      `${API_BASE_URL}/api/v1/contacts/${contactId}/events`,
      { headers: API_HEADERS }
    )
    expect(eventsResponse.ok()).toBe(true)
    const events = (await eventsResponse.json()).data as Array<{
      title: string
      start_time: string
      end_time: string
    }>
    const detailed = events.find(e => e.title === `${testApi.prefix}-Detailed Meeting`)
    expect(detailed).toBeTruthy()

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')

    const detailedCard = meetingCard(page, `${testApi.prefix}-Detailed Meeting`)
    const bareCard = meetingCard(page, `${testApi.prefix}-Bare Meeting`)
    await expect(detailedCard).toBeVisible()
    await expect(bareCard).toBeVisible()

    // Title, date, and start-to-end time range, computed from the API data
    // spec: CAL-026[0]
    await expect(detailedCard).toContainText(expectedDateTime(detailed!.start_time))
    await expect(detailedCard).toContainText(
      expectedTimeRange(detailed!.start_time, detailed!.end_time)
    )

    // Location shows only on the meeting that has one
    // spec: CAL-026[1]
    await expect(detailedCard.getByText(`${testApi.prefix}-Conference Room B`)).toBeVisible()
    await expect(bareCard.getByText(`${testApi.prefix}-Conference Room B`)).not.toBeVisible()

    // Attendee count shows only when the meeting has more than one attendee
    // spec: CAL-026[2]
    await expect(detailedCard.getByText('2 attendees')).toBeVisible()
    await expect(bareCard.getByText(/attendees/)).not.toBeVisible()
  })

  test('should fall back to a label for untitled meetings', async ({ page, request }) => {
    // The seed endpoint validates title as non-empty (min=1), so the untitled
    // branch is driven with a route-mocked events response (the sanctioned
    // technique for states real seeding cannot express).
    const timeResponse = await request.get(`${API_BASE_URL}/api/v1/system/time`, {
      headers: API_HEADERS,
    })
    expect(timeResponse.ok()).toBe(true)
    const now = new Date(
      ((await timeResponse.json()).data as { current_time: string }).current_time
    )
    const start = new Date(now.getTime() + 30 * 24 * 60 * 60 * 1000)
    const end = new Date(start.getTime() + 60 * 60 * 1000)

    const untitledEvent: CalendarEvent = {
      id: 'e2e-untitled-event',
      title: '',
      start_time: start.toISOString(),
      end_time: end.toISOString(),
      status: 'confirmed',
      attendee_count: 0,
    }
    await page.route(`**/api/v1/contacts/${contactId}/events**`, route =>
      fulfillJson(route, { success: true, data: [untitledEvent] })
    )

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')

    // The untitled event's card renders the fallback label
    // spec: CAL-026[0]
    await expect(
      meetingCards(page).getByRole('heading', { level: 4, name: 'Untitled Meeting' })
    ).toBeVisible()
  })

  test('should display html_link as clickable external link', async ({ page }) => {
    // Seed one event with html_link and one without
    await testApi.seedCalendarEvents(contactId, [
      {
        title: 'Meeting With Link',
        is_past: false,
        days_ahead: 3,
        html_link: 'https://calendar.google.com/calendar/event?eid=test123',
      },
      { title: 'Meeting Without Link', is_past: false, days_ahead: 4 },
    ])

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    const region = meetingsRegion(page)

    // The linked meeting's title is a link opening in a new tab
    // spec: CAL-027[0]
    const meetingLink = region.getByRole('link', {
      name: new RegExp(`${testApi.prefix}-Meeting With Link`),
    })
    await expect(meetingLink).toBeVisible()
    await expect(meetingLink).toHaveAttribute('target', '_blank')
    await expect(meetingLink).toHaveAttribute(
      'href',
      'https://calendar.google.com/calendar/event?eid=test123'
    )

    // The meeting without a link renders its title as plain text, not a link
    // spec: CAL-027[1]
    await expect(region.getByText(`${testApi.prefix}-Meeting Without Link`)).toBeVisible()
    await expect(
      region.getByRole('link', { name: new RegExp(`${testApi.prefix}-Meeting Without Link`) })
    ).toHaveCount(0)
  })

  test('should not show meetings section when no events exist or they fail to load', async ({
    page,
  }) => {
    // Don't seed any events - just navigate to the contact
    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')

    // Meetings section should not be visible with no events
    // spec: CAL-024[1]
    await expect(
      page.getByRole('heading', { name: `${testApi.prefix}-Meeting Test Contact` })
    ).toBeVisible()
    await expect(page.getByRole('heading', { name: /Meetings/i })).not.toBeVisible()

    // Even with events seeded, a failing events fetch renders nothing
    // spec: CAL-024[1]
    await testApi.seedCalendarEvents(contactId, [
      { title: 'Hidden Meeting', is_past: false, days_ahead: 3 },
    ])
    await page.route(`**/api/v1/contacts/${contactId}/events**`, route =>
      fulfillJson(
        route,
        { success: false, error: { code: 'NOT_FOUND', message: 'not found' } },
        404
      )
    )
    await page.reload()
    await page.waitForLoadState('domcontentloaded')
    await expect(
      page.getByRole('heading', { name: `${testApi.prefix}-Meeting Test Contact` })
    ).toBeVisible()
    await expect(page.getByRole('heading', { name: /Meetings/i })).not.toBeVisible()
  })

  test('should reveal a long meeting list progressively', async ({ page }) => {
    // Seed more than 10 events (the default display limit)
    const events = Array.from({ length: 15 }, (_, i) => ({
      title: `Event ${i + 1}`,
      is_past: false,
      days_ahead: i + 1,
    }))

    await testApi.seedCalendarEvents(contactId, events)

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    const region = meetingsRegion(page)

    // Initially 10 of the 15 seeded events show, and the control's accessible
    // name reports the remainder derived from the data
    // spec: CAL-028[0]
    await expect(region.getByRole('button', { name: /Load more \(5 remaining\)/i })).toBeVisible()
    await expect(meetingCards(page)).toHaveCount(10)

    // One activation exhausts the list: all 15 show and the control disappears
    // spec: CAL-028[1]
    await region.getByRole('button', { name: /Load more \(5 remaining\)/i }).click()
    await expect(meetingCards(page)).toHaveCount(15)
    await expect(region.getByRole('button', { name: /Load more/i })).not.toBeVisible()

    // Switching filters resets the reveal back to the initial page
    // spec: CAL-028[2]
    await region.getByRole('button', { name: /All \(15\)/i }).click()
    await expect(region.getByRole('button', { name: /Load more \(5 remaining\)/i })).toBeVisible()
    await expect(meetingCards(page)).toHaveCount(10)
  })
})
