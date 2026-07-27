import { test, expect } from '@playwright/test'
import type { APIRequestContext, Page } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

const API_BASE_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080'
const API_KEY = process.env.NEXT_PUBLIC_API_KEY || 'test-api-key-for-ci'
const API_HEADERS = {
  'X-API-Key': API_KEY,
  'Content-Type': 'application/json',
}

// Create a contact directly (the seed endpoint does not take a birthday), so a
// merge test can build target/source pairs that conflict on cadence, location,
// AND birthday. Name is prefixed for cleanup.
async function seedMergeContact(
  request: APIRequestContext,
  testApi: TestAPI,
  input: { fullName: string; cadence: string; location: string; birthday: string }
): Promise<string> {
  const response = await request.post(`${API_BASE_URL}/api/v1/contacts`, {
    headers: API_HEADERS,
    data: {
      full_name: `${testApi.prefix}-${input.fullName}`,
      cadence: input.cadence,
      location: input.location,
      birthday: input.birthday,
    },
  })
  expect(response.ok()).toBeTruthy()
  const body = await response.json()
  return (body.data as { id: string }).id
}

// The merge modal is a labeled dialog (role="dialog", accessible name
// "Merge Contacts") — the canonical scope for every in-modal assertion.
function mergeModal(page: Page) {
  return page.getByRole('dialog', { name: 'Merge Contacts' })
}

