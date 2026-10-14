import { test, expect, type APIRequestContext, type Page } from '@playwright/test'
import { JOB_TIMEOUT_MS } from './helpers/timeouts'
import {
  API_BASE_URL,
  API_HEADERS,
  createTestAPI,
  TestAPI,
  type SeedBehaviorResult,
} from './helpers/test-api'
import { fulfillJson } from './helpers/fulfill-json'
import {
  fetchTimeline,
  itemAgoDays,
  itemWithSource,
  itemsAgoDays,
  itemsWithSource,
} from './helpers/timeline-items'

interface ContactEvent {
  id: string
  title: string
  start_time: string
  end_time: string
  location?: string
  html_link?: string
  attendee_count: number
}

const handleId = (seeded: SeedBehaviorResult, handle: string): string => {
  const entity = seeded.entities[handle]
  expect(entity, `declared handle ${handle} should be in the manifest`).toBeTruthy()
  return entity.id
}

const handleName = (seeded: SeedBehaviorResult, handle: string): string => {
  const entity = seeded.entities[handle]
  expect(entity, `declared handle ${handle} should be in the manifest`).toBeTruthy()
  return entity.name
}

async function fetchUpcoming(
  request: APIRequestContext,
  contactId: string
): Promise<ContactEvent[]> {
  const response = await request.get(
    `${API_BASE_URL}/api/v1/contacts/${contactId}/events/upcoming?limit=250`,
    { headers: API_HEADERS }
  )
  expect(response.ok()).toBe(true)
  return (await response.json()).data as ContactEvent[]
}

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
  const format = new Intl.DateTimeFormat('en-US', { hour: 'numeric', minute: '2-digit' })
  return `${format.format(new Date(startIso))} - ${format.format(new Date(endIso))}`
}

function historyRows(page: Page) {
  return page.getByRole('list', { name: 'Interaction history' }).getByRole('listitem')
}

function upcomingCards(page: Page) {
  return page.getByRole('list', { name: 'Upcoming events' }).getByRole('listitem')
}

const sourceBadges: Record<string, string> = {
  manual: 'Manual',
  gcal: 'Calendar',
  todoist: 'Todoist',
  telegram: 'Telegram',
  messages: 'iMessage',
  anarlog_sessions: 'Meeting',
  phone_calls: 'Call',
  email: 'Email',
  gchat: 'Google Chat',
  whatsapp: 'WhatsApp',
}

