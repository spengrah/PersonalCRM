// `make qa-export` end to end: the command (run.ts `main`, the function the make
// recipe runs) against a fake Langfuse served over loopback HTTP, with assertions on
// the requests it transported. The behaviors are the Langfuse v4 phase 3 spec's
// (`.ai/spec/2026-10-08-langfuse-v4-phase3.md`, P3-n), on top of the usage-cost
// decisions (`.ai/spec/2026-07-22-langfuse-usage-cost-tracking.md`, D1–D9).

import { execFile } from 'child_process'
import * as crypto from 'crypto'
import * as fs from 'fs'
import * as os from 'os'
import * as path from 'path'
import { afterAll, describe, expect, it, vi } from 'vitest'
import { buildGenAiSpan, type GenAiSpan, type SpanParams } from '../adapter/span'
import type { PerItemVerdict } from '../adapter/types'
import type { GradedEvidenceEntry, Scenario } from '../label-trace'
import {
  createFakeLangfuse,
  OTLP_PATH,
  type AnyValueKind,
  type DecodedSpan,
  type FakeLangfuse,
  type FakeOpts,
  type RecordedRequest,
} from './fake-langfuse'
import { exportSpans } from './langfuse'
import { main } from './run'

const TMP = fs.mkdtempSync(path.join(os.tmpdir(), 'qa-export-cmd-'))
afterAll(() => fs.rmSync(TMP, { recursive: true, force: true }))

// A fixed judge-span clock (2026-10-01T12:00:00Z), so times are span-derived and
// visibly not the export clock.
const T0 = Date.UTC(2026, 9, 1, 12, 0, 0)

const base: SpanParams = {
  impl: 'codex-sdk',
  behaviorId: 'CON-042',
  model: 'gpt-5.4-mini',
  startMs: T0,
  endMs: T0 + 3_200,
  inputTokens: 20_000,
  cachedInputTokens: 16_000,
  outputTokens: 1_000,
  prompt: 'the prompt',
}

const scenario = (items: Array<{ itemIndex: number; thenText: string }>): Scenario => ({
  kind: 'behavior',
  behaviorId: 'CON-042',
  behaviorTitle: 'delete confirmation',
  given: 'a contact exists',
  when: 'the user deletes it',
  items,
  allThen: items.map(i => i.thenText),
})

const verdict = (itemIndex: number, v: PerItemVerdict['verdict']): PerItemVerdict => ({
  itemIndex,
  verdict: v,
  citation: 'CAPTURE[0] dialog',
  critique: 'k',
})

// A behavior span over the given (itemIndex, verdict) pairs, in the given order.
function behaviorSpan(
  items: Array<[number, PerItemVerdict['verdict']]>,
  over: Partial<SpanParams> = {}
): GenAiSpan {
  return buildGenAiSpan({
    ...base,
    scenario: scenario(items.map(([i]) => ({ itemIndex: i, thenText: `then ${i}` }))),
    gradedEvidence: [{ captureFile: '001.json', note: 'n', evidence: { url: '/contacts' } }],
    itemVerdicts: items.map(([i, v]) => verdict(i, v)),
    ...over,
  })
}

let pngSeq = 0
function png(bytes?: string): string {
  const n = ++pngSeq
  const p = path.join(TMP, `shot-${n}.png`)
  fs.writeFileSync(p, bytes ?? `png-${n}`)
  return p
}

function graded(paths: string[]): GradedEvidenceEntry[] {
  return paths.map((p, n) => ({
    captureFile: `00${n}.json`,
    note: `capture ${n}`,
    evidence: { url: `/u${n}` },
    screenshot: p,
  }))
}

// --- S1 identity, re-derived here from the spec, independently of the exporter ---

const sha = (s: string): string => crypto.createHash('sha256').update(s, 'utf8').digest('hex')
const baseId = (s: GenAiSpan): string =>
  `judge-${String(s.attributes['qa.behavior_id'])}-${s.span_id}`
const itemId = (s: GenAiSpan, n: number): string => `${baseId(s)}-item${n}`
const hexTrace = (stringId: string): string => sha(stringId).slice(0, 32)
const hexRoot = (stringId: string): string => sha(`${stringId}:root`).slice(0, 16)
const hexGen = (s: GenAiSpan): string => sha(`${baseId(s)}:gen`).slice(0, 16)

// --- the command harness ---

// The nightly's own summary regex, read from the script so this test can never
// drift from what scripts/ci/qa-nightly-round.sh parses.
const NIGHTLY = fs.readFileSync(
  path.join(process.cwd(), '..', 'scripts', 'ci', 'qa-nightly-round.sh'),
  'utf8'
)
const SUMMARY_RE = new RegExp(/^SUMMARY_RE='(.*)'$/m.exec(NIGHTLY)![1])

interface CommandRun {
  code: number
  out: string[]
  err: string[]
  summary?: string
  fake: FakeLangfuse
}

let fileSeq = 0
async function qaExport(
  spans: GenAiSpan[],
  o: {
    env?: Record<string, string | undefined>
    fake?: FakeOpts
    observationTimeoutMs?: number
    file?: string
  } = {}
): Promise<CommandRun> {
  const fake = createFakeLangfuse(o.fake)
  const cfg = await fake.listen()
  const file = o.file ?? path.join(TMP, `trace-${++fileSeq}.jsonl`)
  if (o.file === undefined) {
    fs.writeFileSync(file, spans.map(s => JSON.stringify(s)).join('\n') + '\n')
  }
  const out: string[] = []
  const err: string[] = []
  try {
    const code = await main(
      [file],
      {
        LANGFUSE_HOST: cfg.host,
        LANGFUSE_PUBLIC_KEY: cfg.publicKey,
        LANGFUSE_SECRET_KEY: cfg.secretKey,
        ...o.env,
      },
      {
        log: m => out.push(m),
        errlog: m => err.push(m),
        // The one seam: the generation request's bound, so a timeout case does not
        // wait 30 s. The real exportSpans still runs.
        ...(o.observationTimeoutMs !== undefined
          ? {
              exportSpans: (c, s, l, opts) =>
                exportSpans(c, s, l, { ...opts, observationTimeoutMs: o.observationTimeoutMs }),
            }
          : {}),
      }
    )
    return { code, out, err, summary: out.find(l => SUMMARY_RE.test(l)), fake }
  } finally {
    await fake.close()
  }
}

// Which route of the Langfuse API a recorded request hit.
function routeOf(r: RecordedRequest): string {
  if (r.path === OTLP_PATH && r.method === 'POST') return 'otlp'
  if (r.path === '/api/public/ingestion' && r.method === 'POST') return 'ingestion'
  if (r.path === '/api/public/media' && r.method === 'POST') return 'media.register'
  if (r.path.startsWith('/__upload/') && r.method === 'PUT') return 'media.upload'
  if (/^\/api\/public\/media\/[^/]+$/.test(r.path) && r.method === 'PATCH') return 'media.finalize'
  if (r.path === '/api/public/score-configs' && r.method === 'GET') return 'score-configs'
  if (r.path === '/api/public/annotation-queues' && r.method === 'GET') return 'queues'
  if (/^\/api\/public\/annotation-queues\/[^/]+\/items$/.test(r.path)) {
    return r.method === 'GET' ? 'queue-items.list' : 'queue-items.post'
  }
  return `unknown ${r.method} ${r.path}`
}

