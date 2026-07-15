import { describe, it, expect } from 'vitest'
import { apiItem, cap, pair, root } from './fixtures'
import { applyMutation } from '../doctor'
import type { CaptureSet } from './types'
import type { AriaNode, Capture } from '../../support/types'
import { dsh007 } from './verifiers/dsh007'
import { cad026 } from './verifiers/cad026'
import { cad027 } from './verifiers/cad027'
import { cad028 } from './verifiers/cad028'
import { cad029 } from './verifiers/cad029'
import { cad030 } from './verifiers/cad030'
import { cad031 } from './verifiers/cad031'
import { cad033 } from './verifiers/cad033'
import type { Mutation } from '../corpus/schema'

function set(behaviorId: string, captures: Capture[]): CaptureSet {
  return { behaviorId, captures }
}
function doctored(behaviorId: string, captures: Capture[], m: Mutation): CaptureSet {
  return { behaviorId, captures: applyMutation(captures, m) }
}

// --- DSH-007 ---
describe('dsh007', () => {
  const dashboard = cap({ behaviors: ['DSH-007'], pair: pair('d', 'dashboard'), aria: root([]) })
  const contactsSearch = cap({
    behaviors: ['DSH-007'],
    pair: pair('c', 'contacts-search'),
    aria: root([{ role: 'textbox', name: 'Search contacts...' }]),
  })

  it('clean: [0] pass (contacts search), [1] pass (no dashboard search)', () => {
    const v = dsh007(set('DSH-007', [dashboard, contactsSearch]))
    expect(v[0].verdict).toBe('pass')
    expect(v[1].verdict).toBe('pass')
  })
  it('doctored: contacts search input removed → [0] fail', () => {
    const v = dsh007(
      doctored('DSH-007', [dashboard, contactsSearch], {
        op: 'remove_aria_subtree',
        role: 'contacts-search',
        node_role: 'textbox',
        node_name: 'Search contacts...',
      })
    )
    expect(v[0].verdict).toBe('fail')
  })
  it('a search surface on the dashboard → [1] fail', () => {
    const withSearch = cap({
      behaviors: ['DSH-007'],
      pair: pair('d', 'dashboard'),
      aria: root([{ role: 'searchbox', name: 'Search everything' }]),
    })
    expect(dsh007(set('DSH-007', [withSearch, contactsSearch]))[1].verdict).toBe('fail')
  })
})

