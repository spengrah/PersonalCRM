// makeIntentJudge's adapter dispatch: the codex-oriented DEFAULT_INTENT_MODEL
// must never leak onto non-codex adapters (their model config wins unless
// QA_INTENT_MODEL is explicitly set). Proven observably via the http adapter's
// posted body, mirroring adapter/index.test.ts's fetch-stub pattern.

import { afterEach, describe, expect, it, vi } from 'vitest'
import type { JudgeInput } from './adapter'
import { codexArgs } from './adapter/codex-exec'
import { makeIntentJudge } from './intent-runner'
import { DEFAULT_INTENT_EFFORT, DEFAULT_INTENT_MODEL } from './models'

// Spy on the codex-sdk factory so the codex-sdk dispatch is observable without a
// live SDK call. Hoisted so the vi.mock factory can reference it; only the
// codex-sdk module is replaced (the http/codexArgs tests below use the real code).
const { sdkSpy } = vi.hoisted(() => ({ sdkSpy: vi.fn(() => async () => []) }))
vi.mock('./adapter/codex-sdk', () => ({ makeCodexSdkJudge: sdkSpy }))

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

interface PostedBody {
  model?: string
  messages?: Array<{ content?: string }>
}

function stubFetch(): { posted: () => PostedBody } {
  let body: PostedBody = {}
  vi.stubGlobal(
    'fetch',
    vi.fn(async (_url: unknown, init?: { body?: string }) => {
      body = JSON.parse(init?.body ?? '{}') as PostedBody
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

  it('http keeps the aria-only visual framing even when images were resolved', async () => {
    vi.stubEnv('QA_JUDGE_HTTP_URL', 'http://judge.local/v1/chat/completions')
    vi.stubEnv('QA_INTENT_MODEL', '')
    delete process.env.QA_INTENT_MODEL
    const fetchStub = stubFetch()
    await makeIntentJudge('http')({ ...INPUT, images: ['/runs/x/1.png'] })
    const prompt = fetchStub.posted().messages?.[0]?.content ?? ''
    expect(prompt).toMatch(/do not fail a goal for purely visual\s+qualities/)
    expect(prompt).not.toMatch(/Screenshots of the captured states are attached/)
  })

  // Unlike the http stub, codex-sdk IS a codex adapter driving the same engine, so
  // the stronger intent model + effort DO apply to it (parity with codex-exec).
  it('codex-sdk gets the stronger intent model + effort by default', () => {
    delete process.env.QA_INTENT_MODEL
    delete process.env.QA_INTENT_EFFORT
    sdkSpy.mockClear()
    makeIntentJudge('codex-sdk')
    expect(sdkSpy).toHaveBeenCalledWith({
      model: DEFAULT_INTENT_MODEL,
      effort: DEFAULT_INTENT_EFFORT,
    })
  })

  it('an explicit QA_INTENT_MODEL overrides the codex-sdk intent model', () => {
    vi.stubEnv('QA_INTENT_MODEL', 'gpt-5.6-terra')
    sdkSpy.mockClear()
    makeIntentJudge('codex-sdk')
    expect(sdkSpy).toHaveBeenCalledWith(expect.objectContaining({ model: 'gpt-5.6-terra' }))
  })

  it('an UNCONFIGURED run dispatches to codex-sdk — the transport that reports usage in full', () => {
    // An unconfigured nightly is the common case, and codex-exec reports no
    // cached-input count (its input_tokens is inclusive of cache reads), so
    // defaulting there prices every cached token at the full input rate and
    // publishes the overstatement as the round's cost.
    delete process.env.QA_JUDGE
    sdkSpy.mockClear()
    makeIntentJudge()
    expect(sdkSpy).toHaveBeenCalledOnce()
  })
})
