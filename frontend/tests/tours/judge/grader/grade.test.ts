import { describe, it, expect } from 'vitest'
import { CLASSIFICATION, CLASSIFICATION_ITEM_COUNT, classificationFor } from './classification'
import { aggregate, applyGrounding, gradeBehavior, groupByBehavior } from './grade'
import { apiItem, cap, pair } from './fixtures'

describe('classification map', () => {
  it('has exactly one row per spec then-item (23 total, index-faithful)', () => {
    expect(CLASSIFICATION).toHaveLength(CLASSIFICATION_ITEM_COUNT)
    const counts: Record<string, number> = {}
    for (const c of CLASSIFICATION) counts[c.behaviorId] = (counts[c.behaviorId] ?? 0) + 1
    expect(counts).toEqual({
      'CON-038': 2,
      'CON-040': 4,
      'CON-041': 2,
      'CON-042': 3,
      'CON-043': 6,
      'CON-044': 1,
      'CON-045': 5,
    })
    // then indices are 0..n-1 with no gaps/dupes
    for (const b of Object.keys(counts)) {
      const idxs = classificationFor(b).map(c => c.thenIndex)
      expect(idxs).toEqual([...Array(counts[b]).keys()])
    }
  })

  it('the judge residue is tiny: exactly CON-042[0] is judge-tagged; CON-043[5] is the fallback', () => {
    const judgeItems = CLASSIFICATION.filter(c => c.grader === 'judge')
    expect(judgeItems.map(c => `${c.behaviorId}[${c.thenIndex}]`)).toEqual(['CON-042[0]'])
    const fallbacks = CLASSIFICATION.filter(c => c.judgeFallback)
    expect(fallbacks.map(c => `${c.behaviorId}[${c.thenIndex}]`)).toEqual(['CON-043[5]'])
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