const ingestionTypes = (fake: FakeLangfuse): string[] =>
  fake.requests
    .filter(r => routeOf(r) === 'ingestion')
    .flatMap(r => (r.body as { batch: Array<{ type: string }> }).batch.map(e => e.type))

// The media, score-config and queue requests keep their exact shapes beside the
// OTLP transport: per route (its method and path), the body keys it sends, or the
// query keys for the list reads.
const bodyKeys = (r: RecordedRequest): string[] => Object.keys(r.body as object).sort()
const queryKeys = (r: RecordedRequest): string[] => [...r.query.keys()].sort()
const ROUTE_SHAPES: Record<string, { keys: (r: RecordedRequest) => string[]; expected: string[] }> =
  {
    'media.register': {
      keys: bodyKeys,
      expected: ['contentLength', 'contentType', 'field', 'sha256Hash', 'traceId'],
    },
    'media.finalize': { keys: bodyKeys, expected: ['uploadHttpStatus', 'uploadedAt'] },
    'media.upload': { keys: () => [], expected: [] },
    'score-configs': { keys: queryKeys, expected: ['limit', 'page'] },
    queues: { keys: queryKeys, expected: ['limit', 'page'] },
    'queue-items.list': { keys: queryKeys, expected: ['limit', 'page'] },
    'queue-items.post': { keys: bodyKeys, expected: ['objectId', 'objectType'] },
  }

function expectTransport(fake: FakeLangfuse): void {
  const basic = 'Basic ' + Buffer.from('pk-fake:sk-fake').toString('base64')
  for (const r of fake.requests) {
    const route = routeOf(r)
    expect(route, `request outside the known routes: ${route}`).not.toMatch(/^unknown/)
    if (route === 'otlp') {
      expect(r.headers.authorization).toBe(basic)
      expect(r.headers['content-type']).toBe('application/json')
      expect(r.headers['x-langfuse-ingestion-version']).toBe('4')
    } else if (route !== 'ingestion') {
      expect(ROUTE_SHAPES[route].keys(r), route).toEqual(ROUTE_SHAPES[route].expected)
    }
  }
  // The legacy ingestion endpoint carries scores and nothing else.
  for (const t of ingestionTypes(fake)) expect(t).toBe('score-create')
}

describe('P3-1: trace and generation ship over OTLP; only scores use legacy ingestion', () => {
  it('a span with usage: root + generation over OTLP, the score on ingestion, media and queue unchanged', async () => {
    const shot = png()
    const span = behaviorSpan([[0, 'fail']], { gradedEvidence: graded([shot]) })
    const run = await qaExport([span])
    expect(run.code).toBe(0)
    expectTransport(run.fake)
    const stringId = itemId(span, 0)
    expect(run.fake.roots.map(r => r.traceId)).toEqual([hexTrace(stringId)])
    expect(run.fake.generations.map(g => g.traceId)).toEqual([hexTrace(stringId)])
    expect(ingestionTypes(run.fake)).toEqual(['score-create'])
    expect(run.fake.requests.map(routeOf).filter(r => r.startsWith('media'))).toEqual([
      'media.register',
      'media.upload',
      'media.finalize',
    ])
    expect(run.fake.itemPosts).toEqual([
      { queueId: 'q-triage', objectId: hexTrace(stringId), objectType: 'TRACE' },
    ])
  })

  it('a span without usage: the root ships over OTLP and no generation is sent', async () => {
    const span = behaviorSpan([[0, 'pass']], { inputTokens: undefined })
    const run = await qaExport([span], { env: { QA_SALT_PASSES: '0' } })
    expect(run.code).toBe(0)
    expectTransport(run.fake)
    expect(run.fake.roots.map(r => r.traceId)).toEqual([hexTrace(itemId(span, 0))])
    expect(run.fake.generations).toHaveLength(0)
    expect(ingestionTypes(run.fake)).toEqual(['score-create'])
  })

  it('a multi-item span: one root per item over OTLP, one generation for the span', async () => {
    const span = behaviorSpan([
      [0, 'fail'],
      [2, 'pass'],
    ])
    const run = await qaExport([span], { env: { QA_SALT_PASSES: '0' } })
    expect(run.code).toBe(0)
    expectTransport(run.fake)
    expect(run.fake.roots.map(r => r.traceId).sort()).toEqual(
      [hexTrace(itemId(span, 0)), hexTrace(itemId(span, 2))].sort()
    )
    expect(run.fake.generations).toHaveLength(1)
    expect(ingestionTypes(run.fake)).toEqual(['score-create', 'score-create'])
  })

  it('a no-scenario span: one root over OTLP under the base id, no score, nothing on ingestion', async () => {
    const span = buildGenAiSpan({ ...base, response: 'the completion' })
    const run = await qaExport([span])
    expect(run.code).toBe(0)
    expectTransport(run.fake)
    expect(run.fake.roots.map(r => r.traceId)).toEqual([hexTrace(baseId(span))])
    expect(run.fake.generations.map(g => g.traceId)).toEqual([hexTrace(baseId(span))])
    expect(ingestionTypes(run.fake)).toEqual([])
  })
})

// Every OTLP span's identity, as one comparable tuple.
const identities = (fake: FakeLangfuse): string[] =>
  fake.otlpRequests.flatMap(r =>
    r.spans.map(s =>
      [s.traceId, s.spanId, s.parentSpanId ?? '-', s.startTimeUnixNano, s.endTimeUnixNano].join(' ')
    )
  )

// Each root's ids are S1's derivation of its label_id, its times are the judge span's,
// and the generation (if any) hangs off the carrier's root under the span's gen id.
function expectDerivedIdentity(
  span: GenAiSpan,
  fake: FakeLangfuse,
  labelIds: string[],
  carrier: string | undefined
): void {
  expect(fake.roots.map(r => r.labelId).sort()).toEqual([...labelIds].sort())
  for (const root of fake.roots) {
    expect(root.attributes['langfuse.trace.metadata.label_id']).toBe(root.labelId)
    expect(root.traceId).toBe(hexTrace(root.labelId))
    expect(root.spanId).toBe(hexRoot(root.labelId))
    expect(root.parentSpanId).toBeUndefined()
    expect(Number(root.startTimeUnixNano)).toBe(span.start_time_unix_nano)
    expect(Number(root.endTimeUnixNano)).toBe(span.end_time_unix_nano)
  }
  if (carrier === undefined) {
    expect(fake.generations).toHaveLength(0)
    return
  }
  expect(fake.generations).toHaveLength(1)
  const [gen] = fake.generations
  expect(gen.traceId).toBe(hexTrace(carrier))
  expect(gen.spanId).toBe(hexGen(span))
  expect(gen.parentSpanId).toBe(hexRoot(carrier))
  expect(gen.labelId).toBe(carrier)
  expect(Number(gen.startTimeUnixNano)).toBe(span.start_time_unix_nano)
  expect(Number(gen.endTimeUnixNano)).toBe(span.end_time_unix_nano)
}

