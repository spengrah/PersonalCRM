import { test, expect } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'
import { expectAddContactHeader } from './helpers/dashboard'

test.describe('Dashboard @area:dashboard', () => {
  test('should display dashboard with navigation @smoke', async ({ page }) => {
    // spec: DSH-001[0]
    await page.goto('/')

    // Should redirect to dashboard (client-side redirect via useEffect)
    await expect(page).toHaveURL('/dashboard', { timeout: 10000 })

    // Wait for page to fully load
    await page.waitForLoadState('domcontentloaded')

    // Should have correct title
    await expect(page).toHaveTitle(/Personal CRM/)

    // Should show navigation with links (use exact: true to avoid matching "View All Contacts")
    await expect(page.getByRole('link', { name: 'Dashboard', exact: true })).toBeVisible()
    await expect(page.getByRole('link', { name: 'Contacts', exact: true })).toBeVisible()

    // Should show "Action Required" heading (the main h2 heading)
    await expect(page.getByRole('heading', { name: 'Action Required', level: 2 })).toBeVisible()
  })

  test('should navigate to contacts from dashboard', async ({ page }) => {
    await page.goto('/dashboard')

    // Click on contacts navigation
    await page.getByRole('link', { name: 'Contacts' }).click()

    // Should navigate to contacts page
    await expect(page).toHaveURL('/contacts')
    // Use level: 2 to target the main h2 heading, not the h3 "No contacts"
    await expect(page.getByRole('heading', { name: 'Contacts', level: 2 })).toBeVisible()
  })

  test('should show dashboard content when loaded', async ({ page }) => {
    await page.goto('/dashboard')

    // Wait for content to load
    await page.waitForLoadState('domcontentloaded')

    // Should show status message (either overdue count or "all caught up")
    const hasOverdue = await page
      .getByText('contacts need your attention')
      .isVisible()
      .catch(() => false)
    const hasCaughtUp = await page
      .getByText("You're all caught up")
      .isVisible()
      .catch(() => false)

    expect(hasOverdue || hasCaughtUp).toBeTruthy()
  })

  test('caught-up state offers add-contact and view-list paths', async ({ page }) => {
    // spec: DSH-003[0], DSH-003[1]
    // Route-mock an EMPTY overdue list (full apiClient envelope) before first
    // load so the caught-up state renders deterministically regardless of what
    // parallel workers have seeded (per-page interception, no DB mutation).
    await page.route('**/api/v1/contacts/overdue*', route =>
      route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ success: true, data: [] }),
      })
    )

    await page.goto('/dashboard')
    await expect(page.getByRole('heading', { name: /All caught up/ })).toBeVisible()

    // Both affordances, with their destinations — visibility alone would not
    // prove the offered paths lead to the add/browse surfaces.
    const viewAll = page.getByRole('link', { name: 'View All Contacts' })
    await expect(viewAll).toBeVisible()
    await expect(viewAll).toHaveAttribute('href', '/contacts')
    const addNew = page.getByRole('link', { name: 'Add New Contact' })
    await expect(addNew).toBeVisible()
    await expect(addNew).toHaveAttribute('href', '/contacts/new')

    // The header add-contact CTA is present in the CAUGHT-UP state too.
    await expectAddContactHeader(page)
  })

  test('dashboard exposes no dashboard-level or global search surface', async ({ page }) => {
    // spec: DSH-007[1]
    // NEGATIVE existence proof at a settled state: establish the dashboard
    // has fully rendered first, THEN assert that no plausible search-surface
    // shape is present (a not-yet-rendered page would pass these vacuously).
    // Search lives on the contact list (DSH-007[0], contacts.spec.ts).
    await page.goto('/dashboard')
    await expect(page.getByRole('heading', { name: 'Action Required', level: 2 })).toBeVisible()

    await expect(page.getByRole('searchbox')).toHaveCount(0)
    await expect(page.getByRole('textbox', { name: /search/i })).toHaveCount(0)
    await expect(page.getByPlaceholder(/search/i)).toHaveCount(0)
    await expect(page.getByRole('button', { name: /search|command|⌘K/i })).toHaveCount(0)
  })
})

