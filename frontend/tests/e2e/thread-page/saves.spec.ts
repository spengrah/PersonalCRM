import { test, expect, type Page } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from '../helpers/timeouts'
import { createTestAPI, TestAPI } from '../helpers/test-api'
import {
  flag,
  holdSaves,
  nightsOf,
  reviewOf,
  reviewWorld,
  seedThreadsRun,
  startWithReview,
  threadUrl,
} from './helpers'

const chip = (page: Page, key: string, kind: string) =>
  page.locator(`[data-message-key="${key}"]`).locator(`[data-chip="${kind}"]`)

const SAVE_PATHS = {
  flag: /\/threads\/[^/]+\/flag-labels/,
  close: /\/threads\/[^/]+\/close/,
}
type SaveKind = keyof typeof SAVE_PATHS

/**
 * Fails every save of `kind` while `failing` is set, so the client's retries
 * fail too, and lists the flag and close saves the server recorded, in order.
 */
async function failSaves(page: Page, kind: SaveKind) {
  const saves = { failing: true, recorded: [] as SaveKind[] }
  const kindOf = (url: URL) =>
    (Object.keys(SAVE_PATHS) as SaveKind[]).find(k => SAVE_PATHS[k].test(url.pathname))
  await page.route(
    url => kindOf(url) !== undefined,
    async route => {
      const saved = kindOf(new URL(route.request().url()))!
      if (saves.failing && saved === kind) return route.fulfill({ status: 500, body: '{}' })
      const response = await route.fetch()
      if (response.ok()) saves.recorded.push(saved)
      await route.fulfill({ response })
    }
  )
  return saves
}