// --- CAD-026 ---
describe('cad026', () => {
  // A single overdue card carrying all five sub-elements (count + heading +
  // cadence + recency + method + suggested action); omit any via the `drop` set.
  const overdueAria = (over: { count?: string; drop?: Set<string> } = {}): AriaNode => {
    const drop = over.drop ?? new Set<string>()
    return root([
      { role: 'heading', name: 'Action Required', level: 2 },
      { role: 'text', text: over.count ?? '148 contacts need your attention' },
      { role: 'heading', name: 'synth-a', level: 3 },
      ...(drop.has('cadence') ? [] : [{ role: 'text' as const, text: '(weekly cadence)' }]),
      ...(drop.has('recency')
        ? []
        : [
            { role: 'text' as const, text: '1 days overdue' },
            { role: 'text' as const, text: '- Last contacted 2 days ago' },
          ]),
      ...(drop.has('method') ? [] : [{ role: 'text' as const, text: 'Email' }]),
      ...(drop.has('action') ? [] : [{ role: 'text' as const, text: '💡 A quick check-in' }]),
      { role: 'button', name: 'Mark as Contacted' },
    ])
  }
  const overdue = (over: { count?: string; drop?: Set<string>; tier?: string } = {}): Capture =>
    cap({
      behaviors: ['CAD-026'],
      pair: pair('u', 'sort-urgency'),
      aria: overdueAria(over),
      fields: {
        overdueCards: [
          {
            name: 'synth-a',
            daysOverdue: 1,
            tierClass: over.tier ?? 'border-yellow-200 bg-yellow-50',
            lastContacted: '2026-07-11T00:00:00Z',
          },
        ],
      },
    })
  const caughtUp = cap({
    behaviors: ['CAD-026'],
    pair: pair('c', 'caught-up'),
    aria: root([{ role: 'heading', name: 'All caught up! 🎉', level: 3 }]),
  })

  it('clean: [0]/[1]/[2] pass', () => {
    const v = cad026(set('CAD-026', [overdue(), caughtUp]))
    expect(v[0].verdict).toBe('pass')
    expect(v[1].verdict).toBe('pass')
    expect(v[2].verdict).toBe('pass')
  })
  it('doctored: wrong tierClass → [1] fail', () => {
    expect(cad026(set('CAD-026', [overdue({ tier: 'border-red-200 bg-red-50' })]))[1].verdict).toBe(
      'fail'
    )
  })
  it('[1] fails when a card sub-element is dropped (cadence / method / action)', () => {
    for (const el of ['cadence', 'recency', 'method', 'action']) {
      const v = cad026(set('CAD-026', [overdue({ drop: new Set([el]) })]))
      expect(v[1].verdict, `dropping ${el}`).toBe('fail')
    }
  })
  it('doctored: remove_aria_subtree drops the only method label → [1] fail', () => {
    const v = cad026(
      doctored('CAD-026', [overdue()], {
        op: 'remove_aria_subtree',
        role: 'sort-urgency',
        node_role: 'text',
        node_name: 'Email',
      })
    )
    expect(v[1].verdict).toBe('fail')
  })
  it('[0] fails when the header count is below the visible-card count', () => {
    expect(
      cad026(set('CAD-026', [overdue({ count: '0 contacts need your attention' })]))[0].verdict
    ).toBe('fail')
  })
  it('missing → unsure', () => {
    const v = cad026(set('CAD-026', []))
    expect(v[0].verdict).toBe('unsure')
    expect(v[2].verdict).toBe('unsure')
  })
})

// --- CAD-027 ---
describe('cad027', () => {
  const sortCap = (
    role: string,
    cards: Array<{ name: string; daysOverdue: number; lastContacted: string | null }>
  ): Capture =>
    cap({
      behaviors: ['CAD-027'],
      pair: pair('s', role),
      fields: { overdueCards: cards.map(c => ({ ...c, tierClass: 'x' })) },
    })
  const urgency = sortCap('sort-urgency', [
    { name: 'b', daysOverdue: 12, lastContacted: '2026-07-01T00:00:00Z' },
    { name: 'a', daysOverdue: 1, lastContacted: '2026-07-10T00:00:00Z' },
  ])
  const name = sortCap('sort-name', [
    { name: 'a', daysOverdue: 1, lastContacted: '2026-07-10T00:00:00Z' },
    { name: 'b', daysOverdue: 12, lastContacted: '2026-07-01T00:00:00Z' },
  ])
  const lastContacted = sortCap('sort-last-contacted', [
    { name: 'b', daysOverdue: 12, lastContacted: '2026-07-01T00:00:00Z' },
    { name: 'a', daysOverdue: 1, lastContacted: '2026-07-10T00:00:00Z' },
  ])

  it('clean: [0]/[1]/[2] pass', () => {
    const v = cad027(set('CAD-027', [urgency, name, lastContacted]))
    expect(v[0].verdict).toBe('pass')
    expect(v[1].verdict).toBe('pass')
    expect(v[2].verdict).toBe('pass')
  })
  it('doctored: urgency order reversed → [0] fail', () => {
    const v = cad027(
      doctored('CAD-027', [urgency, name, lastContacted], {
        op: 'set_field',
        role: 'sort-urgency',
        field: 'overdueCards',
        value: [
          { name: 'a', daysOverdue: 1, tierClass: 'x', lastContacted: null },
          { name: 'b', daysOverdue: 12, tierClass: 'x', lastContacted: null },
        ],
      })
    )
    expect(v[0].verdict).toBe('fail')
  })
  it('missing → unsure', () => {
    expect(cad027(set('CAD-027', []))[0].verdict).toBe('unsure')
  })
})

