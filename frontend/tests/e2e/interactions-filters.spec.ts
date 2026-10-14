import { test, expect, type APIRequestContext, type Page } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from './helpers/timeouts'
import {
  API_BASE_URL,
  API_HEADERS,
  createTestAPI,
  TestAPI,
  type SeedBehaviorResult,
} from './helpers/test-api'
import {
  fetchTimeline,
  fetchTimelineContent,
  itemAgoDays,
  itemWithSource,
} from './helpers/timeline-items'

interface ContactEvent {
  id: string
  start_time: string
}

const handleId = (seeded: SeedBehaviorResult, handle: string): string => {
  const entity = seeded.entities[handle]
  expect(entity, `declared handle ${handle} should be in the manifest`).toBeTruthy()
  return entity.id
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

function historyRows(page: Page) {
  return page.getByRole('list', { name: 'Interaction history' }).getByRole('listitem')
}

function upcomingCards(page: Page) {
  return page.getByRole('list', { name: 'Upcoming events' }).getByRole('listitem')
}

function dateOf(event: ContactEvent): string {
  return new Date(event.start_time).toISOString().slice(0, 10)
}

test.use({
  // Date-only custom-range inputs resolve at browser-local midnight; UTC makes
  // that equal the UTC-midnight boundary events seeded by the declaration.
  timezoneId: 'UTC',
})

test.describe('Interactions Filters @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('filters the history to a venue and its expanded evidence', async ({ page, request }) => {
    const seeded = await testApi.seedBehavior('IXN-007')
    const contactId = handleId(seeded, 'subject')
    const list = await fetchTimeline(request, contactId)
    const gchat = itemWithSource(list.items, 'gchat')
    const gchatId = gchat.identity.id
    const emailId = itemWithSource(list.items, 'email').identity.id
    const futureId = handleId(seeded, 'future-noise')
    const selectedVenue = gchat.members[0].venue_tags[0].key

    await page.goto(`/contacts/${contactId}`)
    await expect(page.locator(`[data-interaction-id="${gchatId}"]`)).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(page.locator(`[data-interaction-id="${emailId}"]`)).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(page.locator(`[data-event-id="${futureId}"]`)).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })

    const venueSelect = page.getByRole('combobox', { name: 'Venue' })
    await expect(venueSelect.locator('option')).toHaveText([
      'All venues',
      ...list.venue_options.map(option => option.label),
    ])
    await expect(venueSelect.locator('option').first()).toHaveAttribute('value', '')
    await expect(venueSelect.locator('option').nth(1)).toHaveAttribute(
      'value',
      list.venue_options[0].key
    )
    await expect(venueSelect.locator('option').nth(2)).toHaveAttribute(
      'value',
      list.venue_options[1].key
    )

    await venueSelect.selectOption({ value: selectedVenue })
    // spec: IXN-007.venue-narrows-list
    await expect(page.locator(`[data-interaction-id="${gchatId}"]`)).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(page.locator(`[data-interaction-id="${emailId}"]`)).toHaveCount(0, {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(page.locator(`[data-event-id="${futureId}"]`)).toHaveCount(0, {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(venueSelect.locator('option')).toHaveText(
      ['All venues', ...list.venue_options.map(option => option.label)],
      { timeout: SLOW_TIMEOUT_MS }
    )

    await page
      .locator(`[data-interaction-id="${gchatId}"]`)
      .getByRole('button', { name: 'Expand content' })
      .click()
    const content = await fetchTimelineContent(request, gchat.identity, contactId)
    const relevant = content.members
      .flatMap(member => member.messages)
      .filter(message => message.container === selectedVenue)
    expect(relevant.length).toBeGreaterThan(0)
    const region = page.locator(`[data-interaction-id="${gchatId}"] [data-content-region]`)
    // spec: IXN-007.expanded-shows-venue-messages
    await expect(region.locator('[data-message-id]')).toHaveCount(relevant.length, {
      timeout: SLOW_TIMEOUT_MS,
    })
    for (const message of relevant) {
      const node = region.locator(`[data-message-id="${message.id}"]`)
      await expect(node).toHaveCount(1)
      await expect(node.locator('[data-message-body]')).toHaveText(message.body)
    }
  })

  test('bounds the history with 30 and 90 day presets while paging under the filter', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-008')
    const contactId = handleId(seeded, 'subject')
    const { items } = await fetchTimeline(request, contactId, { limit: 100 })
    const at = (agoDays: number) => itemAgoDays(items, seeded.anchor, 'manual', agoDays).identity.id
    const recentId = at(1)
    const edge29Id = at(29)
    const edge31Id = at(31)
    const edge89Id = at(89)
    const edge91Id = at(91)

    await page.goto(`/contacts/${contactId}`)
    await expect(historyRows(page)).toHaveCount(20, { timeout: SLOW_TIMEOUT_MS })
    await expect(page.getByRole('list', { name: 'Upcoming events' })).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })

    await page.getByRole('button', { name: '30 days', exact: true }).click()
    await expect(page.getByRole('button', { name: '30 days', exact: true })).toHaveAttribute(
      'aria-pressed',
      'true',
      { timeout: SLOW_TIMEOUT_MS }
    )
    // spec: IXN-008.preset-30-days
    await expect(page.locator(`[data-interaction-id="${recentId}"]`)).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(page.getByRole('list', { name: 'Upcoming events' })).toHaveCount(0, {
      timeout: SLOW_TIMEOUT_MS,
    })
    await page.getByRole('button', { name: 'Load more', exact: true }).click()
    await expect(page.locator(`[data-interaction-id="${edge29Id}"]`)).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(historyRows(page)).toHaveCount(21, { timeout: SLOW_TIMEOUT_MS })
    await expect(page.locator(`[data-interaction-id="${edge31Id}"]`)).toHaveCount(0, {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(page.locator(`[data-interaction-id="${edge89Id}"]`)).toHaveCount(0, {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(page.locator(`[data-interaction-id="${edge91Id}"]`)).toHaveCount(0, {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(page.getByRole('button', { name: 'Load more', exact: true })).toHaveCount(0, {
      timeout: SLOW_TIMEOUT_MS,
    })

    await page.getByRole('button', { name: '90 days', exact: true }).click()
    await expect(page.locator(`[data-interaction-id="${recentId}"]`)).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    await page.getByRole('button', { name: 'Load more', exact: true }).click()
    // spec: IXN-008.preset-90-days
    await expect(page.locator(`[data-interaction-id="${edge89Id}"]`)).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(historyRows(page)).toHaveCount(23, { timeout: SLOW_TIMEOUT_MS })
    await expect(page.locator(`[data-interaction-id="${edge31Id}"]`)).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(page.locator(`[data-interaction-id="${edge91Id}"]`)).toHaveCount(0, {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(page.getByRole('button', { name: 'Load more', exact: true })).toHaveCount(0, {
      timeout: SLOW_TIMEOUT_MS,
    })
  })

  test('applies a custom range across past and upcoming and restores everything with All', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-008')
    const contactId = handleId(seeded, 'subject')
    const { items } = await fetchTimeline(request, contactId, { limit: 100 })
    const at = (agoDays: number) => itemAgoDays(items, seeded.anchor, 'manual', agoDays)
    const recentId = at(1).identity.id
    const edge31 = at(31)
    const edge31Id = edge31.identity.id
    const edge91Id = at(91).identity.id
    const boundAId = handleId(seeded, 'bound-a')
    const boundBId = handleId(seeded, 'bound-b')
    const boundCId = handleId(seeded, 'bound-c')
    const upcoming = await fetchUpcoming(request, contactId)
    const event = (id: string) => {
      const value = upcoming.find(item => item.id === id)
      expect(value).toBeTruthy()
      return value!
    }
    await page.goto(`/contacts/${contactId}`)
    await page.getByRole('button', { name: 'Custom', exact: true }).click()
    await page
      .getByLabel('Start date')
      .fill(new Date(edge31.occurred_at).toISOString().slice(0, 10))
    await page.getByLabel('End date').fill(new Date(edge31.occurred_at).toISOString().slice(0, 10))
    // spec: IXN-008.custom-range
    await expect(page.locator(`[data-interaction-id="${edge31Id}"]`)).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(historyRows(page)).toHaveCount(1, { timeout: SLOW_TIMEOUT_MS })
    await expect(page.getByRole('list', { name: 'Upcoming events' })).toHaveCount(0, {
      timeout: SLOW_TIMEOUT_MS,
    })

    const beforeBoundB = new Date(event(boundBId).start_time)
    beforeBoundB.setUTCDate(beforeBoundB.getUTCDate() - 1)
    await page.getByLabel('Start date').fill(dateOf(event(boundAId)))
    await page.getByLabel('End date').fill(beforeBoundB.toISOString().slice(0, 10))
    // spec: IXN-008.custom-range-bounds-upcoming
    await expect(page.locator(`[data-event-id="${boundAId}"]`)).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(page.locator(`[data-event-id="${boundBId}"]`)).toHaveCount(0, {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(page.locator(`[data-event-id="${boundCId}"]`)).toHaveCount(0, {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(historyRows(page)).toHaveCount(0, { timeout: SLOW_TIMEOUT_MS })

    await page.getByRole('button', { name: 'All', exact: true }).click()
    // spec: IXN-008.all-includes-upcoming
    await expect(page.locator(`[data-interaction-id="${recentId}"]`)).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(page.locator(`[data-event-id="${boundAId}"]`)).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(page.locator(`[data-event-id="${boundBId}"]`)).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(page.locator(`[data-event-id="${boundCId}"]`)).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    await page.getByRole('button', { name: 'Load more', exact: true }).click()
    await expect(page.locator(`[data-interaction-id="${edge91Id}"]`)).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(historyRows(page)).toHaveCount(24, { timeout: SLOW_TIMEOUT_MS })
  })
})
