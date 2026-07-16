import * as fs from 'fs'
import * as os from 'os'
import * as path from 'path'
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { buildGenAiSpan, type GenAiSpan, type SpanParams } from '../adapter/span'
import type { PerItemVerdict } from '../adapter/types'
import type { GradedEvidenceEntry, Scenario } from '../label-trace'
import { SCREENSHOT_CAVEAT } from '../label-trace'
import { createScrubber } from '../scrub'
import {
  attachTokens,
  buildTraceBody,
  configFromEnv,
  exportSpans,
  parseSpanFile,
  traceIdFor,
} from './langfuse'

const baseParams: SpanParams = {
  impl: 'codex-exec',
  behaviorId: 'CON-042',
  model: 'gpt-5.4-mini',
  startMs: 1_000,
  endMs: 4_200,
  inputTokens: 90_000,
  outputTokens: 240,
}

const behaviorScenario: Scenario = {
  kind: 'behavior',
  behaviorId: 'CON-042',
  behaviorTitle: 'delete confirmation',
  given: 'a contact exists',
  when: 'the user deletes it',
  items: [
    { itemIndex: 0, thenText: 'warns cannot be undone' },
    { itemIndex: 2, thenText: 'removes the row' },
  ],
  allThen: ['warns cannot be undone', 'closes', 'removes the row'],
}

const twoVerdicts: PerItemVerdict[] = [
  { itemIndex: 0, verdict: 'fail', citation: 'dialog', critique: 'no warning' },
  { itemIndex: 2, verdict: 'pass', citation: 'row', critique: 'gone' },
]

function graded(screenshots?: (string | undefined)[]): GradedEvidenceEntry[] {
  return [
    {
      captureFile: '001.json',
      note: 'the dialog',
      evidence: { url: 'u0' },
      screenshot: screenshots?.[0],
    },
    {
      captureFile: '002.json',
      note: 'after',
      evidence: { url: 'u1' },
      screenshot: screenshots?.[1],
    },
  ]
}

function behaviorSpan(over: Partial<SpanParams> = {}): GenAiSpan {
  return buildGenAiSpan({
    ...baseParams,
    prompt: 'the prompt',
    response: JSON.stringify(twoVerdicts),
    scenario: behaviorScenario,
    gradedEvidence: graded(),
    itemVerdicts: twoVerdicts,
    ...over,
  })
}

describe('configFromEnv', () => {
  it('is undefined unless ALL three vars are present (export stays opt-in)', () => {
    expect(configFromEnv({})).toBeUndefined()
    expect(configFromEnv({ LANGFUSE_HOST: 'h', LANGFUSE_PUBLIC_KEY: 'p' })).toBeUndefined()
    expect(
      configFromEnv({ LANGFUSE_HOST: 'h', LANGFUSE_PUBLIC_KEY: 'p', LANGFUSE_SECRET_KEY: 's' })
    ).toEqual({ host: 'h', publicKey: 'p', secretKey: 's' })
  })
})

describe('traceIdFor', () => {
  it('is stable for a given span, so re-export overwrites rather than duplicates', () => {
    const span = buildGenAiSpan(baseParams)
    expect(traceIdFor(span)).toBe(traceIdFor(span))
    expect(traceIdFor(span)).toContain('CON-042')
  })
})

describe('buildTraceBody — the metrics-only / #379 shape (no scenario)', () => {
  it('returns ONE content-light body carrying the prompt + completion', () => {
    const span = buildGenAiSpan({
      ...baseParams,
      prompt: 'INTENT: the loop closes...',
      response: '[{"itemIndex":0,"verdict":"pass"}]',
    })
    const bodies = buildTraceBody(span)
    expect(bodies).toHaveLength(1)
    const [body] = bodies
    expect((body.input as { prompt: string }).prompt).toContain('the loop closes')
    expect(body.output).toBe('[{"itemIndex":0,"verdict":"pass"}]')
    expect(body.metadata.behavior_id).toBe('CON-042')
    expect(body.metadata.duration_ms).toBe(3_200)
    expect(body.id).toBe(traceIdFor(span)) // no -item suffix for the no-scenario shape
  })

  it('omits input entirely when the caller logged no content (#379 extraction shape)', () => {
    const [body] = buildTraceBody(buildGenAiSpan(baseParams))
    expect(body.input).toBeUndefined()
    expect(body.output).toBeUndefined()
    expect(body.metadata.output_tokens).toBe(240)
  })
})

