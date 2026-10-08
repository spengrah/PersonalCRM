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
  createFakeLangfuse,
  TRIAGE_QUEUE_ID,
  VERDICT_CONFIG_ID,
  type FakeLangfuse,
  type QueueObj,
  type ScoreConfigObj,
  type ShippedRoot,
} from './fake-langfuse'
import {
  api,
  apiGetAllPages,
  attachTokens,
  buildGenerationBody,
  buildTraceBody,
  configFromEnv,
  exportSpans,
  generationSpanId,
  hexTraceId,
  ingestionOutcome,
  otlpRejection,
  parseSpanFile,
  rootSpanId,
  stableSample,
  traceIdFor,
  usageTraceId,
} from './langfuse'
import { TRIAGE_QUEUE_NAME, VERDICT_SCORE_NAME } from './triage-config'

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
  afterEach(() => vi.restoreAllMocks())

  // A DISTINCT email (+ phone on several) sentinel in EVERY free-form /
  // env-sourced branch that can ship — scalar attributes AND every nested
  // evidence branch (url, aria name/text/nested-child, api requestUrl/query/body,
  // dialogs, serverTime strings) AND every mutation string position. A per-branch
  // miss leaves its own raw sentinel behind, pinpointing the leak.
  const S = {
    model: 'mdl-s01@synthetic.example (479) 555-0101',
    prompt: 'prm-s02@synthetic.example +1-479-555-0102',
    error: 'err-s03@synthetic.example (479) 555-0103',
    title: 'ttl-s04@synthetic.example',
    given: 'gvn-s05@synthetic.example',
    when: 'whn-s06@synthetic.example',
    then: 'thn-s07@synthetic.example +1-479-555-0107',
    allThen: 'all-s08@synthetic.example',
    note: 'not-s09@synthetic.example',
    ariaName: 'arn-s10@synthetic.example',
    ariaText: 'art-s11@synthetic.example',
    ariaChildName: 'acn-s12@synthetic.example',
    url: 'https://host/path/url-s13@synthetic.example',
    apiRequestUrl: '/api/v1/x?u=aru-s14@synthetic.example',
    apiQuery: 'aqy-s15@synthetic.example',
    apiBody: 'aby-s16@synthetic.example +1-479-555-0116',
    dialog: 'dlg-s17@synthetic.example +1-479-555-0117',
    serverCurrent: 'sct-s18@synthetic.example',
    serverBase: 'sbt-s19@synthetic.example',
    serverEnv: 'sev-s20@synthetic.example',
    citation: 'cit-s21@synthetic.example',
    critique: 'crt-s22@synthetic.example',
    // Every string-bearing MutationSchema position (judge/mutation.ts), by its
    // REAL property name — target.role, param, value, endpoint, node_role,
    // node_name, field, and each path[] element.
    mutRole: 'mrl-s23@synthetic.example',
    mutParam: 'mpm-s24@synthetic.example',
    mutValue: 'mvl-s25@synthetic.example +1-479-555-0125',
    mutEndpoint: 'men-s26@synthetic.example',
    mutNodeRole: 'mnr-s27@synthetic.example',
    mutNodeName: 'mnn-s28@synthetic.example',
    mutField: 'mfd-s29@synthetic.example',
    mutPath0: 'mp0-s30@synthetic.example',
    mutPath1: 'mp1-s31@synthetic.example',
  }
  const SENTINELS = Object.values(S)

  const doctoredSpan = buildGenAiSpan({
    ...baseParams,
    model: S.model,
    prompt: S.prompt,
    error: S.error,
    scenario: {
      kind: 'behavior',
      behaviorId: 'CON-042',
      behaviorTitle: S.title,
      given: S.given,
      when: S.when,
      items: [{ itemIndex: 0, thenText: S.then }],
      allThen: [S.allThen],
    },
    gradedEvidence: [
      {
        captureFile: '001.json',
        note: S.note,
        evidence: {
          url: S.url,
          aria: {
            role: 'button',
            name: S.ariaName,
            children: [
              { role: 'text', text: S.ariaText },
              { role: 'group', name: S.ariaChildName, children: [] },
            ],
          },
          api: {
            '/api/v1/contacts': [
              {
                method: 'GET',
                requestUrl: S.apiRequestUrl,
                query: { search: S.apiQuery },
                status: 200,
                body: { note: S.apiBody },
              },
            ],
          },
          serverTime: {
            currentTime: S.serverCurrent,
            isAccelerated: true,
            accelerationFactor: 1,
            baseTime: S.serverBase,
            environment: S.serverEnv,
          },
          dialogs: [{ type: 'confirm', message: S.dialog }],
        },
        // A local screenshot PATH — must NEVER ship (paths are stripped, D9).
        screenshot: '/runs/secret/leak-should-not-ship.png',
      },
    ],
    itemVerdicts: [{ itemIndex: 0, verdict: 'fail', citation: S.citation, critique: S.critique }],
    // A SUPERSET carrying every REAL string-bearing MutationSchema property name
    // (the exporter deep-scrubs `mutation` regardless of op, so this proves each
    // real position is covered; `op` is a closed enum, not scrubbed). `path` is
    // an array — two elements prove array members are walked too.
    mutation: {
      op: 'set_json_field',
      role: S.mutRole,
      param: S.mutParam,
      value: S.mutValue,
      endpoint: S.mutEndpoint,
      node_role: S.mutNodeRole,
      node_name: S.mutNodeName,
      field: S.mutField,
      path: [S.mutPath0, S.mutPath1],
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

  it('no sentinel from ANY free-form branch survives ANY transported request; no local path ships', async () => {
    // Assert on what actually goes over the wire (exportSpans → the fake), not just
    // buildTraceBody, so the whole scrub seam is exercised end to end.
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [doctoredSpan])
    expect(res.traces).toBe(1)
    const shipped = JSON.stringify(mock.requests.map(r => r.body))
    // EVERY per-branch sentinel is gone (a leak names its own branch via S).
    for (const raw of SENTINELS) {
      expect(shipped, `sentinel leaked: ${raw}`).not.toContain(raw)
    }
    noRaw(shipped)
    expect(shipped).toContain('<email:')
    expect(shipped).toContain('<phone:')
    // Paths never ship, even though the span carried one (the fake path also fails
    // fs.existsSync → zero tokens, but the point is the skeleton strips it).
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

// --- exportSpans: the wire behaviour (the shared fake Langfuse, stubbed in as fetch) ---
//
// `createFakeLangfuse` (fake-langfuse.ts) decodes what was transported: `roots` are
// the OTLP root spans, each with its string id (`labelId`) and the legacy trace
// body's fields read back off its attributes; `generations`, `scores`, `itemPosts`
// and `order` record the rest. The command-level contract lives in qa-export.test.ts.

const TMP = path.join(os.tmpdir(), `qa-lf-test-${process.pid}`)
function tmpPng(name: string, bytes: string): string {
  const p = path.join(TMP, name)
  fs.writeFileSync(p, bytes)
  return p
}

const activeVerdictConfig: ScoreConfigObj = {
  id: VERDICT_CONFIG_ID,
  name: VERDICT_SCORE_NAME,
  isArchived: false,
  dataType: 'CATEGORICAL',
}
const triageQueue: QueueObj = { id: TRIAGE_QUEUE_ID, name: TRIAGE_QUEUE_NAME, scoreConfigIds: [] }

// The media and root sends, in order, per trace — the per-trace lifecycle.
const lifecycle = (mock: FakeLangfuse): Array<'media' | 'root'> =>
  mock.order.flatMap(o => (o.kind === 'media' || o.kind === 'root' ? [o.kind] : []))

const cfg = { host: 'http://lf', publicKey: 'p', secretKey: 's' }

describe('exportSpans — per-item media lifecycle + attribution', () => {
  beforeAll(() => fs.mkdirSync(TMP, { recursive: true }))
  afterAll(() => fs.rmSync(TMP, { recursive: true, force: true }))
  afterEach(() => vi.restoreAllMocks())

  it('emits media→root PER ITEM-TRACE, media registered against BOTH item-trace ids, HIT-deduped', async () => {
    const shot = tmpPng('seq.png', 'seq-bytes')
    const span = buildGenAiSpan({
      ...baseParams,
      prompt: 'p',
      scenario: behaviorScenario, // 2 items → 2 item-traces
      gradedEvidence: [{ captureFile: '001.json', note: 'n', evidence: {}, screenshot: shot }],
      itemVerdicts: twoVerdicts,
    })
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [span])
    expect(res.traces).toBe(2)
    expect(lifecycle(mock)).toEqual(['media', 'root', 'media', 'root'])
    // Media registered against each item-trace's OWN id.
    const mediaTraceIds = mock.order.filter(o => o.kind === 'media').map(o => o.traceId)
    expect(mediaTraceIds[0]).toBe(hexTraceId(`${traceIdFor(span)}-item0`))
    expect(mediaTraceIds[1]).toBe(hexTraceId(`${traceIdFor(span)}-item2`))
    expect(mediaTraceIds[0]).not.toBe(mediaTraceIds[1])
    // Both bodies got the SAME token (sha dedup HIT), attributed at index 0.
    const tok = (b: ShippedRoot): unknown =>
      (b.input!.graded_evidence as Array<{ screenshot?: string }>)[0].screenshot
    expect(tok(mock.roots[0])).toBe(tok(mock.roots[1]))
    expect(mock.roots.every(b => b.metadata.screenshots_attached === 1)).toBe(true)
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
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [span])
    const ge = mock.roots[0].input!.graded_evidence as Array<{ screenshot?: string }>
    expect(ge[0].screenshot).toBeDefined()
    expect(ge[1].screenshot).toBeDefined()
    // Distinct bytes → distinct tokens → each index keeps its OWN token.
    expect(ge[0].screenshot).not.toBe(ge[1].screenshot)
    expect(mock.roots[0].metadata.screenshots_attached).toBe(2)
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
    const mock = createFakeLangfuse({ failFirstPut: true })
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [span])
    const t0 = mock.roots.find(b => b.labelId.endsWith('-item0'))!
    const t1 = mock.roots.find(b => b.labelId.endsWith('-item2'))!
    expect(t0.metadata.screenshots_expected).toBe(3)
    expect(t0.metadata.screenshots_attached).toBe(0) // one upload failed → zero tokens
    const ge0 = t0.input!.graded_evidence as Array<{ screenshot?: string }>
    expect(ge0.every(e => e.screenshot === undefined)).toBe(true)
    // Sibling trace is INDEPENDENT and ships all three (sha-dedup HITs).
    expect(t1.metadata.screenshots_attached).toBe(3)
  })

  it('a THROWN media registration (non-2xx POST /media) still ships the trace with ZERO tokens (INV-4)', async () => {
    // The P1 regression: a registration THROW must not escape to the outer catch
    // and DROP the trace — the trace must ship with expected=N / attached=0.
    const f1 = tmpPng('r1.png', 'r1-bytes')
    const f2 = tmpPng('r2.png', 'r2-bytes')
    const span = buildGenAiSpan({
      ...baseParams,
      prompt: 'p',
      scenario: { ...behaviorScenario, items: [{ itemIndex: 0, thenText: 't' }] }, // 1 trace
      gradedEvidence: [
        { captureFile: 'a.json', note: 'n', evidence: {}, screenshot: f1 },
        { captureFile: 'b.json', note: 'n', evidence: {}, screenshot: f2 },
      ],
      itemVerdicts: [{ itemIndex: 0, verdict: 'pass', citation: 'c', critique: 'k' }],
    })
    const mock = createFakeLangfuse({ failFirstRegister: true })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [span])
    // Trace SHIPPED (not dropped): traces=1, failed=0.
    expect(res.traces).toBe(1)
    expect(res.failed).toBe(0)
    expect(mock.roots).toHaveLength(1)
    expect(mock.roots[0].metadata.screenshots_expected).toBe(2)
    expect(mock.roots[0].metadata.screenshots_attached).toBe(0)
    const ge = mock.roots[0].input!.graded_evidence as Array<{ screenshot?: string }>
    expect(ge.every(e => e.screenshot === undefined)).toBe(true)
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
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [span])
    expect(res.screenshots).toBe(2)
    expect(mock.roots[0].metadata.screenshots_attached).toBe(2)
  })
})

