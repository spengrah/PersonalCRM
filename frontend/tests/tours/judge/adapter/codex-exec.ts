// The `codex exec --json --output-schema <schema> --sandbox read-only`
// implementation of the Judge interface (design D2). Selectable via
// QA_JUDGE=codex-exec; the codex-sdk adapter is the default and a like-for-like
// transport swap behind the identical interface.
//
// The PARSE + tool-rejection logic is pure + unit-tested with canned event
// streams; the spawn is a thin wrapper (a live call, exercised by a manual
// smoke, not by the automated tests).

import { spawn } from 'child_process'
import * as fs from 'fs'
import * as os from 'os'
import * as path from 'path'
import { buildGradedEvidence, buildScenario } from '../label-trace'
import { DEFAULT_JUDGE_EFFORT, DEFAULT_JUDGE_MODEL } from '../models'
import { buildPrompt, OUTPUT_SCHEMA, parseVerdicts } from './prompt'
import { appendSpan, buildGenAiSpan } from './span'
import type { Judge, JudgeInput, PerItemVerdict } from './types'

// Event `type`/nested-type substrings that indicate the model used a tool /
// executed a command — such a run is NOT a pure-criticism verdict (D4).
const TOOL_EVENT_MARKERS = [
  'command',
  'exec_command',
  'exec',
  'tool',
  'function_call',
  'shell',
  'mcp',
  'apply_patch',
  'patch',
  'web_search',
  'browse',
]

export interface CodexParseResult {
  message: string | undefined
  usedTool: boolean
  model: string | undefined
  inputTokens: number | undefined
  outputTokens: number | undefined
  // The CLI event stream reports `input_tokens` INCLUSIVE of cache reads and this
  // parser extracts no separate cached figure, so it stays undefined on every real
  // parse. Declared anyway because it is what `usageMissesCachedCount` keys off:
  // a transport that ever starts reporting the count silences the warning by
  // populating this, rather than by someone remembering to delete a check.
  cachedInputTokens?: number
}

function typeStringsOf(event: Record<string, unknown>): string[] {
  const out: string[] = []
  const collect = (v: unknown): void => {
    if (typeof v === 'string') out.push(v.toLowerCase())
  }
  collect(event.type)
  const msg = event.msg as Record<string, unknown> | undefined
  if (msg) collect(msg.type)
  const item = event.item as Record<string, unknown> | undefined
  if (item) collect(item.type)
  return out
}

export function eventUsedTool(event: Record<string, unknown>): boolean {
  const types = typeStringsOf(event)
  return types.some(t => TOOL_EVENT_MARKERS.some(m => t.includes(m)))
}

// Extract the final agent/assistant message text from an event, if any.
function messageTextOf(event: Record<string, unknown>): string | undefined {
  const types = typeStringsOf(event)
  const isMessage = types.some(
    t => t.includes('agent_message') || t.includes('assistant') || t === 'message'
  )
  if (!isMessage) return undefined
  const msg = event.msg as Record<string, unknown> | undefined
  const item = event.item as Record<string, unknown> | undefined
  const candidates = [event.text, event.message, msg?.text, msg?.message, item?.text, item?.message]
  const found = candidates.find(c => typeof c === 'string' && c.trim() !== '')
  return typeof found === 'string' ? found : undefined
}

// Parse the codex `--json` JSONL event stream. Pure — the testable seam.
export function parseCodexEventStream(stdout: string): CodexParseResult {
  const result: CodexParseResult = {
    message: undefined,
    usedTool: false,
    model: undefined,
    inputTokens: undefined,
    outputTokens: undefined,
  }
  for (const line of stdout.split('\n')) {
    const trimmed = line.trim()
    if (trimmed === '') continue
    let event: Record<string, unknown>
    try {
      event = JSON.parse(trimmed) as Record<string, unknown>
    } catch {
      continue
    }
    if (eventUsedTool(event)) result.usedTool = true
    const text = messageTextOf(event)
    if (text !== undefined) result.message = text // keep the LAST message

    // Best-effort usage/model extraction (varies by CLI version).
    const msg = (event.msg as Record<string, unknown>) ?? event
    if (typeof msg.model === 'string') result.model = msg.model
    const usage = (msg.usage ?? msg.token_usage) as Record<string, unknown> | undefined
    if (usage) {
      const inTok = usage.input_tokens ?? usage.prompt_tokens
      const outTok = usage.output_tokens ?? usage.completion_tokens
      if (typeof inTok === 'number') result.inputTokens = inTok
      if (typeof outTok === 'number') result.outputTokens = outTok
    }
  }
  return result
}