describe('P3-2, P3-11: identity is derived from the judge span and stable across re-export', () => {
  it('two items of one span get distinct trace ids, each its own derivation', async () => {
    const span = behaviorSpan([
      [0, 'fail'],
      [2, 'pass'],
    ])
    const run = await qaExport([span], { env: { QA_SALT_PASSES: '0' } })
    expect(run.code).toBe(0)
    expectDerivedIdentity(span, run.fake, [itemId(span, 0), itemId(span, 2)], itemId(span, 0))
    const [a, b] = run.fake.roots
    expect(a.traceId).not.toBe(b.traceId)
    expect(a.spanId).not.toBe(b.spanId)
  })

  it('the no-scenario shape derives everything from the base string id', async () => {
    const span = buildGenAiSpan({ ...base, response: 'the completion' })
    const run = await qaExport([span])
    expect(run.code).toBe(0)
    expectDerivedIdentity(span, run.fake, [baseId(span)], baseId(span))
  })

  it('the generation span id differs from the root span id and from the trace id', async () => {
    const span = behaviorSpan([[0, 'fail']])
    const run = await qaExport([span])
    const [root] = run.fake.roots
    const [gen] = run.fake.generations
    expect(gen.spanId).not.toBe(root.spanId)
    expect(gen.spanId).not.toBe(root.traceId.slice(0, 16))
    expect(gen.spanId).not.toBe(gen.traceId.slice(0, 16))
    expect(root.spanId).not.toBe(root.traceId.slice(0, 16))
    expectDerivedIdentity(span, run.fake, [itemId(span, 0)], itemId(span, 0))
  })

  it('a re-export of the same file, at a later clock across a UTC date change, transports identical ids and times', async () => {
    const span = behaviorSpan([
      [0, 'fail'],
      [1, 'unsure'],
    ])
    const file = path.join(TMP, 'reexport.jsonl')
    fs.writeFileSync(file, JSON.stringify(span) + '\n')
    vi.useFakeTimers({ toFake: ['Date'] })
    try {
      vi.setSystemTime(new Date('2026-10-01T23:59:30Z'))
      const first = await qaExport([], { file })
      vi.setSystemTime(new Date('2026-10-02T00:00:30Z'))
      const second = await qaExport([], { file })
      expect(first.code).toBe(0)
      expect(second.code).toBe(0)
      expect(identities(second.fake)).toEqual(identities(first.fake))
      expect(identities(first.fake)).toHaveLength(3) // two roots + one generation
      expectDerivedIdentity(span, second.fake, [itemId(span, 0), itemId(span, 1)], itemId(span, 0))
    } finally {
      vi.useRealTimers()
    }
  })
})

const token = (mediaId: string): string =>
  `@@@langfuseMedia:type=image/png|id=${mediaId}|source=bytes@@@`
const shotsOf = (root: { input?: Record<string, unknown> }): Array<string | undefined> =>
  (root.input!.graded_evidence as Array<{ screenshot?: string }>).map(e => e.screenshot)

// Media for a trace registers against its hex id, all of it before that trace's one
// root send; returns the chronology for further checks.
function expectMediaBeforeSingleRoot(fake: FakeLangfuse, traceId: string, registrations: number) {
  const mine = fake.order.filter(o => o.traceId === traceId)
  const roots = mine.filter(o => o.kind === 'root')
  expect(roots).toHaveLength(1)
  const rootAt = mine.findIndex(o => o.kind === 'root')
  expect(mine.slice(0, rootAt).map(o => o.kind)).toEqual(Array(registrations).fill('media'))
  // One OTLP request carried that root, and no other request re-sent it.
  const rootSends = fake.otlpRequests.filter(r =>
    r.spans.some(s => s.traceId === traceId && s.parentSpanId === undefined)
  )
  expect(rootSends).toHaveLength(1)
}

describe('P3-3: media registers against the hex id first; the root is sent once, tokens spliced, all-or-nothing', () => {
  it('every screenshot uploads: each item-trace registers its own media, then sends one root with every token', async () => {
    const [a, b] = [png('bytes-a'), png('bytes-b')]
    const span = behaviorSpan(
      [
        [0, 'fail'],
        [1, 'pass'],
      ],
      { gradedEvidence: graded([a, b]) }
    )
    const run = await qaExport([span], { env: { QA_SALT_PASSES: '0' } })
    expect(run.code).toBe(0)
    expect(run.summary).toMatch(/^qa-export: 2 trace\(s\), 4 screenshot\(s\)/)
    const registers = run.fake.requests.filter(r => routeOf(r) === 'media.register')
    for (const n of [0, 1]) {
      const traceId = hexTrace(itemId(span, n))
      expect(
        registers.filter(r => (r.body as { traceId: string }).traceId === traceId)
      ).toHaveLength(2)
      expectMediaBeforeSingleRoot(run.fake, traceId, 2)
      const root = run.fake.roots.find(r => r.traceId === traceId)!
      // Bytes dedup by sha: the same two media ids, by index, on both traces.
      expect(shotsOf(root)).toEqual([token('m0'), token('m1')])
      expect(root.metadata.screenshots_expected).toBe(2)
      expect(root.metadata.screenshots_attached).toBe(2)
    }
  })

  it('one of three uploads fails: the root ships once with NO tokens and attached 0', async () => {
    const span = behaviorSpan([[0, 'fail']], {
      gradedEvidence: graded([png('c1'), png('c2'), png('c3')]),
    })
    const run = await qaExport([span], { fake: { failFirstPut: true } })
    expect(run.code).toBe(0)
    expect(run.summary).toMatch(/^qa-export: 1 trace\(s\), 0 screenshot\(s\)/)
    expect(run.summary).not.toContain('FAILED')
    const traceId = hexTrace(itemId(span, 0))
    expectMediaBeforeSingleRoot(run.fake, traceId, 3)
    const [root] = run.fake.roots
    expect(shotsOf(root)).toEqual([undefined, undefined, undefined])
    expect(root.metadata.screenshots_expected).toBe(3)
    expect(root.metadata.screenshots_attached).toBe(0)
  })

  it('a registration request errors: the trace still ships, once, with attached 0', async () => {
    const span = behaviorSpan([[0, 'fail']], { gradedEvidence: graded([png('r1'), png('r2')]) })
    const run = await qaExport([span], { fake: { failFirstRegister: true } })
    expect(run.code).toBe(0)
    expect(run.summary).toMatch(/^qa-export: 1 trace\(s\), 0 screenshot\(s\)/)
    expect(run.summary).not.toContain('FAILED')
    const traceId = hexTrace(itemId(span, 0))
    expectMediaBeforeSingleRoot(run.fake, traceId, 2)
    const [root] = run.fake.roots
    expect(shotsOf(root)).toEqual([undefined, undefined])
    expect(root.metadata.screenshots_attached).toBe(0)
    expect(run.out.some(l => l.includes('media register failed'))).toBe(true)
  })

  it('a trace with no screenshots: no media request, one root', async () => {
    const span = behaviorSpan([[0, 'fail']])
    const run = await qaExport([span])
    expect(run.code).toBe(0)
    expect(run.fake.requests.filter(r => routeOf(r).startsWith('media'))).toHaveLength(0)
    expectMediaBeforeSingleRoot(run.fake, hexTrace(itemId(span, 0)), 0)
    expect(run.fake.roots[0].metadata.screenshots_expected).toBe(0)
    expect(run.fake.roots[0].metadata.screenshots_attached).toBe(0)
  })
})

