import { test, expect, type Locator } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from './helpers/timeouts'
import { createTestAPI, TestAPI } from './helpers/test-api'
import {
  benchRow,
  contactRows,
  entity,
  expandRow,
  fetchAllInteractions,
  rowMessages,
  seedJudgmentRun,
} from './helpers/bench'

/** The view's rows' identities, in the order shown. */
async function shownIds(list: Locator): Promise<string[]> {
  return list
    .locator(':scope > [role="listitem"]')
    .evaluateAll(nodes =>
      nodes.map(
        node =>
          node.getAttribute('data-interaction-id') ?? node.getAttribute('data-missed-call-id') ?? ''
      )
    )
}

const minuteAgo = () => new Date(Date.now() - 60_000).toISOString()

test.describe('Interactions bench list @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('lists every interaction a contact takes part in, newest first, over pages', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-041')
    const subject = entity(seeded, 'subject')
    const rows = await contactRows(request, subject.id)
    expect(rows).toHaveLength(27)
    const ids = rows.map(row => row.item.identity.id)

    await page.goto(`/interactions?contact=${subject.id}`)
    const list = page.getByRole('list', { name: 'Interactions' })

    // spec: IXN-041.paginated-without-gaps, IXN-041.newest-first-tie-breaker, IXN-042.filters-combine
    await expect(list.locator(':scope > [role="listitem"]')).toHaveCount(20, {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(page.getByTestId('pagination')).toContainText(
      'Page 1 of 2 (27 total interactions)'
    )
    const first = await shownIds(list)
    expect(first).toEqual(ids.slice(0, 20))
    await page.getByTestId('pagination').getByRole('button', { name: 'Next' }).click()
    await expect(page).toHaveURL(/page=2/)
    await expect(list.locator(':scope > [role="listitem"]')).toHaveCount(7, {
      timeout: SLOW_TIMEOUT_MS,
    })
    const second = await shownIds(list)
    expect(second).toEqual(ids.slice(20))
    expect(new Set([...first, ...second]).size).toBe(27)
    const ties = rows.filter(row => row.item.occurred_at === rows[5].item.occurred_at)
    expect(ties.length, 'the two manual entries share an instant').toBe(2)
    await page.reload()
    await expect(list.locator(':scope > [role="listitem"]')).toHaveCount(7, {
      timeout: SLOW_TIMEOUT_MS,
    })
    expect(await shownIds(list)).toEqual(second)

    // spec: IXN-041.every-interaction-represented
    await page.goto(`/interactions?contact=${subject.id}`)
    await expect(list.locator(':scope > [role="listitem"]')).toHaveCount(20, {
      timeout: SLOW_TIMEOUT_MS,
    })
    const sources = await list
      .locator(':scope > [role="listitem"]')
      .evaluateAll(nodes => nodes.map(node => node.getAttribute('data-source')))
    expect(new Set(sources)).toEqual(new Set(['gchat', 'gcal', 'phone_calls', 'email', 'manual']))

    // spec: IXN-041.shared-interaction-one-row
    const groupRow = rows.find(row => row.participants.length === 3)
    const meetingRow = rows.find(row => row.item.members[0]?.source === 'gcal')
    expect(groupRow, 'the user’s group message is one row of its three members').toBeTruthy()
    const groupParticipants = benchRow(page, groupRow!.item.identity.id).locator(
      '[data-participants]'
    )
    for (const handle of ['subject', 'member-b', 'member-c']) {
      await expect(groupParticipants).toContainText(entity(seeded, handle).name)
    }
    const meetingParticipants = benchRow(page, meetingRow!.item.identity.id).locator(
      '[data-participants]'
    )
    await expect(meetingParticipants).toContainText(subject.name)
    await expect(meetingParticipants).toContainText(entity(seeded, 'co-attendee').name)
    const coAttendee = await contactRows(request, entity(seeded, 'co-attendee').id)
    expect(coAttendee.map(row => row.item.identity.id)).toEqual([meetingRow!.item.identity.id])

    // spec: IXN-041.row-sets-out-columns
    const emailRow = rows.find(row => row.item.members[0]?.source === 'email')!
    const email = benchRow(page, emailRow.item.identity.id)
    await expect(email.getByLabel(emailRow.item.direction, { exact: true })).toBeVisible()
    await expect(email.locator('[data-badge]')).toHaveText('Email')
    await expect(
      email.locator('[data-participants]').getByRole('link', { name: subject.name })
    ).toHaveAttribute('href', `/contacts/${subject.id}`)
    await expect(email.locator('[data-preview]')).toHaveText(
      `${emailRow.preview!.sender}: ${emailRow.preview!.text}`
    )
    await expect(email.locator('time')).toHaveAttribute('datetime', emailRow.item.occurred_at)
    await expect(email.locator('[data-review-indicator]')).toHaveCount(0)
    const [participantsBox, previewBox, dateBox] = await Promise.all([
      email.locator('[data-participants]').boundingBox(),
      email.locator('[data-preview]').boundingBox(),
      email.locator('time').boundingBox(),
    ])
    // The preview is the widest column and reaches the date column.
    expect(previewBox!.width).toBeGreaterThan(participantsBox!.width)
    expect(dateBox!.x - (previewBox!.x + previewBox!.width)).toBeLessThanOrEqual(20)
  })

  test('renders each row with the contact history row and drill-down, keeping its actions', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-041')
    const subject = entity(seeded, 'subject')
    const rows = await contactRows(request, subject.id)
    await page.goto(`/interactions?contact=${subject.id}`)

    // spec: IXN-041.renders-with-history-row, IXN-041.drill-down-full-width
    const groupRow = rows.find(row => row.participants.length === 3)!
    const group = benchRow(page, groupRow.item.identity.id)
    await expect(group.getByText('Group', { exact: true })).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(group.getByRole('button', { name: 'More actions' })).toHaveCount(0)
    const region = await expandRow(group)
    const messages = await rowMessages(request, groupRow)
    await expect(region.locator('[data-message-id]')).toHaveCount(messages.length, {
      timeout: SLOW_TIMEOUT_MS,
    })
    const [groupBox, regionBox] = await Promise.all([group.boundingBox(), region.boundingBox()])
    expect(regionBox!.width).toBeGreaterThan(groupBox!.width * 0.9)

    const callRow = rows.find(row => row.item.members[0]?.source === 'phone_calls')!
    const call = benchRow(page, callRow.item.identity.id)
    await expect(call.locator('[data-preview]')).toHaveText('Voice call · Answered · 6m 12s')
    await call.getByRole('button', { name: 'More actions' }).click()
    await expect(call.getByRole('menuitem')).toHaveText(['Remove', 'Merge'])
  })

  test('merges two rows into one from the view', async ({ page, request }) => {
    const seeded = await testApi.seedBehavior('IXN-041')
    const subject = entity(seeded, 'subject')
    const rows = await contactRows(request, subject.id)
    const callRow = rows.find(row => row.item.members[0]?.source === 'phone_calls')!
    const meetingRow = rows.find(row => row.item.members[0]?.source === 'gcal')!
    await page.goto(`/interactions?contact=${subject.id}`)

    // spec: IXN-041.renders-with-history-row
    const call = benchRow(page, callRow.item.identity.id)
    await call.getByRole('button', { name: 'More actions' }).click({ timeout: SLOW_TIMEOUT_MS })
    await call.getByRole('menuitem', { name: 'Merge' }).click()
    await benchRow(page, meetingRow.item.identity.id)
      .getByRole('checkbox', { name: 'Select interaction' })
      .check()
    await page
      .getByRole('region', { name: 'Merge selection' })
      .getByRole('button', { name: 'Review merge' })
      .click()
    const dialog = page.getByRole('dialog')
    await expect(dialog.locator('[data-merge-result]')).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    await dialog.getByRole('button', { name: 'Merge', exact: true }).click()
    await expect(dialog).toHaveCount(0, { timeout: SLOW_TIMEOUT_MS })

    const after = await contactRows(request, subject.id)
    expect(after).toHaveLength(rows.length - 1)
    const merged = after.find(row => row.item.members.length === 2)!
    expect(merged.item.members.map(m => m.source).sort()).toEqual(['gcal', 'phone_calls'])
    const mergedRow = benchRow(page, merged.item.identity.id)
    await expect(mergedRow.locator('[data-badge]')).toHaveCount(2, { timeout: SLOW_TIMEOUT_MS })
    await expect(
      page.getByRole('list', { name: 'Interactions' }).locator(':scope > [role="listitem"]')
    ).toHaveCount(20)
  })

  test('the main navigation leads to the view', async ({ page }) => {
    await page.goto('/dashboard')
    // spec: IXN-041.main-navigation-leads
    await page.getByRole('navigation').getByRole('link', { name: 'Interactions' }).first().click()
    await expect(page).toHaveURL(/\/interactions$/)
    await expect(page.getByRole('heading', { name: 'Interactions', level: 2 })).toBeVisible()
    await expect(page.getByText('Every interaction across your contacts')).toBeVisible()
  })

  test('filters by contact and type, combined, from the address', async ({ page, request }) => {
    const seeded = await testApi.seedBehavior('IXN-042')
    const subject = entity(seeded, 'subject')
    const rows = await contactRows(request, subject.id)
    expect(rows).toHaveLength(3)
    await page.goto('/interactions')
    const list = page.getByRole('list', { name: 'Interactions' })
    const filters = page.getByRole('region', { name: 'All interactions' })

    // spec: IXN-042.no-other-filter
    await expect(filters.getByRole('group', { name: 'Type' }).getByRole('button')).toHaveText([
      'All',
      'Messages',
      'Email',
      'Calendar',
      'Calls',
      'Other',
    ])
    await expect(filters.getByRole('combobox')).toHaveCount(2)
    await expect(filters.getByLabel('Contact', { exact: true })).toBeVisible()
    await expect(filters.getByLabel('Run', { exact: true })).toBeVisible()
    await expect(filters.getByRole('textbox')).toHaveCount(0)

    // spec: IXN-042.contact-filter
    await filters.getByLabel('Contact', { exact: true }).selectOption(subject.id)
    await expect(page).toHaveURL(new RegExp(`contact=${subject.id}`))
    await expect(list.locator(':scope > [role="listitem"]')).toHaveCount(3, {
      timeout: SLOW_TIMEOUT_MS,
    })
    expect(await shownIds(list)).toEqual(rows.map(row => row.item.identity.id))

    // spec: IXN-042.type-filter, IXN-042.filters-combine
    await filters
      .getByRole('group', { name: 'Type' })
      .getByRole('button', { name: 'Email' })
      .click()
    await expect(page).toHaveURL(/type=email/)
    const emailRow = rows.find(row => row.item.members[0]?.source === 'email')!
    await expect(list.locator(':scope > [role="listitem"]')).toHaveCount(1, {
      timeout: SLOW_TIMEOUT_MS,
    })
    expect(await shownIds(list)).toEqual([emailRow.item.identity.id])

    // spec: IXN-042.filters-in-address
    const address = page.url()
    await page.goto('/dashboard')
    await page.goto(address)
    await expect(list.locator(':scope > [role="listitem"]')).toHaveCount(1, {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(filters.getByLabel('Contact', { exact: true })).toHaveValue(subject.id)
    await expect(filters.getByRole('button', { name: 'Email' })).toHaveAttribute(
      'aria-pressed',
      'true'
    )
    await filters.getByRole('button', { name: 'Clear filters' }).click()
    await expect(page).toHaveURL(/\/interactions$/)
  })

  test('a run filter keeps the rows of threads judged in the run, a reused judgment included', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-042')
    const subject = entity(seeded, 'subject')
    const rows = await contactRows(request, subject.id)
    const emailRow = rows.find(row => row.item.members[0]?.source === 'email')!
    const chatRow = rows.find(row => row.item.members[0]?.source === 'gchat')!
    const cutOff = minuteAgo()
    const hash = `bench-${seeded.namespace}-email`
    const judged = await seedJudgmentRun(request, {
      judgments: [
        {
          thread_interaction_id: entity(seeded, 'subject-email').id,
          cut_off: cutOff,
          input_hash: hash,
        },
      ],
    })
    const other = await seedJudgmentRun(request, {
      judgments: [{ thread_interaction_id: entity(seeded, 'subject-chat').id, cut_off: cutOff }],
    })
    const reusing = await seedJudgmentRun(request, {
      judgments: [
        {
          thread_interaction_id: entity(seeded, 'subject-email').id,
          cut_off: new Date(Date.now() - 30_000).toISOString(),
          input_hash: hash,
        },
      ],
    })
    const list = page.getByRole('list', { name: 'Interactions' })

    // spec: IXN-042.run-filter-includes-reused
    const shows = (ids: string[]) =>
      expect.poll(() => shownIds(list), { timeout: SLOW_TIMEOUT_MS }).toEqual(ids)
    await page.goto(`/interactions?run=${judged.run_id}`)
    await shows([emailRow.item.identity.id])
    await page.getByLabel('Run', { exact: true }).selectOption(other.run_id)
    await expect(page).toHaveURL(new RegExp(`run=${other.run_id}`))
    await shows([chatRow.item.identity.id])
    await page.getByLabel('Run', { exact: true }).selectOption(reusing.run_id)
    await expect(page).toHaveURL(new RegExp(`run=${reusing.run_id}`))
    await shows([emailRow.item.identity.id])
    const reused = await fetchAllInteractions(request, { run_id: reusing.run_id })
    expect(reused.total).toBe(1)
  })

  test('the contact history links out to the view filtered to the contact', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-050')
    const subject = entity(seeded, 'subject')
    const other = entity(seeded, 'other')
    const rows = await contactRows(request, subject.id)
    await seedJudgmentRun(request, {
      judgments: [
        {
          thread_interaction_id: entity(seeded, 'subject-email').id,
          cut_off: minuteAgo(),
          ball_in_court: [
            {
              question_id: 'ball_in_court:THEM',
              contact_id: subject.id,
              outcome: 'i_owe_reply',
              confidence: 0.9,
            },
          ],
        },
      ],
    })
    await page.goto(`/contacts/${subject.id}`)
    const history = page.getByRole('region', { name: 'Interactions' })

    // spec: IXN-050.history-no-review
    await expect(history.locator('[data-interaction-id]').first()).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    const emailRow = rows.find(row => row.item.members[0]?.source === 'email')!
    await history
      .locator(`[data-interaction-id="${emailRow.item.identity.id}"]`)
      .getByRole('button', { name: 'Expand content' })
      .click()
    await expect(history.locator('[data-content-region]')).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    await expect(history.locator('[data-proposal]')).toHaveCount(0)
    await expect(history.getByRole('button', { name: /^Confirm / })).toHaveCount(0)

    // spec: IXN-050.history-links-out, IXN-050.link-opens-filtered
    await history.getByRole('link', { name: 'Review in all interactions' }).click()
    await expect(page).toHaveURL(new RegExp(`/interactions\\?contact=${subject.id}$`))
    const list = page.getByRole('list', { name: 'Interactions' })
    await expect(list.locator(':scope > [role="listitem"]')).toHaveCount(rows.length, {
      timeout: SLOW_TIMEOUT_MS,
    })
    expect(await shownIds(list)).toEqual(rows.map(row => row.item.identity.id))
    const otherRows = await contactRows(request, other.id)
    const otherOnly = otherRows.filter(
      row => !row.participants.some(p => p.contact_id === subject.id)
    )
    expect(otherOnly.length).toBeGreaterThan(0)
    for (const row of otherOnly) {
      await expect(benchRow(page, row.item.identity.id)).toHaveCount(0)
    }
    await expect(page.getByLabel('Contact', { exact: true })).toHaveValue(subject.id)
  })
})
