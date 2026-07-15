import { describe, it, expect } from 'vitest'
import { renderReport } from './render'
import { gradeBehavior, groupByBehavior, type BehaviorGrade } from '../grader/grade'
import { cap } from '../grader/fixtures'

// Exactly ONE residue behavior in `grades` so both coverage branches are
// exercised: `renderCoverage` marks a behavior toured IFF it appears in grades,
// so CON-042 (present) renders ✅ toured and DSH-004 (catalog-only) renders
// ⬜ untoured.
const grades: BehaviorGrade[] = [
  {
    behaviorId: 'CON-042',
    behaviorVerdict: 'unsure',
    items: [
      {
        thenIndex: 0,
        grader: 'judge',
        source: 'pending',
        verdict: 'unsure',
        reason: 'judge-only item — no judge verdict supplied',
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
    expect(md).toContain('| CON-042 |')
    expect(md).toContain('[0]')
    expect(md).toMatch(/judge \(pending labels\)/)
  })

  it('omits the capture-coverage caveats section once every caveat row has migrated', () => {
    // The verifier lane (incl. its `caveat` records) migrated to Playwright E2E,
    // and the `caveat` field is gone from the classification, so the report
    // honestly renders no caveat section. (DSH-005[1]/CAD-030[0] still appear in
    // the static skip-list section — a separate SSOT, untouched here.)
    expect(md).not.toMatch(/Capture-coverage caveats/)
  })

  it('renders the first-cut coverage section + skip-list (D5)', () => {
    expect(md).toContain('## Coverage — first-cut scope')
    expect(md).toContain('### contacts')
    expect(md).toContain('### dashboard')
    // A behavior in the run's grades is toured; a catalog behavior absent from
    // grades is untoured.
    expect(md).toMatch(/✅ toured — \*\*CON-042\*\*/)
    expect(md).toMatch(/⬜ untoured — \*\*DSH-004\*\*/)
    // The skip-list carries the proposed + provider-dependent entries with reasons.
    expect(md).toContain('### Skip-list')
    expect(md).toMatch(/DSH-006/)
    expect(md).toMatch(/CAD-033\[0\]/)
  })

  it('stubs the label-gated metrics as N/A', () => {
    expect(md).toMatch(/fail-precision.*N\/A/)
    expect(md).toMatch(/judge\/DEFERRED\.md/)
  })

  it('grades then renders a migrated-tagged + both-residue capture set without throwing (INV-3)', () => {
    // A tour still tags captures for a fully-migrated behavior (CON-038 has no
    // classification row → grades to zero items) alongside the two residue
    // behaviors. The full groupByBehavior → gradeBehavior → renderReport path
    // must tolerate the empty grade and render cleanly.
    const captures = [
      cap({ behaviors: ['CON-038'] }),
      cap({ behaviors: ['CON-042'] }),
      cap({ behaviors: ['DSH-004'] }),
    ]
    const graded = groupByBehavior(captures).map(set => gradeBehavior(set))
    const out = renderReport({ grades: graded })
    expect(out).toContain('# Agentic UX QA')
    expect(out.length).toBeGreaterThan(0)
  })
})
