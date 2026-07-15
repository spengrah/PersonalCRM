import { describe, it, expect } from 'vitest'
import { apiItem, cap, pair, root } from './fixtures'
import { applyMutation } from '../doctor'
import type { CaptureSet } from './types'
import type { Capture } from '../../support/types'
import { cad033 } from './verifiers/cad033'
import type { Mutation } from '../corpus/schema'

function set(behaviorId: string, captures: Capture[]): CaptureSet {
  return { behaviorId, captures }
}
function doctored(behaviorId: string, captures: Capture[], m: Mutation): CaptureSet {
  return { behaviorId, captures: applyMutation(captures, m) }
}

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