test.describe('Interactions Section @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('renders every recorded source with its row metadata', async ({ page, request }) => {
    const seeded = await testApi.seedBehavior('IXN-002')
    const contactId = handleId(seeded, 'subject')
    const response = await fetchTimeline(request, contactId)
    const rows = historyRows(page)

    await page.goto(`/contacts/${contactId}`)
    await expect(rows).toHaveCount(10)

    // spec: IXN-001.every-source-rendered
    const expectedSources = [
      'email',
      'gchat',
      'whatsapp',
      'telegram',
      'messages',
      'phone_calls',
      'manual',
      'todoist',
      'anarlog_sessions',
    ]
    for (const source of expectedSources) {
      await expect(page.locator(`[data-source="${source}"]`)).toHaveCount(1)
      expect(itemsWithSource(response.items, source)).toHaveLength(1)
    }
    await expect(page.locator('[data-source="gcal"]')).toHaveCount(1)

    for (const item of response.items) {
      const source = item.members[0].source
      const row = page.locator(`[data-interaction-id="${item.identity.id}"]`)
      await expect(row).toHaveAttribute('data-interaction-id', item.identity.id)
      await expect(row).toHaveAttribute('data-source', source)
      if (source === 'gcal') {
        await expect(page.locator('[data-source="gcal"]')).toHaveCount(1)
      }
      // spec: IXN-002.source-badge
      await expect(row.locator('[data-badge]')).toHaveText(sourceBadges[source])
      // spec: IXN-002.timestamp-shown
      await expect(row.getByText(expectedDateTime(item.occurred_at), { exact: true })).toBeVisible()
      // spec: IXN-002.row-label
      await expect(row.getByText(item.members[0].label, { exact: true }).first()).toBeVisible()
      // spec: IXN-002.overflow-affordance-reserved
      if (item.identity.kind === 'interaction') {
        await expect(row.getByRole('button', { name: 'More actions' })).toBeDisabled()
      } else {
        await expect(row.getByRole('button', { name: 'More actions' })).toHaveCount(0)
      }
    }
    // The seeded iMessage burst is the non-correctable row of this world.
    expect(itemWithSource(response.items, 'messages').identity.kind).toBe('message_burst')

    const email = response.items.find(item => item.members[0].source === 'email')
    expect(email).toBeTruthy()
    const emailRow = page.locator(`[data-interaction-id="${email!.identity.id}"]`)
    // spec: IXN-002.direction-shown
    await expect(emailRow.getByLabel('inbound')).toBeVisible()
    // spec: IXN-002.content-indicator
    await expect(emailRow.getByText('1 message', { exact: true })).toBeVisible()
    expect(email!.members[0].venue_tags[0]).toBeTruthy()
    await expect(
      emailRow.locator(`[data-venue-key="${email!.members[0].venue_tags[0].key}"]`)
    ).toHaveText(email!.members[0].venue_tags[0].label)

    const messages = response.items.find(item => item.members[0].source === 'messages')
    expect(messages).toBeTruthy()
    await expect(
      page
        .locator(`[data-interaction-id="${messages!.identity.id}"]`)
        .getByText('3 messages', { exact: true })
    ).toBeVisible()
    const manual = response.items.find(item => item.members[0].source === 'manual')
    expect(manual).toBeTruthy()
    await expect(
      page.locator(`[data-interaction-id="${manual!.identity.id}"]`).getByLabel('outbound')
    ).toBeVisible()
    await expect(
      page
        .locator(`[data-interaction-id="${manual!.identity.id}"]`)
        .getByText('No content', { exact: true })
    ).toBeVisible()
    // spec: IXN-009.contentless-says-none
    await expect(
      page
        .locator(`[data-interaction-id="${manual!.identity.id}"]`)
        .getByRole('button', { name: /expand/i })
    ).toHaveCount(0)
    const todoist = response.items.find(item => item.members[0].source === 'todoist')
    expect(todoist).toBeTruthy()
    await expect(
      page
        .locator(`[data-interaction-id="${todoist!.identity.id}"]`)
        .getByText('No content', { exact: true })
    ).toBeVisible()
    const note = response.items.find(item => item.members[0].source === 'anarlog_sessions')
    expect(note).toBeTruthy()
    await expect(
      page
        .locator(`[data-interaction-id="${note!.identity.id}"]`)
        .getByText('Meeting note', { exact: true })
    ).toBeVisible()

    const gchat = response.items.find(item => item.members[0].source === 'gchat')
    const whatsapp = response.items.find(item => item.members[0].source === 'whatsapp')
    expect(gchat).toBeTruthy()
    expect(whatsapp).toBeTruthy()
    // spec: IXN-002.group-marker
    await expect(
      page
        .locator(`[data-interaction-id="${gchat!.identity.id}"]`)
        .getByText('Group', { exact: true })
    ).toBeVisible()
    await expect(
      page
        .locator(`[data-interaction-id="${whatsapp!.identity.id}"]`)
        .getByText('Group', { exact: true })
    ).toHaveCount(0)

    const calendar = response.items.find(item => item.members[0].source === 'gcal')
    expect(calendar?.members[0].event).toBeTruthy()
    const calendarRow = page.locator('[data-source="gcal"]')
    // spec: IXN-002.direction-shown
    await expect(calendarRow.getByLabel('mutual')).toBeVisible()
    // spec: IXN-002.content-indicator
    await expect(calendarRow.getByText('No content', { exact: true })).toBeVisible()
    await expect(calendarRow.getByRole('button', { name: /expand/i })).toHaveCount(0)
    // spec: IXN-002.calendar-row-event-details
    await expect(
      calendarRow.getByText(
        expectedTimeRange(
          calendar!.members[0].event!.start_time,
          calendar!.members[0].event!.end_time
        ),
        { exact: true }
      )
    ).toBeVisible()
    await expect(
      calendarRow.getByText(calendar!.members[0].event!.location!, { exact: true })
    ).toBeVisible()
    await expect(
      calendarRow.getByText(`${calendar!.members[0].event!.attendee_count} attendees`, {
        exact: true,
      })
    ).toBeVisible()
    const calendarLink = calendarRow.getByRole('link', { name: calendar!.members[0].event!.title! })
    await expect(calendarLink).toHaveAttribute('target', '_blank')
    await expect(calendarLink).toHaveAttribute('rel', 'noopener noreferrer')
    await expect(calendarLink).toHaveAttribute('href', calendar!.members[0].event!.html_link!)

    const call = response.items.find(item => item.members[0].source === 'phone_calls')
    expect(call?.members[0].call).toBeTruthy()
    const callRow = page.locator(`[data-interaction-id="${call!.identity.id}"]`)
    // spec: IXN-002.call-row-details
    await expect(callRow.getByText('Voice call', { exact: true })).toBeVisible()
    await expect(callRow.getByText('Answered', { exact: true })).toBeVisible()
    const duration =
      call!.members[0].call!.duration_seconds < 60
        ? `${call!.members[0].call!.duration_seconds}s`
        : `${Math.floor(call!.members[0].call!.duration_seconds / 60)}m ${call!.members[0].call!.duration_seconds % 60}s`
    await expect(callRow.getByText(duration, { exact: true })).toBeVisible()
    await expect(
      callRow.getByText(/message|Meeting note|No content/, { exact: false })
    ).toHaveCount(0)
  })

  test('pages the full history without gaps or duplicates', async ({ page, request }) => {
    const seeded = await testApi.seedBehavior('IXN-001')
    const contactId = handleId(seeded, 'subject')
    const first = await fetchTimeline(request, contactId, { page: 1 })
    const second = await fetchTimeline(request, contactId, { page: 2 })
    const expectedIds = [...first.items, ...second.items].map(item => item.identity.id)

    await page.goto(`/contacts/${contactId}`)
    await expect(historyRows(page)).toHaveCount(first.items.length)
    await expect(historyRows(page)).toHaveCount(20)
    await expect(
      historyRows(page).evaluateAll(elements =>
        elements.map(element => element.getAttribute('data-interaction-id'))
      )
    ).resolves.toEqual(expectedIds.slice(0, 20))
    // spec: IXN-001.load-more-pages
    await expect(page.getByRole('button', { name: 'Load more', exact: true })).toBeVisible()
    await page.getByRole('button', { name: 'Load more', exact: true }).click()
    await expect(historyRows(page)).toHaveCount(expectedIds.length)
    await expect(
      historyRows(page).evaluateAll(elements =>
        elements.map(element => element.getAttribute('data-interaction-id'))
      )
    ).resolves.toEqual(expectedIds)
    await expect(page.getByRole('button', { name: 'Load more', exact: true })).toHaveCount(0)
    expect(new Set(expectedIds).size).toBe(expectedIds.length)
    // spec: IXN-001.reverse-chron-order
    expect(expectedIds.length).toBe(25)
    await expect(
      historyRows(page).evaluateAll(elements =>
        elements.map(element => element.getAttribute('data-interaction-id'))
      )
    ).resolves.toEqual(expectedIds)
  })

  test('replaces the Meetings section with the unified history', async ({ page }) => {
    const seeded = await testApi.seedBehavior('IXN-002')
    const contactId = handleId(seeded, 'subject')
    await page.goto(`/contacts/${contactId}`)
    // spec: IXN-001.section-replaces-meetings
    await expect(page.getByRole('region', { name: 'Interactions' })).toBeVisible()
    await expect(page.getByRole('heading', { name: 'Interactions', exact: true })).toBeVisible()
    await expect(page.getByRole('region', { name: 'Meetings' })).toHaveCount(0)
    await expect(page.getByRole('heading', { name: 'Meetings', exact: true })).toHaveCount(0)
  })

  test('shows upcoming events above the history without minting interactions', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-006')
    const contactId = handleId(seeded, 'subject')
    const events = await fetchUpcoming(request, contactId)
    const interactions = await fetchTimeline(request, contactId)
    expect(events).toHaveLength(13)
    await page.goto(`/contacts/${contactId}`)

    // spec: IXN-006.upcoming-badge-at-top
    await expect(upcomingCards(page)).toHaveCount(3)
    await expect(
      upcomingCards(page).evaluateAll(elements =>
        elements.map(element => element.getAttribute('data-event-id'))
      )
    ).resolves.toEqual(events.slice(0, 3).map(event => event.id))
    for (const event of events.slice(0, 3)) {
      await expect(
        page.locator(`[data-event-id="${event.id}"]`).getByText('Upcoming', { exact: true })
      ).toBeVisible()
    }
    const underway = events.find(event => event.id === handleId(seeded, 'underway'))
    expect(underway).toBeTruthy()
    const underwayCard = page.locator(`[data-event-id="${underway!.id}"]`)
    // spec: IXN-006.in-progress-still-upcoming
    await expect(underwayCard).toBeVisible()
    const underwayLink = underwayCard.getByRole('link', { name: underway!.title })
    await expect(underwayLink).toHaveAttribute('target', '_blank')
    await expect(underwayLink).toHaveAttribute('rel', 'noopener noreferrer')
    await expect(underwayLink).toHaveAttribute('href', underway!.html_link!)
    // spec: IXN-006.event-link-preserved
    await expect(underwayLink).toBeVisible()
    // spec: IXN-006.event-time-place-size
    await expect(
      underwayCard.getByText(expectedDateTime(underway!.start_time), { exact: true })
    ).toBeVisible()
    await expect(
      underwayCard.getByText(expectedTimeRange(underway!.start_time, underway!.end_time), {
        exact: true,
      })
    ).toBeVisible()
    await expect(underwayCard.getByText(underway!.location!, { exact: true })).toBeVisible()
    await expect(
      underwayCard.getByText(`${underway!.attendee_count} attendees`, { exact: true })
    ).toBeVisible()
    await expect(
      page
        .locator('[data-event-id="' + underway!.id + '"]')
        .evaluate(
          (card, historyId) =>
            card.compareDocumentPosition(
              document.querySelector(`[data-interaction-id="${historyId}"]`)!
            ) & Node.DOCUMENT_POSITION_FOLLOWING,
          itemAgoDays(interactions.items, seeded.anchor, 'manual', 1).identity.id
        )
    ).resolves.toBeTruthy()
    const linkless = events.find(event => event.id === handleId(seeded, 'future-01'))
    expect(linkless?.html_link).toBeUndefined()
    const linklessCard = page.locator(`[data-event-id="${linkless!.id}"]`)
    await expect(linklessCard.getByText(linkless!.title, { exact: true })).toBeVisible()
    await expect(linklessCard.getByRole('link', { name: linkless!.title })).toHaveCount(0)

    // spec: IXN-006.all-upcoming-reachable
    await page
      .getByRole('button', { name: `Show all ${events.length} upcoming`, exact: true })
      .click()
    await expect(upcomingCards(page)).toHaveCount(events.length)
    await expect(
      upcomingCards(page).evaluateAll(elements =>
        elements.map(element => element.getAttribute('data-event-id'))
      )
    ).resolves.toEqual(events.map(event => event.id))
    await expect(page.getByText('Upcoming', { exact: true })).toHaveCount(events.length)
    const soleAttendee = events.find(event => event.id === handleId(seeded, 'future-03'))
    expect(soleAttendee).toBeTruthy()
    await expect(
      page.locator(`[data-event-id="${soleAttendee!.id}"]`).getByText(/attendees/)
    ).toHaveCount(0)

    // spec: IXN-006.no-interaction-row-for-future
    for (const event of events) {
      expect(
        interactions.items.some(item => item.members.some(member => member.source_ref === event.id))
      ).toBe(false)
    }
  })

  test('renders sparse and duplicated state honestly', async ({ browser, page, request }) => {
    const seeded = await testApi.seedBehavior('IXN-009')
    const duplicateContactId = handleId(seeded, 'dup-host')
    const manualDuplicates = itemsAgoDays(
      (await fetchTimeline(request, duplicateContactId)).items,
      seeded.anchor,
      'manual',
      3
    )
    expect(manualDuplicates).toHaveLength(2)
    await page.goto(`/contacts/${duplicateContactId}`)
    // spec: IXN-009.duplicates-render-distinct
    await expect(historyRows(page)).toHaveCount(4)
    for (const duplicate of manualDuplicates) {
      await expect(page.locator(`[data-interaction-id="${duplicate.identity.id}"]`)).toHaveCount(1)
    }
    await expect(page.locator('[data-source="gcal"]')).toHaveCount(2)

    const silentId = handleId(seeded, 'silent')
    await page.goto(`/contacts/${silentId}`)
    // spec: IXN-009.empty-state-explicit
    await expect(
      page.getByText('No interactions recorded for this contact.', { exact: true })
    ).toBeVisible()

    const futureOnlyId = handleId(seeded, 'future-only')
    await page.goto(`/contacts/${futureOnlyId}`)
    await expect(
      page.locator(`[data-event-id="${handleId(seeded, 'future-only-event')}"]`)
    ).toBeVisible()
    await expect(
      page.getByText('No interactions recorded for this contact.', { exact: true })
    ).toHaveCount(0)

    const errorContext = await browser.newContext({ baseURL: new URL(page.url()).origin })
    const errorPage = await errorContext.newPage()
    await errorPage.route(`**/api/v1/contacts/${silentId}/interactions*`, route =>
      fulfillJson(route, { success: false, error: 'synthetic failure' }, 500)
    )
    await errorPage.goto(`/contacts/${silentId}`)
    await expect(
      errorPage.getByRole('heading', { name: 'Interactions', exact: true })
    ).toBeVisible()
    await expect(errorPage.getByText('Failed to load interactions.', { exact: true })).toHaveCount(
      1,
      { timeout: JOB_TIMEOUT_MS }
    )
    await errorContext.close()
  })
})