describe('exportSpans — re-export identity (INV-6) + shared scrubber (INV-2)', () => {
  afterEach(() => vi.restoreAllMocks())

  it('a re-export sends the SAME trace id, span id and start time, so it overwrites rather than duplicates', async () => {
    const span = buildGenAiSpan({ ...baseParams, prompt: 'p' }) // no-scenario → 1 trace
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [span])
    await exportSpans(cfg, [span])
    expect(mock.roots).toHaveLength(2)
    const [first, second] = mock.roots
    expect(second.traceId).toBe(first.traceId)
    expect(second.spanId).toBe(first.spanId)
    expect(second.startTimeUnixNano).toBe(first.startTimeUnixNano)
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
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, spans)
    expect(res.traces).toBe(2)
    const prompts = mock.roots
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

// --- trace tags + session, verdict-as-score, triage enqueue, salt ---

// A single-item behavior span with a chosen verdict (and optional mutation), so the
// triage tests can shape a round precisely. No screenshots → no media lifecycle.
function itemSpan(verdict: 'pass' | 'fail' | 'unsure', over: Partial<SpanParams> = {}): GenAiSpan {
  const scenario: Scenario = {
    kind: 'behavior',
    behaviorId: 'CON-042',
    behaviorTitle: 'delete confirmation',
    given: 'a contact exists',
    when: 'the user deletes it',
    items: [{ itemIndex: 0, thenText: 't' }],
    allThen: ['t'],
  }
  return buildGenAiSpan({
    ...baseParams,
    prompt: 'p',
    scenario,
    gradedEvidence: [{ captureFile: 'a.json', note: 'n', evidence: {} }],
    itemVerdicts: [{ itemIndex: 0, verdict, citation: 'c', critique: 'k' }],
    ...over,
  })
}
// The OTLP trace id of a single-item span's one trace.
const item0Id = (span: GenAiSpan): string => hexTraceId(`${traceIdFor(span)}-item0`)

describe('exportSpans — trace tags + session (contract #3, component-wise)', () => {
  afterEach(() => vi.restoreAllMocks())

  it('valid runId+gitSha → final body carries sessionId + runId/gitSha/behavior tags', async () => {
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [itemSpan('pass')], () => {}, {
      runId: '20260717T151639Z',
      gitSha: 'abc1234',
    })
    const b = mock.roots[0]
    expect(b.sessionId).toBe('20260717T151639Z')
    expect(b.tags).toEqual(
      expect.arrayContaining(['behavior:CON-042', 'runId:20260717T151639Z', 'gitSha:abc1234'])
    )
  })

  it('only gitSha present → gitSha+behavior tags, NO session/runId tag (components independent)', async () => {
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [itemSpan('pass')], () => {}, { gitSha: 'deadbeef' })
    const b = mock.roots[0]
    expect(b.sessionId).toBeUndefined()
    expect(b.tags).toContain('gitSha:deadbeef')
    expect(b.tags).toContain('behavior:CON-042')
    expect(b.tags?.some(t => t.startsWith('runId:'))).toBe(false)
  })

  it('only runId present → session + runId + behavior, NO gitSha tag', async () => {
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [itemSpan('pass')], () => {}, { runId: '20260717T151639Z' })
    const b = mock.roots[0]
    expect(b.sessionId).toBe('20260717T151639Z')
    expect(b.tags).toContain('runId:20260717T151639Z')
    expect(b.tags?.some(t => t.startsWith('gitSha:'))).toBe(false)
  })

  it('NO provenance → behavior tag is ALWAYS present, no session (never blocks shipping)', async () => {
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [itemSpan('pass')])
    expect(res.traces).toBe(1)
    // The pass + model dimensions ride every trace regardless of provenance.
    expect(mock.roots[0].tags).toEqual(['behavior:CON-042', 'pass:ux', 'model:gpt-5.4-mini'])
    expect(mock.roots[0].sessionId).toBeUndefined()
  })
})

