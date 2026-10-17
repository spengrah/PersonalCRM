import { test, expect } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from './helpers/timeouts'
import { createTestAPI, TestAPI } from './helpers/test-api'
import {
  benchRow,
  contactRows,
  entity,
  expandRow,
  rowMessages,
  seedJudgmentRun,
} from './helpers/bench'

const dateTime = new Intl.DateTimeFormat('en-US', {
  weekday: 'short',
  month: 'short',
  day: 'numeric',
  hour: 'numeric',
  minute: '2-digit',
})

test.describe('Interactions bench runs @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('a view narrowed to a run states the run and lists its incomplete threads', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-043')
    const from = new Date(Date.now() - 7 * 86_400_000)
    const to = new Date(Date.now() - 60_000)
    const incompleteCutOff = new Date(Date.now() - 120_000).toISOString()
    const historical = await seedJudgmentRun(request, {
      kind: 'historical',
      method_version: `v-${seeded.namespace}`,
      from: from.toISOString(),
      to: to.toISOString(),
      judgments: [
        { thread_interaction_id: entity(seeded, 'judged-thread').id, cut_off: to.toISOString() },
      ],
      incomplete: [
        {
          thread_interaction_id: entity(seeded, 'incomplete-thread').id,
          cut_off: incompleteCutOff,
          reason: 'over_limit',
        },
      ],
    })
    const running = await seedJudgmentRun(request, {
      method_version: `v-${seeded.namespace}`,
      unfinished: true,
      incomplete: [
        {
          thread_interaction_id: entity(seeded, 'judged-thread').id,
          cut_off: incompleteCutOff,
          reason: 'call_failed',
        },
      ],
    })
    await page.goto('/interactions')

    // spec: IXN-043.run-chosen-from-list
    const runSelect = page.getByLabel('Run', { exact: true })
    const option = runSelect.locator(`option[value="${historical.run_id}"]`)
    const window = `${new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric' }).format(from)}–`
    await expect(option).toContainText(`Historical · ${window}`, { timeout: SLOW_TIMEOUT_MS })
    await expect(option).toContainText(`bench-production v-${seeded.namespace}`)
    await runSelect.selectOption(historical.run_id)
    await expect(page).toHaveURL(new RegExp(`run=${historical.run_id}`))

    // spec: IXN-043.run-stated
    const summary = page.getByRole('region', { name: 'Run summary' })
    await expect(summary).toContainText('Historical', { timeout: SLOW_TIMEOUT_MS })
    await expect(summary).toContainText(window)
    await expect(summary).toContainText(`bench-production v-${seeded.namespace}`)
    await expect(summary).toContainText('Finished')
    await expect(summary).toContainText('1 thread judged')
    await expect(summary).toContainText('1 incomplete')

    // spec: IXN-043.incomplete-listed
    const incomplete = summary
      .getByRole('list', { name: 'Incomplete threads' })
      .getByRole('listitem')
    await expect(incomplete).toHaveCount(1)
    await expect(incomplete).toContainText(`cut-off ${dateTime.format(new Date(incompleteCutOff))}`)
    await expect(incomplete).toContainText("Over the model's limit")

    // spec: IXN-043.run-stated, IXN-043.incomplete-listed
    await runSelect.selectOption(running.run_id)
    await expect(summary).toContainText('Running', { timeout: SLOW_TIMEOUT_MS })
    await expect(summary).toContainText('0 threads judged')
    await expect(summary.getByRole('list', { name: 'Incomplete threads' })).toContainText(
      'The call failed'
    )
  })

  test('a thread judgment appears once, on the row holding the latest content it read', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-044')
    const [first, second, third] = ['first', 'second', 'third'].map(handle =>
      entity(seeded, handle)
    )
    const [firstRow] = await contactRows(request, first.id)
    const [secondRow] = await contactRows(request, second.id)
    const [thirdRow] = await contactRows(request, third.id)
    const ids = new Set([firstRow, secondRow, thirdRow].map(row => row.item.identity.id))
    expect(ids.size, 'each member of the group holds a row of their own').toBe(3)
    const latest = (await rowMessages(request, thirdRow)).at(-1)!
    const cutOff = latest.sent_at
    const production = await seedJudgmentRun(request, {
      judgments: [
        {
          thread_interaction_id: entity(seeded, 'group').id,
          cut_off: cutOff,
          ball_in_court: [
            {
              question_id: 'ball_in_court:PERSON_2',
              contact_id: first.id,
              outcome: 'they_owe_reply',
              confidence: 0.8,
            },
            {
              question_id: 'ball_in_court:PERSON_4',
              contact_id: third.id,
              outcome: 'they_owe_reply',
              confidence: 0.9,
            },
          ],
        },
      ],
    })
    const other = await seedJudgmentRun(request, {
      method_key: 'bench-other',
      judgments: [
        {
          thread_interaction_id: entity(seeded, 'group').id,
          cut_off: cutOff,
          ball_in_court: [
            {
              question_id: 'ball_in_court:PERSON_3',
              contact_id: second.id,
              outcome: 'i_owe_reply',
              confidence: 0.6,
            },
          ],
        },
      ],
    })

    // spec: IXN-044.one-row-latest-content, IXN-044.collapsed-indicator, IXN-044.names-no-outcomes, IXN-042.run-filter-includes-reused
    await page.goto(`/interactions?run=${production.run_id}`)
    const thirdView = benchRow(page, thirdRow.item.identity.id)
    await expect(thirdView.locator('[data-review-indicator]')).toHaveText('2 to review', {
      timeout: SLOW_TIMEOUT_MS,
    })
    for (const row of [firstRow, secondRow]) {
      await expect(benchRow(page, row.item.identity.id)).toHaveCount(0)
    }
    for (const [member, row] of [
      [first, firstRow],
      [second, secondRow],
    ] as const) {
      await page.goto(`/interactions?contact=${member.id}`)
      await expect(benchRow(page, row.item.identity.id)).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
      await expect(
        benchRow(page, row.item.identity.id).locator('[data-review-indicator]')
      ).toHaveCount(0)
    }
    await page.goto(`/interactions?run=${production.run_id}`)
    await expect(thirdView.locator('[data-proposal]')).toHaveCount(0)
    await expect(thirdView).not.toContainText('Owes me a reply')
    await expect(thirdView).not.toContainText('Proposal')

    // spec: IXN-044.several-methods-production-first
    await page.goto(`/interactions?contact=${third.id}`)
    const region = await expandRow(benchRow(page, thirdRow.item.identity.id))
    const proposals = region.locator('[data-proposal]')
    await expect(proposals).toHaveCount(2)
    await expect(proposals.nth(0)).toContainText('bench-production v1')
    await expect(proposals.nth(0)).toContainText('production method')
    await expect(proposals.nth(1)).toContainText('bench-other v1')

    // spec: IXN-044.run-shows-its-judgments
    await page.goto(`/interactions?run=${other.run_id}&contact=${third.id}`)
    const narrowed = await expandRow(benchRow(page, thirdRow.item.identity.id))
    await expect(narrowed.locator('[data-proposal]')).toHaveCount(1)
    await expect(narrowed.locator('[data-proposal]')).toContainText('bench-other v1')

    // spec: IXN-044.collapsed-indicator
    await page.goto(`/interactions?run=${production.run_id}&contact=${third.id}`)
    const reviewed = benchRow(page, thirdRow.item.identity.id)
    const proposal = (await expandRow(reviewed)).locator('[data-proposal]')
    for (const contact of [first, third]) {
      await proposal.getByRole('button', { name: `Confirm ${contact.name}` }).click()
      await expect(proposal.getByRole('button', { name: `Confirm ${contact.name}` })).toHaveCount(
        0,
        {
          timeout: SLOW_TIMEOUT_MS,
        }
      )
    }
    await expect(reviewed.locator('[data-review-indicator]')).toHaveText('Reviewed')
  })

  test('a judged thread shows its whole content across rows, earlier context above the cut-off and later messages after it', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-044')
    const [first, second, third] = ['first', 'second', 'third'].map(handle =>
      entity(seeded, handle)
    )
    const [firstRow] = await contactRows(request, first.id)
    const [secondRow] = await contactRows(request, second.id)
    const [thirdRow] = await contactRows(request, third.id)
    const [earlier] = await rowMessages(request, firstRow)
    const [judged] = await rowMessages(request, secondRow)
    const [later] = await rowMessages(request, thirdRow)
    await seedJudgmentRun(request, {
      judgments: [
        {
          thread_interaction_id: entity(seeded, 'group').id,
          cut_off: judged.sent_at,
          questions: [
            {
              id: 'commitment_theirs:PERSON_2/source_message',
              probabilities: { [earlier.content_ref]: 0.9 },
              original_answer: earlier.content_ref,
            },
          ],
          commitments: [
            {
              question_id: 'commitment_theirs:PERSON_2',
              contact_id: first.id,
              direction: 'theirs',
              outcome: 'open',
              confidence: 0.9,
            },
          ],
        },
      ],
    })
    await page.goto(`/interactions?contact=${second.id}`)
    const region = await expandRow(benchRow(page, secondRow.item.identity.id))

    // spec: IXN-045.content-cutoff-proposal-together
    const frame = region.locator('[data-judged-frame]')
    await expect(frame.locator('[data-judged-content] [data-content-ref]')).toHaveCount(2, {
      timeout: SLOW_TIMEOUT_MS,
    })
    expect(
      await frame
        .locator('[data-judged-content] [data-content-ref]')
        .evaluateAll(nodes => nodes.map(node => node.getAttribute('data-content-ref')))
    ).toEqual([earlier.content_ref, judged.content_ref])

    // spec: IXN-045.after-content-marked
    const after = region.locator('[data-after-judgment]')
    await expect(after.locator(`[data-content-ref="${later.content_ref}"]`)).toBeVisible()
    await expect(after.getByText('After the judgment')).toHaveCount(1)

    // spec: IXN-046.source-quote-leads
    await frame
      .locator('[data-source-message]')
      .getByRole('button', { name: 'Show in thread' })
      .click()
    await expect(
      frame.locator('[data-highlighted]').locator(`[data-content-ref="${earlier.content_ref}"]`)
    ).toBeVisible()
  })

  test('a proposal sits beneath the content it read, later content marked after the judgment', async ({
    page,
    request,
  }) => {
    const seeded = await testApi.seedBehavior('IXN-045')
    const subject = entity(seeded, 'subject')
    const [row] = await contactRows(request, subject.id)
    const messages = await rowMessages(request, row)
    expect(messages).toHaveLength(3)
    const cutOff = messages[0].sent_at
    const judgment = {
      thread_interaction_id: entity(seeded, 'chat').id,
      cut_off: cutOff,
      ball_in_court: [
        {
          question_id: 'ball_in_court:THEM',
          contact_id: subject.id,
          outcome: 'they_owe_reply',
          confidence: 0.7,
        },
      ],
    }
    await seedJudgmentRun(request, { judgments: [judgment] })
    await seedJudgmentRun(request, { method_key: 'bench-other', judgments: [judgment] })
    await page.goto(`/interactions?contact=${subject.id}`)
    const region = await expandRow(benchRow(page, row.item.identity.id))

    // spec: IXN-045.content-cutoff-proposal-together, IXN-045.cutoff-marked
    const frame = region.locator('[data-judged-frame]')
    await expect(frame).toHaveCount(1)
    await expect(frame.locator('[data-judged-content] [data-message-id]')).toHaveCount(1, {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(frame.locator(`[data-content-ref="${messages[0].content_ref}"]`)).toBeVisible()
    const order = await frame.evaluate(node =>
      Array.from(node.children).map(child =>
        child.hasAttribute('data-judged-content')
          ? 'content'
          : child.hasAttribute('data-cut-off')
            ? 'cut-off'
            : child.hasAttribute('data-proposal')
              ? 'proposal'
              : 'other'
      )
    )
    expect(order).toEqual(['content', 'cut-off', 'proposal', 'proposal'])
    await expect(frame.locator('[data-cut-off]')).toHaveText(
      `judged up to ${dateTime.format(new Date(cutOff))}`
    )

    // spec: IXN-045.after-content-marked
    const after = region.locator('[data-after-judgment]')
    for (const message of messages.slice(1)) {
      await expect(after.locator(`[data-content-ref="${message.content_ref}"]`)).toBeVisible()
    }
    await expect(after.getByText('After the judgment')).toHaveCount(2)
    expect(Number(await after.evaluate(node => getComputedStyle(node).opacity))).toBeLessThan(1)

    // spec: IXN-045.proposal-names-method, IXN-045.other-method-collapsed
    const proposals = frame.locator('[data-proposal]')
    await expect(proposals.nth(0)).toContainText('Proposal')
    await expect(proposals.nth(0)).toContainText('bench-production v1')
    await expect(proposals.nth(0)).toContainText('production method')
    await expect(proposals.nth(1)).toHaveAttribute('data-collapsed')
    await expect(proposals.nth(1)).toContainText('bench-other v1')
    await expect(proposals.nth(1)).toContainText('Not acting · 1 item')
    await proposals.nth(1).getByRole('button', { name: 'Show proposal' }).click()
    await expect(proposals.nth(1).getByRole('group', { name: 'Owes me a reply' })).toContainText(
      subject.name
    )
  })
})
