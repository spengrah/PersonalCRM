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
    output_tokens: num(a['gen_ai.usage.output_tokens']),
    tool_rejected: a['qa.tool_rejected'],
    status: span.status.code,
    error,
    duration_ms: Math.round((span.end_time_unix_nano - span.start_time_unix_nano) / 1e6),
    // Honesty: how many screenshots the trace EXPECTS vs how many attached
    // (attachTokens fills the latter). A silent gap misrepresents the evidence.
    screenshots_expected: expected,
    screenshots_attached: 0,
  }

  return itemDescriptors(scenario).map(descriptor => {
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
      metadata: { ...commonMetadata },
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

export async function api(
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
  if (!res.ok) throw new ApiError(res.status, text, method, path)
  return text ? (JSON.parse(text) as Record<string, unknown>) : {}
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
export async function apiGetAllPages(
  cfg: LangfuseConfig,
  path: string,
  protocol: 'page' | 'cursor'
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

  // eslint-disable-next-line no-constant-condition
  while (true) {
    const params = new URLSearchParams(baseParams)
    params.set('limit', String(LIMIT))
    if (protocol === 'page') {
      params.set('page', String(requestedPage))
    } else if (cursor !== undefined) {
      params.set('cursor', cursor)
    }
    const query = params.toString()
    const res: unknown = await api(cfg, 'GET', query ? `${basePath}?${query}` : basePath)

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

  // One scrubber for the whole export so the same email/phone maps to the same
  // placeholder across every span in this run (arc INV-2).
  const scrubber = createScrubber()
  const scrub = (s: string): string => scrubber.scrub(s)

  // Langfuse ingestion dedups by EVENT id, so a re-export must carry FRESH event
  // ids (else it is silently dropped) while trace ids stay STABLE (so it
  // OVERWRITES). One nonce per submission gives both (INV-6).
  const nonce = crypto.randomUUID()

  for (const span of spans) {
    const behaviorId = String(span.attributes['qa.behavior_id'] ?? 'unknown')
    // The per-index screenshot paths, from the span's graded_evidence — the
    // reviewer sees each token attributed to its OWN capture, so registration is
    // per index (undefined at indices with no screenshot).
    const spanGraded = Array.isArray(span.attributes['qa.graded_evidence'])
      ? (span.attributes['qa.graded_evidence'] as GradedEvidenceEntry[])
      : []
    const screenshotPaths = spanGraded.map(e => e.screenshot)
    const expected = screenshotPaths.filter((p): p is string => typeof p === 'string').length

    // ONE trace per graded item (spec line 41). Each item-trace runs its OWN
    // media lifecycle against its OWN trace id: `uploadMedia` REGISTERS against a
    // specific traceId, so a registration for item-trace 0 does not exist for
    // item-trace 1. Bytes dedup server-side by sha — the first trace PUTs, later
    // ones HIT and get the same token value cheaply.
    for (const body of buildTraceBody(span, scrub)) {
      try {
        // (1) The trace must exist before media can be registered against it.
        await api(cfg, 'POST', '/api/public/ingestion', {
          batch: [
            {
              id: `evt-${body.id}-init-${nonce}`,
              type: 'trace-create',
              timestamp: new Date().toISOString(),
              body: { id: body.id, name: body.name },
            },
          ],
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

        // (3) The final body event carries the tokens attached by index.
        await api(cfg, 'POST', '/api/public/ingestion', {
          batch: [
            {
              id: `evt-${body.id}-${nonce}`,
              type: 'trace-create',
              timestamp: new Date().toISOString(),
              body: attachTokens(body, finalTokens),
            },
          ],
        })
        result.traces++
        log(`  ${behaviorId} → ${body.id} (media ${attached}/${expected})`)
      } catch (err) {
        result.failed++
        log(`  FAILED ${behaviorId}: ${err instanceof Error ? err.message : String(err)}`)
      }
    }
  }
  return result
}
