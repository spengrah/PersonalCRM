import { afterEach, describe, expect, it, vi } from 'vitest'
import { buildGenAiSpan } from '../adapter/span'
import { createScrubber } from '../scrub'
import { buildTraceBody, configFromEnv, exportSpans, parseSpanFile, traceIdFor } from './langfuse'

const baseParams = {
  impl: 'codex-exec',
  behaviorId: 'CAD-038',
  model: 'gpt-5.5',
  startMs: 1_000,
  endMs: 4_200,
  inputTokens: 90_000,
  outputTokens: 240,
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
    expect(traceIdFor(span)).toContain('CAD-038')
  })
})

describe('buildTraceBody', () => {
  it('carries the prompt and the response — a metrics-only trace is unlabelable', () => {
    const span = buildGenAiSpan({
      ...baseParams,
      prompt: 'INTENT: the relationship loop closes...',
      response: '[{"itemIndex":0,"verdict":"pass"}]',
    })
    const body = buildTraceBody(span)
    expect((body.input as { prompt: string }).prompt).toContain('the relationship loop closes')
    expect(body.output).toBe('[{"itemIndex":0,"verdict":"pass"}]')
    expect(body.metadata.behavior_id).toBe('CAD-038')
    expect(body.metadata.input_tokens).toBe(90_000)
    expect(body.metadata.duration_ms).toBe(3_200)
  })

  it('omits input entirely when the caller logged no content (the #379 extraction shape)', () => {
    const body = buildTraceBody(buildGenAiSpan(baseParams))
    expect(body.input).toBeUndefined()
    expect(body.output).toBeUndefined()
    // ...but the metrics still ship, which is the whole point of a content-light trace.
    expect(body.metadata.output_tokens).toBe(240)
  })

  it('reports attached-vs-expected screenshots so a partial upload cannot masquerade as complete', () => {
    const span = buildGenAiSpan({
      ...baseParams,
      prompt: 'p',
      screenshots: ['/runs/a.png', '/runs/b.png', '/runs/c.png'],
    })
    const body = buildTraceBody(span, ['@@@langfuseMedia:type=image/png|id=m1|source=bytes@@@'])
    expect(body.metadata.screenshots_expected).toBe(3)
    expect(body.metadata.screenshots_attached).toBe(1)
    expect((body.input as { screenshots: string[] }).screenshots).toHaveLength(1)
  })

  it('surfaces an errored judge run rather than shipping it as a clean trace', () => {
    const span = buildGenAiSpan({ ...baseParams, error: 'codex timed out' })
    const body = buildTraceBody(span)
    expect(body.metadata.status).toBe('ERROR')
    expect(body.metadata.error).toBe('codex timed out')
  })
})

describe('buildTraceBody PII scrub seam (INV-2)', () => {
  // Distinct sentinel email+phone per free-form / env-sourced branch of the
  // shipped body: prompt, completion, status.message→metadata.error, and
  // gen_ai.request.model→metadata.model. None may survive raw.
  const span = buildGenAiSpan({
    ...baseParams,
    model: 'gpt brex@synthetic.example (479) 555-0104',
    prompt: 'saw brix@synthetic.example and +1-479-555-0100 in aria',
    response: 'reply to brax@synthetic.example at (479) 555-0101',
    error: 'threw for brox@synthetic.example calling +1-479-555-0102',
  })

  it('scrubs every free-form/env-sourced string; no raw sentinel survives', () => {
    const scrubber = createScrubber()
    const body = buildTraceBody(span, [], s => scrubber.scrub(s))
    const shipped = JSON.stringify(body)
    // No raw email/phone anywhere in the shipped body.
    expect(shipped).not.toMatch(/@synthetic\.example/)
    expect(shipped).not.toMatch(/479-555-01\d\d/)
    expect(shipped).not.toMatch(/\(479\) 555-01\d\d/)
    // Placeholders present in each branch.
    expect((body.input as { prompt: string }).prompt).toContain('<email:')
    expect((body.input as { prompt: string }).prompt).toContain('<phone:')
    expect(body.output as string).toContain('<email:')
    expect(body.output as string).toContain('<phone:')
    expect(String(body.metadata.error)).toContain('<email:')
    expect(String(body.metadata.error)).toContain('<phone:')
    expect(String(body.metadata.model)).toContain('<email:')
    expect(String(body.metadata.model)).toContain('<phone:')
  })

  it('defaults to identity scrub, so existing callers are unaffected', () => {
    const body = buildTraceBody(span, [])
    expect((body.input as { prompt: string }).prompt).toContain('brix@synthetic.example')
    expect(body.output).toBe('reply to brax@synthetic.example at (479) 555-0101')
    expect(body.metadata.error).toBe('threw for brox@synthetic.example calling +1-479-555-0102')
    expect(body.metadata.model).toBe('gpt brex@synthetic.example (479) 555-0104')
  })

  it('preserves a non-string model attribute via the fallback (scrub only touches strings)', () => {
    const scrubber = createScrubber()
    // A non-string model attribute: str() yields undefined, so buildTraceBody
    // falls back to the raw attribute value rather than scrubbing/crashing.
    const numericModel = { ...span, attributes: { ...span.attributes, 'gen_ai.request.model': 42 } }
    expect(buildTraceBody(numericModel, [], s => scrubber.scrub(s)).metadata.model).toBe(42)
    // Missing entirely → undefined, no throw.
    const noModel = { ...span, attributes: { ...span.attributes } }
    delete noModel.attributes['gen_ai.request.model']
    expect(buildTraceBody(noModel, [], s => scrubber.scrub(s)).metadata.model).toBeUndefined()
  })
})

describe('exportSpans shared scrubber', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('uses ONE scrubber across the run so a shared email maps to the same placeholder', async () => {
    const bodies: Array<Record<string, unknown>> = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_url: string, init?: { body?: string }) => {
        if (init?.body) {
          const parsed = JSON.parse(init.body) as {
            batch?: Array<{ body?: Record<string, unknown> }>
          }
          for (const evt of parsed.batch ?? []) if (evt.body) bodies.push(evt.body)
        }
        return { ok: true, status: 200, text: async () => '{}' } as Response
      })
    )

    const shared = 'dup@synthetic.example'
    const spans = [
      buildGenAiSpan({ ...baseParams, behaviorId: 'CON-001', prompt: `first ${shared}` }),
      buildGenAiSpan({ ...baseParams, behaviorId: 'CON-002', prompt: `second ${shared}` }),
    ]
    const result = await exportSpans({ host: 'http://lf', publicKey: 'p', secretKey: 's' }, spans)
    expect(result.traces).toBe(2)

    const prompts = bodies
      .map(b => (b.input as { prompt?: string } | undefined)?.prompt)
      .filter((p): p is string => typeof p === 'string')
    expect(prompts).toHaveLength(2)
    // No raw email shipped, and the SAME placeholder in both (shared scrubber).
    for (const p of prompts) {
      expect(p).not.toContain('@synthetic.example')
      expect(p).toContain('<email:1>')
    }
  })
})

describe('parseSpanFile', () => {
  it('skips malformed lines — a partial trace is still worth shipping', () => {
    const good = JSON.stringify(buildGenAiSpan(baseParams))
    const spans = parseSpanFile(`${good}\n{ not json\n\n${good}\n`)
    expect(spans).toHaveLength(2)
  })
})
