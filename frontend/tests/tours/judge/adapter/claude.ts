// The `claude -p --output-format json` implementation of the Judge interface —
// the LABELER drafter (judge/DEFERRED.md): ground-truth drafts come from a
// STRONGER model of a DIFFERENT family than the cheap codex runtime judge, so
// the judge is never graded against its own family's opinions. CLI transport
// (like codex-exec.ts) rather than a raw Anthropic endpoint: the Claude Code
// CLI is already authenticated on the dev hosts, so no API key lands in .env.
// Selected via QA_LABELER=claude (label.ts) or QA_JUDGE=claude — never the
// merge-gate default. Note `claude -p` bills the metered programmatic pool
// (June 2026 billing split), so drafting runs are deliberate, small batches.
//
// The PARSE logic is pure + unit-tested with canned result objects; the spawn
// is a thin wrapper (a live call, exercised by a manual smoke — never the
// merge gate). A run that used tools (num_turns > 1 in print mode) is NOT a
// pure-criticism verdict (design D4): re-run ONCE, then all-unsure.

import { spawn } from 'child_process'
import { buildPrompt, parseVerdicts } from './prompt'
import { appendSpan, buildGenAiSpan } from './span'
import type { Judge, JudgeInput, PerItemVerdict } from './types'

// The drafter default: a strong Claude tier (the runtime judge stays a cheap
// codex mini — see codex-exec.ts). Override via QA_LABELER_MODEL (threaded
// through selectJudge by label.ts) or QA_CLAUDE_MODEL.
export const DEFAULT_CLAUDE_MODEL = 'claude-opus-4-8'

// Replaces the Claude Code default system prompt (a tool-using coding-agent
// preamble): the judge is criticism, not agency, and the user prompt already
// pins the verdict JSON shape.
export const CLAUDE_JUDGE_SYSTEM_PROMPT =
  'You are a read-only UX behavior judge. Reply with ONLY the JSON output the ' +
  'prompt specifies — no prose, no code fences. Never use a tool.'

export interface ClaudeParseResult {
  message: string | undefined
  isError: boolean
  numTurns: number
  inputTokens: number | undefined
  outputTokens: number | undefined
}

// Parse the `claude -p --output-format json` result object. Pure — the
// testable seam.
export function parseClaudeResult(stdout: string): ClaudeParseResult {
  const result: ClaudeParseResult = {
    message: undefined,
    isError: false,
    numTurns: 1,
    inputTokens: undefined,
    outputTokens: undefined,
  }
  let obj: Record<string, unknown>
  try {
    obj = JSON.parse(stdout) as Record<string, unknown>
  } catch {
    return { ...result, isError: true }
  }
  if (typeof obj.result === 'string') result.message = obj.result
  if (obj.is_error === true || obj.subtype !== 'success') result.isError = true
  if (typeof obj.num_turns === 'number') result.numTurns = obj.num_turns
  const usage = obj.usage as Record<string, unknown> | undefined
  if (usage) {
    if (typeof usage.input_tokens === 'number') result.inputTokens = usage.input_tokens
    if (typeof usage.output_tokens === 'number') result.outputTokens = usage.output_tokens
  }
  return result
}

// Build the argv for `claude -p` (prompt arrives on stdin).
export function claudeArgs(model?: string): string[] {
  const args = ['-p', '--output-format', 'json', '--system-prompt', CLAUDE_JUDGE_SYSTEM_PROMPT]
  if (model) args.push('--model', model)
  return args
}

export interface ClaudeExecOptions {
  bin?: string
  model?: string
  tracePath?: string
  // Injected for tests: run claude and return raw stdout (defaults to a spawn).
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
        reject(new Error(`claude -p timed out after ${timeoutMs}ms`))
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
        else reject(new Error(`claude -p exited ${code}: ${stderr.slice(0, 500)}`))
      })
      child.stdin.write(prompt)
      child.stdin.end()
    })
}

function allUnsure(input: JudgeInput, critique: string): PerItemVerdict[] {
  return input.items.map(i => ({
    itemIndex: i.itemIndex,
    verdict: 'unsure' as const,
    citation: '',
    critique,
  }))
}

// The Judge implementation. On a tool-using run (num_turns > 1) it re-runs
// ONCE; a second tool-using run (or a parse miss) yields all-unsure (never a
// fabricated fail).
export function makeClaudeJudge(opts: ClaudeExecOptions = {}): Judge {
  const bin = opts.bin ?? process.env.QA_CLAUDE_BIN ?? 'claude'
  const model = opts.model ?? process.env.QA_CLAUDE_MODEL ?? DEFAULT_CLAUDE_MODEL
  const timeoutMs = opts.timeoutMs ?? 300_000
  const run = opts.run ?? defaultRun(bin, timeoutMs)
  const tracePath = opts.tracePath ?? process.env.QA_JUDGE_TRACE

  return async (input: JudgeInput): Promise<PerItemVerdict[]> => {
    // This adapter posts text only — it does not attach image files, so the
    // prompt must keep the aria-only visual framing even when the caller
    // resolved screenshots (else the model is told images exist that it
    // cannot see, licensing false visual grounding).
    const prompt = buildPrompt({ ...input, images: undefined })
    const start = Date.now()
    let parsed: ClaudeParseResult | undefined
    let error: string | undefined
    try {
      for (let attempt = 0; attempt < 2; attempt++) {
        const stdout = await run(claudeArgs(model), prompt)
        parsed = parseClaudeResult(stdout)
        if (parsed.numTurns <= 1) break // pure-criticism run — accept
      }
    } catch (err) {
      error = err instanceof Error ? err.message : String(err)
    }

    const end = Date.now()
    if (tracePath) {
      appendSpan(
        tracePath,
        buildGenAiSpan({
          impl: 'claude',
          behaviorId: input.behaviorId,
          model,
          startMs: start,
          endMs: end,
          inputTokens: parsed?.inputTokens,
          outputTokens: parsed?.outputTokens,
          toolRejected: parsed !== undefined && parsed.numTurns > 1,
          error,
        })
      )
    }

    if (error) return allUnsure(input, `judge error: ${error}`)
    if (!parsed || parsed.numTurns > 1) return allUnsure(input, 'discarded: judge run used a tool')
    if (parsed.isError || parsed.message === undefined)
      return allUnsure(input, 'judge error: claude -p returned an error result')
    const verdicts = parseVerdicts(parsed.message)
    const byIndex = new Map(verdicts.map(v => [v.itemIndex, v]))
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
