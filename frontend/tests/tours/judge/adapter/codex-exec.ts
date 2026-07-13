// The `codex exec --json --output-schema <schema> --sandbox read-only`
// implementation of the Judge interface (design D2 — the PR2 default; the
// codex-SDK impl is a deferred follow-up behind the identical interface).
//
// The PARSE + tool-rejection logic is pure + unit-tested with canned event
// streams; the spawn is a thin wrapper (a live call, exercised by a manual
// smoke — never the merge gate).

import { spawn } from 'child_process'
import * as fs from 'fs'
import * as os from 'os'
import * as path from 'path'
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

function eventUsedTool(event: Record<string, unknown>): boolean {
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

export interface CodexExecOptions {
  bin?: string
  model?: string
  effort?: string
  tracePath?: string
  // Injected for tests: run codex and return raw stdout (defaults to a spawn).
  run?: (args: string[], prompt: string) => Promise<string>
  timeoutMs?: number
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

// The spec mandates a CHEAP judge ("cheap model judges, stronger model authors
// issues"). Pin a mini-tier model + low reasoning effort as the DEFAULT so the
// judge never silently inherits the operator's codex config (a global
// gpt-5.5 / xhigh default is both costly AND miscalibrating here — over-reasoning
// invents false fails). Overridable via QA_JUDGE_MODEL / QA_JUDGE_EFFORT or opts
// (e.g. the labeler passes a stronger model). gpt-5.4-mini is the cheapest tier
// the pinned Codex CLI supports on a ChatGPT account; gpt-5.6-luna is cheaper/
// newer but needs a Codex CLI upgrade (revalidate exact models at build time).
export const DEFAULT_JUDGE_MODEL = 'gpt-5.4-mini'
export const DEFAULT_JUDGE_EFFORT = 'low'

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

function allUnsure(input: JudgeInput, critique: string): PerItemVerdict[] {
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
          response: result ? JSON.stringify(result.verdicts, null, 2) : undefined,
          screenshots: input.images,
        })
      )
    }

    if (error) return allUnsure(input, `judge error: ${error}`)
    if (!result || result.rejectedForTool)
      return allUnsure(input, 'discarded: judge run used a tool')
    // Fill any item the model omitted with unsure.
    const byIndex = new Map(result.verdicts.map(v => [v.itemIndex, v]))
    return input.items.map(
      i =>
        byIndex.get(i.itemIndex) ?? {
          itemIndex: i.itemIndex,
          verdict: 'unsure',
          citation: '',
          critique: 'no verdict returned',
        }
    )
  }
}
