import { test, expect } from '@playwright/test'
import { createTestAPI, TestAPI, type SeedBehaviorResult } from './helpers/test-api'
import { waitForOverdueListSettled } from './helpers/dashboard'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

test.describe('Cadence skip @area:dashboard @area:overdue', () => {
  let testApi: TestAPI
  let seeded: SeedBehaviorResult

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
    seeded = await testApi.seedBehavior('CAD-046')
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('both card states offer exactly Log Interaction and Skip this cycle, and the name is the link', async ({
    page,
  }) => {
    // spec: CAD-046.two-actions-both-states, CAD-046.mark-as-contacted-removed, CAD-046.name-is-the-link
    const targetId = seeded.entities['target'].id
    const sentinelId = seeded.entities['sentinel'].id
    const awaitingId = seeded.entities['awaiting'].id
    const targetName = seeded.entities['target'].name
    const awaitingName = seeded.entities['awaiting'].name
    const settled = waitForOverdueListSettled(page, {
      presentIds: [targetId, awaitingId, sentinelId],
    })
    await page.goto('/dashboard')
    await settled

    for (const [name, id] of [
      [targetName, targetId],
      [awaitingName, awaitingId],
    ] as const) {
      const card = page
        .getByRole('listitem')
        .filter({ has: page.getByRole('heading', { name, exact: true }) })
      await expect(card.getByRole('button')).toHaveCount(2)
      await expect(card.getByRole('button').nth(0)).toHaveText('Log Interaction')
      await expect(card.getByRole('button').nth(1)).toHaveText('Skip this cycle')
      await expect(card.getByRole('button', { name: /Mark as Contacted/i })).toHaveCount(0)
      await expect(card.getByRole('link', { name: 'View details' })).toHaveCount(0)
      await expect(
        card.getByRole('heading', { level: 3 }).getByRole('link', { name, exact: true })
      ).toHaveAttribute('href', `/contacts/${id}`)
    }

    await expect(page.getByRole('button', { name: /Mark as Contacted/i })).toHaveCount(0)
    await expect(
      page
        .getByRole('listitem')
        .filter({ has: page.getByRole('heading', { name: awaitingName, exact: true }) })
        .getByTestId('awaiting-reply-note')
    ).toBeVisible()
  })

  test('skipping an awaiting-reply card records the skip and ends the wait without a reload', async ({
    page,
    request,
  }) => {
    // spec: CAD-046.skip-ends-awaiting-reply
    const awaitingId = seeded.entities['awaiting'].id
    const awaitingName = seeded.entities['awaiting'].name
    const targetId = seeded.entities['target'].id
    const sentinelId = seeded.entities['sentinel'].id
    const beforeResponse = await request.get(`${API_BASE_URL}/api/v1/contacts/${awaitingId}`, {
      headers: API_HEADERS,
    })
    const before = (await beforeResponse.json()).data
    expect(before.awaiting_reply).toBe(true)
    expect(before.contact_by).toBeTruthy()

    const settled = waitForOverdueListSettled(page, {
      presentIds: [targetId, awaitingId, sentinelId],
    })
    await page.goto('/dashboard')
    await settled
    const card = page
      .getByRole('listitem')
      .filter({ has: page.getByRole('heading', { name: awaitingName, exact: true }) })
    await page.evaluate(() => {
      ;(window as Window & { __cadSkipNoReload?: boolean }).__cadSkipNoReload = true
    })

    const skipResponsePromise = page.waitForResponse(
      response =>
        response.request().method() === 'POST' &&
        response.url().includes(`/api/v1/contacts/${awaitingId}/skip`)
    )
    const refetchPromise = page.waitForResponse(async response => {
      if (
        response.request().method() !== 'GET' ||
        !response.url().includes('/api/v1/contacts/overdue') ||
        !response.ok()
      ) {
        return false
      }
      const body = await response.json().catch(() => null)
      return (body?.data ?? []).some(
        (entry: { id: string; awaiting_reply: boolean }) =>
          entry.id === awaitingId && entry.awaiting_reply === false
      )
    })
    const beforeClick = Date.now()
    await card.getByRole('button', { name: 'Skip this cycle', exact: true }).click()
    const response = await skipResponsePromise
    expect(response.status()).toBe(200)
    const body = (await response.json()).data
    expect(body.awaiting_reply).toBe(false)
    expect(body.last_skipped_contact_by).toBe(before.contact_by)
    expect(Date.parse(body.last_skipped_at)).toBeGreaterThanOrEqual(beforeClick - 1000)
    expect(Date.parse(body.last_skipped_at)).toBeLessThanOrEqual(Date.now() + 1000)
    expect(typeof body.undo_skip_available).toBe('boolean')
    await refetchPromise
    await expect(card.getByTestId('awaiting-reply-note')).toHaveCount(0)
    expect(
      await page.evaluate(
        () => (window as Window & { __cadSkipNoReload?: boolean }).__cadSkipNoReload
      )
    ).toBe(true)
    await expect(page).toHaveURL(/\/dashboard$/)

    const afterResponse = await request.get(`${API_BASE_URL}/api/v1/contacts/${awaitingId}`, {
      headers: API_HEADERS,
    })
    const after = (await afterResponse.json()).data
    expect(after.awaiting_reply).toBe(false)
    expect(after.last_skipped_contact_by).toBe(before.contact_by)
  })
})
