// makeIntentJudge's adapter dispatch: the codex-oriented DEFAULT_INTENT_MODEL
// must never leak onto non-codex adapters (their model config wins unless
// QA_INTENT_MODEL is explicitly set). Proven observably via the http adapter's
// posted body, mirroring adapter/index.test.ts's fetch-stub pattern.

import { afterEach, describe, expect, it, vi } from 'vitest'
import type { JudgeInput } from './adapter'
import { codexArgs } from './adapter/codex-exec'
import { makeIntentJudge } from './intent-runner'

const INPUT: JudgeInput = {
  behaviorId: 'DSH-010',
  behaviorTitle: 't',
  given: '',
  when: '',
  then: ['s'],
  items: [{ itemIndex: 0, thenText: 's' }],
  evidence: {},
  intent: { statement: 's', status: 'current' },
  captureSections: [],
}

function stubFetch(): { posted: () => { model?: string } } {
  let body: { model?: string } = {}
  vi.stubGlobal(
    'fetch',
    vi.fn(async (_url: unknown, init?: { body?: string }) => {
      body = JSON.parse(init?.body ?? '{}') as { model?: string }
      return {
        ok: true,
        json: async () => ({
          choices: [{ message: { content: '{"verdicts":[]}' } }],
        }),
      }
    })
  )
  return { posted: () => body }
}

describe('codexArgs image attachment', () => {
  it('appends -i per image before the stdin marker', () => {
    const args = codexArgs('/tmp/schema.json', 'gpt-5.5', 'medium', ['/runs/a.png', '/runs/b.png'])
    expect(args.join(' ')).toContain('-i /runs/a.png -i /runs/b.png -')
    expect(args[args.length - 1]).toBe('-')
  })

  it('adds no -i flags without images', () => {
    expect(codexArgs('/tmp/schema.json')).not.toContain('-i')
  })
})

describe('makeIntentJudge adapter dispatch', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    vi.unstubAllEnvs()
  })

  it('http adapter keeps its own model when QA_INTENT_MODEL is unset', async () => {
    vi.stubEnv('QA_JUDGE_HTTP_URL', 'http://judge.local/v1/chat/completions')
    vi.stubEnv('QA_JUDGE_HTTP_MODEL', 'venice-large')
    vi.stubEnv('QA_INTENT_MODEL', '')
    delete process.env.QA_INTENT_MODEL
    const fetchStub = stubFetch()
    await makeIntentJudge('http')(INPUT)
    expect(fetchStub.posted().model).toBe('venice-large')
  })

  it('an explicit QA_INTENT_MODEL overrides the http adapter model', async () => {
    vi.stubEnv('QA_JUDGE_HTTP_URL', 'http://judge.local/v1/chat/completions')
    vi.stubEnv('QA_JUDGE_HTTP_MODEL', 'venice-large')
    vi.stubEnv('QA_INTENT_MODEL', 'venice-xl')
    const fetchStub = stubFetch()
    await makeIntentJudge('http')(INPUT)
    expect(fetchStub.posted().model).toBe('venice-xl')
  })
})