// From raw codex stdout → verdicts (or a tool-rejection signal). Pure.
export function verdictsFromCodexOutput(stdout: string): {
  verdicts: PerItemVerdict[]
  rejectedForTool: boolean
  parse: CodexParseResult
} {
  const parse = parseCodexEventStream(stdout)
  if (parse.usedTool) return { verdicts: [], rejectedForTool: true, parse }
  const verdicts = parse.message !== undefined ? parseVerdicts(parse.message) : []
  return { verdicts, rejectedForTool: false, parse }
}

// This transport's usage is INCOMPLETE, and silently so: the CLI's stream reports
// one inclusive `input_tokens` and no cached-read figure, so every span it emits
// prices cache reads at the full input rate. On an observed run that is roughly an
// 8x overstatement of input cost. The number still LOOKS authoritative downstream,
// which is exactly why the operator has to be told at the moment it is produced.
export const EXEC_CACHED_USAGE_WARNING =
  'codex-exec reports no cached-input token count (its input_tokens is inclusive of ' +
  'cache reads), so every cached token prices at the full input rate and this run’s ' +
  'reported cost is an OVERSTATEMENT. Use QA_JUDGE=codex-sdk for accurate usage.'

// A usage-bearing result carrying no cached count — the condition the warning
// exists for. Pure, so the gate is testable without a run.
export function usageMissesCachedCount(parse: CodexParseResult | undefined): boolean {
  return parse?.inputTokens !== undefined && parse.cachedInputTokens === undefined
}

// Once per process, not per judge call: a round makes one call per behavior, and
// a line per call would bury the very thing it is warning about.
let cachedUsageWarned = false

// Test seam — the flag is module state, so a suite that asserts the once-only
// behaviour needs to clear it between cases.
export function resetCachedUsageWarning(): void {
  cachedUsageWarned = false
}

function warnCachedUsageOnce(warn: (msg: string) => void): void {
  if (cachedUsageWarned) return
  cachedUsageWarned = true
  warn(EXEC_CACHED_USAGE_WARNING)
}

export interface CodexExecOptions {
  bin?: string
  model?: string
  effort?: string
  tracePath?: string
  // Injected for tests: run codex and return raw stdout (defaults to a spawn).
  run?: (args: string[], prompt: string) => Promise<string>
  timeoutMs?: number
  // Where the incomplete-usage warning goes (defaults to stderr).
  warn?: (msg: string) => void
}

function defaultRun(
  bin: string,
  timeoutMs: number
): (args: string[], prompt: string) => Promise<string> {
  return (args, prompt) =>
    new Promise<string>((resolve, reject) => {
      const child = spawn(bin, args, { stdio: ['pipe', 'pipe', 'pipe'] })
      let stdout = ''
      let stderr = ''
      const timer = setTimeout(() => {
        child.kill('SIGKILL')
        reject(new Error(`codex exec timed out after ${timeoutMs}ms`))
      }, timeoutMs)
      child.stdout.on('data', d => (stdout += String(d)))
      child.stderr.on('data', d => (stderr += String(d)))
      child.on('error', err => {
        clearTimeout(timer)
        reject(err)
      })
      child.on('close', code => {
        clearTimeout(timer)
        if (code === 0) resolve(stdout)
        else reject(new Error(`codex exec exited ${code}: ${stderr.slice(0, 500)}`))
      })
      child.stdin.write(prompt)
      child.stdin.end()
    })
}

// Monotonic per-process counter for the schema temp file, so judge calls in one
// process can never collide on the same pid+millisecond path (callers may invoke
// the judge for several behaviors close together).
let schemaSeq = 0

// Build the argv for `codex exec` (schema written to a temp file). Passes
// --model when set (so the model is actually selected, not just labeled in the
// trace) and pins reasoning effort via `-c` when set — the judge must NOT
// inherit the operator's codex config effort (e.g. a global xhigh). Images
// (intent-pass screenshots) attach via `-i` per file.
export function codexArgs(
  schemaPath: string,
  model?: string,
  effort?: string,
  images?: string[]
): string[] {
  const args = ['exec', '--json', '--output-schema', schemaPath, '--sandbox', 'read-only']
  if (model) args.push('--model', model)
  if (effort) args.push('-c', `model_reasoning_effort=${effort}`)
  for (const img of images ?? []) args.push('-i', img)
  args.push('-')
  return args
}

