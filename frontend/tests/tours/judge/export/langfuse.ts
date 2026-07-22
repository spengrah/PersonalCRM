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
import type { PerItemVerdict } from '../adapter/types'
import type { GradedEvidenceEntry, Scenario } from '../label-trace'
import { SCREENSHOT_CAVEAT } from '../label-trace'
import { createScrubber, scrubValue, type Scrubber } from '../scrub'
import { TRIAGE_QUEUE_NAME, VERDICT_SCORE_NAME } from './triage-config'

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
// duplicates. Langfuse trace ids are free-form strings. This is the BASE id; the
// per-item traces suffix it with `-item<itemIndex>` (see `itemTraceId`).
export function traceIdFor(span: GenAiSpan): string {
  const behavior = String(span.attributes['qa.behavior_id'] ?? 'unknown')
  return `judge-${behavior}-${span.span_id}`
}

// The trace granularity is PER GRADED ITEM (spec line 41): one span fans out to
// one trace per item, each carrying that item's singular `then_text` + verdict.
// The item's own index keeps the id unique within the span (indices are unique
// per judge call), so a re-export still overwrites the right per-item trace.
function itemTraceId(span: GenAiSpan, itemIndex: number): string {
  return `${traceIdFor(span)}-item${itemIndex}`
}

const str = (v: unknown): string | undefined => (typeof v === 'string' ? v : undefined)
const num = (v: unknown): number | undefined => (typeof v === 'number' ? v : undefined)

// One graded item to fan a trace out over: `undefined` = the metrics-only /
// #379 shape (no scenario → a single content-light trace, no scenario_item).
type ItemDescriptor = { itemIndex: number; thenText: string } | undefined

// Resolve the fan-out: one descriptor per graded item (behavior), one for the
// single statement item (intent), or a single `undefined` (no scenario → the
// content-light metrics trace, preserving the pre-contract shape).
function itemDescriptors(scenario: Scenario | undefined): ItemDescriptor[] {
  if (!scenario) return [undefined]
  if (scenario.kind === 'intent') return [{ itemIndex: 0, thenText: scenario.statement }]
  return scenario.items.map(i => ({ itemIndex: i.itemIndex, thenText: i.thenText }))
}

// The item-trace that carries the span's usage: the one with the LOWEST
// itemIndex — not necessarily index 0, and not necessarily first in the array.
// A span's token counts belong to ONE judge call, so attributing them to every
// item-trace it fans out to would multiply the reported spend by the item count.
// For the no-scenario metrics-only shape this is the BASE trace id.
export function usageTraceId(span: GenAiSpan): string {
  const scenario = (span.attributes['qa.scenario'] as Scenario | undefined) ?? undefined
  const indices = itemDescriptors(scenario)
    .filter((d): d is Exclude<ItemDescriptor, undefined> => d !== undefined)
    .map(d => d.itemIndex)
  if (indices.length === 0) return traceIdFor(span)
  return itemTraceId(span, Math.min(...indices))
}

