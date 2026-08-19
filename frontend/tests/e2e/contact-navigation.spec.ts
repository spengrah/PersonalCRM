import { test, expect, type Page } from '@playwright/test'
import {
  createTestAPI,
  declaredWorldSearch,
  TestAPI,
  type SeedBehaviorResult,
} from './helpers/test-api'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

test.describe('Contact Keyboard Navigation @area:contact-navigation', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should disable keyboard navigation in edit mode', async ({ page }) => {
    // spec: CON-040.arrows-inert-while-editing
    const seeded = await testApi.seedBehavior('CON-040')
    const contactA = seeded.entities['a']
    const fullNameA = contactA.name

    // Go to the list first, scoped to this world so the seeded row is on the
    // first page whatever else the shared database holds, then open the contact.
    await page.goto(`/contacts?search=${encodeURIComponent(declaredWorldSearch(seeded))}`)
    await page.waitForLoadState('domcontentloaded')
    await page.getByText(fullNameA).click()
    await page.waitForURL(/\/contacts\/[A-Za-z0-9-]+/)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullNameA })).toBeVisible({ timeout: 15000 })

    // Establish that nav is READY before edit mode — otherwise the disabled
    // assertion below could pass merely because the id list is still loading.
    await expect(page.getByText(/\d+ of \d+/)).toBeVisible({ timeout: 10000 })
    const nextButton = page.getByRole('button', { name: 'Next contact' })
    await expect(nextButton).toBeEnabled()

    // "Back to list" is enabled on the read view (disabled only in edit mode).
    const backButton = page.getByRole('button', { name: 'Back to list' })
    await expect(backButton).toBeEnabled()

    // Enter edit mode
    await page.getByRole('button', { name: 'Edit' }).first().click()
    await page.waitForLoadState('domcontentloaded')

    // Now the navigation buttons are disabled BECAUSE of edit mode.
    const prevButton = page.getByRole('button', { name: 'Previous contact' })

    // Buttons should have disabled styling (opacity or disabled attribute)
    await expect(prevButton).toHaveAttribute('disabled', '')
    await expect(nextButton).toHaveAttribute('disabled', '')
    // Back is disabled in edit mode too (you can't leave mid-edit).
    await expect(backButton).toHaveAttribute('disabled', '')

    // Try pressing arrow key - should not navigate. Register the observation
    // window BEFORE the keypress (waitForURL only sees navigations that begin
    // after it is called); resolve to a boolean so a real regression is
    // asserted here, not raced.
    const currentUrl = page.url()
    const navProbe = page
      .waitForURL(u => u.pathname.startsWith('/contacts/') && !u.pathname.includes(contactA.id), {
        timeout: 1000,
      })
      .then(
        () => true,
        () => false
      )
    await page.keyboard.press('ArrowRight')
    expect(await navProbe).toBe(false)
    // Final confirmation: still on the original contact's URL.
    await expect(page).toHaveURL(currentUrl)
  })

  test('should not navigate when typing in input fields', async ({ page }) => {
    // spec: CON-040.arrows-inert-while-editing
    const seeded = await testApi.seedBehavior('CON-040')
    const contactA = seeded.entities['a']
    const fullNameA = contactA.name

    // Go to contact detail and enter edit mode
    await page.goto(`/contacts?search=${encodeURIComponent(declaredWorldSearch(seeded))}`)
    await page.waitForLoadState('domcontentloaded')
    await page.getByText(fullNameA).click()
    await page.waitForURL(/\/contacts\/[A-Za-z0-9-]+/)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullNameA })).toBeVisible({ timeout: 15000 })

    // Enter edit mode to get input fields
    await page.getByRole('button', { name: 'Edit' }).first().click()
    await page.waitForLoadState('domcontentloaded')

    // Focus on name input
    const nameInput = page.getByLabel('Full Name')
    await nameInput.focus()

    // Store current URL
    const currentUrl = page.url()

    // Type arrow keys in the input - they should move cursor, not navigate.
    // Register the unexpected-navigation probe BEFORE the keypresses; resolve to
    // a boolean so a real regression is asserted, not raced.
    const navProbe = page
      .waitForURL(u => u.pathname.startsWith('/contacts/') && !u.pathname.includes(contactA.id), {
        timeout: 1000,
      })
      .then(
        () => true,
        () => false
      )
    await nameInput.press('ArrowRight')
    await nameInput.press('ArrowLeft')
    expect(await navProbe).toBe(false)
    // Should still be on same page.
    await expect(page).toHaveURL(currentUrl)
  })

  test('should preserve URL context (sort, search) during navigation', async ({ page }) => {
    // spec: CON-060.sort-order-search-context
    // The declared fixture PINS both names, so under sort=name&order=asc the
    // origin ("a") is first and a forward move genuinely exists. That matters
    // for more than tidiness: at the end of the list the Next control is
    // disabled and ArrowRight does nothing, and the context assertions below
    // would then be satisfied by the origin's own unchanged URL — a destination
    // that was never produced. The destination id is asserted for the same
    // reason, as an independent guard against a no-op navigation.
    const seeded = await testApi.seedBehavior('CON-060')
    const originId = seeded.entities['a'].id
    const nextId = seeded.entities['b'].id
    const searchTerm = declaredWorldSearch(seeded)

    // Go directly to a contact detail page with sort params
    // This tests that the detail page preserves context when navigating
    await page.goto(
      `/contacts/${originId}?sort=name&order=asc&search=${encodeURIComponent(searchTerm)}`
    )
    await page.waitForLoadState('domcontentloaded')

    // URL should contain the sort params
    expect(page.url()).toContain('sort=name')
    expect(page.url()).toContain('order=asc')

    // Keyboard nav is disabled until the navigation id list loads, and the Next
    // control renders DISABLED while it does — an ArrowRight inside that window
    // is a silent no-op with no retry, which the destination assertion below
    // would then read as a real failure. Same gate as every other arrow-pressing
    // test in this file.
    await expect(page.getByText(/\d+ of \d+/)).toBeVisible({ timeout: 10000 })
    await expect(page.getByRole('button', { name: 'Next contact' })).toBeEnabled()

    // Navigate to next contact using keyboard
    await page.keyboard.press('ArrowRight')

    // A real destination: the pinned name-ascending neighbour, not the origin.
    await expect(page).toHaveURL(new RegExp(`/contacts/${nextId}\\?`), { timeout: 10000 })
    const destination = new URL(page.url())
    expect(destination.pathname).toBe(`/contacts/${nextId}`)

    // The destination URL carries the whole list context it was reached with.
    expect(destination.searchParams.get('sort')).toBe('name')
    expect(destination.searchParams.get('order')).toBe('asc')
    expect(destination.searchParams.get('search')).toBe(searchTerm)
  })

  test('should navigate via navigation bar buttons', async ({ page }) => {
    // spec: CON-059.buttons-move-adjacent-contact, CON-059.position-indicator-reports-contact
    // The declared fixture pins three names whose name-ascending order is known
    // before the data exists, so a pass proves real movement to the ADJACENT
    // contact rather than just "some navigation happened".
    const seeded = await testApi.seedBehavior('CON-059')
    const firstId = seeded.entities['a'].id
    const secondId = seeded.entities['b'].id
    const secondName = seeded.entities['b'].name

    // Go directly to first contact with sort param and search filter to isolate IDs
    await page.goto(
      `/contacts/${firstId}?sort=name&order=asc&search=${encodeURIComponent(
        declaredWorldSearch(seeded)
      )}`
    )
    await page.waitForLoadState('domcontentloaded')

    // Wait for navigation bar to be fully ready with IDs loaded; the position
    // indicator reports the contact's EXACT place in the search-isolated
    // 3-contact fixture (CON-059.position-indicator-reports-contact) — first under name-asc, so "1 of 3".
    await expect(page.getByRole('button', { name: 'Next contact' })).toBeVisible({ timeout: 10000 })
    await expect(page.getByText('1 of 3')).toBeVisible({ timeout: 10000 })

    // Click next: it moves to the ADJACENT contact under the carried
    // name-asc order — entity b, the pinned "Button Nav 2"
    // (CON-059.buttons-move-adjacent-contact) — and the position indicator
    // advances with it.
    const nextButton = page.getByRole('button', { name: 'Next contact' })
    await expect(nextButton).toBeEnabled({ timeout: 5000 })
    await nextButton.click()
    await page.waitForURL(u => u.pathname === `/contacts/${secondId}`)
    await expect(page.getByRole('heading', { name: secondName })).toBeVisible({ timeout: 10000 })
    await expect(page.getByText('2 of 3')).toBeVisible({ timeout: 10000 })
  })

  test('should restore search and sort state after Escape back to list', async ({ page }) => {
    // spec: CON-040.escape-discards-edit-mode
    // The declared fixture's pinned names make search + sort visibly shape the
    // list.
    const seeded = await testApi.seedBehavior('CON-040')
    const searchTerm = declaredWorldSearch(seeded)
    const fullNameAlpha = seeded.entities['a'].name

    // Apply a search and a name-asc sort on the list
    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')
    await page.getByPlaceholder('Search contacts...').fill(searchTerm)
    await expect(page.getByText(fullNameAlpha)).toBeVisible({ timeout: 15000 })
    await page.getByRole('columnheader').filter({ hasText: /^Name/ }).click()

    // The list mirrors its state into the URL
    await expect(page).toHaveURL(/sort=name/)
    await expect(page).toHaveURL(/order=asc/)

    // Enter a detail page, then Escape back
    await page.getByText(fullNameAlpha).click()
    await page.waitForURL(/\/contacts\/[A-Za-z0-9-]+/)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullNameAlpha })).toBeVisible({
      timeout: 15000,
    })
    await page.keyboard.press('Escape')
    await page.waitForLoadState('domcontentloaded')

    // Back on the list with search + sort restored, not reset to defaults
    await expect(page.getByRole('heading', { name: 'Contacts' })).toBeVisible()
    await expect(page).toHaveURL(/sort=name/)
    await expect(page).toHaveURL(/order=asc/)
    await expect(page.getByPlaceholder('Search contacts...')).toHaveValue(searchTerm)
  })

  test('detail prev/next follows the same default (cadence) ordering as the list', async ({
    page,
  }) => {
    // spec: CON-038.detail-prev-next-same-default
    // Navigate by CLICKING the seeded row in the default (cadence) list — the
    // detail must CARRY that ordering context, so prev/next walks the same
    // cadence order rather than an ordering hand-fed through the URL. The
    // declaration pins names that are scrambled against the cadence order, so
    // alphabetical order (either direction) differs from it: cadence-desc =
    // Yankee(weekly) → Alpha(monthly) → Mike(annual).
    const seeded = await testApi.seedBehavior('CON-038')
    const annualId = seeded.entities['annual'].id
    const weeklyId = seeded.entities['weekly'].id
    const monthlyId = seeded.entities['monthly'].id
    const weeklyName = seeded.entities['weekly'].name
    const monthlyName = seeded.entities['monthly'].name
    const annualName = seeded.entities['annual'].name

    // Filter the default list to just these three, then open the most-frequent
    // (weekly) contact by clicking its row. Capture the detail's ids_only nav
    // request to prove the traversal order itself is cadence, not name.
    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')
    await page.getByPlaceholder('Search contacts...').fill(declaredWorldSearch(seeded))
    await page.getByPlaceholder('Search contacts...').press('Enter')
    await expect(page.getByText(weeklyName)).toBeVisible({ timeout: 15000 })

    const navIdsRequest = page.waitForResponse(
      resp =>
        resp.request().method() === 'GET' &&
        new URL(resp.url()).searchParams.get('ids_only') === 'true'
    )
    await page.getByText(weeklyName).click()
    await page.waitForURL(new RegExp(`/contacts/${weeklyId}`))
    await expect(page.getByRole('heading', { name: weeklyName })).toBeVisible({ timeout: 15000 })

    // The list's ordering context traveled into the detail URL AND the nav
    // request (cadence-desc) — the traversal order is cadence, not name.
    await expect(page).toHaveURL(/sort=cadence/)
    await expect(page).toHaveURL(/order=desc/)
    const navParams = new URL((await navIdsRequest).url()).searchParams
    expect(navParams.get('sort')).toBe('cadence')
    expect(navParams.get('order')).toBe('desc')
    // Keyboard nav is disabled until the navigation id list loads.
    await expect(page.getByText(/\d+ of \d+/)).toBeVisible({ timeout: 10000 })

    // Weekly is first in cadence-desc order → Previous disabled at this boundary.
    await expect(page.getByRole('button', { name: 'Previous contact' })).toBeDisabled()

    // Next walks weekly → monthly → annual (most- to least-frequent). Wait for
    // each incoming contact to finish loading before the next press — keyboard
    // nav is disabled while the contact is still fetching.
    await page.keyboard.press('ArrowRight')
    await page.waitForURL(new RegExp(`/contacts/${monthlyId}`))
    await expect(page.getByRole('heading', { name: monthlyName })).toBeVisible({ timeout: 10000 })
    await page.keyboard.press('ArrowRight')
    await page.waitForURL(new RegExp(`/contacts/${annualId}`))
    await expect(page.getByRole('heading', { name: annualName })).toBeVisible({ timeout: 10000 })

    // Annual is last → Next disabled at the far boundary.
    await expect(page.getByRole('button', { name: 'Next contact' })).toBeDisabled()
  })

  test('arrow keys move to the previous/next contact and disable at both boundaries @smoke', async ({
    page,
  }) => {
    // spec: CON-040.left-right-arrows-move
    // The declaration pins a known name-asc order and the search isolates the
    // set, so a pass proves real movement to the adjacent contact.
    const seeded = await testApi.seedBehavior('CON-040')
    const alphaId = seeded.entities['a'].id
    const bravoId = seeded.entities['b'].id
    const charlieId = seeded.entities['c'].id
    const alphaName = seeded.entities['a'].name
    const bravoName = seeded.entities['b'].name
    const charlieName = seeded.entities['c'].name

    // Open the middle contact under an explicit name-asc order.
    await page.goto(
      `/contacts/${bravoId}?sort=name&order=asc&search=${encodeURIComponent(
        declaredWorldSearch(seeded)
      )}`
    )
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: bravoName })).toBeVisible({ timeout: 15000 })
    // Keyboard nav is disabled until the navigation id list loads.
    await expect(page.getByText(/\d+ of \d+/)).toBeVisible({ timeout: 10000 })

    // Right → next (Charlie); Left → previous (Alpha). Wait for each incoming
    // contact to finish loading before the next press (keyboard nav is disabled
    // while it fetches).
    await page.keyboard.press('ArrowRight')
    await page.waitForURL(u => u.pathname === `/contacts/${charlieId}`)
    await expect(page.getByRole('heading', { name: charlieName })).toBeVisible({ timeout: 10000 })
    await page.keyboard.press('ArrowLeft')
    await page.waitForURL(u => u.pathname === `/contacts/${bravoId}`)
    await expect(page.getByRole('heading', { name: bravoName })).toBeVisible({ timeout: 10000 })
    await page.keyboard.press('ArrowLeft')
    await page.waitForURL(u => u.pathname === `/contacts/${alphaId}`)
    await expect(page.getByRole('heading', { name: alphaName })).toBeVisible({ timeout: 10000 })

    // Alpha is first → Previous disabled at the near boundary.
    await expect(page.getByRole('button', { name: 'Previous contact' })).toBeDisabled()

    // Walk to the last contact → Next disabled at the far boundary.
    await page.keyboard.press('ArrowRight')
    await page.waitForURL(u => u.pathname === `/contacts/${bravoId}`)
    await expect(page.getByRole('heading', { name: bravoName })).toBeVisible({ timeout: 10000 })
    await page.keyboard.press('ArrowRight')
    await page.waitForURL(u => u.pathname === `/contacts/${charlieId}`)
    await expect(page.getByRole('heading', { name: charlieName })).toBeVisible({ timeout: 10000 })
    await expect(page.getByRole('button', { name: 'Next contact' })).toBeDisabled()
  })

  test('Enter opens edit mode when focus is outside an input', async ({ page }) => {
    // spec: CON-040.enter-opens-edit-mode
    const seeded = await testApi.seedBehavior('CON-040')
    const fullName = seeded.entities['a'].name

    await page.goto(`/contacts/${seeded.entities['a'].id}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Click the (non-interactive) name heading so focus is not on the Edit
    // button or any input, then press Enter.
    await page.getByRole('heading', { name: fullName }).click()
    await page.keyboard.press('Enter')

    await expect(page.getByRole('heading', { name: 'Edit Contact' })).toBeVisible({
      timeout: 10000,
    })
  })

  test('Escape discards an unsaved edit without persisting the change', async ({
    page,
    request,
  }) => {
    // spec: CON-040.escape-discards-edit-mode
    const seeded = await testApi.seedBehavior('CON-040')
    const contactId = seeded.entities['a'].id
    const fullName = seeded.entities['a'].name
    // Derived from the seeded name so that even if the discard DID persist (the
    // regression this test exists to catch), the row stays reachable by the
    // namespace's own name-derived sweep.
    const changedName = `${fullName} CHANGED`

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Watch for any contact-update request — discarding must not persist.
    let updateFired = false
    const watchUpdate = (req: import('@playwright/test').Request) => {
      const m = req.method()
      if ((m === 'PUT' || m === 'PATCH') && req.url().includes(`/api/v1/contacts/${contactId}`)) {
        updateFired = true
      }
    }
    page.on('request', watchUpdate)

    // Enter edit mode and modify the name.
    await page.getByRole('button', { name: 'Edit' }).first().click()
    await expect(page.getByRole('heading', { name: 'Edit Contact' })).toBeVisible({
      timeout: 10000,
    })
    const nameInput = page.getByLabel('Full Name')
    await nameInput.fill(changedName)

    // Blur the input (Escape is ignored while an input is focused), then Escape.
    await page.getByRole('heading', { name: 'Edit Contact' }).click()
    await page.keyboard.press('Escape')

    // Edit mode exits back to the read view AND the modified value did not persist.
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 10000 })
    await expect(page.getByRole('heading', { name: 'Edit Contact' })).not.toBeVisible()
    await expect(page.getByRole('heading', { name: changedName })).not.toBeVisible()

    // No update request fired, and the stored name is still the original.
    page.off('request', watchUpdate)
    expect(updateFired).toBe(false)
    const stored = await request.get(`${API_BASE_URL}/api/v1/contacts/${contactId}`, {
      headers: API_HEADERS,
    })
    expect(stored.ok()).toBeTruthy()
    expect((await stored.json()).data.full_name).toBe(fullName)
  })

  test('Escape closes the Add Task modal in place without navigating away', async ({ page }) => {
    // spec: CON-040.escape-discards-edit-mode
    // Focus a non-input control (a kind toggle button) before Escape, so this
    // is NOT covered by the input-focus guard — it isolates the modal-open gate.
    const seeded = await testApi.seedBehavior('CON-040')
    const contactId = seeded.entities['a'].id
    const fullName = seeded.entities['a'].name

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })
    const detailUrl = page.url()

    await page.getByRole('button', { name: 'Add' }).click()
    await expect(page.getByRole('heading', { name: /Add Task for/ })).toBeVisible({
      timeout: 10000,
    })
    await page.getByRole('button', { name: 'Reach out' }).click()

    await page.keyboard.press('Escape')

    // Modal closes; the page stays put — no back-to-list navigation.
    await expect(page.getByRole('heading', { name: /Add Task for/ })).not.toBeVisible()
    await expect(page).toHaveURL(detailUrl)
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible()
  })

  test('Escape on a not-found contact reaches the normal back-to-list handler', async ({
    page,
  }) => {
    // Regression coverage only — this is the "Contact not found" error view,
    // not a detail page opened from a list, so it does not verify CON-040's
    // given/when and carries no spec citation.
    // A ?action=merge URL sets isMergeModalOpen=true from the query param
    // before the contact ever loads. On a nonexistent id the page takes its
    // error/not-found return with no modal mounted, so isModalOpen must read
    // false here — meaning Escape reaches the SAME normal handler as any other
    // non-modal view and navigates back to the list. (A stale isModalOpen that
    // wrongly reads true from the unmounted merge modal's flag would swallow
    // this Escape and leave the page on "Contact not found" instead — the
    // opposite of what this test asserts.)
    const missingId = '00000000-0000-0000-0000-000000000000'

    await page.goto(`/contacts/${missingId}?action=merge`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: 'Contact not found' })).toBeVisible({
      timeout: 15000,
    })

    await page.keyboard.press('Escape')

    await page.waitForURL(u => new URL(u).pathname === '/contacts')
    await expect(page.getByRole('heading', { name: 'Contacts', level: 2 })).toBeVisible({
      timeout: 10000,
    })
  })

  test('Escape closes the Log Interaction modal in place without navigating away', async ({
    page,
  }) => {
    // spec: CON-040.escape-discards-edit-mode
    // Focus a non-input control (the Mutual direction toggle) before Escape —
    // the existing input-focus test below only ever focuses the date input, so
    // it cannot catch a regression that drops isLogModalOpen from the gate.
    const seeded = await testApi.seedBehavior('CON-040')
    const contactId = seeded.entities['a'].id
    const fullName = seeded.entities['a'].name

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })
    const detailUrl = page.url()

    await page.getByRole('button', { name: 'Log Interaction' }).click()
    await expect(page.getByRole('dialog')).toBeVisible()
    await page.getByRole('button', { name: 'Mutual' }).click()

    await page.keyboard.press('Escape')

    await expect(page.getByRole('dialog')).not.toBeVisible()
    await expect(page).toHaveURL(detailUrl)
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible()
  })

  test('arrows are inert while focus is in an input outside edit mode', async ({ page }) => {
    // spec: CON-040.arrows-inert-while-editing
    // The Log Interaction modal keeps keyboard nav ENABLED (it is not edit
    // mode), so focusing its input exercises the hook's input-target guard
    // specifically — unlike edit mode, which disables the whole hook and would
    // mask a regression in that guard.
    const seeded = await testApi.seedBehavior('CON-040')
    const firstName = seeded.entities['a'].name
    const secondId = seeded.entities['b'].id

    // Open the first of two contacts with nav context so Next is a real move.
    await page.goto(
      `/contacts/${seeded.entities['a'].id}?sort=name&order=asc&search=${encodeURIComponent(
        declaredWorldSearch(seeded)
      )}`
    )
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: firstName })).toBeVisible({ timeout: 15000 })
    await expect(page.getByText(/\d+ of \d+/)).toBeVisible({ timeout: 10000 })
    // Next is enabled — arrows WOULD move if the input guard were removed.
    await expect(page.getByRole('button', { name: 'Next contact' })).toBeEnabled()

    const url = page.url()

    // Open the Log Interaction modal (keyboard nav stays enabled — not edit mode).
    await page.getByRole('button', { name: 'Log Interaction' }).click()
    await expect(page.getByRole('dialog')).toBeVisible()

    // Focus the modal's date input and press the arrow keys — they move the
    // cursor within the input, not the contact.
    const dateInput = page.getByTestId('log-interaction-date-input')
    await dateInput.focus()
    // A broken input-target guard would move to the Next contact (entity b, the
    // pinned "Kbd Move Bravo" under sort=name asc). Register the observation
    // window BEFORE the keypresses — waitForURL only sees navigations that begin
    // after it is called, so starting it afterward could miss the very nav it must
    // catch. Resolve to a boolean rather than throwing so a real regression is
    // asserted here, not killed before the assertion.
    const navProbe = page.waitForURL(`**/contacts/${secondId}**`, { timeout: 1000 }).then(
      () => true,
      () => false
    )
    await dateInput.press('ArrowRight')
    await dateInput.press('ArrowLeft')
    expect(await navProbe).toBe(false)
    // Final confirmation: still on the original contact's URL.
    await expect(page).toHaveURL(url)
  })
})

// A paginated-list request (the useContacts fetch), distinguished from the
// navigation id list (useContactIDs) by the absent ids_only param.
function isListRequest(url: string): boolean {
  const u = new URL(url)
  return u.pathname === '/api/v1/contacts' && u.searchParams.get('ids_only') === null
}

test.describe('Contact Back-to-list navigation @area:contact-navigation', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  // Walk the REAL journey — filtered/sorted list → page 2 → open the first
  // page-2 contact (the 21st under name-asc) → reload the DETAIL page — leaving
  // the page on the reloaded detail view ready to return. The reload tears down
  // the QueryClient so the list's staleTime:2min cache can't serve the return
  // from cache and hang a return waitForResponse. Also asserts the list→detail
  // URL is page-free.
  //
  // The declared cohort is 21 cadence-bearing contacts with pinned, zero-padded
  // names, so under has_cadence + no_followup the filtered set is all 21 and
  // entity p21 is the first (only) row of page 2 (CONTACTS_PAGE_SIZE=20).
  async function journeyToBackNav21Detail(page: Page, seeded: SeedBehaviorResult): Promise<void> {
    const searchTerm = declaredWorldSearch(seeded)
    const last = seeded.entities['p21']
    await page.goto(
      `/contacts?search=${encodeURIComponent(searchTerm)}&followup_filter=no_followup`
    )
    await page.waitForLoadState('domcontentloaded')

    // Real sort interaction, then settle on the applied state before the next
    // interaction (a click mid re-render lands on a replaced node and is lost).
    const nameHeader = page.getByRole('columnheader').filter({ hasText: /^Name/ })
    await nameHeader.click()
    await expect(nameHeader).toHaveAttribute('aria-sort', 'ascending')
    await expect(page).toHaveURL(/sort=name/)
    await expect(page).toHaveURL(/order=asc/)

    // Real filter interaction.
    await page.getByLabel('Filter by cadence').selectOption('has_cadence')
    await expect(page).toHaveURL(/cadence_filter=has_cadence/)

    // Page to page 2 via the Pagination control; the list writes ?page=2.
    const pager = page.locator('[data-testid="pagination"]').first()
    await expect(pager).toBeVisible({ timeout: 10000 })
    await pager.getByRole('button', { name: '2' }).click()
    await expect(page).toHaveURL(/page=2/)

    // Open the first page-2 contact (the 21st under name-asc).
    await page.getByText(last.name).click()
    await page.waitForURL(u => u.pathname === `/contacts/${last.id}`)

    // The list→detail URL carries the full context but NEVER a page.
    const detail = new URL(page.url())
    expect(detail.searchParams.get('sort')).toBe('name')
    expect(detail.searchParams.get('order')).toBe('asc')
    expect(detail.searchParams.get('search')).toBe(searchTerm)
    expect(detail.searchParams.get('cadence_filter')).toBe('has_cadence')
    expect(detail.searchParams.get('followup_filter')).toBe('no_followup')
    expect(detail.searchParams.get('page')).toBeNull()

    // Reload the detail page (clears the QueryClient), then wait for nav to be
    // ready so Back computes the real page, not the page-1 fallback.
    await page.reload()
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: last.name })).toBeVisible({ timeout: 15000 })
    await expect(page.getByText('21 of 21')).toBeVisible({ timeout: 10000 })
  }

  // Assert a captured list request and the browser URL both carry the complete
  // preserved set: sort/order/search/BOTH filters + the given page.
  async function expectFullContextRestored(
    page: Page,
    reqUrl: string,
    expectedPage: string,
    searchTerm: string
  ): Promise<void> {
    const params = new URL(reqUrl).searchParams
    expect(params.get('sort')).toBe('name')
    expect(params.get('order')).toBe('asc')
    expect(params.get('search')).toBe(searchTerm)
    expect(params.get('cadence_filter')).toBe('has_cadence')
    expect(params.get('followup_filter')).toBe('no_followup')
    expect(params.get('page')).toBe(expectedPage)

    await page.waitForURL(
      u =>
        u.pathname === '/contacts' &&
        u.searchParams.get('sort') === 'name' &&
        u.searchParams.get('order') === 'asc' &&
        u.searchParams.get('search') === searchTerm &&
        u.searchParams.get('cadence_filter') === 'has_cadence' &&
        u.searchParams.get('followup_filter') === 'no_followup' &&
        u.searchParams.get('page') === expectedPage
    )
  }

  test('the Back to list control restores full context AND page via the real journey', async ({
    page,
  }) => {
    // spec: CON-065.visible-return-list-control, CON-065.returning-lands-list-page
    const seeded = await testApi.seedBehavior('CON-065')
    await journeyToBackNav21Detail(page, seeded)

    const listReq = page.waitForResponse(
      r => r.request().method() === 'GET' && isListRequest(r.url())
    )
    await page.getByRole('button', { name: 'Back to list' }).click()
    await expectFullContextRestored(page, (await listReq).url(), '2', declaredWorldSearch(seeded))
  })

  test('Escape restores full context AND page identically to the button', async ({ page }) => {
    // spec: CON-040.escape-discards-edit-mode, CON-065.returning-lands-list-page
    const seeded = await testApi.seedBehavior('CON-065')
    await journeyToBackNav21Detail(page, seeded)

    const listReq = page.waitForResponse(
      r => r.request().method() === 'GET' && isListRequest(r.url())
    )
    await page.keyboard.press('Escape')
    await expectFullContextRestored(page, (await listReq).url(), '2', declaredWorldSearch(seeded))
  })

  test('an out-of-range ?page deep-link clamps to the last valid page', async ({ page }) => {
    // spec: CON-058.page-past-end-clamps
    // A stale bookmark / hand-edited URL asking for a page past the end must
    // land on the last valid page with real rows, not an empty table with the
    // pagination controls hidden. The fixture is 21 has_cadence contacts → 2
    // pages, so ?page=9999 must clamp to page 2 (holding the 21st contact).
    const seeded = await testApi.seedBehavior('CON-065')
    const searchTerm = declaredWorldSearch(seeded)
    const backNav21 = seeded.entities['p21'].name

    // The clamp fires after the first response reveals the real page count, so
    // a page-2 list request follows the out-of-range page-9999 request.
    const clampReq = page.waitForResponse(
      r => isListRequest(r.url()) && new URL(r.url()).searchParams.get('page') === '2'
    )
    await page.goto(
      `/contacts?search=${encodeURIComponent(searchTerm)}` +
        `&sort=name&order=asc&cadence_filter=has_cadence&followup_filter=no_followup&page=9999`
    )
    await page.waitForLoadState('domcontentloaded')
    await clampReq

    // URL is rewritten to page 2 and the real page-2 row is on screen — not an
    // empty table.
    await expect(page).toHaveURL(/[?&]page=2\b/)
    await expect(page.getByText(backNav21)).toBeVisible({ timeout: 15000 })
    await expect(page.locator('tbody tr')).toHaveCount(1)
    // Pagination controls are back (they hide entirely on an empty page).
    await expect(page.locator('[data-testid="pagination"]').first()).toBeVisible()
  })

  test('an out-of-range ?page on a single-page list clamps to the bare page-1 URL', async ({
    page,
  }) => {
    // spec: CON-058.page-past-end-clamps
    // When the whole filtered set fits on one page, the clamp target is page 1,
    // which buildContactListUrl renders as the bare (page-less) URL — the
    // distinct recovery branch from the multi-page clamp above. It needs only a
    // population that fits on one page, so it rides CON-040's three-contact
    // declaration rather than owning a fixture of its own.
    const seeded = await testApi.seedBehavior('CON-040')
    const searchTerm = declaredWorldSearch(seeded)

    // The clamp re-fetches page 1 (the request carries page=1 even though the
    // browser URL becomes page-less).
    const clampReq = page.waitForResponse(
      r => isListRequest(r.url()) && new URL(r.url()).searchParams.get('page') === '1'
    )
    await page.goto(`/contacts?search=${encodeURIComponent(searchTerm)}&page=2`)
    await page.waitForLoadState('domcontentloaded')
    await clampReq

    // URL is rewritten to the bare page-1 form (no page param at all) and the
    // rows are on screen — not an empty table.
    await expect(page).not.toHaveURL(/[?&]page=/)
    await expect(page.getByText(seeded.entities['a'].name)).toBeVisible({ timeout: 15000 })
  })

  test('changing sort/search/filter from a later page resets to page 1', async ({ page }) => {
    // spec: CON-066.list-returns-first-page
    const seeded = await testApi.seedBehavior('CON-066')
    const searchTerm = declaredWorldSearch(seeded)
    const backNav21 = seeded.entities['p21'].name
    const page2Url =
      `/contacts?search=${encodeURIComponent(searchTerm)}` +
      `&sort=name&order=asc&cadence_filter=has_cadence&followup_filter=no_followup&page=2`

    // (a) sort-FIELD change: name → cadence.
    await page.goto(page2Url)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByText(backNav21)).toBeVisible({ timeout: 15000 })
    let reset = page.waitForResponse(r => {
      if (!isListRequest(r.url())) return false
      const p = new URL(r.url()).searchParams
      return p.get('page') === '1' && p.get('sort') === 'cadence'
    })
    await page
      .getByRole('columnheader')
      .filter({ hasText: /^Cadence/ })
      .click()
    await reset
    await expect(page).not.toHaveURL(/[?&]page=/)

    // (b) search change: append a character.
    await page.goto(page2Url)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByText(backNav21)).toBeVisible({ timeout: 15000 })
    const newTerm = `${searchTerm} nosuchcontact`
    reset = page.waitForResponse(r => {
      if (!isListRequest(r.url())) return false
      const p = new URL(r.url()).searchParams
      return p.get('page') === '1' && p.get('search') === newTerm
    })
    await page.getByPlaceholder('Search contacts...').fill(newTerm)
    await reset
    await expect(page).not.toHaveURL(/[?&]page=/)

    // (c) cadence-filter change: has_cadence → no_cadence.
    await page.goto(page2Url)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByText(backNav21)).toBeVisible({ timeout: 15000 })
    reset = page.waitForResponse(r => {
      if (!isListRequest(r.url())) return false
      const p = new URL(r.url()).searchParams
      return p.get('page') === '1' && p.get('cadence_filter') === 'no_cadence'
    })
    await page.getByLabel('Filter by cadence').selectOption('no_cadence')
    await reset
    await expect(page).not.toHaveURL(/[?&]page=/)

    // (d) order-TOGGLE change: re-click the active sort header (name asc → desc).
    // Same handleSort → applyContext path as (a), but the toggle branch, which
    // keeps the field and flips only the direction.
    await page.goto(page2Url)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByText(backNav21)).toBeVisible({ timeout: 15000 })
    reset = page.waitForResponse(r => {
      if (!isListRequest(r.url())) return false
      const p = new URL(r.url()).searchParams
      return p.get('page') === '1' && p.get('sort') === 'name' && p.get('order') === 'desc'
    })
    await page.getByRole('columnheader').filter({ hasText: /^Name/ }).click()
    await reset
    await expect(page).not.toHaveURL(/[?&]page=/)
  })

  test('using pagination reflects the page into the URL and survives a reload', async ({
    page,
  }) => {
    // spec: CON-058.current-page-reflected-in-url
    const seeded = await testApi.seedBehavior('CON-065')
    await page.goto(
      `/contacts?search=${encodeURIComponent(declaredWorldSearch(seeded))}` +
        `&sort=name&order=asc&cadence_filter=has_cadence&followup_filter=no_followup`
    )
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByText(seeded.entities['p01'].name)).toBeVisible({ timeout: 15000 })

    // USE the Pagination control → the URL gains ?page=2. Await the click's
    // OWN page-2 fetch to settle first, so the reload waiter below cannot
    // resolve from this still-in-flight response instead of the reload's.
    const pager = page.locator('[data-testid="pagination"]').first()
    await expect(pager).toBeVisible({ timeout: 10000 })
    const clickReq = page.waitForResponse(
      r => isListRequest(r.url()) && new URL(r.url()).searchParams.get('page') === '2'
    )
    await pager.getByRole('button', { name: '2' }).click()
    await expect(page).toHaveURL(/page=2/)
    await clickReq

    // Refresh-stability: arm a DISTINCT waiter AFTER the click fetch settled,
    // then reload. The reloaded list fetches page 2 from the URL (cold, so this
    // also covers deep-linking), not page 1.
    const reloadReq = page.waitForResponse(
      r => isListRequest(r.url()) && new URL(r.url()).searchParams.get('page') === '2'
    )
    await page.reload()
    expect(new URL((await reloadReq).url()).searchParams.get('page')).toBe('2')
  })
})
