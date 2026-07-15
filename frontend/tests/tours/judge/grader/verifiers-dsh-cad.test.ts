import { describe, it, expect } from 'vitest'
import { apiItem, cap, pair, root } from './fixtures'
import { applyMutation } from '../doctor'
import type { CaptureSet } from './types'
import type { Capture } from '../../support/types'
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