// Open the source selector, search by the worker prefix, and pick the named
// source from the selector's listbox options.
async function selectMergeSource(page: Page, prefix: string, sourceName: string) {
  await page.getByText('Search for a contact to merge...').click()
  await page.getByPlaceholder('Search for a contact to merge...').fill(prefix)
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
    // Create a contact to be the merge target
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Target Contact Display',
      },
    ])

    const contactId = ids[0]
    const fullName = `${testApi.prefix}-Target Contact Display`

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
    // Create target and source contacts
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Select Source Target',
        cadence: 'monthly',
      },
      {
        full_name: 'Select Source Contact',
        cadence: 'weekly',
      },
    ])

    const targetId = ids[0]
    const targetName = `${testApi.prefix}-Select Source Target`
    const sourceName = `${testApi.prefix}-Select Source Contact`

    await page.goto(`/contacts/${targetId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: targetName })).toBeVisible({ timeout: 15000 })

    // Open merge modal
    await page.getByRole('button', { name: /Merge/i }).click()
    await expect(mergeModal(page)).toBeVisible()

    // Search for the source contact and wait for its option to appear
    await page.getByText('Search for a contact to merge...').click()
    await page.getByPlaceholder('Search for a contact to merge...').fill(testApi.prefix)
    const sourceOption = page.getByRole('option', { name: sourceName })
    await expect(sourceOption).toBeVisible({ timeout: 5000 })

    // The selector excludes the merge target — it appears only as the kept
    // heading, never as a selectable option (CON-043.current-contact-marked-kept).
    await expect(page.getByRole('option', { name: targetName })).toHaveCount(0)

    // Select the source contact
    await sourceOption.click()

    // Verify "Will Be Merged" section appears (a source loads a preview).
    await expect(page.getByText('Will Be Merged')).toBeVisible({ timeout: 5000 })
  })

  test('should toggle field selection between source and target', async ({ page, request }) => {
    // spec: CON-043.conflicting-fields-cadence-location
    // Conflicts span all three fields the spec names (cadence, location,
    // birthday), so the default-keeps-target proof covers the full clause, not
    // just location. Seeded via direct POST because the seed endpoint has no
    // birthday field.
    const targetId = await seedMergeContact(request, testApi, {
      fullName: 'Toggle Target',
      cadence: 'monthly',
      location: 'New York',
      birthday: '1990-03-15',
    })
    await seedMergeContact(request, testApi, {
      fullName: 'Toggle Source',
      cadence: 'weekly',
      location: 'San Francisco',
      birthday: '1985-07-20',
    })

    const targetName = `${testApi.prefix}-Toggle Target`
    const sourceName = `${testApi.prefix}-Toggle Source`

    await page.goto(`/contacts/${targetId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: targetName })).toBeVisible({ timeout: 15000 })

    // Open merge modal
    await page.getByRole('button', { name: /Merge/i }).click()

    // Select source contact
    await selectMergeSource(page, testApi.prefix, sourceName)

    // Wait for conflicts section
    await expect(page.getByText('Resolve Conflicts')).toBeVisible({ timeout: 5000 })

    // Default (before any toggle) keeps the TARGET value selected for EVERY
    // conflicting field — cadence, location, and birthday. Selection state is
    // exposed as aria-pressed on the toggle buttons. The birthday label is
    // computed the same way the modal formats it, so the assertion is stable
    // across the runner's timezone.
    const targetBirthdayText = new Date('1990-03-15').toLocaleDateString('en-US', {
      month: 'short',
      day: 'numeric',
      year: 'numeric',
    })
    await expect(page.getByRole('button', { name: 'Monthly' })).toHaveAttribute(
      'aria-pressed',
      'true'
    )
    const nyButton = page.getByRole('button', { name: 'New York' })
    await expect(nyButton).toHaveAttribute('aria-pressed', 'true')
    await expect(page.getByRole('button', { name: targetBirthdayText })).toHaveAttribute(
      'aria-pressed',
      'true'
    )

    // Each field toggles to its SOURCE value independently — location, cadence,
    // and birthday.
    const sfButton = page.getByRole('button', { name: 'San Francisco' })
    await expect(sfButton).toBeVisible()
    await sfButton.click()
    await expect(sfButton).toHaveAttribute('aria-pressed', 'true')
    await expect(nyButton).toHaveAttribute('aria-pressed', 'false')

    const weeklyButton = page.getByRole('button', { name: 'Weekly' })
    await weeklyButton.click()
    await expect(weeklyButton).toHaveAttribute('aria-pressed', 'true')
    await expect(page.getByRole('button', { name: 'Monthly' })).toHaveAttribute(
      'aria-pressed',
      'false'
    )

    const sourceBirthdayText = new Date('1985-07-20').toLocaleDateString('en-US', {
      month: 'short',
      day: 'numeric',
      year: 'numeric',
    })
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
    // Create contacts
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Name Edit Target',
      },
      {
        full_name: 'Name Edit Source',
      },
    ])

    const targetId = ids[0]
    const targetName = `${testApi.prefix}-Name Edit Target`
    const sourceName = `${testApi.prefix}-Name Edit Source`

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
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Escape Edit Test',
      },
    ])

    const contactId = ids[0]
    const fullName = `${testApi.prefix}-Escape Edit Test`

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
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Dismiss Modal Test',
      },
    ])

    const contactId = ids[0]
    const fullName = `${testApi.prefix}-Dismiss Modal Test`

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

  test('should successfully merge contacts', async ({ page }) => {
    // spec: CON-043.outcome-reported-auto-dismissed
    // Create contacts with some methods
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Merge Complete Target',
        cadence: 'monthly',
        methods: [{ type: 'email', value: 'target@example.com', is_primary: true }],
      },
      {
        full_name: 'Merge Complete Source',
        methods: [{ type: 'phone', value: '+1-555-0100' }],
      },
    ])

    const targetId = ids[0]
    const sourceId = ids[1]
    const targetName = `${testApi.prefix}-Merge Complete Target`
    const sourceName = `${testApi.prefix}-Merge Complete Source`

    // Seed note for source contact (notes are in separate table now)
    await testApi.seedContactNote(sourceId, 'Source notes to transfer')

    await page.goto(`/contacts/${targetId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: targetName })).toBeVisible({ timeout: 15000 })

    // Open merge modal
    await page.getByRole('button', { name: /Merge/i }).click()

    // Select source contact
    await selectMergeSource(page, testApi.prefix, sourceName)

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
    await expect(page.getByText('+1-555-0100')).toBeVisible({ timeout: 5000 })

    // Verify source contact's notes were transferred
    await expect(page.getByText('Source notes to transfer')).toBeVisible()

    // The outcome banner is reported, then auto-dismisses after its timeout.
    await expect(page.getByText(/merged successfully/i)).not.toBeVisible({ timeout: 10000 })
  })

  test('should show quick-fill name option when source has different name', async ({ page }) => {
    // spec: CON-043.merged-name-editable
    // Create contacts with different names
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'QuickFill Target',
      },
      {
        full_name: 'QuickFill Source Name',
      },
    ])

    const targetId = ids[0]
    const targetName = `${testApi.prefix}-QuickFill Target`
    const sourceName = `${testApi.prefix}-QuickFill Source Name`

    await page.goto(`/contacts/${targetId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: targetName })).toBeVisible({ timeout: 15000 })

    // Open merge modal
    await page.getByRole('button', { name: /Merge/i }).click()

    // Select source contact
    await selectMergeSource(page, testApi.prefix, sourceName)

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
    // Create a contact
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Disabled Button Test',
      },
    ])

    const contactId = ids[0]
    const fullName = `${testApi.prefix}-Disabled Button Test`

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
    const { ids } = await testApi.seedContacts([
      { full_name: 'Preview Loading Target' },
      { full_name: 'Preview Loading Source' },
    ])
    const targetId = ids[0]
    const targetName = `${testApi.prefix}-Preview Loading Target`
    const sourceName = `${testApi.prefix}-Preview Loading Source`

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
    await selectMergeSource(page, testApi.prefix, sourceName)

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
    const { ids } = await testApi.seedContacts([
      { full_name: 'In Flight Target' },
      { full_name: 'In Flight Source' },
    ])
    const targetId = ids[0]
    const targetName = `${testApi.prefix}-In Flight Target`
    const sourceName = `${testApi.prefix}-In Flight Source`

    await page.goto(`/contacts/${targetId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: targetName })).toBeVisible({ timeout: 15000 })

    await page.getByRole('button', { name: /Merge/i }).click()

    // Select the source and wait for the preview to settle.
    await selectMergeSource(page, testApi.prefix, sourceName)
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
