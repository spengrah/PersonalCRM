import { test, expect } from '@playwright/test'
import type { APIRequestContext, Page } from '@playwright/test'
import { createTestAPI, declaredWorldNamePrefix, TestAPI } from './helpers/test-api'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

interface ContactRead {
  location?: string
  birthday?: string
  methods?: { type: string; value: string }[]
}

// Read a declared contact back through the API. The declaration owns every
// generated value (a location is namespace-prefixed, a birthday lands on a
// leap-safe year, a phone comes from the namespace's own block), so an assertion
// that needs one of them reads it rather than restating a literal that would
// silently stop matching.
async function readContact(request: APIRequestContext, contactId: string): Promise<ContactRead> {
  const response = await request.get(`${API_BASE_URL}/api/v1/contacts/${contactId}`, {
    headers: API_HEADERS,
  })
  expect(response.ok()).toBeTruthy()
  return (await response.json()).data as ContactRead
}

// The merge modal's own birthday-button label: the same conversion
// merge-contact-modal.tsx applies, so the label is derived rather than guessed.
// The stored year comes from the generator's leap-safe birth year, so it must
// never be hard-coded.
function mergeBirthdayLabel(birthday: string | undefined): string {
  expect(birthday, 'the declared contact must carry a birthday').toBeTruthy()
  return new Date(birthday as string).toLocaleDateString('en-US', {
    month: 'short',
    day: 'numeric',
    year: 'numeric',
  })
}

// The merge modal is a labeled dialog (role="dialog", accessible name
// "Merge Contacts") — the canonical scope for every in-modal assertion.
function mergeModal(page: Page) {
  return page.getByRole('dialog', { name: 'Merge Contacts' })
}

// Open the source selector, search the declared world (which reaches the target
// too, so its ABSENCE from the options is a real claim), and pick the named
// source from the selector's listbox options.
//
// The term is the world's NAME PREFIX, not its full-text search term: this
// selector filters client-side by plain substring over the contacts it has
// already loaded, so a space-separated tsquery term matches nothing here.
async function selectMergeSource(page: Page, searchTerm: string, sourceName: string) {
  await page.getByText('Search for a contact to merge...').click()
  await page.getByPlaceholder('Search for a contact to merge...').fill(searchTerm)
  const sourceOption = page.getByRole('option', { name: sourceName })
  await expect(sourceOption).toBeVisible({ timeout: 5000 })
  await sourceOption.click()
}

