import { describe, it, expect, vi } from 'vitest'
import { main, renderReport, runJudgeRound, type JudgesBundle } from './render'
import { gradeBehavior, groupByBehavior, type BehaviorGrade } from '../grader/grade'
import { apiItem, cap, pair } from '../grader/fixtures'
import { TRAPS } from '../trap-config'
import { allIntents } from '../intent-catalog'
import type { Judge, PerItemVerdict } from '../adapter/types'
import type { ItemVerdicts } from '../grader/types'
import type { TrapResult } from '../trap-selftest'
import type { Capture } from '../../support/types'

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

  it('renders a completed judge verdict as (judge), not pending labels', () => {
    const judged: BehaviorGrade[] = [
      {
        behaviorId: 'CON-042',
        behaviorVerdict: 'fail',
        items: [
          {
            thenIndex: 0,
            grader: 'judge',
            source: 'judge',
            verdict: 'fail',
            citation: 'DIALOGS[0].message',
            reason: 'no irreversibility warning',
          },
        ],
      },
    ]
    const out = renderReport({ grades: judged })
    // A completed judge verdict renders the source label "(judge)" — distinct
    // from the "(judge (pending labels))" a source:'pending' item renders.
    expect(out).toMatch(/\[0\] ❌ \*\*fail\*\* \(judge\)/)
    expect(out).toContain('cite: DIALOGS[0].message')
    expect(out).not.toMatch(/\(judge \(pending labels\)\)/)
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

  it('renders the Detection self-test section with each trap status', () => {
    const trapResults: TrapResult[] = [
      {
        id: 'trap-a',
        targetBehavior: 'CON-042',
        targetItem: 0,
        status: 'caught',
        reason: 'caught',
      },
      {
        id: 'trap-b',
        targetBehavior: 'DSH-004',
        targetItem: 2,
        status: 'missed',
        reason: 'passed',
      },
      {
        id: 'trap-c',
        targetBehavior: 'CON-042',
        targetItem: 0,
        status: 'error',
        reason: 'no-oped',
      },
    ]
    const out = renderReport({ grades, trapResults })
    expect(out).toContain('## Detection self-test (traps)')
    expect(out).toMatch(/✅ \*\*caught\*\* — `trap-a`/)
    expect(out).toMatch(/❌ \*\*missed\*\* — `trap-b`/)
    expect(out).toMatch(/🔴 \*\*error\*\* — `trap-c`/)
    // The lane is visibly the DETECTOR self-test, not the advisory app verdicts.
    expect(out).toMatch(/Tests the JUDGE \(the detector\), NOT the app/)
  })

  it('omits the self-test section when no trapResults are supplied (bare report)', () => {
    const out = renderReport({ grades })
    expect(out).not.toContain('Detection self-test')
  })
})

// A CON-042 capture with the delete-confirm dialog + a DSH-004 error capture
// with the retried 500 bracket — the shapes the two committed traps doctor.
function roundCaptures(): Capture[] {
  const five = { error: { message: 'overdue fetch failed' } }
  return [
    cap({
      behaviors: ['CON-042'],
      dialogs: [{ type: 'confirm', message: 'Are you sure? This action cannot be undone.' }],
    }),
    cap({
      behaviors: ['DSH-004'],
      pair: pair('dsh004', 'error'),
      apiResponses: {
        'GET /api/v1/contacts/overdue': [
          apiItem({ status: 500, body: five }),
          apiItem({ status: 500, body: five }),
          apiItem({ status: 500, body: five }),
          apiItem({ status: 500, body: five }),
        ],
      },
    }),
  ]
}

// A trap judge returning the same verdict for both trap items (CON-042[0] +
// DSH-004[2]); each trap finds its own item.
function trapJudgeReturning(verdict: PerItemVerdict['verdict']): Judge {
  return async () => [
    { itemIndex: 0, verdict, citation: 'cite', critique: 'c' },
    { itemIndex: 2, verdict, citation: 'cite', critique: 'c' },
  ]
}

function bundle(trapJudge: Judge): JudgesBundle {
  return {
    residueRunner: async (): Promise<ItemVerdicts> => ({}),
    intentJudge: async () => [],
    trapJudge,
    traps: TRAPS,
  }
}

