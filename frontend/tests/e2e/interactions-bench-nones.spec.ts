import { test, expect, type APIRequestContext, type Locator } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from './helpers/timeouts'
import { createTestAPI, TestAPI, type SeedBehaviorResult } from './helpers/test-api'
import {
  benchRow,
  contactRows,
  entity,
  expandRow,
  proposalList,
  rowMessages,
  seedJudgmentRun,
} from './helpers/bench'

/** The proposal of a method in an expanded row. */
function proposalOf(region: Locator, method: string): Locator {
  return region.locator('[data-proposal]').filter({ hasText: method })
}

/** An answered question's option probabilities, as a method stores them. */
function asked(id: string, probabilities: Record<string, number>) {
  const answer = Object.entries(probabilities).sort(([, a], [, b]) => b - a)[0][0]
  return { id, probabilities, original_answer: answer }
}

/** The one-to-one chat of the review world, and its latest message. */
async function dmThread(request: APIRequestContext, seeded: SeedBehaviorResult) {
  const friend = entity(seeded, 'friend')
  const [row] = await contactRows(request, friend.id)
  const latest = (await rowMessages(request, row)).at(-1)!
  return { friend, row, latest }
}

test.describe('Interactions bench nones @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('a proposal whose answers are all None shows each list’s closest decision and confirms its Nones in one act', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-048')
    const { friend, row, latest } = await dmThread(request, seeded)
    await seedJudgmentRun(request, {
      judgments: [
        {
          thread_interaction_id: entity(seeded, 'dm').id,
          cut_off: latest.sent_at,
          questions: [
            asked('ball_in_court:THEM', {
              i_owe_reply: 0.09,
              they_owe_reply: 0.06,
              nobody_owes: 0.85,
            }),
            asked('commitment_mine:THEM', { open: 0.04, none: 0.96 }),
            asked('commitment_theirs:THEM', { open: 0.02, none: 0.98 }),
            asked('big_moment', {
              participant_moment: 0.02,
              other_person_moment: 0.01,
              none: 0.97,
            }),
          ],
          ball_in_court: [
            {
              question_id: 'ball_in_court:THEM',
              contact_id: friend.id,
              outcome: 'nobody_owes',
              confidence: 0.85,
            },
          ],
          commitments: [
            {
              question_id: 'commitment_mine:THEM',
              contact_id: friend.id,
              direction: 'mine',
              outcome: 'none',
              confidence: 0.96,
            },
            {
              question_id: 'commitment_theirs:THEM',
              contact_id: friend.id,
              direction: 'theirs',
              outcome: 'none',
              confidence: 0.98,
            },
          ],
          big_moment: { question_id: 'big_moment', outcome: 'none', confidence: 0.97 },
        },
      ],
    })
    await page.goto(`/interactions?contact=${friend.id}`)
    const bench = benchRow(page, row.item.identity.id)

    // spec: IXN-044.collapsed-indicator, IXN-044.proposal-status
    await expect(bench.locator('[data-review-indicator]')).toHaveText('Nones to review', {
      timeout: SLOW_TIMEOUT_MS,
    })
    const proposal = proposalOf(await expandRow(bench), 'bench-production')
    await expect(proposal.locator('[data-proposal-status]')).toHaveText('Nones to review')

    // spec: IXN-061.none-line-closest, IXN-061.named-as-items
    const lines = {
      'I owe a reply': `None closest: ${friend.name} 9%`,
      'Owes me a reply': `None closest: ${friend.name} 6%`,
      Commitments: `None closest: Me → ${friend.name} 4%`,
      'Big moment': 'None closest: Big moment 3%',
    }
    for (const [title, line] of Object.entries(lines)) {
      await expect(proposalList(proposal, title).locator('[data-none]')).toHaveText(line)
    }

    // spec: IXN-062.offered-once, IXN-062.no-list-control
    const confirm = proposal.getByRole('button', { name: 'Confirm Nones' })
    await expect(confirm).toHaveCount(1)
    for (const title of Object.keys(lines)) {
      await expect(
        proposalList(proposal, title).getByRole('button', { name: /^(?!Add)/ })
      ).toHaveCount(0)
    }

    // spec: IXN-062.confirmed-without-reload, IXN-062.adds-stay-usable
    await confirm.click()
    await expect(proposal.locator('[data-proposal-status]')).toHaveText('All reviewed', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(bench.locator('[data-review-indicator]')).toHaveText('Reviewed')
    await expect(confirm).toHaveCount(0)
    for (const title of Object.keys(lines)) {
      const list = proposalList(proposal, title)
      await expect(list.locator('[data-none] [data-nones-mark]')).toHaveText('Confirmed')
      await expect(list.getByRole('button', { name: /^Add/ })).toBeEnabled()
    }
  })
})
