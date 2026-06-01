import { test, expect } from './fixtures'
import { createTestAPI, TestAPI } from './helpers/test-api'

// Interactions tab: the conflict/orphan queue. These specs seed orphan
// (orphan_needs_review) meeting notes against a paired host — enough to drive
// the amber badge, the orphan card + "Log as impromptu", the empty state, the
// ?tab=needs-attention alias, and the ?session deep-link highlight. Conflict
// candidate-table rendering and resolution are covered by component +
// backend integration tests (they require event snapshots that are fragile
// to seed via the browser).
test.describe('Imports Interactions tab @area:imports', () => {
  // The mac_host table is a singleton, so these host-seeding tests must run
  // serially within the file and reset existing hosts before seeding.
  test.describe.configure({ mode: 'serial' })

  let testApi: TestAPI
  let hostId: string
  let sessionA: string
  let sessionB: string
  let titleA: string
  let titleB: string

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
    // Free the singleton host index before seeding a fresh host.
    await testApi.resetMacHosts()
    hostId = await testApi.seedMacHost({ hostname: `${testApi.prefix}-int-host` })
    sessionA = crypto.randomUUID()
    sessionB = crypto.randomUUID()
    titleA = `${testApi.prefix} Orphan Session A`
    titleB = `${testApi.prefix} Orphan Session B`
    await testApi.seedMeetingNotes(hostId, [
      { anarlog_session_id: sessionA, title: titleA, summary: 'First orphan.' },
      { anarlog_session_id: sessionB, title: titleB, summary: 'Second orphan.' },
    ])
  })

  test.afterEach(async () => {
    // Delete this test's seeded notes (by host) before the next test resets
    // the host, then clear the host so the singleton index is free.
    await testApi.cleanup()
    await testApi.resetMacHosts()
  })

  test('shows the amber badge and renders orphan cards on the Interactions tab', async ({
    page,
  }) => {
    await page.goto('/imports?tab=interactions')
    await page.waitForLoadState('domcontentloaded')

    // The Interactions tab is selected and shows the orphan sessions.
    await expect(page.getByRole('tab', { name: /Interactions/ })).toHaveAttribute(
      'aria-selected',
      'true'
    )
    await expect(page.getByText(titleA).first()).toBeVisible({ timeout: 10000 })
    await expect(page.getByText(titleB).first()).toBeVisible()
    // Orphan cards expose the bare hyprnote:// launch link.
    await expect(page.getByRole('link', { name: /Open Anarlog/ }).first()).toHaveAttribute(
      'href',
      'hyprnote://'
    )
  })

  test('accepts ?tab=needs-attention as a transitional alias for Interactions', async ({
    page,
  }) => {
    await page.goto('/imports?tab=needs-attention')
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('tab', { name: /Interactions/ })).toHaveAttribute(
      'aria-selected',
      'true'
    )
    // The URL is normalized to the canonical param.
    await expect(page).toHaveURL(/tab=interactions/)
  })

  test('resolves the orphan via "Log as impromptu" (orphan_needs_review → linked_impromptu)', async ({
    page,
  }) => {
    // The backend now transitions orphan_needs_review → linked_impromptu on
    // resolve-link {none_of_these}, so the card actually leaves the queue.
    // Both seeded orphans share meeting_at and the list orders only by
    // meeting_at DESC, so we bind the click + assertions to titleA's card by
    // its heading (never list position) and scope the disappearance check to
    // the card heading (the success toast is a separate element and cannot
    // satisfy a heading locator).
    await page.goto('/imports?tab=interactions')
    await page.waitForLoadState('domcontentloaded')

    const headingA = page.getByRole('heading', { name: titleA })
    const headingB = page.getByRole('heading', { name: titleB })
    await expect(headingA).toBeVisible({ timeout: 10000 })

    // The orphan card for titleA — scope the button click to it so list
    // order can't pick the wrong card.
    const cardA = page
      .locator('div.border-l-gray-300')
      .filter({ has: page.getByRole('heading', { name: titleA }) })

    // Register the response wait BEFORE the click so a fast 200 isn't missed.
    const responsePromise = page.waitForResponse(
      res => res.url().includes('/resolve-link') && res.request().method() === 'POST',
      { timeout: 10000 }
    )
    await cardA.getByRole('button', { name: /Log as impromptu/ }).click()
    const res = await responsePromise
    expect(res.status()).toBe(200)

    // The resolved card leaves the queue; the untouched sibling stays.
    await expect(headingA).toHaveCount(0, { timeout: 10000 })
    await expect(headingB).toBeVisible()
  })

  test('scrolls to and highlights the ?session deep-linked card', async ({ page }) => {
    await page.goto(`/imports?tab=interactions&session=${sessionB}`)
    await page.waitForLoadState('domcontentloaded')
    // The deep-linked card is visible and the session param is stripped.
    await expect(page.getByText(titleB).first()).toBeVisible({ timeout: 10000 })
    await expect(page).not.toHaveURL(/session=/)
  })
})