// The OTLP request that carried a generation, which must carry nothing else.
function generationRequest(fake: FakeLangfuse) {
  const reqs = fake.otlpRequests.filter(r => r.spans.some(s => s.parentSpanId !== undefined))
  expect(reqs).toHaveLength(1)
  expect(reqs[0].spans).toHaveLength(1)
  return reqs[0].spans[0]
}

// The usage_details attribute exactly as transported: a JSON object string.
function usageDetails(span: { attributes: Record<string, unknown> }): Record<string, unknown> {
  const raw = span.attributes['langfuse.observation.usage_details']
  expect(typeof raw).toBe('string')
  const parsed = JSON.parse(raw as string) as unknown
  expect(parsed !== null && typeof parsed === 'object' && !Array.isArray(parsed)).toBe(true)
  return parsed as Record<string, unknown>
}

// Trace, score and enqueue of a single-item fail span all survived the generation.
function expectTraceScoreEnqueueIntact(run: CommandRun, span: GenAiSpan): void {
  const traceId = hexTrace(itemId(span, 0))
  expect(run.code).toBe(0)
  expect(run.summary).toMatch(/^qa-export: 1 trace\(s\)/)
  expect(run.summary).not.toContain('FAILED')
  expect(run.fake.roots.map(r => r.traceId)).toEqual([traceId])
  expect(run.fake.scores.map(s => s.body.traceId)).toEqual([traceId])
  expect(run.fake.itemPosts.map(p => p.objectId)).toEqual([traceId])
  expect(run.summary).toContain('; enqueued 1/1')
}

describe('P3-4: one generation per usage-carrying span, exact usage keys, isolated from its trace', () => {
  it('a span with cached tokens: input net of cached, exactly three keys, its own request after the root', async () => {
    const span = behaviorSpan([[0, 'fail']])
    const run = await qaExport([span])
    expect(run.code).toBe(0)
    expect(run.summary).toMatch(/^qa-export: 1 trace\(s\), 0 screenshot\(s\), 1 observation\(s\); /)
    const gen = generationRequest(run.fake)
    expect(gen.attributes['langfuse.observation.type']).toBe('generation')
    expect(gen.attributes['langfuse.observation.model.name']).toBe('gpt-5.4-mini')
    const usage = usageDetails(gen)
    expect(Object.keys(usage).sort()).toEqual(['input', 'input_cached_tokens', 'output'])
    expect(usage).toEqual({ input: 4_000, input_cached_tokens: 16_000, output: 1_000 })
    const kinds = run.fake.order.map(o => o.kind)
    expect(kinds.indexOf('generation')).toBeGreaterThan(kinds.lastIndexOf('root'))
  })

  it('a span with zero cached tokens still sends all three keys, the cached one at 0', async () => {
    const span = behaviorSpan([[0, 'fail']], { cachedInputTokens: 0 })
    const run = await qaExport([span])
    expect(usageDetails(generationRequest(run.fake))).toEqual({
      input: 20_000,
      input_cached_tokens: 0,
      output: 1_000,
    })
  })

  it('a span with reasoning tokens: reasoning and cache-write ride as unpriced metadata only', async () => {
    const span = behaviorSpan([[0, 'fail']], {
      reasoningOutputTokens: 800,
      cacheWriteInputTokens: 500,
    })
    const run = await qaExport([span])
    const gen = generationRequest(run.fake)
    expect(usageDetails(gen)).toEqual({ input: 4_000, input_cached_tokens: 16_000, output: 1_000 })
    expect(gen.attributes['langfuse.observation.metadata.reasoning_output_tokens']).toBe(800)
    expect(gen.attributes['langfuse.observation.metadata.cache_write_input_tokens']).toBe(500)
    expectMetadataKinds(gen)
    const priced = Object.keys(gen.attributes).filter(
      k => k.startsWith('langfuse.observation.') && !k.startsWith('langfuse.observation.metadata.')
    )
    expect(priced.some(k => /reasoning|cache_write/.test(k))).toBe(false)
  })

  it('a multi-item span whose lowest itemIndex is not first: one generation, on the lowest-itemIndex trace', async () => {
    const span = behaviorSpan([
      [3, 'pass'],
      [1, 'fail'],
    ])
    const run = await qaExport([span], { env: { QA_SALT_PASSES: '0' } })
    expect(run.fake.roots).toHaveLength(2)
    const gen = generationRequest(run.fake)
    expect(gen.traceId).toBe(hexTrace(itemId(span, 1)))
    expect(gen.parentSpanId).toBe(hexRoot(itemId(span, 1)))
    // Usage is attributed once, on the carrier, and the sibling says so.
    const carrier = run.fake.roots.find(r => r.labelId === itemId(span, 1))!
    const sibling = run.fake.roots.find(r => r.labelId === itemId(span, 3))!
    expect(carrier.metadata.usage_attributed).toBe(true)
    expect(sibling.metadata.usage_attributed).toBe(false)
  })

  it('a span without usage gets no generation and counts nowhere', async () => {
    const span = behaviorSpan([[0, 'fail']], { inputTokens: undefined })
    const run = await qaExport([span])
    expect(run.fake.generations).toHaveLength(0)
    expect(run.summary).toMatch(/^qa-export: 1 trace\(s\), 0 screenshot\(s\), 0 observation\(s\); /)
  })

  it('a generation request answered 4xx counts as failed; the trace, score and enqueue stand', async () => {
    const span = behaviorSpan([[0, 'fail']])
    const run = await qaExport([span], { fake: { generationStatus: 400 } })
    expect(run.fake.generations).toHaveLength(1)
    expect(run.summary).toContain('0 observation(s), 1 observation(s) failed')
    expectTraceScoreEnqueueIntact(run, span)
  })

  // OTLP answers a full success with an empty body and a partial one with
  // `partialSuccess.rejectedSpans`, an int64 that JSON may carry as a string.
  it.each([
    [
      'a rejected count with its message',
      { partialSuccess: { rejectedSpans: 1, errorMessage: 'invalid usage_details' } },
      '1 span(s) rejected: invalid usage_details',
    ],
    [
      'a rejected count as an int64 string',
      { partialSuccess: { rejectedSpans: '2' } },
      '2 span(s) rejected',
    ],
    [
      'a rejected count that is not a number',
      { partialSuccess: { rejectedSpans: 'lots' } },
      'lots span(s) rejected',
    ],
    [
      'a partialSuccess that is a string',
      { partialSuccess: 'garbage' },
      'unreadable partialSuccess',
    ],
    ['a partialSuccess that is an array', { partialSuccess: ['x'] }, 'unreadable partialSuccess'],
    ['a partialSuccess that is a number', { partialSuccess: 7 }, 'unreadable partialSuccess'],
  ])(
    'a generation answered 200 with %s counts as failed, never as shipped',
    async (_case, body, reason) => {
      const span = behaviorSpan([[0, 'fail']])
      const run = await qaExport([span], { fake: { generationResponse: body } })
      expect(run.summary).toContain('0 observation(s), 1 observation(s) failed')
      expect(run.out.some(l => l.includes('generation failed') && l.includes(reason))).toBe(true)
      expectTraceScoreEnqueueIntact(run, span)
    }
  )

  it.each([
    ['a null partialSuccess', { partialSuccess: null }],
    ['an empty partialSuccess', { partialSuccess: {} }],
    ['zero rejected spans', { partialSuccess: { rejectedSpans: 0 } }],
    [
      'zero rejected spans as an int64 string, with a warning',
      { partialSuccess: { rejectedSpans: '0', errorMessage: 'slow down' } },
    ],
  ])('a generation answered 200 with %s counts as accepted', async (_case, body) => {
    const span = behaviorSpan([[0, 'fail']])
    const run = await qaExport([span], { fake: { generationResponse: body } })
    expect(run.summary).toMatch(/^qa-export: 1 trace\(s\), 0 screenshot\(s\), 1 observation\(s\); /)
    expectTraceScoreEnqueueIntact(run, span)
  })

  it('a generation request that times out counts as failed; the trace, score and enqueue stand', async () => {
    const span = behaviorSpan([[0, 'fail']])
    const run = await qaExport([span], {
      fake: { generationNeverSettles: true },
      observationTimeoutMs: 50,
    })
    expect(run.summary).toContain('0 observation(s), 1 observation(s) failed')
    expectTraceScoreEnqueueIntact(run, span)
  })

  it('three consecutive generation aborts trip the breaker; later generations count as skipped', async () => {
    const spans = Array.from({ length: 5 }, (_, i) =>
      behaviorSpan([[0, 'fail']], { behaviorId: `CON-04${i}` })
    )
    const run = await qaExport(spans, {
      fake: { generationNeverSettles: true },
      observationTimeoutMs: 30,
    })
    expect(run.code).toBe(0)
    expect(run.fake.generations).toHaveLength(3) // never attempted after the third abort
    expect(run.summary).toMatch(
      /^qa-export: 5 trace\(s\), 0 screenshot\(s\), 0 observation\(s\), 3 observation\(s\) failed, 2 observation\(s\) skipped; enqueued 5\/5$/
    )
    expect(run.out.some(l => l.includes('observations DISABLED'))).toBe(true)
  })
})

