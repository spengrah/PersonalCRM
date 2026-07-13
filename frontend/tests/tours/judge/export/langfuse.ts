// Langfuse exporter — ships the judge's GenAI span records (adapter/span.ts) to a
// self-hosted Langfuse, with the intent pass's screenshots attached as media.
//
// WHY NOT PLAIN OTLP: the harness's span record is deliberately backend-neutral
// (OTel GenAI conventions, JSONL), and Langfuse does accept OTLP. But OTLP has no
// media channel, and the intent judge's evidence IS the screenshots — a verdict a
// reviewer cannot see the pixels for is a verdict they cannot adjudicate. So this
// adapter uses Langfuse's ingestion + media APIs, and the neutrality lives where it
// belongs: in `span.ts`, which knows nothing about Langfuse. Swapping backends means
// writing a sibling of THIS file, not touching the harness.
//
// Media flow (all three steps required): register -> presigned PUT -> PATCH finalize.
// Skipping the PATCH leaves the media 404ing even though the bytes are in the store.

import * as crypto from 'crypto'
import * as fs from 'fs'
import type { GenAiSpan } from '../adapter/span'

export interface LangfuseConfig {
  host: string
  publicKey: string
  secretKey: string
}

export interface TraceBody {
  id: string
  name: string
  input?: unknown
  output?: unknown
  metadata: Record<string, unknown>
}

// Resolve config from env. Returns undefined when Langfuse is not configured —
// export is strictly opt-in; a run without these vars simply doesn't ship.
export function configFromEnv(
  env: Record<string, string | undefined> = process.env
): LangfuseConfig | undefined {
  const host = env.LANGFUSE_HOST
  const publicKey = env.LANGFUSE_PUBLIC_KEY
  const secretKey = env.LANGFUSE_SECRET_KEY
  if (!host || !publicKey || !secretKey) return undefined
  return { host, publicKey, secretKey }
}

// A stable trace id per (behavior, span) so a re-export overwrites rather than
// duplicates. Langfuse trace ids are free-form strings.
export function traceIdFor(span: GenAiSpan): string {
  const behavior = String(span.attributes['qa.behavior_id'] ?? 'unknown')
  return `judge-${behavior}-${span.span_id}`
}

const str = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined)
const num = (v: unknown): number | undefined => (typeof v === 'number' ? v : undefined)

// PURE: build the trace body for one span. `mediaTokens` are the Langfuse media
// references for this span's screenshots (already uploaded), spliced into the input
// so the reviewer sees exactly what the judge saw.
export function buildTraceBody(span: GenAiSpan, mediaTokens: string[] = []): TraceBody {
  const a = span.attributes
  const behaviorId = String(a['qa.behavior_id'] ?? 'unknown')
  const prompt = str(a['gen_ai.prompt'])
  const completion = str(a['gen_ai.completion'])
  const screenshots = Array.isArray(a['qa.screenshots']) ? (a['qa.screenshots'] as string[]) : []

  const input: Record<string, unknown> = {}
  if (prompt !== undefined) input.prompt = prompt
  if (mediaTokens.length) input.screenshots = mediaTokens

  return {
    id: traceIdFor(span),
    name: `judge ${behaviorId}`,
    input: Object.keys(input).length ? input : undefined,
    output: completion,
    metadata: {
      behavior_id: behaviorId,
      impl: a['qa.judge.impl'],
      model: a['gen_ai.request.model'],
      input_tokens: num(a['gen_ai.usage.input_tokens']),
      output_tokens: num(a['gen_ai.usage.output_tokens']),
      tool_rejected: a['qa.tool_rejected'],
      status: span.status.code,
      error: span.status.message,
      duration_ms: Math.round((span.end_time_unix_nano - span.start_time_unix_nano) / 1e6),
      // Honesty: say how many screenshots the judge HAD vs how many made it here.
      // A silent gap between the two would misrepresent the evidence.
      screenshots_expected: screenshots.length,
      screenshots_attached: mediaTokens.length,
    },
  }
}

// PURE: parse a JSONL trace artifact. Malformed lines are skipped, not fatal — a
// partial trace is still worth shipping.
export function parseSpanFile(contents: string): GenAiSpan[] {
  const out: GenAiSpan[] = []
  for (const line of contents.split('\n')) {
    if (!line.trim()) continue
    try {
      out.push(JSON.parse(line) as GenAiSpan)
    } catch {
      // skip
    }
  }
  return out
}

