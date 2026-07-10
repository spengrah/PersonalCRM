// The intent pass with a fake judge: zero-evidence abstains WITHOUT a model
// call, verdicts flow through the grounding rule (uncited fail → unsure), and
// missing verdicts degrade to unsure.

import { describe, expect, it } from 'vitest'
import type { Judge, JudgeInput } from './adapter'
import type { Capture } from '../support/types'
import type { IntentSpec } from './intent-catalog'
import { runIntentPass } from './intent-runner'

function cap(behaviors: string[]): Capture {
  return {
    captureFormatVersion: 1,
    captureGeneratorVersion: 1,
    tour: 'dashboard',
    seq: 1,
    behaviors,
    note: 'n',
    url: 'http://x',
    pair: null,
    serverTime: { currentTime: 't', isAccelerated: true, accelerationFactor: 1, baseTime: 't' },
    aria: { role: 'root', children: [] },
    apiResponses: {},
    dialogs: [],
  }
}

const intent: IntentSpec = {
  id: 'DSH-010',
  title: 'at a glance',
  statement: 's',
  status: 'current',
  servedBy: ['CAD-026'],
}

function fakeJudge(verdict: 'pass' | 'fail' | 'unsure', citation: string): Judge {
  return async (_input: JudgeInput) => [{ itemIndex: 0, verdict, citation, critique: 'c' }]
}

describe('runIntentPass', () => {
  it('abstains without a judge call when no evidence binds', async () => {
    let called = 0
    const judge: Judge = async input => {
      called++
      return input.items.map(i => ({
        itemIndex: i.itemIndex,
        verdict: 'pass' as const,
        citation: '',
        critique: '',
      }))
    }
    const [g] = await runIntentPass([cap(['DSH-002'])], judge, [intent])
    expect(called).toBe(0)
    expect(g.verdict).toBe('unsure')
    expect(g.boundCount).toBe(0)
    expect(g.reason).toMatch(/no evidence bound/)
  })

  it('passes a grounded verdict through', async () => {
    const [g] = await runIntentPass([cap(['CAD-026'])], fakeJudge('pass', ''), [intent])
    expect(g.verdict).toBe('pass')
    expect(g.boundCount).toBe(1)
    expect(g.status).toBe('current')
  })

  it('downgrades an uncited fail to unsure (grounding rule)', async () => {
    const [g] = await runIntentPass([cap(['CAD-026'])], fakeJudge('fail', ''), [intent])
    expect(g.verdict).toBe('unsure')
    expect(g.reason).toMatch(/no grounding citation/)
  })

  it('keeps a fail cited with a CAPTURE[n] index', async () => {
    const [g] = await runIntentPass(
      [cap(['CAD-026'])],
      fakeJudge('fail', 'CAPTURE[0]: heading "x"'),
      [intent]
    )
    expect(g.verdict).toBe('fail')
    expect(g.citation).toBe('CAPTURE[0]: heading "x"')
  })

  it('downgrades a fail whose citation lacks the CAPTURE[n] index', async () => {
    const [g] = await runIntentPass(
      [cap(['CAD-026'])],
      fakeJudge('fail', 'heading "Error loading overdue contacts"'),
      [intent]
    )
    expect(g.verdict).toBe('unsure')
    expect(g.reason).toMatch(/citation needs an in-range CAPTURE\[n\] index/)
  })

  it('downgrades a fail cited with a bare capture marker (no node/path)', async () => {
    const [g] = await runIntentPass([cap(['CAD-026'])], fakeJudge('fail', 'CAPTURE[0]:'), [intent])
    expect(g.verdict).toBe('unsure')
  })

  it('downgrades a fail whose residue is punctuation-only', async () => {
    const [g] = await runIntentPass([cap(['CAD-026'])], fakeJudge('fail', 'CAPTURE[0] ()."'), [
      intent,
    ])
    expect(g.verdict).toBe('unsure')
  })

  it('downgrades a fail whose capture index is out of range', async () => {
    const [g] = await runIntentPass(
      [cap(['CAD-026'])],
      fakeJudge('fail', 'CAPTURE[99]: heading "x"'),
      [intent]
    )
    expect(g.verdict).toBe('unsure')
  })

  it('degrades a missing verdict to unsure', async () => {
    const judge: Judge = async () => []
    const [g] = await runIntentPass([cap(['CAD-026'])], judge, [intent])
    expect(g.verdict).toBe('unsure')
    expect(g.reason).toMatch(/no verdict returned/)
  })

  it('flags a visual intent judged without screenshots as ariaOnly', async () => {
    const visual = { ...intent, visual: true }
    const [g] = await runIntentPass([cap(['CAD-026'])], fakeJudge('pass', ''), [visual])
    expect(g.ariaOnly).toBe(true)
  })

  it('does not flag ariaOnly when screenshots attach or the intent is not visual', async () => {
    const visual = { ...intent, visual: true }
    const [withShots] = await runIntentPass(
      [cap(['CAD-026'])],
      fakeJudge('pass', ''),
      [visual],
      8,
      () => '/runs/x/shot.png'
    )
    expect(withShots.ariaOnly).toBeUndefined()
    const [nonVisual] = await runIntentPass([cap(['CAD-026'])], fakeJudge('pass', ''), [intent])
    expect(nonVisual.ariaOnly).toBeUndefined()
  })
})