// Synthetic PII sentinels (RFC 2606 domain, 555-01xx fictional numbers).
const EMAIL_RAW = /@synthetic\.example/
const PHONE_RAW = /479[-) ]+555-01\d\d/

function expectNoRawPii(value: unknown, where: string): void {
  const s = typeof value === 'string' ? value : JSON.stringify(value)
  expect(s, `${where} leaked an email`).not.toMatch(EMAIL_RAW)
  expect(s, `${where} leaked a phone`).not.toMatch(PHONE_RAW)
}

// Every attribute the command transported, plus every request body, carries no raw
// sentinel — the named attributes individually, then the whole wire as a backstop.
function expectWireScrubbed(fake: FakeLangfuse): void {
  for (const root of fake.roots) {
    expectNoRawPii(root.attributes['langfuse.observation.input'], 'root input')
    expectNoRawPii(root.attributes['langfuse.observation.output'], 'root output')
    expectNoRawPii(root.attributes['langfuse.trace.name'], 'trace name')
    expectNoRawPii(root.attributes['langfuse.trace.tags'], 'trace tags')
    for (const [k, v] of Object.entries(root.attributes)) {
      if (k.startsWith('langfuse.trace.metadata.')) expectNoRawPii(v, k)
    }
  }
  for (const gen of fake.generations) {
    expectNoRawPii(gen.attributes['langfuse.observation.model.name'], 'generation model name')
    for (const [k, v] of Object.entries(gen.attributes)) expectNoRawPii(v, `generation ${k}`)
  }
  for (const r of fake.requests) expectNoRawPii(r.body ?? '', `${r.method} ${r.path}`)
}

describe('P3-10: every free-form attribute is scrubbed as judge/scrub.ts scrubs it', () => {
  it('an email address and a phone number in the judge output', async () => {
    const span = behaviorSpan([[0, 'fail']], {
      itemVerdicts: [
        {
          itemIndex: 0,
          verdict: 'fail',
          citation: 'CAPTURE[0] shows out-a1@synthetic.example',
          critique: 'called +1-479-555-0101 instead',
        },
      ],
    })
    const run = await qaExport([span])
    expect(run.code).toBe(0)
    expectWireScrubbed(run.fake)
    const output = run.fake.roots[0].output as PerItemVerdict
    expect(output.verdict).toBe('fail')
    expect(output.citation).toBe('CAPTURE[0] shows <email:1>')
    expect(output.critique).toBe('called <phone:1> instead')
  })

  it('one in the error metadata field', async () => {
    const span = behaviorSpan([[0, 'fail']], {
      error: 'judge threw on err-b2@synthetic.example (479) 555-0102',
    })
    const run = await qaExport([span])
    expect(run.code).toBe(0)
    expectWireScrubbed(run.fake)
    expect(run.fake.roots[0].attributes['langfuse.trace.metadata.error']).toBe(
      'judge threw on <email:1> <phone:1>'
    )
  })

  it('one in an env-sourced model string: the metadata, the model tag and the generation model name', async () => {
    const span = behaviorSpan([[0, 'fail']], { model: 'mdl-c3@synthetic.example' })
    const run = await qaExport([span])
    expect(run.code).toBe(0)
    expectWireScrubbed(run.fake)
    const [root] = run.fake.roots
    expect(root.attributes['langfuse.trace.metadata.model']).toBe('<email:1>')
    expect(root.tags).toContain('model:<email:1>')
    expect(run.fake.generations[0].attributes['langfuse.observation.model.name']).toBe('<email:1>')
    // Trace-level attributes ride the generation too, scrubbed the same way.
    expect(run.fake.generations[0].attributes['langfuse.trace.metadata.model']).toBe('<email:1>')
  })
})

// The trace-level attributes of a span: every `langfuse.trace.*` and the session.
const traceLevel = (s: { attributes: Record<string, unknown> }): Record<string, unknown> =>
  Object.fromEntries(
    Object.entries(s.attributes).filter(
      ([k]) => k.startsWith('langfuse.trace.') || k === 'langfuse.session.id'
    )
  )