// PURE: build the label-trace bodies for one span — ONE per graded item (spec
// line 41), returned as token-free SKELETONS (no media tokens, and NO local
// screenshot paths: paths never ship — D9). `exportSpans` runs the per-item
// media lifecycle and calls `attachTokens` before shipping.
//
// PII scrub seam (arc INV-2): `scrub` is applied to EVERY free-form or
// env-sourced string this body ships — the whole `input`/`output` is deep-walked
// (`scrubValue`), so the prompt, every scenario string, every graded-evidence
// note + evidence block, the mutation's values, and the per-item verdict
// (citation/critique) all route through it; `metadata.error`
// (span.status.message — caught-exception text that can embed model output) and
// the env-configurable `metadata.model` are scrubbed too. Every OTHER string is
// identifier/enum-valued BY CONSTRUCTION (trace `name`/`behavior_id` = a spec id;
// `impl`/`status` = closed enums; `capture_file` = a filename). POLICY: any NEW
// free-form string added to a body MUST sit inside `input`/`output` (so the deep
// scrub covers it) or be routed through `scrub` explicitly.
export function buildTraceBody(
  span: GenAiSpan,
  scrub: (s: string) => string = s => s
): TraceBody[] {
  const a = span.attributes
  const behaviorId = String(a['qa.behavior_id'] ?? 'unknown')
  const scrubber: Scrubber = { scrub }
  const deep = (v: unknown): unknown => scrubValue(v, scrubber)

  const rawPrompt = str(a['gen_ai.prompt'])
  const rawCompletion = str(a['gen_ai.completion'])
  const rawModel = str(a['gen_ai.request.model'])
  const rawError = span.status.message
  const model = rawModel !== undefined ? scrub(rawModel) : a['gen_ai.request.model']
  const error = rawError !== undefined ? scrub(rawError) : undefined

  const scenario = (a['qa.scenario'] as Scenario | undefined) ?? undefined
  const mutation = a['qa.mutation'] // undefined ⇒ real capture (no caveat)
  const spanGraded = Array.isArray(a['qa.graded_evidence'])
    ? (a['qa.graded_evidence'] as GradedEvidenceEntry[])
    : []
  const itemVerdicts = Array.isArray(a['qa.item_verdicts'])
    ? (a['qa.item_verdicts'] as PerItemVerdict[])
    : []

  // The screenshots the reviewer would attach, index-aligned to graded_evidence.
  // `screenshots_expected` counts the entries that HAVE a path (per trace);
  // `exportSpans` registers exactly these against each item-trace's id.
  const expected = spanGraded.filter(e => typeof e.screenshot === 'string').length

  // The graded-evidence skeleton: real capture_file + note + evidence, but NO
  // screenshot (paths never ship; attachTokens splices the media token by index).
  const gradedSkeleton = spanGraded.map(e => ({
    capture_file: e.captureFile,
    note: e.note,
    evidence: e.evidence,
  }))

  const commonMetadata = {
    behavior_id: behaviorId,
    impl: a['qa.judge.impl'],
    model,
    input_tokens: num(a['gen_ai.usage.input_tokens']),
    cached_input_tokens: num(a['gen_ai.usage.cached_input_tokens']),
    output_tokens: num(a['gen_ai.usage.output_tokens']),
    reasoning_output_tokens: num(a['gen_ai.usage.reasoning_output_tokens']),
    cache_write_input_tokens: num(a['qa.usage.cache_write_input_tokens']),
    tool_rejected: a['qa.tool_rejected'],
    status: span.status.code,
    error,
    duration_ms: Math.round((span.end_time_unix_nano - span.start_time_unix_nano) / 1e6),
    // Honesty: how many screenshots the trace EXPECTS vs how many attached
    // (attachTokens fills the latter). A silent gap misrepresents the evidence.
    screenshots_expected: expected,
    screenshots_attached: 0,
  }

  // The span's usage rides ONE of its item-traces (the lowest itemIndex); the
  // siblings say so explicitly rather than leaving a reader to wonder why they
  // read $0. Computed over the descriptors, which are neither sorted nor
  // guaranteed to include index 0.
  const descriptors = itemDescriptors(scenario)
  const carrierIndex = Math.min(
    ...descriptors.filter(d => d !== undefined).map(d => (d as { itemIndex: number }).itemIndex)
  )

  return descriptors.map(descriptor => {
    // The scenario_item a reviewer grades against — the behavior's GWT + THIS
    // item's singular then_text + the full then-list, OR the intent goal/status.
    let scenarioItem: Record<string, unknown> | undefined
    if (scenario?.kind === 'behavior' && descriptor) {
      scenarioItem = {
        behavior_id: scenario.behaviorId,
        behavior_title: scenario.behaviorTitle,
        given: scenario.given,
        when: scenario.when,
        then_text: descriptor.thenText,
        all_then: scenario.allThen,
      }
    } else if (scenario?.kind === 'intent') {
      scenarioItem = {
        intent_id: scenario.intentId,
        title: scenario.title,
        statement: scenario.statement,
        status: scenario.status,
      }
    }

    const rawInput: Record<string, unknown> = {}
    if (scenarioItem) rawInput.scenario_item = scenarioItem
    // The caveat is DERIVED from `mutation` (D6): present together or not at all.
    // A doctored trace shows the undoctored pixels, so the reviewer needs both.
    if (mutation !== undefined) {
      rawInput.mutation = mutation
      rawInput.screenshot_caveat = SCREENSHOT_CAVEAT
    }
    if (gradedSkeleton.length) rawInput.graded_evidence = gradedSkeleton
    if (rawPrompt !== undefined) rawInput.prompt = rawPrompt

    // THIS item's verdict is the trace's output (no completion re-parse). The
    // no-scenario shape falls back to the raw completion.
    const rawOutput = descriptor
      ? itemVerdicts.find(v => v.itemIndex === descriptor.itemIndex)
      : rawCompletion

    return {
      id: descriptor ? itemTraceId(span, descriptor.itemIndex) : traceIdFor(span),
      name: `judge ${behaviorId}`,
      // Deep-scrub the entire input/output so EVERY nested string is covered
      // (INV-2) — idempotent on the already-scrubbed model/error scalars above.
      input: Object.keys(rawInput).length ? (deep(rawInput) as Record<string, unknown>) : undefined,
      output: rawOutput !== undefined ? deep(rawOutput) : undefined,
      metadata: {
        ...commonMetadata,
        usage_attributed: descriptor === undefined || descriptor.itemIndex === carrierIndex,
      },
    }
  })
}

// PURE: splice the uploaded media tokens onto a skeleton body by INDEX —
// `graded_evidence[n].screenshot = tokens[n]` — and set the honest
// `screenshots_attached` count. Index-based (NOT keyed by capture_file, which has
// no uniqueness invariant: two CAPTUREs can share a basename) so a token never
// lands on the wrong capture (INV-4). Returns a NEW body; never mutates.
export function attachTokens(body: TraceBody, tokens: (string | undefined)[]): TraceBody {
  const input = body.input as Record<string, unknown> | undefined
  const graded = input?.graded_evidence
  if (!Array.isArray(graded)) return body
  let attached = 0
  const nextGraded = graded.map((entry, n) => {
    const tok = tokens[n]
    if (typeof tok === 'string') {
      attached++
      return { ...(entry as Record<string, unknown>), screenshot: tok }
    }
    return entry
  })
  return {
    ...body,
    input: { ...input, graded_evidence: nextGraded },
    metadata: { ...body.metadata, screenshots_attached: attached },
  }
}

// The wire body of the usage-carrying generation observation. Langfuse computes
// cost ONLY on generation/embedding observations — trace `metadata` is opaque
// display-only key/value — so this body is the entire cost path.
export interface GenerationBody {
  id: string
  traceId: string
  name: string
  model?: string
  startTime: string
  endTime: string
  usageDetails: { input: number; input_cached_tokens: number; output: number }
  metadata: Record<string, unknown>
}

