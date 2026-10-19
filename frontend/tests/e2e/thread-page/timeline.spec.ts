import { test, expect, type Page } from '@playwright/test'
import { SHORT_TIMEOUT_MS, SLOW_TIMEOUT_MS } from '../helpers/timeouts'
import { createTestAPI, TestAPI } from '../helpers/test-api'
import { COMPARED_METHODS } from '../helpers/bench'
import {
  afterwards,
  flag,
  nightsOf,
  reviewOf,
  reviewWorld,
  seedRun,
  shownKeys,
  startWithReview,
  threadUrl,
} from './helpers'

const day = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric' })
const dayOfMonth = new Intl.DateTimeFormat('en-US', { day: 'numeric' })
const stamp = new Intl.DateTimeFormat('en-US', {
  weekday: 'short',
  month: 'short',
  day: 'numeric',
  hour: 'numeric',
  minute: '2-digit',
})

/** The days of two instants as a night's divider writes them: "Jan 3", or "Jan 2–3". */
function days(first: string, last: string): string {
  const [a, b] = [new Date(first), new Date(last)]
  if (day.format(a) === day.format(b)) return day.format(a)
  return a.getMonth() === b.getMonth()
    ? `${day.format(a)}–${dayOfMonth.format(b)}`
    : `${day.format(a)}–${day.format(b)}`
}

/** The page's dividers, judgment boxes and messages as one sequence in document order. */
function outline(page: Page): Promise<string[]> {
  return page
    .locator('[data-review-timeline]')
    .locator('[data-night-divider], [data-message-key], [data-judgment-box]')
    .evaluateAll(nodes =>
      nodes.map(node =>
        node.hasAttribute('data-night-divider')
          ? 'divider'
          : node.hasAttribute('data-judgment-box')
            ? 'box'
            : (node.getAttribute('data-message-key') ?? '')
      )
    )
}

