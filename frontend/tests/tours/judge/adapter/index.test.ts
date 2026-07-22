import * as fs from 'fs'
import * as path from 'path'
import { describe, it, expect, afterEach } from 'vitest'
import { DEFAULT_JUDGE_KIND, selectJudge } from './index'
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

// The default transport is a COST-CORRECTNESS property, not a preference. The
// codex-exec event stream reports one inclusive `input_tokens` and no cached-read
// figure, so a round on that transport prices every cached token at the full input
// rate and publishes the overstatement as the round's authoritative cost. An
// unconfigured nightly must therefore land on codex-sdk, which reports the split.
describe('DEFAULT_JUDGE_KIND — the transport an unconfigured run gets', () => {
  it('is codex-sdk, the transport that reports cached input tokens', () => {
    expect(DEFAULT_JUDGE_KIND).toBe('codex-sdk')
  })

  // The failure mode this catches is the repo's most common one: fixing one
  // instance of a pattern. The default is read at four sites (the residue runner,
  // the intent pass, selectJudge, the report CLI's image gate) and a literal left
  // behind at ANY of them silently keeps that lane on the old transport while every
  // suite stays green. Asserting over the source is the only way to see a site that
  // no test happens to exercise.
  it('is the ONLY thing any QA_JUDGE fallback resolves to — no site keeps a literal', () => {
    const root = path.resolve(__dirname, '..')
    const sources = fs
      .readdirSync(root, { recursive: true, encoding: 'utf8' })
      .filter(f => f.endsWith('.ts') && !f.endsWith('.test.ts') && !f.endsWith('.d.ts'))
    const offenders: string[] = []
    let fallbacks = 0
    for (const rel of sources) {
      const src = fs.readFileSync(path.join(root, rel), 'utf8')
      for (const m of src.matchAll(/process\.env\.QA_JUDGE\s*\?\?\s*([^,)\s]+)/g)) {
        fallbacks++
        if (m[1] !== 'DEFAULT_JUDGE_KIND') offenders.push(`${rel}: ${m[0]}`)
      }
    }
    expect(offenders).toEqual([])
    // A sweep that matches nothing would pass vacuously forever.
    expect(fallbacks).toBeGreaterThanOrEqual(4)
  })
})
