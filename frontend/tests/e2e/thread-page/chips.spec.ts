import { test, expect, type Locator, type Page } from '@playwright/test'
import { SLOW_TIMEOUT_MS } from '../helpers/timeouts'
import { createTestAPI, TestAPI } from '../helpers/test-api'
import { COMPARED_METHODS } from '../helpers/bench'
import type { FlagKind } from '../../../src/types/generated/judgment'
import {
  flag,
  nightsOf,
  reviewOf,
  reviewWorld,
  seedRun,
  startWithReview,
  threadUrl,
} from './helpers'

const KIND_NAMES: Record<FlagKind, string> = {
  commitment: 'Commitment',
  big_moment: 'Big moment',
  claim: 'Claim',
}

const message = (page: Page, key: string) => page.locator(`[data-message-key="${key}"]`)
const chip = (page: Page, key: string, kind: FlagKind): Locator =>
  message(page, key).locator(`[data-chip="${kind}"]`)

test.describe('Flag chips on the thread page @area:interactions', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  /** The board chat's first night, flagged as given, opened with Review on. */
  async function openFlagged(
    page: Page,
    request: Parameters<typeof reviewWorld>[1],
    flags: (messages: { message_key: string }[]) => ReturnType<typeof flag>[]
  ) {
    const world = await reviewWorld(testApi, request)
    const { interactionId, thread, messages } = world.board
    const runId = await seedRun(request, interactionId, {
      nights: nightsOf(messages, [{ from: 0, to: 4, flags: flags(messages) }]),
    })
    await startWithReview(page, true)
    await page.goto(threadUrl(thread.thread_id, { contact: world.ana.id, run: runId }))
    await expect(page.locator('[data-judgment-box]')).toBeVisible({ timeout: SLOW_TIMEOUT_MS })
    return { world, runId, messages, threadId: thread.thread_id }
  }

  const recorded = async (
    request: Parameters<typeof reviewOf>[0],
    threadId: string,
    runId: string,
    key: string,
    kind: FlagKind
  ) =>
    (await reviewOf(request, threadId, runId)).nights
      .flatMap(night => night.messages)
      .find(m => m.message_key === key)
      ?.chips.find(c => c.kind === kind)

  test('names a flag’s kind and probability and offers confirm and remove while it has no label', async ({
    page,
    request,
  }) => {
    const { messages } = await openFlagged(page, request, all => [
      flag(all[1].message_key, 'commitment', 0.91),
    ])
    const toReview = chip(page, messages[1].message_key, 'commitment')

    // spec: IXN-081.chip-names-kind-and-probability, IXN-081.chip-states
    await expect(toReview).toHaveAttribute('data-chip-state', 'to_review')
    await expect(toReview.locator('[data-chip-name]')).toHaveText('Commitment')
    await expect(toReview.locator('[data-chip-probability]')).toHaveText('91%')
    await expect(toReview.locator('[data-chip-label]')).toHaveText('To review')
    expect(await toReview.evaluate(node => getComputedStyle(node).borderStyle)).toBe('dashed')

    // spec: IXN-081.confirm-remove-change
    await expect(toReview.getByRole('button', { name: 'Confirm Commitment' })).toBeVisible()
    await expect(toReview.getByRole('button', { name: 'Remove Commitment' })).toBeVisible()
    await expect(toReview.getByRole('button', { name: /^Change/ })).toHaveCount(0)
  })

  test('confirm labels the flag as held, and the chip reads Confirmed with Change', async ({
    page,
    request,
  }) => {
    const { messages, runId, threadId } = await openFlagged(page, request, all => [
      flag(all[1].message_key, 'big_moment', 0.8),
    ])
    const key = messages[1].message_key
    await chip(page, key, 'big_moment').getByRole('button', { name: 'Confirm Big moment' }).click()

    // spec: IXN-081.confirm-remove-change, IXN-081.chip-states
    const confirmed = chip(page, key, 'big_moment')
    await expect(confirmed).toHaveAttribute('data-chip-state', 'confirmed')
    await expect(confirmed.locator('[data-chip-label]')).toHaveText('Confirmed')
    await expect(confirmed.getByRole('button', { name: 'Change Big moment' })).toBeVisible()
    await expect(confirmed.getByRole('button', { name: /^Confirm/ })).toHaveCount(0)
    expect((await recorded(request, threadId, runId, key, 'big_moment'))?.state).toBe('confirmed')
  })

  test('remove labels the flag as not held, and the chip reads Removed with its name struck through', async ({
    page,
    request,
  }) => {
    const { messages, runId, threadId } = await openFlagged(page, request, all => [
      flag(all[1].message_key, 'claim', 0.7),
    ])
    const key = messages[1].message_key
    await chip(page, key, 'claim').getByRole('button', { name: 'Remove Claim' }).click()

    // spec: IXN-081.confirm-remove-change, IXN-081.chip-states
    const removed = chip(page, key, 'claim')
    await expect(removed).toHaveAttribute('data-chip-state', 'removed')
    await expect(removed.locator('[data-chip-label]')).toHaveText('Removed')
    expect(
      await removed
        .locator('[data-chip-name]')
        .evaluate(node => getComputedStyle(node).textDecorationLine)
    ).toBe('line-through')
    await expect(removed.getByRole('button', { name: 'Change Claim' })).toBeVisible()
    expect((await recorded(request, threadId, runId, key, 'claim'))?.state).toBe('removed')
  })

  test('Change on a confirmed chip offers confirm and remove, and removing records the new label', async ({
    page,
    request,
  }) => {
    const { messages, runId, threadId } = await openFlagged(page, request, all => [
      flag(all[1].message_key, 'claim', 0.7),
    ])
    const key = messages[1].message_key
    await chip(page, key, 'claim').getByRole('button', { name: 'Confirm Claim' }).click()
    await expect(chip(page, key, 'claim')).toHaveAttribute('data-chip-state', 'confirmed')

    // spec: IXN-081.confirm-remove-change
    await chip(page, key, 'claim').getByRole('button', { name: 'Change Claim' }).click()
    await expect(
      chip(page, key, 'claim').getByRole('button', { name: 'Remove Claim' })
    ).toBeVisible()
    await chip(page, key, 'claim').getByRole('button', { name: 'Remove Claim' }).click()
    await expect(chip(page, key, 'claim')).toHaveAttribute('data-chip-state', 'removed')
    expect((await recorded(request, threadId, runId, key, 'claim'))?.state).toBe('removed')
  })

  test('+ Flag offers only the kinds the message shows no chip for, and records the choice as Added with no probability', async ({
    page,
    request,
  }) => {
    const { messages, runId, threadId } = await openFlagged(page, request, all => [
      flag(all[1].message_key, 'commitment', 0.9),
      // Below the display threshold: this flag shows no chip.
      flag(all[1].message_key, 'big_moment', 0.2),
    ])
    const key = messages[1].message_key
    await expect(chip(page, key, 'big_moment')).toHaveCount(0)

    // spec: IXN-081.plus-flag-adds
    await message(page, key).getByRole('button', { name: '+ Flag' }).click()
    const menu = page.getByRole('menu', { name: 'Add a flag' })
    await expect(menu.getByRole('menuitem')).toHaveText([KIND_NAMES.big_moment, KIND_NAMES.claim])
    await menu.getByRole('menuitem', { name: 'Claim' }).click()

    // spec: IXN-081.chip-states
    const added = chip(page, key, 'claim')
    await expect(added).toHaveAttribute('data-chip-state', 'added')
    await expect(added.locator('[data-chip-label]')).toHaveText('Added')
    await expect(added.locator('[data-chip-probability]')).toHaveCount(0)
    await expect(added.getByRole('button', { name: 'Change Claim' })).toBeVisible()
    expect((await recorded(request, threadId, runId, key, 'claim'))?.state).toBe('added')
    // The kind now shown is no longer offered.
    await message(page, key).getByRole('button', { name: '+ Flag' }).click()
    await expect(page.getByRole('menu', { name: 'Add a flag' }).getByRole('menuitem')).toHaveText([
      KIND_NAMES.big_moment,
    ])
  })

  test('a message showing a chip of every kind offers no + Flag', async ({ page, request }) => {
    const { messages } = await openFlagged(page, request, all =>
      (['commitment', 'big_moment', 'claim'] as const).map(kind =>
        flag(all[1].message_key, kind, 0.9)
      )
    )

    // spec: IXN-081.plus-flag-adds
    await expect(message(page, messages[1].message_key).locator('[data-chip]')).toHaveCount(3)
    await expect(
      message(page, messages[1].message_key).getByRole('button', { name: '+ Flag' })
    ).toHaveCount(0)
  })

  test('a label made under one method’s run shows on another method’s run', async ({
    page,
    request,
  }) => {
    const world = await reviewWorld(testApi, request)
    const { interactionId, thread, messages } = world.board
    const key = messages[1].message_key
    const night = (probability: number) =>
      nightsOf(messages, [{ from: 0, to: 4, flags: [flag(key, 'commitment', probability)] }])
    const clef = await seedRun(request, interactionId, { nights: night(0.9) })
    const jev = await seedRun(request, interactionId, {
      method_key: COMPARED_METHODS.jev,
      from: '2026-02-01T00:00:00Z',
      to: '2026-02-28T00:00:00Z',
      nights: night(0.6),
    })
    await startWithReview(page, true)
    await page.goto(threadUrl(thread.thread_id, { contact: world.ana.id, run: jev }))
    await expect(chip(page, key, 'commitment')).toHaveAttribute('data-chip-state', 'to_review', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(chip(page, key, 'commitment').locator('[data-chip-probability]')).toHaveText('60%')

    // The user labels it under the first method's run.
    await page.goto(threadUrl(thread.thread_id, { contact: world.ana.id, run: clef }))
    await chip(page, key, 'commitment').getByRole('button', { name: 'Confirm Commitment' }).click()
    await expect(chip(page, key, 'commitment')).toHaveAttribute('data-chip-state', 'confirmed')

    // spec: IXN-081.label-on-every-run
    await page.goto(threadUrl(thread.thread_id, { contact: world.ana.id, run: jev }))
    await expect(chip(page, key, 'commitment')).toHaveAttribute('data-chip-state', 'confirmed', {
      timeout: SLOW_TIMEOUT_MS,
    })
    await expect(chip(page, key, 'commitment').locator('[data-chip-probability]')).toHaveText('60%')
  })

  test('a commitment a later night judged done reads cleared with that night’s date', async ({
    page,
    request,
  }) => {
    const world = await reviewWorld(testApi, request)
    const { interactionId, thread, messages } = world.board
    const key = messages[1].message_key
    const [first, second] = nightsOf(messages, [
      { from: 0, to: 4, flags: [flag(key, 'commitment', 0.9)] },
      { from: 5, to: 9, window: 0 },
    ])
    second.clearings = [
      {
        commitment_message_key: key,
        commitment_cut_off: first.cut_off,
        done: true,
        probability: 0.85,
      },
    ]
    const runId = await seedRun(request, interactionId, { nights: [first, second] })
    await startWithReview(page, true)
    await page.goto(threadUrl(thread.thread_id, { contact: world.ana.id, run: runId }))

    // spec: IXN-081.commitment-cleared
    const cleared = chip(page, key, 'commitment')
    const date = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric' }).format(
      new Date(second.cut_off)
    )
    await expect(cleared.locator('[data-chip-cleared]')).toHaveText(`cleared ${date}`, {
      timeout: SLOW_TIMEOUT_MS,
    })
  })
})
