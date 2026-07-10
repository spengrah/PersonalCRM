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

    const judge = selectJudge('http', 'stronger-labeler-model')
    await judge(input)
    expect(sentModel).toBe('stronger-labeler-model')
  })

  it('selects the claude adapter (labeler drafter) without throwing', () => {
    // Model threading into `claude -p --model` is proven at the adapter level
    // (claude.test.ts); selectJudge passes the model opt identically to http.
    expect(typeof selectJudge('claude', 'stronger-labeler-model')).toBe('function')
  })

  it('codex-sdk is deferred (throws pointing at DEFERRED.md)', () => {
    expect(() => selectJudge('codex-sdk')).toThrow(/DEFERRED\.md/)
  })

  it('rejects an unknown profile', () => {
    expect(() => selectJudge('nope')).toThrow(/unknown QA_JUDGE/)
  })
})
