// The `@openai/codex-sdk` implementation of the Judge interface — a like-for-like
// TRANSPORT SWAP of the codex-exec adapter (design D2). It drives the SAME Codex
// engine through the programmatic SDK instead of spawning the `codex` CLI:
// single-shot judgment, evidence-in / verdict-out, ZERO grader change. It is NOT
// an agentic re-architecture — no multi-turn loop, no browser actuation; the
// judge still only reads the captured evidence in the prompt.
//
// Every pure piece is shared with the exec path (buildPrompt / OUTPUT_SCHEMA /
// parseVerdicts / judgeFailureVerdicts / the tool-marker detection);
// only the transport differs. The turn-parsing logic is pure; the live SDK call
// is a thin wrapper, exactly like exec's spawn.

import { DEFAULT_JUDGE_EFFORT, DEFAULT_JUDGE_MODEL } from '../models'
import { eventUsedTool } from './codex-exec'
import { buildPrompt, OUTPUT_SCHEMA, parseVerdicts } from './prompt'
import { judgeFailureVerdicts } from './types'
import type { Judge, JudgeInput, PerItemVerdict } from './types'
import type { Input, ModelReasoningEffort, ThreadOptions } from '@openai/codex-sdk'

// The minimal shape the adapter consumes from a completed turn. This is the
// injectable test seam (mirroring exec's `run: () => Promise<string>`): the real
// SDK `Turn` is mapped onto it in defaultRun, and tests supply canned values
// directly without loading the SDK runtime. Items expose only `type` — the same
// TOOL_EVENT_MARKERS logic the exec parser uses classifies a tool/command item.
export interface JudgeTurn {
  items: ReadonlyArray<{ type?: string }>
  finalResponse: string
}

// From a completed turn → verdicts (or a tool-rejection signal). Pure — the
// testable seam, the SDK analog of exec's `verdictsFromCodexOutput`. A run that
// used any tool/command is NOT a pure-criticism verdict (D4) and is discarded.
export function verdictsFromTurn(turn: JudgeTurn): {
  verdicts: PerItemVerdict[]
  rejectedForTool: boolean
} {
  const usedTool = turn.items.some(it => eventUsedTool({ type: it.type }))
  if (usedTool) return { verdicts: [], rejectedForTool: true }
  const verdicts = turn.finalResponse ? parseVerdicts(turn.finalResponse) : []
  return { verdicts, rejectedForTool: false }
}

// Build the SDK turn input: a bare prompt string when there are no images, or a
// text entry followed by one `local_image` entry per screenshot (the SDK analog
// of exec's `-i <file>` per image). Pure. The intent pass attaches capture
// screenshots in CAPTURE[n] order.
export function sdkInput(prompt: string, images?: string[]): Input {
  if (!images || images.length === 0) return prompt
  return [
    { type: 'text', text: prompt },
    ...images.map(path => ({ type: 'local_image' as const, path })),
  ]
}

// Build the thread options — the SDK analog of exec's `codexArgs`. The sandbox is
// ALWAYS pinned read-only (D2/D4: the judge is criticism, not agency). Model and
// reasoning-effort are set only when provided, so the judge never silently
// inherits the operator's global codex config. Pure.
export function threadOptionsFor(model?: string, effort?: string): ThreadOptions {
  const opts: ThreadOptions = { sandboxMode: 'read-only' }
  if (model) opts.model = model
  if (effort) opts.modelReasoningEffort = effort as ModelReasoningEffort
  return opts
}

export interface CodexSdkOptions {
  model?: string
  effort?: string
  // Injected for tests: run one turn and return the completed turn (defaults to a
  // real SDK thread run). Receives the fully-built thread options so tests can
  // observe model/effort/sandbox threading, exactly as the exec seam observes argv.
  run?: (input: Input, threadOptions: ThreadOptions, outputSchema: unknown) => Promise<JudgeTurn>
  timeoutMs?: number
}

function defaultRun(
  timeoutMs: number
): (input: Input, threadOptions: ThreadOptions, outputSchema: unknown) => Promise<JudgeTurn> {
  return async (input, threadOptions, outputSchema) => {
    // Dynamic import: the SDK runtime loads ONLY on a live call. Unit tests inject
    // `run` and never reach here, so they never instantiate the SDK.
    const { Codex } = await import('@openai/codex-sdk')
    const thread = new Codex().startThread(threadOptions)
    const controller = new AbortController()
    const timer = setTimeout(() => controller.abort(), timeoutMs)
    try {
      const turn = await thread.run(input, { outputSchema, signal: controller.signal })
      return { items: turn.items, finalResponse: turn.finalResponse }
    } finally {
      clearTimeout(timer)
    }
  }
}

// The Judge implementation. On a tool-using run it re-runs ONCE; a second
// tool-using run (or a parse miss) yields all-unsure (never a fabricated fail).
// Identical control flow + normalization to makeCodexExecJudge — only the
// transport (SDK thread.run vs CLI spawn) differs.
export function makeCodexSdkJudge(opts: CodexSdkOptions = {}): Judge {
  const model = opts.model ?? process.env.QA_JUDGE_MODEL ?? DEFAULT_JUDGE_MODEL
  const effort = opts.effort ?? process.env.QA_JUDGE_EFFORT ?? DEFAULT_JUDGE_EFFORT
  const timeoutMs = opts.timeoutMs ?? 120_000
  const run = opts.run ?? defaultRun(timeoutMs)
  const threadOptions = threadOptionsFor(model, effort)

  return async (input: JudgeInput): Promise<PerItemVerdict[]> => {
    const prompt = buildPrompt(input)
    let result: ReturnType<typeof verdictsFromTurn> | undefined
    let error: string | undefined
    try {
      for (let attempt = 0; attempt < 2; attempt++) {
        const turn = await run(sdkInput(prompt, input.images), threadOptions, OUTPUT_SCHEMA)
        result = verdictsFromTurn(turn)
        if (!result.rejectedForTool) break // pure-criticism run — accept
      }
    } catch (err) {
      error = err instanceof Error ? err.message : String(err)
    }

    // Errors, tool use and omitted verdicts abstain instead of fabricating a fail.
    let verdicts: PerItemVerdict[]
    if (error) {
      verdicts = judgeFailureVerdicts(input, `judge error: ${error}`)
    } else if (!result || result.rejectedForTool) {
      verdicts = judgeFailureVerdicts(input, 'discarded: judge run used a tool')
    } else {
      const byIndex = new Map(result.verdicts.map(v => [v.itemIndex, v]))
      verdicts = input.items.map(
        i =>
          byIndex.get(i.itemIndex) ?? {
            itemIndex: i.itemIndex,
            verdict: 'unsure',
            citation: '',
            critique: 'no verdict returned',
            judgeError: true,
          }
      )
    }

    return verdicts
  }
}
