// relationship-loop.tour.ts — the app's core value loop walked as ONE
// continuous journey (the CAD-038 intent): land on the dashboard, read the
// needs-attention signal, follow it into the most urgent overdue contact,
// assess where the relationship stands, close the loop, and return to see the
// dashboard reflect the action. Every capture is tagged CAD-038 directly so
// the intent's evidence binding is explicit; per-surface behaviors are tagged
// only where a capture genuinely evidences them.
//
// One tour, one test — DELIBERATE: the uuid mapper is per-test, so the
// acted-on contact keeps a single placeholder identity from the signal card
// through the detail page and back. Splitting the journey across tours would
// hand the judge the same contact under two pseudonyms.
//
// The contact detail page offers no "Mark as Contacted" button (that
// affordance lives on the dashboard card, CAD-028, and the list row, CON-044);
// its loop-closing affordance is Log Interaction, whose direction defaults to
// mutual — the same mutual-interaction POST the mark-as-contacted quick
// actions send.
//
// Imports ONLY `test` from the fixtures — never `expect` — so the tour stays
// assertion-free. Pair roles here are unique to this journey (loop-landing,
// signal, act, ...) so the item verifiers' byRole lookups keep binding the
// per-surface tours' captures ('landing', 'sort-urgency', 'activity-*',
// 'tasks-empty', 'mark-after'), never these.

import { test } from './support/tour-fixtures'
import { assertOverdueFitsCapture, OVERDUE_CAPTURE_CAP } from './support/pinned-fixtures'
import type { Page } from '@playwright/test'

interface OverdueRow {
  id: string
  full_name: string
}

const OVERDUE_PATH = /\/contacts\/overdue$/ // for waitForApi (response pathname)
const DETAIL_PAGE_PATH = /^\/contacts\/[0-9a-f-]{36}$/ // frontend route (waitForURL)

// The rendered overdue cards in DOM order with the DOM/CSS-only bits the aria
// tree cannot express: the urgency tier is a color class, and the card's
// contact id (from its View-details href) is the identity anchor the judge
// needs to see the acted-on contact leave the list (names are not guaranteed
// unique in a seeded world). Ids inside fields are uuid-mapped by the normalizer,
// so they match the detail url's placeholder.
async function readOverdueCards(
  page: Page
): Promise<Array<{ id: string; name: string; daysOverdue: number | null; tierClass: string }>> {
  return page.evaluate(() => {
    const out: Array<{ id: string; name: string; daysOverdue: number | null; tierClass: string }> =
      []
    document.querySelectorAll('h3.text-lg.font-semibold').forEach(h3 => {
      const card = h3.closest('[role="listitem"]')
      const name = (h3.textContent || '').trim()
      const m = (card?.textContent || '').match(/(\d+)\s+days?\s+overdue/)
      const href = card?.querySelector('a[href*="/contacts/"]')?.getAttribute('href') || ''
      const id = href.match(/\/contacts\/([0-9a-f-]{36})/)?.[1] || ''
      out.push({
        id,
        name,
        daysOverdue: m ? Number(m[1]) : null,
        tierClass: card ? card.className : '',
      })
    })
    return out
  })
}

