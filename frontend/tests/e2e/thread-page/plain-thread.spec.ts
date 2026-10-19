import { test, expect, type Page } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from '../helpers/timeouts'
import { createTestAPI, TestAPI } from '../helpers/test-api'
import { entity, readThread, threadOf } from '../helpers/bench'
import { reviewWorld, startWithReview, threadUrl } from './helpers'

/** The color a utility class draws text in, as the browser computes it. */
function colorOf(page: Page, className: string): Promise<string> {
  return page.evaluate(name => {
    const probe = document.createElement('span')
    probe.className = name
    document.body.append(probe)
    const color = getComputedStyle(probe).color
    probe.remove()
    return color
  }, className)
}

test.describe('Thread page with Review off @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('shows the messages in order in one card, the earlier ones behind a collapsed toggle', async ({
    page,
    request,
  }) => {
    const world = await reviewWorld(testApi, request)
    const { thread, messages } = world.board
    expect(messages.length, 'the chat is longer than the page first shows').toBe(30)
    await startWithReview(page, false)
    await page.goto(threadUrl(thread.thread_id, { contact: world.ana.id }))
    const card = page.locator('[data-plain-thread]')
    const shown = card.locator('[data-message-key]')

    // spec: IXN-079.messages-in-order, IXN-079.earlier-messages-collapsed
    await expect(shown).toHaveCount(20, { timeout: SLOW_TIMEOUT_MS })
    const recent = messages.slice(10).map(m => m.message_key)
    expect(
      await shown.evaluateAll(nodes => nodes.map(n => n.getAttribute('data-message-key')))
    ).toEqual(recent)
    const toggle = card.getByRole('button', { name: 'Earlier messages' })
    await expect(toggle).toHaveAttribute('aria-expanded', 'false')
    const toggleBox = await toggle.boundingBox()
    const firstBox = await shown.first().boundingBox()
    expect(toggleBox!.y, 'the toggle sits at the top').toBeLessThan(firstBox!.y)

    await toggle.click()
    await expect(shown).toHaveCount(30)
    expect(
      await shown.evaluateAll(nodes => nodes.map(n => n.getAttribute('data-message-key')))
    ).toEqual(messages.map(m => m.message_key))
  })

  test('draws each message as its sender and time on one line above its text', async ({
    page,
    request,
  }) => {
    const world = await reviewWorld(testApi, request)
    const { thread, messages } = world.side
    await startWithReview(page, false)
    await page.goto(threadUrl(thread.thread_id, { contact: world.ana.id }))
    const message = page.locator(`[data-message-key="${messages[1].message_key}"]`)
    await expect(message).toBeVisible({ timeout: SLOW_TIMEOUT_MS })

    // spec: IXN-079.sender-and-time-one-line
    const sender = message.locator('[data-message-sender]')
    const time = message.locator('[data-message-time]')
    const text = message.locator('[data-message-text]')
    await expect(sender).toHaveText(messages[1].sender)
    await expect(text).toHaveText(messages[1].text)
    const [senderBox, timeBox, textBox] = await Promise.all([
      sender.boundingBox(),
      time.boundingBox(),
      text.boundingBox(),
    ])
    expect(timeBox!.y, 'sender and time share a line').toBeCloseTo(senderBox!.y, -1)
    expect(textBox!.y, 'the text sits beneath the line').toBeGreaterThan(
      senderBox!.y + senderBox!.height - 2
    )
    expect(await sender.evaluate(node => getComputedStyle(node).fontWeight)).toBe('600')
    expect(await sender.evaluate(node => getComputedStyle(node).color)).toBe(
      await colorOf(page, 'text-gray-700')
    )
  })

  test('draws the user’s own sender name in blue', async ({ page, request }) => {
    // The IXN-076 world seeds a chat for each of two pages of threads.
    test.slow()
    const seeded = await testApi.seedBehavior('IXN-076')
    const meeting = await threadOf(request, entity(seeded, 'host').id, undefined)
    const mine = (await readThread(request, meeting.thread_id)).messages.find(
      m => m.sender === 'Me'
    )!
    expect(mine, 'the meeting note is the user’s').toBeTruthy()
    await startWithReview(page, false)
    await page.goto(threadUrl(meeting.thread_id))

    // spec: IXN-079.sender-and-time-one-line
    const sender = page
      .locator(`[data-message-key="${mine.message_key}"]`)
      .locator('[data-message-sender]')
    await expect(sender).toHaveText('Me', { timeout: SLOW_TIMEOUT_MS })
    expect(await sender.evaluate(node => getComputedStyle(node).color)).toBe(
      await colorOf(page, 'text-blue-700')
    )
  })
})
