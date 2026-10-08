// The one fake Langfuse the exporter tests talk to. It answers every route
// `make qa-export` uses — OTLP/JSON traces, legacy ingestion (scores only), media,
// score-configs and annotation queues — and records what was transported, decoded,
// so a test asserts on the wire rather than on the exporter's internals.
//
// Two front doors over ONE handler: `fetchImpl` for an in-process
// `vi.stubGlobal('fetch', …)`, and `listen()` for a real loopback HTTP server (the
// command-level tests and the `make qa-export` subprocess). Test support only:
// nothing in the exporter imports it.

import * as http from 'http'
import type { AddressInfo } from 'net'
import type { LangfuseConfig } from './langfuse'
import { TRIAGE_QUEUE_NAME, VERDICT_SCORE_NAME } from './triage-config'

export const OTLP_PATH = '/api/public/otel/v1/traces'

// One span as the fake decoded it off an OTLP/JSON request. `attributes` maps each
// key to its decoded AnyValue (a string, a boolean, a number, or an array of them).
export interface DecodedSpan {
  traceId: string
  spanId: string
  parentSpanId?: string
  name: string
  startTimeUnixNano: string
  endTimeUnixNano: string
  attributes: Record<string, unknown>
}

export interface OtlpRequest {
  headers: Record<string, string>
  scopeName?: string
  spans: DecodedSpan[]
}

// A root span, with the legacy trace body's fields read back off its attributes.
// `metadata` values are decoded the way the exporter encodes them: a JSON-parseable
// string becomes its value, any other string stays as sent.
export interface ShippedRoot extends DecodedSpan {
  labelId: string
  input?: Record<string, unknown>
  output?: unknown
  metadata: Record<string, unknown>
  tags?: string[]
  sessionId?: string
}

// A generation span. `labelId` is its carrier trace's string id (a trace-level
// attribute, so it rides the generation too).
export interface ShippedGeneration extends DecodedSpan {
  labelId: string
  model?: string
  usageDetails: unknown
  metadata: Record<string, unknown>
}

export interface ScoreEvent {
  id: string
  type: string
  timestamp: string
  batchLength: number
  body: {
    id: string
    name: string
    value: string
    dataType: string
    traceId: string
    configId?: string
  }
}

export interface QueueItem {
  id: string
  objectId: string
  objectType: string
  status?: string
}

export interface ItemPost {
  queueId: string
  objectId: string
  objectType: string
}

export interface ScoreConfigObj {
  id: string
  name: string
  isArchived: boolean
  dataType?: string
}

export interface QueueObj {
  id: string
  name: string
  scoreConfigIds?: string[]
}

// Every request in arrival order. `body` is the parsed JSON for the JSON routes and
// undefined for the binary upload.
export interface RecordedRequest {
  method: string
  path: string
  query: URLSearchParams
  headers: Record<string, string>
  body?: unknown
}

export const VERDICT_CONFIG_ID = 'cfg-verdict'
export const TRIAGE_QUEUE_ID = 'q-triage'

export interface FakeOpts {
  // Triage substrate (defaults model a correctly-provisioned tenant).
  scoreConfigs?: ScoreConfigObj[]
  configError?: boolean
  queues?: QueueObj[]
  queueError?: boolean
  existingItems?: QueueItem[]
  itemsError?: boolean
  // A structurally malformed list envelope → apiGetAllPages throws PaginationError.
  queueMalformed?: boolean
  itemsMalformed?: boolean
  failEnqueue?: (objectId: string, n: number) => boolean
  configsPerPage?: number
  queuesPerPage?: number
  itemsPerPage?: number
  // Media.
  failFirstPut?: boolean
  failFirstRegister?: boolean
  // Scores (legacy ingestion).
  scoreError?: boolean
  rejectScore?: boolean
  ingestionEnvelope?: 'documented' | 'bare'
  // OTLP root spans, selected by the trace's string id: an HTTP 500, or an HTTP 200
  // whose `partialSuccess` rejects the span.
  failRootFor?: (labelId: string) => boolean
  rejectRootFor?: (labelId: string) => boolean
  // OTLP generation spans: an HTTP error status, a `partialSuccess` rejection, or a
  // request that never settles (rejecting only when the caller aborts it).
  generationStatus?: number
  generationRejected?: boolean
  generationNeverSettles?: boolean
  generationNeverSettlesWhen?: () => boolean
}