describe('exportSpans — verdict-as-score (contract #4, separate non-fatal request)', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    vi.useRealTimers()
  })

  it('a fail item emits a SEPARATE score-create AFTER the root span: envelope ts=span-start ISO, stable body id, bound configId, no body ts, no comment', async () => {
    const span = itemSpan('fail', { startMs: 1_000 })
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [span])
    expect(mock.scores).toHaveLength(1)
    const s = mock.scores[0]
    const tid = item0Id(span)
    expect(s.type).toBe('score-create')
    expect(s.id.startsWith(`evt-${tid}-score-`)).toBe(true)
    expect(s.timestamp).toBe(new Date(span.start_time_unix_nano / 1e6).toISOString())
    expect(s.body).toMatchObject({
      id: `score-${tid}-verdict`,
      name: VERDICT_SCORE_NAME,
      value: 'fail',
      dataType: 'CATEGORICAL',
      traceId: tid,
      configId: 'cfg-verdict',
    })
    // Timestamp lives on the ENVELOPE, not the body; the score carries NO free-text
    // comment (INV-D).
    expect((s.body as Record<string, unknown>).timestamp).toBeUndefined()
    expect((s.body as Record<string, unknown>).comment).toBeUndefined()
    // Chronological proof: the score for this trace is POSTed strictly AFTER its root
    // (the separate, post-ship request contract #4 mandates — never co-batched).
    expect(mock.order).toEqual([
      { kind: 'root', traceId: tid },
      { kind: 'score', traceId: tid },
      // The usage observation follows both, after the span's per-body loop closes.
      { kind: 'generation', traceId: tid },
    ])
  })

  it('pass → value pass, unsure → value unsure', async () => {
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [itemSpan('pass'), itemSpan('unsure')])
    expect(mock.scores.map(s => s.body.value).sort()).toEqual(['pass', 'unsure'])
  })

  it('configId resolves LAZILY + ONCE: two verdicts → one score-config fetch; a verdictless round → zero', async () => {
    const withVerdicts = createFakeLangfuse()
    vi.stubGlobal('fetch', withVerdicts.fetchImpl)
    await exportSpans(cfg, [itemSpan('fail'), itemSpan('pass')])
    expect(withVerdicts.counts.configResolve).toBe(1) // resolved once, cached
    vi.restoreAllMocks()

    const noVerdict = createFakeLangfuse()
    vi.stubGlobal('fetch', noVerdict.fetchImpl)
    await exportSpans(cfg, [buildGenAiSpan({ ...baseParams, prompt: 'p' })])
    expect(noVerdict.counts.configResolve).toBe(0) // never touched without a verdict
  })

  it('binds the verdict config even when it is on page TWO of the score-config list', async () => {
    const decoy: ScoreConfigObj = { id: 'cfg-other', name: 'ground_truth', isArchived: false }
    const mock = createFakeLangfuse({
      scoreConfigs: [decoy, activeVerdictConfig],
      configsPerPage: 1,
    })
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [itemSpan('fail')])
    expect(mock.scores[0].body.configId).toBe('cfg-verdict')
  })

  it('a metrics-only (no-verdict, non-mutation) trace emits NO score AND ZERO queue POSTs (INV-F)', async () => {
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    // No scenario → the content-light shape → output is a completion/undefined, not a verdict.
    const res = await exportSpans(cfg, [buildGenAiSpan({ ...baseParams, prompt: 'p' })])
    expect(res.traces).toBe(1)
    expect(mock.scores).toHaveLength(0)
    expect(mock.itemPosts).toHaveLength(0)
  })

  it('a malformed span start_time_unix_nano FAILS that trace loudly and sends nothing for it; the rest of the round ships (P3-2)', async () => {
    // There is no span-derived time to send, and the export clock would move a
    // re-export's history — so the trace counts FAILED (a non-zero exit) instead of
    // shipping under export time. Nothing is sent for it: no root, score, generation
    // or queue item. The well-formed span alongside it ships and enqueues as usual.
    const bad = itemSpan('fail', { ...usageParams, behaviorId: 'BAD-1' })
    bad.start_time_unix_nano = Number.NaN
    const good = itemSpan('fail', { ...usageParams, behaviorId: 'OK-1' })
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    const logs: string[] = []
    const res = await exportSpans(cfg, [bad, good], m => logs.push(m))
    expect(res.failed).toBe(1)
    expect(res.traces).toBe(1)
    expect(res.observations).toBe(1)
    expect(res.observationsFailed + res.observationsSkipped).toBe(0) // no carrier, no attempt
    expect(mock.roots.map(r => r.traceId)).toEqual([item0Id(good)])
    expect(mock.generations.map(g => g.traceId)).toEqual([item0Id(good)])
    expect(mock.scores.map(s => s.body.traceId)).toEqual([item0Id(good)])
    expect(mock.itemPosts.map(p => p.objectId)).toEqual([item0Id(good)])
    expect(logs.some(l => l.includes('FAILED BAD-1') && l.includes('not a unix-nano'))).toBe(true)
  })

  it('the envelope timestamp is span-derived + STABLE across a UTC-date-crossing re-export', async () => {
    const span = itemSpan('fail', { startMs: 5_000 })
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-01-01T23:59:30Z'))
    await exportSpans(cfg, [span])
    vi.setSystemTime(new Date('2026-01-02T00:00:30Z'))
    await exportSpans(cfg, [span])
    const expectedTs = new Date(span.start_time_unix_nano / 1e6).toISOString()
    expect(mock.scores).toHaveLength(2)
    expect(mock.scores[0].timestamp).toBe(expectedTs)
    expect(mock.scores[1].timestamp).toBe(expectedTs) // same despite crossing UTC midnight
    expect(mock.scores[0].body.id).toBe(mock.scores[1].body.id) // stable body id → overwrite
    expect(mock.scores[0].id).not.toBe(mock.scores[1].id) // fresh event id → not dropped
  })

  it('a rejecting score-create does NOT fail the trace ship', async () => {
    const mock = createFakeLangfuse({ scoreError: true })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [itemSpan('fail')], () => {})
    expect(res.traces).toBe(1)
    expect(res.failed).toBe(0)
  })
})

describe('exportSpans — resolver failures are non-fatal', () => {
  afterEach(() => vi.restoreAllMocks())

  it('(a) score-config resolve 5xx → traces ship, score emitted UNBOUND', async () => {
    const mock = createFakeLangfuse({ configError: true })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [itemSpan('fail')], () => {})
    expect(res.traces).toBe(1)
    expect(res.failed).toBe(0)
    expect(mock.scores).toHaveLength(1)
    expect(mock.scores[0].body.configId).toBeUndefined()
  })

  it('(b) queue-list 5xx AFTER traces shipped → trace result intact, enqueue skipped, no throw', async () => {
    const mock = createFakeLangfuse({ queueError: true })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [itemSpan('fail')], () => {})
    expect(res.traces).toBe(1)
    expect(res.failed).toBe(0)
    expect(res.enqueue.attempted).toBe(0)
    expect(mock.itemPosts).toHaveLength(0)
  })

  it('(c) existing-items-list 5xx → trace result intact, enqueue skipped', async () => {
    const mock = createFakeLangfuse({ itemsError: true })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [itemSpan('fail')], () => {})
    expect(res.traces).toBe(1)
    expect(res.failed).toBe(0)
    expect(res.enqueue.attempted).toBe(0)
  })

  it('(d) multiple active verdict configs → score UNBOUND; multiple queues → enqueue skipped', async () => {
    const twoConfigs = createFakeLangfuse({
      scoreConfigs: [activeVerdictConfig, { ...activeVerdictConfig, id: 'cfg-2' }],
    })
    vi.stubGlobal('fetch', twoConfigs.fetchImpl)
    await exportSpans(cfg, [itemSpan('fail')], () => {})
    expect(twoConfigs.scores[0].body.configId).toBeUndefined()
    vi.restoreAllMocks()

    const twoQueues = createFakeLangfuse({ queues: [triageQueue, { ...triageQueue, id: 'q-2' }] })
    vi.stubGlobal('fetch', twoQueues.fetchImpl)
    const res = await exportSpans(cfg, [itemSpan('fail')], () => {})
    expect(res.failed).toBe(0)
    expect(res.enqueue.attempted).toBe(0)
  })

  it('(e) ZERO / archived-only verdict config → score UNBOUND, still ships', async () => {
    const none = createFakeLangfuse({ scoreConfigs: [] })
    vi.stubGlobal('fetch', none.fetchImpl)
    const res = await exportSpans(cfg, [itemSpan('fail')], () => {})
    expect(res.traces).toBe(1)
    expect(none.scores[0].body.configId).toBeUndefined()
    vi.restoreAllMocks()

    // An archived same-named config is NOT active → also unbound (isArchived !== false).
    const archived = createFakeLangfuse({
      scoreConfigs: [{ ...activeVerdictConfig, isArchived: true }],
    })
    vi.stubGlobal('fetch', archived.fetchImpl)
    await exportSpans(cfg, [itemSpan('fail')], () => {})
    expect(archived.scores[0].body.configId).toBeUndefined()
  })
})