describe('buildTraceBody — per-item fan-out (spec line 41)', () => {
  it('fans a 2-item behavior span out to TWO traces, each with a SINGULAR then_text + its own verdict', () => {
    const bodies = buildTraceBody(behaviorSpan())
    expect(bodies).toHaveLength(2)
    const [b0, b2] = bodies
    // Unique per-item trace ids.
    expect(b0.id).toMatch(/-item0$/)
    expect(b2.id).toMatch(/-item2$/)
    expect(b0.id).not.toBe(b2.id)
    // Singular then_text per trace; full then-list for context.
    const si0 = (b0.input as { scenario_item: Record<string, unknown> }).scenario_item
    expect(si0.then_text).toBe('warns cannot be undone')
    expect(si0.all_then).toEqual(['warns cannot be undone', 'closes', 'removes the row'])
    expect(si0.behavior_id).toBe('CON-042')
    const si2 = (b2.input as { scenario_item: Record<string, unknown> }).scenario_item
    expect(si2.then_text).toBe('removes the row')
    // Output = THAT item's verdict (from item_verdicts, not a completion re-parse).
    expect((b0.output as PerItemVerdict).verdict).toBe('fail')
    expect((b2.output as PerItemVerdict).verdict).toBe('pass')
    // graded_evidence shared, real capture_file, prompt kept for fidelity.
    const ge0 = (b0.input as { graded_evidence: Array<{ capture_file: string }> }).graded_evidence
    expect(ge0.map(e => e.capture_file)).toEqual(['001.json', '002.json'])
    expect((b0.input as { prompt: string }).prompt).toBe('the prompt')
  })

  it('fans an intent span out to ONE trace carrying the goal/statement + status', () => {
    const span = buildGenAiSpan({
      ...baseParams,
      behaviorId: 'DSH-010',
      prompt: 'p',
      scenario: {
        kind: 'intent',
        intentId: 'DSH-010',
        title: 'at a glance',
        statement: 'answers what needs attention',
        status: 'current',
      },
      gradedEvidence: [{ captureFile: 'a.json', note: 'n', evidence: {} }],
      itemVerdicts: [{ itemIndex: 0, verdict: 'pass', citation: 'c', critique: 'k' }],
    })
    const bodies = buildTraceBody(span)
    expect(bodies).toHaveLength(1)
    const si = (bodies[0].input as { scenario_item: Record<string, unknown> }).scenario_item
    expect(si.statement).toBe('answers what needs attention')
    expect(si.status).toBe('current')
    expect(si.given).toBeUndefined() // no GWT on the intent variant
  })

  it('reports screenshots_expected from the graded evidence; the skeleton attaches none', () => {
    const [body] = buildTraceBody(
      buildGenAiSpan({
        ...baseParams,
        scenario: { ...behaviorScenario, items: [{ itemIndex: 0, thenText: 't' }] },
        gradedEvidence: graded(['/runs/a.png', '/runs/b.png']),
        itemVerdicts: [{ itemIndex: 0, verdict: 'pass', citation: 'c', critique: 'k' }],
      })
    )
    expect(body.metadata.screenshots_expected).toBe(2)
    expect(body.metadata.screenshots_attached).toBe(0) // token-free skeleton
    const ge = (body.input as { graded_evidence: Array<{ screenshot?: string }> }).graded_evidence
    expect(ge.every(e => e.screenshot === undefined)).toBe(true) // paths never ship
  })
})

describe('buildTraceBody — derived screenshot_caveat (D6; the ONLY two states)', () => {
  it('renders mutation AND the DERIVED caveat when the evidence was doctored', () => {
    const [body] = buildTraceBody(
      behaviorSpan({
        scenario: { ...behaviorScenario, items: [{ itemIndex: 0, thenText: 't' }] },
        itemVerdicts: [{ itemIndex: 0, verdict: 'fail', citation: 'c', critique: 'k' }],
        mutation: { op: 'blank_dialog', target: { node_name: 'confirm' } },
      })
    )
    const input = body.input as { mutation?: unknown; screenshot_caveat?: string }
    expect(input.mutation).toEqual({ op: 'blank_dialog', target: { node_name: 'confirm' } })
    expect(input.screenshot_caveat).toBe(SCREENSHOT_CAVEAT)
  })

  it('omits BOTH mutation and caveat for a real (undoctored) capture', () => {
    const [body] = buildTraceBody(behaviorSpan())
    const input = body.input as { mutation?: unknown; screenshot_caveat?: string }
    expect(input.mutation).toBeUndefined()
    expect(input.screenshot_caveat).toBeUndefined()
  })
})

