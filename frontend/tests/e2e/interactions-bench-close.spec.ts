import { test, expect, type APIRequestContext, type Locator, type Route } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from './helpers/timeouts'
import { createTestAPI, TestAPI, type SeedBehaviorResult } from './helpers/test-api'
import {
  benchRow,
  contactRows,
  entity,
  expandRow,
  listItem,
  proposalList,
  rowMessages,
  seedJudgmentRun,
  type SeedOutcome,
} from './helpers/bench'

/** The proposal of a method in an expanded row. */
function proposalOf(region: Locator, method: string): Locator {
  return region.locator('[data-proposal]').filter({ hasText: method })
}

/** The group world's shared row of its three members, and its latest message. */
async function groupThread(request: APIRequestContext, seeded: SeedBehaviorResult) {
  const sam = entity(seeded, 'sam')
  const rows = await contactRows(request, sam.id)
  const shared = rows.find(row => row.participants.length === 3)!
  expect(shared, 'the user’s message is one row of the three members').toBeTruthy()
  const latest = (await rowMessages(request, shared)).at(-1)!
  return { shared, latest }
}

function noCommitment(direction: 'mine' | 'theirs', contactId: string, n: number): SeedOutcome {
  return {
    question_id: `commitment_${direction}:P${n}`,
    contact_id: contactId,
    direction,
    outcome: 'none',
    confidence: 0.9,
  }
}

/**
 * A group judgment where the user owes Sam a reply and Kim owes the user one,
 * and everything else is None.
 */
async function seedTwoItems(request: APIRequestContext, seeded: SeedBehaviorResult) {
  const [sam, kim, lee] = ['sam', 'kim', 'lee'].map(h => entity(seeded, h))
  const { shared, latest } = await groupThread(request, seeded)
  await seedJudgmentRun(request, {
    judgments: [
      {
        thread_interaction_id: entity(seeded, 'group').id,
        cut_off: latest.sent_at,
        ball_in_court: [
          {
            question_id: 'ball_in_court:P2',
            contact_id: sam.id,
            outcome: 'i_owe_reply',
            confidence: 0.9,
          },
          {
            question_id: 'ball_in_court:P3',
            contact_id: kim.id,
            outcome: 'they_owe_reply',
            confidence: 0.9,
          },
          {
            question_id: 'ball_in_court:P4',
            contact_id: lee.id,
            outcome: 'nobody_owes',
            confidence: 0.9,
          },
        ],
        commitments: [noCommitment('mine', lee.id, 4), noCommitment('theirs', lee.id, 4)],
        big_moment: { question_id: 'big_moment', outcome: 'none', confidence: 0.95 },
      },
    ],
  })
  return { sam, kim, lee, shared }
}

/** Holds every request a route matches until released. */
function hold() {
  let release!: () => void
  const released = new Promise<void>(resolve => (release = resolve))
  const held: Route[] = []
  return {
    handler: async (route: Route) => {
      held.push(route)
      await released
      await route.continue()
    },
    held,
    release,
  }
}

/** The review controls of a proposal, whose names start with the act they send. */
function reviewControls(proposal: Locator): Locator {
  return proposal.getByRole('button', { name: /^(Confirm|Remove|Change|Add|Accept|Keep)/ })
}

