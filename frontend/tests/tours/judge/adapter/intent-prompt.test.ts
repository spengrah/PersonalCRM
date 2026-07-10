// The intent prompt variant: INTENT block (statement, status framing),
// per-capture CAPTURE[n] sections in order, the single [0] item, and the
// no-visual-styling caution — while the behavior path stays byte-identical.

import { describe, expect, it } from 'vitest'
import { buildPrompt } from './prompt'
import type { JudgeInput } from './types'

const base: JudgeInput = {
  behaviorId: 'DSH-010',
  behaviorTitle: 'at a glance',
  given: '',
  when: '',
  then: ['the statement'],
  items: [{ itemIndex: 0, thenText: 'the statement' }],
  evidence: {},
  intent: { statement: 'the statement', status: 'current' },
  captureSections: [
    {
      note: 'dashboard#1 — cards',
      evidence: { url: 'http://x/1', aria: { role: 'root', children: [] } },
    },
    {
      note: 'dashboard#2 — empty',
      evidence: { url: 'http://x/2', aria: { role: 'root', children: [] } },
    },
  ],
}

describe('buildPrompt (intent variant)', () => {
  const prompt = buildPrompt(base)

  it('renders the INTENT block with statement and status framing', () => {
    expect(prompt).toContain('=== INTENT ===')
    expect(prompt).toContain('INTENT DSH-010: at a glance')
    expect(prompt).toContain('STATEMENT: the statement')
    expect(prompt).toContain('STATUS: current')
  })

  it('renders one CAPTURE[n] section per bound capture, in order', () => {
    const first = prompt.indexOf('=== CAPTURE[0] — dashboard#1 — cards ===')
    const second = prompt.indexOf('=== CAPTURE[1] — dashboard#2 — empty ===')
    expect(first).toBeGreaterThan(-1)
    expect(second).toBeGreaterThan(first)
    expect(prompt.indexOf('http://x/1')).toBeLessThan(prompt.indexOf('http://x/2'))
  })

  it('carries the capture-index grounding rule and the visual-styling caution', () => {
    expect(prompt).toContain('cite the capture index')
    expect(prompt).toMatch(/do not fail a goal for purely visual qualities/)
  })

  it('lists the single [0] item', () => {
    expect(prompt).toContain('=== ITEMS ===')
    expect(prompt).toContain('[0] the statement')
  })

  it('does not affect the behavior-path prompt', () => {
    const behaviorInput: JudgeInput = {
      behaviorId: 'CON-042',
      behaviorTitle: 't',
      given: 'g',
      when: 'w',
      then: ['a'],
      items: [{ itemIndex: 0, thenText: 'a' }],
      evidence: { url: 'http://y' },
    }
    const p = buildPrompt(behaviorInput)
    expect(p).toContain('=== SPEC ===')
    expect(p).not.toContain('=== INTENT ===')
    expect(p).not.toContain('CAPTURE[')
  })
})
