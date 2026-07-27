// dashboard.tour.ts — an assertion-free walk of the dashboard `ux` behaviors
// (DSH-001/002/003/004/005/007) plus the dashboard-hosted overdue behaviors
// (CAD-026/027/028). DSH-006/009 are status:proposed → SKIPPED.
//
// Imports ONLY `test` from the fixtures — never `expect` — so the tour stays
// assertion-free. Aria-invisible visual state (loading skeletons, active-nav
// mark, nav stickiness, urgency tier + card order) is recorded via targeted
// `fields` reads (D2a). Route interception makes the
// loading / error / caught-up overdue states deterministic (no seed luck).

import { test } from './support/tour-fixtures'
import { assertOverdueFitsCapture, OVERDUE_CAPTURE_CAP } from './support/pinned-fixtures'
import type { Page } from '@playwright/test'

interface OverdueRow {
  id: string
  full_name: string
  last_contacted: string | null
}

const OVERDUE_MATCH = (u: URL): boolean => u.pathname.endsWith('/contacts/overdue')
const OVERDUE_PATH = /\/contacts\/overdue$/ // for waitForApi (matches the response pathname)

// Read the rendered overdue cards in DOM order with the DOM/CSS-only bits the
// aria tree cannot express: the urgency tier is a color class, and last_contacted
// shows only as a relative-time string (enriched here from the overdue body).
// Enrichment keys on each card's contact ID (from its View-details href) — NOT
// the name, which is not unique in the seed and would scramble last_contacted.
async function readOverdueCards(
  page: Page,
  lastContactedById: Map<string, string | null>
): Promise<
  Array<{
    name: string
    daysOverdue: number | null
    tierClass: string
    lastContacted: string | null
  }>
> {
  const raw = await page.evaluate(() => {
    const out: Array<{ name: string; daysOverdue: number | null; className: string; id: string }> =
      []
    document.querySelectorAll('h3.text-lg.font-semibold').forEach(h3 => {
      const card = h3.closest('[role="listitem"]')
      const name = (h3.textContent || '').trim()
      const m = (card?.textContent || '').match(/(\d+)\s+days?\s+overdue/)
      const href = card?.querySelector('a[href*="/contacts/"]')?.getAttribute('href') || ''
      const id = href.match(/\/contacts\/([0-9a-f-]{36})/)?.[1] || ''
      out.push({
        name,
        daysOverdue: m ? Number(m[1]) : null,
        className: card ? card.className : '',
        id,
      })
    })
    return out
  })
  return raw.map(c => ({
    name: c.name,
    daysOverdue: c.daysOverdue,
    tierClass: c.className,
    lastContacted: lastContactedById.get(c.id) ?? null,
  }))
}