test('relationship-loop tour — dashboard signal → contact → act → reflected', async ({
  page,
  tour,
}) => {
  test.setTimeout(480_000)

  // A world with nobody overdue is a seeding problem, not a state to tour —
  // the journey has no signal to follow. Fail loudly. Read BEFORE the first
  // capture, because the landing capture already drains the dashboard's overdue
  // GET into its evidence: a cap check that ran afterwards would be checking a
  // capture that had already been truncated. Every capture in this tour carries
  // OVERDUE_CAPTURE_CAP for the same reason — the overdue array reaches them
  // both as a `fields` value and as a drained response body.
  const overdueResp = await tour.apiCtx.get('/api/v1/contacts/overdue')
  const overdueRows = ((await overdueResp.json())?.data ?? []) as OverdueRow[]
  if (overdueRows.length === 0) {
    throw new Error(
      'relationship-loop tour: no overdue contacts in the seed — the loop has no signal to follow'
    )
  }
  assertOverdueFitsCapture(overdueRows.length, 'relationship-loop')

  // --- 1. Land: the app opens on the dashboard (DSH-001) ---
  // Arm the overdue-fetch listener BEFORE navigating: the dashboard issues its
  // overdue GET once, during landing, and the step-1 landing capture() below
  // drains the response buffer before step 2's readiness gate — so a post-hoc
  // waitForApi(OVERDUE_PATH) would wait for a second GET the page never issues
  // (deterministic on slower ARM tenants, gh #707). Awaited at step 2.
  const overdueLoaded = tour.expectApi(page, 'GET', OVERDUE_PATH, { timeout: 30_000 })
  await page.goto('/')
  await page.waitForURL(u => new URL(u).pathname === '/dashboard')
  await page.getByRole('heading', { name: 'Action Required' }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    arrayCap: OVERDUE_CAPTURE_CAP,
    behaviors: ['DSH-001', 'CAD-038'],
    note: 'journey start: app root landed on the dashboard',
    pair: { id: 'relationship-loop', role: 'loop-landing' },
  })

  // --- 2. Read the needs-attention signal (CAD-026) ---
  await overdueLoaded // the landing overdue GET (armed pre-nav; race-safe, gh #707)
  await page
    .getByRole('button', { name: 'Mark as Contacted' })
    .first()
    .waitFor({ state: 'visible' })
  const cardsBefore = await readOverdueCards(page)

  // The journey target: the FIRST card under the default most-urgent sort.
  // Picked from the widget itself (its View-details href), not deep-linked —
  // the point is that the user gets to the contact from the signal.
  const detailLink = page.getByRole('link', { name: 'View details' }).first()
  const detailHref = (await detailLink.getAttribute('href')) ?? ''
  const targetId = detailHref.match(/\/contacts\/([0-9a-f-]{36})/)?.[1] ?? ''
  if (!targetId) {
    throw new Error('relationship-loop tour: the most-urgent overdue card has no View-details link')
  }
  await tour.capture(page, {
    arrayCap: OVERDUE_CAPTURE_CAP,
    behaviors: ['CAD-026', 'CAD-038'],
    note: 'needs-attention signal read: overdue cards + count (default most-urgent sort)',
    pair: { id: 'relationship-loop', role: 'signal' },
    fields: {
      overdueCountBefore: cardsBefore.length,
      overdueCards: cardsBefore,
      targetContactId: targetId,
    },
  })

  // --- 3. Follow the signal into the contact (CAD-029/CAD-030 surfaces) ---
  await detailLink.click()
  await page.waitForURL(u => DETAIL_PAGE_PATH.test(new URL(u).pathname))
  await tour.waitForApi(page, 'GET', new RegExp(`/api/v1/contacts/${targetId}$`))
  await tour.waitForApi(page, 'GET', new RegExp(`/api/v1/contacts/${targetId}/tasks$`))
  await page.getByRole('button', { name: 'Log Interaction' }).waitFor({ state: 'visible' })
  await page.getByRole('heading', { name: 'Tasks', level: 3 }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    arrayCap: OVERDUE_CAPTURE_CAP,
    behaviors: ['CAD-029', 'CAD-030', 'CAD-038'],
    note: 'contact detail reached from the signal: recent activity, cadence, queued tasks',
    pair: { id: 'relationship-loop', role: 'detail-before' },
  })

  // --- 4. Act: the detail page's loop-closing affordance ---
  const logModal = page
    .locator('div.fixed.inset-0')
    .filter({ has: page.getByRole('heading', { name: /Log Interaction with/ }) })
  await page.getByRole('button', { name: 'Log Interaction' }).click()
  await page.getByRole('heading', { name: /Log Interaction with/ }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    arrayCap: OVERDUE_CAPTURE_CAP,
    behaviors: ['CAD-038'],
    note: 'log-interaction modal open: direction defaults to mutual, date to today',
    pair: { id: 'relationship-loop', role: 'act' },
    ariaRoot: logModal,
  })

  // Submit with the defaults (mutual, server-stamped occurred_at) — the same
  // mutual interaction the mark-as-contacted quick actions record.
  await logModal.getByRole('button', { name: 'Log', exact: true }).click()
  await tour.waitForApi(page, 'POST', new RegExp(`/api/v1/contacts/${targetId}/interactions$`))
  await page.getByRole('heading', { name: /Log Interaction with/ }).waitFor({ state: 'hidden' })
  // interaction:created invalidates the contact detail — the refetch is the
  // page reflecting the action without a reload.
  await tour.waitForApi(page, 'GET', new RegExp(`/api/v1/contacts/${targetId}$`))
  await tour.capture(page, {
    arrayCap: OVERDUE_CAPTURE_CAP,
    behaviors: ['CAD-029', 'CAD-038'],
    note: 'detail after the action: mutual interaction POSTed, relationship state refetched in place',
    pair: { id: 'relationship-loop', role: 'detail-after' },
  })

  // --- 5. Return: does the surface that sent us reflect the action? ---
  // Client-side nav (the global nav link, not a fresh page load) so the
  // refetch is invalidation-driven, not a reload.
  await page.getByRole('link', { name: 'Dashboard' }).first().click()
  await page.waitForURL(u => new URL(u).pathname === '/dashboard')
  await tour.waitForApi(page, 'GET', OVERDUE_PATH)
  await page
    .getByRole('button', { name: 'Mark as Contacted' })
    .or(page.getByRole('heading', { name: /All caught up/ }))
    .first()
    .waitFor({ state: 'visible' })
  // Wait for the OBSERVABLE CONSEQUENCE — the acted-on contact leaving the widget —
  // rather than for the refetch alone. The response landing and the DOM re-rendering
  // are different moments: reading the cards straight after `waitForApi` can catch the
  // pre-render tick and record a stale list, which is indistinguishable from the app
  // genuinely failing to update. That ambiguity would make the judge confabulate.
  //
  // Bounded, and deliberately NOT throwing: if the contact never leaves, that is a real
  // regression and the judge must SEE it, not have the tour die before capturing it.
  // `reflectedWithinMs` says which world we are in.
  const settleStart = Date.now()
  let reflected = false
  try {
    await page.waitForFunction(
      id => !document.querySelector(`a[href="/contacts/${id}"]`),
      targetId,
      { timeout: 10_000 }
    )
    reflected = true
  } catch {
    reflected = false // still listed after the wait — the loop did not visibly close
  }
  const reflectedWithinMs = Date.now() - settleStart

  const cardsAfter = await readOverdueCards(page)
  await tour.capture(page, {
    arrayCap: OVERDUE_CAPTURE_CAP,
    behaviors: ['DSH-005', 'CAD-038'],
    note: 'back on the dashboard: did the widget drop the contact we just acted on?',
    pair: { id: 'relationship-loop', role: 'loop-return' },
    fields: {
      overdueCountAfter: cardsAfter.length,
      overdueCards: cardsAfter,
      targetContactId: targetId,
      targetStillListed: !reflected,
      reflectedWithinMs: reflected ? reflectedWithinMs : null,
    },
  })
})
