import * as fs from 'fs'
import * as os from 'os'
import * as path from 'path'
import { describe, it, expect } from 'vitest'
import { DEFAULT_JUDGE_EFFORT, DEFAULT_JUDGE_MODEL } from './codex-exec'
import {
  type JudgeTurn,
  makeCodexSdkJudge,
  sdkInput,
  threadOptionsFor,
  verdictsFromTurn,
} from './codex-sdk'
import { applyGrounding } from '../grader/grade'
import { parseSpanFile } from '../export/langfuse'
import type { GradedEvidenceEntry } from '../label-trace'
import type { Input, ThreadOptions } from '@openai/codex-sdk'
import type { JudgeInput, PerItemVerdict } from './types'

// A canned completed turn: a couple of lifecycle items then the final
// schema-constrained agent message in `finalResponse` (structured output).
function turn(finalMessageObj: unknown, opts: { withTool?: boolean } = {}): JudgeTurn {
  const items: Array<{ type: string }> = [{ type: 'reasoning' }]
  if (opts.withTool) items.push({ type: 'command_execution' })
  items.push({ type: 'agent_message' })
  return {
    items,
    finalResponse: JSON.stringify(finalMessageObj),
    usage: { input_tokens: 1200, output_tokens: 40 },
  }
}

const input: JudgeInput = {
  behaviorId: 'CON-042',
  behaviorTitle: 'delete confirmation',
  given: 'g',
  when: 'w',
  then: ['warns cannot be undone'],
  items: [{ itemIndex: 0, thenText: 'warns cannot be undone' }],
  evidence: { dialogs: [{ type: 'confirm', message: 'cannot be undone' }] },
}

describe('verdictsFromTurn', () => {
  it('returns parsed verdicts + usage for a clean run', () => {
    const { verdicts, rejectedForTool, inputTokens, outputTokens } = verdictsFromTurn(
      turn({ verdicts: [{ item_index: 0, verdict: 'fail', citation: 'dialog', critique: 'ok' }] })
    )
    expect(rejectedForTool).toBe(false)
    expect(verdicts).toEqual([
      { itemIndex: 0, verdict: 'fail', citation: 'dialog', critique: 'ok' },
    ])
    expect(inputTokens).toBe(1200)
    expect(outputTokens).toBe(40)
  })

  it('REJECTS a turn that used a tool / executed a command (dropped — no verdicts)', () => {
    const { verdicts, rejectedForTool } = verdictsFromTurn(
      turn({ verdicts: [] }, { withTool: true })
    )
    expect(rejectedForTool).toBe(true)
    expect(verdicts).toEqual([])
  })

  it('flags a web_search item as a tool use', () => {
    const { rejectedForTool } = verdictsFromTurn({
      items: [{ type: 'web_search' }, { type: 'agent_message' }],
      finalResponse: '{"verdicts":[]}',
      usage: null,
    })
    expect(rejectedForTool).toBe(true)
  })

  it('tolerates null usage', () => {
    const { inputTokens, outputTokens } = verdictsFromTurn({
      items: [{ type: 'agent_message' }],
      finalResponse: '{"verdicts":[]}',
      usage: null,
    })
    expect(inputTokens).toBeUndefined()
    expect(outputTokens).toBeUndefined()
  })
})

describe('sdkInput', () => {
  it('is a bare prompt string when there are no images', () => {
    expect(sdkInput('hello')).toBe('hello')
    expect(sdkInput('hello', [])).toBe('hello')
  })

  it('is a text entry + one local_image entry per image when images are attached', () => {
    expect(sdkInput('hello', ['/runs/a.png', '/runs/b.png'])).toEqual([
      { type: 'text', text: 'hello' },
      { type: 'local_image', path: '/runs/a.png' },
      { type: 'local_image', path: '/runs/b.png' },
    ])
  })
})

describe('threadOptionsFor', () => {
  it('always pins the sandbox read-only (the judge is criticism, not agency)', () => {
    expect(threadOptionsFor().sandboxMode).toBe('read-only')
    expect(threadOptionsFor('m', 'low').sandboxMode).toBe('read-only')
  })

  it('sets model + reasoning effort when provided', () => {
    expect(threadOptionsFor('gpt-5.5', 'medium')).toEqual({
      sandboxMode: 'read-only',
      model: 'gpt-5.5',
      modelReasoningEffort: 'medium',
    })
  })

  it('omits model + effort when unset (never inherits operator config)', () => {
    expect(threadOptionsFor()).toEqual({ sandboxMode: 'read-only' })
  })
})

