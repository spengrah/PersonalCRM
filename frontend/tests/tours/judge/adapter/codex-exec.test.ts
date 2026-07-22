import * as fs from 'fs'
import * as os from 'os'
import * as path from 'path'
import { describe, it, expect } from 'vitest'
import { DEFAULT_JUDGE_EFFORT, DEFAULT_JUDGE_MODEL } from '../models'
import {
  codexArgs,
  makeCodexExecJudge,
  parseCodexEventStream,
  verdictsFromCodexOutput,
} from './codex-exec'
import { applyGrounding } from '../grader/grade'
import { parseSpanFile } from '../export/langfuse'
import type { GradedEvidenceEntry } from '../label-trace'
import type { JudgeInput, PerItemVerdict } from './types'

// A canned codex `--json` event stream: a couple of lifecycle events then the
// final schema-constrained agent message.
function stream(finalMessageObj: unknown, opts: { withTool?: boolean } = {}): string {
  const lines: string[] = [
    JSON.stringify({ type: 'session.created', session_id: 'x' }),
    JSON.stringify({ type: 'thread.started' }),
  ]
  if (opts.withTool) {
    lines.push(
      JSON.stringify({ type: 'item.completed', item: { type: 'command_execution', command: 'ls' } })
    )
  }
  lines.push(
    JSON.stringify({
      type: 'item.completed',
      item: { type: 'agent_message', text: JSON.stringify(finalMessageObj) },
      msg: { model: 'gpt-5-codex', usage: { input_tokens: 1200, output_tokens: 40 } },
    })
  )
  return lines.join('\n')
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

describe('parseCodexEventStream', () => {
  it('extracts the final agent message + model + usage', () => {
    const out = parseCodexEventStream(
      stream({ verdicts: [{ item_index: 0, verdict: 'pass', citation: 'c', critique: 'k' }] })
    )
    expect(out.usedTool).toBe(false)
    expect(out.model).toBe('gpt-5-codex')
    expect(out.inputTokens).toBe(1200)
    expect(out.outputTokens).toBe(40)
    expect(out.message).toContain('verdicts')
  })

  it('flags a run that used a tool / executed a command', () => {
    const out = parseCodexEventStream(stream({ verdicts: [] }, { withTool: true }))
    expect(out.usedTool).toBe(true)
  })
})

describe('verdictsFromCodexOutput', () => {
  it('returns parsed verdicts for a clean run', () => {
    const { verdicts, rejectedForTool } = verdictsFromCodexOutput(
      stream({ verdicts: [{ item_index: 0, verdict: 'fail', citation: 'dialog', critique: 'ok' }] })
    )
    expect(rejectedForTool).toBe(false)
    expect(verdicts).toEqual([
      { itemIndex: 0, verdict: 'fail', citation: 'dialog', critique: 'ok' },
    ])
  })

  it('REJECTS a tool-using run (dropped — no verdicts)', () => {
    const { verdicts, rejectedForTool } = verdictsFromCodexOutput(
      stream({ verdicts: [] }, { withTool: true })
    )
    expect(rejectedForTool).toBe(true)
    expect(verdicts).toEqual([])
  })
})

describe('codexArgs', () => {
  it('spawns read-only with the output schema and --json (no --model when unset)', () => {
    expect(codexArgs('/tmp/s.json')).toEqual([
      'exec',
      '--json',
      '--output-schema',
      '/tmp/s.json',
      '--sandbox',
      'read-only',
      '-',
    ])
  })

  it('passes --model when a model is set', () => {
    expect(codexArgs('/tmp/s.json', 'gpt-5-codex-high')).toEqual([
      'exec',
      '--json',
      '--output-schema',
      '/tmp/s.json',
      '--sandbox',
      'read-only',
      '--model',
      'gpt-5-codex-high',
      '-',
    ])
  })

  it('pins reasoning effort via -c when set', () => {
    expect(codexArgs('/tmp/s.json', 'm', 'low')).toEqual([
      'exec',
      '--json',
      '--output-schema',
      '/tmp/s.json',
      '--sandbox',
      'read-only',
      '--model',
      'm',
      '-c',
      'model_reasoning_effort=low',
      '-',
    ])
  })
})

describe('makeCodexExecJudge (injected run — no live call)', () => {
  it('returns per-item verdicts for a clean run', async () => {
    const judge = makeCodexExecJudge({
      run: async () =>
        stream({
          verdicts: [{ item_index: 0, verdict: 'fail', citation: 'dialog', critique: 'warns' }],
        }),
    })
    const verdicts = await judge(input)
    expect(verdicts).toEqual([
      { itemIndex: 0, verdict: 'fail', citation: 'dialog', critique: 'warns' },
    ])
  })

  it('re-runs once on a tool-using run, then falls back to all-unsure', async () => {
    let calls = 0
    const judge = makeCodexExecJudge({
      run: async () => {
        calls++
        return stream({ verdicts: [] }, { withTool: true })
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
    const judge = makeCodexExecJudge({ run: async () => stream({ verdicts: [] }) })
    const verdicts = await judge(input)
    expect(verdicts[0].verdict).toBe('unsure')
  })

  it('threads the configured model into the codex argv', async () => {
    let seenArgs: string[] = []
    const judge = makeCodexExecJudge({
      model: 'gpt-5-codex-high',
      run: async args => {
        seenArgs = args
        return stream({
          verdicts: [{ item_index: 0, verdict: 'pass', citation: 'c', critique: 'k' }],
        })
      },
    })
    await judge(input)
    expect(seenArgs).toContain('--model')
    expect(seenArgs[seenArgs.indexOf('--model') + 1]).toBe('gpt-5-codex-high')
  })

  // The judge path also reads QA_JUDGE_MODEL as the default model when no
  // explicit model is passed. (An explicit stronger model — e.g. the intent
  // pass's QA_INTENT_MODEL → selectJudge(kind, model) → makeCodexExecJudge({
  // model }) — is covered by composition: adapter/index.test.ts proves
  // selectJudge threads its model arg into the adapter, and the explicit-model
  // test above proves the adapter threads it into the codex argv.)
  it('falls back to QA_JUDGE_MODEL for the codex --model when no model is passed', async () => {
    const prev = process.env.QA_JUDGE_MODEL
    process.env.QA_JUDGE_MODEL = 'gpt-5-codex-env'
    try {
      let seenArgs: string[] = []
      const judge = makeCodexExecJudge({
        run: async args => {
          seenArgs = args
          return stream({
            verdicts: [{ item_index: 0, verdict: 'pass', citation: 'c', critique: 'k' }],
          })
        },
      })
      await judge(input)
      expect(seenArgs).toContain('--model')
      expect(seenArgs[seenArgs.indexOf('--model') + 1]).toBe('gpt-5-codex-env')
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
      let seenArgs: string[] = []
      const judge = makeCodexExecJudge({
        run: async args => {
          seenArgs = args
          return stream({
            verdicts: [{ item_index: 0, verdict: 'pass', citation: 'c', critique: 'k' }],
          })
        },
      })
      await judge(input)
      // The bug this guards: with nothing pinned the judge must use the cheap
      // default, NOT silently inherit the operator's codex config (gpt-5.5/xhigh).
      expect(seenArgs[seenArgs.indexOf('--model') + 1]).toBe(DEFAULT_JUDGE_MODEL)
      expect(seenArgs).toContain(`model_reasoning_effort=${DEFAULT_JUDGE_EFFORT}`)
    } finally {
      if (prevModel === undefined) delete process.env.QA_JUDGE_MODEL
      else process.env.QA_JUDGE_MODEL = prevModel
      if (prevEffort === undefined) delete process.env.QA_JUDGE_EFFORT
      else process.env.QA_JUDGE_EFFORT = prevEffort
    }
  })

  it('threads opts.effort into the codex reasoning-effort arg', async () => {
    let seenArgs: string[] = []
    const judge = makeCodexExecJudge({
      effort: 'high',
      run: async args => {
        seenArgs = args
        return stream({
          verdicts: [{ item_index: 0, verdict: 'pass', citation: 'c', critique: 'k' }],
        })
      },
    })
    await judge(input)
    expect(seenArgs).toContain('model_reasoning_effort=high')
  })

  it('falls back to QA_JUDGE_EFFORT when no effort is passed', async () => {
    const prev = process.env.QA_JUDGE_EFFORT
    process.env.QA_JUDGE_EFFORT = 'medium'
    try {
      let seenArgs: string[] = []
      const judge = makeCodexExecJudge({
        run: async args => {
          seenArgs = args
          return stream({
            verdicts: [{ item_index: 0, verdict: 'pass', citation: 'c', critique: 'k' }],
          })
        },
      })
      await judge(input)
      expect(seenArgs).toContain('model_reasoning_effort=medium')
    } finally {
      if (prev === undefined) delete process.env.QA_JUDGE_EFFORT
      else process.env.QA_JUDGE_EFFORT = prev
    }
  })

  it('grounding rule: an uncited judge fail downgrades to unsure', async () => {
    const judge = makeCodexExecJudge({
      run: async () =>
        stream({
          verdicts: [{ item_index: 0, verdict: 'fail', citation: '', critique: 'no citation' }],
        }),
    })
    const [v] = await judge(input)
    // The adapter returns the raw fail; the grader's grounding rule downgrades it.
    expect(v.verdict).toBe('fail')
    expect(applyGrounding(v).verdict).toBe('unsure')
  })
})

// The codex side of the both-adapters test: the appended span must carry the
// label-trace content (scenario + graded evidence + prompt + completion) AND its
// qa.item_verdicts must EQUAL the NORMALIZED array the adapter RETURNS — proving
// the span is built AFTER normalization, not from the pre-normalization parse.
describe('makeCodexExecJudge — span carries label-trace content + normalized verdicts', () => {
  // Two graded items so an omitted-item fill is observable; two sections + two
  // images so codex attaches per-capture screenshots.
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
    run: (args: string[], prompt: string) => Promise<string>
  ): Promise<{ span: ReturnType<typeof parseSpanFile>[number]; verdicts: PerItemVerdict[] }> {
    const tracePath = path.join(
      os.tmpdir(),
      `qa-span-${process.pid}-${Date.now()}-${Math.random().toString(36).slice(2)}.jsonl`
    )
    try {
      const verdicts = await makeCodexExecJudge({ run, tracePath })(contentInput)
      const [span] = parseSpanFile(fs.readFileSync(tracePath, 'utf8'))
      return { span, verdicts }
    } finally {
      fs.rmSync(tracePath, { force: true })
    }
  }

  it('carries scenario + graded_evidence + prompt + completion, with per-capture screenshots', async () => {
    const { span } = await spanFor(async () =>
      stream({
        verdicts: [
          { item_index: 0, verdict: 'fail', citation: 'dialog', critique: 'k' },
          { item_index: 2, verdict: 'pass', citation: 'row', critique: 'k' },
        ],
      })
    )
    expect((span.attributes['qa.scenario'] as { kind: string }).kind).toBe('behavior')
    const graded = span.attributes['qa.graded_evidence'] as GradedEvidenceEntry[]
    expect(graded.map(g => g.captureFile)).toEqual(['001.json', '002.json'])
    // codex-exec CAN attach images → each entry carries its own screenshot path.
    expect(graded.map(g => g.screenshot)).toEqual(['/runs/001.png', '/runs/002.png'])
    expect(span.attributes['qa.screenshots']).toEqual(['/runs/001.png', '/runs/002.png'])
    expect(typeof span.attributes['gen_ai.prompt']).toBe('string')
    expect(typeof span.attributes['gen_ai.completion']).toBe('string')
  })

  // qa.item_verdicts EQUALS the returned array across all four normalization paths.
  it('(a) success: item_verdicts equals the returned verdicts', async () => {
    const { span, verdicts } = await spanFor(async () =>
      stream({
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
      stream({ verdicts: [{ item_index: 0, verdict: 'fail', citation: 'dialog', critique: 'k' }] })
    )
    expect(span.attributes['qa.item_verdicts']).toEqual(verdicts)
    // item 2 was omitted by the model → filled unsure, NOT the pre-norm parse.
    expect(verdicts.find(v => v.itemIndex === 2)?.verdict).toBe('unsure')
  })

  it('(c) tool-rejection: span carries allUnsure, matching the return', async () => {
    const { span, verdicts } = await spanFor(async () =>
      stream({ verdicts: [] }, { withTool: true })
    )
    expect(span.attributes['qa.item_verdicts']).toEqual(verdicts)
    expect(verdicts.every(v => v.verdict === 'unsure')).toBe(true)
  })

  it('(d) thrown run: span carries allUnsure, matching the return', async () => {
    const { span, verdicts } = await spanFor(async () => {
      throw new Error('boom')
    })
    expect(span.attributes['qa.item_verdicts']).toEqual(verdicts)
    expect(verdicts.every(v => v.verdict === 'unsure')).toBe(true)
  })
})