test.describe('Contact Merge @area:contact-merge', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should display target contact as "Keeping"', async ({ page }) => {
    // spec: CON-043.current-contact-marked-kept
    const seeded = await testApi.seedBehavior('CON-043')
    const contactId = seeded.entities['target'].id
    const fullName = seeded.entities['target'].name

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Open merge modal
    await page.getByRole('button', { name: /Merge/i }).click()
    const modal = mergeModal(page)
    await expect(modal).toBeVisible()

    // Verify target contact is shown with "Keeping" badge
    await expect(modal.getByText(fullName).first()).toBeVisible()
    await expect(modal.getByText('Keeping')).toBeVisible()
  })

  test('should search and select source contact', async ({ page }) => {
    // spec: CON-043.current-contact-marked-kept, CON-043.selecting-source-loads-preview
    const seeded = await testApi.seedBehavior('CON-043')
    const targetId = seeded.entities['target'].id
    const targetName = seeded.entities['target'].name
    const sourceName = seeded.entities['source'].name

    await page.goto(`/contacts/${targetId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: targetName })).toBeVisible({ timeout: 15000 })

    // Open merge modal
    await page.getByRole('button', { name: /Merge/i }).click()
    await expect(mergeModal(page)).toBeVisible()

    // Search for the source contact and wait for its option to appear
    await page.getByText('Search for a contact to merge...').click()
    await page
      .getByPlaceholder('Search for a contact to merge...')
      .fill(declaredWorldNamePrefix(seeded))
    const sourceOption = page.getByRole('option', { name: sourceName })
    await expect(sourceOption).toBeVisible({ timeout: 5000 })

    // The selector excludes the merge target — it appears only as the kept
    // heading, never as a selectable option (CON-043.current-contact-marked-kept).
    //
    // Matched on the option's NAME line, exactly: an option's accessible name also
    // carries its primary method, so an exact role-name match would be vacuous —
    // and a bare substring one would be satisfied by the SOURCE option whenever the
    // two drawn names collide, since the repeat renders as the target's name plus a
    // sequence number.
    await expect(
      page.getByRole('option').filter({ has: page.getByText(targetName, { exact: true }) })
    ).toHaveCount(0)

    // Select the source contact
    await sourceOption.click()

    // Verify "Will Be Merged" section appears (a source loads a preview).
    await expect(page.getByText('Will Be Merged')).toBeVisible({ timeout: 5000 })
  })

  test('should toggle field selection between source and target', async ({ page, request }) => {
    // spec: CON-043.conflicting-fields-cadence-location
    // Conflicts span all three fields the spec names (cadence, location,
    // birthday), so the default-keeps-target proof covers the full clause, not
    // just location.
    const seeded = await testApi.seedBehavior('CON-043')
    const targetId = seeded.entities['target'].id
    const targetName = seeded.entities['target'].name
    const sourceName = seeded.entities['source'].name

    // The button labels come from the STORED values, which the declaration owns:
    // a location is namespace-prefixed and a birthday lands on the generator's
    // leap-safe birth year, so neither literal can be written here.
    const target = await readContact(request, targetId)
    const source = await readContact(request, seeded.entities['source'].id)
    const targetLocation = target.location as string
    const sourceLocation = source.location as string
    expect(targetLocation, 'the declared target must carry a location').toBeTruthy()
    expect(sourceLocation, 'the declared source must carry a location').toBeTruthy()
    const targetBirthdayText = mergeBirthdayLabel(target.birthday)
    const sourceBirthdayText = mergeBirthdayLabel(source.birthday)

    await page.goto(`/contacts/${targetId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: targetName })).toBeVisible({ timeout: 15000 })

    // Open merge modal
    await page.getByRole('button', { name: /Merge/i }).click()

    // Select source contact
    await selectMergeSource(page, declaredWorldNamePrefix(seeded), sourceName)

    // Wait for conflicts section
    await expect(page.getByText('Resolve Conflicts')).toBeVisible({ timeout: 5000 })

    // Default (before any toggle) keeps the TARGET value selected for EVERY
    // conflicting field — cadence, location, and birthday. Selection state is
    // exposed as aria-pressed on the toggle buttons. The birthday label is
    // computed the same way the modal formats it, so the assertion is stable
    // across the runner's timezone.
    await expect(page.getByRole('button', { name: 'Monthly' })).toHaveAttribute(
      'aria-pressed',
      'true'
    )
    const targetLocationButton = page.getByRole('button', { name: targetLocation })
    await expect(targetLocationButton).toHaveAttribute('aria-pressed', 'true')
    await expect(page.getByRole('button', { name: targetBirthdayText })).toHaveAttribute(
      'aria-pressed',
      'true'
    )

    // Each field toggles to its SOURCE value independently — location, cadence,
    // and birthday.
    const sourceLocationButton = page.getByRole('button', { name: sourceLocation })
    await expect(sourceLocationButton).toBeVisible()
    await sourceLocationButton.click()
    await expect(sourceLocationButton).toHaveAttribute('aria-pressed', 'true')
    await expect(targetLocationButton).toHaveAttribute('aria-pressed', 'false')

    const weeklyButton = page.getByRole('button', { name: 'Weekly' })
    await weeklyButton.click()
    await expect(weeklyButton).toHaveAttribute('aria-pressed', 'true')
    await expect(page.getByRole('button', { name: 'Monthly' })).toHaveAttribute(
      'aria-pressed',
      'false'
    )

    const sourceBirthdayButton = page.getByRole('button', { name: sourceBirthdayText })
    await sourceBirthdayButton.click()
    await expect(sourceBirthdayButton).toHaveAttribute('aria-pressed', 'true')
    await expect(page.getByRole('button', { name: targetBirthdayText })).toHaveAttribute(
      'aria-pressed',
      'false'
    )
  })

  test('should edit merged contact name', async ({ page }) => {
    // spec: CON-043.merged-name-editable
    const seeded = await testApi.seedBehavior('CON-043')
    const targetId = seeded.entities['target'].id
    const targetName = seeded.entities['target'].name

    await page.goto(`/contacts/${targetId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: targetName })).toBeVisible({ timeout: 15000 })

    // Open merge modal
    await page.getByRole('button', { name: /Merge/i }).click()
    const modal = mergeModal(page)

    // Click on the name to enter edit mode
    await modal.getByRole('heading', { level: 3 }).first().click()

    // Verify input appears with current name
    const nameInput = modal.getByRole('textbox', { name: 'Merged contact name' })
    await expect(nameInput).toBeVisible({ timeout: 5000 })

    // Edit the name
    await nameInput.fill('Custom Merged Name')
    await nameInput.press('Enter')

    // Verify name is updated in the modal header
    await expect(
      modal.getByRole('heading', { level: 3 }).filter({ hasText: 'Custom Merged Name' })
    ).toBeVisible()
  })

  test('should cancel name edit with Escape', async ({ page }) => {
    // spec: CON-043.merged-name-editable
    // The editable-name contract's discard path: Escape must not adopt the
    // typed name (no accidental rename baked into the merge).
    const seeded = await testApi.seedBehavior('CON-043')
    const contactId = seeded.entities['target'].id
    const fullName = seeded.entities['target'].name

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Open merge modal
    await page.getByRole('button', { name: /Merge/i }).click()
    const modal = mergeModal(page)

    // Click on the name to enter edit mode
    await modal.getByRole('heading', { level: 3 }).first().click()

    // Type a new name
    const nameInput = modal.getByRole('textbox', { name: 'Merged contact name' })
    await expect(nameInput).toBeVisible({ timeout: 5000 })
    await nameInput.fill('Should Not Save')

    // Press Escape to cancel
    await nameInput.press('Escape')

    // Verify original name is restored
    await expect(
      modal.getByRole('heading', { level: 3 }).filter({ hasText: fullName })
    ).toBeVisible()
    await expect(
      modal.getByRole('heading', { level: 3 }).filter({ hasText: 'Should Not Save' })
    ).not.toBeVisible()
  })

  test('should dismiss the modal without merging via backdrop click', async ({ page }) => {
    // spec: CON-043.modal-dismissed-without-merging
    // Backdrop click dismisses the modal IN PLACE: no merge fires, the modal
    // closes, and the user stays on the detail page. The Escape path is NOT
    // asserted here: the detail page's window-level Escape handler is not
    // gated on an open modal, so Escape today closes the modal AND navigates
    // back to the list in the same press — a double-action to fix before an
    // Escape-dismisses-in-place assertion can hold.
    const seeded = await testApi.seedBehavior('CON-043')
    const contactId = seeded.entities['target'].id
    const fullName = seeded.entities['target'].name

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })
    const detailUrl = page.url()

    // No merge may fire during the dismissal.
    let mergeFired = false
    const watchMerge = (req: import('@playwright/test').Request) => {
      if (req.method() === 'POST' && req.url().endsWith('/merge')) {
        mergeFired = true
      }
    }
    page.on('request', watchMerge)

    // Backdrop click dismisses: click the overlay ELEMENT itself (the
    // dialog panel's parent), at a corner outside the centered panel.
    await page.getByRole('button', { name: /Merge/i }).click()
    await expect(mergeModal(page)).toBeVisible()
    const overlay = mergeModal(page).locator('..')
    await overlay.click({ position: { x: 10, y: 10 } })
    await expect(mergeModal(page)).not.toBeVisible()

    // Dismissal, not navigation and not a merge: still on the detail page.
    await expect(page).toHaveURL(detailUrl)
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible()
    page.off('request', watchMerge)
    expect(mergeFired).toBe(false)
  })

  test('should successfully merge contacts', async ({ page, request }) => {
    // spec: CON-043.outcome-reported-auto-dismissed
    const seeded = await testApi.seedBehavior('CON-043')
    const targetId = seeded.entities['target'].id
    const sourceId = seeded.entities['source'].id
    const targetName = seeded.entities['target'].name
    const sourceName = seeded.entities['source'].name

    // The source's phone comes from the namespace's own number block, so its
    // value is read BEFORE the merge and asserted afterwards. Hard-coding a
    // literal here would fail as a real bug rather than a flake.
    const source = await readContact(request, sourceId)
    const sourcePhone = source.methods?.find(m => m.type === 'phone')?.value
    expect(sourcePhone, 'the declared source must carry a phone to transfer').toBeTruthy()

    // Seed note for source contact (notes are in separate table now)
    await testApi.seedContactNote(sourceId, 'Source notes to transfer')

    await page.goto(`/contacts/${targetId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: targetName })).toBeVisible({ timeout: 15000 })

    // Open merge modal
    await page.getByRole('button', { name: /Merge/i }).click()

    // Select source contact
    await selectMergeSource(page, declaredWorldNamePrefix(seeded), sourceName)

    // Wait for preview to load
    await expect(page.getByText('Will Be Merged')).toBeVisible({ timeout: 10000 })

    // Wait for merge button to be enabled
    const mergeButton = page.getByRole('button', { name: /Merge Contacts/i })
    await expect(mergeButton).toBeEnabled({ timeout: 5000 })

    const mergeResponse = page.waitForResponse(
      response =>
        response.request().method() === 'POST' &&
        response.url().includes('/api/v1/contacts/') &&
        response.url().endsWith('/merge'),
      { timeout: 15000 }
    )

    // Click Merge Contacts button
    await mergeButton.click()

    // The merge succeeds
    const response = await mergeResponse
    expect(response.status()).toBe(200)

    // Wait for success notification
    await expect(page.getByText(/merged successfully/i)).toBeVisible({ timeout: 10000 })

    // Verify we're back on the contact page (modal closed)
    await expect(mergeModal(page)).not.toBeVisible()

    // Verify source contact's phone was added
    await expect(page.getByText(sourcePhone as string)).toBeVisible({ timeout: 5000 })

    // Verify source contact's notes were transferred
    await expect(page.getByText('Source notes to transfer')).toBeVisible()

    // The outcome banner is reported, then auto-dismisses after its timeout.
    await expect(page.getByText(/merged successfully/i)).not.toBeVisible({ timeout: 10000 })
  })

  test('should show quick-fill name option when source has different name', async ({ page }) => {
    // spec: CON-043.merged-name-editable
    // The declared target and source render different names, which is what makes
    // the quick-fill affordance appear at all.
    const seeded = await testApi.seedBehavior('CON-043')
    const targetId = seeded.entities['target'].id
    const targetName = seeded.entities['target'].name
    const sourceName = seeded.entities['source'].name

    await page.goto(`/contacts/${targetId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: targetName })).toBeVisible({ timeout: 15000 })

    // Open merge modal
    await page.getByRole('button', { name: /Merge/i }).click()

    // Select source contact
    await selectMergeSource(page, declaredWorldNamePrefix(seeded), sourceName)

    // Wait for preview
    await expect(page.getByText('Will Be Merged')).toBeVisible({ timeout: 5000 })

    // Verify the "use this" quick-fill button appears for the source name
    const useThis = page.getByRole('button', { name: 'use this' })
    await expect(useThis).toBeVisible()

    // Click "use this" to quick-fill the source name
    await useThis.click()

    // Verify name was updated
    await expect(
      mergeModal(page).getByRole('heading', { level: 3 }).filter({ hasText: sourceName })
    ).toBeVisible()
  })

  test('should disable merge button when no source selected', async ({ page }) => {
    // spec: CON-043.merge-cannot-submit-until-ready
    const seeded = await testApi.seedBehavior('CON-043')
    const contactId = seeded.entities['target'].id
    const fullName = seeded.entities['target'].name

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Open merge modal
    await page.getByRole('button', { name: /Merge/i }).click()
    await expect(mergeModal(page)).toBeVisible()

    // Verify merge button is disabled
    const mergeButton = page.getByRole('button', { name: /Merge Contacts/i })
    await expect(mergeButton).toBeDisabled()
  })

  test('keeps the merge submit disabled while the preview is loading', async ({ page }) => {
    // spec: CON-043.merge-cannot-submit-until-ready
    const seeded = await testApi.seedBehavior('CON-043')
    const targetId = seeded.entities['target'].id
    const targetName = seeded.entities['target'].name
    const sourceName = seeded.entities['source'].name

    await page.goto(`/contacts/${targetId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: targetName })).toBeVisible({ timeout: 15000 })

    await page.getByRole('button', { name: /Merge/i }).click()

    // Delay the preview so we can observe the disabled window deterministically.
    await page.route(/\/api\/v1\/contacts\/[^/]+\/merge\/preview/, async route => {
      await new Promise(resolve => setTimeout(resolve, 2000))
      await route.continue()
    })

    // Select the source.
    await selectMergeSource(page, declaredWorldNamePrefix(seeded), sourceName)

    // While the preview loads, the submit cannot be pressed.
    const mergeButton = page.getByRole('button', { name: /Merge Contacts/i })
    await expect(mergeButton).toBeDisabled()

    // Once the preview resolves, the submit enables — proving the disabled
    // state was gated by the in-flight preview, not the missing source.
    await expect(page.getByText('Will Be Merged')).toBeVisible({ timeout: 10000 })
    await expect(mergeButton).toBeEnabled()
  })

  test('keeps the merge submit disabled while the merge is in flight', async ({ page }) => {
    // spec: CON-043.merge-cannot-submit-until-ready
    const seeded = await testApi.seedBehavior('CON-043')
    const targetId = seeded.entities['target'].id
    const targetName = seeded.entities['target'].name
    const sourceName = seeded.entities['source'].name

    await page.goto(`/contacts/${targetId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: targetName })).toBeVisible({ timeout: 15000 })

    await page.getByRole('button', { name: /Merge/i }).click()

    // Select the source and wait for the preview to settle.
    await selectMergeSource(page, declaredWorldNamePrefix(seeded), sourceName)
    await expect(page.getByText('Will Be Merged')).toBeVisible({ timeout: 10000 })

    const mergeButton = page.getByRole('button', { name: /Merge Contacts/i })
    await expect(mergeButton).toBeEnabled({ timeout: 5000 })

    // Delay the merge POST so the in-flight disabled state is observable.
    await page.route(/\/api\/v1\/contacts\/[^/]+\/merge(\?|$)/, async route => {
      await new Promise(resolve => setTimeout(resolve, 2000))
      await route.continue()
    })

    await mergeButton.click()

    // While the merge is in flight, the submit relabels to "Merging..." and is
    // disabled (no double-submit) — the delayed route keeps this window open.
    await expect(page.getByRole('button', { name: /Merging/i })).toBeDisabled({ timeout: 5000 })

    // The merge then completes.
    await expect(page.getByText(/merged successfully/i)).toBeVisible({ timeout: 10000 })
  })
})
