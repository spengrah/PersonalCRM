import { test, expect, type Page } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from './helpers/timeouts'
import {
  API_BASE_URL,
  API_HEADERS,
  createTestAPI,
  TestAPI,
  type SeedBehaviorResult,
} from './helpers/test-api'
import { fetchTimeline, itemAgoDays, itemWithSource } from './helpers/timeline-items'

const handle = (seeded: SeedBehaviorResult, name: string) => {
  const entity = seeded.entities[name]
  expect(entity, `declared handle ${name} should be in the manifest`).toBeTruthy()
  return entity
}

function historyRows(page: Page) {
  return page.getByRole('list', { name: 'Interaction history' }).getByRole('listitem')
}

function recentActivity(page: Page) {
  return page
    .locator('dt', { hasText: 'Recent activity' })
    .locator('xpath=following-sibling::dd[1]')
}

test.describe('Interaction corrections @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('only a correctable interaction offers Remove and Merge', async ({ page, request }) => {
    const seeded = await testApi.seedBehavior('IXN-037')
    const contactId = handle(seeded, 'subject').id
    const { items } = await fetchTimeline(request, contactId)
    const meeting = itemAgoDays(items, seeded.anchor, 'gcal', 2)
    const burst = itemWithSource(items, 'messages')
    const missed = items.find(item => item.identity.kind === 'missed_call')
    expect(burst.identity.kind).toBe('message_burst')
    expect(missed).toBeTruthy()

    await page.goto(`/contacts/${contactId}`)
    await expect(historyRows(page)).toHaveCount(4, { timeout: SLOW_TIMEOUT_MS })

    // spec: IXN-037.menu-offers-remove
    // spec: IXN-038.menu-offers-merge
    // spec: IXN-002.overflow-affordance-reserved
    const meetingRow = page.locator(`[data-interaction-id="${meeting.identity.id}"]`)
    await meetingRow.getByRole('button', { name: 'More actions' }).click()
    await expect(meetingRow.getByRole('menuitem')).toHaveText(['Remove', 'Merge'])
    await expect(
      page.locator(`[data-interaction-id="${burst.identity.id}"]`).getByRole('button', {
        name: 'More actions',
      })
    ).toHaveCount(0)
    await expect(
      page.locator(`[data-missed-call-id="${missed!.identity.id}"]`).getByRole('button')
    ).toHaveCount(0)
    const upcoming = page.getByRole('list', { name: 'Upcoming events' }).getByRole('listitem')
    await expect(upcoming).toHaveCount(1)
    await expect(upcoming.getByRole('button')).toHaveCount(0)
  })

  test('removing with the default scope removes only the current contact', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-037')
    const subject = handle(seeded, 'subject')
    const coAttendee = handle(seeded, 'co-attendee')
    const { items } = await fetchTimeline(request, subject.id)
    const meeting = itemAgoDays(items, seeded.anchor, 'gcal', 2)

    await page.goto(`/contacts/${subject.id}`)
    const meetingRow = page.locator(`[data-interaction-id="${meeting.identity.id}"]`)
    await expect(meetingRow).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    const activityBefore = (await recentActivity(page).textContent()) ?? ''
    expect(activityBefore).toContain('Last outreach')

    await meetingRow.getByRole('button', { name: 'More actions' }).click()
    await meetingRow.getByRole('menuitem', { name: 'Remove' }).click()
    const dialog = page.getByRole('dialog', { name: 'Remove interaction' })

    // spec: IXN-037.names-interaction
    await expect(dialog.getByText(meeting.members[0].label, { exact: true })).toBeVisible()
    // spec: IXN-037.attendees-current-preselected
    const attendees = dialog.getByRole('group', { name: 'Attendees' })
    await expect(attendees.getByRole('checkbox', { name: subject.name })).toBeChecked()
    await expect(attendees.getByRole('checkbox', { name: coAttendee.name })).not.toBeChecked()
    await expect(dialog.getByRole('checkbox', { name: /^Everyone/ })).not.toBeChecked()
    // spec: IXN-037.reason-required
    const submit = dialog.getByRole('button', { name: 'Remove', exact: true })
    await expect(submit).toBeDisabled()
    await dialog.getByRole('radio', { name: /^Didn't attend/ }).check()
    await expect(submit).toBeEnabled()

    await submit.click()
    await expect(dialog).toHaveCount(0)

    // spec: IXN-037.default-removes-current-only
    // spec: IXN-037.cadence-refreshes-without-reload
    await expect(meetingRow).toHaveCount(0, { timeout: SLOW_TIMEOUT_MS })
    await expect(recentActivity(page)).not.toHaveText(activityBefore, { timeout: SLOW_TIMEOUT_MS })
    const activityAfter = (await recentActivity(page).textContent()) ?? ''
    const coTimeline = await fetchTimeline(request, coAttendee.id)
    expect(coTimeline.items.map(item => item.identity.id)).toContain(meeting.identity.id)
    const subjectTimeline = await fetchTimeline(request, subject.id)
    expect(subjectTimeline.items.map(item => item.identity.id)).not.toContain(meeting.identity.id)

    await page.reload()
    await expect(historyRows(page)).toHaveCount(3, { timeout: SLOW_TIMEOUT_MS })
    await expect(recentActivity(page)).toHaveText(activityAfter)
  })

  test('removing for everyone removes every attendee', async ({ page, request }) => {
    const seeded = await testApi.seedBehavior('IXN-037')
    const subject = handle(seeded, 'subject')
    const coAttendee = handle(seeded, 'co-attendee')
    const { items } = await fetchTimeline(request, subject.id)
    const meeting = itemAgoDays(items, seeded.anchor, 'gcal', 2)

    await page.goto(`/contacts/${subject.id}`)
    const meetingRow = page.locator(`[data-interaction-id="${meeting.identity.id}"]`)
    await meetingRow
      .getByRole('button', { name: 'More actions' })
      .click({ timeout: SLOW_TIMEOUT_MS })
    await meetingRow.getByRole('menuitem', { name: 'Remove' }).click()
    const dialog = page.getByRole('dialog', { name: 'Remove interaction' })
    await expect(
      dialog
        .getByRole('group', { name: 'Attendees' })
        .getByRole('checkbox', { name: coAttendee.name })
    ).toBeVisible()

    // spec: IXN-037.everyone-removes-every-attendee
    await dialog.getByRole('checkbox', { name: /^Everyone/ }).check()
    await dialog.getByRole('radio', { name: /^Didn't happen/ }).check()
    await dialog.getByRole('button', { name: 'Remove', exact: true }).click()

    await expect(meetingRow).toHaveCount(0, { timeout: SLOW_TIMEOUT_MS })
    for (const contact of [subject, coAttendee]) {
      const timeline = await fetchTimeline(request, contact.id)
      expect(timeline.items.map(item => item.identity.id)).not.toContain(meeting.identity.id)
    }
  })

  test('merging selected interactions shows one row as the confirmation stated', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-038')
    const contactId = handle(seeded, 'subject').id
    const { items } = await fetchTimeline(request, contactId)
    const meeting = itemAgoDays(items, seeded.anchor, 'gcal', 2)
    const call = itemAgoDays(items, seeded.anchor, 'phone_calls', 1)
    const burst = itemWithSource(items, 'messages')
    const missed = items.find(item => item.identity.kind === 'missed_call')
    expect(missed).toBeTruthy()

    await page.goto(`/contacts/${contactId}`)
    await expect(historyRows(page)).toHaveCount(4, { timeout: SLOW_TIMEOUT_MS })
    const meetingRow = page.locator(`[data-interaction-id="${meeting.identity.id}"]`)
    const callRow = page.locator(`[data-interaction-id="${call.identity.id}"]`)

    // spec: IXN-038.menu-offers-merge
    await meetingRow.getByRole('button', { name: 'More actions' }).click()
    await meetingRow.getByRole('menuitem', { name: 'Merge' }).click()
    await expect(meetingRow.getByRole('checkbox', { name: 'Select interaction' })).toBeChecked()

    // spec: IXN-038.only-correctable-selectable
    await expect(page.getByRole('checkbox', { name: 'Select interaction' })).toHaveCount(2)
    await expect(
      page.locator(`[data-interaction-id="${burst.identity.id}"]`).getByRole('checkbox')
    ).toHaveCount(0)
    await expect(
      page.locator(`[data-missed-call-id="${missed!.identity.id}"]`).getByRole('checkbox')
    ).toHaveCount(0)

    // spec: IXN-038.two-needed
    const review = page.getByRole('button', { name: 'Review merge' })
    await expect(review).toBeDisabled()
    await callRow.getByRole('checkbox', { name: 'Select interaction' }).check()
    await expect(review).toBeEnabled()
    await review.click()

    const dialog = page.getByRole('dialog', { name: 'Merge interactions' })
    // spec: IXN-038.confirmation-side-by-side
    const members = dialog
      .getByRole('list', { name: 'Interactions to merge' })
      .getByRole('listitem')
    await expect(members).toHaveCount(2)
    await expect(members.nth(0).getByText(meeting.members[0].label, { exact: true })).toBeVisible()
    await expect(members.nth(1).getByText(call.members[0].label, { exact: true })).toBeVisible()
    await expect(dialog.getByText(/every attendee/)).toBeVisible()

    // spec: IXN-038.states-preview-result
    const preview = await request.post(`${API_BASE_URL}/api/v1/interactions/merge/preview`, {
      headers: API_HEADERS,
      data: { interaction_ids: [meeting.identity.id, call.identity.id], contact_id: contactId },
    })
    expect(preview.ok()).toBe(true)
    const previewed = (await preview.json()).data as {
      interaction_id: string
      participation: { direction: string; occurred_at: string }
    }
    const result = dialog.locator('[data-merge-result]')
    await expect(result).toHaveAttribute('data-direction', previewed.participation.direction, {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(result).toContainText(previewed.participation.direction)
    const statedAt = await result.locator('time').getAttribute('datetime')
    expect(new Date(statedAt!).getTime()).toBe(
      new Date(previewed.participation.occurred_at).getTime()
    )
    const statedTimeText = await result.locator('time').innerText()

    await dialog.getByRole('button', { name: 'Merge', exact: true }).click()
    await expect(dialog).toHaveCount(0, { timeout: SLOW_TIMEOUT_MS })

    // spec: IXN-038.one-row-as-stated
    await expect(historyRows(page)).toHaveCount(3, { timeout: SLOW_TIMEOUT_MS })
    const merged = page.locator(`[data-interaction-id="${previewed.interaction_id}"]`)
    await expect(merged.locator('[data-badge]')).toHaveText(['Calendar', 'Call'])
    await expect(
      merged.getByLabel(previewed.participation.direction, { exact: true })
    ).toBeVisible()
    await expect(merged.locator('time').first()).toHaveText(statedTimeText)
  })
})