describe('makeCodexSdkJudge (injected run — no live SDK call)', () => {
  it('returns per-item verdicts for a clean run', async () => {
    const judge = makeCodexSdkJudge({
      run: async () =>
        turn({
          verdicts: [{ item_index: 0, verdict: 'fail', citation: 'dialog', critique: 'warns' }],
        }),
    })
    expect(await judge(input)).toEqual([
      { itemIndex: 0, verdict: 'fail', citation: 'dialog', critique: 'warns' },
    ])
  })

  it('re-runs once on a tool-using run, then falls back to all-unsure', async () => {
    let calls = 0
    const judge = makeCodexSdkJudge({
      run: async () => {
        calls++
        return turn({ verdicts: [] }, { withTool: true })
      },
    })
    const verdicts = await judge(input)
    expect(calls).toBe(2) // one re-run
    expect(verdicts).toEqual([
      {
        itemIndex: 0,
        verdict: 'unsure',
        citation: '',
        critique: 'discarded: judge run used a tool',
      },
    ])
  })

  it('fills an omitted item with unsure', async () => {
    const judge = makeCodexSdkJudge({ run: async () => turn({ verdicts: [] }) })
    const verdicts = await judge(input)
    expect(verdicts[0].verdict).toBe('unsure')
  })

  it('all-unsure on a thrown run (never a fabricated fail)', async () => {
    const judge = makeCodexSdkJudge({
      run: async () => {
        throw new Error('boom')
      },
    })
    const [v] = await judge(input)
    expect(v.verdict).toBe('unsure')
    expect(v.critique).toContain('boom')
  })

  it('threads the configured model into the thread options (sandbox stays read-only)', async () => {
    let seen: ThreadOptions | undefined
    const judge = makeCodexSdkJudge({
      model: 'gpt-5-codex-high',
      run: async (_i, threadOptions) => {
        seen = threadOptions
        return turn({
          verdicts: [{ item_index: 0, verdict: 'pass', citation: 'c', critique: 'k' }],
        })
      },
    })
    await judge(input)
    expect(seen?.model).toBe('gpt-5-codex-high')
    expect(seen?.sandboxMode).toBe('read-only')
  })

  it('falls back to QA_JUDGE_MODEL for the thread model when no model is passed', async () => {
    const prev = process.env.QA_JUDGE_MODEL
    process.env.QA_JUDGE_MODEL = 'gpt-5-codex-env'
    try {
      let seen: ThreadOptions | undefined
      const judge = makeCodexSdkJudge({
        run: async (_i, threadOptions) => {
          seen = threadOptions
          return turn({
            verdicts: [{ item_index: 0, verdict: 'pass', citation: 'c', critique: 'k' }],
          })
        },
      })
      await judge(input)
      expect(seen?.model).toBe('gpt-5-codex-env')
    } finally {
      if (prev === undefined) delete process.env.QA_JUDGE_MODEL
      else process.env.QA_JUDGE_MODEL = prev
    }
  })

  it('defaults to the pinned cheap model + low effort (never inherits operator config)', async () => {
    const prevModel = process.env.QA_JUDGE_MODEL
    const prevEffort = process.env.QA_JUDGE_EFFORT
    delete process.env.QA_JUDGE_MODEL
    delete process.env.QA_JUDGE_EFFORT
    try {
      let seen: ThreadOptions | undefined
      const judge = makeCodexSdkJudge({
        run: async (_i, threadOptions) => {
          seen = threadOptions
          return turn({
            verdicts: [{ item_index: 0, verdict: 'pass', citation: 'c', critique: 'k' }],
          })
        },
      })
      await judge(input)
      expect(seen?.model).toBe(DEFAULT_JUDGE_MODEL)
      expect(seen?.modelReasoningEffort).toBe(DEFAULT_JUDGE_EFFORT)
    } finally {
      if (prevModel === undefined) delete process.env.QA_JUDGE_MODEL
      else process.env.QA_JUDGE_MODEL = prevModel
      if (prevEffort === undefined) delete process.env.QA_JUDGE_EFFORT
      else process.env.QA_JUDGE_EFFORT = prevEffort
    }
  })

  it('threads opts.effort into the reasoning-effort thread option', async () => {
    let seen: ThreadOptions | undefined
    const judge = makeCodexSdkJudge({
      effort: 'high',
      run: async (_i, threadOptions) => {
        seen = threadOptions
        return turn({
          verdicts: [{ item_index: 0, verdict: 'pass', citation: 'c', critique: 'k' }],
        })
      },
    })
    await judge(input)
    expect(seen?.modelReasoningEffort).toBe('high')
  })

  it('attaches images as local_image entries in the turn input', async () => {
    let seen: Input | undefined
    const judge = makeCodexSdkJudge({
      run: async i => {
        seen = i
        return turn({
          verdicts: [{ item_index: 0, verdict: 'pass', citation: 'c', critique: 'k' }],
        })
      },
    })
    await judge({ ...input, images: ['/runs/001.png'] })
    expect(Array.isArray(seen)).toBe(true)
    expect(seen).toContainEqual({ type: 'local_image', path: '/runs/001.png' })
  })

  it('grounding rule: an uncited judge fail downgrades to unsure', async () => {
    const judge = makeCodexSdkJudge({
      run: async () =>
        turn({
          verdicts: [{ item_index: 0, verdict: 'fail', citation: '', critique: 'no citation' }],
        }),
    })
    const [v] = await judge(input)
    // The adapter returns the raw fail; the grader's grounding rule downgrades it.
    expect(v.verdict).toBe('fail')
    expect(applyGrounding(v).verdict).toBe('unsure')
  })
})

