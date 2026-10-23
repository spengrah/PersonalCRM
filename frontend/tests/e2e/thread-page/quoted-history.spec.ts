import { test, expect, type APIRequestContext, type Page } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from '../helpers/timeouts'
import { createTestAPI, TestAPI } from '../helpers/test-api'
import { entity, readThread, threadOf } from '../helpers/bench'
import { startWithReview, threadUrl } from './helpers'

/** The line ingest writes after quoted mail it trimmed (ING-047). */
const TRIMMED_MARKER = '[Older quoted mail trimmed]'

test.describe('Thread page quoted history @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  /** Opens the one email thread of the declared contact and returns its one message. */
  async function openThreadOf(page: Page, request: APIRequestContext, handle: string) {
    const seeded = await testApi.seedBehavior('IXN-097')
    const contact = entity(seeded, handle)
    const thread = await threadOf(request, contact.id, undefined)
    const { messages } = await readThread(request, thread.thread_id)
    expect(messages, 'the thread holds one message').toHaveLength(1)
    await startWithReview(page, false)
    await page.goto(threadUrl(thread.thread_id, { contact: contact.id }))
    const message = page.locator(`[data-message-key="${messages[0].message_key}"]`)
    await expect(message).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    return message
  }

  test('shows a quoted history collapsed under the message and opens it on request', async ({
    page,
    request,
  }) => {
    const message = await openThreadOf(page, request, 'ana')

    // spec: IXN-097.quoted-history-collapsed
    await expect(message.locator('[data-message-text]')).toHaveText('synthetic email body')
    const history = message.locator('[data-message-quoted-history]')
    const quote = history.getByText('Earlier synthetic question?')
    await expect(history).toHaveCount(1)
    await expect(quote).toBeHidden()
    await history.getByText('Quoted history', { exact: true }).click()
    await expect(quote).toBeVisible()
    await expect(history).toContainText('On Mon, Synthetic Sender wrote:')
    await expect(history).not.toContainText(TRIMMED_MARKER)
  })

  test('ends a trimmed quoted history with the trimmed marker', async ({ page, request }) => {
    const message = await openThreadOf(page, request, 'ben')

    // spec: IXN-097.trimmed-marker-shown
    const history = message.locator('[data-message-quoted-history]')
    await history.getByText('Quoted history', { exact: true }).click()
    await expect(history).toContainText('Earlier synthetic line 000')
    await expect(history.locator('[data-quoted-history-text]')).toHaveText(
      new RegExp(`${TRIMMED_MARKER.replace(/[[\]]/g, '\\$&')}$`)
    )
  })

  test('shows nothing extra for a message without a quoted history', async ({ page, request }) => {
    const message = await openThreadOf(page, request, 'cai')

    // spec: IXN-097.no-quoted-history-nothing-extra
    await expect(message.locator('[data-message-text]')).toHaveText('synthetic email body')
    await expect(message.locator('[data-message-quoted-history]')).toHaveCount(0)
    await expect(message.getByText('Quoted history', { exact: true })).toHaveCount(0)
  })
})
