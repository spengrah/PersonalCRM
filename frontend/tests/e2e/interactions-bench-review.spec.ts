import { test, expect, type APIRequestContext, type Locator, type Page } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from './helpers/timeouts'
import { createTestAPI, TestAPI, type SeedBehaviorResult } from './helpers/test-api'
import {
  benchRow,
  contactRows,
  entity,
  expandRow,
  fetchObligations,
  listItem,
  localDateFromNow,
  proposalList,
  rowMessages,
  seedJudgmentRun,
  type SeedOutcome,
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

/** The proposal of a method in an expanded row. */
function proposalOf(region: Locator, method: string): Locator {
  return region.locator('[data-proposal]').filter({ hasText: method })
}

/** Records every review the page posts. */
function recordReviews(page: Page): Record<string, unknown>[] {
  const bodies: Record<string, unknown>[] = []
  page.on('request', request => {
    if (request.method() === 'POST' && request.url().includes('/judgment-outcomes/')) {
      bodies.push(request.postDataJSON())
    }
  })
  return bodies
}

/** The group world's shared row: the user's message, held once per member, and its messages. */
async function groupThread(request: APIRequestContext, seeded: SeedBehaviorResult) {
  const sam = entity(seeded, 'sam')
  const rows = await contactRows(request, sam.id)
  const shared = rows.find(row => row.participants.length === 3)!
  expect(shared, 'the user’s message is one row of the three members').toBeTruthy()
  const messages = await rowMessages(request, shared)
  return { shared, messages, latest: messages.at(-1)! }
}

function none(question: 'mine' | 'theirs', contactId: string, n: number): SeedOutcome {
  return {
    question_id: `commitment_${question}:P${n}`,
    contact_id: contactId,
    direction: question,
    outcome: 'none',
    confidence: 0.9,
  }
}

test.describe('Interactions bench review @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('a proposal lists the judgment’s answers, each item with its state', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-046')
    const [sam, kim, lee, friend, host] = ['sam', 'kim', 'lee', 'friend', 'host'].map(h =>
      entity(seeded, h)
    )
    const { shared, latest } = await groupThread(request, seeded)
    const due = daysFromNow(10)
    await seedJudgmentRun(request, {
      judgments: [
        {
          thread_interaction_id: entity(seeded, 'group').id,
          cut_off: latest.sent_at,
          questions: [
            {
              id: 'commitment_mine:PERSON_4/source_message',
              probabilities: { [latest.content_ref]: 0.9 },
              original_answer: latest.content_ref,
            },
          ],
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
              confidence: 0.3,
            },
            {
              question_id: 'ball_in_court:PERSON_4',
              contact_id: lee.id,
              outcome: 'i_owe_reply',
              confidence: 0.95,
            },
          ],
          commitments: [
            {
              question_id: 'commitment_mine:PERSON_4',
              contact_id: lee.id,
              direction: 'mine',
              outcome: 'open',
              confidence: 0.9,
              due_date: due,
            },
            {
              question_id: 'commitment_theirs:PERSON_2',
              contact_id: sam.id,
              direction: 'theirs',
              outcome: 'open',
              confidence: 0.04,
            },
            none('mine', sam.id, 2),
            none('mine', kim.id, 3),
            none('theirs', kim.id, 3),
            none('theirs', lee.id, 4),
          ],
          big_moment: { question_id: 'big_moment', outcome: 'participant_moment', confidence: 0.8 },
        },
      ],
    })
    await seedJudgmentRun(request, {
      method_key: 'bench-other',
      judgments: [
        {
          thread_interaction_id: entity(seeded, 'group').id,
          cut_off: latest.sent_at,
          ball_in_court: [
            {
              question_id: 'ball_in_court:PERSON_3',
              contact_id: kim.id,
              outcome: 'i_owe_reply',
              confidence: 0.6,
            },
          ],
        },
      ],
    })
    await page.goto(`/interactions?contact=${sam.id}`)
    const region = await expandRow(benchRow(page, shared.item.identity.id))
    const proposal = proposalOf(region, 'bench-production')

    // spec: IXN-046.lists-answers, IXN-046.commitment-date-and-stand-in, IXN-046.confidence-shown, IXN-046.states-told-apart, IXN-046.no-item-hidden
    const iOwe = proposalList(proposal, 'I owe a reply')
    await expect(iOwe.locator('[data-item-text]')).toHaveText([sam.name])
    await expect(listItem(iOwe, sam.name)).toHaveAttribute('data-state', 'live')
    await expect(listItem(iOwe, sam.name).locator('[data-state-label]')).toHaveText('Live')
    await expect(listItem(iOwe, sam.name).locator('[data-confidence]')).toHaveText('90%')
    const owesMe = proposalList(proposal, 'Owes me a reply')
    await expect(owesMe.locator('[data-item-text]')).toHaveText([kim.name])
    await expect(listItem(owesMe, kim.name)).toHaveAttribute('data-state', 'suggested')
    await expect(listItem(owesMe, kim.name).locator('[data-state-label]')).toHaveText('Suggested')
    const commitments = proposalList(proposal, 'Commitments')
    const mine = listItem(commitments, `Me → ${lee.name}`)
    await expect(mine.locator('[data-item-text]')).toHaveText(
      `Me → ${lee.name} · due ${formatDue(due)}`
    )
    await expect(mine).toHaveAttribute('data-state', 'live')
    const theirs = listItem(commitments, `${sam.name} → Me`)
    await expect(theirs.locator('[data-item-text]')).toHaveText(`${sam.name} → Me · no stated date`)
    await expect(theirs).toHaveAttribute('data-state', 'suggested')
    await expect(theirs.locator('[data-confidence]')).toHaveText('4%')

    // spec: IXN-046.source-quote-leads
    const quote = mine.locator('[data-source-message]')
    await expect(quote).toContainText('Me')
    await expect(quote).toContainText(`“${latest.body}”`)
    await quote.getByRole('button', { name: 'Show in thread' }).click()
    await expect(
      region.locator('[data-highlighted]').locator(`[data-content-ref="${latest.content_ref}"]`)
    ).toBeVisible()
    await expect(theirs.locator('[data-source-message]')).toHaveCount(0)

    // spec: IXN-046.flag-belongs-to-thread
    const flag = proposalList(proposal, 'Big moment').locator('[data-item]')
    await expect(flag.locator('[data-item-text]')).toHaveText('Big moment')
    await expect(flag).toHaveAttribute('data-state', 'live')

    // spec: IXN-046.states-told-apart
    const other = proposalOf(region, 'bench-other')
    await other.getByRole('button', { name: 'Show proposal' }).click()
    const notActing = listItem(proposalList(other, 'I owe a reply'), kim.name)
    await expect(notActing).toHaveAttribute('data-state', 'not_acting')
    await expect(notActing.locator('[data-state-label]')).toHaveText('Not acting')

    // spec: IXN-046.questions-not-shown
    const shown = await region.innerText()
    for (const question of [
      'ball_in_court',
      'commitment_mine',
      'commitment_theirs',
      'big_moment',
      'PERSON_',
    ]) {
      expect(shown).not.toContain(question)
    }

    // spec: IXN-046.reply-list-limits
    const [dmRow] = await contactRows(request, friend.id)
    const dmLatest = (await rowMessages(request, dmRow)).at(-1)!
    await seedJudgmentRun(request, {
      judgments: [
        {
          thread_interaction_id: entity(seeded, 'dm').id,
          cut_off: dmLatest.sent_at,
          ball_in_court: [
            {
              question_id: 'ball_in_court:THEM',
              contact_id: friend.id,
              outcome: 'they_owe_reply',
              confidence: 0.9,
            },
          ],
        },
      ],
    })
    const [meetingRow] = await contactRows(request, host.id)
    await seedJudgmentRun(request, {
      judgments: [
        {
          thread_interaction_id: meetingRow.item.identity.id,
          context_kind: 'meeting',
          cut_off: new Date(Date.now() - 60_000).toISOString(),
          commitments: [
            {
              question_id: 'commitment_mine:PERSON_2',
              contact_id: host.id,
              direction: 'mine',
              outcome: 'open',
              confidence: 0.9,
            },
          ],
        },
      ],
    })
    await page.goto(`/interactions?contact=${friend.id}`)
    const dm = proposalOf(
      await expandRow(benchRow(page, dmRow.item.identity.id)),
      'bench-production'
    )
    await expect(proposalList(dm, 'Owes me a reply').locator('[data-item]')).toHaveCount(1)
    await expect(proposalList(dm, 'I owe a reply').locator('[data-item]')).toHaveCount(0)
    await page.goto(`/interactions?contact=${host.id}`)
    const meeting = proposalOf(
      await expandRow(benchRow(page, meetingRow.item.identity.id)),
      'bench-production'
    )
    await expect(proposalList(meeting, 'Commitments').locator('[data-item-text]')).toHaveText([
      `Me → ${host.name} · no stated date`,
    ])
    await expect(proposalList(meeting, 'I owe a reply')).toHaveCount(0)
    await expect(proposalList(meeting, 'Owes me a reply')).toHaveCount(0)
  })

  test('the user corrects a group thread’s proposal inline, and each review stays after a reload', async ({
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
          ball_in_court: [
            {
              question_id: 'ball_in_court:PERSON_2',
              contact_id: sam.id,
              outcome: 'i_owe_reply',
              confidence: 0.3,
            },
            {
              question_id: 'ball_in_court:PERSON_3',
              contact_id: kim.id,
              outcome: 'nobody_owes',
              confidence: 0.9,
            },
            {
              question_id: 'ball_in_court:PERSON_4',
              contact_id: lee.id,
              outcome: 'nobody_owes',
              confidence: 0.9,
            },
          ],
          commitments: [
            {
              question_id: 'commitment_theirs:PERSON_3',
              contact_id: kim.id,
              direction: 'theirs',
              outcome: 'open',
              confidence: 0.9,
            },
            none('mine', sam.id, 2),
            none('mine', kim.id, 3),
            none('mine', lee.id, 4),
            none('theirs', sam.id, 2),
            none('theirs', lee.id, 4),
          ],
          big_moment: { question_id: 'big_moment', outcome: 'none', confidence: 0.9 },
        },
      ],
    })
    const reviews = recordReviews(page)
    await page.goto(`/interactions?contact=${sam.id}`)
    const row = benchRow(page, shared.item.identity.id)
    await expect(row.locator('[data-review-indicator]')).toHaveText('2 to review', {
      timeout: SLOW_TIMEOUT_MS,
    })
    const proposal = proposalOf(await expandRow(row), 'bench-production')
    const iOwe = proposalList(proposal, 'I owe a reply')
    const owesMe = proposalList(proposal, 'Owes me a reply')
    const commitments = proposalList(proposal, 'Commitments')
    const flag = proposalList(proposal, 'Big moment')

    // spec: IXN-047.confirm-by-judged, IXN-047.shows-without-reload-or-error
    await expect(listItem(iOwe, sam.name)).toHaveAttribute('data-state', 'suggested')
    await iOwe.getByRole('button', { name: `Confirm ${sam.name}` }).click()
    await expect(listItem(iOwe, sam.name)).toHaveAttribute('data-mark', 'confirmed', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(listItem(iOwe, sam.name).locator('[data-state-label]')).toHaveText('Live')

    // spec: IXN-047.remove-by-no-effect
    await expect(listItem(commitments, `${kim.name} → Me`)).toHaveAttribute('data-state', 'live')
    await commitments.getByRole('button', { name: `Remove ${kim.name} → Me` }).click()
    await expect(listItem(commitments, `${kim.name} → Me`)).toHaveAttribute(
      'data-mark',
      'removed',
      {
        timeout: SLOW_TIMEOUT_MS,
      }
    )

    // spec: IXN-047.add-by-outcome, IXN-047.additions-participants-unreviewed
    await owesMe.getByRole('button', { name: 'Add', exact: true }).click()
    const owesPicker = owesMe.getByRole('group', { name: 'Add to Owes me a reply' })
    await expect(owesPicker.getByRole('button', { name: /^(?!Close)/ })).toHaveCount(2)
    expect(
      new Set(await owesPicker.getByRole('button', { name: /^(?!Close)/ }).allInnerTexts())
    ).toEqual(new Set([kim.name, lee.name]))
    await owesPicker.getByRole('button', { name: lee.name }).click()
    await expect(listItem(owesMe, lee.name)).toHaveAttribute('data-mark', 'added', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(listItem(owesMe, lee.name).locator('[data-state-label]')).toHaveText('Live')

    // spec: IXN-047.add-by-outcome, IXN-047.additions-participants-unreviewed, IXN-047.added-commitment-fallback
    await commitments.getByRole('button', { name: 'Add commitment' }).click()
    const commitPicker = commitments.getByRole('group', { name: 'Add to Commitments' })
    await expect(commitPicker.getByRole('button', { name: /→/ })).toHaveCount(5)
    expect(new Set(await commitPicker.getByRole('button', { name: /→/ }).allInnerTexts())).toEqual(
      new Set([
        `Me → ${sam.name}`,
        `${sam.name} → Me`,
        `Me → ${kim.name}`,
        `Me → ${lee.name}`,
        `${lee.name} → Me`,
      ])
    )
    const fallbackBefore = localDateFromNow(7)
    await commitPicker.getByRole('button', { name: `${lee.name} → Me` }).click()
    const added = listItem(commitments, `${lee.name} → Me`)
    await expect(added).toHaveAttribute('data-mark', 'added', { timeout: SLOW_TIMEOUT_MS })
    await expect(added.locator('[data-item-text]')).toHaveText(`${lee.name} → Me · no stated date`)
    const nudge = (await fetchObligations(request, entity(seeded, 'group').id)).find(
      o => o.open && o.debtor === 'contact' && o.basis === 'commitment' && o.contact_id === lee.id
    )
    expect(nudge?.kind).toBe('reach_out')
    expect(
      [fallbackBefore, localDateFromNow(7)],
      'a contact without a cadence is nudged seven days on'
    ).toContain(nudge?.due_date)

    // spec: IXN-047.add-by-outcome, IXN-047.added-flag-own-moment
    await flag.getByRole('button', { name: 'Add', exact: true }).click()
    await expect(flag.locator('[data-item]')).toHaveAttribute('data-mark', 'added', {
      timeout: SLOW_TIMEOUT_MS,
    })
    expect(reviews.at(-1)).toEqual({ right_outcome: 'participant_moment' })

    // spec: IXN-047.inline-no-verdict
    expect(reviews).toEqual([
      { right_outcome: 'i_owe_reply' },
      { right_outcome: 'none' },
      { right_outcome: 'they_owe_reply' },
      { right_outcome: 'open' },
      { right_outcome: 'participant_moment' },
    ])
    await expect(page.getByRole('dialog')).toHaveCount(0)

    // spec: IXN-048.confirmed-in-place, IXN-048.removed-stays-struck, IXN-048.added-shown, IXN-048.no-control-after-review, IXN-048.counts-exclude-reviewed
    await page.reload()
    await expect(row.locator('[data-review-indicator]')).toHaveText('Reviewed', {
      timeout: SLOW_TIMEOUT_MS,
    })
    const reloaded = proposalOf(await expandRow(row), 'bench-production')
    await expect(reloaded.locator('[data-proposal-status]')).toHaveText('All reviewed')
    const confirmed = listItem(proposalList(reloaded, 'I owe a reply'), sam.name)
    await expect(confirmed.locator('[data-review-mark]')).toHaveText('Confirmed')
    await expect(confirmed.locator('[data-state-label]')).toHaveText('Live')
    const removed = listItem(proposalList(reloaded, 'Commitments'), `${kim.name} → Me`)
    await expect(removed.locator('[data-review-mark]')).toHaveText('Removed by you')
    await expect(removed.locator('[data-item-text]')).toHaveClass(/line-through/)
    await expect(removed.locator('[data-state-label]')).toHaveCount(0)
    await expect(
      listItem(proposalList(reloaded, 'Owes me a reply'), lee.name).locator('[data-review-mark]')
    ).toHaveText('Added by you')
    await expect(
      listItem(proposalList(reloaded, 'Commitments'), `${lee.name} → Me`).locator(
        '[data-review-mark]'
      )
    ).toHaveText('Added by you')
    await expect(proposalList(reloaded, 'Big moment').locator('[data-review-mark]')).toHaveText(
      'Added by you'
    )
    await expect(reloaded.locator('[data-item]').getByRole('button')).toHaveCount(0)
  })

  test('the user removes a reply item and the flag, moves a person between reply lists, and adds their own commitment', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-048')
    const [sam, kim, lee, waiting] = ['sam', 'kim', 'lee', 'waiting'].map(h => entity(seeded, h))
    const { shared, latest } = await groupThread(request, seeded)
    await seedJudgmentRun(request, {
      judgments: [
        {
          thread_interaction_id: entity(seeded, 'group').id,
          cut_off: latest.sent_at,
          ball_in_court: [
            {
              question_id: 'ball_in_court:PERSON_2',
              contact_id: sam.id,
              outcome: 'they_owe_reply',
              confidence: 0.9,
            },
            {
              question_id: 'ball_in_court:PERSON_3',
              contact_id: kim.id,
              outcome: 'i_owe_reply',
              confidence: 0.9,
            },
            {
              question_id: 'ball_in_court:PERSON_4',
              contact_id: lee.id,
              outcome: 'nobody_owes',
              confidence: 0.9,
            },
          ],
          commitments: [
            none('mine', sam.id, 2),
            none('mine', kim.id, 3),
            none('mine', lee.id, 4),
            none('theirs', sam.id, 2),
            none('theirs', kim.id, 3),
            none('theirs', lee.id, 4),
          ],
          big_moment: {
            question_id: 'big_moment',
            outcome: 'participant_moment',
            confidence: 0.9,
          },
        },
      ],
    })
    const reviews = recordReviews(page)
    await page.goto(`/interactions?contact=${sam.id}`)
    const row = benchRow(page, shared.item.identity.id)
    const proposal = proposalOf(await expandRow(row), 'bench-production')
    const iOwe = proposalList(proposal, 'I owe a reply')
    const owesMe = proposalList(proposal, 'Owes me a reply')
    const commitments = proposalList(proposal, 'Commitments')
    const flag = proposalList(proposal, 'Big moment')

    // spec: IXN-047.remove-by-no-effect
    await expect(listItem(iOwe, kim.name)).toHaveAttribute('data-state', 'live', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await iOwe.getByRole('button', { name: `Remove ${kim.name}` }).click()
    await expect(listItem(iOwe, kim.name)).toHaveAttribute('data-mark', 'removed', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(flag.locator('[data-item]')).toHaveAttribute('data-state', 'live')
    await flag.getByRole('button', { name: 'Remove big moment' }).click()
    await expect(flag.locator('[data-item]')).toHaveAttribute('data-mark', 'removed', {
      timeout: SLOW_TIMEOUT_MS,
    })

    // spec: IXN-047.add-by-outcome
    await expect(listItem(owesMe, sam.name)).toHaveAttribute('data-state', 'live')
    await iOwe.getByRole('button', { name: 'Add', exact: true }).click()
    await iOwe
      .getByRole('group', { name: 'Add to I owe a reply' })
      .getByRole('button', { name: sam.name })
      .click()
    await expect(listItem(iOwe, sam.name)).toHaveAttribute('data-mark', 'added', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(listItem(owesMe, sam.name)).toHaveCount(0)

    // spec: IXN-047.added-commitment-fallback
    const sevenBefore = localDateFromNow(7)
    await commitments.getByRole('button', { name: 'Add commitment' }).click()
    await commitments
      .getByRole('group', { name: 'Add to Commitments' })
      .getByRole('button', { name: `Me → ${lee.name}` })
      .click()
    const mine = listItem(commitments, `Me → ${lee.name}`)
    await expect(mine).toHaveAttribute('data-mark', 'added', { timeout: SLOW_TIMEOUT_MS })
    await expect(mine.locator('[data-item-text]')).toHaveText(`Me → ${lee.name} · no stated date`)
    const owed = (await fetchObligations(request, entity(seeded, 'group').id)).find(
      o => o.open && o.debtor === 'user' && o.basis === 'commitment' && o.contact_id === lee.id
    )
    expect(owed?.kind).toBe('reminder')
    expect(
      [sevenBefore, localDateFromNow(7)],
      'the user’s own commitment to a contact without a cadence is due seven days on'
    ).toContain(owed?.due_date)

    // spec: IXN-047.inline-no-verdict
    expect(reviews).toEqual([
      { right_outcome: 'nobody_owes' },
      { right_outcome: 'none' },
      { right_outcome: 'i_owe_reply' },
      { right_outcome: 'open' },
    ])

    // spec: IXN-048.removed-stays-struck, IXN-048.added-shown
    await page.reload()
    const reloaded = proposalOf(await expandRow(row), 'bench-production')
    const removedReply = listItem(proposalList(reloaded, 'I owe a reply'), kim.name)
    await expect(removedReply.locator('[data-review-mark]')).toHaveText('Removed by you', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(removedReply.locator('[data-item-text]')).toHaveClass(/line-through/)
    const removedFlag = proposalList(reloaded, 'Big moment').locator('[data-item]')
    await expect(removedFlag.locator('[data-review-mark]')).toHaveText('Removed by you')
    await expect(removedFlag.locator('[data-item-text]')).toHaveClass(/line-through/)
    await expect(
      listItem(proposalList(reloaded, 'I owe a reply'), sam.name).locator('[data-review-mark]')
    ).toHaveText('Added by you')
    await expect(listItem(proposalList(reloaded, 'Owes me a reply'), sam.name)).toHaveCount(0)
    await expect(
      listItem(proposalList(reloaded, 'Commitments'), `Me → ${lee.name}`).locator(
        '[data-review-mark]'
      )
    ).toHaveText('Added by you')

    // spec: IXN-047.added-commitment-fallback
    const [thanksRow] = await contactRows(request, waiting.id)
    const thanksLatest = (await rowMessages(request, thanksRow)).at(-1)!
    await seedJudgmentRun(request, {
      judgments: [
        {
          thread_interaction_id: entity(seeded, 'thanks').id,
          cut_off: thanksLatest.sent_at,
          commitments: [none('mine', waiting.id, 1)],
        },
      ],
    })
    await page.goto(`/interactions?contact=${waiting.id}`)
    const thanks = proposalList(
      proposalOf(await expandRow(benchRow(page, thanksRow.item.identity.id)), 'bench-production'),
      'Commitments'
    )
    const threeBefore = localDateFromNow(3)
    await thanks.getByRole('button', { name: 'Add commitment' }).click()
    await thanks
      .getByRole('group', { name: 'Add to Commitments' })
      .getByRole('button', { name: `Me → ${waiting.name}` })
      .click()
    await expect(listItem(thanks, `Me → ${waiting.name}`)).toHaveAttribute('data-mark', 'added', {
      timeout: SLOW_TIMEOUT_MS,
    })
    const weekly = (await fetchObligations(request, entity(seeded, 'thanks').id)).find(
      o => o.open && o.debtor === 'user' && o.basis === 'commitment'
    )
    expect(
      [threeBefore, localDateFromNow(3)],
      'the user’s own commitment to a weekly contact is due the cadence’s watchdog days on'
    ).toContain(weekly?.due_date)
  })

  test('a suggested removal of a live wait is accepted, or kept', async ({ page, request }) => {
    const judgeWait = async (seeded: SeedBehaviorResult) => {
      const waiting = entity(seeded, 'waiting')
      const [row] = await contactRows(request, waiting.id)
      const latest = (await rowMessages(request, row)).at(-1)!
      await seedJudgmentRun(request, {
        judgments: [
          {
            thread_interaction_id: entity(seeded, 'thanks').id,
            cut_off: latest.sent_at,
            ball_in_court: [
              {
                question_id: 'ball_in_court:THEM',
                contact_id: waiting.id,
                outcome: 'nobody_owes',
                confidence: 0.85,
              },
            ],
          },
        ],
      })
      await page.goto(`/interactions?contact=${waiting.id}`)
      const proposal = proposalOf(
        await expandRow(benchRow(page, row.item.identity.id)),
        'bench-production'
      )
      return { item: listItem(proposalList(proposal, 'Owes me a reply'), waiting.name), row }
    }
    const reviews = recordReviews(page)

    // spec: IXN-046.confidence-shown, IXN-046.states-told-apart, IXN-047.confirm-by-judged, IXN-048.removed-stays-struck
    const accepted = await judgeWait(await testApi.seedBehavior('IXN-047'))
    await expect(accepted.item).toHaveAttribute('data-state', 'live_with_change')
    await expect(accepted.item.locator('[data-state-label]')).toHaveText('Live')
    await expect(accepted.item.locator('[data-confidence]')).toHaveCount(0)
    const strip = accepted.item.locator('[data-change="remove"]')
    await expect(strip).toContainText('Suggested: remove')
    await expect(strip).toContainText('85%')
    await strip.getByRole('button', { name: 'Accept' }).click()
    await expect(accepted.item).toHaveAttribute('data-mark', 'removed', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await page.reload()
    const acceptedAgain = proposalList(
      proposalOf(
        await expandRow(benchRow(page, accepted.row.item.identity.id)),
        'bench-production'
      ),
      'Owes me a reply'
    ).locator('[data-item]')
    await expect(acceptedAgain.locator('[data-review-mark]')).toHaveText('Removed by you')

    // spec: IXN-047.keep-live-against-removal, IXN-048.confirmed-in-place
    const kept = await judgeWait(await testApi.seedBehavior('IXN-047'))
    await kept.item.locator('[data-change="remove"]').getByRole('button', { name: 'Keep' }).click()
    await expect(kept.item).toHaveAttribute('data-mark', 'confirmed', { timeout: SLOW_TIMEOUT_MS })
    await page.reload()
    const keptAgain = proposalList(
      proposalOf(await expandRow(benchRow(page, kept.row.item.identity.id)), 'bench-production'),
      'Owes me a reply'
    ).locator('[data-item]')
    await expect(keptAgain.locator('[data-review-mark]')).toHaveText('Confirmed')
    await expect(keptAgain.locator('[data-state-label]')).toHaveText('Live')
    expect(reviews).toEqual([{ right_outcome: 'nobody_owes' }, { right_outcome: 'they_owe_reply' }])
  })

  test('Keep original keeps a commitment at its current date against a suggested later date', async ({
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
    const reviews = recordReviews(page)
    await page.goto(`/interactions?run=${moved.run_id}&contact=${promiser.id}`)
    const proposal = proposalOf(
      await expandRow(benchRow(page, row.item.identity.id)),
      'bench-production'
    )
    const item = listItem(proposalList(proposal, 'Commitments'), `${promiser.name} → Me`)

    // spec: IXN-047.keep-date-against-later, IXN-048.confirmed-in-place
    await expect(item).toHaveAttribute('data-state', 'live_with_change')
    await expect(item.locator('[data-item-text]')).toHaveText(
      `${promiser.name} → Me · due ${formatDue(current)}`
    )
    const strip = item.locator('[data-change="later_date"]')
    await expect(strip).toContainText(`Suggested: due ${formatDue(later)}`)
    await strip.getByRole('button', { name: 'Keep original' }).click()
    await expect(item).toHaveAttribute('data-mark', 'confirmed', { timeout: SLOW_TIMEOUT_MS })
    expect(reviews).toEqual([{ right_outcome: 'open', keep_current_date: true }])
    await page.reload()
    const again = listItem(
      proposalList(
        proposalOf(await expandRow(benchRow(page, row.item.identity.id)), 'bench-production'),
        'Commitments'
      ),
      `${promiser.name} → Me`
    )
    await expect(again.locator('[data-item-text]')).toHaveText(
      `${promiser.name} → Me · due ${formatDue(current)}`
    )
    await expect(again.locator('[data-review-mark]')).toHaveText('Confirmed')
    await expect(again.locator('[data-state-label]')).toHaveText('Live')
    const obligation = (await fetchObligations(request, entity(seeded, 'promise').id)).find(
      o => o.open
    )
    expect(obligation?.due_date, 'the obligation keeps its current date').toBe(current)
  })

  test('a failed correction leaves the item’s controls in place with an error', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-047')
    const friend = entity(seeded, 'friend')
    const [row] = await contactRows(request, friend.id)
    const latest = (await rowMessages(request, row)).at(-1)!
    await seedJudgmentRun(request, {
      judgments: [
        {
          thread_interaction_id: entity(seeded, 'dm').id,
          cut_off: latest.sent_at,
          ball_in_court: [
            {
              question_id: 'ball_in_court:THEM',
              contact_id: friend.id,
              outcome: 'they_owe_reply',
              confidence: 0.9,
            },
          ],
        },
      ],
    })
    await page.route('**/api/v1/judgment-outcomes/*/review', route =>
      route.fulfill({ status: 500, contentType: 'application/json', body: '{"success":false}' })
    )
    await page.goto(`/interactions?contact=${friend.id}`)
    const proposal = proposalOf(
      await expandRow(benchRow(page, row.item.identity.id)),
      'bench-production'
    )
    const item = listItem(proposalList(proposal, 'Owes me a reply'), friend.name)

    // spec: IXN-047.shows-without-reload-or-error
    await item.getByRole('button', { name: `Confirm ${friend.name}` }).click()
    await expect(item).toContainText("Couldn't save. Try again.", { timeout: SLOW_TIMEOUT_MS })
    await expect(item.getByRole('button', { name: `Confirm ${friend.name}` })).toBeVisible()
    await expect(item).not.toHaveAttribute('data-mark')
  })

  test('an accepted big-moment flag shows on its thread beneath the content it was judged on', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-049')
    const texts: string[] = []
    for (const [handle, thread, moment] of [
      ['friend', 'own-moment', 'participant_moment'],
      ['pal', 'other-moment', 'other_person_moment'],
    ] as const) {
      const contact = entity(seeded, handle)
      const [row] = await contactRows(request, contact.id)
      const latest = (await rowMessages(request, row)).at(-1)!
      await seedJudgmentRun(request, {
        judgments: [
          {
            thread_interaction_id: entity(seeded, thread).id,
            cut_off: latest.sent_at,
            big_moment: { question_id: 'big_moment', outcome: moment, confidence: 0.9 },
          },
        ],
      })
      await page.goto(`/interactions?contact=${contact.id}`)
      const region = await expandRow(benchRow(page, row.item.identity.id))

      // spec: IXN-049.flag-beneath-content, IXN-046.flag-belongs-to-thread
      const frame = region.locator('[data-judged-frame]')
      await expect(frame.locator(`[data-content-ref="${latest.content_ref}"]`)).toBeVisible({
        timeout: SLOW_TIMEOUT_MS,
      })
      const flag = proposalList(proposalOf(frame, 'bench-production'), 'Big moment').locator(
        '[data-item]'
      )
      await expect(flag).toHaveAttribute('data-state', 'live')
      const placed = await frame.evaluate(node => {
        const content = node.querySelector('[data-judged-content]')!
        const proposal = node.querySelector('[data-proposal]')!
        return content.compareDocumentPosition(proposal) & Node.DOCUMENT_POSITION_FOLLOWING
      })
      expect(placed).toBeTruthy()

      // spec: IXN-049.both-moments-same, IXN-049.no-summary
      texts.push(await flag.locator('[data-item-text]').innerText())
    }
    expect(texts).toEqual(['Big moment', 'Big moment'])
  })
})
