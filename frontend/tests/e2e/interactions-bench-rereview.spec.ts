import { test, expect } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from './helpers/timeouts'
import { createTestAPI, TestAPI } from './helpers/test-api'
import {
  benchRow,
  contactRows,
  entity,
  expandRow,
  listItem,
  proposalList,
  rowMessages,
  seedJudgmentRun,
} from './helpers/bench'

const dueFormat = new Intl.DateTimeFormat('en-US', {
  weekday: 'short',
  month: 'short',
  day: 'numeric',
  timeZone: 'UTC',
})
const formatDue = (date: string) => dueFormat.format(new Date(`${date}T00:00:00Z`))
const daysFromNow = (days: number) =>
  new Date(Date.now() + days * 86_400_000).toISOString().slice(0, 10)

test.describe('Interactions bench re-review @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('Change on a reviewed item restores its controls, and the item shows its live state beside the new mark', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-048')
    const [sam, kim] = ['sam', 'kim'].map(h => entity(seeded, h))
    const rows = await contactRows(request, sam.id)
    const shared = rows.find(row => row.participants.length === 3)!
    const latest = (await rowMessages(request, shared)).at(-1)!
    await seedJudgmentRun(request, {
      judgments: [
        {
          thread_interaction_id: entity(seeded, 'group').id,
          cut_off: latest.sent_at,
          ball_in_court: [
            {
              question_id: 'ball_in_court:PERSON_2',
              contact_id: sam.id,
              outcome: 'i_owe_reply',
              confidence: 0.9,
            },
            {
              question_id: 'ball_in_court:PERSON_3',
              contact_id: kim.id,
              outcome: 'they_owe_reply',
              confidence: 0.9,
            },
          ],
        },
      ],
    })
    await page.goto(`/interactions?contact=${sam.id}`)
    const proposal = (await expandRow(benchRow(page, shared.item.identity.id)))
      .locator('[data-proposal]')
      .filter({ hasText: 'bench-production' })
    const confirmed = listItem(proposalList(proposal, 'I owe a reply'), sam.name)
    const removed = listItem(proposalList(proposal, 'Owes me a reply'), kim.name)
    await confirmed.getByRole('button', { name: `Confirm ${sam.name}` }).click()
    await expect(confirmed.locator('[data-review-mark]')).toHaveText('Confirmed', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await removed.getByRole('button', { name: `Remove ${kim.name}` }).click()
    await expect(removed.locator('[data-review-mark]')).toHaveText('Removed by you', {
      timeout: SLOW_TIMEOUT_MS,
    })

    // spec: IXN-065.struck-only-taken-effect, IXN-046.states-told-apart, IXN-048.removed-stays-struck
    await expect(removed.locator('[data-item-text]')).toHaveClass(/line-through/)
    await expect(removed.locator('[data-state-label]')).toHaveCount(0)

    // spec: IXN-065.change-restores-controls
    await expect(confirmed.getByRole('button', { name: `Confirm ${sam.name}` })).toHaveCount(0)
    await confirmed.getByRole('button', { name: `Change the review of ${sam.name}` }).click()
    await expect(confirmed.getByRole('button', { name: `Confirm ${sam.name}` })).toBeVisible()
    await expect(confirmed.getByRole('button', { name: `Remove ${sam.name}` })).toBeVisible()
    await expect(confirmed.locator('[data-review-mark]')).toHaveCount(0)

    // spec: IXN-065.new-mark-shown, IXN-065.removed-while-live, IXN-065.struck-only-taken-effect, IXN-046.states-told-apart
    await confirmed.getByRole('button', { name: `Remove ${sam.name}` }).click()
    await expect(confirmed.locator('[data-review-mark]')).toHaveText('Removed by you', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(confirmed.locator('[data-state-label]')).toHaveText('Live')
    await expect(confirmed.locator('[data-item-text]')).not.toHaveClass(/line-through/)
    await expect(
      confirmed.getByRole('button', { name: `Change the review of ${sam.name}` })
    ).toBeVisible()

    // spec: IXN-065.change-restores-controls, IXN-065.new-mark-shown, IXN-065.confirmed-while-closed
    await removed.getByRole('button', { name: `Change the review of ${kim.name}` }).click()
    await removed.getByRole('button', { name: `Confirm ${kim.name}` }).click()
    await expect(removed.locator('[data-review-mark]')).toHaveText('Confirmed', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(removed.locator('[data-state-label]')).toHaveText('Not acting')
    await expect(removed.locator('[data-item-text]')).not.toHaveClass(/line-through/)
  })

  test('a commitment whose kept date its obligation no longer holds shows both dates', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-047')
    const promiser = entity(seeded, 'promiser')
    const [row] = await contactRows(request, promiser.id)
    const latest = (await rowMessages(request, row)).at(-1)!
    const current = daysFromNow(3)
    const later = daysFromNow(10)
    const commitment = (due: string) => ({
      thread_interaction_id: entity(seeded, 'promise').id,
      commitments: [
        {
          question_id: 'commitment_theirs:THEM',
          contact_id: promiser.id,
          direction: 'theirs' as const,
          outcome: 'open',
          confidence: 0.9,
          due_date: due,
        },
      ],
    })
    await seedJudgmentRun(request, {
      judgments: [{ ...commitment(current), cut_off: latest.sent_at }],
    })
    const moved = await seedJudgmentRun(request, {
      judgments: [
        {
          ...commitment(later),
          cut_off: new Date(new Date(latest.sent_at).getTime() + 30_000).toISOString(),
        },
      ],
    })
    await page.goto(`/interactions?run=${moved.run_id}&contact=${promiser.id}`)
    const item = listItem(
      proposalList(
        (await expandRow(benchRow(page, row.item.identity.id)))
          .locator('[data-proposal]')
          .filter({ hasText: 'bench-production' }),
        'Commitments'
      ),
      `${promiser.name} → Me`
    )
    await item.locator('[data-change="later_date"]').getByRole('button', { name: 'Accept' }).click()
    await expect(item.locator('[data-review-mark]')).toHaveText('Confirmed', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(item.locator('[data-item-text]')).toHaveText(
      `${promiser.name} → Me · due ${formatDue(later)}`
    )

    // spec: IXN-065.change-restores-controls, IXN-065.kept-date-beside-live
    await item.getByRole('button', { name: `Change the review of ${promiser.name} → Me` }).click()
    await item.getByRole('button', { name: 'Keep original' }).click()
    await expect(item.locator('[data-review-mark]')).toHaveText(
      `Kept ${formatDue(current)} by you`,
      {
        timeout: SLOW_TIMEOUT_MS,
      }
    )
    await expect(item.locator('[data-item-text]')).toHaveText(
      `${promiser.name} → Me · due ${formatDue(later)}`
    )
    await expect(item.locator('[data-state-label]')).toHaveText('Live')
  })
})
