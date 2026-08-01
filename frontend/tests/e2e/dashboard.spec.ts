import { test, expect } from '@playwright/test'
import { createTestAPI, TestAPI, type SeedBehaviorResult } from './helpers/test-api'
import { expectAddContactHeader, waitForOverdueListSettled } from './helpers/dashboard'
import type { OverdueContactResponse } from '../../src/types/generated/contact'

// Full-envelope overdue-entry builder for route-mocked dashboard tests,
// typed against the real wire DTO so fixture drift fails tsc. The real
// response OMITS empty optional fields (json omitempty) rather than sending
// null, so last_contacted / methods are only included when supplied.
function overdueEntry(over: {
  name: string
  days: number
  lastContacted?: string
  createdAt?: string
  email?: string
}): OverdueContactResponse {
  const slug = over.name.toLowerCase().replace(/ /g, '-')
  return {
    id: `mock-${slug}`,
    full_name: over.name,
    methods: over.email
      ? [{ id: `mock-method-${slug}`, type: 'email', value: over.email, is_primary: true }]
      : [],
    cadence: 'weekly',
    ...(over.lastContacted ? { last_contacted: over.lastContacted } : {}),
    contact_by: '2026-07-01T00:00:00Z',
    has_pending_followup: false,
    created_at: over.createdAt ?? '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    days_overdue: over.days,
    next_due_date: '2026-07-01T00:00:00Z',
    suggested_action: 'A quick check-in to reconnect',
  }
}

