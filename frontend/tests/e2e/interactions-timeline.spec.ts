import { test, expect, type Page } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from './helpers/timeouts'
import {
  API_BASE_URL,
  API_HEADERS,
  createTestAPI,
  TestAPI,
  type SeedBehaviorResult,
} from './helpers/test-api'
import { fetchTimeline, fetchTimelineContent, itemAgoDays } from './helpers/timeline-items'

const handleId = (seeded: SeedBehaviorResult, handle: string): string => {
  const entity = seeded.entities[handle]
  expect(entity, `declared handle ${handle} should be in the manifest`).toBeTruthy()
  return entity.id
}

function historyRows(page: Page) {
  return page.getByRole('list', { name: 'Interaction history' }).getByRole('listitem')
}

test.describe('Merged interactions @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  // spec: IXN-019.one-row-every-member-source
  // spec: IXN-019.member-content-in-own-section
  // spec: IXN-019.content-attributed-to-own-channel
  test('renders a calendar interaction merged with a call as one row with a section per member', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-019')
    const contactId = handleId(seeded, 'subject')
    const { items } = await fetchTimeline(request, contactId)
    const calendar = itemAgoDays(items, seeded.anchor, 'gcal', 2)
    const call = itemAgoDays(items, seeded.anchor, 'phone_calls', 2)

    const merge = await request.post(`${API_BASE_URL}/api/v1/interactions/merge`, {
      headers: API_HEADERS,
      data: { interaction_ids: [calendar.identity.id, call.identity.id], contact_id: contactId },
    })
    expect(merge.ok()).toBe(true)
    const mergedId = (await merge.json()).data.interaction_id as string
    const absorbedId = [calendar.identity.id, call.identity.id].find(id => id !== mergedId)

    await page.goto(`/contacts/${contactId}`)
    // The merged interaction and the unmerged calendar interaction are the only rows.
    await expect(historyRows(page)).toHaveCount(2, { timeout: SLOW_TIMEOUT_MS })
    const row = page.locator(`[data-interaction-id="${mergedId}"]`)
    await expect(row.locator('[data-badge]')).toHaveText(['Calendar', 'Call'], {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(page.locator(`[data-interaction-id="${absorbedId}"]`)).toHaveCount(0)

    await row.getByRole('button', { name: 'Expand content', exact: true }).click()
    const content = await fetchTimelineContent(
      request,
      { kind: 'interaction', id: mergedId },
      contactId
    )
    const note = content.members[0].meeting_notes[0]
    const sections = row.locator('[data-member-section]')
    await expect(sections).toHaveCount(2, { timeout: SLOW_TIMEOUT_MS })
    await expect(sections.nth(0)).toHaveAttribute('data-member-section', 'gcal')
    await expect(sections.nth(0).locator('[data-note-summary]')).toHaveText(note.summary!, {
      timeout: SLOW_TIMEOUT_MS,
    })
    const channel = sections.nth(0).locator('[data-channel]')
    await expect(channel).toHaveAttribute('data-channel', note.channel)
    await expect(channel).toHaveText('Meeting')
    await expect(sections.nth(1)).toHaveAttribute('data-member-section', 'phone_calls')
    await expect(sections.nth(1).getByRole('heading', { name: 'Call', exact: true })).toBeVisible()
    await expect(sections.nth(1).locator('[data-meeting-note]')).toHaveCount(0)
  })

  // spec: IXN-019.unmerged-shows-its-one-source
  test('renders an unmerged calendar interaction with its one source and its note', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-019')
    const contactId = handleId(seeded, 'subject')
    const { items } = await fetchTimeline(request, contactId)
    const solo = itemAgoDays(items, seeded.anchor, 'gcal', 5)

    await page.goto(`/contacts/${contactId}`)
    await expect(historyRows(page)).toHaveCount(3, { timeout: SLOW_TIMEOUT_MS })
    const row = page.locator(`[data-interaction-id="${solo.identity.id}"]`)
    await expect(row.locator('[data-badge]')).toHaveText(['Calendar'], {
      timeout: SLOW_TIMEOUT_MS,
    })
    await row.getByRole('button', { name: 'Expand content', exact: true }).click()
    const content = await fetchTimelineContent(request, solo.identity, contactId)
    const note = content.members[0].meeting_notes[0]
    await expect(row.locator('[data-member-section]')).toHaveCount(1, {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(row.locator('[data-note-summary]')).toHaveText(note.summary!, {
      timeout: SLOW_TIMEOUT_MS,
    })
  })
})
