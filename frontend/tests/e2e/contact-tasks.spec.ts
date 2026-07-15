import { test, expect, Page, Route } from '@playwright/test'
import { createTestAPI, TestAPI } from './helpers/test-api'

// The tasks section (TasksSection) renders unconditionally on the contact
// detail page — it does NOT depend on Todoist being configured. The header
// button's accessible name is "Add" (exact); only the modal's submit button
// is labeled "Add Task".
//
// Task ROWS, however, need backend task state that a provider-less E2E
// environment cannot create (manual creation and follow-ups require a live
// Todoist provider, and the /tasks routes are OAuth-gated). The row-rendering
// behaviors below are therefore proved as frontend-render/interaction tests
// over full-envelope route mocks — the same observable signals the retired
// verifiers graded.

interface MockTask {
  id: string
  contact_id: string
  kind: string
  lifecycle: string
  external_task_id: string
  content: string
  state: string
  created_at: string
}

function makeTask(
  contactId: string,
  over: { id: string; content: string; kind?: string; lifecycle?: string; state?: string }
): MockTask {
  return {
    id: over.id,
    contact_id: contactId,
    kind: over.kind ?? 'reach_out',
    lifecycle: over.lifecycle ?? 'manual',
    external_task_id: `ext-${over.id}`,
    content: over.content,
    state: over.state ?? 'managed',
    created_at: '2026-07-01T00:00:00Z',
  }
}

interface TaskLists {
  followup: MockTask[]
  manual: MockTask[]
  completed: MockTask[]
}

// The contact detail page issues THREE param-differentiated task queries —
// {state:managed, lifecycle:manual}, {state:completed, lifecycle:manual},
// {state:managed, lifecycle:followup_loop} — and merges them in the PAGE as
// [...followUpTasks, ...activeManualTasks]. The mock must branch on BOTH
// state AND lifecycle: returning one list to two combos would duplicate rows
// and make the follow-up-first ordering non-discriminating. The glob also
// matches the OAuth-gated POST (create), so dispatch on METHOD first; a
// non-GET falls through unless the test supplies a write handler.
async function mockTaskLists(
  page: Page,
  contactId: string,
  lists: () => TaskLists,
  onWrite?: (route: Route) => Promise<void>
) {
  await page.route(`**/api/v1/contacts/${contactId}/tasks*`, async route => {
    if (route.request().method() !== 'GET') {
      if (onWrite) return onWrite(route)
      return route.fallback()
    }
    const params = new URL(route.request().url()).searchParams
    const state = params.get('state')
    const lifecycle = params.get('lifecycle')
    const current = lists()
    const data =
      state === 'managed' && lifecycle === 'followup_loop'
        ? current.followup
        : state === 'managed' && lifecycle === 'manual'
          ? current.manual
          : state === 'completed' && lifecycle === 'manual'
            ? current.completed
            : []
    await route.fulfill({ json: { success: true, data } })
  })
}