function authHeader(cfg: LangfuseConfig): string {
  return 'Basic ' + Buffer.from(`${cfg.publicKey}:${cfg.secretKey}`).toString('base64')
}

async function api(
  cfg: LangfuseConfig,
  method: string,
  path: string,
  body?: unknown
): Promise<Record<string, unknown>> {
  const res = await fetch(`${cfg.host}${path}`, {
    method,
    headers: { Authorization: authHeader(cfg), 'Content-Type': 'application/json' },
    body: body ? JSON.stringify(body) : undefined,
  })
  const text = await res.text()
  if (!res.ok) throw new Error(`${method} ${path} -> ${res.status} ${text.slice(0, 200)}`)
  return text ? (JSON.parse(text) as Record<string, unknown>) : {}
}

// Upload one screenshot and return its media token, or undefined if the file is
// gone (a best-effort screenshot that never got taken is normal, not an error).
export async function uploadMedia(
  cfg: LangfuseConfig,
  traceId: string,
  file: string
): Promise<string | undefined> {
  if (!fs.existsSync(file)) return undefined
  const bytes = fs.readFileSync(file)
  const sha = crypto.createHash('sha256').update(bytes).digest('base64')

  const reg = await api(cfg, 'POST', '/api/public/media', {
    traceId,
    field: 'input',
    contentType: 'image/png',
    contentLength: bytes.length,
    sha256Hash: sha,
  })
  const mediaId = str(reg.mediaId)
  if (!mediaId) return undefined

  // uploadUrl is absent when Langfuse already has these bytes (dedup by sha) —
  // that is a HIT, not a failure: reference the existing media and move on.
  const uploadUrl = str(reg.uploadUrl)
  if (uploadUrl) {
    const put = await fetch(uploadUrl, {
      method: 'PUT',
      headers: { 'Content-Type': 'image/png', 'x-amz-checksum-sha256': sha },
      body: new Uint8Array(bytes),
    })
    if (!put.ok) return undefined
    // The finalize step. Without it the bytes sit in the object store and every
    // read 404s — the single most expensive thing to rediscover about this API.
    await api(cfg, 'PATCH', `/api/public/media/${mediaId}`, {
      uploadedAt: new Date().toISOString(),
      uploadHttpStatus: put.status,
    })
  }
  return `@@@langfuseMedia:type=image/png|id=${mediaId}|source=bytes@@@`
}

export interface ExportResult {
  traces: number
  screenshots: number
  failed: number
}

export async function exportSpans(
  cfg: LangfuseConfig,
  spans: GenAiSpan[],
  log: (msg: string) => void = () => {}
): Promise<ExportResult> {
  const result: ExportResult = { traces: 0, screenshots: 0, failed: 0 }

  for (const span of spans) {
    const traceId = traceIdFor(span)
    try {
      // The trace must exist before media can be registered against it.
      await api(cfg, 'POST', '/api/public/ingestion', {
        batch: [
          {
            id: `evt-${traceId}-init`,
            type: 'trace-create',
            timestamp: new Date().toISOString(),
            body: { id: traceId, name: `judge ${span.attributes['qa.behavior_id']}` },
          },
        ],
      })

      const files = Array.isArray(span.attributes['qa.screenshots'])
        ? (span.attributes['qa.screenshots'] as string[])
        : []
      const tokens: string[] = []
      for (const f of files) {
        const tok = await uploadMedia(cfg, traceId, f)
        if (tok) tokens.push(tok)
      }
      result.screenshots += tokens.length

      await api(cfg, 'POST', '/api/public/ingestion', {
        batch: [
          {
            id: `evt-${traceId}`,
            type: 'trace-create',
            timestamp: new Date().toISOString(),
            body: buildTraceBody(span, tokens),
          },
        ],
      })
      result.traces++
      log(
        `  ${span.attributes['qa.behavior_id']} → ${traceId} (media ${tokens.length}/${files.length})`
      )
    } catch (err) {
      result.failed++
      log(
        `  FAILED ${span.attributes['qa.behavior_id']}: ${err instanceof Error ? err.message : String(err)}`
      )
    }
  }
  return result
}