// PURE: the generation body for one span, or `undefined` when the span carries no
// usable usage (an error run — an observation with no usage is noise). Reads
// everything off the span itself so it is directly testable without an export.
//
// `toISOString()` throws RangeError on a malformed nano value; that is left to
// throw here and contained at the call site, matching the score step.
export function buildGenerationBody(
  span: GenAiSpan,
  scrub: (s: string) => string = s => s
): GenerationBody | undefined {
  const a = span.attributes
  const inputTokens = num(a['gen_ai.usage.input_tokens'])
  if (inputTokens === undefined) return undefined
  const cached = num(a['gen_ai.usage.cached_input_tokens'])
  const output = num(a['gen_ai.usage.output_tokens'])
  const reasoning = num(a['gen_ai.usage.reasoning_output_tokens'])
  const cacheWrite = num(a['qa.usage.cache_write_input_tokens'])
  const behaviorId = String(a['qa.behavior_id'] ?? 'unknown')
  const rawModel = str(a['gen_ai.request.model'])
  // The SAME scrubbed value the trace metadata ships, so trace and observation
  // can never disagree about which model produced the call.
  const model = rawModel !== undefined ? scrub(rawModel) : undefined

  return {
    id: `obs-${traceIdFor(span)}-gen`,
    traceId: usageTraceId(span),
    name: `judge ${behaviorId}`,
    ...(model !== undefined ? { model } : {}),
    // Span-derived, never the export clock: with no startTime the server falls
    // back to the ingestion envelope, which would stamp a re-export's whole
    // history on the day it was re-exported.
    startTime: new Date(span.start_time_unix_nano / 1e6).toISOString(),
    endTime: new Date(span.end_time_unix_nano / 1e6).toISOString(),
    // EXACTLY these three keys, ALWAYS — never a conditional spread. Langfuse
    // matches bucket names against the model's price keys by exact string
    // equality (a misnamed or missing bucket prices at zero rather than
    // erroring), so a key set that varies with the input makes the priced shape
    // depend on whether the transport happened to report a cached count.
    // `input_cached_tokens: 0` is both true and priced at zero.
    usageDetails: {
      // Cached input is INCLUSIVE of the input count, and Langfuse requires
      // mutually exclusive buckets to derive a correct total — subtract, and
      // clamp so a provider that ever reports exclusive counts cannot ingest a
      // negative bucket.
      input: Math.max(0, inputTokens - (cached ?? 0)),
      input_cached_tokens: cached ?? 0,
      // Reasoning stays INSIDE output: the cheapest judge model has no reasoning
      // price key, so a separate bucket would silently go unpriced.
      output: output ?? 0,
    },
    // Visible for analysis, deliberately unpriced and never double-counted.
    metadata: {
      ...(reasoning !== undefined ? { reasoning_output_tokens: reasoning } : {}),
      ...(cacheWrite !== undefined ? { cache_write_input_tokens: cacheWrite } : {}),
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

// A typed transport error carrying the HTTP status + decoded body, so best-effort
// callers can branch on `.status` and fail-closed callers can surface a precise
// message. Extends Error, so existing `catch (err)` paths that only read `.message`
// are unaffected.
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly body: string,
    readonly method: string,
    readonly path: string
  ) {
    super(`${method} ${path} -> ${status} ${body.slice(0, 200)}`)
    this.name = 'ApiError'
  }
}

// Thrown by `apiGetAllPages` on ANY structural problem while walking a paginated
// list — a stale/replayed page, a stalled cursor, missing/malformed metadata, or an
// item with no id. It is NEVER swallowed into a partial result: silent partial data
// would let setup create resources it failed to see, or a caller misreport coverage.
export class PaginationError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'PaginationError'
  }
}

// `timeoutMs`, when given, bounds the request with an AbortController — a
// never-settling response would otherwise stall the whole export (and, in the
// nightly, the round) indefinitely, which is exactly what the best-effort steps'
// try/catch isolation cannot protect against. Omitting it preserves the previous
// unbounded behavior for existing call sites. An abort surfaces as a rejection,
// which every call site already handles.
export async function api(
  cfg: LangfuseConfig,
  method: string,
  path: string,
  body?: unknown,
  timeoutMs?: number
): Promise<Record<string, unknown>> {
  const controller = timeoutMs !== undefined ? new AbortController() : undefined
  const timer =
    controller !== undefined ? setTimeout(() => controller.abort(), timeoutMs) : undefined
  try {
    const res = await fetch(`${cfg.host}${path}`, {
      method,
      headers: { Authorization: authHeader(cfg), 'Content-Type': 'application/json' },
      body: body ? JSON.stringify(body) : undefined,
      ...(controller !== undefined ? { signal: controller.signal } : {}),
    })
    const text = await res.text()
    if (!res.ok) throw new ApiError(res.status, text, method, path)
    return text ? (JSON.parse(text) as Record<string, unknown>) : {}
  } finally {
    if (timer !== undefined) clearTimeout(timer)
  }
}

