import { test, expect } from './fixtures'
import { createTestAPI, TestAPI, type SeedBehaviorResult } from './helpers/test-api'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = { 'X-API-Key': API_KEY }

// This file holds NO mac-host lock. Disjointness from settings-mac.spec.ts is now
// provable from both sides: this file's orphans hang off its declared namespace's
// REVOKED synthetic host (invisible to GET /api/v1/host, deleted by its own
// namespace's cleanup, and outside the singleton index because it is revoked), and
// it performs no mac_host read or request at all; while settings-mac no longer
// resets hosts — each of its worlds pairs one under a namespace-derived hostname
// and deletes exactly that row.

// Interactions tab: the conflict/orphan queue. IMP-038 declares two orphan
// (orphan_needs_review) sessions — enough to drive the amber badge, the orphan
// card + "Log as impromptu", the ?tab=needs-attention alias, and the ?session
// deep-link. Conflict candidate-table rendering and resolution are covered by
// component + backend integration tests (they require event snapshots that are
// fragile to seed via the browser).
test.describe('Imports Interactions tab @area:imports', () => {
  // Serial WITHIN the file, on its own terms: the needs-attention badge is
  // host-unfiltered, so a sibling test's orphans would move the count the
  // assertions below read.
  test.describe.configure({ mode: 'serial' })
  // Headroom for slow declared seeding on a loaded machine.
  test.setTimeout(60_000)

  let testApi: TestAPI
  let seeded: SeedBehaviorResult
  let sessionB: string
  let titleA: string
  let titleB: string

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
    seeded = await testApi.seedBehavior('IMP-038')
    // The manifest keys a meeting note by its SESSION uuid, which is what the
    // ?session deep link carries; the name is the stored session title.
    sessionB = seeded.entities['orphan-b'].id
    titleA = seeded.entities['orphan-a'].name
    titleB = seeded.entities['orphan-b'].name
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  // spec: IMP-026.interactions-tab-holds-conflicts
  test('shows the attention badge and renders orphan cards on the Interactions tab', async ({
    page,
    request,
  }) => {
    // This test seeds a SECOND declared world mid-run (see below), and IMP-026's
    // queue includes an ingest-pipeline candidate that settles a River cascade.
    test.setTimeout(120_000)

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

    // The attention badge counts at least the two declared orphans. This file is
    // serial, so no sibling test's orphans are in flight.
    const badge = page.getByLabel(/\d+ needing attention/)
    await expect(badge).toBeVisible()
    const badgeCount = parseInt((await badge.getAttribute('aria-label'))!, 10)
    expect(badgeCount).toBeGreaterThanOrEqual(2)

    // Discovery items are excluded from the badge: creating a title-derived
    // discovery token mid-test must not move the count. Seeding it here rather
    // than in the fixture IS the claim — a token that already existed when the
    // first count was read could not show that the count ignores it.
    const discovery = await testApi.seedBehavior('IMP-026')

    // Positive anchor, without which the assertion below is satisfiable by
    // absence: a token that was never created also fails to move the count.
    const tokenRes = await request.get(
      `${API_BASE_URL}/api/v1/imports/${discovery.entities['token-a'].id}`,
      { headers: API_HEADERS }
    )
    expect(tokenRes.ok(), 'the mid-test discovery token must actually exist').toBe(true)
    expect((await tokenRes.json())?.data?.source).toBe('anarlog_title')

    await page.reload()
    await page.waitForLoadState('domcontentloaded')
    const badgeAfter = page.getByLabel(/\d+ needing attention/)
    await expect(badgeAfter).toBeVisible({ timeout: 10000 })
    const badgeCountAfter = parseInt((await badgeAfter.getAttribute('aria-label'))!, 10)
    expect(badgeCountAfter).toBe(badgeCount)
  })

  // spec: IMP-026.interactions-tab-holds-conflicts
  test('accepts ?tab=needs-attention as a transitional alias for Interactions', async ({
    page,
  }) => {
    // Note: the alias itself has no SSOT owner (transitional shim); the
    // durable claims cited here are the tab surface + route normalization.
    await page.goto('/imports?tab=needs-attention')
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('tab', { name: /Interactions/ })).toHaveAttribute(
      'aria-selected',
      'true'
    )
    // The URL is normalized to the canonical param. The normalize effect
    // fires after hydration, which can lag under real parallel worker load.
    await expect(page).toHaveURL(/tab=interactions/, { timeout: 15000 })
  })

  // spec: IMP-031.item-leaves-queue-counts-update, IMP-026.interactions-tab-holds-conflicts
  test('resolves the orphan via "Log as impromptu" (orphan_needs_review → linked_impromptu)', async ({
    page,
  }) => {
    // The backend transitions orphan_needs_review → linked_impromptu on
    // resolve-link {none_of_these}, so the card actually leaves the queue.
    // Both declared orphans share meeting_at and the list orders only by
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
      .locator('div.border', { has: page.getByRole('heading', { name: titleA }) })
      .first()

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

  // spec: IMP-038.session-card-present, IMP-038.one-time-session-param
  test('lands on the ?session deep-linked card and strips the one-time param', async ({ page }) => {
    await page.goto(`/imports?tab=interactions&session=${sessionB}`)
    await page.waitForLoadState('domcontentloaded')
    // The deep-linked card is visible and the session param is stripped.
    await expect(page.getByText(titleB).first()).toBeVisible({ timeout: 10000 })
    await expect(page).not.toHaveURL(/session=/)
  })
})
