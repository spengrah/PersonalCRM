import { describe, expect, it } from 'vitest'
import {
  DEFAULT_INTENT_EFFORT,
  DEFAULT_INTENT_MODEL,
  DEFAULT_JUDGE_EFFORT,
  DEFAULT_JUDGE_MODEL,
} from './models'

describe('judge model defaults', () => {
  it('pins the spec values for both passes', () => {
    expect(DEFAULT_JUDGE_MODEL).toBe('gpt-5.4-mini')
    expect(DEFAULT_JUDGE_EFFORT).toBe('low')
    expect(DEFAULT_INTENT_MODEL).toBe('gpt-5.5')
    expect(DEFAULT_INTENT_EFFORT).toBe('medium')
  })
})

// Guard for the sole-home invariant. A "do the suites still pass" check cannot
// see a compatibility re-export left behind at a retired location
// (`export { DEFAULT_JUDGE_MODEL } from '../models'`): every suite stays green
// while the ambiguity this module exists to remove quietly returns. Asserting
// over each retired module's own EXPORT NAMESPACE catches that regardless of
// syntax — a re-export, a re-declaration, and a multi-line `export {}` block all
// show up the same way, where a source-text grep would miss at least one.
describe('models.ts is the sole home of the defaults', () => {
  const RETIRED: Record<string, () => Promise<Record<string, unknown>>> = {
    'adapter/codex-exec.ts': () => import('./adapter/codex-exec'),
    'adapter/codex-sdk.ts': () => import('./adapter/codex-sdk'),
    'intent-runner.ts': () => import('./intent-runner'),
  }

  it.each(Object.keys(RETIRED))('%s exports no DEFAULT_ name', async rel => {
    const ns = await RETIRED[rel]()
    expect(Object.keys(ns).filter(k => k.startsWith('DEFAULT_'))).toEqual([])
  })

  it('models.ts is where they actually live', async () => {
    const ns = await import('./models')
    expect(
      Object.keys(ns)
        .filter(k => k.startsWith('DEFAULT_'))
        .sort()
    ).toEqual([
      'DEFAULT_INTENT_EFFORT',
      'DEFAULT_INTENT_MODEL',
      'DEFAULT_JUDGE_EFFORT',
      'DEFAULT_JUDGE_MODEL',
    ])
  })
})
