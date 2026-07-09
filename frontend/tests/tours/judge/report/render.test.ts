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

  it('lists the capture-coverage caveats (the provider / multi-surface abstains)', () => {
    expect(md).toMatch(/Capture-coverage caveats/)
    // The lifted CON-038[0]/CON-040[0] caveats are gone (now toured).
    expect(md).not.toMatch(/CON-038\[0\]/)
    expect(md).not.toMatch(/CON-040\[0\]/)
    // A representative surviving caveat is present.
    expect(md).toMatch(/DSH-005\[1\]|CAD-030\[0\]/)
  })

  it('renders the first-cut coverage section + skip-list (D5)', () => {
    expect(md).toContain('## Coverage — first-cut scope')
    expect(md).toContain('### contacts')
    expect(md).toContain('### dashboard')
    expect(md).toContain('### cadence-followup')
    // Behaviors in the run's grades are toured; the rest are untoured.
    expect(md).toMatch(/✅ toured — \*\*CON-041\*\*/)
    expect(md).toMatch(/⬜ untoured — \*\*DSH-001\*\*/)
    // The skip-list carries the proposed + provider-dependent entries with reasons.
    expect(md).toContain('### Skip-list')
    expect(md).toMatch(/DSH-006/)
    expect(md).toMatch(/CAD-033\[0\]/)
  })

  it('stubs the label-gated metrics as N/A', () => {
    expect(md).toMatch(/fail-precision.*N\/A/)
    expect(md).toMatch(/judge\/DEFERRED\.md/)
  })
})