describe('runJudgeRound — orchestration + hard-exit path (dependency-injected)', () => {
  it('CAUGHT round: all traps caught → exitCode 0, self-test section rendered', async () => {
    const { markdown, trapResults, exitCode } = await runJudgeRound(roundCaptures(), {
      judges: bundle(trapJudgeReturning('fail')),
    })
    expect(exitCode).toBe(0)
    expect(trapResults.every(r => r.status === 'caught')).toBe(true)
    expect(markdown).toContain('## Detection self-test (traps)')
  })

  it('MISSED round: a passed trap → exitCode 1 WITH the markdown still produced', async () => {
    const { markdown, exitCode, trapResults } = await runJudgeRound(roundCaptures(), {
      judges: bundle(trapJudgeReturning('pass')),
    })
    expect(exitCode).toBe(1)
    expect(trapResults.some(r => r.status === 'missed')).toBe(true)
    // The report is written even on a hard-fail — a missed trap must leave
    // reviewable evidence (no throw before render).
    expect(markdown).toContain('# Agentic UX QA')
    expect(markdown).toMatch(/❌ \*\*missed\*\*/)
  })

  it('a per-trap exception → error result, exitCode 1, markdown still produced', async () => {
    const throwing: Judge = async () => {
      throw new Error('adapter blew up')
    }
    const { markdown, exitCode, trapResults } = await runJudgeRound(roundCaptures(), {
      judges: bundle(throwing),
    })
    expect(exitCode).toBe(1)
    expect(trapResults.some(r => r.status === 'error')).toBe(true)
    expect(markdown).toContain('# Agentic UX QA')
    expect(markdown).toMatch(/🔴 \*\*error\*\*/)
  })

  it('NON-JUDGE round (judges undefined): no dep called, no traps, exit 0, section omitted', async () => {
    const residueRunner = vi.fn(async (): Promise<ItemVerdicts> => ({}))
    const intentJudge = vi.fn<Judge>(async () => [])
    const trapJudge = vi.fn<Judge>(async () => [])
    void { residueRunner, intentJudge, trapJudge } // spies in scope, deliberately NOT wired

    const { markdown, trapResults, exitCode } = await runJudgeRound(roundCaptures(), {
      judges: undefined,
    })
    expect(exitCode).toBe(0)
    expect(trapResults).toEqual([])
    // None of the judge deps run on a bare round.
    expect(residueRunner).not.toHaveBeenCalled()
    expect(intentJudge).not.toHaveBeenCalled()
    expect(trapJudge).not.toHaveBeenCalled()
    // Residue items render "pending labels"; no self-test section is emitted.
    expect(markdown).toMatch(/pending labels/)
    expect(markdown).not.toContain('Detection self-test')
  })

  it('a RESIDUE judge exception → report STILL written, lane-error section, exitCode 1', async () => {
    const throwingResidue: JudgesBundle = {
      ...bundle(trapJudgeReturning('fail')),
      residueRunner: async () => {
        throw new Error('residue adapter blew up')
      },
    }
    const { markdown, exitCode } = await runJudgeRound(roundCaptures(), { judges: throwingResidue })
    // The exception is caught, NOT propagated — the report is written and the
    // failure is both visible AND reflected in the exit status.
    expect(exitCode).toBe(1)
    expect(markdown).toContain('# Agentic UX QA')
    expect(markdown).toContain('## Judge-lane errors')
    expect(markdown).toMatch(/residue judge for .*: residue adapter blew up/)
  })

  it('an INTENT-pass exception → report STILL written, lane-error section, exitCode 1', async () => {
    // Tag a capture with an intent id so the intent pass actually calls the
    // (throwing) intent judge.
    const captures = [cap({ behaviors: [allIntents()[0].id] }), ...roundCaptures()]
    const throwingIntent: JudgesBundle = {
      ...bundle(trapJudgeReturning('fail')),
      intentJudge: async () => {
        throw new Error('intent adapter blew up')
      },
    }
    const { markdown, exitCode } = await runJudgeRound(captures, { judges: throwingIntent })
    expect(exitCode).toBe(1)
    expect(markdown).toContain('# Agentic UX QA')
    expect(markdown).toMatch(/intent pass: intent adapter blew up/)
  })
})

describe('main() CLI exit behavior (deferred process.exitCode, never process.exit)', () => {
  // main() sets the global process.exitCode; save + restore it (and argv) so a
  // guarded exit code can't leak into vitest's own process status.
  async function runMainWith(args: string[]): Promise<number | undefined> {
    const savedArgv = process.argv
    const savedEc = process.exitCode
    process.argv = ['node', 'render.ts', ...args]
    try {
      await main()
      return process.exitCode as number | undefined
    } finally {
      process.argv = savedArgv
      process.exitCode = savedEc
    }
  }

  it('--help sets exitCode 0 and returns (no process.exit)', async () => {
    expect(await runMainWith(['--help'])).toBe(0)
  })

  it('a missing run dir sets exitCode 2 and returns (no process.exit)', async () => {
    expect(await runMainWith([])).toBe(2)
  })
})