describe('exportSpans — enqueue predicate (INV-B/F, explicit closed union)', () => {
  afterEach(() => vi.restoreAllMocks())

  it('round of {2 fail, 1 unsure, 1 mutation-PASS trap, 5 pass}, salt=3 → 2+1+1+3 = 7, all TRACE', async () => {
    const spans = [
      itemSpan('fail', { behaviorId: 'F-1' }),
      itemSpan('fail', { behaviorId: 'F-2' }),
      itemSpan('unsure', { behaviorId: 'U-1' }),
      itemSpan('pass', { behaviorId: 'T-1', mutation: { op: 'blank_dialog' } }),
      ...Array.from({ length: 5 }, (_, i) => itemSpan('pass', { behaviorId: `P-${i}` })),
    ]
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, spans, () => {}, {
      saltPasses: 3,
      runId: '20260717T151639Z',
    })
    expect(res.enqueue.enqueued).toBe(7)
    expect(res.enqueue.attempted).toBe(7)
    expect(mock.itemPosts).toHaveLength(7)
    expect(mock.itemPosts.every(p => p.objectType === 'TRACE')).toBe(true)
  })

  it('a single mutation trace judged PASS is ALWAYS enqueued as a trap (INV-B)', async () => {
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [itemSpan('pass', { mutation: { op: 'x' } })], () => {}, {
      saltPasses: 0, // no salt — proves the enqueue is the TRAP, not a salted pass
    })
    expect(res.enqueue.enqueued).toBe(1)
    expect(mock.itemPosts[0].objectType).toBe('TRACE')
  })

  it('an all-PASS non-mutation round with saltPasses 0 issues ZERO queue POSTs', async () => {
    // The shape the live smoke ships: synthetic passes must never land in the queue a
    // human uses for real labeling, and zero-salt is what keeps them out.
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(
      cfg,
      [itemSpan('pass', usageParams), itemSpan('pass', { ...usageParams, behaviorId: 'S-2' })],
      () => {},
      { saltPasses: 0 }
    )
    expect(mock.itemPosts).toHaveLength(0)
    expect(res.enqueue.attempted).toBe(0)
    expect(res.enqueue.enqueued).toBe(0)
  })

  it('a mutation trace with NO verdict (verdict === undefined) STILL enqueues as a trap (INV-B)', async () => {
    // A doctored span with no scenario → no per-item verdict. Enqueue must fire on the
    // mutation flag ALONE — never on a `verdict !== 'pass'` negation, which would also
    // (wrongly) admit a verdictless NON-mutation trace.
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    const trap = buildGenAiSpan({ ...baseParams, prompt: 'p', mutation: { op: 'blank' } })
    const res = await exportSpans(cfg, [trap], () => {}, { saltPasses: 0 })
    expect(mock.scores).toHaveLength(0) // no verdict → no score
    expect(res.enqueue.enqueued).toBe(1) // but still enqueued (mutation)
    expect(mock.itemPosts[0].objectType).toBe('TRACE')
  })
})

describe('exportSpans — enqueue idempotency + (objectType,objectId) key', () => {
  afterEach(() => vi.restoreAllMocks())

  it('re-export whose items are already queued → ZERO duplicate POSTs; skipped-existing counts', async () => {
    const span = itemSpan('fail')
    const tid = item0Id(span)
    const mock = createFakeLangfuse({
      existingItems: [{ id: 'e1', objectId: tid, objectType: 'TRACE', status: 'PENDING' }],
    })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [span], () => {})
    expect(mock.itemPosts).toHaveLength(0)
    expect(res.enqueue.skippedExisting).toBe(1)
    expect(res.enqueue.attempted).toBe(0)
  })

  it('an existing OBSERVATION item with the SAME id does NOT suppress the TRACE enqueue', async () => {
    const span = itemSpan('fail')
    const tid = item0Id(span)
    const mock = createFakeLangfuse({
      existingItems: [{ id: 'e1', objectId: tid, objectType: 'OBSERVATION' }],
    })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [span], () => {})
    expect(mock.itemPosts).toHaveLength(1)
    expect(res.enqueue.enqueued).toBe(1)
    expect(res.enqueue.skippedExisting).toBe(0)
  })

  it('a per-item POST failure does NOT abort the rest; attempted/enqueued/failed reported', async () => {
    const spans = [
      itemSpan('fail', { behaviorId: 'F-1' }),
      itemSpan('fail', { behaviorId: 'F-2' }),
      itemSpan('fail', { behaviorId: 'F-3' }),
    ]
    const mock = createFakeLangfuse({ failEnqueue: (_id, n) => n === 1 })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, spans, () => {})
    expect(mock.itemPosts).toHaveLength(3) // all attempted, none short-circuited
    expect(res.enqueue.attempted).toBe(3)
    expect(res.enqueue.failed).toBe(1)
    expect(res.enqueue.enqueued).toBe(2)
    expect(res.failed).toBe(0) // an enqueue POST failure never touches trace-ship health
  })
})

describe('exportSpans — queue pagination + PaginationError best-effort (INV-A)', () => {
  afterEach(() => vi.restoreAllMocks())

  it('resolves qa-triage on page 2 of the queue list AND walks all item pages to dedup', async () => {
    const span = itemSpan('fail')
    const tid = item0Id(span)
    const mock = createFakeLangfuse({
      queues: [{ id: 'q-other', name: 'other-queue' }, triageQueue],
      queuesPerPage: 1, // qa-triage is on page 2
      existingItems: [
        { id: 'e1', objectId: 'zzz', objectType: 'TRACE' },
        { id: 'e2', objectId: tid, objectType: 'TRACE' }, // on page 2 of items
      ],
      itemsPerPage: 1,
    })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [span], () => {})
    expect(mock.itemPosts).toHaveLength(0) // tid found via the full item walk → skipped
    expect(res.enqueue.skippedExisting).toBe(1)
  })

  it('a PaginationError resolving the queue → caught, enqueue skipped, results unchanged', async () => {
    const mock = createFakeLangfuse({ queueMalformed: true })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [itemSpan('fail')], () => {})
    expect(res.traces).toBe(1)
    expect(res.failed).toBe(0)
    expect(res.enqueue.attempted).toBe(0)
  })

  it('a PaginationError reading existing items → caught, enqueue skipped', async () => {
    const mock = createFakeLangfuse({ itemsMalformed: true })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [itemSpan('fail')], () => {})
    expect(res.traces).toBe(1)
    expect(res.failed).toBe(0)
    expect(res.enqueue.attempted).toBe(0)
  })

  it('no qa-triage queue found → logged once, export returns, not failed (INV-A)', async () => {
    const logs: string[] = []
    const mock = createFakeLangfuse({ queues: [] })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [itemSpan('fail')], m => logs.push(m))
    expect(res.traces).toBe(1)
    expect(res.failed).toBe(0)
    expect(res.enqueue.attempted).toBe(0)
    expect(logs.some(l => l.includes('enqueue') && l.includes('skipped'))).toBe(true)
  })
})

