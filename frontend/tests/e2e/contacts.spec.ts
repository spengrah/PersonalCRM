import { test, expect } from '@playwright/test'

test.describe('Contacts', () => {
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

    // Edit the contact to update notes
    await page.getByRole('button', { name: 'Edit' }).click()
    await page.waitForLoadState('networkidle')

    const updatedNotes = 'Updated notes: Follow up about collaboration opportunity.'
    await page.getByLabel('Notes').fill(updatedNotes)

    // Submit the inline edit form
    await page.getByRole('button', { name: 'Update Contact' }).click()
    await page.waitForLoadState('networkidle')

    // Wait for form to close and return to detail view (Edit button visible again)
    await expect(page.getByRole('button', { name: 'Edit' })).toBeVisible({ timeout: 15000 })

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
      { type: 'email_personal', value: personalEmail, expected: personalEmail },
      { type: 'email_work', value: workEmail, expected: workEmail },
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
})
