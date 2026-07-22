// contacts.tour.ts — an assertion-free walk of the CURRENT contacts `ux`
// behaviors that carry judged evidence: CON-038, CON-040, CON-041, CON-042,
// CON-043, CON-044, CON-045, plus CON-065 (the visible "Back to list" control),
// whose captures feed the CON-051 experience-intent judge. CON-046 is
// status:proposed → SKIPPED; CON-066 (a list-context change resets pagination)
// is proven deterministically by cited E2E specs, not judged here.
// Read-only captures run first, then the mutating ones on DISTINCT,
// API-selected contacts, destructive-last.
//
// Imports ONLY `test` from the fixtures — never `expect` — so the tour stays
// assertion-free. Readiness is via waitForApi / locator.waitFor / waitForURL,
// never expect().

import { test } from './support/tour-fixtures'

interface TourContact {
  id: string
  full_name: string
  cadence: string | null
  location: string | null
  birthday: string | null
}

// The default list context (contact-list-params: cadence, desc). Every list /
// detail URL carries it so the nav order matches the list order (CON-038).
const LIST_URL = '/contacts?sort=cadence&order=desc'
const detailUrl = (id: string, action?: 'edit' | 'merge'): string =>
  `/contacts/${id}?sort=cadence&order=desc${action ? `&action=${action}` : ''}`

// Backend API paths (matched by waitForApi against /api/v1 response URLs).
const CONTACT_ID_PATH = /\/api\/v1\/contacts\/[0-9a-f-]{36}$/
const CONTACTS_LIST_PATH = /\/api\/v1\/contacts$/
// Frontend route path (matched by waitForURL against page URLs).
const DETAIL_PAGE_PATH = /^\/contacts\/[0-9a-f-]{36}$/

