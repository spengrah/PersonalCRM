import { test, expect, type Page } from '@playwright/test'
import { createTestAPI, TestAPI, type SeedBehaviorResult } from './helpers/test-api'
import { fulfillJson } from './helpers/fulfill-json'

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

// The declared world owns every name and id, so a handle is how a test names a
// row. Reading through these keeps a title or a uuid from being restated here.
const handleId = (seeded: SeedBehaviorResult, handle: string): string => {
  const entity = seeded.entities[handle]
  expect(entity, `declared handle ${handle} should be in the manifest`).toBeTruthy()
  return entity.id
}
const handleName = (seeded: SeedBehaviorResult, handle: string): string =>
  seeded.entities[handle].name

// The events the API returns for a contact, which is the same data the cards
// render — so every expected string below is derived from it rather than rebuilt.
interface ContactEvent {
  id: string
  title: string
  start_time: string
  end_time: string
  location?: string
  html_link?: string
  attendee_count: number
}

async function fetchContactEvents(
  request: import('@playwright/test').APIRequestContext,
  contactId: string
): Promise<ContactEvent[]> {
  const response = await request.get(`${API_BASE_URL}/api/v1/contacts/${contactId}/events`, {
    headers: API_HEADERS,
  })
  expect(response.ok()).toBe(true)
  return (await response.json()).data as ContactEvent[]
}

