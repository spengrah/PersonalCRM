// The pure label-trace builders: Scenario projection (behavior vs intent) and
// index-aligned graded evidence with all-or-nothing screenshots (INV-4).

import { describe, expect, it } from 'vitest'
import type { CaptureSection, JudgeInput } from './adapter/types'
import { buildGradedEvidence, buildScenario, SCREENSHOT_CAVEAT } from './label-trace'

function section(captureFile: string, note: string): CaptureSection {
  return { captureFile, note, evidence: { url: `http://x/${captureFile}` } }
}

const behaviorInput: JudgeInput = {
  behaviorId: 'CON-042',
  behaviorTitle: 'delete confirmation',
  given: 'a contact exists',
  when: 'the user deletes it',
  then: ['warns cannot be undone', 'closes the dialog', 'removes the row'],
  items: [
    { itemIndex: 0, thenText: 'warns cannot be undone' },
    { itemIndex: 2, thenText: 'removes the row' },
  ],
  evidence: {},
  captureSections: [section('001-dialog.json', 'the dialog'), section('002-gone.json', 'after')],
}

const intentInput: JudgeInput = {
  behaviorId: 'DSH-010',
  behaviorTitle: 'at a glance',
  given: '',
  when: '',
  then: ['the dashboard answers what needs attention'],
  items: [{ itemIndex: 0, thenText: 'the dashboard answers what needs attention' }],
  evidence: {},
  intent: { statement: 'the dashboard answers what needs attention', status: 'current' },
  captureSections: [section('a.json', 'cards')],
}

describe('buildScenario', () => {
  it('projects a behavior input → the behavior variant with ALL graded items + full then-list', () => {
    const s = buildScenario(behaviorInput)
    expect(s.kind).toBe('behavior')
    if (s.kind !== 'behavior') throw new Error('expected behavior')
    expect(s.behaviorId).toBe('CON-042')
    expect(s.behaviorTitle).toBe('delete confirmation')
    expect(s.given).toBe('a contact exists')
    expect(s.when).toBe('the user deletes it')
    // BOTH graded items carried (a behavior call can grade >1 residue item).
    expect(s.items).toEqual([
      { itemIndex: 0, thenText: 'warns cannot be undone' },
      { itemIndex: 2, thenText: 'removes the row' },
    ])
    expect(s.allThen).toEqual(['warns cannot be undone', 'closes the dialog', 'removes the row'])
  })

  it('projects an intent input → the intent variant (statement + status, no GWT)', () => {
    const s = buildScenario(intentInput)
    expect(s.kind).toBe('intent')
    if (s.kind !== 'intent') throw new Error('expected intent')
    expect(s.intentId).toBe('DSH-010')
    expect(s.title).toBe('at a glance')
    expect(s.statement).toBe('the dashboard answers what needs attention')
    expect(s.status).toBe('current')
  })
})

describe('buildGradedEvidence', () => {
  it('emits one index-aligned entry per section with the REAL capture_file + its own screenshot', () => {
    const images = ['/runs/a.png', '/runs/b.png']
    const ge = buildGradedEvidence(behaviorInput, images)
    expect(ge).toHaveLength(2)
    expect(ge[0]).toEqual({
      captureFile: '001-dialog.json',
      note: 'the dialog',
      evidence: { url: 'http://x/001-dialog.json' },
      screenshot: '/runs/a.png',
    })
    expect(ge[1].captureFile).toBe('002-gone.json')
    expect(ge[1].screenshot).toBe('/runs/b.png')
  })

  it('leaves every screenshot undefined on the aria-only degrade (images: []) — honest, not a bug', () => {
    const ge = buildGradedEvidence(behaviorInput, [])
    expect(ge).toHaveLength(2)
    expect(ge.every(e => e.screenshot === undefined)).toBe(true)
    // The capture_file + note still survive — the reviewer keeps the attribution.
    expect(ge.map(e => e.captureFile)).toEqual(['001-dialog.json', '002-gone.json'])
  })
})

describe('SCREENSHOT_CAVEAT', () => {
  it('is fixed synthetic-free text (carries no PII to scrub)', () => {
    expect(SCREENSHOT_CAVEAT).toContain('UNDOCTORED')
    expect(SCREENSHOT_CAVEAT).not.toMatch(/@/)
  })
})
