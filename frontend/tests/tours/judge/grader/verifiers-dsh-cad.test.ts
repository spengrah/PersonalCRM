import { describe, it, expect } from 'vitest'
import { apiItem, cap, pair, root } from './fixtures'
import { applyMutation } from '../doctor'
import type { CaptureSet } from './types'
import type { Capture } from '../../support/types'
import { cad031 } from './verifiers/cad031'
import { cad033 } from './verifiers/cad033'
import type { Mutation } from '../corpus/schema'

function set(behaviorId: string, captures: Capture[]): CaptureSet {
  return { behaviorId, captures }
}
function doctored(behaviorId: string, captures: Capture[], m: Mutation): CaptureSet {
  return { behaviorId, captures: applyMutation(captures, m) }
}

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
