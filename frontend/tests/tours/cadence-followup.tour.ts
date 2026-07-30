// cadence-followup.tour.ts — an assertion-free walk of the cadence-followup
// contact-detail `ux` behaviors: CAD-029 (recent activity), CAD-030 (tasks
// section empty state), CAD-031 (add-task modal), CAD-033 (unlink, skip-listed).
//
// Imports ONLY `test` from the fixtures — never `expect`. The recent-activity
// states (CAD-029) each have a DEDICATED PINNED CONTACT in the seed, resolved by
// its name marker and then verified to carry the state, so the tour walks the
// contact the seed guaranteed rather than whatever the population happened to
// contain. A missing or ambiguous fixture throws fail-loud (a missing state is
// signal, not a vacuous pass). Provider-dependent parts (populated tasks, task
// create, unlink) are skip-listed — a provider-less local sweep cannot reach them.

import { test } from './support/tour-fixtures'
import {
  FIXTURE_NO_ACTIVITY,
  FIXTURE_OUTREACH,
  FIXTURE_PENDING,
  FIXTURE_RESPONSE,
  resolveFixture,
} from './support/pinned-fixtures'

interface ActivityContact {
  id: string
  full_name: string
  last_outreach_at: string | null
  last_response_at: string | null
  // No has_pending_followup: the list payload always ships it false (only the
  // detail handler computes it), so a field here would invite reading it.
}

const CONTACT_ID_PATH = /\/api\/v1\/contacts\/[0-9a-f-]{36}$/