test.describe('Interactions bench close and saves @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('a close made while a review is in flight takes effect after the review is recorded', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-048')
    const { sam, shared } = await seedTwoItems(request, seeded)
    await page.goto(`/interactions?contact=${sam.id}`)
    const bench = benchRow(page, shared.item.identity.id)
    const proposal = proposalOf(await expandRow(bench), 'bench-production')
    const iOwe = proposalList(proposal, 'I owe a reply')
    const review = hold()
    await page.route('**/api/v1/judgment-outcomes/*/review', review.handler)

    // spec: IXN-064.close-after-saves
    await iOwe.getByRole('button', { name: `Confirm ${sam.name}` }).click()
    await expect.poll(() => review.held.length).toBe(1)
    const closed = page.waitForResponse(response => response.url().includes('/judgments/close'))
    await bench.getByRole('button', { name: 'Collapse content', exact: true }).click()
    review.release()
    expect((await closed).status()).toBe(200)
    await expect(bench.locator('[data-review-indicator]')).toHaveText('1 to review')
    await page.reload()
    const reopened = proposalOf(await expandRow(bench), 'bench-production')
    await expect(listItem(proposalList(reopened, 'I owe a reply'), sam.name)).toHaveAttribute(
      'data-mark',
      'confirmed',
      { timeout: SLOW_TIMEOUT_MS }
    )
    await expect(
      proposalList(reopened, 'Big moment').locator('[data-none] [data-nones-mark]')
    ).toHaveText('Confirmed')
    await expect(
      proposalList(reopened, 'Commitments').locator('[data-none] [data-nones-mark]')
    ).toHaveText('Confirmed')
  })

  test('after a review the saved proposal’s controls stay disabled until its recorded state shows, without the list read', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-048')
    const { sam, kim, shared } = await seedTwoItems(request, seeded)
    await page.goto(`/interactions?contact=${sam.id}`)
    const bench = benchRow(page, shared.item.identity.id)
    const proposal = proposalOf(await expandRow(bench), 'bench-production')
    const iOwe = proposalList(proposal, 'I owe a reply')
    await expect(reviewControls(proposal).first()).toBeEnabled()
    const list = hold()
    await page.route(url => url.pathname === '/api/v1/interactions', list.handler)
    const review = hold()
    await page.route('**/api/v1/judgment-outcomes/*/review', review.handler)

    // spec: IXN-069.disabled-until-recorded
    await iOwe.getByRole('button', { name: `Confirm ${sam.name}` }).click()
    await expect.poll(() => review.held.length).toBe(1)
    const controls = await reviewControls(proposal).all()
    expect(controls.length).toBeGreaterThan(0)
    for (const control of controls) await expect(control).toBeDisabled()

    // spec: IXN-069.recorded-without-reload
    review.release()
    await expect(listItem(iOwe, sam.name).locator('[data-review-mark]')).toHaveText('Confirmed', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(proposal.locator('[data-proposal-status]')).toHaveText('1 to review')
    await expect(
      proposalList(proposal, 'Owes me a reply').getByRole('button', {
        name: `Confirm ${kim.name}`,
      })
    ).toBeEnabled()
    // spec: IXN-069.row-agrees-with-proposals
    await expect(bench.locator('[data-review-indicator]')).toHaveText('1 to review')
    expect(list.held.length, 'the list read the save started is still held').toBeGreaterThan(0)
    list.release()
  })

  test('after Confirm Nones the row’s status changes with its proposal, without the list read', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-048')
    const [sam, kim, lee] = ['sam', 'kim', 'lee'].map(h => entity(seeded, h))
    const { shared, latest } = await groupThread(request, seeded)
    await seedJudgmentRun(request, {
      judgments: [
        {
          thread_interaction_id: entity(seeded, 'group').id,
          cut_off: latest.sent_at,
          ball_in_court: [sam, kim, lee].map((contact, i) => ({
            question_id: `ball_in_court:P${i + 2}`,
            contact_id: contact.id,
            outcome: 'nobody_owes',
            confidence: 0.9,
          })),
          commitments: [noCommitment('mine', lee.id, 4), noCommitment('theirs', lee.id, 4)],
          big_moment: { question_id: 'big_moment', outcome: 'none', confidence: 0.95 },
        },
      ],
    })
    await page.goto(`/interactions?contact=${sam.id}`)
    const bench = benchRow(page, shared.item.identity.id)
    const proposal = proposalOf(await expandRow(bench), 'bench-production')
    await expect(bench.locator('[data-review-indicator]')).toHaveText('Nones to review')
    const list = hold()
    await page.route(url => url.pathname === '/api/v1/interactions', list.handler)

    // spec: IXN-069.row-agrees-with-proposals
    await proposal.getByRole('button', { name: 'Confirm Nones' }).click()
    await expect(proposal.locator('[data-proposal-status]')).toHaveText('All reviewed', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(bench.locator('[data-review-indicator]')).toHaveText('Reviewed')
    expect(list.held.length, 'the list read the save started is still held').toBeGreaterThan(0)
    list.release()
  })

  test('a removed commitment keeps its source message, during a Change too', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-048')
    const lee = entity(seeded, 'lee')
    const { shared, latest } = await groupThread(request, seeded)
    await seedJudgmentRun(request, {
      judgments: [
        {
          thread_interaction_id: entity(seeded, 'group').id,
          cut_off: latest.sent_at,
          questions: [
            {
              id: 'commitment_mine:P4/source_message',
              probabilities: { [latest.content_ref]: 0.9 },
              original_answer: latest.content_ref,
            },
          ],
          commitments: [
            {
              question_id: 'commitment_mine:P4',
              contact_id: lee.id,
              direction: 'mine',
              outcome: 'open',
              confidence: 0.9,
            },
          ],
        },
      ],
    })
    await page.goto(`/interactions?contact=${lee.id}`)
    const proposal = proposalOf(
      await expandRow(benchRow(page, shared.item.identity.id)),
      'bench-production'
    )
    const mine = listItem(proposalList(proposal, 'Commitments'), `Me → ${lee.name}`)
    await expect(mine.locator('[data-source-message]')).toContainText(`“${latest.body}”`)

    // spec: IXN-070.removed-keeps-source
    await mine.getByRole('button', { name: `Remove Me → ${lee.name}` }).click()
    await expect(mine.locator('[data-review-mark]')).toHaveText('Removed by you', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(mine.locator('[data-item-text]')).toHaveClass(/line-through/)
    await expect(mine.locator('[data-source-message]')).toContainText(`“${latest.body}”`)

    // spec: IXN-070.kept-during-change
    await mine.getByRole('button', { name: `Change the review of Me → ${lee.name}` }).click()
    await expect(mine.getByRole('button', { name: `Confirm Me → ${lee.name}` })).toBeVisible()
    await expect(mine.locator('[data-source-message]')).toContainText(`“${latest.body}”`)
  })

  test('scrolled to the end of a long open row, the user closes it from its header', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-048')
    const { sam, shared } = await seedTwoItems(request, seeded)
    await page.setViewportSize({ width: 1280, height: 480 })
    await page.goto(`/interactions?contact=${sam.id}`)
    const bench = benchRow(page, shared.item.identity.id)
    const region = await expandRow(bench)
    await expect(proposalOf(region, 'bench-production')).toBeVisible()
    expect(
      (await region.boundingBox())!.height,
      'the drill-down is taller than the viewport'
    ).toBeGreaterThan(480)

    // spec: IXN-071.header-stays-visible
    await region
      .locator('[data-proposal]')
      .last()
      .evaluate(el => el.scrollIntoView({ block: 'end' }))
    const collapse = bench.getByRole('button', { name: 'Collapse content', exact: true })
    await expect(collapse).toBeInViewport()
    await collapse.click()
    await expect(region).toHaveCount(0)
  })

  test('a drill-down message shows its sender and time on one line, its text beneath', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-048')
    const { sam, shared } = await seedTwoItems(request, seeded)
    await page.goto(`/interactions?contact=${sam.id}`)
    const region = await expandRow(benchRow(page, shared.item.identity.id))
    const message = region.locator('[data-message-id]').first()
    await expect(message).toBeVisible({ timeout: SLOW_TIMEOUT_MS })

    // spec: IXN-072.sender-and-time-one-line
    const sender = (await message.locator('[data-message-sender]').boundingBox())!
    const time = (await message.locator('[data-message-timestamp]').boundingBox())!
    const body = (await message.locator('[data-message-body]').boundingBox())!
    expect(time.x).toBeGreaterThan(sender.x + sender.width - 1)
    expect(time.y).toBeLessThan(sender.y + sender.height)
    expect(sender.y).toBeLessThan(time.y + time.height)
    expect(body.y).toBeGreaterThanOrEqual(
      Math.max(sender.y + sender.height, time.y + time.height) - 1
    )
  })
})
