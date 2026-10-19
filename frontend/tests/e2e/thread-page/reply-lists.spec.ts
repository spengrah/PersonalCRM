import { test, expect, type Page } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from '../helpers/timeouts'
import { createTestAPI, TestAPI } from '../helpers/test-api'
import { listItem, proposalList } from '../helpers/bench'
import { nightsOf, reviewOf, reviewWorld, seedRun, startWithReview, threadUrl } from './helpers'

/** An answered question's option probabilities, as a method stores them. */
function asked(id: string, probabilities: Record<string, number>) {
  const answer = Object.entries(probabilities).sort(([, a], [, b]) => b - a)[0][0]
  return { id, probabilities, original_answer: answer }
}

test.describe('Reply lists in a judgment box @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  /**
   * The board chat's first night, where the user owes ana a reply and ben and
   * cai owe nothing; ben came closest to owing the user one.
   */
  async function openReplies(page: Page, request: Parameters<typeof reviewWorld>[1]) {
    const world = await reviewWorld(testApi, request)
    const { interactionId, thread, messages } = world.board
    const { ana, ben, cai } = world
    const [night] = nightsOf(messages, [{ from: 0, to: 4 }])
    night.ball_in_court = [
      {
        question_id: 'ball_in_court:P2',
        contact_id: ana.id,
        outcome: 'i_owe_reply',
        confidence: 0.9,
      },
      {
        question_id: 'ball_in_court:P3',
        contact_id: ben.id,
        outcome: 'nobody_owes',
        confidence: 0.65,
      },
      {
        question_id: 'ball_in_court:P4',
        contact_id: cai.id,
        outcome: 'nobody_owes',
        confidence: 0.85,
      },
    ]
    night.questions = [
      asked('ball_in_court:P2', { i_owe_reply: 0.9, they_owe_reply: 0.06, nobody_owes: 0.04 }),
      asked('ball_in_court:P3', { i_owe_reply: 0.05, they_owe_reply: 0.3, nobody_owes: 0.65 }),
      asked('ball_in_court:P4', { i_owe_reply: 0.05, they_owe_reply: 0.1, nobody_owes: 0.85 }),
    ]
    const runId = await seedRun(request, interactionId, { nights: [night] })
    const reviews: { url: string; body: unknown }[] = []
    page.on('request', req => {
      if (req.method() === 'POST' && /\/judgment-outcomes\/[^/]+\/review/.test(req.url())) {
        reviews.push({ url: req.url(), body: req.postDataJSON() })
      }
    })
    await startWithReview(page, true)
    await page.goto(threadUrl(thread.thread_id, { contact: ana.id, run: runId }))
    const box = page.locator('[data-judgment-box]')
    await expect(box).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    return {
      world,
      runId,
      threadId: thread.thread_id,
      box,
      iOwe: proposalList(box, 'I owe a reply'),
      owesMe: proposalList(box, 'Owes me a reply'),
      reviews,
    }
  }

  test('shows the lists side by side with their items’ state, and an empty list naming its closest decision', async ({
    page,
    request,
  }) => {
    const { world, box, iOwe, owesMe } = await openReplies(page, request)

    // spec: IXN-086.items-with-state, IXN-086.empty-list-names-closest
    await expect(listItem(iOwe, world.ana.name)).toHaveAttribute('data-state', 'not_acting')
    await expect(owesMe.locator('[data-none]')).toHaveText(`None closest: ${world.ben.name} 30%`)
    const [left, right] = await Promise.all([iOwe.boundingBox(), owesMe.boundingBox()])
    expect(right!.x, 'the lists sit side by side').toBeGreaterThan(left!.x + left!.width - 1)
    expect(right!.y).toBeCloseTo(left!.y, -1)

    // The judgment box carries none of the shipped bench's proposal chrome.
    await expect(box.locator('[data-proposal-status]')).toHaveCount(0)
    await expect(box.locator('[data-proposal]')).toHaveCount(0)
    await expect(box.getByRole('button', { name: 'Confirm Nones' })).toHaveCount(0)
  })

  test('confirming an item saves it with the page’s run and updates the night’s status', async ({
    page,
    request,
  }) => {
    const { world, runId, threadId, box, iOwe, reviews } = await openReplies(page, request)
    await expect(box.getByText('1 to review')).toBeVisible()

    // spec: IXN-086.inline-corrections
    await iOwe.getByRole('button', { name: `Confirm ${world.ana.name}` }).click()
    await expect(listItem(iOwe, world.ana.name)).toHaveAttribute('data-mark', 'confirmed', {
      timeout: SLOW_TIMEOUT_MS,
    })
    expect(reviews).toHaveLength(1)
    expect(new URL(reviews[0].url).searchParams.get('run_id')).toBe(runId)
    expect(reviews[0].body).toEqual({ right_outcome: 'i_owe_reply' })
    await expect(box.getByText(/to review$/)).toHaveCount(0)
    await expect(page.locator('[data-thread-header] [data-thread-status]')).toHaveText('Reviewed')
    const night = (await reviewOf(request, threadId, runId)).nights[0]
    expect(night.to_review).toBe(0)
  })

  test('removing an item records the right outcome as nobody owes and strikes the item through', async ({
    page,
    request,
  }) => {
    const { world, iOwe, reviews } = await openReplies(page, request)

    // spec: IXN-086.inline-corrections
    await iOwe.getByRole('button', { name: `Remove ${world.ana.name}` }).click()
    const item = listItem(iOwe, world.ana.name)
    await expect(item).toHaveAttribute('data-mark', 'removed', { timeout: SLOW_TIMEOUT_MS })
    await expect(item.locator('[data-item-text]')).toHaveClass(/line-through/)
    expect(reviews.map(r => r.body)).toEqual([{ right_outcome: 'nobody_owes' }])
  })

  test('an Add picker on an empty list offers the negative decisions, and choosing one adds it', async ({
    page,
    request,
  }) => {
    const { world, owesMe, reviews } = await openReplies(page, request)
    await expect(owesMe.locator('[data-none]')).toBeVisible()

    // spec: IXN-086.add-picker-negatives
    await owesMe.getByRole('button', { name: 'Add', exact: true }).click()
    const picker = owesMe.getByRole('group', { name: 'Add to Owes me a reply' })
    const offered = picker.getByRole('button', { name: /^(?!Close)/ })
    // Ben's and cai's decisions are Nones; ana's is the other list's item, which adding moves.
    expect(new Set(await offered.allInnerTexts())).toEqual(
      new Set([world.ana.name, world.ben.name, world.cai.name])
    )
    await picker.getByRole('button', { name: world.cai.name }).click()
    await expect(listItem(owesMe, world.cai.name)).toHaveAttribute('data-mark', 'added', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(owesMe.locator('[data-none]')).toHaveCount(0)
    expect(reviews.map(r => r.body)).toEqual([{ right_outcome: 'they_owe_reply' }])
  })

  test('corrects the lists inline: removes an item and adds another person, with no modal', async ({
    page,
    request,
  }) => {
    const { world, iOwe, owesMe, reviews } = await openReplies(page, request)
    await iOwe.getByRole('button', { name: `Remove ${world.ana.name}` }).click()
    await expect(listItem(iOwe, world.ana.name)).toHaveAttribute('data-mark', 'removed', {
      timeout: SLOW_TIMEOUT_MS,
    })

    // spec: IXN-086.inline-corrections
    await owesMe.getByRole('button', { name: 'Add', exact: true }).click()
    await owesMe
      .getByRole('group', { name: 'Add to Owes me a reply' })
      .getByRole('button', { name: world.ben.name })
      .click()
    await expect(listItem(owesMe, world.ben.name)).toHaveAttribute('data-mark', 'added', {
      timeout: SLOW_TIMEOUT_MS,
    })
    expect(reviews.map(r => r.body)).toEqual([
      { right_outcome: 'nobody_owes' },
      { right_outcome: 'they_owe_reply' },
    ])
    await expect(page.getByRole('dialog')).toHaveCount(0)
  })

  test('a reviewed item offers Change, which re-reviews it', async ({ page, request }) => {
    const { world, iOwe, reviews } = await openReplies(page, request)
    await iOwe.getByRole('button', { name: `Confirm ${world.ana.name}` }).click()
    const item = listItem(iOwe, world.ana.name)
    await expect(item).toHaveAttribute('data-mark', 'confirmed', { timeout: SLOW_TIMEOUT_MS })
    await expect(iOwe.getByRole('button', { name: `Confirm ${world.ana.name}` })).toHaveCount(0)

    // spec: IXN-086.change-after-review
    await item.getByRole('button', { name: `Change the review of ${world.ana.name}` }).click()
    await iOwe.getByRole('button', { name: `Remove ${world.ana.name}` }).click()
    await expect(item).toHaveAttribute('data-mark', 'removed', { timeout: SLOW_TIMEOUT_MS })
    expect(reviews.map(r => r.body)).toEqual([
      { right_outcome: 'i_owe_reply' },
      { right_outcome: 'nobody_owes' },
    ])
  })
})
