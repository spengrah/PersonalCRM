import { test, expect, type APIRequestContext, type Locator, type Page } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from '../helpers/timeouts'
import { API_BASE_URL, API_HEADERS, createTestAPI, TestAPI } from '../helpers/test-api'
import {
  COMPARED_METHODS,
  contactRows,
  entity,
  fetchThreads,
  labelFlag,
  readThread,
  runNamed,
  seedJudgmentRun,
  threadOf,
} from '../helpers/bench'
import type { ReviewRunsResponse } from '../../../src/types/generated/contact'

/** The Threads list's rows, in the order shown. */
function rows(page: Page): Locator {
  return page.getByRole('list', { name: 'Threads' }).locator(':scope > [role="listitem"]')
}

function shownThreads(page: Page): Promise<(string | null)[]> {
  return rows(page).evaluateAll(nodes => nodes.map(node => node.getAttribute('data-thread-id')))
}

/** The runs the route returns, in its order. */
async function reviewRuns(request: APIRequestContext) {
  const response = await request.get(`${API_BASE_URL}/api/v1/review-runs`, { headers: API_HEADERS })
  expect(response.ok()).toBe(true)
  return ((await response.json()).data as ReviewRunsResponse).runs
}

const reviewSwitch = (page: Page) => page.getByRole('switch', { name: 'Review' })

