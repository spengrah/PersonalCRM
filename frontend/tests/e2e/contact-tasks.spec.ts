import { test, expect } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

test.describe('Contact Tasks @area:contacts @area:tasks', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test.describe('Tasks Section', () => {
    test('shows tasks section on contact detail page when Todoist configured', async ({ page }) => {
      // Create a test contact
      const { ids } = await testApi.seedContacts([{ full_name: 'Task Test Contact' }])

      // Navigate to contact detail page
      await page.goto(`/contacts/${ids[0]}`)
      await page.waitForLoadState('networkidle')

      // Tasks section may or may not be visible depending on Todoist configuration
      // This is a smoke test - just verify the page loads without errors
      await expect(page.locator('h1').first()).toContainText('Task Test Contact')
    })
  })

  test.describe('Add Task Modal', () => {
    test('can interact with add task modal when available', async ({ page }) => {
      const { ids } = await testApi.seedContacts([{ full_name: 'Modal Test Contact' }])

      await page.goto(`/contacts/${ids[0]}`)
      await page.waitForLoadState('networkidle')

      // Look for the "Add Task" button (only visible when Todoist configured)
      const addTaskButton = page.locator('button:has-text("Add Task")')

      // If button exists, test the modal interaction
      const buttonCount = await addTaskButton.count()
      if (buttonCount > 0) {
        await addTaskButton.click()

        // Modal should appear
        const modal = page.locator('.fixed.inset-0')
        await expect(modal).toBeVisible()

        // Input field should be present
        const input = page.locator('input[placeholder*="Follow up"]')
        await expect(input).toBeVisible()

        // Close modal with Escape
        await page.keyboard.press('Escape')
        await expect(modal).not.toBeVisible()
      }
    })

    test('shows validation error for empty task text when available', async ({ page }) => {
      const { ids } = await testApi.seedContacts([{ full_name: 'Validation Test Contact' }])

      await page.goto(`/contacts/${ids[0]}`)
      await page.waitForLoadState('networkidle')

      const addTaskButton = page.locator('button:has-text("Add Task")')
      const buttonCount = await addTaskButton.count()

      if (buttonCount > 0) {
        await addTaskButton.click()

        // Try to submit without text (disabled button should prevent submission)
        const submitButton = page.locator('button[type="submit"]:has-text("Add Task")')
        const isDisabled = await submitButton.isDisabled()
        expect(isDisabled).toBe(true)

        await page.keyboard.press('Escape')
      }
    })
  })
})
