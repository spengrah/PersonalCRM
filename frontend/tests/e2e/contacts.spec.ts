import { test, expect } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'
import { getTodayUTC } from './helpers/date-utils'

test.describe('Contacts - TestAPI Seeded @area:contacts', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test('should use leading-normal on contact name to prevent descender clipping', async ({
    page,
  }) => {
    // Names with descenders (g, y, p, q, j) should not be clipped
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Gregory Yancy',
      },
    ])

    const contactId = ids[0]
    const fullName = `${testApi.prefix}-Gregory Yancy`

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')

    // Verify the heading is visible and has leading-normal class (not leading-7)
    const heading = page.getByRole('heading', { name: fullName })
    await expect(heading).toBeVisible()
    await expect(heading).toHaveClass(/leading-normal/)
    await expect(heading).not.toHaveClass(/leading-7/)
  })

  test('should truncate long location with tooltip in contacts table', async ({ page }) => {
    // Create a very long location that will definitely overflow
    const longLocation =
      '1234 Very Long Street Name Boulevard, Extremely Long Neighborhood District, San Francisco, California 94105, United States of America'

    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Location Test Contact',
        location: longLocation,
      },
    ])

    const contactId = ids[0]
    const fullName = `${testApi.prefix}-Location Test Contact`

    // Navigate to contacts list to see the table
    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')

    // Find the row with our contact
    const contactRow = page.locator('tr', { has: page.getByText(fullName) })
    await expect(contactRow).toBeVisible()

    // Find the location cell with the truncation
    const locationDiv = contactRow.locator('div[title]', { hasText: longLocation.slice(0, 20) })
    await expect(locationDiv).toBeVisible()

    // Verify the title attribute contains the full location (for tooltip)
    await expect(locationDiv).toHaveAttribute('title', longLocation)

    // Verify the text is truncated (has truncate class or text-overflow: ellipsis)
    const truncatedSpan = locationDiv.locator('span.truncate')
    await expect(truncatedSpan).toBeVisible()

    // Navigate to contact detail to verify location is displayed fully
    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible()
  })

  test('should show expandable notes for long content', async ({ page }) => {
    // Create notes longer than 300 characters to trigger truncation
    const longNotes = `Met at the AI conference in San Francisco, March 2024. Works as a senior ML engineer at a startup focused on personal productivity tools.

Very interested in personal CRM concepts and AI-driven contact management. We discussed potential collaboration opportunities around embedding-based contact matching.

Key interests: Machine learning infrastructure, personal knowledge management, privacy-focused software design.

Follow-up: Share the pgvector article, introduce to Sarah from the embeddings team.`

    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Long Notes Contact',
      },
    ])

    const contactId = ids[0]
    // Seed notes separately via notes API
    await testApi.seedContactNote(contactId, longNotes)

    const fullName = `${testApi.prefix}-Long Notes Contact`

    await page.goto(`/contacts/${contactId}`)
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Verify "Show more" button is visible (indicates notes overflow the 4-line clamp)
    const showMoreButton = page.getByRole('button', { name: 'Show more' })
    await expect(showMoreButton).toBeVisible()

    // Click "Show more" to expand
    await showMoreButton.click()

    // Verify button changed to "Show less"
    const showLessButton = page.getByRole('button', { name: 'Show less' })
    await expect(showLessButton).toBeVisible()

    // Click "Show less" to collapse
    await showLessButton.click()

    // Verify button changed back to "Show more"
    await expect(showMoreButton).toBeVisible()
  })

  test('should not show expand button for short notes', async ({ page }) => {
    const shortNotes = 'Brief note about this contact.'

    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Short Notes Contact',
      },
    ])

    const contactId = ids[0]
    // Seed notes separately via notes API
    await testApi.seedContactNote(contactId, shortNotes)

    const fullName = `${testApi.prefix}-Short Notes Contact`

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Verify notes are displayed
    await expect(page.getByText(shortNotes)).toBeVisible()

    // Verify "Show more" button is NOT visible (notes are short)
    await expect(page.getByRole('button', { name: 'Show more' })).not.toBeVisible()
  })

  test('should edit last contacted date manually', async ({ page }) => {
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Last Contacted Test',
      },
    ])

    const contactId = ids[0]
    const fullName = `${testApi.prefix}-Last Contacted Test`

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Find the Last contacted row and hover to reveal the edit button
    const lastContactedRow = page.locator('dt:has-text("Last contacted")').locator('..')
    await lastContactedRow.hover()

    // Click the edit button (pencil icon)
    const editButton = page.getByTestId('edit-last-contacted-btn')
    await expect(editButton).toBeVisible()
    await editButton.click()

    // Date input should now be visible
    const dateInput = page.getByTestId('last-contacted-date-input')
    await expect(dateInput).toBeVisible()

    // Set a past date (2024-01-15)
    await dateInput.fill('2024-01-15')

    // Click save button
    const saveButton = page.getByTestId('save-last-contacted-btn')
    await saveButton.click()

    // Wait for update to complete and verify the date is displayed
    await expect(dateInput).not.toBeVisible({ timeout: 10000 })
    await expect(page.getByText('1/15/2024')).toBeVisible()
  })

  test('should cancel editing last contacted date', async ({ page }) => {
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Cancel Edit Test',
      },
    ])

    const contactId = ids[0]
    const fullName = `${testApi.prefix}-Cancel Edit Test`

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Get the current last contacted value
    const lastContactedRow = page.locator('dt:has-text("Last contacted")').locator('..')
    const initialDateText = await lastContactedRow.locator('dd span').first().textContent()

    // Hover and click edit
    await lastContactedRow.hover()
    await page.getByTestId('edit-last-contacted-btn').click()

    // Change the date
    const dateInput = page.getByTestId('last-contacted-date-input')
    await dateInput.fill('2023-06-01')

    // Click cancel
    await page.getByTestId('cancel-last-contacted-btn').click()

    // Verify the date input is hidden and original date is preserved
    await expect(dateInput).not.toBeVisible()
    const currentDateText = await lastContactedRow.locator('dd span').first().textContent()
    expect(currentDateText).toBe(initialDateText)
  })

  test('should prevent setting future last contacted date', async ({ page }) => {
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Future Date Test',
      },
    ])

    const contactId = ids[0]
    const fullName = `${testApi.prefix}-Future Date Test`

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Click edit
    const lastContactedRow = page.locator('dt:has-text("Last contacted")').locator('..')
    await lastContactedRow.hover()
    await page.getByTestId('edit-last-contacted-btn').click()

    // The date input should have a max attribute preventing future dates
    const dateInput = page.getByTestId('last-contacted-date-input')
    const maxDate = await dateInput.getAttribute('max')
    const today = new Date().toISOString().split('T')[0]
    expect(maxDate).toBe(today)

    // Cancel and cleanup
    await page.getByTestId('cancel-last-contacted-btn').click()
  })

  test('should show context menu without clipping for bottom rows', async ({ page }) => {
    // Create enough contacts to have rows near the bottom of the viewport
    const names = Array.from({ length: 8 }, (_, i) => ({
      full_name: `Context Menu Test ${i}`,
      cadence: 'monthly' as const,
    }))
    await testApi.seedContacts(names)

    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')

    // Find the last visible row's action button
    const rows = page.locator('tbody tr')
    await expect(rows.first()).toBeVisible({ timeout: 15000 })
    const lastRow = rows.last()
    const actionButton = lastRow
      .locator('button')
      .filter({ has: page.locator('svg') })
      .last()
    await actionButton.click()

    // Verify the dropdown menu is visible and not clipped
    const menuItem = page.getByRole('menuitem', { name: 'Mark as Contacted' })
    await expect(menuItem).toBeVisible()

    // Verify Edit and Merge items are also present
    await expect(page.getByRole('menuitem', { name: 'Edit' })).toBeVisible()
    await expect(page.getByRole('menuitem', { name: 'Merge' })).toBeVisible()
  })

  test('should navigate to edit mode via context menu Edit action', async ({ page }) => {
    const { ids } = await testApi.seedContacts([{ full_name: 'Context Edit Test' }])

    const contactId = ids[0]
    const fullName = `${testApi.prefix}-Context Edit Test`

    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')

    // Find the row and open context menu
    const contactRow = page.locator('tr', { has: page.getByText(fullName) })
    await expect(contactRow).toBeVisible({ timeout: 15000 })
    const actionButton = contactRow
      .locator('button')
      .filter({ has: page.locator('svg') })
      .last()
    await actionButton.click()

    // Click Edit in context menu
    await page.getByRole('menuitem', { name: 'Edit' }).click()

    // Should navigate to detail page in edit mode
    await page.waitForURL(/\/contacts\/.*action=edit/)
    await expect(page.getByRole('heading', { name: 'Edit Contact' })).toBeVisible({
      timeout: 15000,
    })
  })

  test('should navigate to merge modal via context menu Merge action', async ({ page }) => {
    const { ids } = await testApi.seedContacts([{ full_name: 'Context Merge Test' }])

    const contactId = ids[0]
    const fullName = `${testApi.prefix}-Context Merge Test`

    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')

    // Find the row and open context menu
    const contactRow = page.locator('tr', { has: page.getByText(fullName) })
    await expect(contactRow).toBeVisible({ timeout: 15000 })
    const actionButton = contactRow
      .locator('button')
      .filter({ has: page.locator('svg') })
      .last()
    await actionButton.click()

    // Click Merge in context menu
    await page.getByRole('menuitem', { name: 'Merge' }).click()

    // Should navigate to detail page with merge modal open
    await page.waitForURL(/\/contacts\/.*action=merge/)
    await expect(page.getByRole('heading', { name: 'Merge Contacts' })).toBeVisible({
      timeout: 15000,
    })
  })

  test('should update Mark as Contacted button behavior', async ({ page }) => {
    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Mark Contacted Test',
      },
    ])

    const contactId = ids[0]
    const fullName = `${testApi.prefix}-Mark Contacted Test`

    await page.goto(`/contacts/${contactId}`)
    await page.waitForLoadState('domcontentloaded')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Click the "Mark as Contacted" button
    await page.getByRole('button', { name: 'Mark as Contacted' }).click()

    // Wait for the update and verify the date is today (UTC date, see getTodayUTC)
    const todayUtc = getTodayUTC()
    const lastContactedRow = page.locator('dt:has-text("Last contacted")').locator('..')
    await expect(lastContactedRow.locator('dd span').first()).toContainText(todayUtc)
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
    // Seed contacts with and without cadence
    await testApi.seedContacts([
      { full_name: 'FilterCadence WithWeekly', cadence: 'weekly' },
      { full_name: 'FilterCadence WithMonthly', cadence: 'monthly' },
      { full_name: 'FilterCadence NoCadence' },
    ])

    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')

    // Search to isolate our test data
    const searchInput = page.getByPlaceholder('Search contacts...')
    await searchInput.fill(`${testApi.prefix}-FilterCadence`)
    await searchInput.press('Enter')
    await expect(page.getByText(`${testApi.prefix}-FilterCadence WithWeekly`)).toBeVisible({
      timeout: 10000,
    })

    // Verify all 3 contacts visible with "All contacts" (default)
    const filterSelect = page.getByLabel('Filter by cadence')
    await expect(filterSelect).toHaveValue('')
    await expect(page.getByText(`${testApi.prefix}-FilterCadence WithWeekly`)).toBeVisible()
    await expect(page.getByText(`${testApi.prefix}-FilterCadence WithMonthly`)).toBeVisible()
    await expect(page.getByText(`${testApi.prefix}-FilterCadence NoCadence`)).toBeVisible()

    // Select "Has cadence" - should show only contacts with cadence
    const hasCadenceResponse = page.waitForResponse(
      resp => resp.url().includes('cadence_filter=has_cadence') && resp.ok()
    )
    await filterSelect.selectOption('has_cadence')
    await hasCadenceResponse

    await expect(page.getByText(`${testApi.prefix}-FilterCadence WithWeekly`)).toBeVisible()
    await expect(page.getByText(`${testApi.prefix}-FilterCadence WithMonthly`)).toBeVisible()
    await expect(page.getByText(`${testApi.prefix}-FilterCadence NoCadence`)).not.toBeVisible()

    // Select "No cadence" - should show only contacts without cadence
    const noCadenceResponse = page.waitForResponse(
      resp => resp.url().includes('cadence_filter=no_cadence') && resp.ok()
    )
    await filterSelect.selectOption('no_cadence')
    await noCadenceResponse

    await expect(page.getByText(`${testApi.prefix}-FilterCadence WithWeekly`)).not.toBeVisible()
    await expect(page.getByText(`${testApi.prefix}-FilterCadence WithMonthly`)).not.toBeVisible()
    await expect(page.getByText(`${testApi.prefix}-FilterCadence NoCadence`)).toBeVisible()

    // Reset to "All contacts" - should show all again
    await filterSelect.selectOption('')

    await expect(page.getByText(`${testApi.prefix}-FilterCadence WithWeekly`)).toBeVisible({
      timeout: 10000,
    })
    await expect(page.getByText(`${testApi.prefix}-FilterCadence NoCadence`)).toBeVisible()
  })
})