// The SDK side of the both-adapters label-trace test: the appended span must
// carry the label-trace content AND its qa.item_verdicts must EQUAL the
// NORMALIZED array the adapter RETURNS — proving the span is built AFTER
// normalization, not from the pre-normalization parse. Identical contract to the
// exec adapter (impl name aside).
describe('makeCodexSdkJudge — span carries label-trace content + normalized verdicts', () => {
  const contentInput: JudgeInput = {
    behaviorId: 'CON-042',
    behaviorTitle: 'delete confirmation',
    given: 'a contact exists',
    when: 'the user deletes it',
    then: ['warns cannot be undone', 'closes', 'removes the row'],
    items: [
      { itemIndex: 0, thenText: 'warns cannot be undone' },
      { itemIndex: 2, thenText: 'removes the row' },
    ],
    evidence: {},
    captureSections: [
      { captureFile: '001.json', note: 'dialog', evidence: { url: 'http://x/1' } },
      { captureFile: '002.json', note: 'gone', evidence: { url: 'http://x/2' } },
    ],
    images: ['/runs/001.png', '/runs/002.png'],
  }

  async function spanFor(
    run: (input: Input, threadOptions: ThreadOptions, outputSchema: unknown) => Promise<JudgeTurn>
  ): Promise<{ span: ReturnType<typeof parseSpanFile>[number]; verdicts: PerItemVerdict[] }> {
    const tracePath = path.join(
      os.tmpdir(),
      `qa-sdk-span-${process.pid}-${Date.now()}-${Math.random().toString(36).slice(2)}.jsonl`
    )
    try {
      const verdicts = await makeCodexSdkJudge({ run, tracePath })(contentInput)
      const [span] = parseSpanFile(fs.readFileSync(tracePath, 'utf8'))
      return { span, verdicts }
    } finally {
      fs.rmSync(tracePath, { force: true })
    }
  }

  it('records impl codex-sdk + scenario + graded_evidence + prompt + completion + per-capture screenshots', async () => {
    const { span } = await spanFor(async () =>
      turn({
        verdicts: [
          { item_index: 0, verdict: 'fail', citation: 'dialog', critique: 'k' },
          { item_index: 2, verdict: 'pass', citation: 'row', critique: 'k' },
        ],
      })
    )
    expect(span.attributes['qa.judge.impl']).toBe('codex-sdk')
    expect((span.attributes['qa.scenario'] as { kind: string }).kind).toBe('behavior')
    const graded = span.attributes['qa.graded_evidence'] as GradedEvidenceEntry[]
    expect(graded.map(g => g.captureFile)).toEqual(['001.json', '002.json'])
    expect(graded.map(g => g.screenshot)).toEqual(['/runs/001.png', '/runs/002.png'])
    expect(span.attributes['qa.screenshots']).toEqual(['/runs/001.png', '/runs/002.png'])
    expect(typeof span.attributes['gen_ai.prompt']).toBe('string')
    expect(typeof span.attributes['gen_ai.completion']).toBe('string')
    expect(span.attributes['gen_ai.usage.input_tokens']).toBe(1200)
  })

  it('(a) success: item_verdicts equals the returned verdicts', async () => {
    const { span, verdicts } = await spanFor(async () =>
      turn({
        verdicts: [
          { item_index: 0, verdict: 'fail', citation: 'dialog', critique: 'k' },
          { item_index: 2, verdict: 'pass', citation: 'row', critique: 'k' },
        ],
      })
    )
    expect(span.attributes['qa.item_verdicts']).toEqual(verdicts)
    expect(verdicts.map(v => v.verdict)).toEqual(['fail', 'pass'])
  })

  it('(b) omitted item: span carries the FILLED (unsure) verdict, matching the return', async () => {
    const { span, verdicts } = await spanFor(async () =>
      turn({ verdicts: [{ item_index: 0, verdict: 'fail', citation: 'dialog', critique: 'k' }] })
    )
    expect(span.attributes['qa.item_verdicts']).toEqual(verdicts)
    expect(verdicts.find(v => v.itemIndex === 2)?.verdict).toBe('unsure')
  })

  it('(c) tool-rejection: span carries allUnsure, matching the return', async () => {
    const { span, verdicts } = await spanFor(async () => turn({ verdicts: [] }, { withTool: true }))
    expect(span.attributes['qa.item_verdicts']).toEqual(verdicts)
    expect(span.attributes['qa.tool_rejected']).toBe(true)
    expect(verdicts.every(v => v.verdict === 'unsure')).toBe(true)
  })

  it('(d) thrown run: span carries allUnsure, matching the return', async () => {
    const { span, verdicts } = await spanFor(async () => {
      throw new Error('boom')
    })
    expect(span.attributes['qa.item_verdicts']).toEqual(verdicts)
    expect(span.status.code).toBe('ERROR')
    expect(verdicts.every(v => v.verdict === 'unsure')).toBe(true)
  })
})