test.describe('Thread page timeline with Review on @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  /** The board chat judged by one run, opened with Review on and the run in the address. */
  async function openBoard(
    page: Page,
    request: Parameters<typeof reviewWorld>[1],
    judge: (messages: { message_key: string; sent_at: string }[]) => ReturnType<typeof nightsOf>,
    unjudged: (
      messages: { message_key: string; sent_at: string }[]
    ) => Parameters<typeof seedRun>[2]['unjudged'] = () => []
  ) {
    const world = await reviewWorld(testApi, request)
    const { interactionId, thread, messages } = world.board
    const runId = await seedRun(request, interactionId, {
      nights: judge(messages),
      unjudged: unjudged(messages),
    })
    await startWithReview(page, true)
    await page.goto(threadUrl(thread.thread_id, { contact: world.ana.id, run: runId }))
    await expect(page.locator('[data-judgment-box]').first()).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    return { world, runId, messages, threadId: thread.thread_id }
  }

  test('a busy group chat judged on many nights shows each message once, each night closed by its judgment box', async ({
    page,
    request,
  }) => {
    const { runId, messages, threadId } = await openBoard(page, request, all =>
      // Ten nights of three new messages, each window reading five earlier messages too.
      nightsOf(
        all,
        Array.from({ length: 10 }, (_, n) => ({
          from: n * 3,
          to: n * 3 + 2,
          window: Math.max(0, n * 3 - 5),
          flags: n % 2 === 0 ? [flag(all[n * 3].message_key, 'commitment', 0.9)] : [],
        }))
      )
    )
    const review = await reviewOf(request, threadId, runId)

    // spec: IXN-080.one-timeline-each-message-once, IXN-080.night-divider, IXN-080.judgment-box-closes-night
    const keys = messages.map(m => m.message_key)
    expect(await shownKeys(page)).toEqual(keys)
    expect(await outline(page)).toEqual(
      Array.from({ length: 10 }, (_, n) => [
        'divider',
        ...keys.slice(n * 3, n * 3 + 3),
        'box',
      ]).flat()
    )
    const dividers = page.locator('[data-night-divider]')
    await expect(dividers).toHaveCount(10)
    for (const [n, night] of review.nights.entries()) {
      const slice = messages.slice(n * 3, n * 3 + 3)
      await expect(dividers.nth(n)).toHaveText(
        `${days(slice[0].sent_at, slice[2].sent_at)} · ${night.new_message_count} new messages`
      )
    }

    // spec: IXN-080.night-messages-with-chips
    await expect(page.getByRole('button', { name: '+ Flag' })).toHaveCount(30)
    await expect(page.locator('[data-chip="commitment"]')).toHaveCount(5)
    await expect(
      page.locator(`[data-message-key="${keys[0]}"]`).locator('[data-chip="commitment"]')
    ).toContainText('90%')
  })

  test('a night’s message the method could not judge shows once among the night’s messages, with no chip', async ({
    page,
    request,
  }) => {
    const { runId, messages, threadId } = await openBoard(
      page,
      request,
      all =>
        nightsOf(all, [
          { from: 0, to: 4 },
          // The sixth message is over the method's limit: it is new at this cut-off but unanswered.
          { from: 5, to: 9, flags: [flag(all[6].message_key, 'claim', 0.8)] },
        ]).map((night, n) =>
          n === 1
            ? { ...night, new_messages: night.new_messages.filter(k => k !== all[5].message_key) }
            : night
        ),
      all => [
        {
          cut_off: afterwards(all[9].sent_at),
          message_key: all[5].message_key,
          reason: 'message_over_limit',
        },
      ]
    )
    const review = await reviewOf(request, threadId, runId)
    const placed = review.nights[1].messages.find(m => m.message_key === messages[5].message_key)
    expect(placed, 'the server places the message in the night').toMatchObject({
      judged: false,
      reason: 'message_over_limit',
      chips: [],
    })

    // spec: IXN-080.one-timeline-each-message-once, IXN-080.night-messages-with-chips
    const over = page.locator(`[data-message-key="${messages[5].message_key}"]`)
    await expect(over).toHaveCount(1)
    await expect(over.locator('[data-chip]')).toHaveCount(0)
    const keys = await shownKeys(page)
    expect(keys.filter(key => key === messages[5].message_key)).toHaveLength(1)
    await expect(page.locator('[data-night]').nth(1).locator('[data-message-key]')).toHaveCount(
      review.nights[1].messages.length
    )
    // The night's divider counts only the messages the judgment answered.
    await expect(page.locator('[data-night-divider]').nth(1)).toContainText(
      `· ${review.nights[1].new_message_count} new messages`
    )
  })

  test('a first night whose window spans two days is dated by both', async ({ page, request }) => {
    // Messages 8 and 9 are the first day's last; 10 and 11 the second day's first.
    const { messages } = await openBoard(page, request, all => nightsOf(all, [{ from: 8, to: 11 }]))

    // spec: IXN-080.night-divider
    const label = days(messages[8].sent_at, messages[11].sent_at)
    expect(label).toContain('–')
    await expect(page.locator('[data-night-divider]')).toHaveText(`${label} · 4 new messages`)
  })

  test('a night after a failed cut-off shows the failed cut-off’s messages unjudged, uncounted by its divider', async ({
    page,
    request,
  }) => {
    const { runId, messages, threadId } = await openBoard(
      page,
      request,
      all =>
        nightsOf(all, [
          { from: 0, to: 2 },
          { from: 6, to: 8, window: 3 },
        ]),
      all => [
        {
          cut_off: afterwards(all[5].sent_at),
          message_key: all[3].message_key,
          reason: 'call_failed',
        },
        {
          cut_off: afterwards(all[5].sent_at),
          message_key: all[4].message_key,
          reason: 'call_failed',
        },
        {
          cut_off: afterwards(all[5].sent_at),
          message_key: all[5].message_key,
          reason: 'call_failed',
        },
      ]
    )
    const review = await reviewOf(request, threadId, runId)
    const second = review.nights[1]
    expect(second.messages.filter(m => !m.judged).map(m => m.message_key)).toEqual(
      [3, 4, 5].map(i => messages[i].message_key)
    )

    // spec: IXN-080.night-divider, IXN-080.night-messages-with-chips
    await expect(page.locator('[data-night-divider]').nth(1)).toHaveText(
      `${days(messages[3].sent_at, messages[8].sent_at)} · 3 new messages`
    )
    const night = page.locator('[data-night]').nth(1)
    await expect(night.locator('[data-message-key]')).toHaveCount(6)
    for (const i of [3, 4, 5]) {
      const unjudged = night.locator(`[data-message-key="${messages[i].message_key}"]`)
      await expect(unjudged.locator('[data-judged="false"]')).toHaveCount(1)
      await expect(unjudged.locator('[data-chip]')).toHaveCount(0)
    }
  })

  test('messages the first night’s window read before its new messages show before the first divider, with no chip', async ({
    page,
    request,
  }) => {
    const { runId, messages, threadId } = await openBoard(page, request, all =>
      nightsOf(all, [
        { from: 5, to: 7, window: 2, flags: [flag(all[6].message_key, 'claim', 0.9)] },
      ])
    )
    const review = await reviewOf(request, threadId, runId)
    expect(review.context_messages).toEqual([2, 3, 4].map(i => messages[i].message_key))

    // spec: IXN-080.one-timeline-each-message-once
    const seq = await outline(page)
    const firstDivider = seq.indexOf('divider')
    const before = seq.slice(0, firstDivider)
    for (const key of review.context_messages) {
      expect(before).toContain(key)
      const message = page.locator(`[data-message-key="${key}"]`)
      await expect(message.locator('[data-message-flags]')).toHaveCount(0)
      await expect(message.getByRole('button', { name: '+ Flag' })).toHaveCount(0)
    }
    // No toggle holds earlier history above the first night.
    const toggleAbove = await page.evaluate(() => {
      const divider = document.querySelector('[data-night-divider]')!
      return [...document.querySelectorAll('[data-collapsed]')].some(
        toggle => toggle.compareDocumentPosition(divider) & Node.DOCUMENT_POSITION_FOLLOWING
      )
    })
    expect(toggleAbove).toBe(false)
  })

  test('a later night’s heading link scrolls to an earlier night’s message and highlights it', async ({
    page,
    request,
  }) => {
    const { messages } = await openBoard(page, request, all =>
      // The second night's window starts at the fourth message, which the first night answered.
      nightsOf(all, [
        { from: 0, to: 4 },
        { from: 5, to: 9, window: 3 },
      ])
    )
    const second = page.locator('[data-judgment-box]').nth(1)
    const target = page.locator(`[data-message-key="${messages[3].message_key}"]`)
    await page.evaluate(() => window.scrollTo(0, document.body.scrollHeight))

    // spec: IXN-080.box-heading-links-first-message
    await expect(second.locator('[data-judgment-heading]')).toHaveText(
      `Judgment · read ${stamp.format(new Date(messages[3].sent_at))} to ${stamp.format(new Date(afterwards(messages[9].sent_at)))}`
    )
    await second
      .locator('[data-judgment-heading]')
      .getByRole('link', { name: stamp.format(new Date(messages[3].sent_at)) })
      .click()
    await expect(target).toHaveAttribute('data-highlighted', 'true')
    await expect(target).toBeInViewport()
    // The highlight fades.
    await expect(target).not.toHaveAttribute('data-highlighted', 'true', {
      timeout: SHORT_TIMEOUT_MS,
    })
  })

  test('messages after the run’s last night sit behind a collapsed toggle', async ({
    page,
    request,
  }) => {
    const { messages } = await openBoard(page, request, all =>
      nightsOf(all, [
        { from: 0, to: 9 },
        { from: 10, to: 19 },
      ])
    )

    // spec: IXN-080.later-messages-collapsed
    const toggle = page.getByRole('button', {
      name: "10 later messages, after this run's last night",
    })
    await expect(toggle).toHaveAttribute('aria-expanded', 'false')
    expect(await shownKeys(page)).toEqual(messages.slice(0, 20).map(m => m.message_key))
    await toggle.click()
    expect(await shownKeys(page)).toEqual(messages.map(m => m.message_key))
    const later = page.locator(`[data-message-key="${messages[25].message_key}"]`)
    await expect(later.locator('[data-message-flags]')).toHaveCount(0)
  })

  test('names, per flag kind, the unflagged message a judgment came closest to flagging, or none above 5%', async ({
    page,
    request,
  }) => {
    const { messages } = await openBoard(page, request, all =>
      nightsOf(all, [
        {
          from: 0,
          to: 4,
          flags: [
            // Below the display threshold: no chip, and the closest unflagged commitment.
            flag(all[2].message_key, 'commitment', 0.2),
            flag(all[3].message_key, 'commitment', 0.12),
            // Shown as a chip, so never the closest unflagged claim.
            flag(all[1].message_key, 'claim', 0.9),
          ],
        },
      ])
    )
    const box = page.locator('[data-judgment-box]')
    const closest = box.locator('[data-closest-unflagged]')

    // spec: IXN-080.box-sections
    await expect(page.locator('[data-chip]')).toHaveCount(1)
    await expect(closest).toContainText('Closest unflagged:')
    const commitment = closest.locator('[data-closest-kind="commitment"]')
    await expect(commitment).toHaveText(
      `Commitment ${messages[2].sender}, ${stamp.format(new Date(messages[2].sent_at))} ↑ 20%`
    )
    await commitment.getByRole('link').click()
    await expect(page.locator(`[data-message-key="${messages[2].message_key}"]`)).toHaveAttribute(
      'data-highlighted',
      'true'
    )
    await expect(closest.locator('[data-closest-kind="big_moment"]')).toHaveText(
      'Big moment none above 5%'
    )
    await expect(closest.locator('[data-closest-kind="claim"]')).toHaveText('Claim none above 5%')
  })

  test('the header adds the status pill, the run and Done', async ({ page, request }) => {
    const { runId, threadId } = await openBoard(page, request, all =>
      nightsOf(all, [{ from: 0, to: 4, flags: [flag(all[1].message_key, 'claim', 0.9)] }])
    )
    const review = await reviewOf(request, threadId, runId)
    const header = page.locator('[data-thread-header]')

    // spec: IXN-080.header-adds-status-run-done
    await expect(header.locator('[data-thread-status="to_review"]')).toHaveText(
      `${review.to_review} to review`
    )
    const month = new Intl.DateTimeFormat('en-US', {
      month: 'long',
      year: 'numeric',
      timeZone: 'UTC',
    }).format(new Date(review.run.from!))
    await expect(header.locator('[data-run]')).toHaveText(
      `· ${COMPARED_METHODS.clef} · ${month} run`
    )
    await expect(header.getByRole('button', { name: 'Done' })).toBeVisible()
  })
})
