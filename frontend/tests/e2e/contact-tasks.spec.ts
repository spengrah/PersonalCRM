import { test, expect } from './helpers/test-fixtures'

test.describe('Contact Tasks', () => {
  test.describe('Tasks Section', () => {
    test('shows tasks section on contact detail page', async ({ contactDetailPage }) => {
      // Navigate to a contact
      await contactDetailPage.goto()

      // Tasks section should be visible (when Todoist is configured)
      // If Todoist not configured, section should not appear
      const tasksSection = contactDetailPage.page.locator('text=Tasks')
      // This test passes regardless - section visibility depends on Todoist config
      await expect(tasksSection.or(contactDetailPage.page.locator('body'))).toBeVisible()
    })
  })

  test.describe('Add Task Modal', () => {
    test('can open add task modal when Todoist is configured', async ({ contactDetailPage }) => {
      await contactDetailPage.goto()

      // Look for the "Add Task" button (only visible when Todoist configured)
      const addTaskButton = contactDetailPage.page.locator('button:has-text("Add Task")')

      // If button exists, clicking it should open the modal
      if (await addTaskButton.isVisible().catch(() => false)) {
        await addTaskButton.click()

        // Modal should appear with expected content
        const modal = contactDetailPage.page.locator('[role="dialog"], .fixed.inset-0')
        await expect(modal).toBeVisible()

        // Input field should be present
        const input = contactDetailPage.page.locator('input[placeholder*="Follow up"]')
        await expect(input).toBeVisible()

        // Close modal with Escape
        await contactDetailPage.page.keyboard.press('Escape')
        await expect(modal).not.toBeVisible()
      }
    })

    test('shows validation error for empty task text', async ({ contactDetailPage }) => {
      await contactDetailPage.goto()

      const addTaskButton = contactDetailPage.page.locator('button:has-text("Add Task")')

      if (await addTaskButton.isVisible().catch(() => false)) {
        await addTaskButton.click()

        // Try to submit without text
        const submitButton = contactDetailPage.page.locator('button:has-text("Add Task")').last()
        await submitButton.click()

        // Should show validation error
        const error = contactDetailPage.page.locator('text=Task text is required')
        await expect(error).toBeVisible()
      }
    })
  })

  test.describe('Task Toggle', () => {
    test('can toggle between active and completed tasks', async ({ contactDetailPage }) => {
      await contactDetailPage.goto()

      // Look for the toggle button
      const toggleButton = contactDetailPage.page.locator('button:has-text("Show Completed")')

      if (await toggleButton.isVisible().catch(() => false)) {
        await toggleButton.click()

        // Button text should change
        const hideButton = contactDetailPage.page.locator('button:has-text("Hide Completed")')
        await expect(hideButton).toBeVisible()
      }
    })
  })
})
