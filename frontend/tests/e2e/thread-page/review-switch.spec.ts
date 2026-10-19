import { test, expect, type Page } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from '../helpers/timeouts'
import { createTestAPI, TestAPI } from '../helpers/test-api'
import { afterwards, flag, reviewWorld, seedRun, startWithReview, threadUrl } from './helpers'

const switchOf = (page: Page) => page.getByRole('switch', { name: 'Review' })

test.describe('Review switch on the thread page @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('is one setting with the list, and stays as the user set it across visits', async ({
    page,
    request,
  }) => {
    const world = await reviewWorld(testApi, request)
    const { thread } = world.side
    await page.goto(threadUrl(thread.thread_id, { contact: world.ana.id }))
    await expect(switchOf(page)).toHaveAttribute('aria-checked', 'false', {
      timeout: SLOW_TIMEOUT_MS,
    })

    // spec: IXN-075.one-setting-both-pages
    await switchOf(page).click()
    await expect(switchOf(page)).toHaveAttribute('aria-checked', 'true')
    await page.getByRole('button', { name: 'Back to list' }).click()
    await expect(page).toHaveURL(/\/interactions\?tab=threads/)
    await expect(switchOf(page)).toHaveAttribute('aria-checked', 'true')
    await expect(page.getByLabel('Run')).toBeVisible({ timeout: SLOW_TIMEOUT_MS })

    // spec: IXN-075.stays-across-visits
    await page.reload()
    await expect(switchOf(page)).toHaveAttribute('aria-checked', 'true')
    await switchOf(page).click()
    await expect(switchOf(page)).toHaveAttribute('aria-checked', 'false')
    await page.goto(threadUrl(thread.thread_id, { contact: world.ana.id }))
    await expect(page.locator('[data-thread-header]')).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    await expect(switchOf(page)).toHaveAttribute('aria-checked', 'false')
  })

  test('shows no run, review status, flag, judgment box or review control with Review off', async ({
    page,
    request,
  }) => {
    const world = await reviewWorld(testApi, request)
    const { interactionId, thread, messages } = world.side
    const runId = await seedRun(request, interactionId, {
      nights: [
        {
          cut_off: afterwards(messages[2].sent_at),
          window_start: messages[0].message_key,
          new_messages: messages.map(m => m.message_key),
          flags: [flag(messages[1].message_key, 'claim', 0.9)],
        },
      ],
    })
    await startWithReview(page, true)
    // The run is the address's: the default is the newest run, which other workers' seeds can outdate.
    await page.goto(threadUrl(thread.thread_id, { contact: world.ana.id, run: runId }))
    // With Review on the same page shows the judgment, so the absences below are not vacuous.
    await expect(page.locator('[data-judgment-box]')).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    await expect(page.locator('[data-chip]')).toHaveCount(1)

    // spec: IXN-075.off-shows-no-judgment
    await switchOf(page).click()
    await expect(page.locator('[data-plain-thread]')).toBeVisible()
    await expect(page.locator('[data-message-key]')).toHaveCount(messages.length)
    for (const judged of [
      page.locator('[data-judgment-box]'),
      page.locator('[data-chip]'),
      page.locator('[data-thread-status]'),
      page.locator('[data-run]'),
      page.locator('[data-night-divider]'),
      page.getByRole('button', { name: '+ Flag' }),
      page.getByRole('button', { name: 'Done' }),
      page.getByRole('button', { name: /^(Confirm|Remove)/ }),
    ]) {
      await expect(judged).toHaveCount(0)
    }
  })
})