describe('stableSample (pure, deterministic salt sampler)', () => {
  it('matches the GOLDEN SHA-256 selection (pins the exact algorithm, not just order-independence)', () => {
    // digest = SHA-256(runId + "\0" + id); sort by (digest, id); take first n.
    // Golden computed independently for these fixed inputs — a naive first-N or a
    // different hash/separator would not reproduce this exact set.
    const ids = ['t-a', 't-b', 't-c', 't-d', 't-e', 't-f']
    expect(stableSample(ids, 3, '20260717T151639Z')).toEqual(['t-d', 't-e', 't-c'])
  })

  it('is order-independent: a permuted input selects the SAME ids (not a naive first-N)', () => {
    const ids = ['t-a', 't-b', 't-c', 't-d', 't-e', 't-f']
    const runId = '20260717T151639Z'
    const first = stableSample(ids, 3, runId)
    const permuted = stableSample([...ids].reverse(), 3, runId)
    expect(permuted).toEqual(first)
    expect(first).toEqual(['t-d', 't-e', 't-c']) // same golden set regardless of input order
  })

  it('is stable across re-runs (same round → same passes)', () => {
    const ids = ['x1', 'x2', 'x3', 'x4']
    expect(stableSample(ids, 2, 'r')).toEqual(stableSample(ids, 2, 'r'))
  })

  it('oversized n → all ids (min(n, available)); n<=0 or empty → []', () => {
    expect([...stableSample(['a', 'b'], 5, 'r')].sort()).toEqual(['a', 'b'])
    expect(stableSample(['a'], 0, 'r')).toEqual([])
    expect(stableSample([], 3, 'r')).toEqual([])
  })

  it('runId feeds the hash: a different round yields a different full ordering', () => {
    const ids = Array.from({ length: 20 }, (_, i) => `id-${i}`)
    // Full-order (n = all) under two runIds — an identical ordering is ~1/20! (never).
    expect(stableSample(ids, ids.length, 'round-1')).not.toEqual(
      stableSample(ids, ids.length, 'round-2')
    )
  })
})

describe('exportSpans — salt integration (default + stability + clamp)', () => {
  afterEach(() => vi.restoreAllMocks())

  const passRound = (): GenAiSpan[] =>
    Array.from({ length: 5 }, (_, i) => itemSpan('pass', { behaviorId: `P-${i}` }))

  it('undefined saltPasses defaults to 3', async () => {
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, passRound())
    expect(res.enqueue.enqueued).toBe(3)
  })

  it('re-export of the same round selects the SAME salted passes', async () => {
    const spans = passRound()
    const run = async (): Promise<string[]> => {
      const mock = createFakeLangfuse()
      vi.stubGlobal('fetch', mock.fetchImpl)
      await exportSpans(cfg, spans, () => {}, { saltPasses: 2, runId: '20260717T151639Z' })
      vi.restoreAllMocks()
      return mock.itemPosts.map(p => p.objectId).sort()
    }
    const a = await run()
    const b = await run()
    expect(a).toEqual(b)
    expect(a).toHaveLength(2)
  })

  it('saltPasses larger than the pass pool enqueues all passes (clamped)', async () => {
    const spans = Array.from({ length: 2 }, (_, i) => itemSpan('pass', { behaviorId: `P-${i}` }))
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, spans, () => {}, { saltPasses: 10 })
    expect(res.enqueue.enqueued).toBe(2)
  })
})

// ===========================================================================
// Usage as a generation observation — the entire cost path. Langfuse computes
// cost ONLY on generation observations, and matches bucket names against the
// model's price keys by EXACT string equality, so both the event type and the
// key set are binding contracts, not cosmetics.
// ===========================================================================

const usageParams: Partial<SpanParams> = {
  inputTokens: 20_000,
  cachedInputTokens: 16_000,
  outputTokens: 1_000,
  reasoningOutputTokens: 800,
  cacheWriteInputTokens: 500,
}

// A 2-item behavior span (indices 0 and 2 — the carrier is neither "first" nor
// guaranteed to be 0 in general) carrying the full five-field usage.
function usageSpan(over: Partial<SpanParams> = {}): GenAiSpan {
  return buildGenAiSpan({
    ...baseParams,
    ...usageParams,
    prompt: 'p',
    scenario: behaviorScenario,
    gradedEvidence: [{ captureFile: 'a.json', note: 'n', evidence: {} }],
    itemVerdicts: twoVerdicts,
    ...over,
  })
}

describe('buildGenerationBody (PURE)', () => {
  it('nets input of cached, keeps cached as its own bucket, and folds reasoning into output', () => {
    const body = buildGenerationBody(usageSpan())!
    expect(body.usageDetails).toEqual({
      input: 4_000, // 20000 gross − 16000 cached (cached is INCLUSIVE)
      input_cached_tokens: 16_000,
      output: 1_000, // reasoning stays INSIDE output — a separate bucket goes unpriced
    })
  })

  it('ships EXACTLY the three price-key buckets — an equality, not a subset', () => {
    const body = buildGenerationBody(usageSpan())!
    expect(Object.keys(body.usageDetails).sort()).toEqual([
      'input',
      'input_cached_tokens',
      'output',
    ])
  })

  it('emits input_cached_tokens: 0 when the transport reported no cached count', () => {
    const span = usageSpan({ cachedInputTokens: undefined })
    const body = buildGenerationBody(span)!
    // The key set must NOT vary with the input: a conditional bucket makes the priced
    // shape depend on whether the transport happened to report a cached count.
    expect(Object.keys(body.usageDetails).sort()).toEqual([
      'input',
      'input_cached_tokens',
      'output',
    ])
    expect(body.usageDetails.input_cached_tokens).toBe(0)
    expect(body.usageDetails.input).toBe(20_000)
  })

  it('CLAMPS at zero when cached exceeds input (never a negative bucket)', () => {
    const body = buildGenerationBody(usageSpan({ cachedInputTokens: 30_000 }))!
    expect(body.usageDetails.input).toBe(0)
    expect(body.usageDetails.input_cached_tokens).toBe(30_000)
  })

  it('puts reasoning + cache-write in METADATA, never in usageDetails', () => {
    const body = buildGenerationBody(usageSpan())!
    expect(body.metadata).toEqual({
      reasoning_output_tokens: 800,
      cache_write_input_tokens: 500,
    })
    expect('reasoning_output_tokens' in body.usageDetails).toBe(false)
    expect('cache_write_input_tokens' in body.usageDetails).toBe(false)
    // No derived total: Langfuse sums the mutually-exclusive buckets itself.
    expect('total' in body.usageDetails).toBe(false)
  })

  it('is undefined when the span carries no input tokens (an error run)', () => {
    expect(
      buildGenerationBody(buildGenAiSpan({ ...baseParams, inputTokens: undefined }))
    ).toBeUndefined()
  })

  it('names the generation after its behavior and takes BOTH timestamps from the span', () => {
    const span = usageSpan({ startMs: 1_600_000_000_000, endMs: 1_600_000_003_200 })
    const body = buildGenerationBody(span)!
    expect(body.name).toBe('judge CON-042')
    expect(body.startTimeUnixNano).toBe('1600000000000000000')
    expect(body.endTimeUnixNano).toBe('1600000003200000000')
    // Span-derived, not the export clock.
    expect(Math.abs(Number(body.startTimeUnixNano) / 1e6 - Date.now())).toBeGreaterThan(1_000)
  })

  it('carries the span usage onto the LOWEST-itemIndex trace, as a child of its root, under a stable id', () => {
    const span = usageSpan()
    const body = buildGenerationBody(span)!
    const carrier = `${traceIdFor(span)}-item0`
    expect(body.traceId).toBe(hexTraceId(carrier))
    expect(body.parentSpanId).toBe(rootSpanId(carrier))
    expect(body.spanId).toBe(generationSpanId(span))
    expect(buildGenerationBody(span)).toEqual(body)
  })

  it('throws RangeError on a span time no Date can represent, rather than inventing one', () => {
    for (const bad of [Number.NaN, -1, 1e30]) {
      const span = usageSpan()
      span.start_time_unix_nano = bad
      expect(() => buildGenerationBody(span)).toThrow(RangeError)
    }
  })

  it('routes the model through the SAME scrubber the trace metadata uses', () => {
    const body = buildGenerationBody(usageSpan(), m => `S(${m})`)!
    expect(body.model).toBe('S(gpt-5.4-mini)')
    expect(body.model).toBe(buildTraceBody(usageSpan(), m => `S(${m})`)[0].metadata.model)
  })
})