test.describe('Meetings Component @area:meetings', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should display meetings section with upcoming and past events', async ({ page }) => {
    // CAL-025's declared world: two upcoming meetings, three past ones, and one
    // straddling now — six in all, three of which classify as upcoming (the
    // straddling meeting has not ended).
    const seeded = await testApi.seedBehavior('CAL-025')
    const contactId = handleId(seeded, 'attendee')

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')

    // Verify Meetings section exists
    // spec: CAL-024.meetings-section-shown-with-events
    await expect(page.getByRole('heading', { name: /Meetings/i })).toBeVisible()
    const region = meetingsRegion(page)

    // Verify filter tabs exist with live counts derived from the seeded data
    // spec: CAL-025.three-filters-all-upcoming
    await expect(region.getByRole('button', { name: /All \(6\)/i })).toBeVisible()
    await expect(region.getByRole('button', { name: /Upcoming \(3\)/i })).toBeVisible()
    await expect(region.getByRole('button', { name: /Past \(3\)/i })).toBeVisible()

    // By default (Upcoming tab pressed), only upcoming events should be visible
    // spec: CAL-025.upcoming-default-view
    await expect(region.getByRole('button', { name: /Upcoming \(3\)/i })).toHaveAttribute(
      'aria-pressed',
      'true'
    )
    await expect(region.getByText(handleName(seeded, 'upcoming-near'))).toBeVisible()
    await expect(region.getByText(handleName(seeded, 'upcoming-far'))).toBeVisible()
    await expect(region.getByText(handleName(seeded, 'in-progress'))).toBeVisible()
    await expect(region.getByText(handleName(seeded, 'past-recent'))).not.toBeVisible()
    await expect(region.getByText(handleName(seeded, 'past-oldest'))).not.toBeVisible()

    // Click All filter to see all events (no CAL-025 then-item states the
    // combined view; this block just exercises the third filter's activation)
    await region.getByRole('button', { name: /All \(6\)/i }).click()
    await expect(region.getByRole('button', { name: /All \(6\)/i })).toHaveAttribute(
      'aria-pressed',
      'true'
    )
    await expect(region.getByRole('button', { name: /Upcoming \(3\)/i })).toHaveAttribute(
      'aria-pressed',
      'false'
    )
    await expect(region.getByText(handleName(seeded, 'upcoming-near'))).toBeVisible()
    await expect(region.getByText(handleName(seeded, 'past-recent'))).toBeVisible()
    await expect(region.getByText(handleName(seeded, 'past-oldest'))).toBeVisible()
  })

  test('should filter between upcoming and past events', async ({ page }) => {
    const seeded = await testApi.seedBehavior('CAL-025')
    const contactId = handleId(seeded, 'attendee')

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    const region = meetingsRegion(page)

    // Click Upcoming filter: shows only the upcoming events
    // spec: CAL-025.three-filters-all-upcoming
    await region.getByRole('button', { name: /Upcoming \(3\)/i }).click()
    await expect(region.getByRole('button', { name: /Upcoming \(3\)/i })).toHaveAttribute(
      'aria-pressed',
      'true'
    )
    await expect(region.getByText(handleName(seeded, 'upcoming-near'))).toBeVisible()
    await expect(region.getByText(handleName(seeded, 'past-middle'))).not.toBeVisible()

    // Click Past filter: only the past-seeded events show (the end-time
    // classification boundary itself is proven by the in-progress test below)
    // spec: CAL-025.three-filters-all-upcoming
    await region.getByRole('button', { name: /Past \(3\)/i }).click()
    await expect(region.getByRole('button', { name: /Past \(3\)/i })).toHaveAttribute(
      'aria-pressed',
      'true'
    )
    await expect(region.getByText(handleName(seeded, 'past-middle'))).toBeVisible()
    await expect(region.getByText(handleName(seeded, 'upcoming-near'))).not.toBeVisible()

    // The past meeting's card carries the past marker (scoped to that card)
    // spec: CAL-026.past-meeting-carries-past
    await expect(
      meetingCard(page, handleName(seeded, 'past-middle')).getByText('Past', { exact: true })
    ).toBeVisible()
  })

  test('should order past meetings most-recent-first', async ({ page }) => {
    // The three past meetings are DECLARED oldest, most-recent, middle (10, 1, 5
    // days ago) and render most-recent-first, so this proves the sort rather than
    // echoing the order they were created in.
    const seeded = await testApi.seedBehavior('CAL-025')
    const contactId = handleId(seeded, 'attendee')

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    const region = meetingsRegion(page)

    await region.getByRole('button', { name: /Past \(3\)/i }).click()

    // spec: CAL-025.past-meetings-ordered-most
    const cards = meetingCards(page)
    await expect(cards).toHaveCount(3)
    await expect(cards.nth(0)).toContainText(handleName(seeded, 'past-recent'))
    await expect(cards.nth(1)).toContainText(handleName(seeded, 'past-middle'))
    await expect(cards.nth(2)).toContainText(handleName(seeded, 'past-oldest'))
  })

  test('should classify a meeting as past only once its end time has passed', async ({ page }) => {
    // The declared world holds one meeting IN PROGRESS (started before the run
    // anchor, ending after it) alongside three that have fully ended. Only the
    // ended ones may classify as past — an in-progress meeting has a past START
    // time, so this distinguishes end-time classification from start-time
    // classification.
    //
    // The clock is frozen to the anchor the SEED returned, which is the instant
    // the fixture's offsets are relative to. Freezing is irreducible here and it
    // is the only thing mocked: the events themselves are real rows written by the
    // real sync provider, and an unfrozen clock would walk past the straddling
    // meeting's end time mid-test.
    const seeded = await testApi.seedBehavior('CAL-025')
    const contactId = handleId(seeded, 'attendee')

    await mockFrozenSystemTime(page, seeded.anchor)

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    const region = meetingsRegion(page)

    // The live counts split 3/3 against the frozen accelerated now: the
    // in-progress meeting stays upcoming, the three ended ones are past
    // spec: CAL-025.meeting-classified-past-once
    await expect(region.getByRole('button', { name: /Upcoming \(3\)/i })).toBeVisible()
    await expect(region.getByRole('button', { name: /Past \(3\)/i })).toBeVisible()

    // Default (Upcoming) view carries the in-progress meeting, unmarked
    await expect(region.getByText(handleName(seeded, 'in-progress'))).toBeVisible()
    await expect(region.getByText(handleName(seeded, 'past-recent'))).not.toBeVisible()
    await expect(
      meetingCard(page, handleName(seeded, 'in-progress')).getByText('Past', { exact: true })
    ).not.toBeVisible()

    // Past view carries only the ended meetings
    await region.getByRole('button', { name: /Past \(3\)/i }).click()
    await expect(region.getByText(handleName(seeded, 'past-recent'))).toBeVisible()
    await expect(region.getByText(handleName(seeded, 'in-progress'))).not.toBeVisible()
  })

  test('should summarize time, place, and size on the meeting card', async ({ page, request }) => {
    // CAL-026's world: a meeting with a location and the default two attendees, a
    // bare one the account organizes without attending (the only shape that stores
    // a single attendee), and an untitled one.
    const seeded = await testApi.seedBehavior('CAL-026')
    const contactId = handleId(seeded, 'attendee')
    const detailedTitle = handleName(seeded, 'detailed')
    const bareTitle = handleName(seeded, 'bare')

    // Read the seeded events back from the API so the expected date/time and
    // location strings are derived from the same data the card renders.
    const events = await fetchContactEvents(request, contactId)
    const detailed = events.find(e => e.title === detailedTitle)
    const bare = events.find(e => e.title === bareTitle)
    expect(detailed).toBeTruthy()
    expect(bare).toBeTruthy()
    expect(detailed!.location, 'the detailed meeting carries a location').toBeTruthy()
    // The provider maps an empty location to NULL, so the response omits the key
    // entirely — that absence is the state the card's negative assertion reads.
    expect(bare!.location, 'the bare meeting carries no location').toBeUndefined()
    expect(detailed!.attendee_count).toBe(2)
    expect(bare!.attendee_count).toBe(1)

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')

    const detailedCard = meetingCard(page, detailedTitle)
    const bareCard = meetingCard(page, bareTitle)
    await expect(detailedCard).toBeVisible()
    await expect(bareCard).toBeVisible()

    // Title, date, and start-to-end time range, computed from the API data
    // spec: CAL-026.shows-title-fallback-label
    await expect(detailedCard).toContainText(detailedTitle)
    await expect(detailedCard).toContainText(expectedDateTime(detailed!.start_time))
    await expect(detailedCard).toContainText(
      expectedTimeRange(detailed!.start_time, detailed!.end_time)
    )

    // Location shows only on the meeting that has one
    // spec: CAL-026.shows-location-when-meeting
    await expect(detailedCard.getByText(detailed!.location!)).toBeVisible()
    await expect(bareCard.getByText(detailed!.location!)).not.toBeVisible()

    // Attendee count shows only when the meeting has more than one attendee
    // spec: CAL-026.shows-attendee-count-only
    await expect(detailedCard.getByText('2 attendees')).toBeVisible()
    await expect(bareCard.getByText(/attendees/)).not.toBeVisible()
  })

  test('should fall back to a label for untitled meetings', async ({ page }) => {
    // The untitled meeting is a real row: the provider stores an empty summary as
    // the empty STRING (never NULL), which is exactly the state the card's
    // fallback label serves.
    const seeded = await testApi.seedBehavior('CAL-026')
    const contactId = handleId(seeded, 'attendee')

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')

    // The untitled event's card renders the fallback label
    // spec: CAL-026.shows-title-fallback-label
    await expect(
      meetingCards(page).getByRole('heading', { level: 4, name: 'Untitled Meeting' })
    ).toBeVisible()
  })

  test('should display html_link as clickable external link', async ({ page, request }) => {
    const seeded = await testApi.seedBehavior('CAL-027')
    const contactId = handleId(seeded, 'attendee')
    const linkedTitle = handleName(seeded, 'linked')
    const plainTitle = handleName(seeded, 'plain')

    // The href is read back from the API, never restated here.
    const events = await fetchContactEvents(request, contactId)
    const linked = events.find(e => e.title === linkedTitle)
    const plain = events.find(e => e.title === plainTitle)
    expect(linked?.html_link, 'the linked meeting carries a source link').toBeTruthy()
    expect(plain?.html_link, 'the plain meeting carries none').toBeUndefined()

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    const region = meetingsRegion(page)

    // The linked meeting's title is a link opening in a new tab
    // spec: CAL-027.title-becomes-link-opens
    const meetingLink = region.getByRole('link', { name: new RegExp(linkedTitle) })
    await expect(meetingLink).toBeVisible()
    await expect(meetingLink).toHaveAttribute('target', '_blank')
    await expect(meetingLink).toHaveAttribute('href', linked!.html_link!)

    // The meeting without a link renders its title as plain text, not a link
    // spec: CAL-027.meeting-without-link-renders
    await expect(region.getByText(plainTitle)).toBeVisible()
    await expect(region.getByRole('link', { name: new RegExp(plainTitle) })).toHaveCount(0)
  })

  test('should not show meetings section when no events exist or they fail to load', async ({
    page,
  }) => {
    // CAL-024's world holds TWO contacts: one with a meeting and one with none.
    // The one with a meeting is the positive control — without it, an assertion
    // that no Meetings section rendered would pass just as happily against a page
    // that never renders the section at all.
    const seeded = await testApi.seedBehavior('CAL-024')
    const withEventId = handleId(seeded, 'with-event')
    const bareId = handleId(seeded, 'bare')

    // Positive control: the contact that HAS an event shows the section
    // spec: CAL-024.meetings-section-shown-with-events
    await page.goto(`/contacts/${withEventId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: /Meetings/i })).toBeVisible()

    // Meetings section is not visible for the contact with no events
    // spec: CAL-024.nothing-shown-without-events
    await page.goto(`/contacts/${bareId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: handleName(seeded, 'bare') })).toBeVisible()
    await expect(page.getByRole('heading', { name: /Meetings/i })).not.toBeVisible()

    // And a failing events fetch renders nothing even for the contact that HAS an
    // event — the same absence, reached by a different route
    // spec: CAL-024.nothing-shown-without-events
    await page.route(`**/api/v1/contacts/${withEventId}/events**`, route =>
      fulfillJson(
        route,
        { success: false, error: { code: 'NOT_FOUND', message: 'not found' } },
        404
      )
    )
    await page.goto(`/contacts/${withEventId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(
      page.getByRole('heading', { name: handleName(seeded, 'with-event') })
    ).toBeVisible()
    await expect(page.getByRole('heading', { name: /Meetings/i })).not.toBeVisible()
  })

  test('should reveal a long meeting list progressively', async ({ page }) => {
    // 15 meetings — five past the initial ten-card page.
    const seeded = await testApi.seedBehavior('CAL-028')
    const contactId = handleId(seeded, 'attendee')

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    const region = meetingsRegion(page)

    // Initially 10 of the 15 seeded events show, and the control's accessible
    // name reports the remainder derived from the data
    // spec: CAL-028.fixed-number-meetings-show
    await expect(region.getByRole('button', { name: /Load more \(5 remaining\)/i })).toBeVisible()
    await expect(meetingCards(page)).toHaveCount(10)

    // One activation exhausts the list: all 15 show and the control disappears
    // spec: CAL-028.each-activation-reveals-more
    await region.getByRole('button', { name: /Load more \(5 remaining\)/i }).click()
    await expect(meetingCards(page)).toHaveCount(15)
    await expect(region.getByRole('button', { name: /Load more/i })).not.toBeVisible()

    // Switching filters resets the reveal back to the initial page
    // spec: CAL-028.switching-filters-resets-reveal
    await region.getByRole('button', { name: /All \(15\)/i }).click()
    await expect(region.getByRole('button', { name: /Load more \(5 remaining\)/i })).toBeVisible()
    await expect(meetingCards(page)).toHaveCount(10)
  })
})
