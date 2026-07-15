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

  it('omits the capture-coverage caveats section once every caveat row has migrated', () => {
    // All CON/DSH/CAD verifier rows (incl. their `caveat` records) have moved
    // to Playwright E2E, so CLASSIFICATION carries no caveat rows and the
    // report honestly renders no caveat section. (DSH-005[1]/CAD-030[0] still
    // appear in the static skip-list section — a separate SSOT, untouched here.)
    expect(md).not.toMatch(/Capture-coverage caveats/)
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
