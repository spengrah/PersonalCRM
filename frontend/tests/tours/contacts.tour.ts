// contacts.tour.ts — an assertion-free walk of the 7 CURRENT contacts `ux`
// behaviors (CON-038, CON-040, CON-041, CON-042, CON-043, CON-044, CON-045).
// CON-046 is status:proposed → SKIPPED. Read-only captures run first, then the
// mutating ones on DISTINCT, API-selected contacts, destructive-last (D7).
//
// Imports ONLY `test` from the fixtures — never `expect` — so the tour stays
// assertion-free (arc §3). Readiness is via waitForApi / locator.waitFor /
// waitForURL (D9), never expect().

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

test('contacts tour — 7 current ux behaviors', async ({ page, tour }) => {
  test.setTimeout(480_000)

  // --- Reserve distinct contacts up front, by API query, not list position ---
  const listResp = await tour.apiCtx.get('/api/v1/contacts?limit=100&sort=cadence&order=desc')
  const contacts = ((await listResp.json())?.data ?? []) as TourContact[]
  if (contacts.length < 4) {
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
  // is signal (arc §7), not a silent evidence-less capture.
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

  const deleteContact = contacts.find(c => !reserved.has(c.id))
  if (!deleteContact) throw new Error('tour: no distinct contact left for the delete step')
  reserved.add(deleteContact.id)
  const actionParamContact = contacts.find(c => !reserved.has(c.id)) ?? markContact

  // Mid-list + first contact for keyboard-nav (read-only; order = default nav).
  const firstId = contacts[0].id
  const midId = contacts[1].id

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
  })

  await page.locator('tbody tr').first().getByRole('link').first().click()
  await page.waitForURL(u => DETAIL_PAGE_PATH.test(new URL(u).pathname))
  await tour.waitForApi(page, 'GET', CONTACT_ID_PATH) // detail contact
  await tour.waitForApi(page, 'GET', CONTACTS_LIST_PATH) // ids_only nav order
  await page.getByRole('button', { name: 'Edit' }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CON-038'],
    note: 'contact detail opened from default list; prev/next nav bar',
  })

  // --- CON-045: birthdays grouped by proximity under accelerated time ---
  // Readiness = rendered cards (the cache-warm accelerated frame), NOT a fresh
  // system/time GET (which would deadlock on the warm 5m-stale cache).
  await page.goto('/birthdays')
  await tour.waitForApi(page, 'GET', CONTACTS_LIST_PATH) // the limit=1000 load
  await page.getByTestId('birthday-card').first().waitFor({ state: 'visible', timeout: 20_000 })
  await tour.capture(page, {
    behaviors: ['CON-045'],
    note: 'birthdays page, rendered accelerated frame (full list + all cards)',
    arrayCap: Infinity, // preserve the FULL limit=1000 list (grouping/order/placeholder-year)
    ariaCap: Infinity, // preserve ALL visible birthday cards (a display behavior)
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
  await gotoDetailReady(midId)
  await tour.capture(page, {
    behaviors: ['CON-040'],
    note: 'detail before keyboard nav (view mode)',
  })

  let beforePath = new URL(page.url()).pathname
  await page.keyboard.press('ArrowRight')
  await page.waitForURL(u => new URL(u).pathname !== beforePath)
  await page.getByRole('button', { name: 'Edit' }).waitFor({ state: 'visible' })
  await tour.capture(page, { behaviors: ['CON-040'], note: 'ArrowRight moved to next contact' })

  beforePath = new URL(page.url()).pathname
  await page.keyboard.press('ArrowLeft')
  await page.waitForURL(u => new URL(u).pathname !== beforePath)
  await page.getByRole('button', { name: 'Edit' }).waitFor({ state: 'visible' })
  await tour.capture(page, { behaviors: ['CON-040'], note: 'ArrowLeft moved to previous contact' })

  // Boundary: first contact → Previous nav disabled (aria [disabled]).
  await gotoDetailReady(firstId)
  await tour.capture(page, {
    behaviors: ['CON-040'],
    note: 'boundary: first contact, Previous nav disabled',
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
  })
  await page.keyboard.press('Escape')
  await page.getByRole('heading', { name: /Add Task for/ }).waitFor({ state: 'hidden' })

  // Enter opens edit mode (view swaps to ContactForm).
  await page.getByRole('heading', { name: 'Contact Information' }).click() // focus → body
  await page.keyboard.press('Enter')
  await page.getByRole('heading', { name: 'Edit Contact' }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CON-040'],
    note: 'Enter opened edit mode (ContactForm)',
  })

  // Arrows inert while editing — evidence is the UNCHANGED url (nav is also
  // present-but-disabled in edit mode; the url is the load-bearing proof).
  await page.getByRole('heading', { name: 'Edit Contact' }).click() // focus → body
  await page.keyboard.press('ArrowRight')
  await page.getByRole('heading', { name: 'Edit Contact' }).waitFor({ state: 'visible' }) // still editing
  await tour.capture(page, {
    behaviors: ['CON-040'],
    note: 'ArrowRight in edit mode: url unchanged — arrows inert while editing',
  })

  // Escape discards edit, back to view mode.
  await page.getByRole('heading', { name: 'Edit Contact' }).click() // focus → body
  await page.keyboard.press('Escape')
  await page.getByRole('button', { name: 'Edit' }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CON-040'],
    note: 'Escape discards edit, back to view mode',
  })

  // Escape from view returns to the list (context preserved).
  await gotoDetailReady(midId)
  await page.keyboard.press('Escape')
  await page.waitForURL(u => new URL(u).pathname === '/contacts') // strict pathname
  await tour.waitForApi(page, 'GET', CONTACTS_LIST_PATH)
  await tour.capture(page, {
    behaviors: ['CON-040'],
    note: 'Escape from view returns to the list, context preserved',
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