describe('ingestionOutcome (PURE) — three tiers', () => {
  it('ACCEPTS only when the documented envelope names the event as stored', () => {
    expect(ingestionOutcome({ successes: [{ id: 'e1' }], errors: [] }, 'e1')).toEqual({
      kind: 'accepted',
    })
    expect(
      ingestionOutcome({ successes: [{ id: 'other' }, { id: 'e1' }], errors: [] }, 'e1')
    ).toEqual({ kind: 'accepted' })
  })

  it('REJECTS when the envelope is well-formed but does not name the event', () => {
    const out = ingestionOutcome({ successes: [], errors: [] }, 'e1')
    expect(out.kind).toBe('rejected')
    expect(out.kind === 'rejected' && out.reason).toContain('did not confirm')
    const other = ingestionOutcome({ successes: [{ id: 'other' }], errors: [] }, 'e1')
    expect(other.kind).toBe('rejected')
  })

  it('REJECTS on an errors entry, including one it cannot attribute', () => {
    const mine = ingestionOutcome(
      { successes: [], errors: [{ id: 'e1', status: 400, message: 'invalid usageDetails' }] },
      'e1'
    )
    expect(mine.kind === 'rejected' && mine.reason).toContain('invalid usageDetails')
    // Every batch carries exactly ONE event, so an unattributable entry is ours.
    const foreign = ingestionOutcome(
      { successes: [], errors: [{ id: 'other', status: 400, message: 'nope' }] },
      'e1'
    )
    expect(foreign.kind === 'rejected' && foreign.reason).toContain('reported against other')
    const junk = ingestionOutcome({ successes: [], errors: ['garbage'] }, 'e1')
    expect(junk.kind === 'rejected' && junk.reason).toContain('unreadable')
  })

  it('degrades to ACCEPTED — loudly — on an UNRECOGNIZED envelope', () => {
    // Strict confirmation matches the documented contract, but the deployed shape is
    // unverified from here. If it lacks `successes`, strict-only would report EVERY
    // event as failed and the first live run would fail for a reason unrelated to
    // what it tests. Degrade, and say so.
    for (const res of [{}, { errors: [] }, { successes: 'nope' }, { errors: 'boom' }]) {
      const out = ingestionOutcome(res, 'e1')
      expect(out.kind).toBe('unrecognized')
      expect(out.kind === 'unrecognized' && out.note).toContain('unrecognized')
      expect(out.kind === 'unrecognized' && out.note).toContain('verify against the live API')
    }
  })
})

describe('otlpRejection (PURE) — a 2xx that rejects a span is a rejection', () => {
  it('ACCEPTS an empty response and a partialSuccess that rejects nothing (a warning)', () => {
    expect(otlpRejection({})).toBeUndefined()
    expect(otlpRejection({ partialSuccess: null })).toBeUndefined()
    expect(otlpRejection({ partialSuccess: {} })).toBeUndefined()
    expect(otlpRejection({ partialSuccess: { rejectedSpans: 0 } })).toBeUndefined()
    expect(
      otlpRejection({ partialSuccess: { rejectedSpans: '0', errorMessage: 'slow down' } })
    ).toBeUndefined()
  })

  it('REJECTS a rejected-span count, as a number or an int64 string, with its message', () => {
    expect(
      otlpRejection({ partialSuccess: { rejectedSpans: 1, errorMessage: 'bad usage_details' } })
    ).toBe('1 span(s) rejected: bad usage_details')
    expect(otlpRejection({ partialSuccess: { rejectedSpans: '2' } })).toBe('2 span(s) rejected')
  })

  it('REJECTS a partialSuccess it cannot read, never reading it as success', () => {
    for (const ps of ['garbage', ['x'], 7]) {
      expect(otlpRejection({ partialSuccess: ps })).toContain('unreadable partialSuccess')
    }
    expect(otlpRejection({ partialSuccess: { rejectedSpans: 'lots' } })).toBe(
      'lots span(s) rejected'
    )
  })
})

describe('usageTraceId — the carrier trace', () => {
  it('is the LOWEST itemIndex, which is neither first in the array nor necessarily 0', () => {
    const span = usageSpan({
      scenario: {
        ...behaviorScenario,
        items: [
          { itemIndex: 3, thenText: 'c' },
          { itemIndex: 1, thenText: 'a' },
        ],
      },
    })
    expect(usageTraceId(span)).toBe(`${traceIdFor(span)}-item1`)
  })

  it('is the BASE trace id for the metrics-only (no-scenario) shape', () => {
    const span = buildGenAiSpan({ ...baseParams, ...usageParams, prompt: 'p' })
    expect(usageTraceId(span)).toBe(traceIdFor(span))
  })
})

