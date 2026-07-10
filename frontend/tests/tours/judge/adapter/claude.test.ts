import { describe, it, expect, afterEach } from 'vitest'
import {
  claudeArgs,
  makeClaudeJudge,
  parseClaudeResult,
  CLAUDE_JUDGE_SYSTEM_PROMPT,
  DEFAULT_CLAUDE_MODEL,
} from './claude'
import { buildPrompt } from './prompt'
import type { JudgeInput } from './types'

const input: JudgeInput = {
  behaviorId: 'CON-042',
  behaviorTitle: 'Deleting a contact requires explicit confirmation',
  given: 'a contact detail page',
  when: 'the user clicks Delete',
  then: ['a confirmation warns the action cannot be undone'],
  items: [{ itemIndex: 0, thenText: 'a confirmation warns the action cannot be undone' }],
  evidence: { url: '/contacts/<uuid:1>' },
}

function resultJson(overrides: Record<string, unknown> = {}): string {
  return JSON.stringify({
    type: 'result',
    subtype: 'success',
    is_error: false,
    num_turns: 1,
    result:
      '{"verdicts":[{"item_index":0,"verdict":"pass","citation":"DIALOGS[0]","critique":"warned"}]}',
    usage: { input_tokens: 12, output_tokens: 7 },
    ...overrides,
  })
}

describe('parseClaudeResult (pure)', () => {
  it('extracts message, turns, and usage from a success result', () => {
    const r = parseClaudeResult(resultJson())
    expect(r.message).toContain('"verdicts"')
    expect(r.isError).toBe(false)
    expect(r.numTurns).toBe(1)
    expect(r.inputTokens).toBe(12)
    expect(r.outputTokens).toBe(7)
  })

  it('flags non-success subtypes and is_error results', () => {
    expect(parseClaudeResult(resultJson({ subtype: 'error_max_turns' })).isError).toBe(true)
    expect(parseClaudeResult(resultJson({ is_error: true })).isError).toBe(true)
  })

  it('treats unparseable stdout as an error result', () => {
    const r = parseClaudeResult('not json at all')
    expect(r.isError).toBe(true)
    expect(r.message).toBeUndefined()
  })
})

describe('claudeArgs', () => {
  it('pins print mode, json output, and the judge system prompt', () => {
    const args = claudeArgs('m1')
    expect(args).toEqual([
      '-p',
      '--output-format',
      'json',
      '--system-prompt',
      CLAUDE_JUDGE_SYSTEM_PROMPT,
      '--model',
      'm1',
    ])
  })
})

describe('makeClaudeJudge (injected run — offline)', () => {
  const origModel = process.env.QA_CLAUDE_MODEL
  afterEach(() => {
    if (origModel === undefined) delete process.env.QA_CLAUDE_MODEL
    else process.env.QA_CLAUDE_MODEL = origModel
  })

  it('parses verdicts from the result and passes the default model', async () => {
    const seen: string[][] = []
    const judge = makeClaudeJudge({
      run: async args => {
        seen.push(args)
        return resultJson()
      },
    })
    const verdicts = await judge(input)
    expect(verdicts).toEqual([
      { itemIndex: 0, verdict: 'pass', citation: 'DIALOGS[0]', critique: 'warned' },
    ])
    expect(seen[0]).toContain(DEFAULT_CLAUDE_MODEL)
  })

  it('threads an explicit model over the QA_CLAUDE_MODEL env', async () => {
    process.env.QA_CLAUDE_MODEL = 'env-model'
    const seen: string[][] = []
    const judge = makeClaudeJudge({
      model: 'stronger-labeler-model',
      run: async args => {
        seen.push(args)
        return resultJson()
      },
    })
    await judge(input)
    expect(seen[0]).toContain('stronger-labeler-model')
    expect(seen[0]).not.toContain('env-model')
  })

  it('strips images so the prompt keeps the aria-only visual framing', async () => {
    const prompts: string[] = []
    const judge = makeClaudeJudge({
      run: async (_args, prompt) => {
        prompts.push(prompt)
        return resultJson()
      },
    })
    await judge({ ...input, images: ['/run/screens/001.png'] })
    expect(prompts[0]).toBe(buildPrompt({ ...input, images: undefined }))
  })

  it('re-runs ONCE on a tool-using run, then accepts a pure run', async () => {
    let calls = 0
    const judge = makeClaudeJudge({
      run: async () => {
        calls += 1
        return calls === 1 ? resultJson({ num_turns: 3 }) : resultJson()
      },
    })
    const verdicts = await judge(input)
    expect(calls).toBe(2)
    expect(verdicts[0].verdict).toBe('pass')
  })

  it('yields all-unsure after two tool-using runs (never a fabricated fail)', async () => {
    const judge = makeClaudeJudge({ run: async () => resultJson({ num_turns: 2 }) })
    const verdicts = await judge(input)
    expect(verdicts).toEqual([
      {
        itemIndex: 0,
        verdict: 'unsure',
        citation: '',
        critique: 'discarded: judge run used a tool',
      },
    ])
  })

  it('yields all-unsure on an error result', async () => {
    const judge = makeClaudeJudge({ run: async () => resultJson({ is_error: true }) })
    const verdicts = await judge(input)
    expect(verdicts[0].verdict).toBe('unsure')
    expect(verdicts[0].critique).toMatch(/error result/)
  })

  it('yields all-unsure when the spawn rejects', async () => {
    const judge = makeClaudeJudge({
      run: async () => {
        throw new Error('claude -p exited 1: no credit')
      },
    })
    const verdicts = await judge(input)
    expect(verdicts[0].verdict).toBe('unsure')
    expect(verdicts[0].critique).toMatch(/judge error: claude -p exited 1/)
  })

  it('fills items the model omitted with unsure', async () => {
    const judge = makeClaudeJudge({
      run: async () => resultJson({ result: '{"verdicts":[]}' }),
    })
    const verdicts = await judge(input)
    expect(verdicts[0].verdict).toBe('unsure')
    expect(verdicts[0].critique).toBe('no verdict returned')
  })
})