test.describe('Call evidence @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('IXN-020: a missed call without voicemail is a distinct, cadence-inert row', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-020')
    const contactId = handleId(seeded, 'subject')
    const missedId = handleId(seeded, 'missed-call')
    const response = await fetchTimeline(request, contactId)
    const missed = response.items.find(item => item.identity.id === missedId)
    expect(missed?.identity.kind).toBe('missed_call')
    const newerId = itemAgoDays(response.items, seeded.anchor, 'manual', 1).identity.id
    const olderId = itemAgoDays(response.items, seeded.anchor, 'manual', 3).identity.id

    await page.goto(`/contacts/${contactId}`)
    const rows = historyRows(page)
    await expect(rows).toHaveCount(3)

    // spec: IXN-020.distinct-missed-call-row
    await expect(rows.nth(0)).toHaveAttribute('data-interaction-id', newerId)
    await expect(rows.nth(1)).toHaveAttribute('data-missed-call-id', missedId)
    await expect(rows.nth(2)).toHaveAttribute('data-interaction-id', olderId)
    const missedRow = rows.nth(1)
    await expect(missedRow).not.toHaveAttribute('data-interaction-id', /.*/)
    await expect(missedRow.locator('[data-badge]')).toHaveText('Missed call')
    await expect(
      missedRow.getByText(expectedDateTime(missed!.occurred_at), { exact: true })
    ).toBeVisible()
    await expect(missedRow.getByText('Missed', { exact: true })).toBeVisible()
    // spec: IXN-020.cadence-inert-shown
    await expect(missedRow.getByText('Not counted toward cadence', { exact: true })).toBeVisible()
    // spec: IXN-020.no-row-actions
    // spec: IXN-002.overflow-affordance-reserved
    await expect(missedRow.getByRole('button')).toHaveCount(0)
    await expect(rows.nth(0).getByRole('button', { name: 'More actions' })).toBeDisabled()
  })

  test('IXN-021: a call the user placed never shows as missed', async ({ page, request }) => {
    const seeded = await testApi.seedBehavior('IXN-021')
    const contactId = handleId(seeded, 'subject')
    const { items } = await fetchTimeline(request, contactId)
    const callAgoDays: Record<string, number> = {
      'outbound-connected': 1,
      'outbound-no-answer': 2,
      'inbound-voicemail': 3,
    }
    const row = (handle: string) =>
      page.locator(
        `[data-interaction-id="${itemAgoDays(items, seeded.anchor, 'phone_calls', callAgoDays[handle]).identity.id}"]`
      )

    await page.goto(`/contacts/${contactId}`)
    await expect(historyRows(page)).toHaveCount(3)

    // spec: IXN-021.connected-outbound-not-missed
    await expect(row('outbound-connected').getByText('Connected', { exact: true })).toBeVisible()
    await expect(row('outbound-connected').getByText(/missed/i)).toHaveCount(0)
    // spec: IXN-021.unanswered-outbound-not-missed
    await expect(row('outbound-no-answer').getByText('No answer', { exact: true })).toBeVisible()
    await expect(row('outbound-no-answer').getByText(/missed/i)).toHaveCount(0)
    // spec: IXN-021.missed-inbound-shows-missed
    await expect(row('inbound-voicemail').getByText('Missed', { exact: true })).toBeVisible()
    await expect(row('inbound-voicemail').getByText('Voicemail', { exact: true })).toBeVisible()
  })
})
