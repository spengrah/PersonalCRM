import { test, expect } from './fixtures'
import type { APIRequestContext } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'
import { candidateCardByName, findCandidateByName } from './helpers/imports-helpers'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

/**
 * The '@handle' a declared telegram candidate actually carries, read back
 * from the candidate endpoint (the generator owns the handle, so a test that
 * rebuilt it from the namespace would be asserting against a string it
 * invented).
 */
async function telegramHandleOf(request: APIRequestContext, candidateId: string): Promise<string> {
  const res = await request.get(`${API_BASE_URL}/api/v1/imports/${candidateId}`, {
    headers: API_HEADERS,
  })
  expect(res.ok()).toBe(true)
  const handle: string = (await res.json())?.data?.metadata?.username
  expect(handle, 'the declared telegram candidate must carry a handle').toBeTruthy()
  return handle
}

/**
 * The declared telegram candidate's message_count, read back from the API —
 * the generator's message-count constant is internal, so a test asserting an
 * exact rendered count reads it back rather than hardcoding it.
 */
async function messageCountOf(request: APIRequestContext, candidateId: string): Promise<number> {
  const res = await request.get(`${API_BASE_URL}/api/v1/imports/${candidateId}`, {
    headers: API_HEADERS,
  })
  expect(res.ok()).toBe(true)
  const count: number = (await res.json())?.data?.metadata?.message_count
  expect(count, 'the declared telegram candidate must carry a message count').toBeTruthy()
  return count
}

// The evidence line generalized across discovery sources (#803): a chat-source
// candidate (telegram) and a Gmail candidate both render count/recency/counterpart
// evidence, not just gmail_correspondence.
test.describe('Imports evidence line @area:imports', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  // spec: IMP-048.count-recency-visible-any-source, IMP-048.counterpart-visible
  test('evidence renders for chat and gmail sources alike', async ({ page, request }) => {
    // IMP-036's "named" telegram candidate carries a display name distinct
    // from its username, so the card's existing username chip renders — the
    // evidence line must not repeat that identity, only count + recency.
    const telegramWorld = await testApi.seedBehavior('IMP-036')
    const telegramName = telegramWorld.entities['named'].name
    const telegramHandle = await telegramHandleOf(request, telegramWorld.entities['named'].id)
    const telegramMessageCount = await messageCountOf(request, telegramWorld.entities['named'].id)

    // IMP-037's fixture is the existing correspondence evidence badge, whose
    // rendered text this element must NOT change.
    const correspondenceWorld = await testApi.seedBehavior('IMP-037')
    const correspondenceName = correspondenceWorld.entities['corr'].name
    const coOccurringName = correspondenceWorld.entities['cooccur'].name

    await page.goto('/imports')
    await page.waitForLoadState('domcontentloaded')

    // --- Telegram (chat source): count + recency evidence line, peer
    // identity visible via the existing username chip on the card. The
    // shared queue is paginated under parallel-worker load, so the card is
    // located via the paging helper rather than a direct locator.
    await findCandidateByName(page, telegramName)
    const telegramCard = candidateCardByName(page, telegramName)
    await expect(telegramCard).toBeVisible({ timeout: 10000 })
    await expect(
      telegramCard.getByText(
        `${telegramMessageCount} ${telegramMessageCount === 1 ? 'message' : 'messages'}`
      )
    ).toBeVisible()
    await expect(telegramCard.getByText(/Last: [A-Za-z]{3} \d{1,2}, \d{4}/)).toBeVisible()
    await expect(telegramCard.getByRole('link', { name: telegramHandle })).toBeVisible()

    // --- Gmail correspondence: unchanged "Seen with X · N messages" text. ---
    await findCandidateByName(page, correspondenceName)
    const correspondenceCard = candidateCardByName(page, correspondenceName)
    await expect(correspondenceCard).toBeVisible()
    await expect(correspondenceCard.getByText(`Seen with ${coOccurringName}`)).toBeVisible()
    await expect(correspondenceCard.getByText('4 messages')).toBeVisible()
  })
})
