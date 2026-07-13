// cadence-followup.tour.ts — an assertion-free walk of the cadence-followup
// contact-detail `ux` behaviors: CAD-029 (recent activity), CAD-030 (tasks
// section empty state), CAD-031 (add-task modal), CAD-033 (unlink, skip-listed).
//
// Imports ONLY `test` from the fixtures — never `expect`. The recent-activity
// states (CAD-029) are API-SELECTED: a distinct seeded contact for each required
// state, throwing fail-loud if a state is absent (a missing state is signal, not
// a vacuous pass). Provider-dependent parts (populated tasks, task create,
// unlink) are skip-listed — a provider-less local sweep cannot reach them.

import { test } from './support/tour-fixtures'

interface ActivityContact {
  id: string
  full_name: string
  last_outreach_at: string | null
  last_response_at: string | null
  has_pending_followup: boolean
}

const CONTACT_ID_PATH = /\/api\/v1\/contacts\/[0-9a-f-]{36}$/

test('cadence-followup tour — contact-detail cadence surfaces', async ({ page, tour }) => {
  test.setTimeout(480_000)

  // --- API-select a distinct contact for each CAD-029 activity state ---
  const listResp = await tour.apiCtx.get('/api/v1/contacts?limit=300&sort=cadence&order=desc')
  const contacts = ((await listResp.json())?.data ?? []) as ActivityContact[]

  const reserved = new Set<string>()
  const pick = (pred: (c: ActivityContact) => boolean, label: string): ActivityContact => {
    const c = contacts.find(x => !reserved.has(x.id) && pred(x))
    if (!c) throw new Error(`cadence-followup tour: no seeded contact with ${label}`)
    reserved.add(c.id)
    return c
  }
  const outreachContact = pick(c => !!c.last_outreach_at, 'last_outreach_at (CAD-029[0])')
  const responseContact = pick(c => !!c.last_response_at, 'last_response_at (CAD-029[1])')
  // has_pending_followup is ONLY computed by the DETAIL handler. The list (and overdue)
  // payloads carry the field as a non-pointer bool with no omitempty, so they ship
  // `has_pending_followup: false` for every contact unconditionally — present, always
  // false, and therefore meaningless. Selecting on it from the list silently matches
  // nothing (and `noneContact`'s !has_pending_followup clause below is vacuously true for
  // the same reason). Probe the detail endpoint, which actually answers.
  const pendingContact = await (async () => {
    for (const c of contacts) {
      const r = await tour.apiCtx.get(`/api/v1/contacts/${c.id}`)
      if (((await r.json())?.data ?? {}).has_pending_followup) return c
    }
    // Loud, never skipped: a world with no live follow-up is a SEED bug (the prod-shaped
    // profile seeds one), and touring without this state is exactly what produced the
    // false CAD-036 regression — the judge read the missing state as a missing feature.
    throw new Error(
      'cadence-followup tour: no seeded contact with has_pending_followup (CAD-029[2])'
    )
  })()
  reserved.add(pendingContact.id)
  const noneContact = pick(
    c => !c.last_outreach_at && !c.last_response_at && !c.has_pending_followup,
    'no recent-activity signals (CAD-029[3])'
  )

  const gotoDetail = async (id: string): Promise<void> => {
    await page.goto(`/contacts/${id}`)
    await tour.waitForApi(page, 'GET', new RegExp(`/api/v1/contacts/${id}$`))
    await page.getByRole('button', { name: 'Edit' }).waitFor({ state: 'visible' })
  }

  // --- CAD-029[0]: last outreach shown when it exists ---
  await gotoDetail(outreachContact.id)
  await page.getByText('Last outreach:').waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CAD-029'],
    note: 'recent activity: last-outreach state',
    pair: { id: 'activity', role: 'activity-outreach' },
  })

  // --- CAD-029[1]: last response shown when it exists ---
  await gotoDetail(responseContact.id)
  await page.getByText('Last response:').waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CAD-029'],
    note: 'recent activity: last-response state',
    pair: { id: 'activity', role: 'activity-response' },
  })

  // --- CAD-029[2]: pending-reply ("Awaiting reply") state ---
  // The state the judge could never see. It is unreachable from a historical replay
  // (FollowUpManager is off-mode in the seed harness, and CAD-012 suppresses follow-ups
  // for backdated automated outbounds), so the prod-shaped profile now seeds one live
  // follow-up explicitly. Without this capture the judge sees only contact pages with no
  // "Awaiting reply" marker and concludes the FEATURE DOES NOT EXIST — a confident,
  // well-cited, false CAD-036 regression. Absence of evidence is not evidence of absence,
  // and the judge cannot tell the difference; only the capture can.
  await gotoDetail(pendingContact.id)
  await page.getByText('Awaiting reply').waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CAD-029'],
    note: 'recent activity: pending-reply state (awaiting reply on an outbound)',
    pair: { id: 'activity', role: 'activity-pending' },
  })

  // --- CAD-029[3]: no-recent-activity state ---
  await gotoDetail(noneContact.id)
  await page.getByText('No recent activity').waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CAD-029'],
    note: 'recent activity: explicit no-recent-activity state',
    pair: { id: 'activity', role: 'activity-none' },
  })

  // --- CAD-030[3] + CAD-033 (skip-listed): the tasks section empty state ---
  const tasksSection = page
    .locator('div.bg-white.shadow')
    .filter({ has: page.getByRole('heading', { name: 'Tasks', level: 3 }) })
  await page.getByText('No tasks yet').waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CAD-030', 'CAD-033'],
    note: 'tasks section: no-tasks empty state (populated ordering / unlink need a provider)',
    pair: { id: 'tasks', role: 'tasks-empty' },
    ariaRoot: tasksSection,
  })

  // --- CAD-031[0][1]: the add-task modal (kind picker + text-required) ---
  const addTaskModal = page
    .locator('div.fixed.inset-0')
    .filter({ has: page.getByRole('heading', { name: /Add Task for/ }) })
  await tasksSection.getByRole('button', { name: 'Add' }).click()
  await page.getByRole('heading', { name: /Add Task for/ }).waitFor({ state: 'visible' })
  const taskInput = page.getByPlaceholder('Follow up about surgery next tuesday p2')
  await taskInput.waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CAD-031'],
    note: 'add-task modal, empty text: kind picker + submit disabled',
    pair: { id: 'addtask', role: 'add-task-empty' },
    ariaRoot: addTaskModal,
  })

  await taskInput.fill('synth follow-up task')
  await page.getByRole('button', { name: 'Add Task' }).waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CAD-031'],
    note: 'add-task modal, text entered: submit enabled (create needs a provider → skip-list)',
    pair: { id: 'addtask', role: 'add-task-filled' },
    ariaRoot: addTaskModal,
  })
  await page.keyboard.press('Escape') // close without submitting (no provider)
  await page.getByRole('heading', { name: /Add Task for/ }).waitFor({ state: 'hidden' })
})
