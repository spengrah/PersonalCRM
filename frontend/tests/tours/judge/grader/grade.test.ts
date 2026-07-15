import { describe, it, expect } from 'vitest'
import { CLASSIFICATION } from './classification'
import { SPEC_CATALOG } from '../spec-catalog'
import { aggregate, applyGrounding, gradeBehavior, groupByBehavior } from './grade'
import { apiItem, cap, pair } from './fixtures'

// The index-faithful subset guard (INV-1): CLASSIFICATION is pinned to an
// EXPLICIT expected set of `${behaviorId}[${thenIndex}]:${grader}` rows. A
// migrated verifier row leaving (or a survivor silently re-indexed to a
// different valid catalog slot) shows up as a diff here. `thenIndex` stays
// faithful to the spec position and is never re-indexed, so a partially
// migrated behavior may keep a non-zero / gapped index (e.g. DSH-004[2]).
const EXPECTED_ROWS = [
  'CON-042[0]:judge',
  'CON-045[0]:verifier',
  'CON-045[1]:verifier',
  'CON-045[2]:verifier',
  'CON-045[3]:verifier',
  'CON-045[4]:verifier',
  'DSH-001[0]:verifier',
  'DSH-002[0]:verifier',
  'DSH-002[1]:verifier',
  'DSH-002[2]:verifier',
  'DSH-003[0]:verifier',
  'DSH-003[1]:verifier',
  'DSH-004[0]:verifier',
  'DSH-004[1]:verifier',
  'DSH-004[2]:judge',
  'DSH-005[0]:verifier',
  'DSH-005[1]:verifier',
  'DSH-005[2]:verifier',
  'DSH-005[3]:verifier',
  'DSH-007[0]:verifier',
  'DSH-007[1]:verifier',
  'CAD-026[0]:verifier',
  'CAD-026[1]:verifier',
  'CAD-026[2]:verifier',
  'CAD-027[0]:verifier',
  'CAD-027[1]:verifier',
  'CAD-027[2]:verifier',
  'CAD-028[0]:verifier',
  'CAD-028[1]:verifier',
  'CAD-028[2]:verifier',
  'CAD-029[0]:verifier',
  'CAD-029[1]:verifier',
  'CAD-029[2]:verifier',
  'CAD-029[3]:verifier',
  'CAD-030[0]:verifier',
  'CAD-030[1]:verifier',
  'CAD-030[2]:verifier',
  'CAD-030[3]:verifier',
  'CAD-031[0]:verifier',
  'CAD-031[1]:verifier',
  'CAD-031[2]:verifier',
  'CAD-033[0]:verifier',
  'CAD-033[1]:verifier',
]

describe('classification map', () => {
  it('matches the explicit index-faithful expected row set (INV-1)', () => {
    const actual = CLASSIFICATION.map(c => `${c.behaviorId}[${c.thenIndex}]:${c.grader}`).sort()
    expect(actual).toEqual([...EXPECTED_ROWS].sort())
  })

  it('has unique (behaviorId, thenIndex) keys', () => {
    const keys = CLASSIFICATION.map(c => `${c.behaviorId}[${c.thenIndex}]`)
    expect(new Set(keys).size).toBe(keys.length)
  })

  it('indexes every row within its catalog then-item ceiling (SSOT proxy)', () => {
    for (const c of CLASSIFICATION) {
      const spec = SPEC_CATALOG[c.behaviorId]
      expect(spec, `${c.behaviorId} catalog entry`).toBeDefined()
      expect(c.thenIndex, `${c.behaviorId}[${c.thenIndex}] within then.length`).toBeLessThan(
        spec.then.length
      )
    }
  })

  it('the judge owns the semantic residue: CON-042[0], DSH-004[2] (unbound routing is dynamic)', () => {
    const judgeItems = CLASSIFICATION.filter(c => c.grader === 'judge')
    expect(judgeItems.map(c => `${c.behaviorId}[${c.thenIndex}]`)).toEqual([
      'CON-042[0]',
      'DSH-004[2]',
    ])
  })
})