// The legacy trace body's metadata for `base` + reasoning/cache-write usage, as
// transported: every field under `langfuse.trace.metadata.`, each with the JSON type
// the legacy body gave it (counts are numbers, flags are booleans).
const legacyMetadata = (labelId: string, over: Record<string, unknown> = {}) => ({
  'langfuse.trace.metadata.label_id': labelId,
  'langfuse.trace.metadata.behavior_id': 'CON-042',
  'langfuse.trace.metadata.impl': 'codex-sdk',
  'langfuse.trace.metadata.model': 'gpt-5.4-mini',
  'langfuse.trace.metadata.input_tokens': 20_000,
  'langfuse.trace.metadata.cached_input_tokens': 16_000,
  'langfuse.trace.metadata.output_tokens': 1_000,
  'langfuse.trace.metadata.reasoning_output_tokens': 800,
  'langfuse.trace.metadata.cache_write_input_tokens': 500,
  'langfuse.trace.metadata.tool_rejected': false,
  'langfuse.trace.metadata.status': 'OK',
  'langfuse.trace.metadata.duration_ms': 3_200,
  'langfuse.trace.metadata.screenshots_expected': 0,
  'langfuse.trace.metadata.screenshots_attached': 0,
  'langfuse.trace.metadata.usage_attributed': true,
  ...over,
})

// The OTLP kind that carries a value through Langfuse's decoder unchanged: a string, an
// integer, a non-integer number and a boolean each as their own kind, an array as
// `arrayValue`. A decoded value is never null or an object (the decoder drops the
// empty AnyValue and stringifies a kvlistValue), so no other kind qualifies.
const kindFor = (v: unknown): AnyValueKind | undefined => {
  if (typeof v === 'string') return 'stringValue'
  if (typeof v === 'boolean') return 'boolValue'
  if (typeof v === 'number') return Number.isInteger(v) ? 'intValue' : 'doubleValue'
  if (Array.isArray(v)) return 'arrayValue'
  return undefined
}

// `legacyMetadata` keyed as `ShippedRoot.metadata` is: the field name alone.
const prefixStripped = (m: Record<string, unknown>): Record<string, unknown> =>
  Object.fromEntries(
    Object.entries(m).map(([k, v]) => [k.slice('langfuse.trace.metadata.'.length), v])
  )

// Every metadata attribute a span sent survived Langfuse's decoder, as the kind its
// decoded JSON type names: none was dropped (an empty AnyValue) or mangled (a
// kvlistValue, which decodes to a string).
function expectMetadataKinds(span: DecodedSpan): void {
  for (const [k, kind] of Object.entries(span.kinds)) {
    if (/^langfuse\.(trace|observation)\.metadata\./.test(k)) {
      expect(span.attributes, `${k} was sent as ${kind} and dropped`).toHaveProperty([k])
      expect(kind, k).toBe(kindFor(span.attributes[k]))
    }
  }
}

describe('P3-1, D3: the root carries the trace name, session, tags, IO and every legacy metadata field', () => {
  const usage = { reasoningOutputTokens: 800, cacheWriteInputTokens: 500 }

  it('a run with QA_RUN_ID and QA_GIT_SHA: session = run id, provenance tags, every metadata field, on every span', async () => {
    const span = behaviorSpan([[0, 'fail']], usage)
    const run = await qaExport([span], {
      env: { QA_RUN_ID: '20261001T120000Z', QA_GIT_SHA: 'abc1234' },
    })
    expect(run.code).toBe(0)
    const [root] = run.fake.roots
    const id = itemId(span, 0)
    expect(root.name).toBe('judge CON-042')
    expect(traceLevel(root)).toEqual({
      'langfuse.trace.name': 'judge CON-042',
      'langfuse.session.id': '20261001T120000Z',
      'langfuse.trace.tags': [
        'behavior:CON-042',
        'runId:20261001T120000Z',
        'gitSha:abc1234',
        'pass:ux',
        'model:gpt-5.4-mini',
      ],
      ...legacyMetadata(id),
    })
    expect(root.input).toEqual({
      scenario_item: {
        behavior_id: 'CON-042',
        behavior_title: 'delete confirmation',
        given: 'a contact exists',
        when: 'the user deletes it',
        then_text: 'then 0',
        all_then: ['then 0'],
      },
      graded_evidence: [{ capture_file: '001.json', note: 'n', evidence: { url: '/contacts' } }],
      prompt: 'the prompt',
    })
    expect(root.output).toEqual(verdict(0, 'fail'))
    // The generation is a span of the same trace: it carries the same trace-level
    // attributes, value and type alike.
    expect(traceLevel(run.fake.generations[0])).toEqual(traceLevel(root))
    expectMetadataKinds(root)
    expectMetadataKinds(run.fake.generations[0])
  })

  it('a run with neither: no session, no provenance tags; an errored span carries its status and error', async () => {
    const span = behaviorSpan([[0, 'fail']], { ...usage, error: 'judge timed out' })
    const run = await qaExport([span])
    expect(run.code).toBe(0)
    const [root] = run.fake.roots
    expect(traceLevel(root)).toEqual({
      'langfuse.trace.name': 'judge CON-042',
      'langfuse.trace.tags': ['behavior:CON-042', 'pass:ux', 'model:gpt-5.4-mini'],
      ...legacyMetadata(itemId(span, 0), {
        'langfuse.trace.metadata.status': 'ERROR',
        'langfuse.trace.metadata.error': 'judge timed out',
      }),
    })
    expect(traceLevel(run.fake.generations[0])).toEqual(traceLevel(root))
    expectMetadataKinds(root)
  })

  it('an intent-judge span: pass:intent, the intent scenario item, its verdict as output', async () => {
    const span = buildGenAiSpan({
      ...base,
      ...usage,
      behaviorId: 'DSH-010',
      model: 'gpt-5.5',
      scenario: {
        kind: 'intent',
        intentId: 'DSH-010',
        title: 'at a glance',
        statement: 'answers what needs attention',
        status: 'current',
      },
      gradedEvidence: [{ captureFile: 'a.json', note: 'n', evidence: {} }],
      itemVerdicts: [verdict(0, 'pass')],
    })
    const run = await qaExport([span], {
      env: { QA_RUN_ID: '20261001T120000Z', QA_SALT_PASSES: '0' },
    })
    expect(run.code).toBe(0)
    const [root] = run.fake.roots
    expect(root.attributes['langfuse.trace.name']).toBe('judge DSH-010')
    expect(root.sessionId).toBe('20261001T120000Z')
    expect(root.tags).toEqual([
      'behavior:DSH-010',
      'runId:20261001T120000Z',
      'pass:intent',
      'model:gpt-5.5',
    ])
    expect(root.input).toEqual({
      scenario_item: {
        intent_id: 'DSH-010',
        title: 'at a glance',
        statement: 'answers what needs attention',
        status: 'current',
      },
      graded_evidence: [{ capture_file: 'a.json', note: 'n', evidence: {} }],
      prompt: 'the prompt',
    })
    expect(root.output).toEqual(verdict(0, 'pass'))
    expect(root.metadata).toEqual(
      prefixStripped(
        legacyMetadata(itemId(span, 0), {
          'langfuse.trace.metadata.behavior_id': 'DSH-010',
          'langfuse.trace.metadata.model': 'gpt-5.5',
        })
      )
    )
    expect(traceLevel(run.fake.generations[0])).toEqual(traceLevel(root))
    expectMetadataKinds(root)
  })

  it('a span file carrying a null, an object, an array holding a null and a non-integer count: the null is omitted, the object and that array read as their JSON, the count keeps its type (R4)', async () => {
    const span = behaviorSpan([[0, 'fail']], { ...usage, cacheWriteInputTokens: 12.5 })
    // Values only a hand-edited or malformed span file carries; the legacy body
    // shipped them as JSON. Langfuse's OTLP decoder drops an empty AnyValue and has no
    // kvlistValue branch, so ruling R4 fixes what each reads as.
    span.attributes['qa.judge.impl'] = null
    span.attributes['qa.tool_rejected'] = { by: 'policy', codes: [1, 2.5] }
    ;(span.status as { code: unknown }).code = ['OK', null]
    const run = await qaExport([span])
    expect(run.code).toBe(0)
    const [root] = run.fake.roots
    const [gen] = run.fake.generations
    const expected = prefixStripped(
      legacyMetadata(itemId(span, 0), {
        'langfuse.trace.metadata.tool_rejected': '{"by":"policy","codes":[1,2.5]}',
        'langfuse.trace.metadata.status': '["OK",null]',
        'langfuse.trace.metadata.cache_write_input_tokens': 12.5,
      })
    )
    delete expected.impl
    expect(root.metadata).toEqual(expected)
    expect(gen.metadata).toEqual({ reasoning_output_tokens: 800, cache_write_input_tokens: 12.5 })
    // Nothing was sent for Langfuse to drop or mangle, on either span of the trace.
    expectMetadataKinds(root)
    expectMetadataKinds(gen)
  })
})