describe('buildTraceBody — exhaustive PII scrub (INV-2)', () => {
  // A DISTINCT email+phone sentinel in every free-form / env-sourced branch that
  // ships for a behavior span. A per-branch miss leaves a raw sentinel behind.
  const doctoredSpan = buildGenAiSpan({
    ...baseParams,
    model: 'gpt m0@synthetic.example (479) 555-0100',
    prompt: 'prompt p1@synthetic.example +1-479-555-0101',
    error: 'threw e2@synthetic.example (479) 555-0102',
    scenario: {
      kind: 'behavior',
      behaviorId: 'CON-042',
      behaviorTitle: 'title t3@synthetic.example +1-479-555-0103',
      given: 'given g4@synthetic.example',
      when: 'when w5@synthetic.example',
      items: [{ itemIndex: 0, thenText: 'then th6@synthetic.example (479) 555-0106' }],
      allThen: ['allthen at7@synthetic.example'],
    },
    gradedEvidence: [
      {
        captureFile: '001.json',
        note: 'note nt8@synthetic.example',
        evidence: {
          aria: { role: 'button', name: 'aria ar9@synthetic.example' },
          api: {
            '/api/v1/contacts': [
              {
                method: 'GET',
                requestUrl: '/api/v1/contacts',
                query: {},
                status: 200,
                body: { note: 'api ap10@synthetic.example +1-479-555-0110' },
              },
            ],
          },
        },
        // A local screenshot PATH — must NEVER ship (paths are stripped, D9).
        screenshot: '/runs/secret/leak-should-not-ship.png',
      },
    ],
    itemVerdicts: [
      {
        itemIndex: 0,
        verdict: 'fail',
        citation: 'cite ct11@synthetic.example',
        critique: 'crit cr12@synthetic.example',
      },
    ],
    mutation: {
      op: 'set_json_field',
      value: 'mut mv13@synthetic.example',
      node_name: 'nn14@synthetic.example',
    },
  })

  const intentSpan = buildGenAiSpan({
    ...baseParams,
    behaviorId: 'DSH-010',
    prompt: 'p',
    scenario: {
      kind: 'intent',
      intentId: 'DSH-010',
      // The intent title is FREE-FORM catalog prose (r6) — it must be scrubbed.
      title: 'intent-title it15@synthetic.example',
      statement: 'stmt st16@synthetic.example +1-479-555-0116',
      status: 'current',
    },
    gradedEvidence: [{ captureFile: 'a.json', note: 'n', evidence: {} }],
    itemVerdicts: [{ itemIndex: 0, verdict: 'pass', citation: 'c', critique: 'k' }],
  })

  // A no-scenario span so the completion (gen_ai.completion → output) branch — a
  // PR1 branch that only ships in the content-light shape — is still covered.
  const completionSpan = buildGenAiSpan({
    ...baseParams,
    prompt: 'p cp17@synthetic.example',
    response: 'completion co18@synthetic.example (479) 555-0118',
  })

  const noRaw = (shipped: string): void => {
    expect(shipped).not.toMatch(/@synthetic\.example/)
    expect(shipped).not.toMatch(/479-555-01\d\d/)
    expect(shipped).not.toMatch(/\(479\) 555-01\d\d/)
  }

  it('scrubs EVERY new/existing free-form branch of a doctored behavior span; no raw sentinel, no local path', () => {
    const scrubber = createScrubber()
    const bodies = buildTraceBody(doctoredSpan, s => scrubber.scrub(s))
    const shipped = JSON.stringify(bodies)
    noRaw(shipped)
    expect(shipped).toContain('<email:')
    expect(shipped).toContain('<phone:')
    // Paths never ship, even though the span carried one.
    expect(shipped).not.toContain('leak-should-not-ship.png')
  })

  it('scrubs the intent title + statement (free-form catalog prose; r6)', () => {
    const scrubber = createScrubber()
    noRaw(JSON.stringify(buildTraceBody(intentSpan, s => scrubber.scrub(s))))
  })

  it('scrubs the completion in the content-light shape (the PR1 branch)', () => {
    const scrubber = createScrubber()
    const [body] = buildTraceBody(completionSpan, s => scrubber.scrub(s))
    expect(String(body.output)).toContain('<email:')
    expect(String(body.output)).toContain('<phone:')
    noRaw(JSON.stringify(body))
  })

  it('scrubs metadata.error + env-sourced metadata.model', () => {
    const scrubber = createScrubber()
    const [body] = buildTraceBody(doctoredSpan, s => scrubber.scrub(s))
    expect(String(body.metadata.error)).toContain('<email:')
    expect(String(body.metadata.model)).toContain('<email:')
  })

  it('defaults to identity scrub, so existing callers are unaffected', () => {
    const [body] = buildTraceBody(completionSpan)
    expect(body.output).toContain('co18@synthetic.example')
  })

  it('preserves a non-string model attribute via the fallback (scrub only touches strings)', () => {
    const scrubber = createScrubber()
    const numeric = {
      ...completionSpan,
      attributes: { ...completionSpan.attributes, 'gen_ai.request.model': 42 },
    }
    expect(buildTraceBody(numeric, s => scrubber.scrub(s))[0].metadata.model).toBe(42)
  })
})

