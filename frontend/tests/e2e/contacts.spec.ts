import { test, expect } from '@playwright/test'

test.describe('Contacts', () => {
  test('should truncate long location with tooltip in contacts table', async ({ page }) => {
    const suffix = Date.now()
    const fullName = `Location Test Contact ${suffix}`
    // Create a very long location that will definitely overflow
    const longLocation =
      '1234 Very Long Street Name Boulevard, Extremely Long Neighborhood District, San Francisco, California 94105, United States of America'

    // Create contact with long location
    await page.goto('/contacts/new')
    await page.getByLabel('Full Name').fill(fullName)
    await page.getByLabel('Location').fill(longLocation)

    await Promise.all([
      page.waitForURL(/\/contacts\/[A-Za-z0-9-]+$/),
      page.getByRole('button', { name: 'Create Contact' }).click(),
    ])

    await page.waitForLoadState('networkidle')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Navigate to contacts list to see the table
    await page.goto('/contacts')
    await page.waitForLoadState('networkidle')

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

    // Cleanup: Go back to contact detail page and delete
    await contactRow.click()
    await page.waitForURL(/\/contacts\/[A-Za-z0-9-]+$/)
    await page.waitForLoadState('networkidle')

    page.once('dialog', dialog => dialog.accept())
    await Promise.all([
      page.waitForURL('/contacts'),
      page.getByRole('button', { name: 'Delete' }).click(),
    ])
  })

  test('should show expandable notes for long content', async ({ page }) => {
    const suffix = Date.now()
    const fullName = `Long Notes Contact ${suffix}`
    // Create notes longer than 300 characters to trigger truncation
    const longNotes = `Met at the AI conference in San Francisco, March 2024. Works as a senior ML engineer at a startup focused on personal productivity tools.

Very interested in personal CRM concepts and AI-driven contact management. We discussed potential collaboration opportunities around embedding-based contact matching.

Key interests: Machine learning infrastructure, personal knowledge management, privacy-focused software design.

Follow-up: Share the pgvector article, introduce to Sarah from the embeddings team.`

    // Create contact with long notes
    await page.goto('/contacts/new')
    await page.getByLabel('Full Name').fill(fullName)
    await page.getByLabel('Notes').fill(longNotes)

    await Promise.all([
      page.waitForURL(/\/contacts\/[A-Za-z0-9-]+$/),
      page.getByRole('button', { name: 'Create Contact' }).click(),
    ])

    await page.waitForLoadState('networkidle')
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

    // Cleanup
    page.once('dialog', dialog => dialog.accept())
    await Promise.all([
      page.waitForURL('/contacts'),
      page.getByRole('button', { name: 'Delete' }).click(),
    ])
  })

  test('should not show expand button for short notes', async ({ page }) => {
    const suffix = Date.now()
    const fullName = `Short Notes Contact ${suffix}`
    const shortNotes = 'Brief note about this contact.'

    // Create contact with short notes
    await page.goto('/contacts/new')
    await page.getByLabel('Full Name').fill(fullName)
    await page.getByLabel('Notes').fill(shortNotes)

    await Promise.all([
      page.waitForURL(/\/contacts\/[A-Za-z0-9-]+$/),
      page.getByRole('button', { name: 'Create Contact' }).click(),
    ])

    await page.waitForLoadState('networkidle')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Verify notes are displayed
    await expect(page.getByText(shortNotes)).toBeVisible()

    // Verify "Show more" button is NOT visible (notes are short)
    await expect(page.getByRole('button', { name: 'Show more' })).not.toBeVisible()

    // Cleanup
    page.once('dialog', dialog => dialog.accept())
    await Promise.all([
      page.waitForURL('/contacts'),
      page.getByRole('button', { name: 'Delete' }).click(),
    ])
  })

  test('should create and edit contact with notes', async ({ page }) => {
    const suffix = Date.now()
    const fullName = `Notes Test Contact ${suffix}`
    const notes =
      'Met at a conference in 2024. Works in AI/ML. Very interested in personal CRM tools.'

    // Create contact with notes
    await page.goto('/contacts/new')
    await page.getByLabel('Full Name').fill(fullName)
    await page.getByLabel('Notes').fill(notes)

    await Promise.all([
      page.waitForURL(/\/contacts\/[A-Za-z0-9-]+$/),
      page.getByRole('button', { name: 'Create Contact' }).click(),
    ])

    await page.waitForLoadState('networkidle')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Verify notes are displayed on detail page
    await expect(page.getByText(notes)).toBeVisible()

    // Edit the contact to update notes (use first() to get header Edit button, not the last contacted edit)
    await page.getByRole('button', { name: 'Edit' }).first().click()
    await page.waitForLoadState('networkidle')

    const updatedNotes = 'Updated notes: Follow up about collaboration opportunity.'
    await page.getByLabel('Notes').fill(updatedNotes)

    // Submit the inline edit form
    await page.getByRole('button', { name: 'Update Contact' }).click()
    await page.waitForLoadState('networkidle')

    // Wait for form to close and return to detail view (Edit button visible again)
    await expect(page.getByRole('button', { name: 'Edit' }).first()).toBeVisible({ timeout: 15000 })

    // Verify updated notes are displayed
    await expect(page.getByText(updatedNotes)).toBeVisible()
    await expect(page.getByText(notes)).not.toBeVisible()

    // Cleanup
    page.once('dialog', dialog => dialog.accept())
    await Promise.all([
      page.waitForURL('/contacts'),
      page.getByRole('button', { name: 'Delete' }).click(),
    ])
  })

  test('should create contact with all methods and normalized handles', async ({ page }) => {
    const suffix = Date.now()
    const fullName = `Playwright Contact ${suffix}`
    const personalEmail = `personal-${suffix}@example.com`
    const workEmail = `work-${suffix}@example.com`
    const phone = '(555) 555-1234'
    const telegramHandle = `@@telegram${suffix}`
    const discordHandle = `@@discord${suffix}`
    const twitterHandle = `@@twitter${suffix}`
    const signal = '+1 555 555 9876'
    const gchatEmail = `gchat-${suffix}@example.com`

    const methods = [
      { type: 'email', value: personalEmail, expected: personalEmail },
      { type: 'email', value: workEmail, expected: workEmail },
      { type: 'phone', value: phone, expected: phone },
      { type: 'telegram', value: telegramHandle, expected: `@telegram${suffix}` },
      { type: 'signal', value: signal, expected: signal },
      { type: 'discord', value: discordHandle, expected: `@discord${suffix}` },
      { type: 'twitter', value: twitterHandle, expected: `@twitter${suffix}` },
      { type: 'gchat', value: gchatEmail, expected: gchatEmail },
    ]

    await page.goto('/contacts/new')
    await page.getByLabel('Full Name').fill(fullName)

    // Add method buttons (styled as text link but still a button element)
    const addMethodButton = page.getByRole('button', { name: 'Add method' })
    for (let i = 1; i < methods.length; i += 1) {
      await addMethodButton.click()
    }

    // Contact method type selects have IDs like "methods.0.type"
    const typeSelects = page.locator('select[id^="methods"]')
    await expect(typeSelects).toHaveCount(methods.length)

    for (const [index, method] of methods.entries()) {
      // Type selector and value input are identified by their IDs
      await page.locator(`#methods\\.${index}\\.type`).selectOption(method.type)
      await page.locator(`#methods\\.${index}\\.value`).fill(method.value)
    }

    // Primary toggle is now a star icon button with title attribute
    const primaryIndex = methods.findIndex(method => method.type === 'telegram')
    await page.getByTitle('Set as primary').nth(primaryIndex).click()

    await Promise.all([
      page.waitForURL(/\/contacts\/[A-Za-z0-9-]+$/),
      page.getByRole('button', { name: 'Create Contact' }).click(),
    ])

    await page.waitForLoadState('networkidle')
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

    page.once('dialog', dialog => dialog.accept())
    await Promise.all([
      page.waitForURL('/contacts'),
      page.getByRole('button', { name: 'Delete' }).click(),
    ])
  })

  test('should edit last contacted date manually', async ({ page }) => {
    const suffix = Date.now()
    const fullName = `Last Contacted Test ${suffix}`

    // Create a new contact
    await page.goto('/contacts/new')
    await page.getByLabel('Full Name').fill(fullName)

    await Promise.all([
      page.waitForURL(/\/contacts\/[A-Za-z0-9-]+$/),
      page.getByRole('button', { name: 'Create Contact' }).click(),
    ])

    await page.waitForLoadState('networkidle')
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

    // Cleanup
    page.once('dialog', dialog => dialog.accept())
    await Promise.all([
      page.waitForURL('/contacts'),
      page.getByRole('button', { name: 'Delete' }).click(),
    ])
  })

  test('should cancel editing last contacted date', async ({ page }) => {
    const suffix = Date.now()
    const fullName = `Cancel Edit Test ${suffix}`

    // Create a new contact
    await page.goto('/contacts/new')
    await page.getByLabel('Full Name').fill(fullName)

    await Promise.all([
      page.waitForURL(/\/contacts\/[A-Za-z0-9-]+$/),
      page.getByRole('button', { name: 'Create Contact' }).click(),
    ])

    await page.waitForLoadState('networkidle')
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

    // Cleanup
    page.once('dialog', dialog => dialog.accept())
    await Promise.all([
      page.waitForURL('/contacts'),
      page.getByRole('button', { name: 'Delete' }).click(),
    ])
  })

  test('should prevent setting future last contacted date', async ({ page }) => {
    const suffix = Date.now()
    const fullName = `Future Date Test ${suffix}`

    // Create a new contact
    await page.goto('/contacts/new')
    await page.getByLabel('Full Name').fill(fullName)

    await Promise.all([
      page.waitForURL(/\/contacts\/[A-Za-z0-9-]+$/),
      page.getByRole('button', { name: 'Create Contact' }).click(),
    ])

    await page.waitForLoadState('networkidle')
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

    // Cleanup
    await page.getByTestId('cancel-last-contacted-btn').click()
    page.once('dialog', dialog => dialog.accept())
    await Promise.all([
      page.waitForURL('/contacts'),
      page.getByRole('button', { name: 'Delete' }).click(),
    ])
  })

  test('should update Mark as Contacted button behavior', async ({ page }) => {
    const suffix = Date.now()
    const fullName = `Mark Contacted Test ${suffix}`

    // Create a new contact
    await page.goto('/contacts/new')
    await page.getByLabel('Full Name').fill(fullName)

    await Promise.all([
      page.waitForURL(/\/contacts\/[A-Za-z0-9-]+$/),
      page.getByRole('button', { name: 'Create Contact' }).click(),
    ])

    await page.waitForLoadState('networkidle')
    await expect(page.getByRole('heading', { name: fullName })).toBeVisible({ timeout: 15000 })

    // Click the "Mark as Contacted" button
    await page.getByRole('button', { name: 'Mark as Contacted' }).click()

    // Wait for the update and verify the date is today
    await page.waitForLoadState('networkidle')
    const today = new Date().toLocaleDateString('en-US')
    const lastContactedRow = page.locator('dt:has-text("Last contacted")').locator('..')
    await expect(lastContactedRow.locator('dd span').first()).toContainText(today)

    // Cleanup
    page.once('dialog', dialog => dialog.accept())
    await Promise.all([
      page.waitForURL('/contacts'),
      page.getByRole('button', { name: 'Delete' }).click(),
    ])
  })
})