// Walk a paginated Langfuse list endpoint to completion and return the accumulated
// items, deduped by resource `id`. The REQUIRED `protocol` selects one of v3's two
// incompatible list styles — there is no reliable way to infer it from a response:
//
//   'page'   — annotation-queues, queue items, score-configs, traces. Sends
//              page+limit; meta is `{page, limit, totalItems, totalPages}`. Advances
//              until `page >= totalPages`.
//   'cursor' — scores v3. Sends/follows `cursor`; meta is `{limit, cursor?}`. The
//              cursor PROPERTY is ABSENT on the terminal page, INCLUDING a one-page
//              result — do NOT mistake a valid meta with no cursor property for
//              malformed. A present cursor (even null) that isn't a non-empty string
//              is malformed.
//
// It MERGES its pagination params into whatever query the caller already put on
// `path` (callers filter by tag/name), never clobbering them. It throws
// `PaginationError` (never returns partial data) on any structural violation, so a
// short read can't masquerade as "resource absent".
//
// `timeoutMs` bounds EACH page request, not the whole walk — the honest semantic
// for a paginated read, and the same per-request bound `api` applies everywhere
// else. A timeout that stopped at `api`'s signature would leave every paginated
// read able to hang forever on page 2.
export async function apiGetAllPages(
  cfg: LangfuseConfig,
  path: string,
  protocol: 'page' | 'cursor',
  timeoutMs?: number
): Promise<Record<string, unknown>[]> {
  const LIMIT = 100
  const qIdx = path.indexOf('?')
  const basePath = qIdx === -1 ? path : path.slice(0, qIdx)
  const baseParams = qIdx === -1 ? '' : path.slice(qIdx + 1)

  const byId = new Map<string, Record<string, unknown>>()
  let requestedPage = 1
  let cursor: string | undefined
  const seenCursors = new Set<string>()

  // Require a schema-mandated integer meta field. A missing / non-integer field means
  // the read is malformed and must fail closed, NOT terminate as if complete.
  const reqInt = (v: unknown, name: string, min: number): number => {
    if (!Number.isInteger(v) || (v as number) < min) {
      throw new PaginationError(`${basePath}: invalid meta.${name} (${String(v)})`)
    }
    return v as number
  }

  while (true) {
    const params = new URLSearchParams(baseParams)
    params.set('limit', String(LIMIT))
    if (protocol === 'page') {
      params.set('page', String(requestedPage))
    } else if (cursor !== undefined) {
      params.set('cursor', cursor)
    }
    const query = params.toString()
    const res: unknown = await api(
      cfg,
      'GET',
      query ? `${basePath}?${query}` : basePath,
      undefined,
      timeoutMs
    )

    // A 2xx `null`/scalar/array body is a malformed envelope — raise PaginationError
    // rather than letting `.data` throw an untyped TypeError.
    if (res === null || typeof res !== 'object' || Array.isArray(res)) {
      throw new PaginationError(`${basePath}: response envelope is not a JSON object`)
    }
    const envelope = res as Record<string, unknown>

    const data = envelope.data
    if (!Array.isArray(data)) {
      throw new PaginationError(`${basePath}: response missing 'data' array`)
    }
    const meta = envelope.meta
    if (meta === null || typeof meta !== 'object' || Array.isArray(meta)) {
      throw new PaginationError(`${basePath}: missing or malformed 'meta' object`)
    }
    const m = meta as Record<string, unknown>

    // Accumulate BEFORE deciding termination, deduped by id — an overlapping page
    // (one repeated id + one new id still makes forward progress) must not leak a
    // phantom duplicate into a caller's uniqueness check. `added` counts the NEW
    // ids this page contributed, so a non-terminal page that adds nothing (a
    // stalled server / fabricated-cursor loop) can be caught below.
    const sizeBefore = byId.size
    for (const item of data) {
      if (item === null || typeof item !== 'object' || Array.isArray(item)) {
        throw new PaginationError(`${basePath}: non-object item in 'data'`)
      }
      const id = (item as Record<string, unknown>).id
      if (typeof id !== 'string' || id.length === 0) {
        throw new PaginationError(`${basePath}: item lacking a valid string 'id'`)
      }
      if (!byId.has(id)) byId.set(id, item as Record<string, unknown>)
    }
    const added = byId.size - sizeBefore

    if (protocol === 'page') {
      // A cursor property here means we got a scores-v3-shaped body under 'page'.
      if ('cursor' in m) {
        throw new PaginationError(`${basePath}: page-mode response carries cursor metadata`)
      }
      // utilsMetaResponse requires ALL of page/limit/totalItems/totalPages as
      // integers — a response missing any of them is malformed, not terminal.
      const page = reqInt(m.page, 'page', 1)
      reqInt(m.limit, 'limit', 1)
      reqInt(m.totalItems, 'totalItems', 0)
      const totalPages = reqInt(m.totalPages, 'totalPages', 0)
      if (page !== requestedPage) {
        throw new PaginationError(
          `${basePath}: requested page ${requestedPage} but got ${String(page)}`
        )
      }
      if (page >= totalPages) break
      // We are about to request another page; if this one added no new ids the
      // list isn't making progress — refuse rather than loop until totalPages.
      if (added === 0) {
        throw new PaginationError(
          `${basePath}: page ${String(page)} added no new items but more pages remain`
        )
      }
      requestedPage += 1
    } else {
      // Reject page metadata — a utilsMetaResponse under 'cursor' is a protocol mix.
      // (`limit` is shared by both metas, so it is NOT a discriminator; page/
      // totalPages/totalItems are page-only.)
      if ('page' in m || 'totalPages' in m || 'totalItems' in m) {
        throw new PaginationError(`${basePath}: cursor-mode response carries page metadata`)
      }
      // GetScoresV3Meta requires an integer `limit`; validate it before trusting an
      // absent cursor as terminal, so an empty/malformed `{}` meta fails closed.
      reqInt(m.limit, 'limit', 1)
      // Terminal state = the cursor PROPERTY is ABSENT (the API omits it when there
      // are no more results, INCLUDING a one-page result). A PRESENT cursor — even
      // `null` — that is not a non-empty string is malformed, never terminal.
      if (!('cursor' in m)) break
      const next = m.cursor
      if (typeof next !== 'string' || next.length === 0) {
        throw new PaginationError(
          `${basePath}: invalid meta.cursor (must be a non-empty string when present)`
        )
      }
      if (seenCursors.has(next)) {
        throw new PaginationError(`${basePath}: cursor did not advance (repeated ${next})`)
      }
      // A fresh cursor that yields no new ids is a fabricated-continuation loop.
      if (added === 0) {
        throw new PaginationError(`${basePath}: continuation cursor but page added no new items`)
      }
      seenCursors.add(next)
      cursor = next
    }
  }

  return [...byId.values()]
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

// The bound on the usage-observation POST. Generous — it is a single small
// request — but finite: an unbounded best-effort step is not best-effort, it is a
// stall that blocks every step after it.
const OBSERVATION_TIMEOUT_MS = 30_000

export interface ExportResult {
  traces: number
  screenshots: number
  failed: number
  // Usage-carrying generation observations shipped. A rejected observation is
  // non-fatal, so it is visible ONLY as a lower count here plus a log line —
  // without surfacing it, a round where every generation was rejected would print
  // a clean summary.
  observations: number
  // Best-effort triage enqueue (INV-A): never affects trace shipping / process exit.
  // `attempted` = POSTs issued (eligible minus already-present); `enqueued` +
  // `failed` partition it; `skippedExisting` = eligible items already in the queue.
  enqueue: {
    attempted: number
    enqueued: number
    skippedExisting: number
    failed: number
  }
}

type Verdict = 'pass' | 'fail' | 'unsure'

const errMsg = (e: unknown): string => (e instanceof Error ? e.message : String(e))

// PURE: the rejection reason in an ingestion response, or `undefined` when the event
// was accepted. `/api/public/ingestion` is MULTI-STATUS — a 2xx body carries
// `successes` and `errors` arrays, so an HTTP success does NOT mean the event was
// stored, and a resolved `api()` call is not evidence of anything.
//
// EVERY event this exporter posts rides a batch of exactly ONE, so ANY entry in
// `errors` is our event's rejection — including an entry whose id we do not
// recognize. Skipping an unattributable entry would fail OPEN: the one shape where
// we are least sure what happened is the one where we would claim success. An
// absent/empty `errors` array is acceptance; that is the documented success shape,
// not an unknown.
export function ingestionRejection(
  res: Record<string, unknown>,
  eventId: string
): string | undefined {
  const errors = res.errors
  if (!Array.isArray(errors) || errors.length === 0) return undefined
  // Prefer the entry naming our event; otherwise report the first, flagging that the
  // envelope attributed it elsewhere (still a rejection — the batch had one event).
  const rows = errors.filter(
    (e): e is Record<string, unknown> => e !== null && typeof e === 'object'
  )
  if (rows.length === 0) return `ingestion reported ${errors.length} unreadable error entry/entries`
  const mine = rows.find(r => r.id === eventId)
  const row = mine ?? rows[0]
  const status = row.status !== undefined ? `status ${String(row.status)}` : undefined
  const message = typeof row.message === 'string' ? row.message : undefined
  const attribution =
    mine === undefined && typeof row.id === 'string' && row.id !== eventId
      ? `(reported against ${row.id}, not ${eventId})`
      : undefined
  return (
    [status, message, attribution].filter(Boolean).join(': ') || JSON.stringify(row).slice(0, 200)
  )
}

// Post ONE ingestion event and treat a per-event rejection as a failure, not a
// success. Every ingestion POST in this file goes through here: inferring success
// from a resolved `api()` call is the defect, and it is identical at all four sites
// (init trace-create, final trace-create, score-create, generation-create). Callers
// keep their own failure policy — the fatal ones let it throw, the best-effort ones
// catch and log.
async function postIngestionEvent(
  cfg: LangfuseConfig,
  event: Record<string, unknown>,
  timeoutMs?: number
): Promise<void> {
  const res = await api(cfg, 'POST', '/api/public/ingestion', { batch: [event] }, timeoutMs)
  const rejection = ingestionRejection(res, String(event.id))
  if (rejection !== undefined) {
    throw new Error(`ingestion rejected ${String(event.id)}: ${rejection}`)
  }
}

// The per-item-trace verdict IS that trace's `output` (a PerItemVerdict). The enum
// value is identifier-valued and survives the deep PII scrub untouched, so reading it
// back off the built body needs no re-derivation from the span. Non-graded shapes
// (metrics-only / no-scenario completion) have a string/undefined output → no verdict.
function bodyVerdict(body: TraceBody): Verdict | undefined {
  const out = body.output
  if (out !== null && typeof out === 'object' && !Array.isArray(out)) {
    const v = (out as { verdict?: unknown }).verdict
    if (v === 'pass' || v === 'fail' || v === 'unsure') return v
  }
  return undefined
}

// A STABLE per-round sample of PASS trace ids for the triage salt. Deterministic by
// construction (no RNG): hash each id under the round's `runId` with a NUL separator,
// sort by (digest, traceId), take the first n. Re-exporting the same round selects the
// SAME passes, so it never inflates the queue or biases the recall rate. `runId` is
// '' when provenance is absent — still deterministic, just keyed on the id alone.
// Order-independent: a permuted input yields the same selection.
export function stableSample(passIds: string[], n: number, runId: string): string[] {
  if (n <= 0 || passIds.length === 0) return []
  const keyed = passIds.map(id => ({
    id,
    digest: crypto.createHash('sha256').update(`${runId}\0${id}`).digest('hex'),
  }))
  keyed.sort((a, b) =>
    a.digest < b.digest ? -1 : a.digest > b.digest ? 1 : a.id < b.id ? -1 : a.id > b.id ? 1 : 0
  )
  return keyed.slice(0, Math.min(n, keyed.length)).map(k => k.id)
}

// One item-trace's triage inputs, collected during the ship loop and consumed by the
// enqueue pass after every trace has shipped (INV-B: a trap-miss trace still enqueues).
interface EnqueueItem {
  traceId: string
  verdict?: Verdict
  isMutation: boolean
}

// Validated provenance + salt config, applied at export time (contract #3). All
// values are pre-validated by the caller (run.ts); `exportSpans` never re-parses.
export interface ExportOptions {
  runId?: string
  gitSha?: string
  saltPasses?: number
  // Test seam: the per-request bound on the usage-observation POST. Defaults to
  // OBSERVATION_TIMEOUT_MS; a test proving the bound exists must not wait 30s for it.
  observationTimeoutMs?: number
}

export async function exportSpans(
  cfg: LangfuseConfig,
  spans: GenAiSpan[],
  log: (msg: string) => void = () => {},
  opts: ExportOptions = {}
): Promise<ExportResult> {
  const result: ExportResult = {
    traces: 0,
    screenshots: 0,
    failed: 0,
    observations: 0,
    enqueue: { attempted: 0, enqueued: 0, skippedExisting: 0, failed: 0 },
  }

  // One scrubber for the whole export so the same email/phone maps to the same
  // placeholder across every span in this run (arc INV-2).
  const scrubber = createScrubber()
  const scrub = (s: string): string => scrubber.scrub(s)

  // Langfuse ingestion dedups by EVENT id, so a re-export must carry FRESH event
  // ids (else it is silently dropped) while trace ids stay STABLE (so it
  // OVERWRITES). One nonce per submission gives both (INV-6).
  const nonce = crypto.randomUUID()

  // The verdict score binds to its config by id. Resolve LAZILY, ONCE, and GUARDED:
  // a resolve failure (or a zero/multiple-match) degrades scores to UNBOUND and must
  // NEVER abort trace shipping. Cached across the run.
  let configResolved = false
  let verdictConfigId: string | undefined
  const resolveVerdictConfigId = async (): Promise<string | undefined> => {
    if (configResolved) return verdictConfigId
    configResolved = true
    try {
      const configs = await apiGetAllPages(cfg, '/api/public/score-configs', 'page')
      const active = configs.filter(
        c => c.name === VERDICT_SCORE_NAME && (c as { isArchived?: unknown }).isArchived === false
      )
      if (active.length === 1 && typeof active[0].id === 'string') {
        verdictConfigId = active[0].id as string
      } else {
        log(
          `  verdict score-config: expected exactly 1 active '${VERDICT_SCORE_NAME}', ` +
            `found ${active.length} — scores emitted UNBOUND`
        )
      }
    } catch (err) {
      log(`  verdict score-config resolve failed (${errMsg(err)}) — scores emitted UNBOUND`)
    }
    return verdictConfigId
  }

  // Per-item-trace triage inputs, drained by the enqueue pass after the ship loop.
  const enqueueItems: EnqueueItem[] = []

  for (const span of spans) {
    // Mutation is a SPAN-level property (`qa.mutation`): every item-trace fanned out
    // of a doctored span is a trap trace (INV-B — always an enqueue candidate).
    const isMutation = span.attributes['qa.mutation'] !== undefined
    const behaviorId = String(span.attributes['qa.behavior_id'] ?? 'unknown')
    // The per-index screenshot paths, from the span's graded_evidence — the
    // reviewer sees each token attributed to its OWN capture, so registration is
    // per index (undefined at indices with no screenshot).
    const spanGraded = Array.isArray(span.attributes['qa.graded_evidence'])
      ? (span.attributes['qa.graded_evidence'] as GradedEvidenceEntry[])
      : []
    const screenshotPaths = spanGraded.map(e => e.screenshot)
    const expected = screenshotPaths.filter((p): p is string => typeof p === 'string').length

    // The pass + model tag dimensions are SPAN-level, so they are derived here —
    // the per-body loop below has neither value in scope. `model` is the same
    // scrubbed value the trace metadata ships, deliberately including the empty
    // string: a misconfigured harness that sets the model to '' must show a bare
    // `model:` tag rather than look like a run with no model dimension at all.
    const scenario = (span.attributes['qa.scenario'] as Scenario | undefined) ?? undefined
    const rawSpanModel = str(span.attributes['gen_ai.request.model'])
    const spanModel = rawSpanModel !== undefined ? scrub(rawSpanModel) : undefined

    // The item-trace that will carry this span's usage observation, and whether it
    // actually shipped. A body failure is caught INSIDE the loop below, so without
    // this the observation could reference a trace that never landed.
    const carrierId = usageTraceId(span)
    let carrierShipped = false

    // ONE trace per graded item (spec line 41). Each item-trace runs its OWN
    // media lifecycle against its OWN trace id: `uploadMedia` REGISTERS against a
    // specific traceId, so a registration for item-trace 0 does not exist for
    // item-trace 1. Bytes dedup server-side by sha — the first trace PUTs, later
    // ones HIT and get the same token value cheaply.
    for (const body of buildTraceBody(span, scrub)) {
      try {
        // (1) The trace must exist before media can be registered against it.
        await postIngestionEvent(cfg, {
          id: `evt-${body.id}-init-${nonce}`,
          type: 'trace-create',
          timestamp: new Date().toISOString(),
          body: { id: body.id, name: body.name },
        })

        // (2) Register/upload each expected screenshot against THIS trace's id,
        // by index. All-or-nothing PER TRACE (INV-4): if ANY expected token is
        // missing, this trace ships ZERO tokens (honest expected=N/attached=0)
        // rather than mis-attributing a partial set; sibling traces are
        // independent. A registration/upload THROW (e.g. a non-2xx POST /media)
        // must NOT escape and drop the trace — it is caught per screenshot and
        // collapses THIS trace to zero tokens, so the final body below still
        // ships with an honest attached=0. Only an init/final-body failure (the
        // outer catch) can drop a trace.
        const tokens: (string | undefined)[] = []
        let missing = false
        for (const p of screenshotPaths) {
          if (typeof p !== 'string') {
            tokens.push(undefined)
            continue
          }
          try {
            const tok = await uploadMedia(cfg, body.id, p)
            if (!tok) missing = true
            tokens.push(tok)
          } catch (err) {
            missing = true
            tokens.push(undefined)
            log(
              `  media register failed for ${body.id}: ${err instanceof Error ? err.message : String(err)}`
            )
          }
        }
        const finalTokens = missing ? [] : tokens
        const attached = finalTokens.filter((t): t is string => typeof t === 'string').length
        result.screenshots += attached

        // (3) The final body event carries the tokens attached by index, plus the
        // trace tags + session. The `behavior:` tag is ALWAYS emitted (the downstream
        // backfill CLI's lookup key — it comes from the span, not env); `runId:`/
        // `gitSha:` tags + `sessionId` ride only the valid components (contract #3).
        // All values are identifier/enum by construction (INV-D).
        const finalBody = attachTokens(body, finalTokens)
        const tags = [`behavior:${behaviorId}`]
        if (opts.runId) tags.push(`runId:${opts.runId}`)
        if (opts.gitSha) tags.push(`gitSha:${opts.gitSha}`)
        // `pass:` is the only place the ux/intent dimension exists — the two are
        // otherwise indistinguishable by name or tag. Suppressed for a
        // metrics-only span, which genuinely has no pass dimension.
        if (scenario) tags.push(`pass:${scenario.kind === 'intent' ? 'intent' : 'ux'}`)
        if (spanModel !== undefined) tags.push(`model:${spanModel}`)
        const wireBody: Record<string, unknown> = { ...finalBody, tags }
        if (opts.runId) wireBody.sessionId = opts.runId
        // A per-event rejection here throws, so `result.traces` and `carrierShipped`
        // are never set for a body that did not land — the generation below must
        // not be emitted against a trace whose full body was refused.
        await postIngestionEvent(cfg, {
          id: `evt-${body.id}-${nonce}`,
          type: 'trace-create',
          timestamp: new Date().toISOString(),
          body: wireBody,
        })
        result.traces++
        if (body.id === carrierId) carrierShipped = true
        log(`  ${behaviorId} → ${body.id} (media ${attached}/${expected})`)

        // (4) Verdict-as-score: a SEPARATE, non-fatal ingestion request AFTER the
        // trace shipped (contract #4). NOT co-batched — a rejecting score must not
        // couple/fail the trace ship (INV-A). Metrics-only shapes have no verdict → no
        // score. The ENTIRE score step is inside its own try/catch (including the
        // span-start timestamp conversion) so NOTHING here — not a malformed
        // start_time_unix_nano, not a resolve/ingest failure — can abort the already-
        // shipped trace or the enqueue collection below.
        const verdict = bodyVerdict(finalBody)
        if (verdict) {
          try {
            // Span-derived ENVELOPE timestamp so a re-export on a later UTC date does
            // not duplicate the score. A malformed/out-of-range start_time_unix_nano
            // makes toISOString() throw RangeError — caught here, degrading to a
            // skipped score, never a dropped trace.
            const spanStartIso = new Date(span.start_time_unix_nano / 1e6).toISOString()
            const configId = await resolveVerdictConfigId()
            const scoreBody: Record<string, unknown> = {
              id: `score-${body.id}-verdict`,
              name: VERDICT_SCORE_NAME,
              value: verdict,
              dataType: 'CATEGORICAL',
              traceId: body.id,
            }
            if (configId) scoreBody.configId = configId
            await postIngestionEvent(cfg, {
              id: `evt-${body.id}-score-${nonce}`,
              type: 'score-create',
              timestamp: spanStartIso,
              body: scoreBody,
            })
          } catch (err) {
            log(`  score-create failed for ${body.id}: ${errMsg(err)}`)
          }
        }

        // Collect for the post-loop enqueue pass (runs even on a trap miss — INV-B).
        enqueueItems.push({ traceId: body.id, verdict, isMutation })
      } catch (err) {
        result.failed++
        log(`  FAILED ${behaviorId}: ${err instanceof Error ? err.message : String(err)}`)
      }
    }

    // (5) Usage-as-generation: ONE observation per SPAN — Langfuse computes cost
    // only on generation observations — carried by the span's lowest-itemIndex
    // trace, and emitted ONLY IF that trace actually shipped (an observation
    // whose trace never landed is dangling). A SEPARATE, non-fatal ingestion
    // request, never co-batched, so a rejected observation can never couple to or
    // drop an already-shipped trace. The ENTIRE step — including the span-derived
    // timestamp conversion, which throws RangeError on a malformed nano value —
    // sits in its own try/catch: it degrades to a skipped observation, never a
    // dropped trace and never a suppressed enqueue pass.
    if (carrierShipped) {
      try {
        const genBody = buildGenerationBody(span, scrub)
        if (genBody) {
          // A per-event rejection throws, so the count reflects ACCEPTED
          // observations — counting the POST would report a shipped observation
          // that was never stored.
          await postIngestionEvent(
            cfg,
            {
              id: `evt-${traceIdFor(span)}-gen-${nonce}`,
              type: 'generation-create',
              timestamp: genBody.startTime,
              body: genBody,
            },
            opts.observationTimeoutMs ?? OBSERVATION_TIMEOUT_MS
          )
          result.observations++
        }
      } catch (err) {
        log(`  generation-create failed for ${traceIdFor(span)}: ${errMsg(err)}`)
      }
    }
  }

  // After every trace has shipped, route fails/traps/salt into the triage queue.
  // Wholly best-effort: a resolve/list failure or a per-item POST failure logs and
  // continues, never touching `result.traces`/`failed` or the process exit (INV-A).
  await runEnqueue(cfg, enqueueItems, result, opts, log)

  return result
}

// The triage enqueue pass (contract #4, INV-A/B/F). Idempotent + single-writer:
// resolve the standing queue, read its existing items ONCE, and POST only the
// eligible items not already present, each in its own try/catch. Eligibility is an
// EXPLICIT closed union — never a `verdict !== 'pass'` negation, which would admit a
// verdictless (metrics-only) trace via `undefined !== 'pass'`.
async function runEnqueue(
  cfg: LangfuseConfig,
  items: EnqueueItem[],
  result: ExportResult,
  opts: ExportOptions,
  log: (msg: string) => void
): Promise<void> {
  try {
    const queues = await apiGetAllPages(cfg, '/api/public/annotation-queues', 'page')
    const matches = queues.filter(q => q.name === TRIAGE_QUEUE_NAME)
    if (matches.length !== 1 || typeof matches[0].id !== 'string') {
      log(
        `  enqueue: expected exactly 1 '${TRIAGE_QUEUE_NAME}' queue, ` +
          `found ${matches.length} — enqueue skipped`
      )
      return
    }
    const queueId = matches[0].id as string

    // Existing items → already-present set keyed by (objectType, objectId): the API
    // identity includes objectType, so a same-id OBSERVATION item must NOT suppress a
    // TRACE enqueue. Annotation-queue POST has no server dedup — this is the only guard
    // against re-export duplication (safe: export is single-writer per round).
    const existing = await apiGetAllPages(
      cfg,
      `/api/public/annotation-queues/${queueId}/items`,
      'page'
    )
    const present = new Set<string>()
    const key = (objectType: string, objectId: string): string => `${objectType}\0${objectId}`
    for (const it of existing) {
      const objectType = (it as { objectType?: unknown }).objectType
      const objectId = (it as { objectId?: unknown }).objectId
      if (typeof objectType === 'string' && typeof objectId === 'string') {
        present.add(key(objectType, objectId))
      }
    }

    // Salt pool = non-mutation PASS trace ids; a STABLE per-round sample of them.
    const passIds = items.filter(i => !i.isMutation && i.verdict === 'pass').map(i => i.traceId)
    const n = opts.saltPasses ?? 3
    const salted = new Set(stableSample(passIds, n, opts.runId ?? ''))

    let failUnsure = 0
    let traps = 0
    let salt = 0
    const eligible = items.filter(i => {
      if (i.isMutation) {
        traps++
        return true
      }
      if (i.verdict === 'fail' || i.verdict === 'unsure') {
        failUnsure++
        return true
      }
      if (i.verdict === 'pass' && salted.has(i.traceId)) {
        salt++
        return true
      }
      return false
    })

    for (const item of eligible) {
      const k = key('TRACE', item.traceId)
      if (present.has(k)) {
        result.enqueue.skippedExisting++
        continue
      }
      result.enqueue.attempted++
      try {
        await api(cfg, 'POST', `/api/public/annotation-queues/${queueId}/items`, {
          objectId: item.traceId,
          objectType: 'TRACE',
        })
        result.enqueue.enqueued++
        present.add(k) // guard against a duplicate id within this same batch
      } catch (err) {
        result.enqueue.failed++
        log(`  enqueue failed for ${item.traceId}: ${errMsg(err)}`)
      }
    }
    log(
      `  enqueued ${result.enqueue.enqueued}/${result.enqueue.attempted} ` +
        `(${failUnsure} fail/unsure, ${traps} trap, ${salt} salt; ` +
        `${result.enqueue.skippedExisting} skipped-existing; ${result.enqueue.failed} failed)`
    )
  } catch (err) {
    log(`  enqueue skipped: ${errMsg(err)}`)
  }
}