describe('exportSpans — the generation observation (separate, non-fatal, after the trace)', () => {
  afterEach(() => vi.restoreAllMocks())

  it("ships ONE observation per SPAN, on the lowest-itemIndex trace, with the span's FULL usage", async () => {
    const span = usageSpan() // 2 item-traces
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [span])
    expect(res.traces).toBe(2)
    expect(res.observations).toBe(1)
    expect(mock.generations).toHaveLength(1)
    const gen = mock.generations[0]
    expect(gen.attributes['langfuse.observation.type']).toBe('generation')
    expect(gen.traceId).toBe(hexTraceId(`${traceIdFor(span)}-item0`))
    expect(gen.usageDetails).toEqual({
      input: 4_000,
      input_cached_tokens: 16_000,
      output: 1_000,
    })
    expect(Number(gen.startTimeUnixNano)).toBe(span.start_time_unix_nano)
  })

  it('is NEVER co-sent: the generation rides an OTLP request of exactly one span', async () => {
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [usageSpan()])
    const genRequests = mock.otlpRequests.filter(r => r.spans.some(sp => sp.parentSpanId))
    expect(genRequests.map(r => r.spans.length)).toEqual([1])
  })

  it('is POSTed AFTER every root span for that span', async () => {
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [usageSpan()])
    const kinds = mock.order.map(o => o.kind)
    expect(kinds.filter(k => k === 'generation')).toHaveLength(1)
    expect(kinds.lastIndexOf('generation')).toBe(kinds.length - 1)
    expect(kinds.indexOf('generation')).toBeGreaterThan(kinds.lastIndexOf('root'))
  })

  it('puts the observation on the BASE trace for a metrics-only span', async () => {
    const span = buildGenAiSpan({ ...baseParams, ...usageParams, prompt: 'p' })
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [span])
    expect(mock.generations[0].traceId).toBe(hexTraceId(traceIdFor(span)))
  })

  it('ships NO observation for a span with no usage (an error run still ships its trace)', async () => {
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [itemSpan('fail', { inputTokens: undefined })])
    expect(res.traces).toBe(1)
    expect(res.observations).toBe(0)
    expect(mock.generations).toHaveLength(0)
  })

  it('SKIPS the observation when the CARRIER trace failed to ship (never dangling)', async () => {
    const span = usageSpan()
    const carrier = `${traceIdFor(span)}-item0`
    const mock = createFakeLangfuse({ failRootFor: id => id === carrier })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [span])
    expect(mock.generations).toHaveLength(0)
    expect(res.observations).toBe(0)
    expect(res.failed).toBe(1)
    expect(res.traces).toBe(1) // the sibling still shipped
  })

  it('SKIPS the observation when the carrier root was REJECTED inside a 200 (OTLP partialSuccess)', async () => {
    // The HTTP call resolves, so nothing throws — only the response says the carrier's
    // root never landed. Emitting the generation anyway would produce the dangling
    // observation the carrier gate exists to prevent.
    const span = usageSpan()
    const carrier = `${traceIdFor(span)}-item0`
    const mock = createFakeLangfuse({ rejectRootFor: id => id === carrier })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [span])
    expect(mock.generations).toHaveLength(0)
    expect(res.observations).toBe(0)
    expect(res.failed).toBe(1) // the rejected root counts as a ship failure
    expect(res.traces).toBe(1) // the sibling still shipped
  })

  it('logs a score rejected by the envelope, without touching the trace or the enqueue', async () => {
    const mock = createFakeLangfuse({ rejectScore: true })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const logs: string[] = []
    const res = await exportSpans(cfg, [itemSpan('fail', usageParams)], m => logs.push(m))
    expect(res.traces).toBe(1)
    expect(res.failed).toBe(0)
    expect(res.enqueue.enqueued).toBe(1)
    expect(logs.some(l => l.includes('score-create failed') && l.includes('score refused'))).toBe(
      true
    )
  })

  it('does NOT count a trace whose only root was rejected inside a 200', async () => {
    const span = itemSpan('fail', usageParams)
    const mock = createFakeLangfuse({ rejectRootFor: () => true })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [span])
    expect(res.traces).toBe(0)
    expect(res.failed).toBe(1)
    expect(res.observations).toBe(0)
  })

  it('STILL ships the observation when only a NON-carrier sibling failed', async () => {
    const span = usageSpan()
    const sibling = `${traceIdFor(span)}-item2`
    const mock = createFakeLangfuse({ failRootFor: id => id === sibling })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [span])
    expect(mock.generations).toHaveLength(1)
    expect(mock.generations[0].traceId).toBe(hexTraceId(`${traceIdFor(span)}-item0`))
    expect(res.observations).toBe(1)
    expect(res.failed).toBe(1)
  })

  it('does NOT count an observation the OTLP partialSuccess rejected (HTTP 200)', async () => {
    // The silently-wrong-but-green failure: the endpoint can accept the REQUEST while
    // rejecting the SPAN. Counting the POST rather than the span would report a
    // shipped observation that was never stored.
    const mock = createFakeLangfuse({ generationRejected: true })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const logs: string[] = []
    const res = await exportSpans(cfg, [itemSpan('fail', usageParams)], m => logs.push(m))
    expect(mock.generations).toHaveLength(1) // it WAS posted
    expect(res.observations).toBe(0) // but never accepted
    expect(res.traces).toBe(1)
    expect(res.failed).toBe(0)
    expect(res.enqueue.enqueued).toBe(1)
    expect(
      logs.some(l => l.includes('generation failed') && l.includes('invalid usage_details'))
    ).toBe(true)
  })

  it('a REJECTED observation is non-fatal: traces/failed untouched, enqueue still runs', async () => {
    const mock = createFakeLangfuse({ generationStatus: 500 })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const logs: string[] = []
    const res = await exportSpans(cfg, [itemSpan('fail', usageParams)], m => logs.push(m))
    expect(res.traces).toBe(1)
    expect(res.failed).toBe(0)
    expect(res.observations).toBe(0)
    expect(res.enqueue.enqueued).toBe(1) // the enqueue pass still ran
    expect(logs.some(l => l.includes('generation failed'))).toBe(true)
  })

  it('a NEVER-SETTLING observation POST is time-bounded: the export completes and enqueue runs', async () => {
    const mock = createFakeLangfuse({ generationNeverSettles: true })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [itemSpan('fail', usageParams)], () => {}, {
      observationTimeoutMs: 20,
    })
    expect(res.observations).toBe(0)
    expect(res.traces).toBe(1)
    expect(res.enqueue.enqueued).toBe(1)
  })

  it('OPENS a breaker after consecutive timeouts: later spans are skipped, not re-timed-out', async () => {
    // The per-request timeout bounds ONE call; paying it once per span is a ~43-minute
    // stall on a full round. Asserted on ATTEMPTS, not wall clock, so it is fast and
    // deterministic.
    const spans = Array.from({ length: 12 }, (_, i) =>
      itemSpan('fail', { ...usageParams, behaviorId: `B-${i}` })
    )
    const mock = createFakeLangfuse({ generationNeverSettles: true })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const logs: string[] = []
    const res = await exportSpans(cfg, spans, m => logs.push(m), { observationTimeoutMs: 5 })
    // Three consecutive stalls open the breaker; the remaining nine are never posted.
    expect(mock.generations).toHaveLength(3)
    expect(res.observations).toBe(0)
    expect(res.observationsFailed).toBe(3) // the three stalls that OPENED the breaker
    expect(res.observationsSkipped).toBe(9)
    // Complete accounting: every eligible span lands in exactly one bucket. Without
    // the failed count this reports 9 missing when 12 are.
    expect(res.observations + res.observationsFailed + res.observationsSkipped).toBe(12)
    expect(logs.some(l => l.includes('observations DISABLED'))).toBe(true)
    // Traces, scores and the enqueue pass are untouched by the breaker.
    expect(res.traces).toBe(12)
    expect(res.failed).toBe(0)
    expect(mock.scores).toHaveLength(12)
    expect(res.enqueue.enqueued).toBeGreaterThan(0)
  })

  it('still ships and counts everything against a BARE ingestion envelope, warning ONCE', async () => {
    // The survival case: if the deployed ingestion endpoint does not carry
    // `successes`, strict confirmation alone would report every score as failed, a
    // live run failing for an unrelated reason. Traces and generations ride OTLP and
    // are untouched by the ingestion envelope.
    const spans = [
      itemSpan('fail', { ...usageParams, behaviorId: 'E-1' }),
      itemSpan('fail', { ...usageParams, behaviorId: 'E-2' }),
    ]
    const mock = createFakeLangfuse({ ingestionEnvelope: 'bare' })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const logs: string[] = []
    const res = await exportSpans(cfg, spans, m => logs.push(m))
    expect(res.traces).toBe(2)
    expect(res.failed).toBe(0)
    expect(res.observations).toBe(2)
    expect(res.observationsFailed).toBe(0)
    // Loud, and exactly ONCE across every event of the export.
    const warnings = logs.filter(l => l.includes('envelope unrecognized'))
    expect(warnings).toHaveLength(1)
    expect(warnings[0]).toContain('verify against the live API')
  })

  it('does NOT count a usage-LESS span as skipped once the breaker is open', async () => {
    // A judge call that failed before reporting input tokens would never have
    // produced an observation; counting it as skipped reports a loss that never
    // existed — the same dishonest-count defect the breaker itself was meant to close.
    const stalling = Array.from({ length: 3 }, (_, i) =>
      itemSpan('fail', { ...usageParams, behaviorId: `T-${i}` })
    )
    const usageless = Array.from({ length: 4 }, (_, i) =>
      itemSpan('fail', { behaviorId: `U-${i}`, inputTokens: undefined })
    )
    const withUsage = itemSpan('fail', { ...usageParams, behaviorId: 'V-1' })
    const mock = createFakeLangfuse({ generationNeverSettles: true })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, [...stalling, ...usageless, withUsage], () => {}, {
      observationTimeoutMs: 5,
    })
    expect(res.observations).toBe(0)
    expect(res.observationsFailed).toBe(3)
    // ONLY the one usage-bearing span after the breaker opened is a real skip; the
    // four usage-LESS spans are in no bucket because they were never eligible.
    expect(res.observationsSkipped).toBe(1)
    expect(res.observations + res.observationsFailed + res.observationsSkipped).toBe(4)
    expect(res.traces).toBe(8)
  })

  it('does NOT trip the breaker on rejected payloads — those stay attempted and honest', async () => {
    // A 400 is fast and per-span meaningful; only the never-settling class must stop.
    const spans = Array.from({ length: 6 }, (_, i) =>
      itemSpan('fail', { ...usageParams, behaviorId: `R-${i}` })
    )
    const mock = createFakeLangfuse({ generationStatus: 400 })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const res = await exportSpans(cfg, spans)
    expect(mock.generations).toHaveLength(6) // every span still attempted
    expect(res.observations).toBe(0)
    expect(res.observationsFailed).toBe(6) // attempted and lost, every one accounted
    expect(res.observationsSkipped).toBe(0) // nothing was SKIPPED — all were tried
    expect(res.observations + res.observationsFailed + res.observationsSkipped).toBe(6)
  })

  it('resets the streak on a success, so isolated stalls never open the breaker', async () => {
    let n = 0
    const mock = createFakeLangfuse({ generationNeverSettlesWhen: () => ++n % 2 === 1 })
    vi.stubGlobal('fetch', mock.fetchImpl)
    const spans = Array.from({ length: 8 }, (_, i) =>
      itemSpan('fail', { ...usageParams, behaviorId: `A-${i}` })
    )
    const res = await exportSpans(cfg, spans, () => {}, { observationTimeoutMs: 5 })
    expect(res.observationsSkipped).toBe(0)
    expect(res.observations).toBe(4)
  })

  it('counts observations across spans, and one rejection lowers the count by exactly one', async () => {
    const spans = [
      itemSpan('fail', { ...usageParams, behaviorId: 'A-1' }),
      itemSpan('fail', { ...usageParams, behaviorId: 'A-2' }),
      itemSpan('fail', { ...usageParams, behaviorId: 'A-3' }),
    ]
    const all = createFakeLangfuse()
    vi.stubGlobal('fetch', all.fetchImpl)
    expect((await exportSpans(cfg, spans)).observations).toBe(3)
    vi.restoreAllMocks()

    // One span carries no usage → 2 observations, and no failure anywhere.
    const partial = createFakeLangfuse()
    vi.stubGlobal('fetch', partial.fetchImpl)
    const res = await exportSpans(cfg, [
      ...spans.slice(0, 2),
      itemSpan('fail', { behaviorId: 'A-4', inputTokens: undefined }),
    ])
    expect(res.observations).toBe(2)
    expect(res.failed).toBe(0)
  })

  it('uses a STABLE generation span id and start time across re-export', async () => {
    const span = usageSpan()
    const first = createFakeLangfuse()
    vi.stubGlobal('fetch', first.fetchImpl)
    await exportSpans(cfg, [span])
    vi.restoreAllMocks()
    const second = createFakeLangfuse()
    vi.stubGlobal('fetch', second.fetchImpl)
    await exportSpans(cfg, [span])
    expect(second.generations[0].spanId).toBe(first.generations[0].spanId)
    expect(second.generations[0].startTimeUnixNano).toBe(first.generations[0].startTimeUnixNano)
  })

  it('ships ONE model value across the trace metadata, the tag, and the observation', async () => {
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [usageSpan()])
    const model = mock.generations[0].model
    expect(mock.roots[0].metadata.model).toBe(model)
    expect(mock.roots[0].tags).toContain(`model:${model}`)
  })
})