describe('unbound routing', () => {
  // DSH-004[1] (the overdue error surface) emits `unbound` when the failure
  // bracket lacks the 'Error loading overdue contacts' heading and shows no
  // caught-up/cards — the error copy may be renamed. It routes to the judge
  // dynamically, alongside DSH-004's static judge item [2].
  const unboundSet = () => ({
    behaviorId: 'DSH-004',
    captures: [
      cap({
        behaviors: ['DSH-004'],
        pair: pair('d', 'error'),
        note: 'overdue request failed',
        aria: { role: 'root' as const, children: [{ role: 'text', text: 'Something went wrong' }] },
      }),
    ],
  })

  it('routes an unbound verifier item to the judge verdict when available', () => {
    const grade = gradeBehavior(unboundSet(), {
      judge: { 1: { verdict: 'fail', citation: 'CAPTURE[0]: error surface' } },
    })
    const item = grade.items.find(i => i.thenIndex === 1)
    expect(item?.source).toBe('judge')
    expect(item?.verdict).toBe('fail')
  })

  it('grades an unrouted unbound item as pending unsure (verifiers-only mode)', () => {
    const grade = gradeBehavior(unboundSet())
    const item = grade.items.find(i => i.thenIndex === 1)
    expect(item?.source).toBe('pending')
    expect(item?.verdict).toBe('unsure')
    expect(item?.reason).toMatch(/judge routing pending/)
  })

  it('an unbound verdict never leaks into a graded item', () => {
    for (const g of [gradeBehavior(unboundSet())]) {
      for (const i of g.items) expect(['pass', 'fail', 'unsure']).toContain(i.verdict)
    }
  })
})

describe('aggregate', () => {
  it('any fail → fail; all pass → pass; else unsure', () => {
    expect(aggregate(['pass', 'pass'])).toBe('pass')
    expect(aggregate(['pass', 'fail', 'unsure'])).toBe('fail')
    expect(aggregate(['pass', 'unsure'])).toBe('unsure')
    expect(aggregate([])).toBe('unsure')
  })
})

describe('grounding rule', () => {
  it('downgrades a fail with no citation to unsure', () => {
    expect(applyGrounding({ verdict: 'fail', reason: 'x' }).verdict).toBe('unsure')
    expect(applyGrounding({ verdict: 'fail', citation: 'aria node' }).verdict).toBe('fail')
    expect(applyGrounding({ verdict: 'pass' }).verdict).toBe('pass')
  })
})

describe('gradeBehavior', () => {
  it('judge item is pending (unsure) in verifiers-only mode', () => {
    const g = gradeBehavior({
      behaviorId: 'CON-042',
      captures: [
        cap({
          behaviors: ['CON-042'],
          pair: pair('del', 'after-dismiss'),
          apiResponses: { 'GET /api/v1/contacts/:id': [apiItem({ status: 200, probe: true })] },
        }),
        cap({
          behaviors: ['CON-042'],
          pair: pair('del', 'after-accept'),
          url: '/contacts',
          apiResponses: {
            'DELETE /api/v1/contacts/:id': [apiItem({ method: 'DELETE', status: 204 })],
            'GET /api/v1/contacts/:id': [apiItem({ status: 404, probe: true })],
          },
        }),
      ],
    })
    const judgeItem = g.items.find(i => i.thenIndex === 0)
    expect(judgeItem?.grader).toBe('judge')
    expect(judgeItem?.source).toBe('pending')
    expect(judgeItem?.verdict).toBe('unsure')
    // The behavior aggregates to unsure (a judge item is pending) even though
    // the verifier items pass.
    expect(g.behaviorVerdict).toBe('unsure')
  })

  it('merges a supplied judge verdict for the judge item (and grounds it)', () => {
    const g = gradeBehavior(
      { behaviorId: 'CON-042', captures: [] },
      { judge: { 0: { verdict: 'fail', reason: 'no citation' } } }
    )
    const judgeItem = g.items.find(i => i.thenIndex === 0)
    // fail without a citation is downgraded to unsure by the grounding rule
    expect(judgeItem?.verdict).toBe('unsure')
  })
})

describe('groupByBehavior', () => {
  it('routes each capture into every behavior it tags, sorted by id', () => {
    const a = cap({ behaviors: ['CON-038', 'CON-040'] })
    const b = cap({ behaviors: ['CON-040'] })
    const groups = groupByBehavior([a, b])
    expect(groups.map(g => g.behaviorId)).toEqual(['CON-038', 'CON-040'])
    expect(groups[0].captures).toHaveLength(1)
    expect(groups[1].captures).toHaveLength(2)
  })
})