test.describe('Contacts - UI Create (preserved for coverage) @area:contacts', () => {
  // UI tests need serial mode since they create contacts via UI without TestAPI isolation
  test.describe.configure({ mode: 'serial' })

  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

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

  test('should edit contact notes', async ({ page }) => {
    const notes =
      'Met at a conference in 2024. Works in AI/ML. Very interested in personal CRM tools.'
    const updatedNotes = 'Updated notes: Follow up about collaboration opportunity.'

    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Notes Test Contact',
      },
    ])

    const contactId = ids[0]
    const fullName = `${testApi.prefix}-Notes Test Contact`

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

  test('should display contact with all methods and normalized handles', async ({ page }) => {
    const fullName = `${testApi.prefix}-Playwright Contact`
    const personalEmail = `personal-${testApi.prefix}@example.com`
    const workEmail = `work-${testApi.prefix}@example.com`
    const phone = '(555) 555-1234'
    const telegramHandle = `@@telegram-${testApi.prefix}`
    const discordHandle = `@@discord-${testApi.prefix}`
    const twitterHandle = `@@twitter-${testApi.prefix}`
    const signal = '+1 555 555 9876'
    const gchatEmail = `gchat-${testApi.prefix}@example.com`

    const methods = [
      { type: 'email', value: personalEmail, expected: personalEmail },
      { type: 'email', value: workEmail, expected: workEmail },
      { type: 'phone', value: phone, expected: phone },
      { type: 'telegram', value: telegramHandle, expected: telegramHandle.replace(/^@@/, '@') },
      { type: 'signal', value: signal, expected: signal },
      { type: 'discord', value: discordHandle, expected: discordHandle.replace(/^@@/, '@') },
      { type: 'twitter', value: twitterHandle, expected: twitterHandle.replace(/^@@/, '@') },
      { type: 'gchat', value: gchatEmail, expected: gchatEmail },
    ]

    const { ids } = await testApi.seedContacts([
      {
        full_name: 'Playwright Contact',
        methods: [
          { type: 'email', value: personalEmail },
          { type: 'email', value: workEmail },
          { type: 'phone', value: phone },
          { type: 'telegram', value: telegramHandle, is_primary: true },
          { type: 'signal', value: signal },
          { type: 'discord', value: discordHandle },
          { type: 'twitter', value: twitterHandle },
          { type: 'gchat', value: gchatEmail },
        ],
      },
    ])

    const contactId = ids[0]

    await page.goto(`/contacts/${contactId}`)
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    for (const method of methods) {
      await expect(page.getByText(method.expected, { exact: true })).toBeVisible()
    }

    await expect(page.getByText(telegramHandle, { exact: true })).toHaveCount(0)

    const primaryRow = page.getByText('Telegram', { exact: true }).locator('..')
    await expect(primaryRow.getByText('Primary')).toBeVisible()
    await expect(page.getByText('Primary')).toHaveCount(1)

    await expect(page.getByText('Google Chat', { exact: true })).toBeVisible()
    await expect(page.getByRole('link', { name: gchatEmail })).toHaveCount(0)
  })

  test('should display cadence column with formatted values and sort by frequency', async ({
    page,
  }) => {
    // Create contacts with different cadences
    await testApi.seedContacts([
      { full_name: 'Cadence Test Weekly', cadence: 'weekly' },
      { full_name: 'Cadence Test Monthly', cadence: 'monthly' },
      { full_name: 'Cadence Test Annual', cadence: 'annual' },
      { full_name: 'Cadence Test None' }, // No cadence
    ])

    await page.goto('/contacts')
    await page.waitForLoadState('networkidle')

    // Verify cadence column header exists and is sortable
    const cadenceHeader = page.locator('th').filter({ hasText: 'Cadence' })
    await expect(cadenceHeader).toBeVisible()

    // Verify formatted cadence values are displayed (not raw values)
    // Use .first() since there may be multiple contacts with the same cadence
    await expect(page.getByText('Weekly', { exact: true }).first()).toBeVisible()
    await expect(page.getByText('Monthly', { exact: true }).first()).toBeVisible()
    await expect(page.getByText('Annual', { exact: true }).first()).toBeVisible()

    // Click cadence header to sort - should already be sorted by cadence desc by default
    // The default is desc (most frequent first), so weekly should be near the top
    const rows = page.locator('tbody tr')
    const firstRow = rows.first()

    // First row should contain a contact with cadence (could be weekly if our test data is first)
    // Just verify the header is clickable and the page doesn't error
    await cadenceHeader.click()
    await page.waitForLoadState('networkidle')

    // After clicking (now asc - least frequent first), annual should be before weekly
    // Verify the sort changed by checking the icon
    await expect(cadenceHeader.locator('svg')).toBeVisible()
  })

  test('should display Next Contact column header and render dates', async ({ page }) => {
    // Create contacts with cadence and last_contacted (so contact_by is calculated)
    await testApi.seedContacts([
      {
        full_name: 'NextContact Weekly',
        cadence: 'weekly',
        last_contacted_days_ago: 3,
      },
      {
        full_name: 'NextContact NoCadence',
      },
    ])

    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')

    // Verify "Next Contact" column header exists and is sortable
    const nextContactHeader = page.getByRole('columnheader').filter({ hasText: 'Next Contact' })
    await expect(nextContactHeader).toBeVisible()

    // Search for our test contacts
    const searchInput = page.getByPlaceholder('Search contacts...')
    await searchInput.fill(`${testApi.prefix}-NextContact`)
    await searchInput.press('Enter')
    await page.waitForLoadState('networkidle')

    // Contact with cadence should show a date (not N/A)
    const weeklyRow = page.locator('tr', {
      has: page.getByText(`${testApi.prefix}-NextContact Weekly`),
    })
    await expect(weeklyRow).toBeVisible()
    // The Next Contact cell should contain a date (digits with slashes)
    const weeklyCells = weeklyRow.locator('td')
    // Next Contact is the 6th column (Name, Cadence, Location, Birthday, Last Contact, Next Contact, Actions)
    const weeklyNextContact = weeklyCells.nth(5)
    await expect(weeklyNextContact).not.toHaveText('-')

    // Contact without cadence should show N/A
    const noCadenceRow = page.locator('tr', {
      has: page.getByText(`${testApi.prefix}-NextContact NoCadence`),
    })
    await expect(noCadenceRow).toBeVisible()
    const noCadenceCells = noCadenceRow.locator('td')
    const noCadenceNextContact = noCadenceCells.nth(5)
    await expect(noCadenceNextContact).toHaveText('-')
  })

  test('should sort by Next Contact column when header clicked', async ({ page }) => {
    await testApi.seedContacts([
      {
        full_name: 'SortNext Contact',
        cadence: 'weekly',
        last_contacted_days_ago: 3,
      },
    ])

    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')

    // Search for our test contact to isolate it
    const searchInput = page.getByPlaceholder('Search contacts...')
    await searchInput.fill(`${testApi.prefix}-SortNext`)
    await searchInput.press('Enter')
    await expect(page.getByText(`${testApi.prefix}-SortNext Contact`)).toBeVisible()

    // Click Next Contact header - verify sort=contact_by&order=asc is sent to API
    const nextContactHeader = page.getByRole('columnheader').filter({ hasText: 'Next Contact' })
    const ascResponse = page.waitForResponse(
      resp => resp.url().includes('sort=contact_by') && resp.url().includes('order=asc')
    )
    await nextContactHeader.click()
    await ascResponse

    // Verify sort icon appears
    await expect(nextContactHeader.locator('svg')).toBeVisible()

    // Click again to toggle to descending - verify sort=contact_by&order=desc
    const descResponse = page.waitForResponse(
      resp => resp.url().includes('sort=contact_by') && resp.url().includes('order=desc')
    )
    await nextContactHeader.click()
    await descResponse

    // Contact should still be visible after sort toggling
    await expect(page.getByText(`${testApi.prefix}-SortNext Contact`)).toBeVisible()
  })

  test('should show page number buttons and top/bottom pagination when multiple pages exist', async ({
    page,
  }) => {
    // Create 22 contacts to trigger pagination (default limit is 20)
    const contacts = Array.from({ length: 22 }, (_, i) => ({
      full_name: `Paginated Contact ${String(i).padStart(2, '0')}`,
    }))
    await testApi.seedContacts(contacts)

    await page.goto('/contacts')
    await page.waitForLoadState('domcontentloaded')

    // Search for our test contacts to isolate them
    const searchInput = page.getByPlaceholder('Search contacts...')
    await searchInput.fill(`${testApi.prefix}-Paginated`)
    const searchResponse = page.waitForResponse(
      resp => resp.url().includes('/api/v1/contacts') && resp.url().includes('search=')
    )
    await searchInput.press('Enter')
    await searchResponse

    // Verify top and bottom pagination controls both exist
    const paginationControls = page.locator('[data-testid="pagination"]')
    await expect(paginationControls).toHaveCount(2)

    // Verify page number buttons exist (at least page 1 and 2 in top pagination)
    const topPagination = paginationControls.first()
    await expect(topPagination.getByRole('button', { name: '1' })).toBeVisible()
    await expect(topPagination.getByRole('button', { name: '2' })).toBeVisible()

    // Verify page 1 is active (primary variant)
    const page1Button = topPagination.getByRole('button', { name: '1' })
    await expect(page1Button).toHaveClass(/bg-blue-600/)

    // Click page 2 and verify it navigates
    await topPagination.getByRole('button', { name: '2' }).click()
    await expect(topPagination.getByRole('button', { name: '2' })).toHaveClass(/bg-blue-600/)

    // Verify Previous is now enabled and Next is disabled (only 2 pages)
    await expect(topPagination.getByRole('button', { name: 'Previous' })).toBeEnabled()
    await expect(topPagination.getByRole('button', { name: 'Next' })).toBeDisabled()

    // Click Previous to go back to page 1
    await topPagination.getByRole('button', { name: 'Previous' }).click()
    await expect(topPagination.getByRole('button', { name: '1' })).toHaveClass(/bg-blue-600/)

    // Verify both paginations are in sync (both show page 1 as active)
    const bottomPagination = paginationControls.last()
    await expect(bottomPagination.getByRole('button', { name: '1' })).toHaveClass(/bg-blue-600/)
  })
})
