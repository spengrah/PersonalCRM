// OTel GenAI-convention span records (Tier-1 instrumentation: git-diffable JSONL,
// no external backend). Field naming follows the OpenTelemetry GenAI semantic
// conventions so any OTLP backend can ingest runs later without rework.

import * as fs from 'fs'
import * as path from 'path'
import type { GradedEvidenceEntry, Scenario } from '../label-trace'
import type { PerItemVerdict } from './types'

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
  // --- Content (prompt / response / screenshots) ---
  //
  // Deliberately OPT-IN AT THE CALL SITE, never from an env default. Whether a
  // span may carry content is a property of the DATA, and only the caller knows
  // its provenance: the QA judge grades a provably-synthetic corpus, so it passes
  // content in full. A future extraction judge (#379) grades REAL message content
  // and must pass none of these — its traces carry outputs, tokens, and id
  // references only. Defaulting content on (or gating it on an env var someone
  // forgets to set) would put real content on a rented VPS.
  //
  // A metrics-only span is also an UNLABELABLE span: a reviewer opening the
  // annotation queue has nothing to read. That is why the QA path logs content —
  // you cannot label what you didn't log.
  prompt?: string
  response?: string
  screenshots?: string[] // absolute paths; the exporter uploads them as media
  // --- Label-trace contract (spec lines 51–53; same call-site opt-in) ---
  //
  // These are the STRUCTURED evidence a Langfuse reviewer adjudicates against
  // (built from JudgeInput at the adapter, never re-parsed from the prompt).
  // They are CONTENT and follow the same opt-in discipline as prompt/response
  // above — a future #379 real-content judge passes none. The JSONL span stays
  // backend-neutral: the exporter (`export/langfuse.ts`) fans `scenario.items`
  // out to one trace per graded item and DERIVES `screenshot_caveat` from
  // `mutation`; nothing Langfuse-specific lives here.
  scenario?: Scenario // GWT + graded items (behavior) OR goal/status (intent)
  gradedEvidence?: GradedEvidenceEntry[] // per-capture, real capture_file + own screenshot
  itemVerdicts?: PerItemVerdict[] // per-item verdicts (the exporter's per-item `output`)
  mutation?: unknown // the doctoring on the trap self-test path; undefined = real capture
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
  // Content, when the caller supplied it (see SpanParams). gen_ai.prompt /
  // gen_ai.completion are the conventional content attributes every OTLP backend
  // recognizes; screenshots stay a harness-namespaced path list because OTLP has
  // no media channel — the exporter resolves them against its backend's object store.
  if (p.prompt !== undefined) attributes['gen_ai.prompt'] = p.prompt
  if (p.response !== undefined) attributes['gen_ai.completion'] = p.response
  if (p.screenshots?.length) attributes['qa.screenshots'] = p.screenshots
  // Label-trace contract carriers (harness-namespaced). `qa.mutation` is set
  // ONLY when the caller doctored the evidence — its presence is what the
  // exporter keys the screenshot_caveat off, so an undefined mutation must NOT
  // materialize the attribute.
  if (p.scenario !== undefined) attributes['qa.scenario'] = p.scenario
  if (p.gradedEvidence !== undefined) attributes['qa.graded_evidence'] = p.gradedEvidence
  if (p.itemVerdicts !== undefined) attributes['qa.item_verdicts'] = p.itemVerdicts
  if (p.mutation !== undefined) attributes['qa.mutation'] = p.mutation

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