test.describe('Dashboard - Overdue Cards @area:dashboard @area:overdue', () => {
  let testApi: TestAPI

  // One contact per urgency tier (dashboard/page.tsx getUrgencyIndicator
  // boundaries: <=2 yellow, <=7 orange, >7 red).
  const tierSeeds = [
    { key: 'Tier Low Contact', days_overdue: 1, dot: 'bg-yellow-500' },
    { key: 'Tier Mid Contact', days_overdue: 5, dot: 'bg-orange-500' },
    { key: 'Tier High Contact', days_overdue: 20, dot: 'bg-red-500' },
  ]

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
    await testApi.seedOverdueContacts(
      tierSeeds.map(seed => ({
        full_name: seed.key,
        cadence: 'weekly' as const,
        days_overdue: seed.days_overdue,
        email: `${seed.key.toLowerCase().replace(/ /g, '-')}@example.com`,
      }))
    )
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('shows overdue contacts as cards with the count in the header', async ({ page }) => {
    // spec: CAD-026[0]
    await page.goto('/dashboard')
    await page.waitForLoadState('domcontentloaded')

    // Every seeded card renders (prefix-scoped names — parallel-safe).
    for (const seed of tierSeeds) {
      await expect(
        page.getByRole('heading', { name: `${testApi.prefix}-${seed.key}` })
      ).toBeVisible()
    }

    // The header carries the overdue count. The count is GLOBAL (other
    // workers seed overdue contacts too), so assert a loose lower bound
    // against our own rendered cards, never an exact number.
    const headerText = await page.getByText(/\d+ contacts? need your attention/).textContent()
    const headerCount = Number(/(\d+)/.exec(headerText ?? '')?.[1])
    expect(headerCount).toBeGreaterThanOrEqual(tierSeeds.length)
  })

  test('each card shows urgency tier, cadence, recency, a reachable method, and the suggested action', async ({
    page,
  }) => {
    // spec: CAD-026[1]
    await page.goto('/dashboard')
    await page.waitForLoadState('domcontentloaded')

    // All three urgency tiers are exercised. The tier dot is CSS-only (no
    // accessible text), so the color class is the observable signal — the
    // same one the retired verifier graded.
    for (const seed of tierSeeds) {
      const name = `${testApi.prefix}-${seed.key}`
      await expect(page.getByRole('heading', { name })).toBeVisible()
      const card = page.locator('div.rounded-lg').filter({ hasText: name })
      await expect(card.locator(`div.rounded-full.${seed.dot}`)).toBeVisible()
    }

    // The remaining card sub-elements (cadence, recency, a reachable method,
    // the suggested action) are text — assert them on the mid-tier card.
    const midName = `${testApi.prefix}-Tier Mid Contact`
    const midCard = page.locator('div.rounded-lg').filter({ hasText: midName })
    await expect(midCard.getByText('(weekly cadence)')).toBeVisible()
    await expect(midCard.getByText(/\d+ days overdue.*Last contacted/)).toBeVisible()
    await expect(midCard.getByText('tier-mid-contact@example.com')).toBeVisible()
    await expect(midCard.getByText('Email', { exact: true })).toBeVisible()
    const suggestion = await midCard.getByText(/💡/).textContent()
    expect(suggestion?.replace('💡', '').trim().length).toBeGreaterThan(0)
  })
})

test.describe('Dashboard - All Caught Up (mocked) @area:dashboard', () => {
  test('shows the all-caught-up state when nothing is overdue', async ({ page }) => {
    // spec: CAD-026[2]
    // The empty-overdue state is GLOBAL — parallel workers seed overdue
    // contacts, so it is not deterministically reachable by seeding. Mock the
    // overdue response with the full apiClient envelope (a bare array would
    // fall through to a real call). Route installed BEFORE goto.
    await page.route('**/api/v1/contacts/overdue', route =>
      route.fulfill({ json: { success: true, data: [] } })
    )
    await page.goto('/dashboard')

    await expect(page.getByRole('heading', { name: 'All caught up! 🎉' })).toBeVisible()
    await expect(page.getByText("You're all caught up")).toBeVisible()
  })
})

