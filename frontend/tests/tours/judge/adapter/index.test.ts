import { describe, it, expect, afterEach } from 'vitest'
import { selectJudge } from './index'
import type { JudgeInput } from './types'

const input: JudgeInput = {
  behaviorId: 'CON-042',
  behaviorTitle: 't',
  given: 'g',
  when: 'w',
  then: ['warns'],
  items: [{ itemIndex: 0, thenText: 'warns' }],
  evidence: {},
}

describe('selectJudge', () => {
  const origFetch = globalThis.fetch
  const origUrl = process.env.QA_JUDGE_HTTP_URL
  afterEach(() => {
    globalThis.fetch = origFetch
    if (origUrl === undefined) delete process.env.QA_JUDGE_HTTP_URL
    else process.env.QA_JUDGE_HTTP_URL = origUrl
  })

  it('threads the model arg into the selected adapter (http POSTs that model)', async () => {
    process.env.QA_JUDGE_HTTP_URL = 'http://judge.local/v1/chat/completions'
    let sentModel: unknown
    globalThis.fetch = (async (_url: string, init: { body: string }) => {
      sentModel = JSON.parse(init.body).model
      return {
        ok: true,
        status: 200,
        json: async () => ({ choices: [{ message: { content: '{"verdicts":[]}' } }] }),
      }
    }) as unknown as typeof fetch

    const judge = selectJudge('http', 'stronger-intent-model')
    await judge(input)
    expect(sentModel).toBe('stronger-intent-model')
  })

  it('selects the codex-sdk adapter (a Judge, constructed without loading the SDK runtime)', () => {
    // Constructing the adapter must not spawn/import the SDK — that only happens
    // on a live run. Selecting it returns a callable Judge.
    expect(typeof selectJudge('codex-sdk')).toBe('function')
    expect(typeof selectJudge('codex-sdk', 'gpt-5.5')).toBe('function')
  })

  it('rejects an unknown profile', () => {
    expect(() => selectJudge('nope')).toThrow(/unknown QA_JUDGE/)
  })
})
