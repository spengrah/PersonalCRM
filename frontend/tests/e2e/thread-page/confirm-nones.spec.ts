import { test, expect } from '@playwright/test'
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

test.describe('Confirm Nones on the thread page @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('a night that shows nothing offers Confirm Nones, which labels it and ends the offer', async ({
    page,
    request,
  }) => {
    const world = await reviewWorld(testApi, request)
    const { quiet } = world
    const runId = await seedRun(request, quiet.interactionId, {
      nights: nightsOf(quiet.messages, [{ from: 0, to: 1 }]),
    })
    expect((await reviewOf(request, quiet.thread.thread_id, runId)).status).toBe('nothing_flagged')
    await startWithReview(page, true)
    await page.goto(threadUrl(quiet.thread.thread_id, { contact: world.cai.id, run: runId }))
    const box = page.locator('[data-judgment-box]')
    const confirm = box.getByRole('button', { name: 'Confirm Nones' })

    // spec: IXN-084.offered-when-nothing-shown
    await expect(confirm).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    await expect(page.locator('[data-thread-header] [data-thread-status]')).toHaveText(
      'Nothing flagged'
    )

    // spec: IXN-084.night-labeled-and-offer-ends
    await confirm.click()
    await expect(confirm).toHaveCount(0)
    await expect(page.locator('[data-thread-header] [data-thread-status]')).toHaveText('Reviewed')
    const night = (await reviewOf(request, quiet.thread.thread_id, runId)).nights[0]
    expect(night.holds_label).toBe(true)
    expect(night.confirm_nones).toBe(false)
    await page.reload()
    await expect(page.locator('[data-judgment-box]')).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    await expect(page.getByRole('button', { name: 'Confirm Nones' })).toHaveCount(0)
  })

  test('a night showing a flag offers no Confirm Nones', async ({ page, request }) => {
    const world = await reviewWorld(testApi, request)
    const { quiet } = world
    const runId = await seedRun(request, quiet.interactionId, {
      nights: nightsOf(quiet.messages, [
        { from: 0, to: 1, flags: [flag(quiet.messages[0].message_key, 'claim', 0.9)] },
      ]),
    })
    await startWithReview(page, true)
    await page.goto(threadUrl(quiet.thread.thread_id, { contact: world.cai.id, run: runId }))

    // spec: IXN-084.not-offered-otherwise
    await expect(page.locator('[data-chip="claim"]')).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    await expect(page.getByRole('button', { name: 'Confirm Nones' })).toHaveCount(0)
  })
})
