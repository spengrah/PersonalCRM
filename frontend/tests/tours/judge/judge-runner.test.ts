import { describe, it, expect, vi } from 'vitest'
import { runnerFromJudge } from './judge-runner'
import type { Judge } from './adapter'
import type { Capture } from '../support/types'

// A minimal capture tagging a behavior; the evidence content is irrelevant to
// the runner (buildJudgeInput derives residue items from the behavior's spec +
// classification, not from the capture's payload).
const cap = (behavior: string): Capture =>
  ({
    behaviors: [behavior],
    aria: { role: 'root', children: [] },
    apiResponses: {},
    url: '/x',
    serverTime: { now: '2026-01-01T00:00:00Z', accelerationFactor: 1, isAccelerated: false },
    dialogs: [],
  }) as unknown as Capture

describe('runnerFromJudge', () => {
  it('maps the judge per-item verdicts into ItemVerdicts keyed by then-index', async () => {
    // CON-042[0] ("a confirmation prompt warns…") is a judge-tagged residue item.
    const judge: Judge = vi.fn(async () => [
      {
        itemIndex: 0,
        verdict: 'fail' as const,
        citation: 'DIALOGS[0].message',
        critique: 'empty warning',
      },
    ])
    const run = runnerFromJudge(judge)
    const out = await run('CON-042', [cap('CON-042')])
    expect(judge).toHaveBeenCalledOnce()
    expect(out).toEqual({
      0: { verdict: 'fail', citation: 'DIALOGS[0].message', reason: 'empty warning' },
    })
  })

  it('returns {} and never calls the judge when the behavior has no residue items', async () => {
    const judge: Judge = vi.fn(async () => [])
    const run = runnerFromJudge(judge)
    // An unknown behavior has no spec → buildJudgeInput returns undefined.
    const out = await run('ZZZ-999', [cap('ZZZ-999')])
    expect(out).toEqual({})
    expect(judge).not.toHaveBeenCalled()
  })
})
