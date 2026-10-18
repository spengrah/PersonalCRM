import { test, expect, type APIRequestContext, type Locator } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from './helpers/timeouts'
import {
  API_BASE_URL,
  API_HEADERS,
  createTestAPI,
  TestAPI,
  type SeedBehaviorResult,
} from './helpers/test-api'
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

/** An answered question's option probabilities, as a method stores them. */
function asked(id: string, probabilities: Record<string, number>) {
  const answer = Object.entries(probabilities).sort(([, a], [, b]) => b - a)[0][0]
  return { id, probabilities, original_answer: answer }
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

/** The one-to-one chat of the review world, and its latest message. */
async function dmThread(request: APIRequestContext, seeded: SeedBehaviorResult) {
  const friend = entity(seeded, 'friend')
  const [row] = await contactRows(request, friend.id)
  const latest = (await rowMessages(request, row)).at(-1)!
  return { friend, row, latest }
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

/** A one-to-one judgment whose every answer is None. */
function allNone(threadId: string, cutOff: string, contactId: string) {
  return {
    thread_interaction_id: threadId,
    cut_off: cutOff,
    ball_in_court: [
      {
        question_id: 'ball_in_court:THEM',
        contact_id: contactId,
        outcome: 'nobody_owes',
        confidence: 0.85,
      },
    ],
    commitments: [
      { ...noCommitment('mine', contactId, 1), question_id: 'commitment_mine:THEM' },
      { ...noCommitment('theirs', contactId, 1), question_id: 'commitment_theirs:THEM' },
    ],
    big_moment: { question_id: 'big_moment', outcome: 'none', confidence: 0.97 },
  }
}

const LIST_TITLES = ['I owe a reply', 'Owes me a reply', 'Commitments', 'Big moment']

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
          ...allNone(entity(seeded, 'dm').id, latest.sent_at, friend.id),
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
        },
      ],
    })
    await page.goto(`/interactions?contact=${friend.id}`)
    const bench = benchRow(page, row.item.identity.id)

    // spec: IXN-044.collapsed-indicator, IXN-063.proposal-status
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
    for (const title of LIST_TITLES) {
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
    for (const title of LIST_TITLES) {
      const list = proposalList(proposal, title)
      await expect(list.locator('[data-none] [data-nones-mark]')).toHaveText('Confirmed')
      await expect(list.getByRole('button', { name: /^Add/ })).toBeEnabled()
    }

    // spec: IXN-066.offers-confirmed-none, IXN-066.add-acts-as-before
    const owesMe = proposalList(proposal, 'Owes me a reply')
    await owesMe.getByRole('button', { name: 'Add', exact: true }).click()
    await owesMe
      .getByRole('group', { name: 'Add to Owes me a reply' })
      .getByRole('button', { name: friend.name })
      .click()
    const added = listItem(owesMe, friend.name)
    await expect(added.locator('[data-review-mark]')).toHaveText('Added by you', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(added.locator('[data-state-label]')).toHaveText('Live')
  })

  test('a failed Confirm Nones leaves the proposal as it was, with an error', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-047')
    const { friend, row, latest } = await dmThread(request, seeded)
    await seedJudgmentRun(request, {
      judgments: [allNone(entity(seeded, 'dm').id, latest.sent_at, friend.id)],
    })
    await page.route('**/api/v1/judgments/*/confirm-nones', route =>
      route.fulfill({ status: 500, contentType: 'application/json', body: '{"success":false}' })
    )
    await page.goto(`/interactions?contact=${friend.id}`)
    const bench = benchRow(page, row.item.identity.id)
    const proposal = proposalOf(await expandRow(bench), 'bench-production')

    // spec: IXN-061.no-closest-plain
    for (const title of LIST_TITLES) {
      await expect(proposalList(proposal, title).locator('[data-none]')).toHaveText('None')
    }

    // spec: IXN-062.failed-leaves-as-was
    await proposal.getByRole('button', { name: 'Confirm Nones' }).click()
    await expect(proposal).toContainText("Couldn't save. Try again.", { timeout: SLOW_TIMEOUT_MS })
    const unchanged = async (shown: Locator) => {
      await expect(shown.locator('[data-proposal-status]')).toHaveText('Nones to review')
      await expect(shown.getByRole('button', { name: 'Confirm Nones' })).toBeEnabled()
      await expect(shown.locator('[data-nones-mark]')).toHaveCount(0)
    }
    await unchanged(proposal)
    await page.reload()
    await expect(bench.locator('[data-review-indicator]')).toHaveText('Nones to review', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await unchanged(proposalOf(await expandRow(bench), 'bench-production'))
  })

  test('a 30-person meeting with nothing proposed shows one closest decision per list', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-046')
    const host = entity(seeded, 'host')
    const [meetingRow] = await contactRows(request, host.id)
    const others = await Promise.all(
      Array.from({ length: 29 }, async (_, i) => {
        const name = `${testApi.prefix} Attendee ${String(i + 1).padStart(2, '0')}`
        const response = await request.post(`${API_BASE_URL}/api/v1/contacts`, {
          headers: API_HEADERS,
          data: { full_name: name },
        })
        expect(response.status(), await response.text()).toBe(201)
        return { id: (await response.json()).data.id as string, name }
      })
    )
    const people = [host, ...others]
    const closest = others[6]
    await seedJudgmentRun(request, {
      judgments: [
        {
          thread_interaction_id: meetingRow.item.identity.id,
          context_kind: 'meeting',
          cut_off: new Date(Date.now() - 60_000).toISOString(),
          questions: [
            ...people.flatMap((person, i) => [
              asked(`commitment_mine:P${i + 1}`, { open: 0.05, none: 0.95 }),
              asked(`commitment_theirs:P${i + 1}`, {
                open: person === closest ? 0.38 : 0.1,
                none: person === closest ? 0.62 : 0.9,
              }),
            ]),
            asked('big_moment', {
              participant_moment: 0.06,
              other_person_moment: 0.05,
              none: 0.89,
            }),
          ],
          commitments: people.flatMap((person, i) => [
            noCommitment('mine', person.id, i + 1),
            noCommitment('theirs', person.id, i + 1),
          ]),
          big_moment: { question_id: 'big_moment', outcome: 'none', confidence: 0.89 },
        },
      ],
    })
    await page.goto(`/interactions?contact=${host.id}`)
    const bench = benchRow(page, meetingRow.item.identity.id)
    await expect(bench.locator('[data-review-indicator]')).toHaveText('Nones to review', {
      timeout: SLOW_TIMEOUT_MS,
    })
    const proposal = proposalOf(await expandRow(bench), 'bench-production')

    // spec: IXN-061.one-line-per-list, IXN-061.named-as-items
    await expect(proposalList(proposal, 'I owe a reply')).toHaveCount(0)
    await expect(proposalList(proposal, 'Owes me a reply')).toHaveCount(0)
    const commitments = proposalList(proposal, 'Commitments')
    await expect(commitments.locator('[data-none]')).toHaveText(
      `None closest: ${closest.name} → Me 38%`
    )
    await expect(commitments.locator('[data-item]')).toHaveCount(0)
    await expect(proposalList(proposal, 'Big moment').locator('[data-none]')).toHaveText(
      'None closest: Big moment 11%'
    )
    await expect(proposal.getByRole('button', { name: 'Confirm Nones' })).toHaveCount(1)
  })

  test('a proposal with items shows its closest None on a list’s heading line, and closing its row confirms its Nones', async ({
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
          questions: [
            asked('commitment_mine:P2', { open: 0.31, none: 0.69 }),
            asked('commitment_mine:P3', { open: 0.05, none: 0.95 }),
            asked('commitment_mine:P4', { open: 0.02, none: 0.98 }),
            asked('commitment_theirs:P2', { open: 0.1, none: 0.9 }),
            asked('commitment_theirs:P3', { open: 0.64, none: 0.36 }),
            asked('commitment_theirs:P4', { open: 0.03, none: 0.97 }),
            asked('big_moment', {
              participant_moment: 0.03,
              other_person_moment: 0.02,
              none: 0.95,
            }),
          ],
          ball_in_court: [
            {
              question_id: 'ball_in_court:P2',
              contact_id: sam.id,
              outcome: 'i_owe_reply',
              confidence: 0.88,
            },
            {
              question_id: 'ball_in_court:P3',
              contact_id: kim.id,
              outcome: 'nobody_owes',
              confidence: 0.9,
            },
            {
              question_id: 'ball_in_court:P4',
              contact_id: lee.id,
              outcome: 'nobody_owes',
              confidence: 0.9,
            },
          ],
          commitments: [
            noCommitment('mine', sam.id, 2),
            noCommitment('mine', kim.id, 3),
            noCommitment('mine', lee.id, 4),
            noCommitment('theirs', sam.id, 2),
            {
              question_id: 'commitment_theirs:P3',
              contact_id: kim.id,
              direction: 'theirs',
              outcome: 'open',
              confidence: 0.64,
            },
            noCommitment('theirs', lee.id, 4),
          ],
          big_moment: { question_id: 'big_moment', outcome: 'none', confidence: 0.95 },
        },
      ],
    })
    await page.goto(`/interactions?contact=${sam.id}`)
    const bench = benchRow(page, shared.item.identity.id)
    await expect(bench.locator('[data-review-indicator]')).toHaveText('2 to review', {
      timeout: SLOW_TIMEOUT_MS,
    })
    const proposal = proposalOf(await expandRow(bench), 'bench-production')
    const iOwe = proposalList(proposal, 'I owe a reply')
    const owesMe = proposalList(proposal, 'Owes me a reply')
    const commitments = proposalList(proposal, 'Commitments')
    const flag = proposalList(proposal, 'Big moment')
    await expect(proposal.locator('[data-proposal-status]')).toHaveText('2 to review')
    await expect(proposal.getByRole('button', { name: 'Confirm Nones' })).toHaveCount(0)

    // spec: IXN-061.heading-line-closest, IXN-061.named-as-items
    await expect(commitments.locator('[data-closest]')).toHaveText(
      `closest None: Me → ${sam.name} · 31%`
    )
    await expect(commitments.locator('[data-none]')).toHaveCount(0)
    await expect(commitments.locator('[data-item]')).toHaveCount(1)
    await expect(flag.locator('[data-none]')).toHaveText('None closest: Big moment 5%')

    // spec: IXN-061.no-closest-plain
    await expect(iOwe.locator('[data-closest]')).toHaveCount(0)
    await expect(owesMe.locator('[data-none]')).toHaveText('None')

    // spec: IXN-064.nones-wait-for-close
    await iOwe.getByRole('button', { name: `Confirm ${sam.name}` }).click()
    await expect(listItem(iOwe, sam.name)).toHaveAttribute('data-mark', 'confirmed', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(proposal.locator('[data-proposal-status]')).toHaveText('1 to review')
    await expect(proposal.locator('[data-nones-mark]')).toHaveCount(0)

    // spec: IXN-064.close-confirms-nones, IXN-064.items-stay-to-review, IXN-064.nones-mark-copy
    await bench.getByRole('button', { name: 'Collapse content', exact: true }).click()
    await expect(bench.locator('[data-review-indicator]')).toHaveText('1 to review', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expandRow(bench)
    await expect(owesMe.locator('[data-none] [data-nones-mark]')).toHaveText('Confirmed', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(flag.locator('[data-none] [data-nones-mark]')).toHaveText('Confirmed')
    await expect(commitments.locator('[data-closest] [data-nones-mark]')).toHaveText(
      'Nones confirmed'
    )
    await expect(listItem(commitments, `${kim.name} → Me`)).not.toHaveAttribute('data-mark')
    await expect(proposal.locator('[data-proposal-status]')).toHaveText('1 to review')

    // spec: IXN-066.offers-confirmed-none, IXN-066.add-acts-as-before
    await owesMe.getByRole('button', { name: 'Add', exact: true }).click()
    const picker = owesMe.getByRole('group', { name: 'Add to Owes me a reply' })
    expect(
      new Set(await picker.getByRole('button', { name: /^(?!Close)/ }).allInnerTexts())
    ).toEqual(new Set([kim.name, lee.name]))
    await picker.getByRole('button', { name: lee.name }).click()
    const added = listItem(owesMe, lee.name)
    await expect(added.locator('[data-review-mark]')).toHaveText('Added by you', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(added.locator('[data-state-label]')).toHaveText('Live')
  })

  test('each proposal’s header describes itself, and its row counts the items of all', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-048')
    const [sam, kim, lee] = ['sam', 'kim', 'lee'].map(h => entity(seeded, h))
    const { shared, latest } = await groupThread(request, seeded)
    const nobody = (contactId: string, n: number) => ({
      question_id: `ball_in_court:P${n}`,
      contact_id: contactId,
      outcome: 'nobody_owes',
      confidence: 0.9,
    })
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
            nobody(kim.id, 3),
            nobody(lee.id, 4),
          ],
        },
      ],
    })
    await seedJudgmentRun(request, {
      method_key: 'bench-other',
      judgments: [
        {
          thread_interaction_id: entity(seeded, 'group').id,
          cut_off: latest.sent_at,
          ball_in_court: [nobody(kim.id, 3), nobody(lee.id, 4)],
        },
      ],
    })
    await page.goto(`/interactions?contact=${sam.id}`)
    const bench = benchRow(page, shared.item.identity.id)

    // spec: IXN-044.collapsed-indicator, IXN-063.describes-itself, IXN-063.proposal-status
    await expect(bench.locator('[data-review-indicator]')).toHaveText('1 to review', {
      timeout: SLOW_TIMEOUT_MS,
    })
    const region = await expandRow(bench)
    await expect(
      proposalOf(region, 'bench-production').locator('[data-proposal-status]')
    ).toHaveText('1 to review')
    const other = proposalOf(region, 'bench-other')
    await other.getByRole('button', { name: 'Show proposal' }).click()
    await expect(other.locator('[data-proposal-status]')).toHaveText('Nones to review')
  })

  test('a review made in one proposal shows in another proposal on the same content', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-048')
    const { friend, row, latest } = await dmThread(request, seeded)
    const owes = (confidence: number) => ({
      thread_interaction_id: entity(seeded, 'dm').id,
      cut_off: latest.sent_at,
      ball_in_court: [
        {
          question_id: 'ball_in_court:THEM',
          contact_id: friend.id,
          outcome: 'they_owe_reply',
          confidence,
        },
      ],
    })
    await seedJudgmentRun(request, { judgments: [owes(0.9)] })
    await seedJudgmentRun(request, { method_key: 'bench-other', judgments: [owes(0.6)] })
    await page.goto(`/interactions?contact=${friend.id}`)
    const region = await expandRow(benchRow(page, row.item.identity.id))
    const production = proposalList(proposalOf(region, 'bench-production'), 'Owes me a reply')
    const other = proposalOf(region, 'bench-other')
    await other.getByRole('button', { name: 'Show proposal' }).click()
    const otherItem = listItem(proposalList(other, 'Owes me a reply'), friend.name)
    await expect(otherItem).not.toHaveAttribute('data-mark')

    // spec: IXN-067.review-shown-in-other
    await production.getByRole('button', { name: `Confirm ${friend.name}` }).click()
    await expect(otherItem.locator('[data-review-mark]')).toHaveText('Confirmed', {
      timeout: SLOW_TIMEOUT_MS,
    })
  })
})
