import { test, expect, type Page } from '@playwright/test'
import { createTestAPI, declaredWorldSearch, TestAPI } from './helpers/test-api'

// The Notes row in the contact-detail definition list, scoped so a
// presence/absence assertion on its expand control cannot collide with another
// section's. Same locator notepad.spec.ts uses.
const notesRow = (page: Page) =>
  page.locator('dl > div').filter({ has: page.getByText('Notes', { exact: true }) })

// API configuration for direct backend assertions in E2E tests.
const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

test.describe('Contacts - TestAPI Seeded @area:contacts', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  // visual-guard: descender clipping is an already-bitten UI bug (leading-7 +
  // truncate clips y/g/j/p/q — see the core.md gotcha) with no data or aria
  // surface; the class assertion is the deliberate, budgeted exception to the
  // no-CSS-assertion rule (CON visual-guard 1/2).
  test('should use leading-normal on contact name to prevent descender clipping', async ({
    page,
  }) => {
    // Rides CON-042's fixture, whose contact declares the DESCENDER name edge —
    // so the name under the heading actually carries g/y/p/q rather than
    // whatever the name generator happened to draw.
    const seeded = await testApi.seedBehavior('CON-042')
    const contactId = seeded.entities['target'].id
    const fullName = seeded.entities['target'].name

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')

    // Verify the heading is visible and has leading-normal class (not leading-7)
    const heading = page.getByRole('heading', { name: fullName })
    await expect(heading).toBeVisible()
    await expect(heading).toHaveClass(/leading-normal/)
    await expect(heading).not.toHaveClass(/leading-7/)
  })

  // spec: NTS-007.long-content-clamped-expand
  test('should show expandable notes for long content', async ({ page }) => {
    // Create notes longer than 300 characters to trigger truncation
    const longNotes = `Met at the AI conference in San Francisco, March 2024. Works as a senior ML engineer at a startup focused on personal productivity tools.

Very interested in personal CRM concepts and AI-driven contact management. We discussed potential collaboration opportunities around embedding-based contact matching.

Key interests: Machine learning infrastructure, personal knowledge management, privacy-focused software design.

Follow-up: Share the pgvector article, introduce to Sarah from the embeddings team.`

    const seeded = await testApi.seedBehavior('NTS-007')
    const contactId = seeded.entities['target'].id
    const fullName = seeded.entities['target'].name
    // The note body is the claim under test, so it is written here rather than
    // hoisted into the fixture — the declared contact deliberately starts with
    // no note at all.
    await testApi.seedContactNote(contactId, longNotes)

    await page.goto(`/contacts/${contactId}`)
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Wait for notes to render before checking overflow detection
    await expect(page.getByText('Met at the AI conference')).toBeVisible({ timeout: 5000 })

    // The expand control exists only while the page's client-side overflow
    // measurement is true. Scoped to the Notes ROW so it cannot resolve against
    // another section's control, and so a failure names this row.
    const showMoreButton = notesRow(page).getByRole('button', { name: 'Show more' })
    await expect(showMoreButton).toBeVisible({ timeout: 5000 })

    // Click "Show more" to expand
    await showMoreButton.click()

    // Verify button changed to "Show less"
    const showLessButton = notesRow(page).getByRole('button', { name: 'Show less' })
    await expect(showLessButton).toBeVisible()

    // Click "Show less" to collapse
    await showLessButton.click()

    // Verify button changed back to "Show more"
    await expect(showMoreButton).toBeVisible()
  })

  // spec: NTS-007.long-content-clamped-expand
  test('shows the expand control when the note request finishes before the contact', async ({
    page,
  }) => {
    // A REGRESSION test for a real ordering bug, not a duplicate of the test
    // above. The Notes row renders only inside the block the detail page shows
    // once the CONTACT has loaded, so when the note request finishes first the
    // overflow measurement has no element yet. An implementation that keys that
    // measurement on the note body alone never re-measures once the row appears,
    // and the expand control stays hidden over content that plainly overflows.
    //
    // The natural ordering is whichever request happens to win, so the test above
    // covers this only by luck. Here the ordering is FORCED: the contact-detail
    // response is held at the network layer until the note response has come back.
    const longNote = [
      'First paragraph with enough background to push the body past the clamp.',
      'Second paragraph adding more detail so the rendered height clearly exceeds four lines.',
      'Third paragraph continuing with still more detail about shared projects.',
      'Fourth paragraph covering preferences, interests and recent conversations.',
    ].join('\n\n')

    // How long the contact response is held after the note response lands.
    const contactHoldAfterNoteMs = 750

    const seeded = await testApi.seedBehavior('NTS-007')
    const contactId = seeded.entities['target'].id
    const fullName = seeded.entities['target'].name
    await testApi.seedContactNote(contactId, longNote)

    let releaseContact = () => {}
    const contactGate = new Promise<void>(resolve => {
      releaseContact = resolve
    })
    let noteCompleted = false

    // Held: the contact detail. Its glob has no trailing wildcard, so it cannot
    // also match the /notes or /tasks paths.
    //
    // The extra pause after the gate opens is load-bearing. Releasing the contact
    // the instant the note lands puts both responses in ONE React commit, and the
    // Notes row then exists on the very first render that has the note — which is
    // the ordering the buggy implementation also survives. The pause guarantees at
    // least one committed render with the note present and the contact still
    // loading, which is the state under test.
    await page.route(`**/api/v1/contacts/${contactId}`, async route => {
      if (route.request().method() !== 'GET') return route.continue()
      await contactGate
      await new Promise(resolve => setTimeout(resolve, contactHoldAfterNoteMs))
      return route.continue()
    })
    // The releaser: fetch the real note response, mark it done, then let the
    // contact through. Registered second so it cannot be shadowed.
    await page.route(`**/api/v1/contacts/${contactId}/notes`, async route => {
      if (route.request().method() !== 'GET') return route.continue()
      const response = await route.fetch()
      noteCompleted = true
      releaseContact()
      return route.fulfill({ response })
    })

    await page.goto(`/contacts/${contactId}`)
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })
    await expect(notesRow(page)).toContainText('First paragraph')

    // The ordering actually held — otherwise this test would be asserting the
    // same thing the previous one does, under a different name.
    expect(noteCompleted).toBe(true)

    // The claim: the expand control is present even though the note won the race.
    await expect(notesRow(page).getByRole('button', { name: 'Show more' })).toBeVisible()
  })

  // spec: NTS-007.long-content-clamped-expand
  test('should not show expand button for short notes', async ({ page }) => {
    const shortNotes = 'Brief note about this contact.'

    const seeded = await testApi.seedBehavior('NTS-007')
    const contactId = seeded.entities['target'].id
    const fullName = seeded.entities['target'].name
    await testApi.seedContactNote(contactId, shortNotes)

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Verify notes are displayed
    await expect(page.getByText(shortNotes)).toBeVisible()

    // Verify "Show more" button is NOT visible (notes are short)
    await expect(notesRow(page).getByRole('button', { name: 'Show more' })).not.toBeVisible()
  })

  // spec: CON-053.optional-backdated-date, CON-053.interaction-posted-chosen-direction
  test('should log a backdated interaction via the Log Interaction modal', async ({ page }) => {
    // The Log Interaction modal (direction picker + date picker)
    // replaces the previous inline pencil-edit on `last_contacted`.
    // The modal posts to POST /contacts/:id/interactions; the backend
    // applies cadence math from the chosen direction. This test
    // exercises the happy path (mutual + a backdated date).
    const seeded = await testApi.seedBehavior('CON-053')
    const contactId = seeded.entities['target'].id
    const fullName = seeded.entities['target'].name

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Open the modal via the header button.
    await page.getByRole('button', { name: 'Log Interaction' }).click()
    await expect(page.getByRole('dialog')).toBeVisible()

    // Pick a backdated date and submit. We assert via the API response
    // because cadence math + accelerated time can otherwise make
    // direct row-text assertions racy.
    const dateInput = page.getByTestId('log-interaction-date-input')
    await dateInput.fill('2024-01-15')

    const responsePromise = page.waitForResponse(
      resp =>
        resp.url().includes(`/api/v1/contacts/${contactId}/interactions`) &&
        resp.request().method() === 'POST'
    )
    await page.getByRole('button', { name: 'Log', exact: true }).click()
    const response = await responsePromise
    expect(response.status()).toBe(201)

    // The BACKDATED date travels: the request carries the chosen date (the
    // modal pins it to UTC midnight) and the stored interaction echoes it.
    expect(response.request().postDataJSON()?.occurred_at).toBe('2024-01-15T00:00:00.000Z')
    const body = await response.json()
    expect(body.success).toBe(true)
    expect(body.data.occurred_at).toContain('2024-01-15')

    // Modal closes on success.
    await expect(page.getByRole('dialog')).not.toBeVisible({ timeout: 5000 })
  })

  // visual-guard: the context menu clipping on bottom rows is an already-bitten
  // UI bug (dropdown rendered inside an overflow container was cut off) with no
  // data or aria surface; the menuitem-visibility proof on the LAST row is the
  // deliberate, budgeted exception (CON visual-guard 2/2).
  test('should show context menu without clipping for bottom rows', async ({ page }) => {
    // Rides CON-065's fixture: 21 cadence-bearing contacts under zero-padded
    // PINNED names, so under an explicit name-ascending sort the identity of the
    // last row on page 1 is known before the data is seeded — a strictly
    // stronger anchor than counting whatever the list happened to return.
    const seeded = await testApi.seedBehavior('CON-065')
    const lastRowName = seeded.entities['p20'].name

    // Scope to this world by search AND pin the sort: without an explicit sort
    // the search falls back to relevance order, and the last row of page 1 is
    // then not knowable in advance.
    await page.goto(
      `/contacts?search=${encodeURIComponent(declaredWorldSearch(seeded))}&sort=name&order=asc`
    )
    await page.waitForLoadState('domcontentloaded')

    // Wait for the FILTERED page before touching last(): the unfiltered render
    // satisfies a bare visibility check immediately, so last() could still grab
    // a foreign row that vanishes when the search response lands. The known
    // page-1 tail row plus a full 20-row page == the filter and sort applied.
    const rows = page.locator('tbody tr')
    await expect(page.locator('tr', { has: page.getByText(lastRowName) })).toBeVisible({
      timeout: 15000,
    })
    await expect(rows).toHaveCount(20)
    const lastRow = rows.last()
    await expect(lastRow).toContainText(lastRowName)
    const actionButton = lastRow.getByRole('button', { name: 'Contact actions' })
    await actionButton.click()

    // Verify the dropdown menu is visible and not clipped
    const menuItem = page.getByRole('menuitem', { name: 'Mark as Contacted' })
    await expect(menuItem).toBeVisible()

    // Verify Edit and Merge items are also present
    await expect(page.getByRole('menuitem', { name: 'Edit' })).toBeVisible()
    await expect(page.getByRole('menuitem', { name: 'Merge' })).toBeVisible()
  })

  test('should navigate to edit mode via context menu Edit action', async ({ page }) => {
    // spec: CON-041.action-runs-once-edit, CON-041.parameter-stripped-from-url
    const seeded = await testApi.seedBehavior('CON-041')
    const fullName = seeded.entities['target'].name

    // The list is scoped to this world by search: the default order is cadence
    // DESCENDING, which files a cadence-less contact last, so first-page
    // visibility is not something to rely on in a shared database.
    await page.goto(`/contacts?search=${encodeURIComponent(declaredWorldSearch(seeded))}`)
    await page.waitForLoadState('domcontentloaded')

    // Find the row and open context menu
    const contactRow = page.locator('tr', { has: page.getByText(fullName) })
    await expect(contactRow).toBeVisible({ timeout: 15000 })
    const actionButton = contactRow.getByRole('button', { name: 'Contact actions' })
    await actionButton.click()

    // Click Edit in context menu
    await page.getByRole('menuitem', { name: 'Edit' }).click()

    // Should navigate to detail page in edit mode
    await page.waitForURL(/\/contacts\/.*action=edit/)
    await expect(page.getByRole('heading', { name: 'Edit Contact' })).toBeVisible({
      timeout: 15000,
    })

    // The action param is consumed once, then stripped from the URL (mount-only
    // router.replace) so a refresh does not re-trigger it.
    await expect(page).not.toHaveURL(/action=/)
  })

  test('should navigate to merge modal via context menu Merge action', async ({ page }) => {
    // spec: CON-041.action-runs-once-edit, CON-041.parameter-stripped-from-url
    const seeded = await testApi.seedBehavior('CON-041')
    const fullName = seeded.entities['target'].name

    // The list is scoped to this world by search: the default order is cadence
    // DESCENDING, which files a cadence-less contact last, so first-page
    // visibility is not something to rely on in a shared database.
    await page.goto(`/contacts?search=${encodeURIComponent(declaredWorldSearch(seeded))}`)
    await page.waitForLoadState('domcontentloaded')

    // Find the row and open context menu
    const contactRow = page.locator('tr', { has: page.getByText(fullName) })
    await expect(contactRow).toBeVisible({ timeout: 15000 })
    const actionButton = contactRow.getByRole('button', { name: 'Contact actions' })
    await actionButton.click()

    // Click Merge in context menu
    await page.getByRole('menuitem', { name: 'Merge' }).click()

    // Should navigate to detail page with merge modal open
    await page.waitForURL(/\/contacts\/.*action=merge/)
    await expect(page.getByRole('heading', { name: 'Merge Contacts' })).toBeVisible({
      timeout: 15000,
    })

    // The action param is consumed once, then stripped from the URL (mount-only
    // router.replace) so a refresh does not re-trigger it.
    await expect(page).not.toHaveURL(/action=/)
  })

  // spec: CON-053.direction-chosen-outbound-inbound, CON-053.interaction-posted-chosen-direction
  test('should log a mutual interaction via the Log Interaction modal default', async ({
    page,
  }) => {
    // The default-direction path of the Log Interaction modal, preserving the
    // old "I just talked to them" semantic. Which timestamp columns each
    // direction bumps is CAD-006 (backend-owned) — asserted by
    // internal/consumer/cadence_updater_test.go and
    // tests/api/direction_api_test.go, not re-checked through the browser.
    const seeded = await testApi.seedBehavior('CON-053')
    const contactId = seeded.entities['target'].id
    const fullName = seeded.entities['target'].name

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    const responsePromise = page.waitForResponse(
      resp =>
        resp.url().includes(`/api/v1/contacts/${contactId}/interactions`) &&
        resp.request().method() === 'POST'
    )
    await page.getByRole('button', { name: 'Log Interaction' }).click()
    await expect(page.getByRole('dialog')).toBeVisible()
    // Submit without changing the direction (default = mutual) or date.
    await page.getByRole('button', { name: 'Log', exact: true }).click()
    const response = await responsePromise
    expect(response.status()).toBe(201)
    expect((await response.json()).data.direction).toBe('mutual')

    await expect(page.getByRole('dialog')).not.toBeVisible({ timeout: 5000 })
  })

  // spec: CON-053.direction-chosen-outbound-inbound
  test('should log an outbound interaction via the Log Interaction modal', async ({ page }) => {
    // The modal's direction picker reaches the API: choosing Outbound posts
    // direction=outbound. The cadence timestamp effects of each direction are
    // CAD-006 (backend-owned, Go-covered).
    const seeded = await testApi.seedBehavior('CON-053')
    const contactId = seeded.entities['target'].id
    const fullName = seeded.entities['target'].name

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    await page.getByRole('button', { name: 'Log Interaction' }).click()
    await expect(page.getByRole('dialog')).toBeVisible()
    await page.getByRole('button', { name: 'Outbound' }).click()
    const responsePromise = page.waitForResponse(
      resp =>
        resp.url().includes(`/api/v1/contacts/${contactId}/interactions`) &&
        resp.request().method() === 'POST'
    )
    await page.getByRole('button', { name: 'Log', exact: true }).click()
    const response = await responsePromise
    expect(response.status()).toBe(201)
    const body = await response.json()
    expect(body.data.direction).toBe('outbound')
  })

  // spec: CON-053.direction-chosen-outbound-inbound
  test('should log an inbound interaction via the Log Interaction modal', async ({ page }) => {
    // The modal's direction picker reaches the API: choosing Inbound posts
    // direction=inbound. The cadence timestamp effects of each direction are
    // CAD-006 (backend-owned, Go-covered).
    const seeded = await testApi.seedBehavior('CON-053')
    const contactId = seeded.entities['target'].id
    const fullName = seeded.entities['target'].name

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    await page.getByRole('button', { name: 'Log Interaction' }).click()
    await expect(page.getByRole('dialog')).toBeVisible()
    await page.getByRole('button', { name: 'Inbound' }).click()
    const responsePromise = page.waitForResponse(
      resp =>
        resp.url().includes(`/api/v1/contacts/${contactId}/interactions`) &&
        resp.request().method() === 'POST'
    )
    await page.getByRole('button', { name: 'Log', exact: true }).click()
    const response = await responsePromise
    expect(response.status()).toBe(201)
    const body = await response.json()
    expect(body.data.direction).toBe('inbound')
  })

  test('defaults the contact list to cadence order, most-frequent-first', async ({ page }) => {
    // spec: CON-038.list-defaults-cadence-order
    // Names are chosen so ALPHABETICAL order (either direction) differs from the
    // cadence-desc order — otherwise a backend that ignored sort=cadence and fell
    // back to name order would pass this test. Cadence-desc = Yankee(weekly) →
    // Alpha(monthly) → Mike(annual); name-asc = Alpha, Mike, Yankee; name-desc =
    // Yankee, Mike, Alpha. All three orders are distinct.
    const seeded = await testApi.seedBehavior('CON-038')
    const weeklyName = seeded.entities['weekly'].name
    const monthlyName = seeded.entities['monthly'].name
    const annualName = seeded.entities['annual'].name

    // A BARE load resolves the default context: the request the app issues
    // carries the cadence-desc default, with no user having chosen a sort.
    const listRequest = page.waitForResponse(
      resp =>
        resp.request().method() === 'GET' &&
        /\/api\/v1\/contacts(\?|$)/.test(resp.url()) &&
        !new URL(resp.url()).searchParams.has('ids_only')
    )
    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')
    const params = new URL((await listRequest).url()).searchParams
    expect(['cadence', null]).toContain(params.get('sort'))
    expect(['desc', null]).toContain(params.get('order'))

    // Filter to THIS test's rows (search keeps the default sort) so parallel
    // workers' contacts cannot satisfy or break the ordering, then assert the
    // three seeded rows render most-frequent-first (weekly → monthly → annual)
    // in DOM order.
    await page.getByPlaceholder('Search contacts...').fill(declaredWorldSearch(seeded))
    await page.getByPlaceholder('Search contacts...').press('Enter')
    await expect(page.locator('tbody tr', { has: page.getByText(monthlyName) })).toBeVisible({
      timeout: 15000,
    })

    const rowText = await page.locator('tbody tr').allTextContents()
    const weeklyIdx = rowText.findIndex(t => t.includes(weeklyName))
    const monthlyIdx = rowText.findIndex(t => t.includes(monthlyName))
    const annualIdx = rowText.findIndex(t => t.includes(annualName))
    expect(weeklyIdx).toBeGreaterThanOrEqual(0)
    expect(monthlyIdx).toBeGreaterThan(weeklyIdx)
    expect(annualIdx).toBeGreaterThan(monthlyIdx)
  })

  test('deletes a contact only after confirmation, then returns to the list', async ({
    page,
    request,
  }) => {
    // spec: CON-042.confirmation-prompt-warns-action, CON-042.only-confirmation-contact-deleted, CON-042.success-user-returned-contact
    const seeded = await testApi.seedBehavior('CON-042')
    const contactId = seeded.entities['target'].id
    const fullName = seeded.entities['target'].name

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Deletion is guarded by a native window.confirm (see the detail page).
    // Dismiss path: no DELETE fires and the contact stays live.
    let deleteFired = false
    const watchDelete = (req: import('@playwright/test').Request) => {
      if (req.method() === 'DELETE' && req.url().includes(`/contacts/${contactId}`)) {
        deleteFired = true
      }
    }
    page.on('request', watchDelete)
    // The confirmation copy is captured INSIDE the handler (it cannot be read
    // after the dialog resolves) and asserted below: the prompt must warn the
    // action cannot be undone (CON-042.confirmation-prompt-warns-action).
    let confirmMessage = ''
    page.once('dialog', dialog => {
      confirmMessage = dialog.message()
      void dialog.dismiss()
    })
    await page.getByRole('button', { name: 'Delete' }).click()
    // Asserting ABSENCE: there is no positive signal to await on dismiss, so give
    // any (erroneous) DELETE a bounded settle window to appear, then confirm none
    // fired and the contact is still live.
    await page.waitForTimeout(1000)
    expect(deleteFired).toBe(false)
    expect(confirmMessage).toContain('cannot be undone')
    page.off('request', watchDelete)
    const liveResp = await request.get(`${API_BASE_URL}/api/v1/contacts/${contactId}`, {
      headers: API_HEADERS,
    })
    expect(liveResp.status()).toBe(200)

    // Accept path: the DELETE fires (204), the contact 404s, and we land on the list.
    const deleteResponse = page.waitForResponse(
      resp => resp.request().method() === 'DELETE' && resp.url().includes(`/contacts/${contactId}`)
    )
    page.once('dialog', dialog => dialog.accept())
    await page.getByRole('button', { name: 'Delete' }).click()
    const delResp = await deleteResponse
    expect(delResp.status()).toBe(204)

    // Redirected to the contact list on success (CON-042.success-user-returned-contact).
    await expect(page).toHaveURL(/\/contacts(\?|$)/)

    // The contact is now gone (CON-042.only-confirmation-contact-deleted).
    const afterResp = await request.get(`${API_BASE_URL}/api/v1/contacts/${contactId}`, {
      headers: API_HEADERS,
    })
    expect(afterResp.status()).toBe(404)
  })

  test('logs a mutual interaction from the list-row Mark as Contacted quick action', async ({
    page,
  }) => {
    // spec: CON-044.mutual-direction-interaction-logged
    // The LIST-row context-menu quick action (distinct from the detail-page Log
    // Interaction modal): it posts a mutual, server-timestamped interaction.
    const seeded = await testApi.seedBehavior('CON-044')
    const contactId = seeded.entities['target'].id
    const fullName = seeded.entities['target'].name

    // Scoped to this world by search: the shared database holds many other
    // cadence-bearing contacts, so the seeded row's page is not predictable.
    await page.goto(`/contacts?search=${encodeURIComponent(declaredWorldSearch(seeded))}`)
    await page.waitForLoadState('domcontentloaded')

    const row = page.locator('tr', { has: page.getByText(fullName) })
    await expect(row).toBeVisible({ timeout: 15000 })
    const actionButton = row.getByRole('button', { name: 'Contact actions' })
    await actionButton.click()

    const responsePromise = page.waitForResponse(
      resp =>
        resp.url().includes(`/api/v1/contacts/${contactId}/interactions`) &&
        resp.request().method() === 'POST'
    )
    await page.getByRole('menuitem', { name: 'Mark as Contacted' }).click()
    const response = await responsePromise
    expect(response.status()).toBe(201)

    // The client does not send occurred_at — the server assigns it from its
    // (accelerated) clock.
    const requestBody = response.request().postDataJSON() ?? {}
    expect(requestBody.occurred_at).toBeUndefined()

    const body = await response.json()
    expect(body.data.direction).toBe('mutual')
    expect(body.data.occurred_at).toBeTruthy()
  })
})

