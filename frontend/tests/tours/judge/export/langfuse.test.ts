import { describe, expect, it } from 'vitest'
import { buildGenAiSpan } from '../adapter/span'
import { buildTraceBody, configFromEnv, parseSpanFile, traceIdFor } from './langfuse'

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

describe('parseSpanFile', () => {
  it('skips malformed lines — a partial trace is still worth shipping', () => {
    const good = JSON.stringify(buildGenAiSpan(baseParams))
    const spans = parseSpanFile(`${good}\n{ not json\n\n${good}\n`)
    expect(spans).toHaveLength(2)
  })
})