export function allUnsure(input: JudgeInput, critique: string): PerItemVerdict[] {
  return input.items.map(i => ({
    itemIndex: i.itemIndex,
    verdict: 'unsure' as const,
    citation: '',
    critique,
  }))
}

// The Judge implementation. On a tool-using run it re-runs ONCE; a second
// tool-using run (or a parse miss) yields all-unsure (never a fabricated fail).
export function makeCodexExecJudge(opts: CodexExecOptions = {}): Judge {
  const bin = opts.bin ?? process.env.QA_JUDGE_CODEX_BIN ?? 'codex'
  const model = opts.model ?? process.env.QA_JUDGE_MODEL ?? DEFAULT_JUDGE_MODEL
  const effort = opts.effort ?? process.env.QA_JUDGE_EFFORT ?? DEFAULT_JUDGE_EFFORT
  const timeoutMs = opts.timeoutMs ?? 120_000
  const run = opts.run ?? defaultRun(bin, timeoutMs)
  const tracePath = opts.tracePath ?? process.env.QA_JUDGE_TRACE
  const warn = opts.warn ?? ((msg: string) => console.warn(msg))

  return async (input: JudgeInput): Promise<PerItemVerdict[]> => {
    const prompt = buildPrompt(input)
    const schemaPath = path.join(
      os.tmpdir(),
      `qa-judge-schema-${process.pid}-${Date.now()}-${schemaSeq++}.json`
    )
    fs.writeFileSync(schemaPath, JSON.stringify(OUTPUT_SCHEMA), 'utf8')
    const start = Date.now()
    let result: ReturnType<typeof verdictsFromCodexOutput> | undefined
    let error: string | undefined
    try {
      for (let attempt = 0; attempt < 2; attempt++) {
        const stdout = await run(codexArgs(schemaPath, model, effort, input.images), prompt)
        result = verdictsFromCodexOutput(stdout)
        if (!result.rejectedForTool) break // pure-criticism run — accept
      }
    } catch (err) {
      error = err instanceof Error ? err.message : String(err)
    } finally {
      fs.rmSync(schemaPath, { force: true })
    }

    const end = Date.now()

    // Normalize the verdicts ONCE, BEFORE the span append — so the span's
    // `response`/`item_verdicts` are the SAME array the judge returns, not the
    // pre-normalization parse (error → allUnsure, tool-rejected → allUnsure,
    // else omitted-item fill). One source of truth for span + return value.
    let verdicts: PerItemVerdict[]
    if (error) {
      verdicts = allUnsure(input, `judge error: ${error}`)
    } else if (!result || result.rejectedForTool) {
      verdicts = allUnsure(input, 'discarded: judge run used a tool')
    } else {
      const byIndex = new Map(result.verdicts.map(v => [v.itemIndex, v]))
      verdicts = input.items.map(
        i =>
          byIndex.get(i.itemIndex) ?? {
            itemIndex: i.itemIndex,
            verdict: 'unsure',
            citation: '',
            critique: 'no verdict returned',
          }
      )
    }

    // The usage this run produced is missing its cached-read share. Say so before
    // the span is written, so the operator sees it alongside the run that emitted
    // the understated split rather than discovering it in a cost figure later.
    if (usageMissesCachedCount(result?.parse)) warnCachedUsageOnce(warn)

    if (tracePath) {
      appendSpan(
        tracePath,
        buildGenAiSpan({
          impl: 'codex-exec',
          behaviorId: input.behaviorId,
          model: result?.parse.model ?? model,
          startMs: start,
          endMs: end,
          inputTokens: result?.parse.inputTokens,
          outputTokens: result?.parse.outputTokens,
          toolRejected: result?.rejectedForTool,
          error,
          // Content IS logged here: the QA judge grades a provably-synthetic
          // corpus, and a span without it cannot be adjudicated by a human later.
          // See SpanParams — this is a call-site decision, not an env default.
          prompt,
          response: JSON.stringify(verdicts),
          screenshots: input.images,
          // The label-trace contract carriers (spec lines 51–53).
          scenario: buildScenario(input),
          gradedEvidence: buildGradedEvidence(input, input.images ?? []),
          itemVerdicts: verdicts,
          mutation: input.__trap?.mutation,
        })
      )
    }

    return verdicts
  }
}
