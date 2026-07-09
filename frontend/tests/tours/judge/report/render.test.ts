import { describe, it, expect } from 'vitest'
import { renderReport } from './render'
import type { BehaviorGrade } from '../grader/grade'

const grades: BehaviorGrade[] = [
  {
    behaviorId: 'CON-041',
    behaviorVerdict: 'fail',
    items: [
      {
        thenIndex: 0,
        grader: 'verifier',
        source: 'verifier',
        verdict: 'pass',
        citation: 'aria heading',
      },
      {
        thenIndex: 1,
        grader: 'verifier',
        source: 'verifier',
        verdict: 'fail',
        citation: 'url',
        reason: 'action= not stripped',
      },
    ],
  },
  {
    behaviorId: 'CON-042',
    behaviorVerdict: 'unsure',
    items: [
      {
        thenIndex: 0,
        grader: 'judge',
        source: 'pending',
        verdict: 'unsure',
        reason: 'judge-only item',
      },
      {
        thenIndex: 1,
        grader: 'verifier',
        source: 'verifier',
        verdict: 'pass',
        citation: 'probe 404',
      },
      {
        thenIndex: 2,
        grader: 'verifier',
        source: 'verifier',
        verdict: 'pass',
        citation: 'url /contacts',
      },
    ],
  },
]

describe('renderReport', () => {
  const md = renderReport({ meta: { runId: 'r1', gitSha: 'abc' }, grades })

  it('is advisory and files no issues', () => {
    expect(md).toMatch(/ADVISORY ONLY/)
    expect(md).toMatch(/files NO issues/i)
    expect(md).not.toMatch(/gh issue/i)
  })

  it('rolls up per-behavior verdicts and per-item detail', () => {
    expect(md).toContain('| CON-041 |')
    expect(md).toContain('action= not stripped')
    expect(md).toContain('[0]')
    expect(md).toMatch(/judge \(pending labels\)/)
  })

  it('lists the capture-coverage caveats (CON-038[0], CON-040[0])', () => {
    expect(md).toMatch(/CON-038\[0\]/)
    expect(md).toMatch(/CON-040\[0\]/)
  })

  it('stubs the label-gated metrics as N/A', () => {
    expect(md).toMatch(/fail-precision.*N\/A/)
    expect(md).toMatch(/judge\/DEFERRED\.md/)
  })
})