test.describe('Review saves never wait on the server @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  /**
   * Ana's side chat (newest) and board chat in one run. The side chat's one
   * night holds a claim and a big moment to review; the board's holds a claim.
   * The quiet chat of cai, on a run of its own, shows nothing.
   */
  async function openSide(page: Page, request: Parameters<typeof reviewWorld>[1]) {
    const world = await reviewWorld(testApi, request)
    const { side, board } = world
    const runId = await seedThreadsRun(request, [
      {
        interactionId: side.interactionId,
        nights: nightsOf(side.messages, [
          {
            from: 0,
            to: 2,
            flags: [
              flag(side.messages[0].message_key, 'claim', 0.9),
              flag(side.messages[1].message_key, 'big_moment', 0.8),
            ],
          },
        ]),
      },
      {
        interactionId: board.interactionId,
        nights: nightsOf(board.messages, [{ from: 0, to: 4 }]),
      },
    ])
    await startWithReview(page, true)
    await page.goto(threadUrl(side.thread.thread_id, { contact: world.ana.id, run: runId }))
    await expect(page.locator('[data-chip]')).toHaveCount(2, { timeout: SLOW_TIMEOUT_MS })
    return { world, side, board, runId }
  }

  test('a second confirm works on another chip while the first save is pending, and saves are recorded in the order made', async ({
    page,
    request,
  }) => {
    const { side, runId } = await openSide(page, request)
    const [a, b] = [side.messages[0].message_key, side.messages[1].message_key]
    const saves = await holdSaves(page)

    // spec: IXN-085.next-click-works, IXN-085.saves-in-order
    await chip(page, a, 'claim').getByRole('button', { name: 'Confirm Claim' }).click()
    await expect.poll(() => saves.events).toEqual(['sent:flag'])
    const second = chip(page, b, 'big_moment').getByRole('button', { name: 'Remove Big moment' })
    await expect(second).toBeEnabled()
    await second.click()
    // The click was taken: its control waits for its own save and the first chip's control for its.
    await expect(second).toBeDisabled()
    await expect(
      chip(page, a, 'claim').getByRole('button', { name: 'Confirm Claim' })
    ).toBeDisabled()
    expect(saves.events, 'the second save waits behind the first').toEqual(['sent:flag'])
    expect(await chip(page, a, 'claim').getAttribute('data-chip-state')).toBe('to_review')

    saves.release()
    // spec: IXN-085.saved-item-shows-recorded
    await expect(chip(page, a, 'claim')).toHaveAttribute('data-chip-state', 'confirmed')
    await expect(chip(page, b, 'big_moment')).toHaveAttribute('data-chip-state', 'removed')
    expect(saves.events).toEqual(['sent:flag', 'answered:flag', 'sent:flag', 'answered:flag'])
    expect(saves.sent.map(s => (s.body as { holds: boolean }).holds)).toEqual([true, false])
    const recorded = (await reviewOf(request, side.thread.thread_id, runId)).nights[0].messages
    expect(recorded[0].chips[0].state).toBe('confirmed')
    expect(recorded[1].chips[0].state).toBe('removed')
  })

  test('every kind of review click works while a save is pending: + Flag, remove, Change', async ({
    page,
    request,
  }) => {
    const { side } = await openSide(page, request)
    const [a, b, c] = side.messages.map(m => m.message_key)
    await chip(page, a, 'claim').getByRole('button', { name: 'Confirm Claim' }).click()
    await expect(chip(page, a, 'claim')).toHaveAttribute('data-chip-state', 'confirmed')
    const saves = await holdSaves(page)

    // spec: IXN-085.next-click-works
    await chip(page, b, 'big_moment').getByRole('button', { name: 'Confirm Big moment' }).click()
    await expect.poll(() => saves.events).toEqual(['sent:flag'])
    await chip(page, a, 'claim').getByRole('button', { name: 'Change Claim' }).click()
    await chip(page, a, 'claim').getByRole('button', { name: 'Remove Claim' }).click()
    await page.locator(`[data-message-key="${c}"]`).getByRole('button', { name: '+ Flag' }).click()
    await page.getByRole('menuitem', { name: 'Commitment' }).click()
    await expect(
      page.locator(`[data-message-key="${c}"]`).getByRole('button', { name: '+ Flag' })
    ).toBeEnabled()
    expect(saves.events).toEqual(['sent:flag'])

    saves.release()
    await expect(chip(page, c, 'commitment')).toHaveAttribute('data-chip-state', 'added')
    await expect(chip(page, a, 'claim')).toHaveAttribute('data-chip-state', 'removed')
    await expect(chip(page, b, 'big_moment')).toHaveAttribute('data-chip-state', 'confirmed')
    expect(saves.sent.map(s => (s.body as { kind: string; holds: boolean }).kind)).toEqual([
      'big_moment',
      'claim',
      'commitment',
    ])
  })

  test('Next pressed while a confirm is pending opens the next thread at once and closes after the confirm is recorded', async ({
    page,
    request,
  }) => {
    const { side, board, runId } = await openSide(page, request)
    const saves = await holdSaves(page)
    await chip(page, side.messages[0].message_key, 'claim')
      .getByRole('button', { name: 'Confirm Claim' })
      .click()
    await expect.poll(() => saves.events).toEqual(['sent:flag'])

    // spec: IXN-085.close-after-earlier-saves
    await page.keyboard.press('ArrowRight')
    await expect(page).toHaveURL(new RegExp(`/interactions/threads/${board.thread.thread_id}\\?`))
    await expect(page.locator('[data-thread-header]')).toBeVisible()
    expect(saves.events, 'the close is not sent while the confirm is pending').toEqual([
      'sent:flag',
    ])

    saves.release()
    await expect
      .poll(() => saves.events, { timeout: SLOW_TIMEOUT_MS })
      .toEqual(['sent:flag', 'answered:flag', 'sent:close', 'answered:close'])
    // The close saw the confirm: the night held a label, so the close labeled its one message without a chip.
    const night = (await reviewOf(request, side.thread.thread_id, runId)).nights[0]
    expect(night.labeled_none).toBe(1)
  })

  test('Next pressed after a confirm failed opens the next thread at once, holds the close and offers the failed save to retry', async ({
    page,
    request,
  }) => {
    const { side, board, runId } = await openSide(page, request)
    let failing = true
    await page.route(/\/threads\/[^/]+\/flag-labels/, route =>
      failing ? route.fulfill({ status: 500, body: '{}' }) : route.continue()
    )
    const closes: string[] = []
    page.on('request', req => {
      if (req.method() === 'POST' && /\/threads\/[^/]+\/close/.test(req.url())) {
        closes.push(req.url())
      }
    })
    await chip(page, side.messages[0].message_key, 'claim')
      .getByRole('button', { name: 'Confirm Claim' })
      .click()

    // spec: IXN-085.close-after-earlier-saves
    await page.keyboard.press('ArrowRight')
    await expect(page).toHaveURL(new RegExp(`/interactions/threads/${board.thread.thread_id}\\?`))
    await expect(page.locator('[data-thread-header]')).toBeVisible()
    const failure = page.locator('[data-save-failures]').getByRole('alert')
    await expect(failure).toContainText("Couldn't save a flag label", { timeout: SLOW_TIMEOUT_MS })
    await expect(failure).toContainText('Its close waits')
    expect(closes, 'the close is not sent after a save that failed').toEqual([])
    expect((await reviewOf(request, side.thread.thread_id, runId)).nights[0].labeled_none).toBe(0)

    failing = false
    await failure.getByRole('button', { name: 'Retry' }).click()
    await expect.poll(() => closes, { timeout: SLOW_TIMEOUT_MS }).toHaveLength(1)
    await expect(failure).toHaveCount(0)
    const night = (await reviewOf(request, side.thread.thread_id, runId)).nights[0]
    expect(night.messages[0].chips[0].state).toBe('confirmed')
    expect(night.labeled_none).toBe(1)
  })

  test('a failed confirm retried through its own chip after Next is recorded before the close it holds', async ({
    page,
    request,
  }) => {
    const { side, board, runId } = await openSide(page, request)
    const saves = await failSaves(page, 'flag')
    const confirm = chip(page, side.messages[0].message_key, 'claim').getByRole('button', {
      name: 'Confirm Claim',
    })
    await confirm.click()
    await expect(
      page.locator(`[data-message-key="${side.messages[0].message_key}"] [data-message-flags]`)
    ).toContainText("Couldn't save", { timeout: SLOW_TIMEOUT_MS })

    // spec: IXN-085.saves-in-order, IXN-085.close-after-earlier-saves
    await page.keyboard.press('ArrowRight')
    await expect(page).toHaveURL(new RegExp(`/interactions/threads/${board.thread.thread_id}\\?`))
    await expect(page.locator('[data-save-failures]').getByRole('alert')).toContainText(
      'Its close waits'
    )
    await page.keyboard.press('ArrowLeft')
    await expect(page).toHaveURL(new RegExp(`/interactions/threads/${side.thread.thread_id}\\?`))
    saves.failing = false
    await confirm.click()

    await expect.poll(() => saves.recorded, { timeout: SLOW_TIMEOUT_MS }).toEqual(['flag', 'close'])
    const night = (await reviewOf(request, side.thread.thread_id, runId)).nights[0]
    expect(night.messages[0].chips[0].state).toBe('confirmed')
    // The close saw the confirm: it labeled the night's one message without a chip none.
    expect(night.labeled_none).toBe(1)
  })

  test('a failed Done shows on its own thread with Retry, and Retry records the close and the saves behind it', async ({
    page,
    request,
  }) => {
    const { side, runId } = await openSide(page, request)
    await chip(page, side.messages[0].message_key, 'claim')
      .getByRole('button', { name: 'Confirm Claim' })
      .click()
    await expect(chip(page, side.messages[0].message_key, 'claim')).toHaveAttribute(
      'data-chip-state',
      'confirmed'
    )
    const saves = await failSaves(page, 'close')

    // spec: IXN-085.close-after-earlier-saves
    await page.getByRole('button', { name: 'Done' }).click()
    const failure = page.locator('[data-save-failures]').getByRole('alert')
    await expect(failure).toContainText("Couldn't save Done", { timeout: SLOW_TIMEOUT_MS })
    const later = chip(page, side.messages[1].message_key, 'big_moment').getByRole('button', {
      name: 'Confirm Big moment',
    })
    await later.click()
    await expect(later, 'a save after the failed Done waits for it').toBeDisabled()
    expect(saves.recorded).toEqual([])

    saves.failing = false
    await failure.getByRole('button', { name: 'Retry' }).click()
    await expect.poll(() => saves.recorded, { timeout: SLOW_TIMEOUT_MS }).toEqual(['close', 'flag'])
    await expect(failure).toHaveCount(0)
    const night = (await reviewOf(request, side.thread.thread_id, runId)).nights[0]
    expect(night.labeled_none).toBe(1)
    expect(night.messages[1].chips[0].state).toBe('confirmed')
  })

  test('Done pressed while Confirm Nones is pending closes after the night is labeled', async ({
    page,
    request,
  }) => {
    const world = await reviewWorld(testApi, request)
    const { quiet } = world
    const runId = await seedThreadsRun(request, [
      {
        interactionId: quiet.interactionId,
        nights: nightsOf(quiet.messages, [{ from: 0, to: 1 }]),
      },
    ])
    await startWithReview(page, true)
    await page.goto(threadUrl(quiet.thread.thread_id, { contact: world.cai.id, run: runId }))
    const confirm = page.getByRole('button', { name: 'Confirm Nones' })
    await expect(confirm).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    const saves = await holdSaves(page)

    // spec: IXN-085.next-click-works, IXN-085.close-after-earlier-saves
    await confirm.click()
    await expect.poll(() => saves.events).toEqual(['sent:nones'])
    const done = page.getByRole('button', { name: 'Done' })
    await expect(done).toBeEnabled()
    await done.click()
    await expect(done).toBeDisabled()
    expect(saves.events).toEqual(['sent:nones'])

    saves.release()
    await expect
      .poll(() => saves.events, { timeout: SLOW_TIMEOUT_MS })
      .toEqual(['sent:nones', 'answered:nones', 'sent:close', 'answered:close'])
    await expect(confirm).toHaveCount(0)
    expect((await reviewOf(request, quiet.thread.thread_id, runId)).status).toBe('reviewed')
  })
})