describe('attachTokens (pure)', () => {
  it('splices tokens onto graded_evidence BY INDEX and counts attached', () => {
    const [skeleton] = buildTraceBody(
      buildGenAiSpan({
        ...baseParams,
        scenario: { ...behaviorScenario, items: [{ itemIndex: 0, thenText: 't' }] },
        gradedEvidence: graded(['/a.png', '/b.png']),
        itemVerdicts: [{ itemIndex: 0, verdict: 'pass', citation: 'c', critique: 'k' }],
      })
    )
    const out = attachTokens(skeleton, ['@@tokA@@', undefined])
    const ge = (out.input as { graded_evidence: Array<{ screenshot?: string }> }).graded_evidence
    expect(ge[0].screenshot).toBe('@@tokA@@')
    expect(ge[1].screenshot).toBeUndefined()
    expect(out.metadata.screenshots_attached).toBe(1)
    // Non-mutating: the skeleton is untouched.
    const orig = (skeleton.input as { graded_evidence: Array<{ screenshot?: string }> })
      .graded_evidence
    expect(orig[0].screenshot).toBeUndefined()
  })
})

// --- exportSpans: the wire behaviour (mocked Langfuse) ---

const TMP = path.join(os.tmpdir(), `qa-lf-test-${process.pid}`)
function tmpPng(name: string, bytes: string): string {
  const p = path.join(TMP, name)
  fs.writeFileSync(p, bytes)
  return p
}

interface MockCall {
  kind: 'init' | 'body' | 'media'
  traceId?: string
  id?: string
}
interface ShippedBody {
  id: string
  input?: Record<string, unknown>
  metadata: Record<string, unknown>
}
function mockLangfuse(opts: { failFirstPut?: boolean } = {}): {
  fetchImpl: typeof fetch
  calls: MockCall[]
  bodies: ShippedBody[]
} {
  const calls: MockCall[] = []
  const bodies: ShippedBody[] = []
  const shaToId = new Map<string, string>()
  let seq = 0
  let putCount = 0
  const okText = (obj: unknown): Response =>
    ({ ok: true, status: 200, text: async () => JSON.stringify(obj) }) as Response
  const fetchImpl = (async (url: string | URL, init?: { method?: string; body?: unknown }) => {
    const u = String(url)
    const method = init?.method ?? 'GET'
    // Only the JSON APIs (ingestion / media register / PATCH) carry a JSON body;
    // the presigned PUT carries binary — never JSON.parse it.
    const json = (): Record<string, unknown> =>
      JSON.parse(String(init?.body)) as Record<string, unknown>
    if (u.endsWith('/api/public/ingestion')) {
      const evt = (json().batch as Array<{ id: string; body: ShippedBody }>)[0]
      const isInit = String(evt.id).includes('-init-')
      calls.push({ kind: isInit ? 'init' : 'body', traceId: evt.body.id, id: evt.id })
      if (!isInit) bodies.push(evt.body)
      return okText({})
    }
    if (u.endsWith('/api/public/media') && method === 'POST') {
      const body = json()
      calls.push({ kind: 'media', traceId: String(body.traceId) })
      const sha = String(body.sha256Hash)
      let id = shaToId.get(sha)
      const firstTime = !id
      if (!id) {
        id = `m${seq++}`
        shaToId.set(sha, id)
      }
      // uploadUrl only on the first registration of these bytes (sha dedup).
      return okText({ mediaId: id, ...(firstTime ? { uploadUrl: `http://up/${id}` } : {}) })
    }
    if (u.startsWith('http://up/')) {
      putCount++
      const fail = opts.failFirstPut === true && putCount === 1
      return { ok: !fail, status: fail ? 500 : 200, text: async () => '' } as Response
    }
    // PATCH finalize /api/public/media/{id}
    return okText({})
  }) as unknown as typeof fetch
  return { fetchImpl, calls, bodies }
}

