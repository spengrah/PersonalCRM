import { describe, expect, it } from 'vitest'
import {
  activeModels,
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

describe('activeModels', () => {
  it('returns both passes’ defaults, sorted', () => {
    expect(activeModels({})).toEqual(['gpt-5.4-mini', 'gpt-5.5'])
  })

  it('honors both env overrides', () => {
    expect(
      activeModels({ QA_JUDGE_MODEL: 'gpt-5.6-luna', QA_INTENT_MODEL: 'gpt-5.6-terra' })
    ).toEqual(['gpt-5.6-luna', 'gpt-5.6-terra'])
  })

  // The intent model is the expensive one; resolving only QA_JUDGE_MODEL would
  // leave an experiment's intent model unpriced while everything still looked
  // healthy.
  it('honors the intent override on its own', () => {
    expect(activeModels({ QA_INTENT_MODEL: 'gpt-5.6-terra' })).toEqual([
      'gpt-5.4-mini',
      'gpt-5.6-terra',
    ])
  })

  it('honors the ux override on its own', () => {
    expect(activeModels({ QA_JUDGE_MODEL: 'gpt-5.6-luna' })).toEqual(['gpt-5.5', 'gpt-5.6-luna'])
  })

  it('dedupes when both passes resolve to the same model', () => {
    expect(activeModels({ QA_JUDGE_MODEL: 'gpt-5.5' })).toEqual(['gpt-5.5'])
  })

  // `??` parity with the transports: an empty override must NOT fall back to
  // the default here, because the transport resolves it to '' and then sends no
  // model at all. An '' target matches nothing upstream and is reported loudly;
  // a defaulted target would silently price a model nothing sends.
  // Pinned for BOTH variables: a `||` regression on either branch alone would
  // otherwise stay green here.
  it('does not default away an empty-string ux override', () => {
    expect(activeModels({ QA_JUDGE_MODEL: '' })).toEqual(['', 'gpt-5.5'])
  })

  it('does not default away an empty-string intent override', () => {
    expect(activeModels({ QA_INTENT_MODEL: '' })).toEqual(['', 'gpt-5.4-mini'])
  })

  // The default argument is the production path — every other test passes an
  // explicit env, so swapping `process.env` for `{}` would leave them all green
  // while real callers stopped seeing overrides entirely.
  it('reads process.env when called with no argument', () => {
    const prevJudge = process.env.QA_JUDGE_MODEL
    const prevIntent = process.env.QA_INTENT_MODEL
    try {
      process.env.QA_JUDGE_MODEL = 'gpt-5.6-luna'
      process.env.QA_INTENT_MODEL = 'gpt-5.6-terra'
      expect(activeModels()).toEqual(['gpt-5.6-luna', 'gpt-5.6-terra'])
    } finally {
      if (prevJudge === undefined) delete process.env.QA_JUDGE_MODEL
      else process.env.QA_JUDGE_MODEL = prevJudge
      if (prevIntent === undefined) delete process.env.QA_INTENT_MODEL
      else process.env.QA_INTENT_MODEL = prevIntent
    }
  })

  // The http adapter resolves its own model and is a reference stub no round
  // runs — its default is deliberately not a sync target.
  it('excludes the http stub’s model', () => {
    expect(activeModels({})).not.toContain('gpt-4o-mini')
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
