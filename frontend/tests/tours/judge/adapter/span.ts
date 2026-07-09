// OTel GenAI-convention span records (Tier-1 instrumentation: git-diffable JSONL,
// no external backend). Field naming follows the OpenTelemetry GenAI semantic
// conventions so any OTLP backend can ingest runs later without rework.

import * as fs from 'fs'
import * as path from 'path'

export interface GenAiSpan {
  name: string // "chat <model>" per the GenAI span-name convention
  trace_id: string
  span_id: string
  start_time_unix_nano: number
  end_time_unix_nano: number
  attributes: Record<string, unknown>
  status: { code: 'OK' | 'ERROR'; message?: string }
}

export interface SpanParams {
  impl: string // qa.judge.impl — codex-exec | http | codex-sdk
  behaviorId: string
  model?: string
  startMs: number
  endMs: number
  inputTokens?: number
  outputTokens?: number
  finishReasons?: string[]
  toolRejected?: boolean
  error?: string
}

// 16 random hex bytes (trace id) / 8 (span id).
function randHex(bytes: number): string {
  let s = ''
  for (let i = 0; i < bytes; i++)
    s += Math.floor(Math.random() * 256)
      .toString(16)
      .padStart(2, '0')
  return s
}

export function buildGenAiSpan(p: SpanParams): GenAiSpan {
  const model = p.model ?? 'unknown'
  const attributes: Record<string, unknown> = {
    'gen_ai.operation.name': 'chat',
    'gen_ai.provider.name': 'openai',
    'gen_ai.system': 'openai',
    'gen_ai.request.model': model,
    'gen_ai.response.model': model,
    // harness-specific attributes (namespaced, not gen_ai.*)
    'qa.judge.impl': p.impl,
    'qa.behavior_id': p.behaviorId,
    'qa.tool_rejected': p.toolRejected ?? false,
  }
  if (p.inputTokens !== undefined) attributes['gen_ai.usage.input_tokens'] = p.inputTokens
  if (p.outputTokens !== undefined) attributes['gen_ai.usage.output_tokens'] = p.outputTokens
  if (p.finishReasons !== undefined) attributes['gen_ai.response.finish_reasons'] = p.finishReasons

  return {
    name: `chat ${model}`,
    trace_id: randHex(16),
    span_id: randHex(8),
    start_time_unix_nano: Math.round(p.startMs * 1e6),
    end_time_unix_nano: Math.round(p.endMs * 1e6),
    attributes,
    status: p.error ? { code: 'ERROR', message: p.error } : { code: 'OK' },
  }
}

// Append a span as one JSONL line to the trace artifact.
export function appendSpan(tracePath: string, span: GenAiSpan): void {
  fs.mkdirSync(path.dirname(tracePath), { recursive: true })
  fs.appendFileSync(tracePath, `${JSON.stringify(span)}\n`, 'utf8')
}
