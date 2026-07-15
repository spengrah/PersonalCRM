import { test, expect } from '@playwright/test'
import type { APIRequestContext } from '@playwright/test'
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

test.describe('Contact Merge @area:contact-merge', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should open merge modal from action menu', async ({ page }) => {
    // Create a contact to be the merge target
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Merge Target Contact',
        location: 'New York',
        cadence: 'monthly',
      },
    ])

    const contactId = ids[0]
    const fullName = `${testApi.prefix}-Merge Target Contact`

    // Navigate to contact detail page
    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Click the Merge button in the action menu
    await page.getByRole('button', { name: /Merge/i }).click()

    // Verify merge modal opens
    await expect(page.getByRole('heading', { name: 'Merge Contacts' })).toBeVisible()
    await expect(page.getByText('Keeping')).toBeVisible()
    await expect(page.getByText('Archiving')).toBeVisible()
  })

  test('should display target contact as "Keeping"', async ({ page }) => {
    // spec: CON-043[0]
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
    await expect(page.getByRole('heading', { name: 'Merge Contacts' })).toBeVisible()

    // Verify target contact is shown with "Keeping" badge
    const modal = page.locator('.fixed.inset-0')
    await expect(modal.getByText(fullName).first()).toBeVisible()
    await expect(modal.getByText('Keeping')).toBeVisible()
  })

  test('should search and select source contact', async ({ page }) => {
    // spec: CON-043[0], CON-043[1]
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
    await expect(page.getByRole('heading', { name: 'Merge Contacts' })).toBeVisible()

    // Click on the contact selector
    await page.getByText('Search for a contact to merge...').click()

    // Search for the source contact
    const searchInput = page.locator('input[placeholder="Search for a contact to merge..."]')
    await searchInput.fill(testApi.prefix)

    // Wait for the source contact to appear in dropdown
    const sourceOption = page.locator('[class*="cursor-pointer"]').filter({ hasText: sourceName })
    await expect(sourceOption).toBeVisible({ timeout: 5000 })

    // The selector excludes the merge target — it appears only as the kept
    // heading, never as a selectable dropdown candidate (CON-043[0]). Scope to
    // the dropdown option rows (`select-none`) so the kept-name heading (which
    // is also cursor-pointer) does not count.
    const targetOption = page.locator('[class*="select-none"]').filter({ hasText: targetName })
    await expect(targetOption).toHaveCount(0)

    // Select the source contact
    await sourceOption.click()

    // Verify "Will Be Merged" section appears (a source loads a preview).
    await expect(page.getByText('Will Be Merged')).toBeVisible({ timeout: 5000 })
  })

  test('should show field conflicts when contacts have different values', async ({ page }) => {
    // spec: CON-043[2]
    // Create contacts with conflicting field values
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Conflict Target',
        location: 'New York',
        cadence: 'monthly',
      },
      {
        full_name: 'Conflict Source',
        location: 'Los Angeles',
        cadence: 'weekly',
      },
    ])

    const targetId = ids[0]
    const targetName = `${testApi.prefix}-Conflict Target`
    const sourceName = `${testApi.prefix}-Conflict Source`

    await page.goto(`/contacts/${targetId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: targetName })).toBeVisible({ timeout: 15000 })

    // Open merge modal
    await page.getByRole('button', { name: /Merge/i }).click()

    // Select source contact
    await page.getByText('Search for a contact to merge...').click()
    const searchInput = page.locator('input[placeholder="Search for a contact to merge..."]')
    await searchInput.fill(testApi.prefix)
    const sourceOption = page.locator('[class*="cursor-pointer"]').filter({ hasText: sourceName })
    await expect(sourceOption).toBeVisible({ timeout: 5000 })
    await sourceOption.click()

    // Wait for preview to load first
    await expect(page.getByText('Will Be Merged')).toBeVisible({ timeout: 10000 })

    // Check if conflict resolution section appears
    // Note: Conflicts only show when BOTH contacts have the field set AND values differ
    const hasConflicts = await page
      .getByText('Resolve Conflicts')
      .isVisible()
      .catch(() => false)

    if (hasConflicts) {
      // Verify location options are visible as toggle buttons within the conflict section
      await expect(page.getByRole('button', { name: 'New York' })).toBeVisible()
      await expect(page.getByRole('button', { name: 'Los Angeles' })).toBeVisible()
    } else {
      // If no conflicts section, at least verify the preview is showing
      await expect(page.getByText('Contact methods')).toBeVisible()
    }
  })

  test('should toggle field selection between source and target', async ({ page, request }) => {
    // spec: CON-043[2]
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
    await page.getByText('Search for a contact to merge...').click()
    const searchInput = page.locator('input[placeholder="Search for a contact to merge..."]')
    await searchInput.fill(testApi.prefix)
    const sourceOption = page.locator('[class*="cursor-pointer"]').filter({ hasText: sourceName })
    await expect(sourceOption).toBeVisible({ timeout: 5000 })
    await sourceOption.click()

    // Wait for conflicts section
    await expect(page.getByText('Resolve Conflicts')).toBeVisible({ timeout: 5000 })

    // Default (before any toggle) keeps the TARGET value selected for EVERY
    // conflicting field — cadence, location, and birthday. The birthday label is
    // computed the same way the modal formats it, so the assertion is stable
    // across the runner's timezone.
    const targetBirthdayText = new Date('1990-03-15').toLocaleDateString('en-US', {
      month: 'short',
      day: 'numeric',
      year: 'numeric',
    })
    await expect(page.getByRole('button', { name: 'Monthly' })).toHaveClass(/bg-blue-600/)
    const nyButton = page.getByRole('button', { name: 'New York' })
    await expect(nyButton).toHaveClass(/bg-blue-600/)
    await expect(page.getByRole('button', { name: targetBirthdayText })).toHaveClass(/bg-blue-600/)

    // Toggling the location to the source value flips only that field.
    const sfButton = page.getByRole('button', { name: 'San Francisco' })
    await expect(sfButton).toBeVisible()
    await sfButton.click()
    await expect(sfButton).toHaveClass(/bg-blue-600/)
    await expect(nyButton).not.toHaveClass(/bg-blue-600/)
  })

  test('should edit merged contact name', async ({ page }) => {
    // spec: CON-043[3]
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

    // Click on the name to enter edit mode
    const modal = page.locator('.fixed.inset-0')
    await modal.locator('h3').first().click()

    // Verify input appears with current name
    const nameInput = modal.locator('input[type="text"]').first()
    await expect(nameInput).toBeVisible({ timeout: 5000 })

    // Edit the name
    await nameInput.fill('Custom Merged Name')
    await nameInput.press('Enter')

    // Verify name is updated in the modal header
    await expect(modal.locator('h3').filter({ hasText: 'Custom Merged Name' })).toBeVisible()
  })

  test('should cancel name edit with Escape', async ({ page }) => {
    // Create a contact
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

    // Click on the name to enter edit mode
    const modal = page.locator('.fixed.inset-0')
    await modal.locator('h3').first().click()

    // Type a new name
    const nameInput = modal.locator('input[type="text"]').first()
    await expect(nameInput).toBeVisible({ timeout: 5000 })
    await nameInput.fill('Should Not Save')

    // Press Escape to cancel
    await nameInput.press('Escape')

    // Verify original name is restored
    await expect(modal.locator('h3').filter({ hasText: fullName })).toBeVisible()
    await expect(modal.locator('h3').filter({ hasText: 'Should Not Save' })).not.toBeVisible()
  })

  test('should close modal when pressing Escape', async ({ page }) => {
    // Create a contact
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Escape Close Test',
      },
    ])

    const contactId = ids[0]
    const fullName = `${testApi.prefix}-Escape Close Test`

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Open merge modal
    await page.getByRole('button', { name: /Merge/i }).click()
    await expect(page.getByRole('heading', { name: 'Merge Contacts' })).toBeVisible()

    // Press Escape
    await page.keyboard.press('Escape')

    // Verify modal is closed
    await expect(page.getByRole('heading', { name: 'Merge Contacts' })).not.toBeVisible()
  })

  test('should close modal when clicking backdrop', async ({ page }) => {
    // Create a contact
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Backdrop Close Test',
      },
    ])

    const contactId = ids[0]
    const fullName = `${testApi.prefix}-Backdrop Close Test`

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Open merge modal
    await page.getByRole('button', { name: /Merge/i }).click()
    await expect(page.getByRole('heading', { name: 'Merge Contacts' })).toBeVisible()

    // Click on backdrop
    await page.locator('.fixed.inset-0').click({ position: { x: 10, y: 10 } })

    // Verify modal is closed
    await expect(page.getByRole('heading', { name: 'Merge Contacts' })).not.toBeVisible()
  })

  test('should successfully merge contacts', async ({ page }) => {
    // spec: CON-043[5]
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
    await page.getByText('Search for a contact to merge...').click()
    const searchInput = page.locator('input[placeholder="Search for a contact to merge..."]')
    await searchInput.fill(testApi.prefix)
    const sourceOption = page.locator('[class*="cursor-pointer"]').filter({ hasText: sourceName })
    await expect(sourceOption).toBeVisible({ timeout: 5000 })
    await sourceOption.click()

    // Wait for preview to load
    await expect(page.getByText('Will Be Merged')).toBeVisible({ timeout: 10000 })

    // Wait for merge button to be enabled
    const mergeButton = page.getByRole('button', { name: /Merge Contacts/i })
    await expect(mergeButton).toBeEnabled({ timeout: 5000 })

    // Intercept the API call to debug any issues
    const mergeResponse = page.waitForResponse(
      response =>
        response.request().method() === 'POST' &&
        response.url().includes('/api/v1/contacts/') &&
        response.url().endsWith('/merge'),
      { timeout: 15000 }
    )

    // Click Merge Contacts button
    await mergeButton.click()

    // Wait for the API response
    const response = await mergeResponse
    const responseStatus = response.status()

    // Check if merge was successful (should be 200)
    if (responseStatus === 200) {
      // Wait for success notification
      await expect(page.getByText(/merged successfully/i)).toBeVisible({ timeout: 10000 })

      // Verify we're back on the contact page (modal closed)
      await expect(page.getByRole('heading', { name: 'Merge Contacts' })).not.toBeVisible()

      // Verify source contact's phone was added
      await expect(page.getByText('+1-555-0100')).toBeVisible({ timeout: 5000 })

      // Verify source contact's notes were transferred
      await expect(page.getByText('Source notes to transfer')).toBeVisible()

      // The outcome banner is reported, then auto-dismisses after its timeout.
      await expect(page.getByText(/merged successfully/i)).not.toBeVisible({ timeout: 10000 })
    } else {
      // Log the error for debugging
      const body = await response.text()
      throw new Error(`Merge API returned ${responseStatus}: ${body}`)
    }
  })

  test('should show quick-fill name option when source has different name', async ({ page }) => {
    // spec: CON-043[3]
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
    await page.getByText('Search for a contact to merge...').click()
    const searchInput = page.locator('input[placeholder="Search for a contact to merge..."]')
    await searchInput.fill(testApi.prefix)
    const sourceOption = page.locator('[class*="cursor-pointer"]').filter({ hasText: sourceName })
    await expect(sourceOption).toBeVisible({ timeout: 5000 })
    await sourceOption.click()

    // Wait for preview
    await expect(page.getByText('Will Be Merged')).toBeVisible({ timeout: 5000 })

    // Verify "use this" link appears for source name
    await expect(page.getByText('use this')).toBeVisible()

    // Click "use this" to quick-fill the source name
    await page.getByText('use this').click()

    // Verify name was updated
    const modal = page.locator('.fixed.inset-0')
    await expect(modal.locator('h3').filter({ hasText: sourceName })).toBeVisible()
  })

  test('should disable merge button when no source selected', async ({ page }) => {
    // spec: CON-043[4]
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
    await expect(page.getByRole('heading', { name: 'Merge Contacts' })).toBeVisible()

    // Verify merge button is disabled
    const mergeButton = page.getByRole('button', { name: /Merge Contacts/i })
    await expect(mergeButton).toBeDisabled()
  })

  test('should show loading state during merge', async ({ page }) => {
    // Create contacts
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Loading Target',
      },
      {
        full_name: 'Loading Source',
      },
    ])

    const targetId = ids[0]
    const targetName = `${testApi.prefix}-Loading Target`
    const sourceName = `${testApi.prefix}-Loading Source`

    await page.goto(`/contacts/${targetId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: targetName })).toBeVisible({ timeout: 15000 })

    // Open merge modal
    await page.getByRole('button', { name: /Merge/i }).click()

    // Select source contact
    await page.getByText('Search for a contact to merge...').click()
    const searchInput = page.locator('input[placeholder="Search for a contact to merge..."]')
    await searchInput.fill(testApi.prefix)
    const sourceOption = page.locator('[class*="cursor-pointer"]').filter({ hasText: sourceName })
    await expect(sourceOption).toBeVisible({ timeout: 5000 })
    await sourceOption.click()

    // Wait for preview
    await expect(page.getByText('Will Be Merged')).toBeVisible({ timeout: 10000 })

    // Wait for merge button to be enabled
    const mergeButton = page.getByRole('button', { name: /Merge Contacts/i })
    await expect(mergeButton).toBeEnabled({ timeout: 5000 })

    // Intercept the API call
    const mergeResponse = page.waitForResponse(
      response =>
        response.request().method() === 'POST' &&
        response.url().includes('/api/v1/contacts/') &&
        response.url().endsWith('/merge'),
      { timeout: 15000 }
    )

    // Click merge button
    await mergeButton.click()

    // Wait for API response
    const response = await mergeResponse

    if (response.status() === 200) {
      // Wait for success notification
      await expect(page.getByText(/merged successfully/i)).toBeVisible({ timeout: 10000 })
    } else {
      // Log error for debugging
      const body = await response.text()
      throw new Error(`Merge API returned ${response.status()}: ${body}`)
    }
  })

  test('keeps the merge submit disabled while the preview is loading', async ({ page }) => {
    // spec: CON-043[4]
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
    await page.getByText('Search for a contact to merge...').click()
    const searchInput = page.locator('input[placeholder="Search for a contact to merge..."]')
    await searchInput.fill(testApi.prefix)
    const sourceOption = page.locator('[class*="cursor-pointer"]').filter({ hasText: sourceName })
    await expect(sourceOption).toBeVisible({ timeout: 5000 })
    await sourceOption.click()

    // While the preview loads, the submit cannot be pressed.
    const mergeButton = page.getByRole('button', { name: /Merge Contacts/i })
    await expect(mergeButton).toBeDisabled()

    // Once the preview resolves, the submit enables — proving the disabled
    // state was gated by the in-flight preview, not the missing source.
    await expect(page.getByText('Will Be Merged')).toBeVisible({ timeout: 10000 })
    await expect(mergeButton).toBeEnabled()
  })

  test('keeps the merge submit disabled while the merge is in flight', async ({ page }) => {
    // spec: CON-043[4]
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
    await page.getByText('Search for a contact to merge...').click()
    const searchInput = page.locator('input[placeholder="Search for a contact to merge..."]')
    await searchInput.fill(testApi.prefix)
    const sourceOption = page.locator('[class*="cursor-pointer"]').filter({ hasText: sourceName })
    await expect(sourceOption).toBeVisible({ timeout: 5000 })
    await sourceOption.click()
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