describe('P3-1, P3-2: the verdict score and the queue item key to the hex trace id', () => {
  it('a failing verdict: score-create on the hex id, span-start envelope, then a TRACE queue item', async () => {
    const span = behaviorSpan([[0, 'fail']])
    const run = await qaExport([span])
    expect(run.code).toBe(0)
    const traceId = hexTrace(itemId(span, 0))
    expect(run.fake.scores).toHaveLength(1)
    const [score] = run.fake.scores
    expect(score.type).toBe('score-create')
    expect(score.batchLength).toBe(1)
    expect(score.timestamp).toBe(new Date(T0).toISOString())
    expect(score.body).toEqual({
      id: `score-${traceId}-verdict`,
      name: 'verdict',
      value: 'fail',
      dataType: 'CATEGORICAL',
      traceId,
      configId: 'cfg-verdict',
    })
    expect(run.fake.order.filter(o => o.traceId === traceId).map(o => o.kind)).toEqual([
      'root',
      'score',
      'generation',
    ])
    expect(run.fake.itemPosts).toEqual([
      { queueId: 'q-triage', objectId: traceId, objectType: 'TRACE' },
    ])
  })

  it('a salted pass: scored pass, enqueued as a TRACE under its hex id', async () => {
    const span = behaviorSpan([[0, 'pass']])
    const run = await qaExport([span], { env: { QA_SALT_PASSES: '1' } })
    const traceId = hexTrace(itemId(span, 0))
    expect(run.fake.scores.map(s => [s.body.id, s.body.value, s.body.traceId])).toEqual([
      [`score-${traceId}-verdict`, 'pass', traceId],
    ])
    expect(run.fake.itemPosts).toEqual([
      { queueId: 'q-triage', objectId: traceId, objectType: 'TRACE' },
    ])
    expect(run.summary).toContain('; enqueued 1/1')
  })

  it('a re-export sends the same score id and timestamp under a new event id', async () => {
    const span = behaviorSpan([[0, 'fail']])
    const file = path.join(TMP, 'rescore.jsonl')
    fs.writeFileSync(file, JSON.stringify(span) + '\n')
    vi.useFakeTimers({ toFake: ['Date'] })
    try {
      vi.setSystemTime(new Date('2026-10-01T23:59:30Z'))
      const first = await qaExport([], { file })
      vi.setSystemTime(new Date('2026-10-02T00:00:30Z'))
      const second = await qaExport([], { file })
      const [a] = first.fake.scores
      const [b] = second.fake.scores
      expect(b.body).toEqual(a.body)
      expect(b.timestamp).toBe(a.timestamp)
      expect(b.id).not.toBe(a.id)
    } finally {
      vi.useRealTimers()
    }
  })

  it('a score request failure is counted apart from the trace: the trace ships, nothing FAILED, the enqueue runs', async () => {
    const span = behaviorSpan([[0, 'fail']])
    const run = await qaExport([span], { fake: { scoreError: true } })
    expect(run.code).toBe(0)
    expect(run.fake.scores).toHaveLength(1)
    expect(run.out.some(l => l.includes('score-create failed'))).toBe(true)
    expectTraceScoreEnqueueIntact(run, span)
  })
})

const tagsOf = (s: { attributes: Record<string, unknown> }): string[] =>
  (s.attributes['langfuse.trace.tags'] as string[] | undefined) ?? []
const allSpans = (fake: FakeLangfuse) => fake.otlpRequests.flatMap(r => r.spans)

describe('PAR-5: QA_TEST_TAG tags every span of every trace, and an invalid value sends nothing', () => {
  it('a valid slug: every span of every trace carries test:<slug>', async () => {
    const span = behaviorSpan([
      [0, 'fail'],
      [2, 'pass'],
    ])
    const run = await qaExport([span], {
      env: { QA_TEST_TAG: 'lfv4-pr3_c8.x', QA_SALT_PASSES: '0' },
    })
    expect(run.code).toBe(0)
    const spans = allSpans(run.fake)
    expect(spans).toHaveLength(3) // two roots + one generation
    for (const s of spans) expect(tagsOf(s)).toContain('test:lfv4-pr3_c8.x')
  })

  it('a slug at the 64-character limit is valid', async () => {
    const slug = 'a'.repeat(64)
    const run = await qaExport([behaviorSpan([[0, 'fail']])], { env: { QA_TEST_TAG: slug } })
    expect(run.code).toBe(0)
    for (const s of allSpans(run.fake)) expect(tagsOf(s)).toContain(`test:${slug}`)
  })

  it.each([
    ['an empty value', ''],
    ['a value with a space', 'lfv4 pr3'],
    ['a value longer than the slug limit', 'a'.repeat(65)],
  ])('%s: exits non-zero before sending any request', async (_case, value) => {
    const run = await qaExport([behaviorSpan([[0, 'fail']])], { env: { QA_TEST_TAG: value } })
    expect(run.code).not.toBe(0)
    expect(run.fake.requests).toHaveLength(0)
    expect(run.err.some(l => l.includes('QA_TEST_TAG'))).toBe(true)
    expect(run.summary).toBeUndefined()
  })

  it('a tagged run that sends a verdict score: the score attaches to a tagged trace', async () => {
    const span = behaviorSpan([[0, 'fail']])
    const run = await qaExport([span], { env: { QA_TEST_TAG: 'lfv4-pr3-c8' } })
    expect(run.code).toBe(0)
    const tagged = new Set(
      run.fake.roots.filter(r => tagsOf(r).includes('test:lfv4-pr3-c8')).map(r => r.traceId)
    )
    expect(run.fake.scores).toHaveLength(1)
    for (const s of run.fake.scores) expect(tagged.has(s.body.traceId)).toBe(true)
  })

  it('unset: no test: tag is sent', async () => {
    const run = await qaExport([behaviorSpan([[0, 'fail']])])
    expect(run.code).toBe(0)
    for (const s of allSpans(run.fake)) {
      expect(tagsOf(s).some(t => t.startsWith('test:'))).toBe(false)
    }
  })
})