// --- CAD-028 ---
describe('cad028', () => {
  // The default frame() currentTime is 2026-07-12T15:48:12Z; an accelerated stamp
  // sits just before it. (over.occurredAt lets a test place it outside the frame.)
  const after = (over: { overdueIds?: string[]; occurredAt?: string } = {}): Capture =>
    cap({
      behaviors: ['CAD-028'],
      pair: pair('m', 'mark-after'),
      url: '/dashboard',
      apiResponses: {
        'POST /api/v1/contacts/:id/interactions': [
          apiItem({
            method: 'POST',
            requestUrl: '/api/v1/contacts/<id:5>/interactions',
            status: 201,
            requestBody: { direction: 'mutual' },
            body: {
              data: {
                direction: 'mutual',
                occurred_at: over.occurredAt ?? '2026-07-12T15:47:00Z',
              },
            },
          }),
        ],
        'GET /api/v1/contacts/overdue': [
          apiItem({ body: { data: (over.overdueIds ?? ['<id:9>']).map(id => ({ id })) } }),
        ],
      },
    })

  it('clean: [0] pass (mutual, in accelerated frame), [1] pass (marked left the list)', () => {
    const v = cad028(set('CAD-028', [after()]))
    expect(v[0].verdict).toBe('pass')
    expect(v[1].verdict).toBe('pass')
  })
  it('[0] fails on a wall-clock stamp outside the accelerated frame', () => {
    // A day behind the accelerated currentTime — fails the recency bound.
    expect(cad028(set('CAD-028', [after({ occurredAt: '2026-07-11T15:47:00Z' })]))[0].verdict).toBe(
      'fail'
    )
  })
  it('doctored: POST interaction deleted → [0] fail', () => {
    const v = cad028(
      doctored('CAD-028', [after()], {
        op: 'delete_endpoint',
        role: 'mark-after',
        endpoint: 'POST /api/v1/contacts/:id/interactions',
      })
    )
    expect(v[0].verdict).toBe('fail')
  })
  it('[1] abstains when the marked contact is still overdue (timing)', () => {
    expect(cad028(set('CAD-028', [after({ overdueIds: ['<id:5>', '<id:9>'] })]))[1].verdict).toBe(
      'unsure'
    )
  })
  it('missing → unsure', () => {
    expect(cad028(set('CAD-028', []))[0].verdict).toBe('unsure')
  })
})

// --- CAD-029 ---
describe('cad029', () => {
  const detail = (role: string, data: Record<string, unknown>, ariaText: string): Capture =>
    cap({
      behaviors: ['CAD-029'],
      pair: pair('a', role),
      apiResponses: { 'GET /api/v1/contacts/:id': [apiItem({ body: { data } })] },
      aria: root([{ role: 'text', text: ariaText }]),
    })
  const outreach = detail(
    'activity-outreach',
    { last_outreach_at: '2026-07-12T12:00:00Z' },
    'Last outreach: 2 days ago'
  )
  const response = detail(
    'activity-response',
    { last_response_at: '2026-07-12T12:00:00Z' },
    'Last response: 1 day ago'
  )
  const none = detail('activity-none', { has_pending_followup: false }, 'No recent activity')

  it('clean: [0] pass, [1] pass, [2] abstain, [3] pass', () => {
    const v = cad029(set('CAD-029', [outreach, response, none]))
    expect(v[0].verdict).toBe('pass')
    expect(v[1].verdict).toBe('pass')
    expect(v[2].verdict).toBe('unsure')
    expect(v[3].verdict).toBe('pass')
  })
  it('doctored: last_outreach_at cleared but line still shown → [0] fail', () => {
    const v = cad029(
      doctored('CAD-029', [outreach, response, none], {
        op: 'set_json_field',
        role: 'activity-outreach',
        endpoint: 'GET /api/v1/contacts/:id',
        path: ['data', 'last_outreach_at'],
        value: null,
      })
    )
    expect(v[0].verdict).toBe('fail')
  })
  it('missing → unsure', () => {
    const v = cad029(set('CAD-029', []))
    expect(v[0].verdict).toBe('unsure')
    expect(v[3].verdict).toBe('unsure')
  })
})