const cfg = { host: 'http://lf', publicKey: 'p', secretKey: 's' }

describe('exportSpans — per-item media lifecycle + attribution', () => {
  beforeAll(() => fs.mkdirSync(TMP, { recursive: true }))
  afterAll(() => fs.rmSync(TMP, { recursive: true, force: true }))
  afterEach(() => vi.restoreAllMocks())

  it('emits init→media→body PER ITEM-TRACE, media registered against BOTH item-trace ids, HIT-deduped', async () => {
    const shot = tmpPng('seq.png', 'seq-bytes')
    const span = buildGenAiSpan({
      ...baseParams,
      prompt: 'p',
      scenario: behaviorScenario, // 2 items → 2 item-traces
      gradedEvidence: [{ captureFile: '001.json', note: 'n', evidence: {}, screenshot: shot }],
      itemVerdicts: twoVerdicts,
    })
    const mock = mockLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [span])
    expect(res.traces).toBe(2)
    expect(mock.calls.map(c => c.kind)).toEqual(['init', 'media', 'body', 'init', 'media', 'body'])
    // Media registered against each item-trace's OWN id.
    const mediaTraceIds = mock.calls.filter(c => c.kind === 'media').map(c => c.traceId)
    expect(mediaTraceIds[0]).toMatch(/-item0$/)
    expect(mediaTraceIds[1]).toMatch(/-item2$/)
    expect(mediaTraceIds[0]).not.toBe(mediaTraceIds[1])
    // Both bodies got the SAME token (sha dedup HIT), attributed at index 0.
    const tok = (b: ShippedBody): unknown =>
      (b.input!.graded_evidence as Array<{ screenshot?: string }>)[0].screenshot
    expect(tok(mock.bodies[0])).toBe(tok(mock.bodies[1]))
    expect(mock.bodies.every(b => b.metadata.screenshots_attached === 1)).toBe(true)
  })

  it('attributes tokens BY INDEX even when two captures share a basename', async () => {
    const a = tmpPng('dupA.png', 'A-bytes')
    const b = tmpPng('dupB.png', 'B-bytes')
    const span = buildGenAiSpan({
      ...baseParams,
      prompt: 'p',
      scenario: { ...behaviorScenario, items: [{ itemIndex: 0, thenText: 't' }] }, // 1 item → 1 trace
      // BOTH capture_file share the basename 'dup.json' — a Map<captureFile> would collapse them.
      gradedEvidence: [
        { captureFile: 'dup.json', note: 'n0', evidence: {}, screenshot: a },
        { captureFile: 'dup.json', note: 'n1', evidence: {}, screenshot: b },
      ],
      itemVerdicts: [{ itemIndex: 0, verdict: 'pass', citation: 'c', critique: 'k' }],
    })
    const mock = mockLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [span])
    const ge = mock.bodies[0].input!.graded_evidence as Array<{ screenshot?: string }>
    expect(ge[0].screenshot).toBeDefined()
    expect(ge[1].screenshot).toBeDefined()
    // Distinct bytes → distinct tokens → each index keeps its OWN token.
    expect(ge[0].screenshot).not.toBe(ge[1].screenshot)
    expect(mock.bodies[0].metadata.screenshots_attached).toBe(2)
  })

  it('all-or-nothing PER TRACE: a failed upload in item-trace 0 ships ZERO tokens; item-trace 1 ships all', async () => {
    const f1 = tmpPng('p1.png', 'p1-bytes')
    const f2 = tmpPng('p2.png', 'p2-bytes')
    const f3 = tmpPng('p3.png', 'p3-bytes')
    const span = buildGenAiSpan({
      ...baseParams,
      prompt: 'p',
      scenario: behaviorScenario, // 2 items → 2 item-traces
      gradedEvidence: [
        { captureFile: 'a.json', note: 'n', evidence: {}, screenshot: f1 },
        { captureFile: 'b.json', note: 'n', evidence: {}, screenshot: f2 },
        { captureFile: 'c.json', note: 'n', evidence: {}, screenshot: f3 },
      ],
      itemVerdicts: twoVerdicts,
    })
    const mock = mockLangfuse({ failFirstPut: true })
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [span])
    const byTrace = new Map(mock.bodies.map(b => [b.id, b]))
    const t0 = [...byTrace.values()].find(b => String(b.id).endsWith('-item0'))!
    const t1 = [...byTrace.values()].find(b => String(b.id).endsWith('-item2'))!
    expect(t0.metadata.screenshots_expected).toBe(3)
    expect(t0.metadata.screenshots_attached).toBe(0) // one upload failed → zero tokens
    const ge0 = t0.input!.graded_evidence as Array<{ screenshot?: string }>
    expect(ge0.every(e => e.screenshot === undefined)).toBe(true)
    // Sibling trace is INDEPENDENT and ships all three (sha-dedup HITs).
    expect(t1.metadata.screenshots_attached).toBe(3)
  })

  it('happy path attributes all screenshots by index on every item-trace', async () => {
    const f1 = tmpPng('h1.png', 'h1-bytes')
    const f2 = tmpPng('h2.png', 'h2-bytes')
    const span = buildGenAiSpan({
      ...baseParams,
      prompt: 'p',
      scenario: { ...behaviorScenario, items: [{ itemIndex: 0, thenText: 't' }] },
      gradedEvidence: [
        { captureFile: 'a.json', note: 'n', evidence: {}, screenshot: f1 },
        { captureFile: 'b.json', note: 'n', evidence: {}, screenshot: f2 },
      ],
      itemVerdicts: [{ itemIndex: 0, verdict: 'pass', citation: 'c', critique: 'k' }],
    })
    const mock = mockLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [span])
    expect(res.screenshots).toBe(2)
    expect(mock.bodies[0].metadata.screenshots_attached).toBe(2)
  })
})