test.describe('Dashboard - Sort Orderings (mocked) @area:dashboard', () => {
  // The sort is CLIENT-SIDE over the fetched overdue list (dashboard/page.tsx
  // sortBy state — no sort= request is issued), so the observable outcome is
  // the rendered card DOM order. One full-envelope route mock feeds all three
  // orderings; the fixture is built so urgency, name, and last-contacted
  // orders are pairwise DISTINCT (a no-op or wrong sort fails), and one
  // never-contacted record proves the null-sink branch.
  const fixtureSuffix = 'Sortfix'
  const overdueEntry = (over: { name: string; days: number; lastContacted: string | null }) => ({
    id: `mock-${over.name.toLowerCase().replace(/ /g, '-')}`,
    full_name: over.name,
    methods: [],
    cadence: 'weekly',
    last_contacted: over.lastContacted,
    contact_by: '2026-07-01T00:00:00Z',
    has_pending_followup: false,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    days_overdue: over.days,
    next_due_date: '2026-07-01T00:00:00Z',
    suggested_action: 'A quick check-in to reconnect',
  })
  // urgency (days desc):      Zulu(30), Mike(12), Alpha(3), Bravo(1)
  // name (alphabetical):      Alpha, Bravo, Mike, Zulu
  // last-contacted (oldest→): Alpha(01-10), Bravo(03-01), Zulu(05-01), Mike(null last)
  const fixture = [
    overdueEntry({
      name: `Alpha ${fixtureSuffix}`,
      days: 3,
      lastContacted: '2026-01-10T12:00:00Z',
    }),
    overdueEntry({
      name: `Zulu ${fixtureSuffix}`,
      days: 30,
      lastContacted: '2026-05-01T12:00:00Z',
    }),
    overdueEntry({ name: `Mike ${fixtureSuffix}`, days: 12, lastContacted: null }),
    overdueEntry({
      name: `Bravo ${fixtureSuffix}`,
      days: 1,
      lastContacted: '2026-03-01T12:00:00Z',
    }),
  ]

  const gotoMockedDashboard = async (page: import('@playwright/test').Page) => {
    await page.route('**/api/v1/contacts/overdue', route =>
      route.fulfill({ json: { success: true, data: fixture } })
    )
    await page.goto('/dashboard')
    await expect(page.getByRole('heading', { name: `Zulu ${fixtureSuffix}` })).toBeVisible()
  }

  // The rendered card order, read as DOM order of the card name headings
  // (only the mocked cards render — the mock replaced the whole list).
  const cardOrder = async (page: import('@playwright/test').Page) => {
    const headings = await page.locator('h3').allTextContents()
    return headings.filter(h => h.endsWith(fixtureSuffix))
  }

  test('urgency (default) orders most-overdue first', async ({ page }) => {
    // spec: CAD-027[0]
    await gotoMockedDashboard(page)
    expect(await cardOrder(page)).toEqual([
      `Zulu ${fixtureSuffix}`,
      `Mike ${fixtureSuffix}`,
      `Alpha ${fixtureSuffix}`,
      `Bravo ${fixtureSuffix}`,
    ])
  })

  test('name orders alphabetically', async ({ page }) => {
    // spec: CAD-027[1]
    await gotoMockedDashboard(page)
    await page.getByRole('button', { name: 'Name', exact: true }).click()
    await expect
      .poll(() => cardOrder(page))
      .toEqual([
        `Alpha ${fixtureSuffix}`,
        `Bravo ${fixtureSuffix}`,
        `Mike ${fixtureSuffix}`,
        `Zulu ${fixtureSuffix}`,
      ])
  })

  test('last-contacted orders oldest first with never-contacted last', async ({ page }) => {
    // spec: CAD-027[2]
    await gotoMockedDashboard(page)
    await page.getByRole('button', { name: 'Last Contacted', exact: true }).click()
    // Mike (last_contacted: null) renders "Never contacted" and must sink to
    // the END regardless of its days_overdue — the null-sink branch.
    await expect
      .poll(() => cardOrder(page))
      .toEqual([
        `Alpha ${fixtureSuffix}`,
        `Bravo ${fixtureSuffix}`,
        `Zulu ${fixtureSuffix}`,
        `Mike ${fixtureSuffix}`,
      ])
    const mikeCard = page.locator('div.rounded-lg').filter({ hasText: `Mike ${fixtureSuffix}` })
    await expect(mikeCard.getByText('Never contacted')).toBeVisible()
  })
})

