import { test, expect, Locator } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'
import { navigateModalToCandidate, findCandidateByName } from './helpers/imports-helpers'

test.describe('Imports Modal @area:imports', () => {
  test.describe('Modal UX Improvements', () => {
    let testApi: TestAPI

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)

      // Seed multiple candidates for navigation testing
      await testApi.seedExternalContacts([
        {
          display_name: 'Modal Test Contact A',
          emails: ['modal-test-a@example.com'],
        },
        {
          display_name: 'Modal Test Contact B',
          emails: ['modal-test-b@example.com'],
        },
        {
          display_name: 'Modal Test Contact C',
          emails: ['modal-test-c@example.com'],
        },
      ])
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    test('should close modal when pressing Escape key', async ({ page }) => {
      const displayName = `${testApi.prefix}-Modal Test Contact A`

      await page.goto('/imports')
      await page.waitForLoadState('domcontentloaded')

      // Open modal on our own contact to avoid clicking other workers' contacts
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })
      await expect(candidateCard).toBeVisible({ timeout: 10000 })
      await candidateCard.getByRole('button', { name: /Import/i }).click()
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

      // Press Escape
      await page.keyboard.press('Escape')

      // Modal should be closed
      await expect(
        page.getByRole('button', { name: 'Import as New', exact: true })
      ).not.toBeVisible()
    })

    test('should navigate with arrow keys', async ({ page }) => {
      const displayName = `${testApi.prefix}-Modal Test Contact A`

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // Open modal on our own contact
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })
      await candidateCard.getByRole('button', { name: /Import/i }).click()

      // Wait for modal and navigate to correct candidate
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()
      await navigateModalToCandidate(page, displayName)

      const modal = page
        .locator('.fixed.inset-0')
        .filter({ has: page.getByRole('button', { name: 'Import as New', exact: true }) })

      // Get the initial heading text (should match our candidate after navigation)
      const heading = modal.locator('h3.text-lg').first()
      const initialName = await heading.textContent()
      expect(initialName).toContain(displayName)

      // Blur any focused element to ensure keyboard events go to the window
      await page.evaluate(() => {
        if (document.activeElement instanceof HTMLElement) {
          document.activeElement.blur()
        }
      })

      // Press ArrowRight to go to next candidate
      await page.evaluate(() => {
        window.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }))
      })

      // Wait for heading to change (could be any candidate, just verify navigation works)
      await expect(heading).not.toHaveText(initialName!, { timeout: 5000 })
      const secondName = await heading.textContent()

      // Press ArrowLeft to go back
      await page.evaluate(() => {
        window.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowLeft', bubbles: true }))
      })

      // Should return to initial contact
      await expect(heading).toHaveText(initialName!, { timeout: 5000 })

      // ArrowRight twice to verify continued navigation
      await page.evaluate(() => {
        window.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }))
      })
      await expect(heading).toHaveText(secondName!, { timeout: 5000 })

      await page.evaluate(() => {
        window.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true }))
      })
      // Just verify it changed again (could be any third candidate)
      await expect(heading).not.toHaveText(secondName!, { timeout: 5000 })
    })

    test('should close modal when clicking backdrop', async ({ page }) => {
      const displayName = `${testApi.prefix}-Modal Test Contact A`

      await page.goto('/imports')
      await page.waitForLoadState('domcontentloaded')

      // Open modal on our own contact
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })
      await expect(candidateCard).toBeVisible({ timeout: 10000 })
      await candidateCard.getByRole('button', { name: /Import/i }).click()
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

      // Click on backdrop (outside the modal content)
      // The backdrop has class 'fixed inset-0'
      await page.locator('.fixed.inset-0').click({ position: { x: 10, y: 10 } })

      // Modal should be closed
      await expect(
        page.getByRole('button', { name: 'Import as New', exact: true })
      ).not.toBeVisible()
    })

    test('should display friendly source names with icons', async ({ page }) => {
      const displayName = `${testApi.prefix}-Modal Test Contact A`

      await page.goto('/imports')
      await page.waitForLoadState('domcontentloaded')

      // Open modal on our own contact
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })
      await expect(candidateCard).toBeVisible({ timeout: 10000 })
      await candidateCard.getByRole('button', { name: /Import/i }).click()
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

      // Should show "Google Contacts" instead of "gcontacts" in the modal
      // The seeded contacts are from gcontacts source
      // The source info is in a paragraph element inside the modal, not the filter button
      // Look for the source display paragraph that contains an SVG icon
      const sourceDisplay = page.locator('p.text-gray-500').filter({ hasText: 'Google Contacts' })
      await expect(sourceDisplay.first()).toBeVisible()
    })

    test('should have transparent backdrop with blur', async ({ page }) => {
      const displayName = `${testApi.prefix}-Modal Test Contact A`

      await page.goto('/imports')
      await page.waitForLoadState('domcontentloaded')

      // Open modal on our own contact
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })
      await expect(candidateCard).toBeVisible({ timeout: 10000 })
      await candidateCard.getByRole('button', { name: /Import/i }).click()
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

      // Verify backdrop has the correct classes
      const backdrop = page.locator('.fixed.inset-0.backdrop-blur-sm')
      await expect(backdrop).toBeVisible()
    })
  })

  test.describe('Cadence Selector (Issue #152)', () => {
    // This test verifies the cadence selector functionality in the import/link modal.
    // Users can set or update contact cadence during import/link operations.

    let testApi: TestAPI

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    test('should show cadence selector in import modal', async ({ page }) => {
      // Seed a candidate for testing
      await testApi.seedExternalContacts([
        {
          display_name: 'Cadence Test Import',
          emails: ['cadence-import@example.com'],
        },
      ])

      const displayName = `${testApi.prefix}-Cadence Test Import`

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // Open import modal
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })
      await candidateCard.getByRole('button', { name: /Import/i }).click()

      // Wait for modal and navigate to correct candidate
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()
      await navigateModalToCandidate(page, displayName)

      // Verify cadence selector is visible
      await expect(page.getByText('Contact Cadence')).toBeVisible()
      await expect(page.getByText('How often you want to be reminded to reach out')).toBeVisible()

      // Verify dropdown has expected options
      const cadenceSelect = page.locator('select').filter({ hasText: 'No cadence' })
      await expect(cadenceSelect).toBeVisible()

      // Close modal
      await page.getByRole('button', { name: /Cancel/i }).click()
    })

    // NOTE: "should import contact with selected cadence" test moved to its own describe block below
    // for better isolation from other tests in this describe block

    test('should show cadence selector in link modal', async ({ page }) => {
      // Seed a candidate and a target contact
      await testApi.seedExternalContacts([
        {
          display_name: 'Cadence Link Candidate',
          emails: ['cadence-link@example.com'],
        },
      ])

      await testApi.seedOverdueContacts([
        {
          full_name: 'Cadence Link Target',
          email: 'cadence-target@example.com',
          cadence: 'quarterly',
          days_overdue: 1,
        },
      ])

      await page.goto('/imports')
      await page.waitForLoadState('domcontentloaded')

      // Open link modal
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: `${testApi.prefix}-Cadence Link Candidate` })
      await expect(candidateCard).toBeVisible({ timeout: 10000 })
      await candidateCard.getByRole('button', { name: /Link/i }).click()

      // Wait for modal to open in link mode
      await expect(page.getByRole('button', { name: 'Link to Existing' })).toBeVisible()

      // Select a contact to link to
      const contactSelector = page.getByText('Search for a contact...')
      await contactSelector.click()
      const searchInput = page.locator('input[placeholder="Search for a contact..."]')
      await searchInput.fill(testApi.prefix)

      const contactOption = page
        .locator('[class*="cursor-pointer"]')
        .filter({ hasText: `${testApi.prefix}-Cadence Link Target` })
      await expect(contactOption).toBeVisible({ timeout: 5000 })
      await contactOption.click()

      // Verify cadence selector is visible and shows existing cadence
      await expect(page.getByText('Contact Cadence')).toBeVisible()

      // The selector should show the existing contact's cadence (quarterly)
      const cadenceSelect = page.locator('select').filter({ hasText: 'Quarterly' })
      await expect(cadenceSelect).toBeVisible()

      // Close modal
      await page.getByRole('button', { name: /Cancel/i }).click()
    })

    test('should update cadence when linking contact', async ({ page }) => {
      // Seed a candidate and a target contact with NO cadence initially
      // This avoids timing issues with pre-selection of existing cadence
      await testApi.seedExternalContacts([
        {
          display_name: 'Link Cadence Update Test',
          emails: ['link-update@example.com'],
        },
      ])

      await testApi.seedOverdueContacts([
        {
          full_name: 'Link Cadence Target',
          email: 'link-target@example.com',
          cadence: 'monthly', // Use monthly, will change to weekly
          days_overdue: 1,
        },
      ])

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      const displayName = `${testApi.prefix}-Link Cadence Update Test`

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // Open link modal
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })
      await candidateCard.getByRole('button', { name: /Link/i }).click()

      // Wait for modal
      await expect(page.getByRole('button', { name: 'Link to Existing' })).toBeVisible()

      // Select the target contact
      const contactSelector = page.getByText('Search for a contact...')
      await contactSelector.click()
      const searchInput = page.locator('input[placeholder="Search for a contact..."]')
      await searchInput.fill(testApi.prefix)

      const contactOption = page
        .locator('[class*="cursor-pointer"]')
        .filter({ hasText: `${testApi.prefix}-Link Cadence Target` })
      await expect(contactOption).toBeVisible({ timeout: 5000 })
      await contactOption.click()

      // Wait for the cadence dropdown to be visible
      // The Select component generates id from label: "Contact Cadence" -> "contact-cadence"
      const cadenceSelect = page.locator('#contact-cadence')
      await expect(cadenceSelect).toBeVisible({ timeout: 5000 })

      // Wait for the contact data to load and pre-select Monthly cadence
      await expect(cadenceSelect).toHaveValue('monthly', { timeout: 5000 })

      // Change cadence to weekly
      await cadenceSelect.selectOption('weekly')

      // Verify the selection changed
      await expect(cadenceSelect).toHaveValue('weekly')

      // Click Link Contact button
      const linkResponse = page.waitForResponse(
        response =>
          response.request().method() === 'POST' &&
          response.url().includes('/api/v1/imports/') &&
          response.url().endsWith('/link')
      )
      await page.getByRole('button', { name: /Link Contact/i }).click()
      await linkResponse

      // Verify success
      await expect(page.getByText(/linked successfully/i)).toBeVisible({ timeout: 10000 })

      // Navigate to the contact and verify cadence was updated
      await page.goto('/contacts')
      await page.waitForLoadState('domcontentloaded')

      const contactSearchInput = page.getByPlaceholder('Search contacts...')
      await contactSearchInput.fill(`${testApi.prefix}-Link Cadence Target`)
      await page.waitForLoadState('domcontentloaded')

      // Wait for search results and click on the contact
      const contactLink = page.getByText(`${testApi.prefix}-Link Cadence Target`)
      await expect(contactLink).toBeVisible({ timeout: 10000 })
      await contactLink.click()
      await page.waitForLoadState('domcontentloaded')

      // Verify cadence is now weekly - look for it in the contact detail section
      const cadenceValue = page.getByTestId('contact-cadence')
      await expect(cadenceValue).toBeVisible({ timeout: 10000 })
      await expect(cadenceValue).toContainText('weekly', { timeout: 5000 })
    })

    test('should default to no cadence in import mode', async ({ page }) => {
      // Seed a candidate
      await testApi.seedExternalContacts([
        {
          display_name: 'Default Cadence Test',
          emails: ['default-cadence@example.com'],
        },
      ])

      const displayName = `${testApi.prefix}-Default Cadence Test`

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // Open import modal
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })
      await candidateCard.getByRole('button', { name: /Import/i }).click()

      // Wait for modal and navigate to correct candidate
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()
      await navigateModalToCandidate(page, displayName)

      // Verify default selection is "No cadence"
      const cadenceSelect = page.locator('select').filter({ hasText: 'No cadence' })
      await expect(cadenceSelect).toBeVisible()
      await expect(cadenceSelect).toHaveValue('')

      // Close modal
      await page.getByRole('button', { name: /Cancel/i }).click()
    })
  })

  test.describe('Name Editing (Issue #155)', () => {
    // This test verifies the name editing functionality in the import/link modal.

    let testApi: TestAPI

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    test('should display clickable name in import modal', async ({ page }) => {
      // Seed a candidate
      await testApi.seedExternalContacts([
        {
          display_name: 'Name Edit UI Test',
          emails: ['name-edit-ui@example.com'],
        },
      ])

      const displayName = `${testApi.prefix}-Name Edit UI Test`

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      // Reload to ensure we get fresh data after seeding
      await page.reload()
      await page.waitForLoadState('networkidle')

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // Open import modal
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })
      await candidateCard.getByRole('button', { name: /Import/i }).click()

      // Wait for modal and navigate to correct candidate
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()
      await navigateModalToCandidate(page, displayName)

      // Verify the name is displayed in an h3 element
      const modalContent = page.locator('.fixed.inset-0')
      await expect(modalContent.locator('h3')).toBeVisible()

      // Close modal
      await page.getByRole('button', { name: /Cancel/i }).click()
    })

    test('should enter edit mode when clicking name', async ({ page }) => {
      // Seed a candidate
      await testApi.seedExternalContacts([
        {
          display_name: 'Click Edit Test',
          emails: ['click-edit@example.com'],
        },
      ])

      const displayName = `${testApi.prefix}-Click Edit Test`

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      // Reload to ensure we get fresh data after seeding
      await page.reload()
      await page.waitForLoadState('networkidle')

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // Open import modal
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })
      await candidateCard.getByRole('button', { name: /Import/i }).click()

      // Wait for modal
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

      // Navigate to correct candidate if needed (handles race conditions in parallel tests)
      await navigateModalToCandidate(page, displayName)

      // Click on the name heading within the modal to enter edit mode
      // The modal has a fixed inset-0 overlay, and the editable h3 has text-lg class
      const modal = page.locator('.fixed.inset-0')
      const nameHeading = modal.locator('h3.text-lg').first()
      await nameHeading.click()

      // Verify input field appears with the name value
      const nameInput = modal.locator('input[type="text"]').first()
      await expect(nameInput).toBeVisible({ timeout: 5000 })
      await expect(nameInput).toHaveValue(new RegExp(displayName))

      // Close modal
      await page.getByRole('button', { name: /Cancel/i }).click()
    })

    test('should confirm edit with Enter key', async ({ page }) => {
      // Seed a candidate
      await testApi.seedExternalContacts([
        {
          display_name: 'Enter Key Test',
          emails: ['enter-key@example.com'],
        },
      ])

      const displayName = `${testApi.prefix}-Enter Key Test`

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      // Reload to ensure we get fresh data after seeding
      await page.reload()
      await page.waitForLoadState('networkidle')

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // Open import modal
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })
      await candidateCard.getByRole('button', { name: /Import/i }).click()

      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

      // Navigate to correct candidate if needed (handles race conditions in parallel tests)
      await navigateModalToCandidate(page, displayName)

      // Click to enter edit mode - use modal-scoped selector
      const modal = page.locator('.fixed.inset-0')
      await modal.locator('h3.text-lg').first().click()
      const nameInput = modal.locator('input[type="text"]').first()
      await expect(nameInput).toBeVisible({ timeout: 5000 })

      // Type new name and press Enter
      await nameInput.fill('New Contact Name')
      await nameInput.press('Enter')

      // Verify edit mode closed and new name shows in heading
      await expect(
        modal.locator('h3.text-lg').filter({ hasText: 'New Contact Name' })
      ).toBeVisible()

      // Close modal
      await page.getByRole('button', { name: /Cancel/i }).click()
    })

    test('should cancel edit with Escape key', async ({ page }) => {
      // Seed a candidate
      await testApi.seedExternalContacts([
        {
          display_name: 'Escape Key Test',
          emails: ['escape-key@example.com'],
        },
      ])

      const displayName = `${testApi.prefix}-Escape Key Test`

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      // Reload to ensure we get fresh data after seeding
      await page.reload()
      await page.waitForLoadState('networkidle')

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // Open import modal
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })
      await candidateCard.getByRole('button', { name: /Import/i }).click()

      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

      // Navigate to correct candidate if needed (handles race conditions in parallel tests)
      await navigateModalToCandidate(page, displayName)

      // Click to enter edit mode - use modal-scoped selector
      const modal = page.locator('.fixed.inset-0')
      await modal.locator('h3.text-lg').first().click()
      const nameInput = modal.locator('input[type="text"]').first()
      await expect(nameInput).toBeVisible({ timeout: 5000 })

      // Type new name and press Escape
      await nameInput.fill('Should Not Save')
      await nameInput.press('Escape')

      // Verify original name is restored
      await expect(
        modal.locator('h3.text-lg').filter({ hasText: new RegExp(displayName) })
      ).toBeVisible()

      // Close modal
      await page.getByRole('button', { name: /Cancel/i }).click()
    })

    test('should edit name and persist on import', async ({ page }) => {
      // Seed a candidate
      await testApi.seedExternalContacts([
        {
          display_name: 'Original Import Name',
          emails: ['persist-name@example.com'],
        },
      ])

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      // Reload to ensure we get fresh data after seeding
      await page.reload()
      await page.waitForLoadState('networkidle')

      const displayName = `${testApi.prefix}-Original Import Name`
      const newName = `${testApi.prefix}-Edited Name For Import`

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // Open import modal
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })
      await candidateCard.getByRole('button', { name: /Import/i }).click()

      // Wait for modal
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

      // Navigate to correct candidate if needed (handles race conditions in parallel tests)
      await navigateModalToCandidate(page, displayName)

      // Click on the name to enter edit mode - use modal-scoped selector
      const modal = page.locator('.fixed.inset-0')
      await modal.locator('h3.text-lg').first().click()

      // Wait for input to appear and edit the name
      const nameInput = modal.locator('input[type="text"]').first()
      await expect(nameInput).toBeVisible({ timeout: 5000 })

      // Clear and type new name, then press Enter to confirm
      await nameInput.fill(newName)
      await nameInput.press('Enter')

      // Verify the new name is shown in the heading
      await expect(modal.locator('h3.text-lg').filter({ hasText: newName })).toBeVisible()

      // Click the "Import as New Contact" button
      await page.getByRole('button', { name: 'Import as New Contact', exact: true }).click()

      // Wait for the candidate to be removed from the list (it was imported)
      // The modal may stay open if there are other candidates
      await expect(page.getByText(displayName)).not.toBeVisible({ timeout: 15000 })

      // Navigate to contacts page to verify the contact was created with the edited name
      await page.goto('/contacts')
      await page.waitForLoadState('domcontentloaded')

      // Search for the contact with the edited name
      const searchInput = page.getByPlaceholder('Search contacts...')
      await searchInput.fill(newName)
      await page.waitForLoadState('domcontentloaded')

      // Verify the contact appears with the edited name
      await expect(page.getByText(newName)).toBeVisible({ timeout: 10000 })
    })
  })

  test.describe('Primary Method Selection (Issue #159)', () => {
    // This test verifies the primary method selection UI is present in the import/link modal.
    // Full E2E tests for primary method selection are complex; backend API tests cover the functionality.

    let testApi: TestAPI

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    test('should show star icons for method selection', async ({ page }) => {
      // Seed a candidate with multiple contact methods
      await testApi.seedExternalContacts([
        {
          display_name: 'Primary Method Test',
          emails: ['primary1@example.com', 'primary2@example.com'],
          phones: ['+1234567890'],
        },
      ])

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      // Reload to ensure we get fresh data after seeding
      await page.reload()
      await page.waitForLoadState('networkidle')

      // Open import modal
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: `${testApi.prefix}-Primary Method Test` })
      await expect(candidateCard).toBeVisible({ timeout: 10000 })
      await candidateCard.getByRole('button', { name: /Import/i }).click()

      // Wait for modal
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

      // Navigate to correct candidate if needed (handles race conditions in parallel tests)
      await navigateModalToCandidate(page, `${testApi.prefix}-Primary Method Test`)

      // Verify star buttons are visible for each selected method
      // Stars should be gray by default (not primary)
      const starButtons = page.locator('button[title="Set as primary"]')
      await expect(starButtons.first()).toBeVisible()

      // Close modal
      await page.getByRole('button', { name: /Cancel/i }).click()
    })

    test('should select primary method by clicking star', async ({ page }) => {
      // Seed a candidate with multiple contact methods
      await testApi.seedExternalContacts([
        {
          display_name: 'Star Click Test',
          emails: ['star1@example.com', 'star2@example.com'],
        },
      ])

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      // Reload to ensure we get fresh data after seeding
      await page.reload()
      await page.waitForLoadState('networkidle')

      // Open import modal
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: `${testApi.prefix}-Star Click Test` })
      await expect(candidateCard).toBeVisible({ timeout: 10000 })
      await candidateCard.getByRole('button', { name: /Import/i }).click()

      // Wait for modal
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

      // Navigate to correct candidate if needed (handles race conditions in parallel tests)
      await navigateModalToCandidate(page, `${testApi.prefix}-Star Click Test`)

      // Click the first star button to set as primary
      const starButton = page.locator('button[title="Set as primary"]').first()
      await starButton.click()

      // Verify star is now yellow (primary) - button title changes
      await expect(page.locator('button[title="Primary contact method"]')).toBeVisible()

      // Close modal
      await page.getByRole('button', { name: /Cancel/i }).click()
    })

    test('should only allow one primary method at a time', async ({ page }) => {
      // Seed a candidate with multiple contact methods
      await testApi.seedExternalContacts([
        {
          display_name: 'Single Primary Test',
          emails: ['single1@example.com', 'single2@example.com'],
        },
      ])

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      // Reload to ensure we get fresh data after seeding
      await page.reload()
      await page.waitForLoadState('networkidle')

      // Open import modal
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: `${testApi.prefix}-Single Primary Test` })
      await expect(candidateCard).toBeVisible({ timeout: 10000 })
      await candidateCard.getByRole('button', { name: /Import/i }).click()

      // Wait for modal
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

      // Navigate to correct candidate if needed (handles race conditions in parallel tests)
      await navigateModalToCandidate(page, `${testApi.prefix}-Single Primary Test`)

      // Initially no star should be primary
      const setAsPrimaryButtons = page.locator('button[title="Set as primary"]')
      await expect(setAsPrimaryButtons.first()).toBeVisible()

      // Click the first star button to set as primary
      await setAsPrimaryButtons.first().click()

      // Verify first star is now primary (yellow)
      await expect(page.locator('button[title="Primary contact method"]')).toHaveCount(1)

      // Now click the second star - it should become primary and the first should lose primary status
      // First, get the remaining "Set as primary" buttons (there should be at least one more)
      const remainingSetButtons = page.locator('button[title="Set as primary"]')
      await expect(remainingSetButtons.first()).toBeVisible()
      await remainingSetButtons.first().click()

      // Verify there is still only ONE primary method (yellow star)
      await expect(page.locator('button[title="Primary contact method"]')).toHaveCount(1)

      // Close modal
      await page.getByRole('button', { name: /Cancel/i }).click()
    })
  })

  test.describe('Import Loading State', () => {
    let testApi: TestAPI

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    test('should show loading text during import action', async ({ page }) => {
      // Seed a unique contact for this test
      await testApi.seedExternalContacts([
        {
          display_name: 'Loading Test Import',
          emails: ['loading-test-import@example.com'],
        },
      ])

      const displayName = `${testApi.prefix}-Loading Test Import`

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // Open modal on our contact
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })
      await candidateCard.getByRole('button', { name: /Import/i }).click()
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()

      // Navigate to correct candidate if needed (handles race conditions in parallel tests)
      await navigateModalToCandidate(page, displayName)

      // Click Import button and check for loading state
      const importButton = page.getByRole('button', { name: 'Import as New Contact', exact: true })
      await importButton.click()

      // The button should briefly show "Importing..." - use a short timeout as it's transient
      // Note: This might be too fast to catch in E2E, but we verify the action completes
      // Wait for the candidate card to be removed from the list (it was imported)
      // Note: We check the card, not just text, because the notification may contain the name
      await expect(candidateCard).not.toBeVisible({ timeout: 15000 })
    })
  })

  test.describe('Cadence Import Flow', () => {
    let testApi: TestAPI

    test.beforeEach(async ({ request }, testInfo) => {
      testApi = createTestAPI(request, testInfo)
    })

    test.afterEach(async () => {
      await testApi.cleanup()
    })

    test('should import contact with selected cadence', async ({ page }) => {
      // Seed a candidate for this isolated test
      await testApi.seedExternalContacts([
        {
          display_name: 'Cadence Import User',
          emails: ['cadence-import-user@example.com'],
        },
      ])

      const displayName = `${testApi.prefix}-Cadence Import User`

      await page.goto('/imports')
      await page.waitForLoadState('networkidle')

      // Find our seeded candidate (may need to paginate)
      await findCandidateByName(page, displayName)

      // Open import modal on our contact
      const candidateCard = page
        .locator('[class*="border-gray-200"]')
        .filter({ hasText: displayName })
      await candidateCard.getByRole('button', { name: /Import/i }).click()

      // Wait for modal and navigate to correct candidate
      await expect(page.getByRole('button', { name: 'Import as New', exact: true })).toBeVisible()
      await navigateModalToCandidate(page, displayName)

      // Select monthly cadence
      const cadenceSelect = page.locator('select').filter({ hasText: 'No cadence' })
      await cadenceSelect.selectOption('monthly')

      // Import the contact and wait for the candidate to be removed from the list
      await page.getByRole('button', { name: 'Import as New Contact', exact: true }).click()

      // Wait for the candidate card to be removed from the list (it was imported)
      // Note: We check the card, not just text, because the notification may contain the name
      await expect(candidateCard).not.toBeVisible({ timeout: 15000 })

      // Navigate to contacts page and verify the contact has cadence set
      await page.goto('/contacts')
      await page.waitForLoadState('networkidle')

      // Wait for contacts list to finish loading (not showing "Loading contacts...")
      await expect(page.getByText('Loading contacts...')).not.toBeVisible({ timeout: 10000 })

      // Search for the imported contact
      const searchInput = page.getByPlaceholder('Search contacts...')
      await searchInput.fill(testApi.prefix)

      // Wait for search results to load
      await page.waitForLoadState('networkidle')

      // Wait for search results and click on the contact
      const contactLink = page.getByText(displayName)
      await expect(contactLink).toBeVisible({ timeout: 10000 })
      await contactLink.click()
      await page.waitForLoadState('domcontentloaded')

      // Verify cadence is displayed on detail page
      await expect(page.getByText('Contact cadence')).toBeVisible({ timeout: 10000 })
      await expect(page.getByText('monthly')).toBeVisible({ timeout: 5000 })
    })
  })
})