test.describe('Contacts - Cadence Filter @area:contacts', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should filter contacts by cadence status', async ({ page }) => {
    // spec: DSH-007.contact-text-search-provided, CON-054.has-cadence-no-cadence, CON-054.clearing-filter-restores-unfiltered
    // Contact text search is provided through the contact list's search
    // input: the tightened search step below proves typing a term drives a
    // `search=` list request that FILTERS the results (the matching fixtures
    // render, a seeded non-matching one does not) — not merely that an input
    // exists.
    // The declared fixture is three contacts sharing a surname — weekly, monthly
    // and cadence-less — plus a fourth that shares neither the surname nor a
    // cadence, which is the row the TEXT search has to remove.
    const seeded = await testApi.seedBehavior('CON-054')
    const weekly = seeded.entities['weekly'].name
    const monthly = seeded.entities['monthly'].name
    const noCadence = seeded.entities['none'].name
    const unrelated = seeded.entities['unrelated'].name

    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')

    // Two-phase search. Phase 1 searches the whole declared world (matches ALL
    // four fixtures) and establishes the non-matching contact IS reachable via
    // search on this page — without this, its later absence could vacuously
    // pass (e.g. an ignored filter leaving it on page 2 of the shared DB).
    // Phase 2 appends the SURNAME the three cadence fixtures share, and asserts
    // the fourth row disappears: only an applied text filter can remove a row
    // that phase 1 proved present. The literal below mirrors the surname pinned
    // in CON-054's declaration; it has to be a standalone word, because the
    // list search is full-text and ANDs its terms.
    const worldTerm = declaredWorldSearch(seeded)
    const searchTerm = `${worldTerm} Cadfilter`
    const searchInput = page.getByPlaceholder('Search contacts...')
    const listResponseFor = (term: string) =>
      page.waitForResponse(
        resp =>
          resp.request().method() === 'GET' &&
          resp.url().includes('/api/v1/contacts') &&
          new URL(resp.url()).searchParams.get('search') === term &&
          !new URL(resp.url()).searchParams.has('ids_only')
      )

    // Each response listener is registered BEFORE .fill() (a param-carrying
    // request can fire before a listener added after the fill) and requires the
    // EXACT search term.
    const worldResponse = listResponseFor(worldTerm)
    await searchInput.fill(worldTerm)
    await searchInput.press('Enter')
    await worldResponse
    await expect(page.getByText(weekly, { exact: true })).toBeVisible({ timeout: 10000 })
    await expect(page.getByText(unrelated, { exact: true })).toBeVisible()

    const searchResponse = listResponseFor(searchTerm)
    await searchInput.fill(searchTerm)
    await searchInput.press('Enter')
    await searchResponse
    await expect(page.getByText(weekly, { exact: true })).toBeVisible({ timeout: 10000 })
    // The search FILTERS: the non-matching contact phase 1 proved present is
    // now absent.
    await expect(page.getByText(unrelated, { exact: true })).not.toBeVisible()

    // Verify all 3 contacts visible with "All contacts" (default)
    const filterSelect = page.getByLabel('Filter by cadence')
    await expect(filterSelect).toHaveValue('')
    await expect(page.getByText(weekly, { exact: true })).toBeVisible()
    await expect(page.getByText(monthly, { exact: true })).toBeVisible()
    await expect(page.getByText(noCadence, { exact: true })).toBeVisible()

    // Select "Has cadence" - should show only contacts with cadence
    const hasCadenceResponse = page.waitForResponse(
      resp => resp.url().includes('cadence_filter=has_cadence') && resp.ok()
    )
    await filterSelect.selectOption('has_cadence')
    await hasCadenceResponse

    await expect(page.getByText(weekly, { exact: true })).toBeVisible()
    await expect(page.getByText(monthly, { exact: true })).toBeVisible()
    await expect(page.getByText(noCadence, { exact: true })).not.toBeVisible()

    // Select "No cadence" - should show only contacts without cadence
    const noCadenceResponse = page.waitForResponse(
      resp => resp.url().includes('cadence_filter=no_cadence') && resp.ok()
    )
    await filterSelect.selectOption('no_cadence')
    await noCadenceResponse

    await expect(page.getByText(weekly, { exact: true })).not.toBeVisible()
    await expect(page.getByText(monthly, { exact: true })).not.toBeVisible()
    await expect(page.getByText(noCadence, { exact: true })).toBeVisible()

    // Reset to "All contacts" - should show all again
    await filterSelect.selectOption('')

    await expect(page.getByText(weekly, { exact: true })).toBeVisible({
      timeout: 10000,
    })
    await expect(page.getByText(noCadence, { exact: true })).toBeVisible()
  })
})