export interface FakeLangfuse {
  fetchImpl: typeof fetch
  listen(): Promise<LangfuseConfig>
  close(): Promise<void>
  requests: RecordedRequest[]
  otlpRequests: OtlpRequest[]
  roots: ShippedRoot[]
  generations: ShippedGeneration[]
  scores: ScoreEvent[]
  itemPosts: ItemPost[]
  // Ingestion/OTLP chronology per trace (hex id), so a test can prove the score for a
  // trace is sent after its root, and the generation after every root of its span.
  order: Array<{ kind: 'media' | 'root' | 'score' | 'generation'; traceId: string }>
  counts: { configResolve: number }
}

const json = (obj: unknown, status = 200): Response =>
  new Response(JSON.stringify(obj), {
    status,
    headers: { 'content-type': 'application/json' },
  })
const errText = (status: number, msg: string): Response => new Response(msg, { status })

// Decode one OTLP AnyValue. Unknown kinds throw, so a malformed attribute fails the
// request (400) instead of decoding to something a test would trust.
function decodeAnyValue(v: unknown): unknown {
  if (v === null || typeof v !== 'object') throw new Error(`AnyValue is not an object`)
  const o = v as Record<string, unknown>
  if (typeof o.stringValue === 'string') return o.stringValue
  if (typeof o.boolValue === 'boolean') return o.boolValue
  if (typeof o.doubleValue === 'number') return o.doubleValue
  if (typeof o.intValue === 'string' || typeof o.intValue === 'number') return Number(o.intValue)
  if (o.arrayValue !== null && typeof o.arrayValue === 'object') {
    const values = (o.arrayValue as { values?: unknown }).values
    if (!Array.isArray(values)) throw new Error('arrayValue without values')
    return values.map(decodeAnyValue)
  }
  throw new Error(`unsupported AnyValue ${JSON.stringify(v)}`)
}

const HEX32 = /^[0-9a-f]{32}$/
const HEX16 = /^[0-9a-f]{16}$/
const NANOS = /^[0-9]+$/

// Decode an OTLP/JSON ExportTraceServiceRequest, validating the shape a real
// receiver needs: hex trace/span ids, decimal-string nano times, key/value attributes.
function decodeOtlp(body: unknown): { scopeName?: string; spans: DecodedSpan[] } {
  const rs = (body as { resourceSpans?: unknown }).resourceSpans
  if (!Array.isArray(rs)) throw new Error('resourceSpans is not an array')
  const spans: DecodedSpan[] = []
  let scopeName: string | undefined
  for (const r of rs) {
    const scopeSpans = (r as { scopeSpans?: unknown }).scopeSpans
    if (!Array.isArray(scopeSpans)) throw new Error('scopeSpans is not an array')
    for (const ss of scopeSpans) {
      const scope = (ss as { scope?: { name?: unknown } }).scope
      if (typeof scope?.name === 'string') scopeName = scope.name
      const list = (ss as { spans?: unknown }).spans
      if (!Array.isArray(list)) throw new Error('spans is not an array')
      for (const s of list) {
        const sp = s as Record<string, unknown>
        if (typeof sp.traceId !== 'string' || !HEX32.test(sp.traceId))
          throw new Error(`traceId is not 32 lowercase hex: ${String(sp.traceId)}`)
        if (typeof sp.spanId !== 'string' || !HEX16.test(sp.spanId))
          throw new Error(`spanId is not 16 lowercase hex: ${String(sp.spanId)}`)
        if (sp.parentSpanId !== undefined && !HEX16.test(String(sp.parentSpanId)))
          throw new Error(`parentSpanId is not 16 lowercase hex: ${String(sp.parentSpanId)}`)
        if (typeof sp.name !== 'string') throw new Error('span name missing')
        for (const k of ['startTimeUnixNano', 'endTimeUnixNano'] as const) {
          if (typeof sp[k] !== 'string' || !NANOS.test(sp[k] as string))
            throw new Error(`${k} is not a decimal string: ${String(sp[k])}`)
        }
        if (!Array.isArray(sp.attributes)) throw new Error('attributes is not an array')
        const attributes: Record<string, unknown> = {}
        for (const a of sp.attributes as Array<{ key?: unknown; value?: unknown }>) {
          if (typeof a.key !== 'string') throw new Error('attribute key missing')
          if (a.key in attributes) throw new Error(`duplicate attribute ${a.key}`)
          attributes[a.key] = decodeAnyValue(a.value)
        }
        spans.push({
          traceId: sp.traceId,
          spanId: sp.spanId,
          ...(sp.parentSpanId !== undefined ? { parentSpanId: String(sp.parentSpanId) } : {}),
          name: sp.name,
          startTimeUnixNano: sp.startTimeUnixNano as string,
          endTimeUnixNano: sp.endTimeUnixNano as string,
          attributes,
        })
      }
    }
  }
  return { scopeName, spans }
}