test.describe('Threads list with Review @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    // The world seeds a chat for each of two pages of threads.
    test.slow()
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  /**
   * The IXN-076 world judged by two runs. The first run (January) judges the
   * family chat on twelve nights, flagging one commitment; the email thread on
   * one night with nothing flagged; and the meeting on one night, flagging a
   * claim the user then labels. The second run (February, a later start)
   * judges the email thread only.
   */
  async function judgedWorld(request: APIRequestContext) {
    const seeded = await testApi.seedBehavior('IXN-076')
    const [ana, ben, host] = ['ana', 'ben', 'host'].map(handle => entity(seeded, handle))
    const family = await threadOf(request, ben.id, undefined)
    const mail = (await fetchThreads(request, { contact_id: ana.id })).data.items.find(
      thread => thread.venue === 'email'
    )!
    const meeting = await threadOf(request, host.id, undefined)
    // The meeting's interaction is the host's one row.
    const meetingInteraction = (await contactRows(request, host.id))[0].item.identity.id
    const familyMessages = (await readThread(request, family.thread_id)).messages
    const mailMessages = (await readThread(request, mail.thread_id)).messages
    const meetingMessages = (await readThread(request, meeting.thread_id)).messages
    const afterwards = (sentAt: string) =>
      new Date(new Date(sentAt).getTime() + 30_000).toISOString()
    const january = await seedJudgmentRun(request, {
      method_key: COMPARED_METHODS.clef,
      from: '2026-01-01T00:00:00Z',
      to: '2026-01-31T00:00:00Z',
      judgments: [
        ...familyMessages.map((message, i) => ({
          thread_interaction_id: entity(seeded, 'family').id,
          cut_off: afterwards(message.sent_at),
          window_start: familyMessages[0].message_key,
          new_messages: [message.message_key],
          flags:
            i === 0
              ? [
                  {
                    message_key: message.message_key,
                    kind: 'commitment' as const,
                    probability: 0.9,
                  },
                ]
              : [],
        })),
        {
          thread_interaction_id: entity(seeded, 'mail').id,
          cut_off: afterwards(mailMessages[0].sent_at),
          window_start: mailMessages[0].message_key,
          new_messages: [mailMessages[0].message_key],
        },
        {
          thread_interaction_id: meetingInteraction,
          context_kind: 'meeting' as const,
          cut_off: afterwards(meetingMessages[0].sent_at),
          window_start: meetingMessages[0].message_key,
          new_messages: [meetingMessages[0].message_key],
          flags: [
            {
              message_key: meetingMessages[0].message_key,
              kind: 'claim' as const,
              probability: 0.9,
            },
          ],
        },
      ],
    })
    await labelFlag(request, meeting.thread_id, january.run_id, {
      message_key: meetingMessages[0].message_key,
      kind: 'claim',
      holds: true,
    })
    const february = await seedJudgmentRun(request, {
      method_key: COMPARED_METHODS.jev,
      from: '2026-02-01T00:00:00Z',
      to: '2026-02-28T00:00:00Z',
      judgments: [
        {
          thread_interaction_id: entity(seeded, 'mail').id,
          cut_off: afterwards(mailMessages[0].sent_at),
          window_start: mailMessages[0].message_key,
          new_messages: [mailMessages[0].message_key],
        },
      ],
    })
    return { seeded, ana, family, mail, meeting, january, february }
  }

  test('Review on narrows to a run, naming the threads the run judged by count and status', async ({
    page,
    request,
  }) => {
    const { family, mail, meeting, january, february } = await judgedWorld(request)
    const runs = await reviewRuns(request)
    const runSelect = page.getByLabel('Run', { exact: true })

    await page.goto('/interactions?tab=threads')
    await expect(rows(page).first()).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    await expect(runSelect).toHaveCount(0)

    // spec: IXN-077.run-filter-lists-runs, IXN-077.newest-run-default
    await reviewSwitch(page).click()
    await expect(runSelect).toBeVisible()
    await expect(runSelect.locator('option')).toHaveCount(runs.length)
    expect(
      await runSelect
        .locator('option')
        .evaluateAll(nodes => nodes.map(n => n.getAttribute('value')))
    ).toEqual(runs.map(run => run.id))
    await expect(runSelect.locator(`option[value="${february.run_id}"]`)).toHaveText(
      runNamed('jev-masked · February 2026')
    )
    await expect(runSelect.locator(`option[value="${january.run_id}"]`)).toHaveText(
      runNamed('clef-masked · January 2026')
    )
    await expect(runSelect).toHaveValue(runs[0].id)
    await expect(page).toHaveURL(/\/interactions\?tab=threads$/)
    // The list is narrowed to that run, not to the runs before it.
    const newest = (await fetchThreads(request, { run_id: runs[0].id })).data.items
    await expect
      .poll(() => shownThreads(page), { timeout: SLOW_TIMEOUT_MS })
      .toEqual(newest.map(thread => thread.thread_id))

    // spec: IXN-077.run-keeps-its-threads, IXN-077.no-method-filter
    await runSelect.selectOption(january.run_id)
    await expect(page).toHaveURL(new RegExp(`run=${january.run_id}`))
    await expect(rows(page)).toHaveCount(3, { timeout: SLOW_TIMEOUT_MS })
    const inRun = (await fetchThreads(request, { run_id: january.run_id })).data.items
    expect(await shownThreads(page)).toEqual(inRun.map(thread => thread.thread_id))
    expect(new Set(await shownThreads(page))).toEqual(
      new Set([family.thread_id, mail.thread_id, meeting.thread_id])
    )
    await expect(page.getByLabel('Method')).toHaveCount(0)
    await runSelect.selectOption(february.run_id)
    await expect(rows(page)).toHaveCount(1, { timeout: SLOW_TIMEOUT_MS })
    await expect(rows(page).first()).toHaveAttribute('data-thread-id', mail.thread_id)

    // spec: IXN-077.summary-line
    await runSelect.selectOption(january.run_id)
    await expect(rows(page)).toHaveCount(3, { timeout: SLOW_TIMEOUT_MS })
    const summary = (await fetchThreads(request, { run_id: january.run_id })).data.summary!
    expect(summary).toEqual({ judged: 3, to_review: 1, nothing_flagged: 1, reviewed: 1 })
    await expect(page.locator('[data-run-summary]')).toContainText(
      '3 threads judged in this run · 1 to review · 1 with nothing flagged · 1 reviewed'
    )

    // spec: IXN-077.row-count-and-status
    const rowOf = (threadId: string) =>
      page.locator(`[role="listitem"][data-thread-id="${threadId}"]`)
    const familyRow = rowOf(family.thread_id)
    await expect(familyRow.locator('[data-count]')).toHaveText('12 judged in this run')
    const familyStatus = (await fetchThreads(request, { run_id: january.run_id })).data.items.find(
      thread => thread.thread_id === family.thread_id
    )!.review!
    await expect(familyRow.locator('[data-thread-status]')).toHaveText(
      `${familyStatus.to_review} to review`
    )
    await expect(familyRow.locator('[data-thread-status]')).toHaveAttribute(
      'data-thread-status',
      'to_review'
    )
    // The chip, and the eleven nights that show nothing and await Confirm Nones.
    expect(familyStatus.to_review).toBe(12)
    const mailRow = rowOf(mail.thread_id)
    await expect(mailRow.locator('[data-count]')).toHaveText('1 judged in this run')
    await expect(mailRow.locator('[data-thread-status]')).toHaveText('Nothing flagged')
    const meetingRow = rowOf(meeting.thread_id)
    await expect(meetingRow.locator('[data-count]')).toHaveText('1 judged in this run')
    await expect(meetingRow.locator('[data-thread-status]')).toHaveText('Reviewed')

    // spec: IXN-076.row-opens-thread-page
    await expect(familyRow.getByRole('link', { name: 'Open thread' })).toHaveAttribute(
      'href',
      `/interactions/threads/${family.thread_id}?run=${january.run_id}`
    )
  })

  test('the Run filter lists no rerun or removed method’s run, and a judged count leaves out a message no judgment answered', async ({
    page,
    request,
  }) => {
    const { seeded, family, mail, january, february } = await judgedWorld(request)
    const familyMessages = (await readThread(request, family.thread_id)).messages
    const mailMessages = (await readThread(request, mail.thread_id)).messages
    const afterwards = (sentAt: string) =>
      new Date(new Date(sentAt).getTime() + 30_000).toISOString()

    // A March run judges the twelve-message chat in one night whose sixth
    // message is over the method's limit: new at the cut-off, answered by no
    // judgment, while its neighbors are judged.
    const nightEnd = afterwards(familyMessages[familyMessages.length - 1].sent_at)
    const oversized = familyMessages[5].message_key
    const march = await seedJudgmentRun(request, {
      method_key: COMPARED_METHODS.reasoned,
      from: '2026-03-01T00:00:00Z',
      to: '2026-03-31T00:00:00Z',
      judgments: [
        {
          thread_interaction_id: entity(seeded, 'family').id,
          cut_off: nightEnd,
          window_start: familyMessages[0].message_key,
          new_messages: familyMessages
            .map(message => message.message_key)
            .filter(key => key !== oversized),
        },
      ],
      unjudged: [
        {
          thread_interaction_id: entity(seeded, 'family').id,
          cut_off: nightEnd,
          message_key: oversized,
          reason: 'message_over_limit',
        },
      ],
    })

    // Runs the filter must never list, each judging the email thread and each
    // started after every compared run, so the newest run of all is one of them.
    const mailNight = {
      thread_interaction_id: entity(seeded, 'mail').id,
      cut_off: afterwards(mailMessages[0].sent_at),
      window_start: mailMessages[0].message_key,
      new_messages: [mailMessages[0].message_key],
    }
    const excluded: string[] = []
    for (const run of [
      { kind: 'rerun' as const, method_key: COMPARED_METHODS.clef },
      { kind: 'historical' as const, method_key: 'sage-masked' },
      { kind: 'historical' as const, method_key: 'clef-flash-masked' },
    ]) {
      const seededRun = await seedJudgmentRun(request, {
        ...run,
        from: '2026-04-01T00:00:00Z',
        to: '2026-04-30T00:00:00Z',
        judgments: [mailNight],
      })
      excluded.push(seededRun.run_id)
    }

    const runSelect = page.getByLabel('Run', { exact: true })
    const listed = () =>
      runSelect.locator('option').evaluateAll(nodes => nodes.map(n => n.getAttribute('value')))
    await page.goto('/interactions?tab=threads')
    await expect(rows(page).first()).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    await reviewSwitch(page).click()

    // spec: IXN-077.run-filter-lists-runs
    await expect(runSelect.locator(`option[value="${march.run_id}"]`)).toHaveText(
      runNamed('clef-masked-reasoned · March 2026')
    )
    expect(await listed()).toEqual(
      expect.arrayContaining([january.run_id, february.run_id, march.run_id])
    )
    for (const run of excluded) {
      expect(await listed()).not.toContain(run)
    }

    // spec: IXN-077.newest-run-default
    await expect(runSelect).toHaveValue((await listed())[0]!)

    // spec: IXN-077.row-count-and-status
    await runSelect.selectOption(march.run_id)
    await expect(rows(page)).toHaveCount(1, { timeout: SLOW_TIMEOUT_MS })
    const familyRow = page.locator(`[role="listitem"][data-thread-id="${family.thread_id}"]`)
    await expect(familyRow.locator('[data-count]')).toHaveText('11 judged in this run')
  })

  test('Review off shows no judgment, whatever run the address names, in the order Review on shows', async ({
    page,
    request,
  }) => {
    const { ana, family, mail, january, february } = await judgedWorld(request)

    // spec: IXN-077.run-keeps-its-threads
    await page.goto(`/interactions?tab=threads&contact=${ana.id}&run=${january.run_id}`)
    await expect(rows(page)).toHaveCount(2, { timeout: SLOW_TIMEOUT_MS })
    const off = await shownThreads(page)
    expect(off).toEqual([family.thread_id, mail.thread_id])
    await reviewSwitch(page).click()
    await expect(page.getByLabel('Run', { exact: true })).toHaveValue(january.run_id)
    await expect(page.locator('[data-run-summary]')).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    expect(await shownThreads(page)).toEqual(off)

    // spec: IXN-075.stays-across-visits
    await page.reload()
    await expect(reviewSwitch(page)).toHaveAttribute('aria-checked', 'true', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(page.locator('[data-run-summary]')).toBeVisible({ timeout: SLOW_TIMEOUT_MS })

    // spec: IXN-075.off-shows-no-judgment, IXN-075.order-unchanged
    await reviewSwitch(page).click()
    await expect(reviewSwitch(page)).toHaveAttribute('aria-checked', 'false')
    await expect(page.getByLabel('Run', { exact: true })).toHaveCount(0)
    await expect(page.locator('[data-run-summary]')).toHaveCount(0)
    await expect(rows(page).first().locator('[data-thread-status]')).toHaveCount(0)
    await expect(rows(page).first().getByText('judged in this run')).toHaveCount(0)
    await expect(rows(page).first().locator('[data-count]')).toHaveText('12 messages')
    expect(await shownThreads(page)).toEqual(off)

    // spec: IXN-077.run-keeps-its-threads
    await page.goto(`/interactions?tab=threads&contact=${ana.id}&run=${february.run_id}`)
    await expect(rows(page)).toHaveCount(2, { timeout: SLOW_TIMEOUT_MS })
    expect(await shownThreads(page)).toEqual(off)
    await expect(page.locator('[data-run-summary]')).toHaveCount(0)
  })
})