test('contacts tour — current ux behaviors', async ({ page, tour }) => {
  test.setTimeout(480_000)

  // --- Reserve distinct contacts up front, by API query, not list position ---
  // limit MUST cover the merge selector's candidate universe (useContacts({ limit: 500 })
  // in merge-contact-modal.tsx). uniqueName below is only sound when computed over the
  // SAME set the selector shows: a smaller window lets a duplicate full_name that lives
  // outside it pass the guard, then surface twice in the selector — breaking CON-043's
  // exact-text option click with a strict-mode violation.
  const listResp = await tour.apiCtx.get('/api/v1/contacts?limit=500&sort=cadence&order=desc')
  const contacts = ((await listResp.json())?.data ?? []) as TourContact[]
  if (contacts.length < 5) {
    throw new Error(`tour: prod-shaped seed too small (${contacts.length} contacts) for the tour`)
  }

  const nameCount = new Map<string, number>()
  for (const c of contacts) nameCount.set(c.full_name, (nameCount.get(c.full_name) ?? 0) + 1)
  const uniqueName = (c: TourContact): boolean => nameCount.get(c.full_name) === 1

  // markContact = the first cadence-desc row (guaranteed on list page 1).
  const markContact = contacts[0]
  const reserved = new Set<string>([markContact.id])

  // CON-043 REQUIRES a cadence-differing pair with unique names (so the
  // client-side selector filter is unambiguous). Throw if none — a tour error
  // is signal, not a silent evidence-less capture.
  let target: TourContact | undefined
  let source: TourContact | undefined
  for (let i = 0; i < contacts.length && !target; i++) {
    const a = contacts[i]
    if (reserved.has(a.id) || !a.cadence || !uniqueName(a)) continue
    for (let j = i + 1; j < contacts.length; j++) {
      const b = contacts[j]
      if (reserved.has(b.id) || !b.cadence || !uniqueName(b)) continue
      if (a.cadence !== b.cadence) {
        target = a
        source = b
        break
      }
    }
  }
  if (!target || !source) {
    throw new Error('tour: CON-043 requires a cadence-differing, unique-named pair; seed lacks one')
  }
  reserved.add(target.id)
  reserved.add(source.id)

  // CON-065 walks a searched + cadence-filtered list, so it needs a distinct
  // contact with a cadence (survives cadence_filter=has_cadence) and a unique
  // name (the search resolves to its row unambiguously). Never mutated below.
  const navContact = contacts.find(c => !reserved.has(c.id) && c.cadence && uniqueName(c))
  if (!navContact) {
    throw new Error('tour: CON-065 needs a distinct cadence-bearing, unique-named contact')
  }
  reserved.add(navContact.id)

  const deleteContact = contacts.find(c => !reserved.has(c.id))
  if (!deleteContact) throw new Error('tour: no distinct contact left for the delete step')
  reserved.add(deleteContact.id)
  const actionParamContact = contacts.find(c => !reserved.has(c.id)) ?? markContact

  // Mid-list + first contact for keyboard-nav (read-only; order = default nav).
  const firstId = contacts[0].id
  const midId = contacts[1].id
  // The true LAST contact in the ids_only nav order (the list may exceed page 1,
  // so read the full navigation id list, not the limit=100 page).
  const idsResp = await tour.apiCtx.get('/api/v1/contacts?ids_only=true&sort=cadence&order=desc')
  const navIds = ((await idsResp.json())?.data?.ids ?? []) as string[]
  const lastId = navIds[navIds.length - 1]
  if (!lastId) throw new Error('tour: no ids_only navigation order for the last-boundary capture')

  // The merge modal overlay — used to root the aria snapshot on the modal for
  // its captures, so its subtree is not truncated out of the content-rich body.
  const mergeModal = page
    .locator('div.fixed.inset-0')
    .filter({ has: page.getByRole('heading', { name: 'Merge Contacts' }) })

  // =====================================================================
  // READ-ONLY BEHAVIORS FIRST
  // =====================================================================

  // --- CON-038: list + detail share the default cadence order ---
  await page.goto(LIST_URL)
  await tour.waitForApi(page, 'GET', CONTACTS_LIST_PATH)
  await page.getByRole('heading', { name: 'Contacts', level: 2 }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CON-038'],
    note: 'contact list, default cadence order (no explicit sort)',
    pair: { id: 'default-order-CON-038', role: 'list' },
  })

  await page.locator('tbody tr').first().getByRole('link').first().click()
  await page.waitForURL(u => DETAIL_PAGE_PATH.test(new URL(u).pathname))
  await tour.waitForApi(page, 'GET', CONTACT_ID_PATH) // detail contact
  await tour.waitForApi(page, 'GET', CONTACTS_LIST_PATH) // ids_only nav order
  await page.getByRole('button', { name: 'Edit' }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CON-038'],
    note: 'contact detail opened from default list; prev/next nav bar',
    pair: { id: 'default-order-CON-038', role: 'detail' },
  })

  // Bare /contacts (NO explicit sort param) — proves the IMPLICIT default is
  // cadence order (most frequent first), the half the explicit-sort list cannot.
  await page.goto('/contacts')
  await tour.waitForApi(page, 'GET', CONTACTS_LIST_PATH)
  await page.getByRole('heading', { name: 'Contacts', level: 2 }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CON-038'],
    note: 'bare contact list (no explicit sort): implicit default cadence order',
    pair: { id: 'default-order-CON-038', role: 'list-bare' },
  })

  // --- CON-065: a visible "Back to list" control restores list context ---
  // A NON-default search + cadence filter make the preserved context observable.
  // The round trip is list → detail (open a row) → "Back to list" (by mouse):
  // the return lands on the SAME searched + filtered list, at parity with the
  // keyboard Escape route (CON-040). Placed early so both captures survive the
  // CON-051 intent capture cap (see intent-catalog captureCap).
  const navSearch = navContact.full_name
  const filteredListUrl =
    `/contacts?sort=cadence&order=desc&search=${encodeURIComponent(navSearch)}` +
    '&cadence_filter=has_cadence'
  await page.goto(filteredListUrl)
  await tour.waitForApi(page, 'GET', CONTACTS_LIST_PATH)
  await page.getByRole('heading', { name: 'Contacts', level: 2 }).waitFor({ state: 'visible' })
  const navRow = page
    .locator('tbody tr')
    .filter({ has: page.locator(`a[href*="/contacts/${navContact.id}"]`) })
  await navRow.first().getByRole('link').first().click()
  await page.waitForURL(u => DETAIL_PAGE_PATH.test(new URL(u).pathname))
  await tour.waitForApi(page, 'GET', CONTACT_ID_PATH)
  await tour.waitForApi(page, 'GET', CONTACTS_LIST_PATH) // ids_only nav order
  await page.getByRole('button', { name: 'Back to list' }).waitFor({ state: 'visible' })
  // Gate the capture on the pager RESOLVING to "N of M", not just the button.
  // waitForApi resolves on response headers, and the button + heading are static,
  // so without this the ARIA snapshot can catch the pager mid-load as "No contacts"
  // — contradictory evidence (see the post-nav capture-race gotcha in core.md).
  await page
    .getByText(/\d+ of \d+/)
    .first()
    .waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CON-065'],
    note: 'detail opened from a searched + cadence-filtered list: nav bar shows a visible "Back to list" control; the URL carries the non-default search + filter context',
    pair: { id: 'back-to-list-CON-065', role: 'from-detail' },
  })

  await page.getByRole('button', { name: 'Back to list' }).click()
  await page.waitForURL(u => {
    const url = new URL(u)
    return url.pathname === '/contacts' && url.searchParams.get('search') === navSearch
  })
  await tour.waitForApi(page, 'GET', CONTACTS_LIST_PATH)
  await page.getByRole('heading', { name: 'Contacts', level: 2 }).waitFor({ state: 'visible' })
  // Gate on the restored row rendering, not the static heading — this is the
  // observable consequence that the filtered list re-committed after Back, so
  // the capture never snapshots the list skeleton mid-load.
  await navRow.first().waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CON-065'],
    note: 'clicking "Back to list" returns to the SAME searched + cadence-filtered list — search still populated, cadence filter still applied: context preserved by mouse, at parity with Escape',
    pair: { id: 'back-to-list-CON-065', role: 'back-to-list' },
  })

  // --- CON-045: birthdays grouped by proximity under accelerated time ---
  // Readiness = rendered cards (the cache-warm accelerated frame), NOT a fresh
  // system/time GET (which would deadlock on the warm 5m-stale cache).
  await page.goto('/birthdays')
  await tour.waitForApi(page, 'GET', CONTACTS_LIST_PATH) // the limit=1000 load
  await page.getByTestId('birthday-card').first().waitFor({ state: 'visible', timeout: 20_000 })
  // DECISION 1 (capture weight): the con045 verifier's birthday ground-truth
  // ([0] grouping / [1] gift candidates / [3] placeholder names) needs only
  // { full_name, birthday } per birthday-bearing contact — NOT the full
  // 1000-contact body, whose per-contact bloat dominated the committed capture's
  // git weight. Record that compact projection as a field and let the API body
  // fall back to the default array cap. The aria (ariaCap: Infinity) stays the
  // source for section headings / order / per-card age / frame date.
  const birthdayResp = await tour.apiCtx.get('/api/v1/contacts?limit=1000')
  const allContacts = ((await birthdayResp.json())?.data ?? []) as TourContact[]
  const birthdayContacts = allContacts
    .filter(c => c.birthday)
    .map(c => ({ full_name: c.full_name, birthday: c.birthday }))
  await tour.capture(page, {
    behaviors: ['CON-045'],
    note: 'birthdays page, rendered accelerated frame (all cards + birthday projection)',
    ariaCap: Infinity, // preserve ALL visible birthday cards (a display behavior)
    fields: { birthdayContacts },
  })

  // --- CON-041: one-shot ?action= param strip ---
  await page.goto(detailUrl(actionParamContact.id, 'edit'))
  await page.waitForURL(u => !new URL(u).search.includes('action='))
  await tour.waitForApi(page, 'GET', CONTACT_ID_PATH)
  await page.getByRole('heading', { name: 'Edit Contact' }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CON-041'],
    note: 'action=edit consumed once and stripped from URL',
  })
  await page.getByRole('heading', { name: 'Edit Contact' }).click() // blur, focus → body
  await page.keyboard.press('Escape') // discard edit without saving
  await page.getByRole('button', { name: 'Edit' }).waitFor({ state: 'visible' })

  await page.goto(detailUrl(actionParamContact.id, 'merge'))
  await page.waitForURL(u => !new URL(u).search.includes('action='))
  await page.getByRole('heading', { name: 'Merge Contacts' }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CON-041'],
    note: 'action=merge consumed once and stripped from URL',
    ariaRoot: mergeModal,
  })
  await page.keyboard.press('Escape') // close merge modal without merging
  await page.getByRole('heading', { name: 'Merge Contacts' }).waitFor({ state: 'hidden' })

  // --- CON-040: keyboard navigation drives the detail page ---
  // The whole keyboard sequence is one bracket the grader diffs by seq: the
  // nav/inert then-items are proven by comparing consecutive captures' urls.
  const navPair = 'keyboard-nav-CON-040'
  await gotoDetailReady(midId)
  await tour.capture(page, {
    behaviors: ['CON-040'],
    note: 'detail before keyboard nav (view mode)',
    pair: { id: navPair, role: 'view-before' },
  })

  let beforePath = new URL(page.url()).pathname
  await page.keyboard.press('ArrowRight')
  await page.waitForURL(u => new URL(u).pathname !== beforePath)
  await page.getByRole('button', { name: 'Edit' }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CON-040'],
    note: 'ArrowRight moved to next contact',
    pair: { id: navPair, role: 'arrow-right-next' },
  })

  beforePath = new URL(page.url()).pathname
  await page.keyboard.press('ArrowLeft')
  await page.waitForURL(u => new URL(u).pathname !== beforePath)
  await page.getByRole('button', { name: 'Edit' }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CON-040'],
    note: 'ArrowLeft moved to previous contact',
    pair: { id: navPair, role: 'arrow-left-prev' },
  })

  // Boundary: first contact → Previous nav disabled (aria [disabled]).
  await gotoDetailReady(firstId)
  await tour.capture(page, {
    behaviors: ['CON-040'],
    note: 'boundary: first contact, Previous nav disabled',
    pair: { id: navPair, role: 'boundary-first' },
  })

  // Input-focus inertness (view mode, unconfounded from edit): the Add-Task
  // modal does NOT set isEditing, so nav stays enabled; the input-focus guard
  // alone suppresses the arrow.
  await page.getByRole('button', { name: 'Add' }).click()
  await page.getByRole('heading', { name: /Add Task for/ }).waitFor({ state: 'visible' })
  await page
    .getByPlaceholder('Follow up about surgery next tuesday p2')
    .waitFor({ state: 'visible' }) // auto-focused on mount
  await page.keyboard.press('ArrowRight')
  await page.getByRole('heading', { name: /Add Task for/ }).waitFor({ state: 'visible' }) // no nav
  await tour.capture(page, {
    behaviors: ['CON-040'],
    note: 'focus in Add-Task input (view mode): ArrowRight does NOT navigate — input-focus guard',
    pair: { id: navPair, role: 'input-focus-inert' },
  })
  await page.keyboard.press('Escape')
  await page.getByRole('heading', { name: /Add Task for/ }).waitFor({ state: 'hidden' })

  // Boundary: last contact → Next nav disabled (the other half of CON-040[0]).
  // Placed after the input-focus capture so it does not move the page off the
  // first contact that input-focus-inert's url is diffed against.
  await gotoDetailReady(lastId)
  await tour.capture(page, {
    behaviors: ['CON-040'],
    note: 'boundary: last contact, Next nav disabled',
    pair: { id: navPair, role: 'boundary-last' },
  })

  // Enter opens edit mode (view swaps to ContactForm).
  await page.getByRole('heading', { name: 'Contact Information' }).click() // focus → body
  await page.keyboard.press('Enter')
  await page.getByRole('heading', { name: 'Edit Contact' }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CON-040'],
    note: 'Enter opened edit mode (ContactForm)',
    pair: { id: navPair, role: 'enter-edit' },
  })

  // Arrows inert while editing — evidence is the UNCHANGED url (nav is also
  // present-but-disabled in edit mode; the url is the load-bearing proof).
  await page.getByRole('heading', { name: 'Edit Contact' }).click() // focus → body
  await page.keyboard.press('ArrowRight')
  await page.getByRole('heading', { name: 'Edit Contact' }).waitFor({ state: 'visible' }) // still editing
  await tour.capture(page, {
    behaviors: ['CON-040'],
    note: 'ArrowRight in edit mode: url unchanged — arrows inert while editing',
    pair: { id: navPair, role: 'arrow-edit-inert' },
  })

  // Escape discards edit, back to view mode.
  await page.getByRole('heading', { name: 'Edit Contact' }).click() // focus → body
  await page.keyboard.press('Escape')
  await page.getByRole('button', { name: 'Edit' }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CON-040'],
    note: 'Escape discards edit, back to view mode',
    pair: { id: navPair, role: 'escape-discard' },
  })

  // Escape from view returns to the list (context preserved).
  await gotoDetailReady(midId)
  await page.keyboard.press('Escape')
  await page.waitForURL(u => new URL(u).pathname === '/contacts') // strict pathname
  await tour.waitForApi(page, 'GET', CONTACTS_LIST_PATH)
  await tour.capture(page, {
    behaviors: ['CON-040'],
    note: 'Escape from view returns to the list, context preserved',
    pair: { id: navPair, role: 'escape-to-list' },
  })

  // =====================================================================
  // MUTATING BEHAVIORS (distinct contacts, destructive-last)
  // =====================================================================

  // --- CON-044: mark-as-contacted logs a mutual interaction from the list ---
  await page.goto(LIST_URL)
  await tour.waitForApi(page, 'GET', CONTACTS_LIST_PATH)
  await page.getByRole('heading', { name: 'Contacts', level: 2 }).waitFor({ state: 'visible' })
  const markRow = page
    .locator('tbody tr')
    .filter({ has: page.locator(`a[href*="/contacts/${markContact.id}"]`) })
  await markRow.first().waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CON-044'],
    note: 'before mark-as-contacted (list row activity state)',
    pair: { id: 'markcontacted-CON-044', role: 'before' },
  })

  await markRow.getByRole('button', { name: 'Contact actions' }).click()
  await page.getByRole('menuitem', { name: 'Mark as Contacted' }).click()
  await tour.waitForApi(
    page,
    'POST',
    new RegExp(`/api/v1/contacts/${markContact.id}/interactions$`)
  )
  await tour.capture(page, {
    behaviors: ['CON-044'],
    note: 'after mark-as-contacted: mutual interaction logged (server-timestamped)',
    pair: { id: 'markcontacted-CON-044', role: 'after' },
  })

  // --- CON-043: the merge flow keeps the current contact, archives the source ---
  // The modal-open captures root aria on `mergeModal` (defined above) so the
  // modal's queryable evidence (submit disabled, conflict toggles, summary,
  // name affordances) survives rather than being truncated behind the page.
  await page.goto(detailUrl(target.id))
  await tour.waitForApi(page, 'GET', CONTACT_ID_PATH)
  await page.getByRole('button', { name: 'Merge' }).waitFor({ state: 'visible' })
  await page.getByRole('button', { name: 'Merge' }).click()
  await page.getByRole('heading', { name: 'Merge Contacts' }).waitFor({ state: 'visible' })
  await tour.waitForApi(page, 'GET', CONTACTS_LIST_PATH) // selector's limit=500 load
  await tour.capture(page, {
    behaviors: ['CON-043'],
    note: 'merge modal open, no source: submit disabled',
    pair: { id: 'merge-CON-043', role: 'open' },
    ariaRoot: mergeModal,
  })

  // Binding selector-exclusion: typing the TARGET's own name yields no target
  // candidate (excluded upstream), despite the query matching it.
  await mergeModal.getByText('Search for a contact to merge...').click()
  const selectorInput = mergeModal.getByPlaceholder('Search for a contact to merge...')
  await selectorInput.waitFor({ state: 'visible' })
  await selectorInput.fill(target.full_name)
  await tour.capture(page, {
    behaviors: ['CON-043'],
    note: 'selector filtered by TARGET name: target absent from candidates = selector excludes target',
    pair: { id: 'merge-CON-043', role: 'selector-open' },
    ariaRoot: mergeModal,
  })

  // Select the source; hold the preview response to capture the loading state.
  await selectorInput.fill(source.full_name)
  const sourceOption = mergeModal.getByText(source.full_name, { exact: true })
  await sourceOption.waitFor({ state: 'visible' })
  const previewHold = await tour.holdRoute(page, u => u.pathname.endsWith('/merge/preview'))
  await sourceOption.click()
  await tour.capture(page, {
    behaviors: ['CON-043'],
    note: 'preview loading: submit disabled',
    pair: { id: 'merge-CON-043', role: 'preview-loading' },
    ariaRoot: mergeModal,
  })
  await previewHold.release()
  await tour.waitForApi(page, 'GET', /\/merge\/preview$/)
  await mergeModal.getByRole('heading', { name: 'Will Be Merged' }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CON-043'],
    note: 'preview loaded: transfer summary + conflict toggle(s) for the actually-conflicting field(s)',
    pair: { id: 'merge-CON-043', role: 'preview-loaded' },
    ariaRoot: mergeModal,
  })

  // Name quick-fill (renders only when not editing and source_name !== edited).
  await mergeModal.getByRole('button', { name: 'use this' }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CON-043'],
    note: 'quick-fill affordance visible (merged name not yet source)',
    pair: { id: 'merge-CON-043', role: 'name-quickfill-available' },
    ariaRoot: mergeModal,
  })
  await mergeModal.getByRole('button', { name: 'use this' }).click()
  await tour.capture(page, {
    behaviors: ['CON-043'],
    note: 'quick-fill applied: merged name adopts source name',
    pair: { id: 'merge-CON-043', role: 'name-quickfilled' },
    ariaRoot: mergeModal,
  })

  // Manual name edit in edit mode; the live input value is captured via fields
  // (ariaSnapshot does not reliably emit a textbox's value).
  const editedMergedName = `${source.full_name} (merged)`
  await mergeModal.getByRole('heading', { level: 3 }).click() // handleStartEditingName
  const nameInput = mergeModal.getByRole('textbox').first()
  await nameInput.waitFor({ state: 'visible' })
  await nameInput.fill(editedMergedName)
  const mergedNameValue = await nameInput.inputValue() // read while visible
  await page.keyboard.press('Enter') // confirm
  await tour.capture(page, {
    behaviors: ['CON-043'],
    note: 'merged name manually edited in edit mode',
    pair: { id: 'merge-CON-043', role: 'name-edited' },
    fields: { mergedNameInput: mergedNameValue },
    ariaRoot: mergeModal,
  })

  // Merge in flight: hold the POST to capture the disabled submit.
  const mergeHold = await tour.holdRoute(page, u => u.pathname.endsWith('/merge'))
  await mergeModal.getByRole('button', { name: 'Merge Contacts' }).click()
  await tour.capture(page, {
    behaviors: ['CON-043'],
    note: 'merge in flight: submit disabled',
    pair: { id: 'merge-CON-043', role: 'in-flight' },
    ariaRoot: mergeModal,
  })
  await mergeHold.release()
  await tour.waitForApi(page, 'POST', /\/merge$/)
  await page.getByRole('heading', { name: 'Merge Contacts' }).waitFor({ state: 'hidden' }) // closed on success
  await tour.capture(page, {
    behaviors: ['CON-043'],
    note: 'merge succeeded: POST kept target + field_selections default-to-target; probe GET source → 404 (archived)',
    pair: { id: 'merge-CON-043', role: 'after' },
    probes: [{ method: 'GET', path: `/api/v1/contacts/${source.id}` }],
  })

  // Page-level success banner, then its ~5s auto-dismiss.
  await page.getByText(/merged successfully/i).waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CON-043'],
    note: 'success banner shown (page-level mergeMessage)',
    pair: { id: 'merge-CON-043', role: 'outcome-reported' },
  })
  await page.getByText(/merged successfully/i).waitFor({ state: 'hidden', timeout: 10_000 })
  await tour.capture(page, {
    behaviors: ['CON-043'],
    note: 'success banner auto-dismissed after ~5s',
    pair: { id: 'merge-CON-043', role: 'dismissed' },
  })

  // --- CON-042: deleting a contact requires explicit confirmation ---
  // Dismiss bracket then accept bracket on the SAME contact.
  await page.goto(detailUrl(deleteContact.id))
  await tour.waitForApi(page, 'GET', CONTACT_ID_PATH)
  await page.getByRole('button', { name: 'Delete' }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CON-042'],
    note: 'detail before delete (dismiss path)',
    pair: { id: 'delete-CON-042', role: 'before' },
  })

  await tour.withDialog(page, { accept: false }, async () => {
    await page.getByRole('button', { name: 'Delete' }).click()
  })
  await tour.capture(page, {
    behaviors: ['CON-042'],
    note: 'after dismiss: still on detail, contact NOT deleted (probe GET 200)',
    pair: { id: 'delete-CON-042', role: 'after-dismiss' },
    probes: [{ method: 'GET', path: `/api/v1/contacts/${deleteContact.id}` }],
  })

  await tour.withDialog(page, { accept: true }, async () => {
    await page.getByRole('button', { name: 'Delete' }).click()
  })
  await page.waitForURL(u => new URL(u).pathname === '/contacts') // strict pathname
  await tour.waitForApi(page, 'GET', CONTACTS_LIST_PATH)
  await tour.capture(page, {
    behaviors: ['CON-042'],
    note: 'after accept: returned to list; DELETE 204 + probe GET 404 confirm deletion',
    pair: { id: 'delete-CON-042', role: 'after-accept' },
    probes: [{ method: 'GET', path: `/api/v1/contacts/${deleteContact.id}` }],
  })

  // Open a fresh detail page (view mode) and wait for both the contact + nav ids.
  async function gotoDetailReady(id: string): Promise<void> {
    await page.goto(detailUrl(id))
    await tour.waitForApi(page, 'GET', new RegExp(`/api/v1/contacts/${id}$`))
    await tour.waitForApi(page, 'GET', CONTACTS_LIST_PATH) // ids_only nav order
    await page.getByRole('button', { name: 'Edit' }).waitFor({ state: 'visible' })
  }
})