const lenient = (v: unknown): unknown => {
  if (typeof v !== 'string') return v
  try {
    return JSON.parse(v)
  } catch {
    return v
  }
}

// The `<prefix><key>` attributes of a span, prefix stripped, values decoded leniently.
export function prefixed(span: DecodedSpan, prefix: string): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const [k, v] of Object.entries(span.attributes)) {
    if (k.startsWith(prefix)) out[k.slice(prefix.length)] = lenient(v)
  }
  return out
}

function rootView(span: DecodedSpan): ShippedRoot {
  const a = span.attributes
  const input = a['langfuse.observation.input']
  const output = a['langfuse.observation.output']
  return {
    ...span,
    labelId: String(a['langfuse.trace.metadata.label_id']),
    ...(input !== undefined ? { input: JSON.parse(String(input)) as Record<string, unknown> } : {}),
    ...(output !== undefined ? { output: JSON.parse(String(output)) } : {}),
    metadata: prefixed(span, 'langfuse.trace.metadata.'),
    ...(a['langfuse.trace.tags'] !== undefined
      ? { tags: a['langfuse.trace.tags'] as string[] }
      : {}),
    ...(a['langfuse.session.id'] !== undefined
      ? { sessionId: String(a['langfuse.session.id']) }
      : {}),
  }
}

function generationView(span: DecodedSpan): ShippedGeneration {
  const a = span.attributes
  const usage = a['langfuse.observation.usage_details']
  return {
    ...span,
    labelId: String(a['langfuse.trace.metadata.label_id']),
    ...(a['langfuse.observation.model.name'] !== undefined
      ? { model: String(a['langfuse.observation.model.name']) }
      : {}),
    usageDetails: usage !== undefined ? JSON.parse(String(usage)) : undefined,
    metadata: prefixed(span, 'langfuse.observation.metadata.'),
  }
}

const lowerKeys = (h: unknown): Record<string, string> => {
  const out: Record<string, string> = {}
  if (h === undefined || h === null) return out
  const entries =
    h instanceof Headers ? [...h.entries()] : Object.entries(h as Record<string, unknown>)
  for (const [k, v] of entries) {
    if (v !== undefined) out[k.toLowerCase()] = Array.isArray(v) ? v.join(', ') : String(v)
  }
  return out
}

