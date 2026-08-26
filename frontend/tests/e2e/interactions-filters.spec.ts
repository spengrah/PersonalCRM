import { test, expect, type APIRequestContext, type Page } from '@playwright/test'
import { createTestAPI, TestAPI, type SeedBehaviorResult } from './helpers/test-api'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

interface InteractionItem {
  id: string
  occurred_at: string
  venue_tags: Array<{ key: string; label: string }>
}

interface InteractionPage {
  items: InteractionItem[]
  venue_options: Array<{ key: string; label: string }>
}

interface ContactEvent {
  id: string
  start_time: string
}

interface InteractionContent {
  messages: Array<{ id: string; body: string; venue_key: string }>
}

const handleId = (seeded: SeedBehaviorResult, handle: string): string => {
  const entity = seeded.entities[handle]
  expect(entity, `declared handle ${handle} should be in the manifest`).toBeTruthy()
  return entity.id
}

async function fetchInteractions(
  request: APIRequestContext,
  contactId: string,
  limit?: number
): Promise<InteractionPage> {
  const params = limit ? `?limit=${limit}` : ''
  const response = await request.get(
    `${API_BASE_URL}/api/v1/contacts/${contactId}/interactions${params}`,
    { headers: API_HEADERS }
  )
  expect(response.ok()).toBe(true)
  return (await response.json()).data as InteractionPage
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
    const list = await fetchInteractions(request, contactId)
    const upcoming = await fetchUpcoming(request, contactId)
    const gchatId = handleId(seeded, 'gchat-thread')
    const emailId = handleId(seeded, 'email-thread')
    const futureId = handleId(seeded, 'future-noise')
    const gchat = list.items.find(item => item.id === gchatId)
    expect(gchat).toBeTruthy()
    const selectedVenue = gchat!.venue_tags[0].key

    await page.goto(`/contacts/${contactId}`)
    await expect(page.locator(`[data-interaction-id="${gchatId}"]`)).toBeVisible({ timeout: 15000 })
    await expect(page.locator(`[data-interaction-id="${emailId}"]`)).toBeVisible({ timeout: 15000 })
    await expect(page.locator(`[data-event-id="${futureId}"]`)).toBeVisible({ timeout: 15000 })

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
    await expect(page.locator(`[data-interaction-id="${gchatId}"]`)).toBeVisible({ timeout: 15000 })
    await expect(page.locator(`[data-interaction-id="${emailId}"]`)).toHaveCount(0, {
      timeout: 15000,
    })
    await expect(page.locator(`[data-event-id="${futureId}"]`)).toHaveCount(0, { timeout: 15000 })
    await expect(venueSelect.locator('option')).toHaveText(
      ['All venues', ...list.venue_options.map(option => option.label)],
      { timeout: 15000 }
    )

    await page
      .locator(`[data-interaction-id="${gchatId}"]`)
      .getByRole('button', { name: 'Expand content' })
      .click()
    const contentResponse = await request.get(
      `${API_BASE_URL}/api/v1/interactions/${gchatId}/content`,
      {
        headers: API_HEADERS,
      }
    )
    expect(contentResponse.ok()).toBe(true)
    const content = (await contentResponse.json()).data as InteractionContent
    const relevant = content.messages.filter(message => message.venue_key === selectedVenue)
    expect(relevant.length).toBeGreaterThan(0)
    const region = page.locator(`[data-interaction-id="${gchatId}"] [data-content-region]`)
    // spec: IXN-007.expanded-shows-venue-messages
    await expect(region.locator('[data-message-id]')).toHaveCount(relevant.length, {
      timeout: 15000,
    })
    for (const message of relevant) {
      const node = region.locator(`[data-message-id="${message.id}"]`)
      await expect(node).toHaveCount(1)
      await expect(node.locator('[data-message-body]')).toHaveText(message.body)
    }
  })

  test('bounds the history with 30 and 90 day presets while paging under the filter', async ({
    page,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-008')
    const contactId = handleId(seeded, 'subject')
    const recentId = handleId(seeded, 'recent-01')
    const edge29Id = handleId(seeded, 'edge-29')
    const edge31Id = handleId(seeded, 'edge-31')
    const edge89Id = handleId(seeded, 'edge-89')
    const edge91Id = handleId(seeded, 'edge-91')

    await page.goto(`/contacts/${contactId}`)
    await expect(historyRows(page)).toHaveCount(20, { timeout: 15000 })
    await expect(page.getByRole('list', { name: 'Upcoming events' })).toBeVisible({
      timeout: 15000,
    })

    await page.getByRole('button', { name: '30 days', exact: true }).click()
    await expect(page.getByRole('button', { name: '30 days', exact: true })).toHaveAttribute(
      'aria-pressed',
      'true',
      { timeout: 15000 }
    )
    // spec: IXN-008.preset-30-days
    await expect(page.locator(`[data-interaction-id="${recentId}"]`)).toBeVisible({
      timeout: 15000,
    })
    await expect(page.getByRole('list', { name: 'Upcoming events' })).toHaveCount(0, {
      timeout: 15000,
    })
    await page.getByRole('button', { name: 'Load more', exact: true }).click()
    await expect(page.locator(`[data-interaction-id="${edge29Id}"]`)).toBeVisible({
      timeout: 15000,
    })
    await expect(historyRows(page)).toHaveCount(21, { timeout: 15000 })
    await expect(page.locator(`[data-interaction-id="${edge31Id}"]`)).toHaveCount(0, {
      timeout: 15000,
    })
    await expect(page.locator(`[data-interaction-id="${edge89Id}"]`)).toHaveCount(0, {
      timeout: 15000,
    })
    await expect(page.locator(`[data-interaction-id="${edge91Id}"]`)).toHaveCount(0, {
      timeout: 15000,
    })
    await expect(page.getByRole('button', { name: 'Load more', exact: true })).toHaveCount(0, {
      timeout: 15000,
    })

    await page.getByRole('button', { name: '90 days', exact: true }).click()
    await expect(page.locator(`[data-interaction-id="${recentId}"]`)).toBeVisible({
      timeout: 15000,
    })
    await page.getByRole('button', { name: 'Load more', exact: true }).click()
    // spec: IXN-008.preset-90-days
    await expect(page.locator(`[data-interaction-id="${edge89Id}"]`)).toBeVisible({
      timeout: 15000,
    })
    await expect(historyRows(page)).toHaveCount(23, { timeout: 15000 })
    await expect(page.locator(`[data-interaction-id="${edge31Id}"]`)).toBeVisible({
      timeout: 15000,
    })
    await expect(page.locator(`[data-interaction-id="${edge91Id}"]`)).toHaveCount(0, {
      timeout: 15000,
    })
    await expect(page.getByRole('button', { name: 'Load more', exact: true })).toHaveCount(0, {
      timeout: 15000,
    })
  })

  test('applies a custom range across past and upcoming and restores everything with All', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-008')
    const contactId = handleId(seeded, 'subject')
    const recentId = handleId(seeded, 'recent-01')
    const edge31Id = handleId(seeded, 'edge-31')
    const edge91Id = handleId(seeded, 'edge-91')
    const boundAId = handleId(seeded, 'bound-a')
    const boundBId = handleId(seeded, 'bound-b')
    const boundCId = handleId(seeded, 'bound-c')
    const upcoming = await fetchUpcoming(request, contactId)
    const event = (id: string) => {
      const value = upcoming.find(item => item.id === id)
      expect(value).toBeTruthy()
      return value!
    }
    const edge31 = (await fetchInteractions(request, contactId, 100)).items.find(
      item => item.id === edge31Id
    )
    expect(edge31).toBeTruthy()

    await page.goto(`/contacts/${contactId}`)
    await page.getByRole('button', { name: 'Custom', exact: true }).click()
    await page
      .getByLabel('Start date')
      .fill(new Date(edge31!.occurred_at).toISOString().slice(0, 10))
    await page.getByLabel('End date').fill(new Date(edge31!.occurred_at).toISOString().slice(0, 10))
    // spec: IXN-008.custom-range
    await expect(page.locator(`[data-interaction-id="${edge31Id}"]`)).toBeVisible({
      timeout: 15000,
    })
    await expect(historyRows(page)).toHaveCount(1, { timeout: 15000 })
    await expect(page.getByRole('list', { name: 'Upcoming events' })).toHaveCount(0, {
      timeout: 15000,
    })

    const beforeBoundB = new Date(event(boundBId).start_time)
    beforeBoundB.setUTCDate(beforeBoundB.getUTCDate() - 1)
    await page.getByLabel('Start date').fill(dateOf(event(boundAId)))
    await page.getByLabel('End date').fill(beforeBoundB.toISOString().slice(0, 10))
    // spec: IXN-008.custom-range-bounds-upcoming
    await expect(page.locator(`[data-event-id="${boundAId}"]`)).toBeVisible({ timeout: 15000 })
    await expect(page.locator(`[data-event-id="${boundBId}"]`)).toHaveCount(0, { timeout: 15000 })
    await expect(page.locator(`[data-event-id="${boundCId}"]`)).toHaveCount(0, { timeout: 15000 })
    await expect(historyRows(page)).toHaveCount(0, { timeout: 15000 })

    await page.getByRole('button', { name: 'All', exact: true }).click()
    // spec: IXN-008.all-includes-upcoming
    await expect(page.locator(`[data-interaction-id="${recentId}"]`)).toBeVisible({
      timeout: 15000,
    })
    await expect(page.locator(`[data-event-id="${boundAId}"]`)).toBeVisible({ timeout: 15000 })
    await expect(page.locator(`[data-event-id="${boundBId}"]`)).toBeVisible({ timeout: 15000 })
    await expect(page.locator(`[data-event-id="${boundCId}"]`)).toBeVisible({ timeout: 15000 })
    await page.getByRole('button', { name: 'Load more', exact: true }).click()
    await expect(page.locator(`[data-interaction-id="${edge91Id}"]`)).toBeVisible({
      timeout: 15000,
    })
    await expect(historyRows(page)).toHaveCount(24, { timeout: 15000 })
  })
})