test.describe('Contacts - UI Create (preserved for coverage) @area:contacts', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  // spec: CON-055.contact-created-user-lands
  test('should create a contact from the form @smoke', async ({ page }) => {
    const fullName = `${testApi.prefix}-Create Contact`

    await page.goto('/contacts/new')
    await page.getByLabel('Full Name').fill(fullName)

    await Promise.all([
      page.waitForURL(/\/contacts\/[A-Za-z0-9-]+$/),
      page.getByRole('button', { name: 'Create Contact' }).click(),
    ])

    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })
  })

  // spec: CON-055.contact-created-user-lands
  //
  // ContactForm and transformContactFormData are SHARED with the edit path, and
  // the edit path stopped sending `methods` on the contact PUT. Creation still
  // must: there is nothing yet to lose, so CreateContactRequest keeps its
  // methods field. This test passes against the pre-change tree by design — it
  // guards a behavior that must not break, and its discrimination gate is
  // mutating the shared transform to drop methods, not reverting the feature.
  test('creates a contact with a contact method', async ({ page }) => {
    const fullName = `${testApi.prefix}-Create With Method`
    const email = `create-method-${Date.now()}@example.com`

    await page.goto('/contacts/new')
    await page.getByLabel('Full Name').fill(fullName)
    await page.getByRole('combobox', { name: 'Contact method type' }).first().selectOption('email')
    await page.getByRole('textbox', { name: 'Contact method value' }).first().fill(email)

    await Promise.all([
      page.waitForURL(/\/contacts\/[A-Za-z0-9-]+$/),
      page.getByRole('button', { name: 'Create Contact' }).click(),
    ])

    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })
    await expect(page.getByText(email)).toBeVisible({ timeout: 15000 })
  })

  // spec: NTS-008.displayed-notepad-reflects-new
  test('should edit contact notes', async ({ page }) => {
    const notes =
      'Met at a conference in 2024. Works in AI/ML. Very interested in personal CRM tools.'
    const updatedNotes = 'Updated notes: Follow up about collaboration opportunity.'

    const seeded = await testApi.seedBehavior('NTS-008')
    const contactId = seeded.entities['target'].id
    const fullName = seeded.entities['target'].name

    await testApi.seedContactNote(contactId, notes)

    await page.goto(`/contacts/${contactId}`)
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })
    await expect(page.getByText(notes)).toBeVisible()

    // Edit the contact to update notes (use first() to get header Edit button, not the last contacted edit)
    await page.getByRole('button', { name: 'Edit' }).first().click()
    await expect(page.getByLabel('Notes')).toBeVisible()

    await page.getByLabel('Notes').fill(updatedNotes)

    // Submit the inline edit form
    await page.getByRole('button', { name: 'Update Contact' }).click()

    // Wait for form to close and return to detail view (Edit button visible again)
    await expect(page.getByRole('button', { name: 'Edit' }).first()).toBeVisible({ timeout: 15000 })

    // Verify updated notes are displayed
    await expect(page.getByText(updatedNotes)).toBeVisible()
    await expect(page.getByText(notes)).not.toBeVisible()
  })

  // spec: CON-056.methods-displayed-normalized, CON-056.primary-method-marked-only, CON-056.no-link-surface-plain-text
  test('should display contact with methods and the primary marked', async ({ page, request }) => {
    // A slim display proof: seeded methods render with normalized values and
    // exactly one Primary mark. The normalization RULES themselves (per-type
    // handle/phone canonicalization) are CON-012, backend-owned and covered by
    // internal/identity/normalize_test.go — one spot check here proves the
    // display path, not the rules.
    //
    // The declared fixture carries email + telegram + gchat with the primary on
    // TELEGRAM, which is the discriminating shape: email is the default primary
    // whenever a contact has one. The stored values are generator-derived, so
    // they are read back off the detail API rather than restated here.
    const seeded = await testApi.seedBehavior('CON-056')
    const contactId = seeded.entities['methods'].id
    const fullName = seeded.entities['methods'].name

    const detail = await request.get(`${API_BASE_URL}/api/v1/contacts/${contactId}`, {
      headers: API_HEADERS,
    })
    expect(detail.ok()).toBe(true)
    const methods = ((await detail.json()).data.methods ?? []) as Array<{
      type: string
      value: string
    }>
    const valueOf = (type: string) => {
      const method = methods.find(m => m.type === type)
      expect(method, `seeded contact should carry a ${type} method`).toBeTruthy()
      return method!.value
    }
    const personalEmail = valueOf('email')
    const gchatEmail = valueOf('gchat')
    // The handler strips a leading '@' from a handle before the service sees it,
    // so the STORED telegram value is bare and the '@' in the rendered string can
    // only have come from the frontend's display normalization.
    const storedTelegram = valueOf('telegram')
    expect(storedTelegram.startsWith('@')).toBe(false)
    const normalizedTelegram = `@${storedTelegram}`

    await page.goto(`/contacts/${contactId}`)
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Seeded methods display, the handle in its normalized form.
    await expect(page.getByText(personalEmail, { exact: true })).toBeVisible()
    await expect(page.getByText(normalizedTelegram, { exact: true })).toBeVisible()

    // The primary (telegram) row carries the mark, and it is the ONLY one.
    // Row scoped from the seeded value (data anchor), not the type label copy.
    const primaryRow = page.getByText(normalizedTelegram, { exact: true }).locator('..')
    await expect(primaryRow.getByText('Primary')).toBeVisible()
    await expect(page.getByText('Primary')).toHaveCount(1)

    // A gchat identifier has no external link surface: its value renders as
    // plain text with its type label, NOT as a link (a frontend-only invariant
    // of getContactMethodHref that the CON-012 Go tests do not cover).
    const gchatRow = page.getByText(gchatEmail, { exact: true }).locator('..')
    await expect(gchatRow.getByText('Google Chat', { exact: true })).toBeVisible()
    await expect(page.getByRole('link', { name: gchatEmail })).toHaveCount(0)
  })

  // spec: CON-061.cadence-renders-formatted-label, CON-061.next-contact-date-or-placeholder
  test('should render formatted cadence and next-contact values in list rows', async ({ page }) => {
    // Derived-value display: a contact with a cadence + last-contacted has a
    // computed contact_by, rendered as a date in the Next Contact column; a
    // contact without a cadence renders the '-' placeholder there, and the
    // cadence value renders as its formatted label. Column resolved by its
    // header position at runtime (no hard-coded cell index).
    const seeded = await testApi.seedBehavior('CON-061')

    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')
    const searchInput = page.getByPlaceholder('Search contacts...')
    await searchInput.fill(declaredWorldSearch(seeded))
    await searchInput.press('Enter')

    // Matched EXACTLY: the two declared names are generator-drawn, and two
    // contacts in one namespace that draw the same pair render "<name>" and
    // "<name> N", so a substring match on the shorter one would resolve both rows.
    const weeklyRow = page.locator('tr', {
      has: page.getByText(seeded.entities['with-cadence'].name, { exact: true }),
    })
    const noneRow = page.locator('tr', {
      has: page.getByText(seeded.entities['without-cadence'].name, { exact: true }),
    })
    await expect(weeklyRow).toBeVisible({ timeout: 15000 })
    await expect(noneRow).toBeVisible()

    // Formatted cadence label in the seeded row (derived from the stored
    // 'weekly' value, not raw enum text).
    await expect(weeklyRow.getByText('Weekly', { exact: true })).toBeVisible()

    // Resolve the Next Contact column by header position.
    const headerTexts = await page.getByRole('columnheader').allTextContents()
    const nextIdx = headerTexts.findIndex(t => t.includes('Next Contact'))
    expect(nextIdx).toBeGreaterThanOrEqual(0)

    // With a cadence: a real date value (contains digits). Without: '-'.
    await expect(weeklyRow.getByRole('cell').nth(nextIdx)).toHaveText(/\d/)
    await expect(noneRow.getByRole('cell').nth(nextIdx)).toHaveText('-')
  })

  // spec: CON-057.list-refetches-column-sort-field
  test('should sort by Next Contact column when header clicked', async ({ page }) => {
    // Two sequential click -> response round trips under real parallel
    // worker load can exceed the default 30s budget; give this test room.
    test.setTimeout(60000)
    const seeded = await testApi.seedBehavior('CON-057')
    const targetName = seeded.entities['target'].name

    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')

    // Search for our test contact to isolate it
    const searchInput = page.getByPlaceholder('Search contacts...')
    await searchInput.fill(declaredWorldSearch(seeded))
    await searchInput.press('Enter')
    await expect(page.getByText(targetName)).toBeVisible()

    // Click Next Contact header - verify sort=contact_by&order=asc is sent to API
    const nextContactHeader = page.getByRole('columnheader').filter({ hasText: 'Next Contact' })
    const ascResponse = page.waitForResponse(
      resp => resp.url().includes('sort=contact_by') && resp.url().includes('order=asc')
    )
    await nextContactHeader.click()
    await ascResponse
    // The response landing and the header re-rendering are different
    // moments: clicking again mid-re-render dispatches into a replaced DOM
    // node and is silently lost. Wait for the applied state first.
    await expect(nextContactHeader).toHaveAttribute('aria-sort', 'ascending')

    // Click again to toggle to descending - verify sort=contact_by&order=desc
    const descResponse = page.waitForResponse(
      resp => resp.url().includes('sort=contact_by') && resp.url().includes('order=desc')
    )
    await nextContactHeader.click()
    await descResponse
    await expect(nextContactHeader).toHaveAttribute('aria-sort', 'descending')

    // Contact should still be visible after sort toggling
    await expect(page.getByText(targetName)).toBeVisible()
  })

  // spec: CON-057.list-refetches-column-sort-field
  test('should sort by Last response column when header clicked', async ({ page }) => {
    // Two sequential click -> response round trips under real parallel
    // worker load can exceed the default 30s budget; give this test room.
    test.setTimeout(60000)
    const seeded = await testApi.seedBehavior('CON-057')
    const targetName = seeded.entities['target'].name

    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')

    const lastResponseHeader = page.getByRole('columnheader').filter({ hasText: 'Last response' })

    // Isolate our test contact via search so the sort click reliably
    // produces a fetch we can listen for.
    const searchInput = page.getByPlaceholder('Search contacts...')
    await searchInput.fill(declaredWorldSearch(seeded))
    await searchInput.press('Enter')
    await expect(page.getByText(targetName)).toBeVisible()

    // First click → desc (default direction for last_response_at, matching the
    // existing cadence-column convention of "most recent first").
    const descResponse = page.waitForResponse(
      resp => resp.url().includes('sort=last_response_at') && resp.url().includes('order=desc')
    )
    await lastResponseHeader.click()
    await descResponse
    // Settle on the applied state before toggling again — a click during
    // the post-response re-render lands on a replaced node and is lost.
    await expect(lastResponseHeader).toHaveAttribute('aria-sort', 'descending')

    // Second click → asc.
    const ascResponse = page.waitForResponse(
      resp => resp.url().includes('sort=last_response_at') && resp.url().includes('order=asc')
    )
    await lastResponseHeader.click()
    await ascResponse
    await expect(lastResponseHeader).toHaveAttribute('aria-sort', 'ascending')

    await expect(page.getByText(targetName)).toBeVisible()
  })

  // spec: CON-058.page-number-buttons-move, CON-058.previous-next-controls-disable
  test('should show page number buttons and top/bottom pagination when multiple pages exist', async ({
    page,
  }) => {
    // The declared fixture is 22 contacts, two past the twenty-row default page
    // size, so page 2 holds exactly two rows.
    const seeded = await testApi.seedBehavior('CON-058')

    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')

    // Search for our test contacts to isolate them
    const searchInput = page.getByPlaceholder('Search contacts...')
    await searchInput.fill(declaredWorldSearch(seeded))
    const searchResponse = page.waitForResponse(
      resp => resp.url().includes('/api/v1/contacts') && resp.url().includes('search=')
    )
    await searchInput.press('Enter')
    await searchResponse

    // BOTH pagination controls render (top + bottom) — this count guards the
    // sync assertion at the end from vacuously passing when first() and
    // last() resolve to the same single control.
    const paginationControls = page.locator('[data-testid="pagination"]')
    await expect(paginationControls).toHaveCount(2)

    // Page number buttons exist (at least page 1 and 2 in the top control).
    const topPagination = paginationControls.first()
    await expect(topPagination.getByRole('button', { name: '1' })).toBeVisible()
    await expect(topPagination.getByRole('button', { name: '2' })).toBeVisible()

    // Page 1 is the current page (aria-current marks the active page button),
    // and Previous is disabled at the near boundary.
    const page1Button = topPagination.getByRole('button', { name: '1' })
    await expect(page1Button).toHaveAttribute('aria-current', 'page')
    await expect(topPagination.getByRole('button', { name: 'Previous' })).toBeDisabled()

    // Click page 2: a page=2 list request fires (the control PAGES the list,
    // not just restyles itself) and the current-page mark moves.
    const page2Response = page.waitForResponse(
      resp =>
        resp.request().method() === 'GET' &&
        resp.url().includes('/api/v1/contacts') &&
        new URL(resp.url()).searchParams.get('page') === '2' &&
        !new URL(resp.url()).searchParams.has('ids_only')
    )
    await topPagination.getByRole('button', { name: '2' }).click()
    await page2Response
    await expect(topPagination.getByRole('button', { name: '2' })).toHaveAttribute(
      'aria-current',
      'page'
    )
    // The rows changed: 22 seeded contacts at limit 20 leave 2 on page 2.
    await expect(page.locator('tbody tr')).toHaveCount(2)

    // Verify Previous is now enabled and Next is disabled (only 2 pages)
    await expect(topPagination.getByRole('button', { name: 'Previous' })).toBeEnabled()
    await expect(topPagination.getByRole('button', { name: 'Next' })).toBeDisabled()

    // Click Previous to go back to page 1
    await topPagination.getByRole('button', { name: 'Previous' }).click()
    await expect(topPagination.getByRole('button', { name: '1' })).toHaveAttribute(
      'aria-current',
      'page'
    )

    // Both paginations stay in sync (bottom also shows page 1 as current)
    const bottomPagination = paginationControls.last()
    await expect(bottomPagination.getByRole('button', { name: '1' })).toHaveAttribute(
      'aria-current',
      'page'
    )
  })
})