export function createFakeLangfuse(opts: FakeOpts = {}): FakeLangfuse {
  const requests: RecordedRequest[] = []
  const otlpRequests: OtlpRequest[] = []
  const roots: ShippedRoot[] = []
  const generations: ShippedGeneration[] = []
  const scores: ScoreEvent[] = []
  const itemPosts: ItemPost[] = []
  const order: FakeLangfuse['order'] = []
  const counts = { configResolve: 0 }
  const shaToId = new Map<string, string>()
  let seq = 0
  let putCount = 0
  let registerCount = 0
  let enqueueCount = 0

  const activeVerdictConfig: ScoreConfigObj = {
    id: VERDICT_CONFIG_ID,
    name: VERDICT_SCORE_NAME,
    isArchived: false,
    dataType: 'CATEGORICAL',
  }
  const triageQueue: QueueObj = {
    id: TRIAGE_QUEUE_ID,
    name: TRIAGE_QUEUE_NAME,
    scoreConfigIds: [],
  }

  // The ingestion success envelopes. 'documented' names each event (strict
  // confirmation); 'bare' is a 2xx that confirms nothing (the degrade-and-warn tier).
  const accepted = (eventId: string): Response =>
    opts.ingestionEnvelope === 'bare'
      ? json({})
      : json({ successes: [{ id: eventId, status: 201 }], errors: [] })

  // A valid v3 page-protocol envelope over `all`, sliced to the requested page.
  const page = (all: unknown[], q: URLSearchParams, per: number): Response => {
    const requested = Number(q.get('page') ?? '1')
    const totalPages = Math.max(1, Math.ceil(all.length / per))
    const start = (requested - 1) * per
    return json({
      data: all.slice(start, start + per),
      meta: { page: requested, limit: per, totalItems: all.length, totalPages },
    })
  }

  // Never settles on its own; rejects only if the caller aborts it — exactly how a
  // real fetch behaves, so a test proves the exporter's bound, not the fake's.
  const hang = (signal?: AbortSignal | null): Promise<Response> =>
    new Promise<Response>((_resolve, reject) => {
      signal?.addEventListener('abort', () => {
        const e = new Error('The operation was aborted')
        e.name = 'AbortError'
        reject(e)
      })
    })

  async function handle(
    url: string,
    method: string,
    headers: Record<string, string>,
    rawBody: string | undefined,
    signal?: AbortSignal | null
  ): Promise<Response> {
    const parsed = new URL(url)
    const pathname = parsed.pathname
    const q = parsed.searchParams
    const isUpload = pathname.startsWith('/__upload/')
    let body: unknown
    if (!isUpload && rawBody !== undefined && rawBody !== '') {
      try {
        body = JSON.parse(rawBody)
      } catch {
        return errText(400, 'invalid JSON body')
      }
    }
    requests.push({ method, path: pathname, query: q, headers, body })

    if (pathname === OTLP_PATH && method === 'POST') {
      let decoded: ReturnType<typeof decodeOtlp>
      try {
        decoded = decodeOtlp(body)
      } catch (e) {
        return errText(400, `invalid OTLP payload: ${(e as Error).message}`)
      }
      otlpRequests.push({ headers, scopeName: decoded.scopeName, spans: decoded.spans })
      let response: Response | 'hang' = json({})
      for (const span of decoded.spans) {
        if (span.parentSpanId === undefined) {
          const root = rootView(span)
          if (opts.failRootFor?.(root.labelId) === true) return errText(500, 'root boom')
          if (opts.rejectRootFor?.(root.labelId) === true) {
            return json({ partialSuccess: { rejectedSpans: 1, errorMessage: 'root refused' } })
          }
          roots.push(root)
          order.push({ kind: 'root', traceId: root.traceId })
        } else {
          const gen = generationView(span)
          generations.push(gen)
          order.push({ kind: 'generation', traceId: gen.traceId })
          if (
            opts.generationNeverSettles === true ||
            opts.generationNeverSettlesWhen?.() === true
          ) {
            response = 'hang'
          } else if (opts.generationRejected === true) {
            response = json({
              partialSuccess: { rejectedSpans: 1, errorMessage: 'invalid usage_details' },
            })
          } else if (opts.generationStatus !== undefined) {
            response = errText(opts.generationStatus, 'generation boom')
          }
        }
      }
      return response === 'hang' ? hang(signal) : response
    }

    if (pathname === '/api/public/ingestion' && method === 'POST') {
      const batch = (body as { batch: Array<Record<string, unknown>> }).batch
      const evt = batch[0]
      if (evt.type === 'score-create') {
        const score = { ...(evt as unknown as ScoreEvent), batchLength: batch.length }
        scores.push(score)
        order.push({ kind: 'score', traceId: score.body.traceId })
        if (opts.rejectScore === true) {
          return json({
            successes: [],
            errors: [{ id: score.id, status: 400, message: 'score refused' }],
          })
        }
        return opts.scoreError === true ? errText(500, 'score boom') : accepted(score.id)
      }
      // The events_only instance accepts only score-create on this endpoint.
      return json({
        successes: [],
        errors: [{ id: String(evt.id), status: 400, message: `${String(evt.type)} rejected` }],
      })
    }

    if (pathname === '/api/public/media' && method === 'POST') {
      const b = body as Record<string, unknown>
      order.push({ kind: 'media', traceId: String(b.traceId) })
      registerCount++
      if (opts.failFirstRegister === true && registerCount === 1) return errText(500, 'boom')
      const sha = String(b.sha256Hash)
      let id = shaToId.get(sha)
      const firstTime = id === undefined
      if (id === undefined) {
        id = `m${seq++}`
        shaToId.set(sha, id)
      }
      // uploadUrl only on the first registration of these bytes (sha dedup).
      return json({
        mediaId: id,
        ...(firstTime ? { uploadUrl: `${parsed.origin}/__upload/${id}` } : {}),
      })
    }
    if (isUpload) {
      putCount++
      const fail = opts.failFirstPut === true && putCount === 1
      return new Response('', { status: fail ? 500 : 200 })
    }
    if (/^\/api\/public\/media\/[^/]+$/.test(pathname) && method === 'PATCH') {
      return new Response(null, { status: 204 })
    }

    if (pathname === '/api/public/score-configs' && method === 'GET') {
      // Count only the FIRST page request per resolve so a paged config list still
      // reads as one lazy resolution.
      if (Number(q.get('page') ?? '1') === 1) counts.configResolve++
      if (opts.configError === true) return errText(500, 'score-config boom')
      return page(opts.scoreConfigs ?? [activeVerdictConfig], q, opts.configsPerPage ?? 100)
    }
    if (pathname === '/api/public/annotation-queues' && method === 'GET') {
      if (opts.queueError === true) return errText(500, 'queue boom')
      if (opts.queueMalformed === true)
        return json({ meta: { page: 1, limit: 100, totalPages: 1 } })
      return page(opts.queues ?? [triageQueue], q, opts.queuesPerPage ?? 100)
    }
    const itemsMatch = /^\/api\/public\/annotation-queues\/([^/]+)\/items$/.exec(pathname)
    if (itemsMatch) {
      if (method === 'GET') {
        if (opts.itemsError === true) return errText(500, 'items boom')
        if (opts.itemsMalformed === true)
          return json({ meta: { page: 1, limit: 100, totalPages: 1 } })
        return page(opts.existingItems ?? [], q, opts.itemsPerPage ?? 100)
      }
      const b = body as Record<string, unknown>
      enqueueCount++
      itemPosts.push({
        queueId: itemsMatch[1],
        objectId: String(b.objectId),
        objectType: String(b.objectType),
      })
      if (opts.failEnqueue?.(String(b.objectId), enqueueCount) === true) {
        return errText(500, 'enqueue boom')
      }
      return json({ id: `item-${enqueueCount}`, ...b, status: 'PENDING' })
    }
    return errText(404, `fake langfuse: no route for ${method} ${pathname}`)
  }

  const fetchImpl = (async (input: string | URL, init?: RequestInit) => {
    const body = init?.body
    return handle(
      String(input),
      init?.method ?? 'GET',
      lowerKeys(init?.headers),
      typeof body === 'string' ? body : undefined,
      init?.signal
    )
  }) as unknown as typeof fetch

  let server: http.Server | undefined
  async function listen(): Promise<LangfuseConfig> {
    server = http.createServer((req, res) => {
      const chunks: Buffer[] = []
      req.on('data', (c: Buffer) => chunks.push(c))
      req.on('end', () => {
        const raw = Buffer.concat(chunks)
        void handle(
          `http://${req.headers.host ?? '127.0.0.1'}${req.url ?? '/'}`,
          req.method ?? 'GET',
          lowerKeys(req.headers),
          raw.length ? raw.toString('utf8') : undefined
        ).then(async r => {
          res.writeHead(r.status, { 'content-type': 'application/json' })
          res.end(await r.text())
        })
      })
    })
    await new Promise<void>(resolve => server!.listen(0, '127.0.0.1', () => resolve()))
    const { port } = server.address() as AddressInfo
    return { host: `http://127.0.0.1:${port}`, publicKey: 'pk-fake', secretKey: 'sk-fake' }
  }
  async function close(): Promise<void> {
    if (!server) return
    server.closeAllConnections()
    await new Promise<void>(resolve => server!.close(() => resolve()))
    server = undefined
  }

  return {
    fetchImpl,
    listen,
    close,
    requests,
    otlpRequests,
    roots,
    generations,
    scores,
    itemPosts,
    order,
    counts,
  }
}