test.describe('Dashboard - With Seeded Data @area:dashboard @area:overdue', () => {
  let testApi: TestAPI
  let overdueContactId: string

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)

    // Seed an overdue contact for dashboard testing
    const { ids } = await testApi.seedOverdueContacts([
      {
        full_name: 'Dashboard Test Contact',
        cadence: 'weekly',
        days_overdue: 3,
        email: 'dashboard-test@example.com',
      },
    ])
    overdueContactId = ids[0]
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('header add-contact action is available on a populated dashboard', async ({ page }) => {
    // spec: DSH-003[0]
    // Establish the POPULATED state first — the seeded overdue card must have
    // rendered (a loading or error mask would otherwise vacuously pass a
    // state-independent affordance check) — then assert the header CTA.
    const contactName = `${testApi.prefix}-Dashboard Test Contact`
    await page.goto('/dashboard')
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: contactName })).toBeVisible()

    await expectAddContactHeader(page)
  })

  test('marking contact as contacted updates dashboard immediately without navigation', async ({
    page,
  }) => {
    // spec: DSH-005[0]
    // Cited for DSH-005[0] only: the on-dashboard interaction:created trigger
    // refreshing the overdue list without a manual reload. DSH-005's broader
    // trigger coverage (merge / meeting-note-resolve), the cosmetic-edit no-op,
    // and the refocus/staleTime timing were verifier-abstained and are not
    // asserted here. Deliberately NOT cited for CAD-028: this test proves only
    // part of that behavior (the mutual interaction and the no-reload
    // overdue-list exit). Its other then-items — the accelerated-clock
    // timestamp, the count update, and dashboard/list/detail consistency — are
    // not asserted here, and a partial proof must not mark the behavior
    // covered.
    const contactName = `${testApi.prefix}-Dashboard Test Contact`

    // Navigate to dashboard
    await page.goto('/dashboard')
    await page.waitForLoadState('domcontentloaded')

    // Verify our seeded contact is visible
    await expect(page.getByRole('heading', { name: contactName })).toBeVisible()

    // No-reload sentinel: a window marker survives only if no full navigation
    // or reload happens between the mutation and the refreshed list. Its
    // survival (asserted below) proves "without a manual page reload".
    await page.evaluate(() => {
      ;(window as Window & { __dsh005NoReload?: boolean }).__dsh005NoReload = true
    })

    // Find the "Mark as Contacted" button for our contact
    const contactCard = page.locator('div.rounded-lg').filter({ hasText: contactName })
    const markContactedButton = contactCard.getByRole('button', { name: /Mark as Contacted/i })
    await expect(markContactedButton).toBeVisible()

    // Register both listeners BEFORE the click (waitForResponse must be
    // set up before the triggering action, not after).

    // The dashboard "Mark as Contacted" quick action posts to
    // POST /interactions {direction:"mutual"} (the legacy PATCH
    // /last-contacted endpoint was removed).
    const markContactedResponsePromise = page.waitForResponse(
      response =>
        response.request().method() === 'POST' &&
        response.url().includes(`/api/v1/contacts/${overdueContactId}/interactions`)
    )

    // The invalidation-driven refetch: the open dashboard re-fetches the
    // overdue list after the mutation. Content-based predicate (reads the
    // response body) so there is no ordering race against a pre-mutation
    // fetch that still contains our id.
    const overdueRefetchPromise = page.waitForResponse(async response => {
      if (
        response.request().method() !== 'GET' ||
        !response.url().includes('/api/v1/contacts/overdue') ||
        !response.ok()
      ) {
        return false
      }
      const body = await response.json().catch(() => null)
      const entries: Array<{ id: string }> = body?.data ?? []
      return !entries.some(entry => entry.id === overdueContactId)
    })

    // Click "Mark as Contacted"
    await markContactedButton.click()

    // A mutual interaction is logged: the request asks for direction=mutual
    // AND the server persists it as mutual (the response body reflects the
    // stored interaction, not just the request).
    const markContactedResponse = await markContactedResponsePromise
    expect(markContactedResponse.ok()).toBe(true)
    expect(markContactedResponse.request().postDataJSON()?.direction).toBe('mutual')
    const interactionBody = await markContactedResponse.json()
    expect(interactionBody?.data?.direction).toBe('mutual')

    // The contact leaves the overdue list without a page reload: the open
    // dashboard's own refetch no longer includes it.
    await overdueRefetchPromise

    // The count updates: the card vanishes from the live dashboard without navigation.
    await expect(page.getByRole('heading', { name: contactName })).not.toBeVisible({
      timeout: 5000,
    })

    // The no-reload sentinel survived (a reload/navigation would wipe it) and
    // we are still on the dashboard — the refresh happened in place.
    expect(
      await page.evaluate(
        () => (window as Window & { __dsh005NoReload?: boolean }).__dsh005NoReload
      )
    ).toBe(true)
    await expect(page).toHaveURL(/\/dashboard(\?|$)/)
  })
})
