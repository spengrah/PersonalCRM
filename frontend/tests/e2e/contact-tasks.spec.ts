import { test, expect } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

// The tasks section (TasksSection) renders unconditionally on the contact
// detail page — it does NOT depend on Todoist being configured. The header
// button's accessible name is "Add" (exact); only the modal's submit button
// is labeled "Add Task".
test.describe('Contact Tasks @area:contacts @area:tasks', () => {
  let testApi: TestAPI

  test.beforeEach(async ({ request }, testInfo) => {
    testApi = createTestAPI(request, testInfo)
  })

  test.afterEach(async () => {
    await testApi.cleanup()
  })

  test.describe('Tasks Section', () => {
    test('shows tasks section on contact detail page', async ({ page }) => {
      const { ids } = await testApi.seedContacts([{ full_name: 'Task Test Contact' }])

      await page.goto(`/contacts/${ids[0]}`)
      await page.waitForLoadState('domcontentloaded')
      await expect(page.getByRole('heading', { name: 'Task Test Contact' })).toBeVisible({
        timeout: 15000,
      })

      // Tasks section renders unconditionally with its header and Add button
      await expect(page.getByRole('heading', { name: 'Tasks' })).toBeVisible()
      await expect(page.getByRole('button', { name: 'Add', exact: true })).toBeVisible()
    })
  })

  test.describe('Add Task Modal', () => {
    test('opens add task modal and closes it with Escape', async ({ page }) => {
      const { ids } = await testApi.seedContacts([{ full_name: 'Modal Test Contact' }])

      await page.goto(`/contacts/${ids[0]}`)
      await page.waitForLoadState('domcontentloaded')
      await expect(page.getByRole('heading', { name: 'Modal Test Contact' })).toBeVisible({
        timeout: 15000,
      })

      await page.getByRole('button', { name: 'Add', exact: true }).click()

      // Modal should appear with its heading and task text input
      // (seeded contact names carry a worker-namespace prefix, so match loosely)
      const modalHeading = page.getByRole('heading', { name: /Add Task for .*Modal Test Contact/ })
      await expect(modalHeading).toBeVisible()
      await expect(page.getByPlaceholder(/Follow up/)).toBeVisible()

      // Close modal with Escape
      await page.keyboard.press('Escape')
      await expect(modalHeading).not.toBeVisible()
    })

    test('disables submit while task text is empty', async ({ page }) => {
      const { ids } = await testApi.seedContacts([{ full_name: 'Validation Test Contact' }])

      await page.goto(`/contacts/${ids[0]}`)
      await page.waitForLoadState('domcontentloaded')
      await expect(page.getByRole('heading', { name: 'Validation Test Contact' })).toBeVisible({
        timeout: 15000,
      })

      await page.getByRole('button', { name: 'Add', exact: true }).click()

      // Submit is disabled until task text is entered
      const submitButton = page.getByRole('button', { name: 'Add Task' })
      await expect(submitButton).toBeVisible()
      await expect(submitButton).toBeDisabled()

      await page.getByPlaceholder(/Follow up/).fill('Say hello')
      await expect(submitButton).toBeEnabled()

      await page.keyboard.press('Escape')
    })
  })
})