test('dashboard tour — DSH + dashboard-hosted CAD behaviors', async ({ page, tour }) => {
  test.setTimeout(480_000)

  // --- Reserve the overdue set (by API) for CAD-026/027 tier + order evidence ---
  //
  // Read BEFORE the first capture, and EVERY capture in this tour carries
  // OVERDUE_CAPTURE_CAP. The landing `page.goto('/')` below redirects to the
  // dashboard, whose widget fires its own parameterless overdue GET; that
  // page-context response is buffered and drained by whichever capture runs next,
  // so the overdue array reaches captures as a drained response body long before
  // the first one that names it in `fields`. A cap check placed after those
  // captures would be checking evidence that had already been sliced.
  const overdueResp = await tour.apiCtx.get('/api/v1/contacts/overdue')
  const overdueRows = ((await overdueResp.json())?.data ?? []) as OverdueRow[]
  if (overdueRows.length === 0) {
    throw new Error('dashboard tour: no overdue contacts in the seed — cannot tour CAD-026/027/028')
  }
  assertOverdueFitsCapture(overdueRows.length, 'dashboard')
  const lastContactedById = new Map<string, string | null>(
    overdueRows.map(r => [r.id, r.last_contacted ?? null])
  )

  // --- DSH-001: the dashboard is the default landing surface ---------------
  await page.goto('/')
  // Best-effort mid-redirect capture: the in-flight state (and its screenshot)
  // is evidence for the DSH-011 intent ("the dashboard never dead-ends"), bound
  // via DSH-001's serves edge — the retired DSH-001 clause that graded this
  // frame directly is gone, but the intent judge still weighs interim surfaces
  // holistically. The client redirect may already have fired — the capture
  // records whatever state was caught.
  // Best-effort in both senses: the OBSERVATION is opportunistic and the
  // CAPTURE itself must not break the sweep if the client redirect races the
  // aria snapshot (the reviewed redirect is a soft router.push, but the
  // evidence is optional either way — a missed in-flight capture is not a
  // defect).
  try {
    await tour.capture(page, {
      arrayCap: OVERDUE_CAPTURE_CAP,
      behaviors: ['DSH-001'],
      note: 'mid-redirect state at the app root (best-effort; may already show the destination)',
      pair: { id: 'redirect', role: 'inflight' },
    })
  } catch {
    // Swallowed by design — see above.
  }
  await page.waitForURL(u => new URL(u).pathname === '/dashboard')
  await page.getByRole('heading', { name: 'Action Required' }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    arrayCap: OVERDUE_CAPTURE_CAP,
    behaviors: ['DSH-001'],
    note: 'app root redirected to the dashboard (default landing)',
    pair: { id: 'landing', role: 'landing' },
  })

  // --- DSH-002 / DSH-003 / DSH-007: global nav + header CTA + no dashboard search ---
  const activeNavClass =
    (await page.getByRole('link', { name: 'Dashboard' }).first().getAttribute('class')) ?? ''
  const navPosition = await page
    .locator('nav')
    .first()
    .evaluate(el => getComputedStyle(el).position)
  await tour.capture(page, {
    arrayCap: OVERDUE_CAPTURE_CAP,
    behaviors: ['DSH-002', 'DSH-003', 'DSH-007'],
    note: 'dashboard shell: global nav, header Add Contact, no dashboard search',
    pair: { id: 'dashboard', role: 'dashboard' },
    fields: { activeNavClass, navPosition },
  })

  // --- DSH-007.contact-text-search-provided: search lives on the contact list ---
  await page.goto('/contacts')
  await page.getByPlaceholder('Search contacts...').waitFor({ state: 'visible' })
  await tour.capture(page, {
    arrayCap: OVERDUE_CAPTURE_CAP,
    behaviors: ['DSH-007'],
    note: 'contact list search input (contact text search)',
    pair: { id: 'contacts-search', role: 'contacts-search' },
  })

  // --- CAD-026 / CAD-027.urgency-default-orders-most: overdue cards + urgency (default) order ---
  await page.goto('/dashboard')
  await tour.waitForApi(page, 'GET', OVERDUE_PATH)
  await page
    .getByRole('button', { name: 'Mark as Contacted' })
    .first()
    .waitFor({ state: 'visible' })
  await tour.capture(page, {
    arrayCap: OVERDUE_CAPTURE_CAP,
    behaviors: ['CAD-026', 'CAD-027'],
    note: 'overdue list, urgency (default) sort: cards + count + tiers',
    pair: { id: 'sort', role: 'sort-urgency' },
    fields: { overdueCards: await readOverdueCards(page, lastContactedById) },
  })

  // --- CAD-027.name-orders-alphabetically: name order ----------------------
  await page.getByRole('button', { name: 'Name' }).click()
  await tour.capture(page, {
    arrayCap: OVERDUE_CAPTURE_CAP,
    behaviors: ['CAD-027'],
    note: 'overdue list, name sort (alphabetical)',
    pair: { id: 'sort', role: 'sort-name' },
    fields: { overdueCards: await readOverdueCards(page, lastContactedById) },
  })

  // --- CAD-027.recency-orders-longest-waiting: last-contacted order --------
  await page.getByRole('button', { name: 'Last Contacted' }).click()
  await tour.capture(page, {
    arrayCap: OVERDUE_CAPTURE_CAP,
    behaviors: ['CAD-027'],
    note: 'overdue list, recency sort (longest wait first; never-connected ranked by added date)',
    pair: { id: 'sort', role: 'sort-last-contacted' },
    fields: { overdueCards: await readOverdueCards(page, lastContactedById) },
  })

  // --- CAD-028 / DSH-005: mark-as-contacted on the dashboard (mutating) ----
  await page.getByRole('button', { name: 'Most Urgent' }).click() // back to default
  await tour.capture(page, {
    arrayCap: OVERDUE_CAPTURE_CAP,
    behaviors: ['CAD-028', 'DSH-005'],
    note: 'before mark-as-contacted (dashboard overdue list)',
    pair: { id: 'mark', role: 'mark-before' },
  })
  await page.getByRole('button', { name: 'Mark as Contacted' }).first().click()
  await tour.waitForApi(page, 'POST', /\/contacts\/[0-9a-f-]{36}\/interactions$/)
  await tour.waitForApi(page, 'GET', OVERDUE_PATH) // the invalidation refetch
  await tour.capture(page, {
    arrayCap: OVERDUE_CAPTURE_CAP,
    behaviors: ['CAD-028', 'DSH-005'],
    note: 'after mark-as-contacted: mutual interaction + overdue refetch (no reload)',
    pair: { id: 'mark', role: 'mark-after' },
  })

  // =====================================================================
  // ROUTE-INTERCEPTION CAPTURES LAST (deterministic loading / error / caught-up)
  // =====================================================================

  // --- DSH-004.while-loading-placeholder-content: loading placeholder (route held) ---
  const hold = await tour.holdRoute(page, OVERDUE_MATCH)
  await page.goto('/dashboard')
  await page.locator('.max-w-7xl [class*="animate-pulse"]').first().waitFor({ state: 'visible' })
  const overdueLoadingSkeletons = await page.locator('.max-w-7xl [class*="animate-pulse"]').count()
  await tour.capture(page, {
    arrayCap: OVERDUE_CAPTURE_CAP,
    behaviors: ['DSH-004'],
    note: 'overdue loading state (route held): placeholder shown, not empty/caught-up',
    pair: { id: 'dsh004', role: 'loading' },
    fields: { overdueLoadingSkeletons },
  })
  await hold.release()

  // --- DSH-004.request-failure-error-state: error state (route → 500) ------
  await page.route('**/contacts/overdue', route =>
    route.fulfill({
      status: 500,
      contentType: 'application/json',
      body: JSON.stringify({ error: { message: 'overdue fetch failed' } }),
    })
  )
  await page.goto('/dashboard')
  await page.getByText('Error loading overdue contacts').waitFor({ state: 'visible' })
  await tour.capture(page, {
    arrayCap: OVERDUE_CAPTURE_CAP,
    behaviors: ['DSH-004'],
    note: 'overdue error state (500): error carries the reason, not caught-up',
    pair: { id: 'dsh004', role: 'error' },
  })

  // --- DSH-003.caught-up-offers-add-and-list + CAD-026.nothing-overdue-all-caught: caught-up state (route → empty) ---
  await page.route('**/contacts/overdue', route =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ success: true, data: [] }),
    })
  )
  await page.goto('/dashboard')
  await page.getByRole('heading', { name: /All caught up/ }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    arrayCap: OVERDUE_CAPTURE_CAP,
    behaviors: ['DSH-003', 'CAD-026'],
    note: 'caught-up empty state: add + view-list affordances',
    pair: { id: 'caughtup', role: 'caught-up' },
  })
})