describe('P3-9: the summary line keeps its format and every count its meaning', () => {
  it('a mixed round: shipped traces, attached screenshots, accepted generations, a FAILED trace, enqueue counts', async () => {
    const a = behaviorSpan(
      [
        [0, 'fail'],
        [1, 'pass'],
      ],
      { behaviorId: 'CON-042', gradedEvidence: graded([png('mixed')]) }
    )
    const b = behaviorSpan([[0, 'fail']], { behaviorId: 'CON-043' })
    const c = behaviorSpan([[0, 'fail']], { behaviorId: 'CON-044' })
    const run = await qaExport([a, b, c], {
      env: { QA_SALT_PASSES: '0' },
      fake: {
        // B's root is refused inside a 200, so B never ships and counts FAILED.
        rejectRootFor: id => id === itemId(b, 0),
        existingItems: [{ id: 'e1', objectId: hexTrace(itemId(a, 0)), objectType: 'TRACE' }],
        failEnqueue: id => id === hexTrace(itemId(c, 0)),
      },
    })
    expect(run.out.filter(l => SUMMARY_RE.test(l))).toHaveLength(1)
    expect(run.summary).toBe(
      'qa-export: 3 trace(s), 2 screenshot(s), 2 observation(s), 1 FAILED; ' +
        'enqueued 0/1, 1 already queued, 1 enqueue-failed'
    )
    expect(run.code).toBe(1)
    // B sent nothing past its refused root: no score, no generation, no queue item.
    const bTrace = hexTrace(itemId(b, 0))
    expect(run.fake.scores.some(s => s.body.traceId === bTrace)).toBe(false)
    expect(run.fake.generations.some(g => g.traceId === bTrace)).toBe(false)
    expect(run.fake.itemPosts.some(p => p.objectId === bTrace)).toBe(false)
  })
})

// Coordinator ruling R3, the one exception to the unchanged accounting above: a judge
// span whose start or end time is unusable cannot ship without export-time times, so
// each of its traces fails — nothing is sent for it, FAILED counts it, the command
// exits non-zero — while the rest of the round ships.
describe('R3: a judge span with an unusable time fails its traces loudly; the rest of the round ships', () => {
  it.each([
    ['a NaN start, which the span file carries as null', { start_time_unix_nano: Number.NaN }],
    ['a negative start', { start_time_unix_nano: -1 }],
    ['an end out of the representable range', { end_time_unix_nano: 1e30 }],
  ])('%s', async (_case, times) => {
    const bad = behaviorSpan(
      [
        [0, 'fail'],
        [1, 'pass'],
      ],
      { behaviorId: 'CON-050', gradedEvidence: graded([png('r3')]) }
    )
    Object.assign(bad, times)
    const good = behaviorSpan([[0, 'fail']], { behaviorId: 'CON-051' })
    const goodTrace = hexTrace(itemId(good, 0))
    const run = await qaExport([bad, good])

    expect(run.code).toBe(1)
    expect(run.out.filter(l => SUMMARY_RE.test(l))).toHaveLength(1)
    // Both item-traces of the bad span count FAILED; its usage is never attempted, so
    // it lands in no observation count.
    expect(run.summary).toBe(
      'qa-export: 1 trace(s), 0 screenshot(s), 1 observation(s), 2 FAILED; enqueued 1/1'
    )
    // Nothing is sent for the bad span: no media, no span, no score, no queue item.
    expect(run.fake.requests.filter(r => routeOf(r).startsWith('media'))).toHaveLength(0)
    expect(allSpans(run.fake).map(s => s.traceId)).toEqual([goodTrace, goodTrace])
    expect(run.fake.roots.map(r => r.labelId)).toEqual([itemId(good, 0)])
    expect(run.fake.generations.map(g => g.labelId)).toEqual([itemId(good, 0)])
    expect(run.fake.scores.map(s => s.body.traceId)).toEqual([goodTrace])
    expect(run.fake.itemPosts.map(p => p.objectId)).toEqual([goodTrace])
    // Each failure names its trace and why.
    for (const n of [0, 1]) {
      expect(
        run.out.some(
          l => l.includes(`FAILED CON-050 ${itemId(bad, n)}`) && l.includes('unix-nano timestamp')
        )
      ).toBe(true)
    }
  })
})

// Run a command to completion without blocking the event loop the fake server needs.
function runProcess(
  cmd: string,
  args: string[],
  env: NodeJS.ProcessEnv
): Promise<{ code: number | null; stdout: string; stderr: string }> {
  return new Promise(resolve => {
    execFile(cmd, args, { env, timeout: 60_000 }, (err, stdout, stderr) => {
      const code = err ? ((err as { code?: number }).code ?? 1) : 0
      resolve({ code: typeof code === 'number' ? code : 1, stdout, stderr })
    })
  })
}

describe('make qa-export: the make recipe end to end against the fake server', () => {
  it('ships over OTLP with the v4 header and the test tag, and prints the one summary line the nightly parses', async () => {
    const fake = createFakeLangfuse()
    const cfg = await fake.listen()
    try {
      const span = behaviorSpan([[0, 'fail']])
      const file = path.join(TMP, 'make.jsonl')
      fs.writeFileSync(file, JSON.stringify(span) + '\n')
      const env: NodeJS.ProcessEnv = { ...process.env }
      for (const k of Object.keys(env)) if (k.startsWith('QA_')) delete env[k]
      Object.assign(env, {
        LANGFUSE_HOST: cfg.host,
        LANGFUSE_PUBLIC_KEY: cfg.publicKey,
        LANGFUSE_SECRET_KEY: cfg.secretKey,
        QA_RUN_ID: '20261001T120000Z',
        QA_TEST_TAG: 'lfv4-pr3-make',
      })
      const repoRoot = path.join(process.cwd(), '..')
      const res = await runProcess('make', ['-C', repoRoot, 'qa-export', `TRACE=${file}`], env)
      expect(res.code, res.stderr).toBe(0)
      const summaries = res.stdout.split('\n').filter(l => SUMMARY_RE.test(l))
      expect(summaries).toEqual([
        'qa-export: 1 trace(s), 0 screenshot(s), 1 observation(s); enqueued 1/1',
      ])
      const otlp = fake.requests.filter(r => routeOf(r) === 'otlp')
      expect(otlp).toHaveLength(2) // root, then generation
      for (const r of otlp) expect(r.headers['x-langfuse-ingestion-version']).toBe('4')
      const traceId = hexTrace(itemId(span, 0))
      expect(fake.roots.map(r => r.traceId)).toEqual([traceId])
      expect(fake.roots[0].tags).toContain('test:lfv4-pr3-make')
      expect(ingestionTypes(fake)).toEqual(['score-create'])
      expect(fake.itemPosts.map(p => p.objectId)).toEqual([traceId])
    } finally {
      await fake.close()
    }
  }, 60_000)
})