// The TasksSection card, scoped by its "Tasks" heading; task rows are the
// TaskRow containers inside it.
function tasksSection(page: Page) {
  return page
    .locator('div.bg-white.shadow')
    .filter({ has: page.getByRole('heading', { name: 'Tasks', exact: true }) })
}
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

      // spec: CAD-030[3]
      // A freshly seeded contact has no tasks, so the empty state is
      // guaranteed: it invites adding a task.
      await expect(page.getByText('No tasks yet')).toBeVisible()
      await expect(page.getByText('Add a task to track follow-ups for this contact')).toBeVisible()
    })

    test('lists follow-up tasks first with a distinct pending indicator, then manual tasks', async ({
      page,
    }) => {
      // spec: CAD-030[0]
      const { ids } = await testApi.seedContacts([{ full_name: 'Task Order Contact' }])
      const contactId = ids[0]
      const followUp = makeTask(contactId, {
        id: 'task-followup-1',
        content: 'Mock follow-up task',
        lifecycle: 'followup_loop',
      })
      const manual = makeTask(contactId, { id: 'task-manual-1', content: 'Mock manual task' })
      await mockTaskLists(page, contactId, () => ({
        followup: [followUp],
        manual: [manual],
        completed: [],
      }))

      await page.goto(`/contacts/${contactId}`)
      await page.waitForLoadState('domcontentloaded')
      await expect(page.getByText('Mock follow-up task')).toBeVisible({ timeout: 15000 })

      // The follow-up row precedes the manual row in DOM order (the page
      // merges [...followUpTasks, ...activeManualTasks] — reversing the
      // concat flips this).
      const rows = tasksSection(page).locator('div.group')
      await expect(rows).toHaveCount(2)
      await expect(rows.nth(0)).toContainText('Mock follow-up task')
      await expect(rows.nth(1)).toContainText('Mock manual task')

      // Distinct pending indicator: follow-up rows carry the amber Clock
      // icon (a CSS-only signal — the class is the observable); the manual
      // row does not.
      await expect(rows.nth(0).locator('svg.text-amber-400')).toBeVisible()
      await expect(rows.nth(1).locator('svg.text-amber-400')).toHaveCount(0)
    })

    test('derives each task badge from its kind and lifecycle', async ({ page }) => {
      // spec: CAD-030[1]
      const { ids } = await testApi.seedContacts([{ full_name: 'Task Badge Contact' }])
      const contactId = ids[0]
      // Distinct kind/lifecycle pairs across the followup_loop + manual
      // queries (getBadgeLabel: reach_out+followup_loop → Follow-up;
      // send → Send; reminder → Reminder; reach_out+manual → Reach out).
      const badgeCases = [
        {
          task: makeTask(contactId, {
            id: 'task-b-followup',
            content: 'Badge case followup',
            lifecycle: 'followup_loop',
          }),
          badge: 'Follow-up',
        },
        {
          task: makeTask(contactId, {
            id: 'task-b-send',
            content: 'Badge case send',
            kind: 'send',
          }),
          badge: 'Send',
        },
        {
          task: makeTask(contactId, {
            id: 'task-b-reminder',
            content: 'Badge case reminder',
            kind: 'reminder',
          }),
          badge: 'Reminder',
        },
        {
          task: makeTask(contactId, { id: 'task-b-reach', content: 'Badge case reach' }),
          badge: 'Reach out',
        },
      ]
      await mockTaskLists(page, contactId, () => ({
        followup: badgeCases.slice(0, 1).map(c => c.task),
        manual: badgeCases.slice(1).map(c => c.task),
        completed: [],
      }))

      await page.goto(`/contacts/${contactId}`)
      await page.waitForLoadState('domcontentloaded')
      await expect(page.getByText('Badge case followup')).toBeVisible({ timeout: 15000 })

      const rows = tasksSection(page).locator('div.group')
      for (const { task, badge } of badgeCases) {
        const row = rows.filter({ hasText: task.content })
        await expect(row.getByText(badge, { exact: true })).toBeVisible()
      }
    })

    test('collapses completed tasks behind a toggle with a count', async ({ page }) => {
      // spec: CAD-030[2]
      const { ids } = await testApi.seedContacts([{ full_name: 'Task History Contact' }])
      const contactId = ids[0]
      await mockTaskLists(page, contactId, () => ({
        followup: [],
        manual: [],
        completed: [
          makeTask(contactId, { id: 'task-done-1', content: 'Done task one', state: 'completed' }),
          makeTask(contactId, { id: 'task-done-2', content: 'Done task two', state: 'completed' }),
        ],
      }))

      await page.goto(`/contacts/${contactId}`)
      await page.waitForLoadState('domcontentloaded')

      // Collapsed by default: the toggle carries the count, the completed
      // rows are hidden until it is clicked.
      const toggle = page.getByRole('button', { name: 'Show completed (2)' })
      await expect(toggle).toBeVisible({ timeout: 15000 })
      await expect(page.getByText('Done task one')).not.toBeVisible()
      await expect(page.getByText('Done task two')).not.toBeVisible()

      await toggle.click()
      await expect(page.getByText('Done task one')).toBeVisible()
      await expect(page.getByText('Done task two')).toBeVisible()
      await expect(page.getByRole('button', { name: 'Hide completed (2)' })).toBeVisible()
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
