import { test, expect, type Page } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from '../helpers/timeouts'
import { createTestAPI, TestAPI } from '../helpers/test-api'
import {
  flag,
  nightsOf,
  reviewOf,
  reviewWorld,
  seedThreadsRun,
  startWithReview,
  threadUrl,
} from './helpers'

test.describe('Closing a thread @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  /**
   * One run judging the side chat on one night of its three messages, the first
   * flagged as a claim, and the board chat on one night of its first five
   * messages. Ana's threads list the side chat, then the board chat.
   */
  async function openSide(page: Page, request: Parameters<typeof reviewWorld>[1]) {
    const world = await reviewWorld(testApi, request)
    const { side, board } = world
    const runId = await seedThreadsRun(request, [
      {
        interactionId: side.interactionId,
        nights: nightsOf(side.messages, [
          { from: 0, to: 2, flags: [flag(side.messages[0].message_key, 'claim', 0.9)] },
        ]),
      },
      {
        interactionId: board.interactionId,
        nights: nightsOf(board.messages, [
          { from: 0, to: 4, flags: [flag(board.messages[0].message_key, 'claim', 0.9)] },
        ]),
      },
    ])
    const closes: string[] = []
    page.on('request', req => {
      if (req.method() === 'POST' && /\/threads\/[^/]+\/close/.test(req.url())) {
        closes.push(req.url())
      }
    })
    await startWithReview(page, true)
    const query = { contact: world.ana.id, run: runId }
    await page.goto(threadUrl(side.thread.thread_id, query))
    await expect(page.locator('[data-judgment-box]')).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    const confirm = page.getByRole('button', { name: 'Confirm Claim' })
    await confirm.click()
    await expect(page.locator('[data-chip-state="confirmed"]')).toHaveCount(1)
    return { world, side, board, runId, query, closes }
  }

  const footer = (page: Page) => page.locator('[data-judgment-footer]')
  const status = (page: Page) => page.locator('[data-thread-header] [data-thread-status]')

  test('Next by the right-arrow key closes the thread and opens the next one', async ({
    page,
    request,
  }) => {
    const { side, board, runId, query, closes } = await openSide(page, request)
    const closed = page.waitForResponse(res => res.url().includes('/close'))

    // spec: IXN-083.done-and-next-close
    await page.keyboard.press('ArrowRight')
    await expect(page).toHaveURL(new RegExp(`/interactions/threads/${board.thread.thread_id}\\?`))
    expect((await closed).status()).toBe(200)
    expect(closes).toHaveLength(1)
    expect(closes[0]).toContain(`/threads/${side.thread.thread_id}/close`)
    expect(closes[0]).toContain(`run_id=${runId}`)

    // spec: IXN-083.footer-reports-nones
    // The thread reopened reads Reviewed, with its night's footer.
    await page.goto(threadUrl(side.thread.thread_id, query))
    await expect(footer(page)).toHaveText(
      '2 unflagged new messages labeled none when you moved on',
      {
        timeout: SLOW_TIMEOUT_MS,
      }
    )
    await expect(status(page)).toHaveText('Reviewed')
    expect((await reviewOf(request, side.thread.thread_id, runId)).nights[0].labeled_none).toBe(2)
  })

  test('Next by the pager’s › closes the thread and opens the next one', async ({
    page,
    request,
  }) => {
    const { side, board, runId, closes } = await openSide(page, request)
    const closed = page.waitForResponse(res => res.url().includes('/close'))

    // spec: IXN-083.done-and-next-close
    await page.getByRole('button', { name: 'Next thread' }).click()
    await expect(page).toHaveURL(new RegExp(`/interactions/threads/${board.thread.thread_id}\\?`))
    expect((await closed).status()).toBe(200)
    expect(closes).toHaveLength(1)
    expect((await reviewOf(request, side.thread.thread_id, runId)).nights[0].labeled_none).toBe(2)
  })

  test('Done closes the thread and the page reads as the server answered', async ({
    page,
    request,
  }) => {
    const { side, runId, closes } = await openSide(page, request)
    await expect(footer(page)).toHaveCount(0)

    // spec: IXN-083.done-and-next-close, IXN-083.footer-reports-nones
    await page.getByRole('button', { name: 'Done' }).click()
    await expect(footer(page)).toHaveText(
      '2 unflagged new messages labeled none when you moved on',
      {
        timeout: SLOW_TIMEOUT_MS,
      }
    )
    await expect(page).toHaveURL(new RegExp(`/interactions/threads/${side.thread.thread_id}\\?`))
    expect(closes).toHaveLength(1)
    expect((await reviewOf(request, side.thread.thread_id, runId)).nights[0].labeled_none).toBe(2)
  })

  /** The labels a close would have written: the night's messages labeled none. */
  const labeledNone = async (
    request: Parameters<typeof reviewOf>[0],
    threadId: string,
    runId: string
  ) => (await reviewOf(request, threadId, runId)).nights[0].labeled_none

  test('Back to list after a confirm closes nothing and leaves the other messages unlabeled', async ({
    page,
    request,
  }) => {
    const { side, runId, closes } = await openSide(page, request)

    // spec: IXN-083.other-exits-do-not-close
    await page.getByRole('button', { name: 'Back to list' }).click()
    await expect(page).toHaveURL(/\/interactions\?tab=threads/)
    expect(await labeledNone(request, side.thread.thread_id, runId)).toBe(0)
    expect(closes).toHaveLength(0)
  })

  test('the previous control after a confirm closes nothing', async ({ page, request }) => {
    const { side, board, runId, query, closes } = await openSide(page, request)
    await page.goto(threadUrl(board.thread.thread_id, query))
    await page.getByRole('button', { name: 'Confirm Claim' }).click()
    await expect(page.locator('[data-chip-state="confirmed"]')).toHaveCount(1)

    // spec: IXN-083.other-exits-do-not-close
    await page.getByRole('button', { name: 'Previous thread' }).click()
    await expect(page).toHaveURL(new RegExp(`/interactions/threads/${side.thread.thread_id}\\?`))
    expect(await labeledNone(request, board.thread.thread_id, runId)).toBe(0)
    expect(closes).toHaveLength(0)
  })

  test('the left-arrow key after a confirm closes nothing', async ({ page, request }) => {
    const { side, board, runId, query, closes } = await openSide(page, request)
    await page.goto(threadUrl(board.thread.thread_id, query))
    await page.getByRole('button', { name: 'Confirm Claim' }).click()
    await expect(page.locator('[data-chip-state="confirmed"]')).toHaveCount(1)

    // spec: IXN-083.other-exits-do-not-close
    await page.keyboard.press('ArrowLeft')
    await expect(page).toHaveURL(new RegExp(`/interactions/threads/${side.thread.thread_id}\\?`))
    expect(await labeledNone(request, board.thread.thread_id, runId)).toBe(0)
    expect(closes).toHaveLength(0)
  })

  test('leaving the page by a link after a confirm closes nothing', async ({ page, request }) => {
    const { world, side, runId, closes } = await openSide(page, request)

    // spec: IXN-083.other-exits-do-not-close
    await page.locator('[data-thread-header]').getByRole('link', { name: world.ana.name }).click()
    await expect(page).toHaveURL(new RegExp(`/contacts/${world.ana.id}`))
    expect(await labeledNone(request, side.thread.thread_id, runId)).toBe(0)
    expect(closes).toHaveLength(0)
  })
})
