import { test, expect, type Page } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from '../helpers/timeouts'
import { createTestAPI, TestAPI } from '../helpers/test-api'
import { entity, fetchThreads } from '../helpers/bench'
import { reviewWorld, startWithReview, threadUrl } from './helpers'

const switchOf = (page: Page) => page.getByRole('switch', { name: 'Review' })
const pagerText = (page: Page) => page.getByText(/^\d+ of \d+$/)

test.describe('Thread page navigation @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    // The IXN-076 world seeds a chat for each of two pages of threads.
    test.slow()
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  /** The type-filtered list of the IXN-076 world's `many` contact: 21 Telegram chats, two list pages. */
  async function manyThreads(request: Parameters<typeof fetchThreads>[0]) {
    const seeded = await testApi.seedBehavior('IXN-076')
    const many = entity(seeded, 'many')
    const { data } = await fetchThreads(request, {
      contact_id: many.id,
      type: 'message',
      limit: 100,
    })
    expect(data.items).toHaveLength(21)
    return { many, ids: data.items.map(row => row.thread_id) }
  }

  test('walks the list as filtered with the pager and the arrow keys, and stops at either end', async ({
    page,
    request,
  }) => {
    const { many, ids } = await manyThreads(request)
    const query = { type: 'message', contact: many.id }
    await page.goto(threadUrl(ids[0], query))

    // spec: IXN-078.navigation-bar, IXN-078.header-names-thread
    await expect(pagerText(page)).toHaveText('1 of 21', { timeout: SLOW_TIMEOUT_MS })
    await expect(page.getByRole('button', { name: 'Back to list' })).toBeVisible()
    await expect(switchOf(page)).toBeVisible()
    const header = page.locator('[data-thread-header]')
    await expect(header.locator('[data-venue-chip="telegram"]')).toBeVisible()
    await expect(header.locator('[data-group-chip]')).toBeVisible()
    await expect(header.getByRole('link', { name: many.name })).toHaveAttribute(
      'href',
      `/contacts/${many.id}`
    )
    await expect(page.getByRole('button', { name: 'Previous thread' })).toBeDisabled()

    // spec: IXN-078.arrow-keys-move
    await page.keyboard.press('ArrowRight')
    await expect(pagerText(page)).toHaveText('2 of 21')
    await expect(page).toHaveURL(new RegExp(`/interactions/threads/${ids[1]}\\?`))
    await expect(page).toHaveURL(/type=message/)
    await page.keyboard.press('ArrowLeft')
    await expect(pagerText(page)).toHaveText('1 of 21')
    await expect(page).toHaveURL(new RegExp(`/interactions/threads/${ids[0]}\\?`))

    // spec: IXN-078.navigation-bar
    await page.getByRole('button', { name: 'Next thread' }).click()
    await expect(pagerText(page)).toHaveText('2 of 21')

    // The pager stops past either end.
    await page.goto(threadUrl(ids[20], { ...query, page: '2' }))
    await expect(pagerText(page)).toHaveText('21 of 21', { timeout: SLOW_TIMEOUT_MS })
    await expect(page.getByRole('button', { name: 'Next thread' })).toBeDisabled()
    await page.keyboard.press('ArrowRight')
    await expect(page).toHaveURL(new RegExp(`/interactions/threads/${ids[20]}\\?`))
    await expect(pagerText(page)).toHaveText('21 of 21')
  })

  test('Back to list returns to the list with its filters and page', async ({ page, request }) => {
    const { many, ids } = await manyThreads(request)
    await page.goto(threadUrl(ids[20], { type: 'message', contact: many.id, page: '2' }))
    await expect(pagerText(page)).toHaveText('21 of 21', { timeout: SLOW_TIMEOUT_MS })

    // spec: IXN-078.navigation-bar
    await page.getByRole('button', { name: 'Back to list' }).click()
    await expect(page).toHaveURL(`/interactions?tab=threads&type=message&contact=${many.id}&page=2`)
    await expect(page.getByRole('tab', { name: 'Threads' })).toHaveAttribute(
      'aria-selected',
      'true'
    )
    await expect(
      page.getByRole('list', { name: 'Threads' }).locator(`[data-thread-id="${ids[20]}"]`)
    ).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
  })

  test('a thread address loaded directly holds the page and its list position', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-076')
    const ana = entity(seeded, 'ana')
    const mail = (await fetchThreads(request, { contact_id: ana.id })).data.items.find(
      thread => thread.venue === 'email'
    )!

    // spec: IXN-078.own-address-with-filters
    await page.goto(threadUrl(mail.thread_id, { contact: ana.id }))
    const header = page.locator('[data-thread-header]')
    await expect(header.locator('[data-venue-chip="email"]')).toBeVisible({
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(header.getByRole('link', { name: ana.name })).toBeVisible()
    await expect(pagerText(page)).toHaveText(/^\d+ of 2$/)
    expect(new URL(page.url()).pathname).toBe(`/interactions/threads/${mail.thread_id}`)
  })

  test('the bar and the header stay at the top while the page scrolls', async ({
    page,
    request,
  }) => {
    const world = await reviewWorld(testApi, request)
    await startWithReview(page, false)
    await page.goto(threadUrl(world.board.thread.thread_id, { contact: world.ana.id }))
    const bar = page.getByRole('button', { name: 'Back to list' })
    const header = page.locator('[data-thread-header]')
    await expect(header).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    const before = [await bar.boundingBox(), await header.boundingBox()]

    // spec: IXN-078.bar-and-header-stay
    await page.evaluate(() => window.scrollTo(0, document.body.scrollHeight))
    expect(await page.evaluate(() => window.scrollY)).toBeGreaterThan(300)
    const after = [await bar.boundingBox(), await header.boundingBox()]
    expect(after[0]!.y).toBeCloseTo(before[0]!.y, 0)
    expect(after[1]!.y).toBeCloseTo(before[1]!.y, 0)
  })
})
