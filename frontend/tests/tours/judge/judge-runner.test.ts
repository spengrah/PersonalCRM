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
  it('two-phase: an unbound verifier item joins the judge call dynamically', async () => {
    // CON-041 has NO statically judge-tagged items, but a present capture with
    // no 'Edit Contact'/'Merge Contacts' heading makes [0] emit `unbound`.
    const asked: number[] = []
    const judge: Judge = vi.fn(async (input: Parameters<Judge>[0]) => {
      asked.push(...input.items.map(i => i.itemIndex))
      return input.items.map(i => ({
        itemIndex: i.itemIndex,
        verdict: 'unsure' as const,
        citation: '',
        critique: '',
      }))
    })
    const run = runnerFromJudge(judge)
    const c = {
      ...cap('CON-041'),
      note: 'action=edit consumed',
      aria: { role: 'root', children: [{ role: 'heading', name: 'Renamed Heading' }] },
    } as unknown as Capture
    const out = await run('CON-041', [c])
    expect(judge).toHaveBeenCalledOnce()
    expect(asked).toContain(0)
    expect(out[0]).toBeDefined()
  })

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