describe('exportSpans — event-id nonce (INV-6) + shared scrubber (INV-2)', () => {
  afterEach(() => vi.restoreAllMocks())

  it('a re-export reuses the STABLE trace id but with FRESH event ids (nonce), so it overwrites not drops', async () => {
    const span = buildGenAiSpan({ ...baseParams, prompt: 'p' }) // no-scenario → 1 trace
    const mock = mockLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [span])
    await exportSpans(cfg, [span])
    const inits = mock.calls.filter(c => c.kind === 'init')
    const bodies = mock.calls.filter(c => c.kind === 'body')
    // Same stable trace id both runs...
    expect(inits[0].traceId).toBe(inits[1].traceId)
    expect(bodies[0].traceId).toBe(bodies[1].traceId)
    // ...but every EVENT id is unique (nonce per submission), so neither is dropped.
    const eventIds = mock.calls.map(c => c.id)
    expect(new Set(eventIds).size).toBe(eventIds.length)
  })

  it('uses ONE scrubber across the run so DISTINCT+overlapping PII maps consistently and nothing raw ships', async () => {
    // Distinct emails/phones across the two spans, WITH an overlap (dup@) — a
    // per-span or per-item scrubber would number the overlap inconsistently and
    // a per-span regression would leak. (PR1 carry-forward hardening.)
    const spans = [
      buildGenAiSpan({
        ...baseParams,
        behaviorId: 'CON-001',
        prompt: 'first a1@synthetic.example dup@synthetic.example +1-479-555-0101',
      }),
      buildGenAiSpan({
        ...baseParams,
        behaviorId: 'CON-002',
        prompt: 'second b2@synthetic.example dup@synthetic.example +1-479-555-0102',
      }),
    ]
    const mock = mockLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, spans)
    expect(res.traces).toBe(2)
    const prompts = mock.bodies
      .map(b => b.input?.prompt as string | undefined)
      .filter(Boolean) as string[]
    expect(prompts).toHaveLength(2)
    const shipped = prompts.join('\n')
    expect(shipped).not.toContain('@synthetic.example')
    expect(shipped).not.toMatch(/479-555-01\d\d/)
    // The shared/overlapping email maps to the SAME placeholder in BOTH prompts.
    const dupPlaceholders = prompts.map(p => {
      const others = new Set(p.match(/<email:\d+>/g) ?? [])
      return others
    })
    // Every prompt contains 2 distinct email placeholders; the overlap is shared,
    // so across both prompts the union is 3 (a1, b2, dup), not 4.
    const union = new Set(prompts.flatMap(p => p.match(/<email:\d+>/g) ?? []))
    expect(union.size).toBe(3)
    expect(dupPlaceholders[0].size).toBe(2)
  })
})

describe('parseSpanFile', () => {
  it('skips malformed lines — a partial trace is still worth shipping', () => {
    const good = JSON.stringify(buildGenAiSpan(baseParams))
    const spans = parseSpanFile(`${good}\n{ not json\n\n${good}\n`)
    expect(spans).toHaveLength(2)
  })
})