test.describe('Dashboard @area:dashboard', () => {
  test('should display dashboard with navigation @smoke', async ({ page }) => {
    // spec: DSH-001.user-taken-dashboard-default
    await page.goto('/')

    // Should redirect to dashboard (client-side redirect via useEffect)
    await expect(page).toHaveURL('/dashboard', { timeout: 10000 })

    // Wait for page to fully load
    await page.waitForLoadState('domcontentloaded')

    // The redirect target has render-settled: the main h2 heading is up.
    // (The nav links themselves are covered per-surface by the DSH-002.wide-sm-viewports-links
    // loop in navigation.spec.ts.)
    await expect(page.getByRole('heading', { name: 'Action Required', level: 2 })).toBeVisible()
  })

  test('caught-up state offers add-contact and view-list paths', async ({ page }) => {
    // spec: DSH-003.add-contact-action-always, DSH-003.caught-up-offers-add-and-list
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
    // spec: DSH-007.no-global-search-surface
    // NEGATIVE existence proof at a settled state: establish the dashboard
    // has fully rendered first, THEN assert that no plausible search-surface
    // shape is present (a not-yet-rendered page would pass these vacuously).
    // Search lives on the contact list (DSH-007.contact-text-search-provided, contacts.spec.ts).
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
  let seeded: SeedBehaviorResult

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
    // CAD-026's own declared fixture: three graded-overdue contacts. The test
    // provisions the behavior it cites, and the names come back in the
    // manifest rather than being re-derived here.
    seeded = await testApi.seedBehavior('CAD-026')
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('shows overdue contacts as cards with the count in the header', async ({ page }) => {
    // spec: CAD-026.overdue-contacts-appear-cards
    await page.goto('/dashboard')
    await page.waitForLoadState('domcontentloaded')

    // Every seeded card renders (namespace-scoped names — parallel-safe).
    //
    // Matched EXACTLY. The three names are generator-drawn, and two contacts in
    // one namespace that draw the same given+surname render as "<name>" and
    // "<name> N" — and getByRole matches an accessible name by SUBSTRING, so a
    // non-exact match on the shorter one resolves BOTH headings and fails strict
    // mode. The disambiguator lands on the later contact, so it is the earlier
    // card's name that is at risk; the whole loop is exact rather than only the
    // first two entries.
    const cardNames = ['card-a', 'card-b', 'card-c'].map(handle => seeded.entities[handle].name)
    for (const name of cardNames) {
      await expect(page.getByRole('heading', { name, exact: true })).toBeVisible()
    }

    // The header count is DERIVED from the same list the cards render from,
    // so it must EQUAL the number of rendered cards (a hard-coded or stale
    // header fails), and it must cover at least our seeded cards. The
    // absolute count is GLOBAL (other workers seed overdue contacts too),
    // so no exact-number assertion beyond the header==cards invariant —
    // both sides are read in ONE DOM pass so a concurrent re-render cannot
    // straddle the reads.
    await expect
      .poll(() =>
        page.evaluate(minimum => {
          const header = Array.from(document.querySelectorAll('p')).find(p =>
            /\d+ contacts? need your attention/.test(p.textContent ?? '')
          )
          if (!header) return 'no numeric header'
          const headerCount = Number(/(\d+)/.exec(header.textContent ?? '')?.[1])
          const cardCount = Array.from(document.querySelectorAll('button')).filter(b =>
            (b.textContent ?? '').includes('Mark as Contacted')
          ).length
          if (headerCount !== cardCount) return `${headerCount} !== ${cardCount}`
          if (headerCount < minimum) return `${headerCount} < seeded ${minimum}`
          return 'header equals cards'
        }, cardNames.length)
      )
      .toBe('header equals cards')
  })
})

test.describe('Dashboard - All Caught Up (mocked) @area:dashboard', () => {
  test('shows the all-caught-up state when nothing is overdue', async ({ page }) => {
    // spec: CAD-026.nothing-overdue-all-caught
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

test.describe('Dashboard - Card Anatomy (mocked) @area:dashboard', () => {
  // getUrgencyIndicator boundaries: <=2 low, <=7 medium, >7 high. The
  // fixture pins BOTH SIDES of each boundary — 2|3 (low→medium) and 7|8
  // (medium→high) — so shifting either threshold in either direction flips a
  // tier label and fails. Mocked rather than seeded: in testing mode a scaled
  // "day" is ~17s of wall time, so a seeded boundary value drifts across
  // tiers before the page settles.
  const tierCases = [
    { name: 'Yellow Boundary Card', days: 2, tier: 'Low urgency' },
    { name: 'Orange Lower Card', days: 3, tier: 'Medium urgency' },
    { name: 'Orange Boundary Card', days: 7, tier: 'Medium urgency' },
    { name: 'Red Tier Card', days: 8, tier: 'High urgency' },
  ]

  test('each card shows urgency tier, cadence, recency, a reachable method, and the suggested action', async ({
    page,
  }) => {
    // spec: CAD-026.each-card-shows-urgency
    await page.route('**/api/v1/contacts/overdue', route =>
      route.fulfill({
        json: {
          success: true,
          data: tierCases.map(c =>
            overdueEntry({
              name: c.name,
              days: c.days,
              lastContacted: '2026-07-01T12:00:00Z',
              email: `${c.name.toLowerCase().replace(/ /g, '-')}@example.com`,
            })
          ),
        },
      })
    )
    await page.goto('/dashboard')

    // The clause is per-card ("each card shows ..."): assert ALL FIVE
    // sub-elements on EVERY card, across all three urgency tiers. The tier
    // dot carries an accessible label naming its tier, so the label is the
    // observable signal.
    for (const c of tierCases) {
      await expect(page.getByRole('heading', { name: c.name })).toBeVisible()
      const card = page.getByRole('listitem').filter({ hasText: c.name })
      await expect(card.getByLabel(c.tier, { exact: true })).toBeVisible()
      await expect(card.getByText('(weekly cadence)')).toBeVisible()
      // Recency requires a VALUE after the label, not just the label
      // (formatOverdueRecency regressing to '' would fail the \S+ match).
      await expect(
        card.getByText(new RegExp(`${c.days} days overdue - Last connected \\S+`))
      ).toBeVisible()
      await expect(
        card.getByText(`${c.name.toLowerCase().replace(/ /g, '-')}@example.com`)
      ).toBeVisible()
      await expect(card.getByText('Email', { exact: true })).toBeVisible()
      const suggestion = await card.getByText(/💡/).textContent()
      expect(suggestion?.replace('💡', '').trim().length).toBeGreaterThan(0)
    }
  })
})

test.describe('Dashboard - Sort Orderings (mocked) @area:dashboard', () => {
  // The sort is CLIENT-SIDE over the fetched overdue list (dashboard/page.tsx
  // sortBy state — no sort= request is issued), so the observable outcome is
  // the rendered card DOM order. One full-envelope route mock feeds all three
  // orderings; the fixture is built so urgency, name, and last-contacted
  // orders are pairwise DISTINCT (a no-op or wrong sort fails), and one
  // never-connected record (last_contacted OMITTED, like the real omitempty
  // response) proves it is ranked by its created_at rather than dropped.
  const fixtureSuffix = 'Sortfix'
  // urgency (days desc):      Zulu(30), Mike(12), Alpha(3), Bravo(1)
  // name (alphabetical):      Alpha, Bravo, Mike, Zulu
  // recency (longest wait→):  Alpha(lc 01-10), Mike(added 02-01), Bravo(lc 03-01), Zulu(lc 05-01)
  //   Mike (never-connected) is deliberately placed BETWEEN two connected
  //   contacts by its created_at — a regression that pinned null-last_contacted
  //   rows to an edge would reorder Mike and fail, distinguishing "ranked by
  //   added date" (CAD-027.recency-orders-longest-waiting) from "never-connected grouped first/last".
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
    overdueEntry({ name: `Mike ${fixtureSuffix}`, days: 12, createdAt: '2026-02-01T00:00:00Z' }),
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
    // spec: CAD-027.urgency-default-orders-most
    await gotoMockedDashboard(page)
    expect(await cardOrder(page)).toEqual([
      `Zulu ${fixtureSuffix}`,
      `Mike ${fixtureSuffix}`,
      `Alpha ${fixtureSuffix}`,
      `Bravo ${fixtureSuffix}`,
    ])
  })

  test('name orders alphabetically', async ({ page }) => {
    // spec: CAD-027.name-orders-alphabetically
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

  test('recency orders longest-waiting first, ranking never-connected by added date', async ({
    page,
  }) => {
    // spec: CAD-027.recency-orders-longest-waiting
    await gotoMockedDashboard(page)
    await page.getByRole('button', { name: 'Last Contacted', exact: true }).click()
    // Mike (last_contacted omitted) is ranked by its created_at (02-01), landing
    // BETWEEN Alpha (connected 01-10) and Bravo (connected 03-01) — not pinned to
    // an edge. This ordering fails if never-connected rows are grouped first/last.
    await expect
      .poll(() => cardOrder(page))
      .toEqual([
        `Alpha ${fixtureSuffix}`,
        `Mike ${fixtureSuffix}`,
        `Bravo ${fixtureSuffix}`,
        `Zulu ${fixtureSuffix}`,
      ])
  })

  // spec: CAD-026.each-card-shows-urgency
  // The regression guard for the bug this copy exists to prevent: a contact with
  // no recorded connection must be described by when it was ADDED, never as a
  // contact that happened. Before the fix the card asserted "Last contacted
  // <creation date>" — inventing a conversation and contradicting the same
  // contact's detail page, which correctly showed no recent activity.
  test('a never-connected contact is described by when it was added, not as contact', async ({
    page,
  }) => {
    await gotoMockedDashboard(page)
    const mikeCard = page.getByRole('listitem').filter({ hasText: `Mike ${fixtureSuffix}` })
    await expect(mikeCard.getByText(/12 days overdue - Added \S+/)).toBeVisible()
    // Scoped to the recency phrasing — the card's "Mark as Contacted" action
    // legitimately contains the word.
    await expect(mikeCard.getByText(/Last (contacted|connected)/i)).toHaveCount(0)
  })
})

test.describe('Dashboard - With Seeded Data @area:dashboard @area:overdue', () => {
  let testApi: TestAPI
  let seeded: SeedBehaviorResult
  let overdueContactId: string

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)

    // DSH-005's declared fixture: the mark-contacted target plus a SENTINEL
    // that stays overdue. Without the sentinel, marking the target can drop the
    // total overdue count to zero on a clean run — the dashboard then renders
    // the caught-up prose instead of a numeric header, and the header==cards
    // invariant below has nothing to parse.
    seeded = await testApi.seedBehavior('DSH-005')
    overdueContactId = seeded.entities['refresh-target'].id
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('header add-contact action is available on a populated dashboard', async ({ page }) => {
    // spec: DSH-003.add-contact-action-always
    // Establish the POPULATED state first — the seeded overdue card must have
    // rendered (a loading or error mask would otherwise vacuously pass a
    // state-independent affordance check) — then assert the header CTA.
    // Matched EXACTLY: the target and the sentinel are both generator-drawn and
    // both render as cards here, so when the two draw the same given+surname the
    // later one renders as "<target name> N" and a substring match on the
    // target's own name resolves both headings.
    const contactName = seeded.entities['refresh-target'].name
    await page.goto('/dashboard')
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: contactName, exact: true })).toBeVisible()

    await expectAddContactHeader(page)
  })

  test('marking contact as contacted updates dashboard immediately without navigation', async ({
    page,
  }) => {
    // spec: DSH-005.overdue-list-refreshes-reflect, CAD-028.mutual-interaction-logged-timestamped, CAD-028.contact-leaves-overdue-list
    // DSH-005.overdue-list-refreshes-reflect: the on-dashboard interaction:created trigger refreshing the
    // overdue list without a manual reload. DSH-005's broader trigger coverage
    // (merge / meeting-note-resolve), the cosmetic-edit no-op, and the
    // refocus/staleTime timing were verifier-abstained and are not asserted
    // here.
    // CAD-028.mutual-interaction-logged-timestamped: the mutual interaction is logged with a server-assigned,
    // full-precision accelerated-clock timestamp. CAD-028.contact-leaves-overdue-list: the contact
    // leaves the overdue list without a reload and the header count updates.
    // CAD-028.change-consistent-across-dashboard (dashboard/list/detail consistency) is proved in
    // overdue-contact-updates.spec.ts.
    const contactName = seeded.entities['refresh-target'].name
    const sentinelName = seeded.entities['refresh-sentinel'].name

    // Navigate to dashboard
    await page.goto('/dashboard')
    await page.waitForLoadState('domcontentloaded')

    // Verify our seeded contact is visible. Exact, for the same reason the
    // card-resolution and disappearance assertions below are: the target and the
    // sentinel can draw the same name, and the sentinel then renders as the
    // target's name plus a sequence number.
    await expect(page.getByRole('heading', { name: contactName, exact: true })).toBeVisible()

    // No-reload sentinel: a window marker survives only if no full navigation
    // or reload happens between the mutation and the refreshed list. Its
    // survival (asserted below) proves "without a manual page reload".
    await page.evaluate(() => {
      ;(window as Window & { __dsh005NoReload?: boolean }).__dsh005NoReload = true
    })

    // Find the "Mark as Contacted" button for our contact. The card is resolved
    // by its NAME HEADING matched exactly, not by hasText: a substring filter
    // would keep the sentinel's card too whenever the two drawn names collide,
    // and the button lookup below would then be ambiguous.
    const contactCard = page
      .getByRole('listitem')
      .filter({ has: page.getByRole('heading', { name: contactName, exact: true }) })
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
    const overdueRefetchPromise = waitForOverdueListSettled(page, {
      absentIds: [overdueContactId],
    })

    // Click "Mark as Contacted", bracketed by wall-clock reads so the
    // server-assigned timestamp can be bounded. The E2E env runs WITHOUT
    // TIME_ACCELERATION, so the server's accelerated clock IS the wall
    // clock here (mirrors the retired verifier's optional-base rule).
    const beforeClick = Date.now()
    await markContactedButton.click()

    // A mutual interaction is logged: the request asks for direction=mutual
    // AND the server persists it as mutual (the response body reflects the
    // stored interaction, not just the request).
    const markContactedResponse = await markContactedResponsePromise
    const afterResponse = Date.now()
    expect(markContactedResponse.ok()).toBe(true)
    expect(markContactedResponse.request().postDataJSON()?.direction).toBe('mutual')
    const interactionBody = await markContactedResponse.json()
    expect(interactionBody?.data?.direction).toBe('mutual')

    // The timestamp is SERVER-assigned: the client omits occurred_at (the
    // backend stamps accelerated.GetCurrentTime()), and the stored stamp
    // lands inside the click bracket with full sub-second precision (not
    // a midnight date-only value).
    expect(markContactedResponse.request().postDataJSON()).not.toHaveProperty('occurred_at')
    const occurredAt: string = interactionBody?.data?.occurred_at
    const occurredAtMs = Date.parse(occurredAt)
    expect(occurredAtMs).toBeGreaterThanOrEqual(beforeClick - 1000)
    expect(occurredAtMs).toBeLessThanOrEqual(afterResponse + 1000)
    expect(occurredAt).not.toMatch(/T00:00:00(\.0+)?Z$/)
    expect(occurredAt).toMatch(/T\d{2}:\d{2}:\d{2}\.\d+Z$/)

    // The contact leaves the overdue list without a page reload: the open
    // dashboard's own refetch no longer includes it.
    await overdueRefetchPromise

    // The card vanishes from the live dashboard without navigation. Exact again,
    // and here exactness is what makes the negative meaningful rather than
    // weaker: on a name collision the sentinel's heading CONTAINS the target's
    // name, so a substring match would still resolve it and the assertion would
    // fail for a reason unrelated to the claim. The negative is not vacuous
    // either — the same heading was asserted VISIBLE above, and the sentinel's
    // card is asserted still visible below, so the list is proven to be
    // rendering cards at this point.
    await expect(page.getByRole('heading', { name: contactName, exact: true })).not.toBeVisible({
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

    // The count updates: the header count is re-derived from the refetched
    // list, so it must EQUAL the number of rendered overdue cards (a stale
    // header would still show the pre-mutation number). The absolute count
    // is GLOBAL (parallel workers seed/mark concurrently), so assert the
    // header==cards invariant — which holds regardless of other workers'
    // data — never an exact decrement. The sentinel keeps the header
    // numeric (zero overdue renders caught-up prose instead). Header and
    // card count are read in ONE DOM pass so a concurrent re-render cannot
    // straddle the two reads.
    // Matched EXACTLY, like every other name assertion in this file: a
    // drawn-name collision inside the world renders "<name>" and "<name> N",
    // and a substring match would resolve both cards.
    await expect(page.getByRole('heading', { name: sentinelName, exact: true })).toBeVisible()
    await expect
      .poll(() =>
        page.evaluate(() => {
          const header = Array.from(document.querySelectorAll('p')).find(p =>
            /\d+ contacts? need your attention/.test(p.textContent ?? '')
          )
          if (!header) return 'no numeric header'
          const headerCount = Number(/(\d+)/.exec(header.textContent ?? '')?.[1])
          const cardCount = Array.from(document.querySelectorAll('button')).filter(b =>
            (b.textContent ?? '').includes('Mark as Contacted')
          ).length
          return headerCount === cardCount
            ? 'header equals cards'
            : `${headerCount} !== ${cardCount}`
        })
      )
      .toBe('header equals cards')
  })
})
