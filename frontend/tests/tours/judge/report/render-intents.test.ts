// The Intents report section: present only when intent grades are passed,
// current-fail framed as regression, proposed-pass framed as progress, and
// over-cap drops surfaced (no silent truncation).

import { describe, expect, it } from 'vitest'
import type { IntentGrade } from '../intent-runner'
import { renderReport } from './render'

function grade(over: Partial<IntentGrade>): IntentGrade {
  return {
    intentId: 'DSH-010',
    title: 'at a glance',
    status: 'current',
    verdict: 'pass',
    boundCount: 3,
    droppedCount: 0,
    servedBy: ['CAD-026'],
    ...over,
  }
}

describe('renderReport intents section', () => {
  it('is absent without intent grades', () => {
    expect(renderReport({ grades: [] })).not.toContain('## Intents')
  })

  it('frames a current fail as a regression signal', () => {
    const md = renderReport({
      grades: [],
      intents: [grade({ verdict: 'fail', citation: 'CAPTURE[1]: heading "x"' })],
    })
    expect(md).toContain('## Intents — judged experience goals (advisory)')
    expect(md).toContain('REGRESSION SIGNAL')
    expect(md).toContain('cite: CAPTURE[1]: heading "x"')
  })

  it('frames a proposed pass as a progress signal', () => {
    const md = renderReport({
      grades: [],
      intents: [grade({ intentId: 'DSH-012', status: 'proposed', verdict: 'pass' })],
    })
    expect(md).toContain('progress signal')
  })

  it('surfaces over-cap drops', () => {
    const md = renderReport({ grades: [], intents: [grade({ droppedCount: 2 })] })
    expect(md).toContain('2 over the cap DROPPED')
  })

  it('carries the aria-only evidence caveat for visual intents without screenshots', () => {
    const md = renderReport({ grades: [], intents: [grade({ ariaOnly: true })] })
    expect(md).toContain('EVIDENCE CAVEAT: visual intent judged aria-only')
    expect(renderReport({ grades: [], intents: [grade({})] })).not.toContain('EVIDENCE CAVEAT')
  })
})
