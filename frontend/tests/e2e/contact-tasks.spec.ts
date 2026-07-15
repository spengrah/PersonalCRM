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
      // spec: CAD-030[3]
      const { ids } = await testApi.seedContacts([{ full_name: 'Task Test Contact' }])

      // Mock SUCCESSFUL empty lists for all three task queries: without the
      // mock the OAuth-gated routes 404 and the section would render the
      // empty state from the error default — a pass on API failure, not a
      // proof of the no-tasks state.
      await mockTaskLists(page, ids[0], () => ({ followup: [], manual: [], completed: [] }))

      await page.goto(`/contacts/${ids[0]}`)
      await page.waitForLoadState('domcontentloaded')
      await expect(page.getByRole('heading', { name: 'Task Test Contact' })).toBeVisible({
        timeout: 15000,
      })

      // Tasks section renders unconditionally with its header and Add button
      await expect(page.getByRole('heading', { name: 'Tasks' })).toBeVisible()
      await expect(page.getByRole('button', { name: 'Add', exact: true })).toBeVisible()

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
      // spec: CAD-031[0]
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

      // The kind is CHOSEN from reach-out / send / reminder: the segmented
      // control exposes exactly those three, defaults to Reach out, and
      // clicking another kind moves the aria-pressed selection.
      const kindGroup = page.getByRole('group', { name: 'Task type' })
      const reachOut = kindGroup.getByRole('button', { name: 'Reach out', exact: true })
      const send = kindGroup.getByRole('button', { name: 'Send', exact: true })
      const reminder = kindGroup.getByRole('button', { name: 'Reminder', exact: true })
      await expect(reachOut).toBeVisible()
      await expect(send).toBeVisible()
      await expect(reminder).toBeVisible()
      await expect(kindGroup.getByRole('button')).toHaveCount(3)

      // Every kind is SELECTABLE, not merely rendered: the default is
      // Reach out, and clicking each other kind (then back) moves the
      // single aria-pressed selection.
      await expect(reachOut).toHaveAttribute('aria-pressed', 'true')
      await expect(send).toHaveAttribute('aria-pressed', 'false')
      await expect(reminder).toHaveAttribute('aria-pressed', 'false')
      await send.click()
      await expect(send).toHaveAttribute('aria-pressed', 'true')
      await expect(reachOut).toHaveAttribute('aria-pressed', 'false')
      await expect(reminder).toHaveAttribute('aria-pressed', 'false')
      await reminder.click()
      await expect(reminder).toHaveAttribute('aria-pressed', 'true')
      await expect(send).toHaveAttribute('aria-pressed', 'false')
      await expect(reachOut).toHaveAttribute('aria-pressed', 'false')
      await reachOut.click()
      await expect(reachOut).toHaveAttribute('aria-pressed', 'true')
      await expect(reminder).toHaveAttribute('aria-pressed', 'false')
      await expect(send).toHaveAttribute('aria-pressed', 'false')

      // Close modal with Escape
      await page.keyboard.press('Escape')
      await expect(modalHeading).not.toBeVisible()
    })

    test('disables submit while task text is empty', async ({ page }) => {
      // spec: CAD-031[1]
      const { ids } = await testApi.seedContacts([{ full_name: 'Validation Test Contact' }])

      await page.goto(`/contacts/${ids[0]}`)
      await page.waitForLoadState('domcontentloaded')
      await expect(page.getByRole('heading', { name: 'Validation Test Contact' })).toBeVisible({
        timeout: 15000,
      })

      await page.getByRole('button', { name: 'Add', exact: true }).click()

      // Submit is disabled until task text is entered (task text required)
      const submitButton = page.getByRole('button', { name: 'Add Task' })
      await expect(submitButton).toBeVisible()
      await expect(submitButton).toBeDisabled()

      // Notes are OPTIONAL: a separate collapsed "Add notes" affordance
      // exists rather than a required field.
      await expect(page.getByRole('button', { name: /Add notes/i })).toBeVisible()

      await page.getByPlaceholder(/Follow up/).fill('Say hello')
      await expect(submitButton).toBeEnabled()

      await page.keyboard.press('Escape')
    })

    test('created task appears in the live tasks list', async ({ page }) => {
      // spec: CAD-031[2]
      // Real creation needs a Todoist provider AND the POST route is
      // OAuth-gated (the real endpoint 404s in this env, so an unmocked
      // waitForResponse would observe that 404). Mock the write loop:
      // POST → 201 created-task envelope, and the invalidation-driven
      // manual-list refetch then includes the created task.
      const { ids } = await testApi.seedContacts([{ full_name: 'Task Create Contact' }])
      const contactId = ids[0]
      const created = makeTask(contactId, {
        id: 'task-created-1',
        content: 'Say hello to Task Create',
      })
      let taskCreated = false
      await mockTaskLists(
        page,
        contactId,
        () => ({ followup: [], manual: taskCreated ? [created] : [], completed: [] }),
        async route => {
          if (route.request().method() === 'POST') {
            taskCreated = true
            await route.fulfill({ status: 201, json: { success: true, data: created } })
            return
          }
          await route.fallback()
        }
      )

      await page.goto(`/contacts/${contactId}`)
      await page.waitForLoadState('domcontentloaded')
      await expect(page.getByRole('heading', { name: 'Task Create Contact' })).toBeVisible({
        timeout: 15000,
      })

      await page.getByRole('button', { name: 'Add', exact: true }).click()
      await page.getByPlaceholder(/Follow up/).fill('Say hello to Task Create')

      const postResponsePromise = page.waitForResponse(
        response =>
          response.request().method() === 'POST' &&
          response.url().includes(`/api/v1/contacts/${contactId}/tasks`)
      )
      await page.getByRole('button', { name: 'Add Task' }).click()

      const postResponse = await postResponsePromise
      expect(postResponse.status()).toBe(201)
      expect(postResponse.request().postDataJSON()).toMatchObject({
        kind: 'reach_out',
        text: 'Say hello to Task Create',
      })

      // The modal closes and the created task renders in the live tasks
      // list (the task:created invalidation refetches the manual query,
      // which now includes it).
      await expect(page.getByRole('heading', { name: /Add Task for/ })).not.toBeVisible()
      await expect(tasksSection(page).getByText('Say hello to Task Create')).toBeVisible()
    })
  })

  test.describe('Linked Task Row (mocked)', () => {
    const UNLINK_TITLE = 'Remove from CRM (keeps in Todoist)'

    test('offers unlink with confirmation and fires only the CRM-link DELETE', async ({ page }) => {
      // spec: CAD-033[0]
      const { ids } = await testApi.seedContacts([{ full_name: 'Unlink Accept Contact' }])
      const contactId = ids[0]
      const linked = makeTask(contactId, { id: 'task-linked-1', content: 'Mock linked task' })
      let unlinked = false
      await mockTaskLists(page, contactId, () => ({
        followup: [],
        manual: unlinked ? [] : [linked],
        completed: [],
      }))
      // The DELETE route is OAuth-gated like the POST (the real endpoint
      // 404s in this env and would falsely read as an outcome), so mock the
      // CRM-link-only DELETE → 204 and ASSERT that status below.
      await page.route(`**/api/v1/contacts/${contactId}/tasks/*`, async route => {
        if (route.request().method() === 'DELETE') {
          unlinked = true
          await route.fulfill({ status: 204 })
          return
        }
        await route.fallback()
      })

      await page.goto(`/contacts/${contactId}`)
      await page.waitForLoadState('domcontentloaded')
      const row = tasksSection(page).locator('div.group').filter({ hasText: 'Mock linked task' })
      await expect(row).toBeVisible({ timeout: 15000 })

      // Native confirm: register the dialog handler AND the DELETE response
      // listener BEFORE the click (an unhandled native dialog auto-dismisses
      // → false negative). The confirmation copy is read INSIDE the handler
      // (it cannot be read after accept) and asserted after the DELETE — the
      // accept gates the DELETE, so the handler necessarily ran first.
      let confirmMessage = ''
      page.once('dialog', dialog => {
        confirmMessage = dialog.message()
        void dialog.accept()
      })
      const deleteResponsePromise = page.waitForResponse(
        response =>
          response.request().method() === 'DELETE' &&
          response.url().includes(`/api/v1/contacts/${contactId}/tasks/`)
      )
      await row.hover()
      await row.locator(`button[title="${UNLINK_TITLE}"]`).click()

      const deleteResponse = await deleteResponsePromise
      expect(deleteResponse.status()).toBe(204)
      expect(deleteResponse.request().url()).toContain(`/tasks/${linked.id}`)
      // The confirmation states the task stays alive in the remote app.
      expect(confirmMessage).toContain('remain in Todoist')

      // Only the CRM link was mutated: the refetched manual list no longer
      // includes the task, so its row leaves the section.
      await expect(row).not.toBeVisible()
    })

    test('dismissing the unlink confirmation leaves the task linked', async ({ page }) => {
      // spec: CAD-033[0]
      const { ids } = await testApi.seedContacts([{ full_name: 'Unlink Dismiss Contact' }])
      const contactId = ids[0]
      const linked = makeTask(contactId, { id: 'task-linked-2', content: 'Mock linked task' })
      await mockTaskLists(page, contactId, () => ({
        followup: [],
        manual: [linked],
        completed: [],
      }))

      await page.goto(`/contacts/${contactId}`)
      await page.waitForLoadState('domcontentloaded')
      const row = tasksSection(page).locator('div.group').filter({ hasText: 'Mock linked task' })
      await expect(row).toBeVisible({ timeout: 15000 })

      // Dismiss the confirm; collect any DELETE over a bounded settle
      // window (a negative outcome needs an observation window, not a
      // first-success poll).
      page.once('dialog', dialog => void dialog.dismiss())
      const deleteRequests: string[] = []
      page.on('request', request => {
        if (
          request.method() === 'DELETE' &&
          request.url().includes(`/api/v1/contacts/${contactId}/tasks/`)
        ) {
          deleteRequests.push(request.url())
        }
      })
      await row.hover()
      await row.locator(`button[title="${UNLINK_TITLE}"]`).click()

      await page.waitForTimeout(1500)
      expect(deleteRequests).toEqual([])
      await expect(row).toBeVisible()
    })

    test('exposes no in-CRM complete or dismiss control on a linked task', async ({ page }) => {
      // spec: CAD-033[1]
      // The CRM-side half of the clause: a linked task row offers ONLY
      // unlink (plus "Open in Todoist") — completing and dismissing happen
      // in the remote task app. The remote-survival half ("keeps the task
      // alive in Todoist") is a backend guarantee covered by backend
      // integration tests; the retired verifier abstained on it, so no
      // proven coverage is lost.
      const { ids } = await testApi.seedContacts([{ full_name: 'No Complete Contact' }])
      const contactId = ids[0]
      const linked = makeTask(contactId, { id: 'task-linked-3', content: 'Mock linked task' })
      await mockTaskLists(page, contactId, () => ({
        followup: [],
        manual: [linked],
        completed: [],
      }))

      await page.goto(`/contacts/${contactId}`)
      await page.waitForLoadState('domcontentloaded')
      const row = tasksSection(page).locator('div.group').filter({ hasText: 'Mock linked task' })
      await expect(row).toBeVisible({ timeout: 15000 })

      await expect(row.locator(`button[title="${UNLINK_TITLE}"]`)).toHaveCount(1)
      await expect(row.locator('a[title="Open in Todoist"]')).toHaveCount(1)
      await expect(row.getByRole('button', { name: /complete|dismiss|done/i })).toHaveCount(0)
      await expect(row.getByRole('checkbox')).toHaveCount(0)
    })
  })
})
