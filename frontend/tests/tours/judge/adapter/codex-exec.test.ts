import { describe, it, expect } from 'vitest'
import {
  codexArgs,
  makeCodexExecJudge,
  parseCodexEventStream,
  verdictsFromCodexOutput,
} from './codex-exec'
import { applyGrounding } from '../grader/grade'
import type { JudgeInput } from './types'

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