describe('exportSpans — usage attribution + the pass/model tags', () => {
  afterEach(() => vi.restoreAllMocks())

  it('marks the carrier trace usage_attributed TRUE and its siblings FALSE', async () => {
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [usageSpan()])
    const carrier = mock.roots.find(b => b.labelId.endsWith('-item0'))!
    const sibling = mock.roots.find(b => b.labelId.endsWith('-item2'))!
    expect(carrier.metadata.usage_attributed).toBe(true)
    expect(sibling.metadata.usage_attributed).toBe(false)
  })

  it("marks a metrics-only span's single trace usage_attributed TRUE", () => {
    const [body] = buildTraceBody(buildGenAiSpan({ ...baseParams, ...usageParams, prompt: 'p' }))
    expect(body.metadata.usage_attributed).toBe(true)
  })

  it('carries all three new counts in the trace metadata (display-only, alongside the observation)', () => {
    for (const body of buildTraceBody(usageSpan())) {
      expect(body.metadata.cached_input_tokens).toBe(16_000)
      expect(body.metadata.reasoning_output_tokens).toBe(800)
      expect(body.metadata.cache_write_input_tokens).toBe(500)
    }
  })

  it('tags a behavior span pass:ux + model:<model> alongside the existing tags', async () => {
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [usageSpan()], () => {}, {
      runId: '20260717T151639Z',
      gitSha: 'abc1234',
    })
    expect(mock.roots[0].tags).toEqual([
      'behavior:CON-042',
      'runId:20260717T151639Z',
      'gitSha:abc1234',
      'pass:ux',
      'model:gpt-5.4-mini',
    ])
  })

  it('tags an intent span pass:intent', async () => {
    const span = buildGenAiSpan({
      ...baseParams,
      ...usageParams,
      behaviorId: 'DSH-010',
      prompt: 'p',
      scenario: {
        kind: 'intent',
        intentId: 'DSH-010',
        title: 't',
        statement: 's',
        status: 'current',
      },
      itemVerdicts: [{ itemIndex: 0, verdict: 'pass', citation: 'c', critique: 'k' }],
    })
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [span])
    expect(mock.roots[0].tags).toContain('pass:intent')
    expect(mock.roots[0].tags).not.toContain('pass:ux')
  })

  it('emits NO pass: tag for a metrics-only span (it has no pass dimension)', async () => {
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [buildGenAiSpan({ ...baseParams, ...usageParams, prompt: 'p' })])
    expect(mock.roots[0].tags?.some(t => t.startsWith('pass:'))).toBe(false)
    expect(mock.roots[0].tags).toContain('model:gpt-5.4-mini')
  })

  it('an EMPTY model emits a bare model: tag, in parity with metadata.model', async () => {
    // A misconfigured harness (QA_JUDGE_MODEL='') must LOOK misconfigured — suppressing
    // the tag would make it indistinguishable from a run with no model dimension.
    const mock = createFakeLangfuse()
    vi.stubGlobal('fetch', mock.fetchImpl)
    await exportSpans(cfg, [itemSpan('pass', { ...usageParams, model: '' })])
    expect(mock.roots[0].metadata.model).toBe('')
    expect(mock.roots[0].tags).toContain('model:')
  })
})

describe('api / apiGetAllPages — time-bounded requests', () => {
  afterEach(() => vi.restoreAllMocks())

  it('sends NO abort signal when no timeout is given (existing call sites unchanged)', async () => {
    const seen: Array<{ signal?: AbortSignal }> = []
    vi.stubGlobal('fetch', (async (_u: string, init: { signal?: AbortSignal }) => {
      seen.push(init)
      return { ok: true, status: 200, text: async () => '{}' } as Response
    }) as unknown as typeof fetch)
    await api(cfg, 'GET', '/x')
    expect(seen[0].signal).toBeUndefined()
  })

  it('aborts a never-settling single request once the timeout elapses', async () => {
    vi.stubGlobal('fetch', (async (_u: string, init: { signal?: AbortSignal }) => {
      return new Promise<Response>((_res, rej) => {
        init.signal?.addEventListener('abort', () => rej(new Error('aborted')))
      })
    }) as unknown as typeof fetch)
    await expect(api(cfg, 'GET', '/x', undefined, 20)).rejects.toThrow()
  })

  it('propagates the timeout to EVERY page: a stalled page 2 rejects instead of hanging', async () => {
    // The paginated call is the one that matters — a bound that stopped at `api`'s
    // signature would leave every multi-page read able to hang forever on page 2.
    let calls = 0
    vi.stubGlobal('fetch', (async (u: string, init: { signal?: AbortSignal }) => {
      calls++
      if (calls === 1) {
        return {
          ok: true,
          status: 200,
          text: async () =>
            JSON.stringify({
              data: [{ id: 'a' }],
              meta: { page: 1, limit: 1, totalItems: 2, totalPages: 2 },
            }),
        } as Response
      }
      expect(String(u)).toContain('page=2')
      return new Promise<Response>((_res, rej) => {
        init.signal?.addEventListener('abort', () => rej(new Error('aborted')))
      })
    }) as unknown as typeof fetch)
    await expect(apiGetAllPages(cfg, '/api/public/models', 'page', 20)).rejects.toThrow()
    expect(calls).toBe(2)
  })
})