test('cadence-followup tour — contact-detail cadence surfaces', async ({ page, tour }) => {
  test.setTimeout(480_000)

  // --- Resolve the pinned contact for each CAD-029 activity state ----------
  // Resolution is by marker; the STATE is then verified on the resolved row, since
  // a search that returned something is not proof it returned the right thing.
  //
  // detailOf throws on a non-OK response rather than degrading to {}. An empty
  // object reads as "no activity signals", so a failed fetch would let the
  // no-activity check below pass for a reason that has nothing to do with the
  // contact — the same vacuity the state verification exists to remove.
  const detailOf = async (id: string): Promise<Record<string, unknown>> => {
    const r = await tour.apiCtx.get(`/api/v1/contacts/${id}`)
    if (!r.ok()) {
      throw new Error(
        `cadence-followup tour: contact detail fetch failed (${r.status()}) for ${id}`
      )
    }
    return ((await r.json())?.data ?? {}) as Record<string, unknown>
  }

  // Every CAD-029 state must sit on its OWN contact: the four captures are read as
  // four distinct relationships, and two states landing on one contact would let a
  // single page stand in for two behaviors. The seed's markers make that true and
  // the Go gate proves it per-PR, but staging drift is exactly what a tour-side
  // check is for, so each resolved subject is claimed here as it is resolved.
  const reserved = new Map<string, string>()
  const claim = (id: string, label: string): void => {
    const prior = reserved.get(id)
    if (prior) {
      throw new Error(
        `cadence-followup tour: ${label} resolved to the same contact as ${prior} — ` +
          'each CAD-029 activity state needs its own subject'
      )
    }
    reserved.set(id, label)
  }

  const outreachContact = await resolveFixture<ActivityContact>(
    tour.apiCtx,
    FIXTURE_OUTREACH,
    'CAD-029[0] last_outreach_at'
  )
  if (!outreachContact.last_outreach_at) {
    throw new Error('cadence-followup tour: the outreach fixture carries no last_outreach_at')
  }
  claim(outreachContact.id, 'the outreach fixture')

  const responseContact = await resolveFixture<ActivityContact>(
    tour.apiCtx,
    FIXTURE_RESPONSE,
    'CAD-029[1] last_response_at'
  )
  if (!responseContact.last_response_at) {
    throw new Error('cadence-followup tour: the response fixture carries no last_response_at')
  }
  claim(responseContact.id, 'the response fixture')

  // has_pending_followup is ONLY computed by the DETAIL handler. The list (and overdue)
  // payloads carry the field as a non-pointer bool with no omitempty, so they ship
  // `has_pending_followup: false` for every contact unconditionally — present, always
  // false, and therefore meaningless. Verifying it means one detail probe on the
  // resolved fixture, where the old selection swept the detail endpoint across the
  // whole population looking for a hit.
  const pendingContact = await resolveFixture<ActivityContact>(
    tour.apiCtx,
    FIXTURE_PENDING,
    'CAD-029[2] has_pending_followup'
  )
  claim(pendingContact.id, 'the pending fixture')
  // Loud, never skipped: a world with no live follow-up is a SEED bug (the standard
  // world seeds one), and touring without this state is exactly what produced the
  // false CAD-036 regression — the judge read the missing state as a missing feature.
  if (!(await detailOf(pendingContact.id)).has_pending_followup) {
    throw new Error(
      'cadence-followup tour: the pending fixture carries no live follow-up (CAD-029[2])'
    )
  }

  const noneContact = await resolveFixture<ActivityContact>(
    tour.apiCtx,
    FIXTURE_NO_ACTIVITY,
    'CAD-029[3] no recent-activity signals'
  )
  claim(noneContact.id, 'the no-activity fixture')
  const noneDetail = await detailOf(noneContact.id)
  if (
    noneDetail.last_outreach_at ||
    noneDetail.last_response_at ||
    noneDetail.has_pending_followup
  ) {
    throw new Error('cadence-followup tour: the no-activity fixture carries an activity signal')
  }

  const gotoDetail = async (id: string): Promise<void> => {
    await page.goto(`/contacts/${id}`)
    await tour.waitForApi(page, 'GET', new RegExp(`/api/v1/contacts/${id}$`))
    await page.getByRole('button', { name: 'Edit' }).waitFor({ state: 'visible' })
  }

  // --- CAD-029.last-outreach-time-shown: last outreach shown when it exists ---
  await gotoDetail(outreachContact.id)
  await page.getByText('Last outreach:').waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CAD-029'],
    note: 'recent activity: last-outreach state',
    pair: { id: 'activity', role: 'activity-outreach' },
  })

  // --- CAD-029.last-response-time-shown: last response shown when it exists ---
  await gotoDetail(responseContact.id)
  await page.getByText('Last response:').waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CAD-029'],
    note: 'recent activity: last-response state',
    pair: { id: 'activity', role: 'activity-response' },
  })

  // --- CAD-029.awaiting-reply-indicator-shown: pending-reply ("Awaiting reply") state ---
  // The state the judge could never see. It is unreachable from a historical replay
  // (FollowUpManager is off-mode in the seed harness, and CAD-012 suppresses follow-ups
  // for backdated automated outbounds), so the standard world seeds one live
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

  // --- CAD-029.explicit-no-recent-activity: no-recent-activity state -------
  await gotoDetail(noneContact.id)
  await page.getByText('No recent activity').waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CAD-029'],
    note: 'recent activity: explicit no-recent-activity state',
    pair: { id: 'activity', role: 'activity-none' },
  })

  // --- CAD-030.no-tasks-empty-state + CAD-033 (skip-listed): the tasks section empty state ---
  // The TasksSection card is a labeled region (<section aria-label="Tasks">).
  const tasksSection = page.getByRole('region', { name: 'Tasks', exact: true })
  await page.getByText('No tasks yet').waitFor({ state: 'visible' })
  await tour.capture(page, {
    behaviors: ['CAD-030', 'CAD-033'],
    note: 'tasks section: no-tasks empty state (populated ordering / unlink need a provider)',
    pair: { id: 'tasks', role: 'tasks-empty' },
    ariaRoot: tasksSection,
  })

  // --- CAD-031.kind-chosen-reach-out + .task-text-required-notes: the add-task modal (kind picker + text-required) ---
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
