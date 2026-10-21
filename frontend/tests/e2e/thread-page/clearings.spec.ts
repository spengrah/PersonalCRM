import { test, expect, type Page } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from '../helpers/timeouts'
import { createTestAPI, TestAPI } from '../helpers/test-api'
import {
  flag,
  nightsOf,
  reviewOf,
  reviewWorld,
  seedRun,
  startWithReview,
  threadUrl,
} from './helpers'

const stamp = new Intl.DateTimeFormat('en-US', {
  weekday: 'short',
  month: 'short',
  day: 'numeric',
  hour: 'numeric',
  minute: '2-digit',
})

test.describe('Clearing decisions in a judgment box @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  /**
   * Four commitments flagged on the first night, and a second night that
   * re-checks them: the first and third judged done, the second and fourth
   * judged not done.
   */
  async function openClearings(page: Page, request: Parameters<typeof reviewWorld>[1]) {
    const world = await reviewWorld(testApi, request)
    const { interactionId, thread, messages } = world.board
    const committed = [0, 1, 2, 3]
    const [first, second] = nightsOf(messages, [
      {
        from: 0,
        to: 4,
        flags: committed.map(i => flag(messages[i].message_key, 'commitment', 0.9)),
      },
      { from: 5, to: 9, window: 0 },
    ])
    second.clearings = committed.map(i => ({
      commitment_message_key: messages[i].message_key,
      commitment_cut_off: first.cut_off,
      done: i % 2 === 0,
      probability: 0.7 + i / 20,
    }))
    const runId = await seedRun(request, interactionId, { nights: [first, second] })
    await startWithReview(page, true)
    await page.goto(threadUrl(thread.thread_id, { contact: world.ana.id, run: runId }))
    const section = page.locator('[data-judgment-box]').nth(1)
    await expect(section.locator('[data-clearing]')).toHaveCount(4, { timeout: SLOW_TIMEOUT_MS })
    return { messages, runId, threadId: thread.thread_id, box: section }
  }

  test('lists each clearing decision with its commitment’s time, answer and probability, and no commitment text', async ({
    page,
    request,
  }) => {
    const { messages, box } = await openClearings(page, request)

    // spec: IXN-082.lists-each-decision, IXN-082.show-in-thread-and-probability
    await expect(box.getByRole('heading', { name: 'Commitments re-checked' })).toBeVisible()
    const items = box.locator('[data-clearing]')
    for (const i of [0, 1, 2, 3]) {
      const item = items.nth(i)
      await expect(item.locator('[data-clearing-text]')).toHaveText(
        `Commitment from ${stamp.format(new Date(messages[i].sent_at))} · judged ${i % 2 === 0 ? 'done' : 'not done'}`
      )
      await expect(item.locator('[data-clearing-probability]')).toHaveText(`${70 + i * 5}%`)
      await expect(item).not.toContainText(messages[i].text)
    }

    // spec: IXN-082.show-in-thread-and-probability
    await items.nth(2).getByRole('link', { name: 'Show in thread ↑' }).click()
    await expect(page.locator(`[data-message-key="${messages[2].message_key}"]`)).toHaveAttribute(
      'data-highlighted',
      'true'
    )
  })

  test('Done and Not done label the clearing decision with their own answer, whatever the method answered', async ({
    page,
    request,
  }) => {
    const { messages, runId, threadId, box } = await openClearings(page, request)
    const sent: { key: string; done: boolean }[] = []
    page.on('request', req => {
      if (req.method() === 'POST' && req.url().includes('/clearing-labels')) {
        const body = req.postDataJSON()
        sent.push({ key: body.commitment_message_key, done: body.done })
      }
    })
    const items = box.locator('[data-clearing]')
    const control = (i: number, name: 'Done' | 'Not done') =>
      items.nth(i).getByRole('button', { name, exact: true })
    const mark = (i: number) => items.nth(i).locator('[data-clearing-mark]')

    // spec: IXN-082.done-not-done
    await expect(items.nth(3).getByRole('button')).toHaveText(['Done', 'Not done'])
    await control(0, 'Not done').click() // judged done
    await expect(mark(0)).toHaveText('Marked not done')
    await control(1, 'Done').click() // judged not done
    await expect(mark(1)).toHaveText('Marked done')
    await control(2, 'Done').click() // judged done
    await expect(mark(2)).toHaveText('Marked done')
    await expect(items.nth(2).getByRole('button')).toHaveText(['Change'])

    // spec: IXN-082.done-not-done
    await items.nth(2).getByRole('button', { name: 'Change the clearing review' }).click()
    await expect(items.nth(2).getByRole('button')).toHaveText(['Done', 'Not done', 'Cancel'])
    await control(2, 'Not done').click()
    await expect(mark(2)).toHaveText('Marked not done')

    expect(sent).toEqual([
      { key: messages[0].message_key, done: false },
      { key: messages[1].message_key, done: true },
      { key: messages[2].message_key, done: true },
      { key: messages[2].message_key, done: false },
    ])
    const night = (await reviewOf(request, threadId, runId)).nights[1]
    expect(night.clearings.map(c => c.label)).toEqual(['removed', 'removed', 'removed', 'none'])

    // spec: IXN-082.done-not-done
    await page.reload()
    await expect(mark(0)).toHaveText('Marked not done', { timeout: SLOW_TIMEOUT_MS })
    await expect(mark(1)).toHaveText('Marked done')
    await expect(mark(2)).toHaveText('Marked not done')
    await expect(mark(3)).toHaveCount(0)
  })
})