// --- CAD-030 ---
describe('cad030', () => {
  const tasksEmpty = cap({
    behaviors: ['CAD-030'],
    pair: pair('t', 'tasks-empty'),
    aria: root([
      { role: 'heading', name: 'Tasks', level: 3 },
      { role: 'text', text: 'No tasks yet' },
    ]),
  })

  it('clean: [3] pass, [0]/[1]/[2] abstain', () => {
    const v = cad030(set('CAD-030', [tasksEmpty]))
    expect(v[0].verdict).toBe('unsure')
    expect(v[1].verdict).toBe('unsure')
    expect(v[2].verdict).toBe('unsure')
    expect(v[3].verdict).toBe('pass')
  })
  it('doctored: empty-state text removed → [3] fail', () => {
    const v = cad030(
      doctored('CAD-030', [tasksEmpty], {
        op: 'remove_aria_subtree',
        node_role: 'text',
        node_name: 'No tasks yet',
      })
    )
    expect(v[3].verdict).toBe('fail')
  })
  it('missing → [3] unsure', () => {
    expect(cad030(set('CAD-030', []))[3].verdict).toBe('unsure')
  })
})

// --- CAD-031 ---
describe('cad031', () => {
  const empty = cap({
    behaviors: ['CAD-031'],
    pair: pair('e', 'add-task-empty'),
    aria: root([
      { role: 'group', name: 'Task type' },
      { role: 'button', name: 'Reach out' },
      { role: 'button', name: 'Send' },
      { role: 'button', name: 'Reminder' },
      { role: 'button', name: 'Add Task', disabled: true },
    ]),
  })
  const filled = cap({
    behaviors: ['CAD-031'],
    pair: pair('f', 'add-task-filled'),
    aria: root([
      { role: 'button', name: 'Add notes' },
      { role: 'button', name: 'Add Task' },
    ]),
  })

  it('clean: [0] pass, [1] pass, [2] abstain', () => {
    const v = cad031(set('CAD-031', [empty, filled]))
    expect(v[0].verdict).toBe('pass')
    expect(v[1].verdict).toBe('pass')
    expect(v[2].verdict).toBe('unsure')
  })
  it('[1] fails when the optional notes affordance is absent', () => {
    const noNotes = cap({
      behaviors: ['CAD-031'],
      pair: pair('f', 'add-task-filled'),
      aria: root([{ role: 'button', name: 'Add Task' }]),
    })
    expect(cad031(set('CAD-031', [empty, noNotes]))[1].verdict).toBe('fail')
  })
  it('doctored: a kind removed → [0] fail', () => {
    const v = cad031(
      doctored('CAD-031', [empty, filled], {
        op: 'remove_aria_subtree',
        role: 'add-task-empty',
        node_role: 'button',
        node_name: 'Reach out',
      })
    )
    expect(v[0].verdict).toBe('fail')
  })
  it('doctored: submit enabled with empty text → [1] fail', () => {
    const v = cad031(
      doctored('CAD-031', [empty, filled], {
        op: 'set_aria_disabled',
        role: 'add-task-empty',
        node_role: 'button',
        node_name: 'Add Task',
        value: false,
      })
    )
    expect(v[1].verdict).toBe('fail')
  })
  it('missing → unsure', () => {
    expect(cad031(set('CAD-031', []))[0].verdict).toBe('unsure')
  })
})

// --- CAD-033 (all skip-listed → abstain) ---
describe('cad033', () => {
  it('both clauses abstain (provider-dependent skip-list)', () => {
    const v = cad033(
      set('CAD-033', [cap({ behaviors: ['CAD-033'], pair: pair('t', 'tasks-empty') })])
    )
    expect(v[0].verdict).toBe('unsure')
    expect(v[1].verdict).toBe('unsure')
  })
})
