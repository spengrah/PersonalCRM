import { execFileSync } from 'child_process'
import * as path from 'path'
import { describe, expect, it } from 'vitest'
import { buildGenAiSpan } from '../adapter/span'
import type { exportSpans as ExportSpansFn } from './langfuse'
import { main, parseSaltPasses, validGitSha, validRunId, type RunFs } from './run'

// --- provenance component validators ---

describe('validRunId (regex + UTC round-trip)', () => {
  it('accepts a real UTC run-id', () => {
    expect(validRunId('20260717T151639Z')).toBe('20260717T151639Z')
  })
  it('rejects a regex-shaped but IMPOSSIBLE instant (20269999T999999Z)', () => {
    expect(validRunId('20269999T999999Z')).toBeUndefined()
  })
  it('rejects an out-of-range day that silently rolls over (20260231T000000Z)', () => {
    expect(validRunId('20260231T000000Z')).toBeUndefined() // Feb 31 → rolls into March
  })
  it('accepts a year 0000–0099 on its own terms (Date.UTC 2-digit-year mapping must not reject it)', () => {
    expect(validRunId('00500717T151639Z')).toBe('00500717T151639Z')
  })
  it('accepts the year-0000 leap day (0000-02-29: 0000 is a 400-divisible leap year)', () => {
    expect(validRunId('00000229T000000Z')).toBe('00000229T000000Z')
  })
  it('rejects a NON-leap-year Feb 29 (2026-02-29 rolls into March)', () => {
    expect(validRunId('20260229T000000Z')).toBeUndefined()
  })
  it('rejects free text / wrong shape / empty / undefined', () => {
    expect(validRunId('unknown')).toBeUndefined()
    expect(validRunId('2026-07-17T15:16:39Z')).toBeUndefined()
    expect(validRunId('')).toBeUndefined()
    expect(validRunId(undefined)).toBeUndefined()
  })
})

describe('validGitSha (hex 7–40, short prefix OK)', () => {
  it('accepts a short 7-char sha AS-IS (a verified prefix, not required to be 40 chars)', () => {
    expect(validGitSha('abc1234')).toBe('abc1234')
  })
  it('accepts a full 40-char sha', () => {
    const full = 'a366dc9e1234567890abcdef1234567890abcdef'
    expect(validGitSha(full)).toBe(full)
  })
  it('rejects too-short / uppercase / non-hex / unknown / undefined', () => {
    expect(validGitSha('abc123')).toBeUndefined() // 6 chars
    expect(validGitSha('ABC1234')).toBeUndefined() // uppercase
    expect(validGitSha('zzz1234')).toBeUndefined() // non-hex
    expect(validGitSha('unknown')).toBeUndefined()
    expect(validGitSha(undefined)).toBeUndefined()
  })
})

describe('parseSaltPasses', () => {
  it('honors a non-negative integer incl. 0; drops negative/fractional/malformed/empty', () => {
    expect(parseSaltPasses('5')).toBe(5)
    expect(parseSaltPasses('0')).toBe(0)
    expect(parseSaltPasses('-1')).toBeUndefined()
    expect(parseSaltPasses('2.5')).toBeUndefined()
    expect(parseSaltPasses('1e2')).toBeUndefined()
    expect(parseSaltPasses('abc')).toBeUndefined()
    expect(parseSaltPasses('')).toBeUndefined()
    expect(parseSaltPasses(undefined)).toBeUndefined()
  })
})

// --- the CLI main() seam ---

const LF_ENV = { LANGFUSE_HOST: 'h', LANGFUSE_PUBLIC_KEY: 'p', LANGFUSE_SECRET_KEY: 's' }
const okResult = {
  traces: 1,
  screenshots: 0,
  failed: 0,
  enqueue: { attempted: 0, enqueued: 0, skippedExisting: 0, failed: 0 },
}
const oneSpanJsonl = JSON.stringify(
  buildGenAiSpan({ impl: 'x', behaviorId: 'B-1', startMs: 0, endMs: 1, prompt: 'p' })
)

// A capturing fake exportSpans + a filesystem seam whose trace read succeeds and whose
// sidecar behavior is caller-controlled.
function makeExport(): { fn: typeof ExportSpansFn; calls: { spans: unknown; opts: unknown }[] } {
  const calls: { spans: unknown; opts: unknown }[] = []
  const fn = (async (_cfg: unknown, spans: unknown, _log: unknown, opts: unknown) => {
    calls.push({ spans, opts })
    return okResult
  }) as unknown as typeof ExportSpansFn
  return { fn, calls }
}

describe('main — opt-in guard FIRST (INV-A)', () => {
  it('no LANGFUSE_* and NO trace arg → returns 0 (not 1/2), ships nothing', async () => {
    const logs: string[] = []
    const code = await main([], {}, { log: m => logs.push(m), errlog: m => logs.push(m) })
    expect(code).toBe(0)
    expect(logs.some(l => l.includes('nothing shipped'))).toBe(true)
  })

  it('IMPORTING run.ts (not as the entry) runs NO side effects — discriminating import-guard proof', () => {
    // Import run.ts as a MODULE in a subprocess with VALID creds and NO trace arg, then
    // print a marker. If the import.meta.main guard were broken, the import would invoke
    // main() → usage exit 2 BEFORE the marker (execFileSync would throw). A correct guard
    // imports side-effect-free, so the marker prints and the process exits 0. This is the
    // real proof the static top-of-file `import { main }` cannot give on its own.
    // vitest runs with cwd = frontend/, so resolve run.ts from there.
    const runTs = path.join(process.cwd(), 'tests/tours/judge/export/run.ts')
    const script = `import(${JSON.stringify(runTs)}).then(m => console.log('IMPORT_OK:' + typeof m.main))`
    const env = { ...process.env, ...LF_ENV } // valid creds → a broken guard would run + exit 2
    const out = execFileSync('bun', ['-e', script], { env, encoding: 'utf8' }) // throws if exit != 0
    expect(out).toContain('IMPORT_OK:function')
  })

  it('run as the ENTRY with no args + no creds → exit 0 shipping nothing (opt-in-first, end to end)', () => {
    const runTs = path.join(process.cwd(), 'tests/tours/judge/export/run.ts')
    const env = {
      ...process.env,
      LANGFUSE_HOST: '',
      LANGFUSE_PUBLIC_KEY: '',
      LANGFUSE_SECRET_KEY: '',
    }
    const out = execFileSync('bun', [runTs], { env, encoding: 'utf8' }) // throws if exit != 0
    expect(out).toContain('nothing shipped')
  })

  it('LANGFUSE_* set + no trace ARG → returns 2 (usage)', async () => {
    const code = await main([], LF_ENV, { log: () => {}, errlog: () => {} })
    expect(code).toBe(2)
  })

  it('LANGFUSE_* set + missing trace FILE → returns 1', async () => {
    const rfs: RunFs = { existsSync: () => false, readFileSync: () => '', writeFileSync: () => {} }
    const code = await main(['/no/such.jsonl'], LF_ENV, {
      fs: rfs,
      log: () => {},
      errlog: () => {},
    })
    expect(code).toBe(1)
  })
})

describe('main — provenance plumbing (component-wise, never fail-closed)', () => {
  // Trace read succeeds; the sidecar is absent (guard just writes) so these tests
  // isolate provenance plumbing from the stale-file guard.
  const traceFs = (): RunFs => ({
    existsSync: p => !p.endsWith('.qa-provenance'),
    readFileSync: () => oneSpanJsonl,
    writeFileSync: () => {},
  })

  it('valid QA_RUN_ID/QA_GIT_SHA/QA_SALT_PASSES are parsed + the exact spans+opts passed to exportSpans', async () => {
    const exp = makeExport()
    const code = await main(
      ['/t.jsonl'],
      { ...LF_ENV, QA_RUN_ID: '20260717T151639Z', QA_GIT_SHA: 'abc1234', QA_SALT_PASSES: '2' },
      { fs: traceFs(), exportSpans: exp.fn, log: () => {}, errlog: () => {} }
    )
    expect(code).toBe(0)
    expect(exp.calls).toHaveLength(1)
    // Exact spans (the one parsed from the trace file) and opts.
    expect(exp.calls[0].spans).toEqual([JSON.parse(oneSpanJsonl)])
    expect(exp.calls[0].opts).toEqual({
      runId: '20260717T151639Z',
      gitSha: 'abc1234',
      saltPasses: 2,
    })
  })

  it('an INVALID QA_RUN_ID is dropped while a valid QA_GIT_SHA still applies (never blocks)', async () => {
    const exp = makeExport()
    const logs: string[] = []
    const code = await main(
      ['/t.jsonl'],
      { ...LF_ENV, QA_RUN_ID: '20269999T999999Z', QA_GIT_SHA: 'abc1234' },
      { fs: traceFs(), exportSpans: exp.fn, log: m => logs.push(m), errlog: () => {} }
    )
    expect(code).toBe(0)
    expect(exp.calls[0].opts).toEqual({
      runId: undefined,
      gitSha: 'abc1234',
      saltPasses: undefined,
    })
    expect(logs.some(l => l.includes('QA_RUN_ID') && l.includes('dropping'))).toBe(true)
  })

  it('an EMPTY (but PROVIDED) QA_RUN_ID warns as invalid — distinguished from an absent var', async () => {
    const exp = makeExport()
    const logs: string[] = []
    const code = await main(
      ['/t.jsonl'],
      { ...LF_ENV, QA_RUN_ID: '' },
      { fs: traceFs(), exportSpans: exp.fn, log: m => logs.push(m), errlog: () => {} }
    )
    expect(code).toBe(0)
    expect((exp.calls[0].opts as { runId?: string }).runId).toBeUndefined()
    expect(logs.some(l => l.includes('QA_RUN_ID') && l.includes('dropping'))).toBe(true)
  })

  it('an ABSENT QA_GIT_SHA is silently skipped (no warning)', async () => {
    const exp = makeExport()
    const logs: string[] = []
    await main(
      ['/t.jsonl'],
      { ...LF_ENV },
      {
        fs: traceFs(),
        exportSpans: exp.fn,
        log: m => logs.push(m),
        errlog: () => {},
      }
    )
    expect(logs.some(l => l.includes('QA_GIT_SHA'))).toBe(false)
  })

  it('a partial enqueue loss (enqueue.failed > 0, trace failed === 0) STILL exits 0 (INV-A)', async () => {
    const fn = (async () => ({
      traces: 3,
      screenshots: 0,
      failed: 0,
      enqueue: { attempted: 2, enqueued: 1, skippedExisting: 0, failed: 1 },
    })) as unknown as typeof ExportSpansFn
    const logs: string[] = []
    const code = await main(['/t.jsonl'], LF_ENV, {
      fs: traceFs(),
      exportSpans: fn,
      log: m => logs.push(m),
      errlog: () => {},
    })
    expect(code).toBe(0) // enqueue loss never changes the exit; only trace-ship failure does
    expect(logs.some(l => l.includes('enqueue-failed'))).toBe(true)
  })
})

// --- the stale-file sidecar guard (contract #3): exception-contained, non-fatal ---

describe('main — stale-file sidecar guard', () => {
  const SIDECAR = '/t.jsonl.qa-provenance'

  // rfs whose trace read always succeeds; sidecar existence/content/errors are injected.
  function guardFs(over: {
    sidecarExists?: boolean
    sidecarContent?: string
    readThrows?: boolean
    writeThrows?: boolean
    writes?: string[]
  }): RunFs {
    return {
      existsSync: p => (p === SIDECAR ? (over.sidecarExists ?? false) : true),
      readFileSync: p => {
        if (p === SIDECAR) {
          if (over.readThrows) throw new Error('EACCES read')
          return over.sidecarContent ?? '{}'
        }
        return oneSpanJsonl
      },
      writeFileSync: (p, data) => {
        if (p === SIDECAR && over.writeThrows) throw new Error('EACCES write')
        over.writes?.push(data)
      },
    }
  }

  async function run(
    rfs: RunFs,
    env: Record<string, string | undefined> = LF_ENV
  ): Promise<{ code: number; logs: string[]; exp: ReturnType<typeof makeExport> }> {
    const logs: string[] = []
    const exp = makeExport()
    const code = await main(['/t.jsonl'], env, {
      fs: rfs,
      exportSpans: exp.fn,
      log: m => logs.push(m),
      errlog: m => logs.push(m),
    })
    return { code, logs, exp }
  }

  it('absent sidecar → writes provenance, ships the exact spans/opts silently (no warning)', async () => {
    const writes: string[] = []
    const { code, logs, exp } = await run(guardFs({ sidecarExists: false, writes }))
    expect(code).toBe(0)
    expect(exp.calls).toHaveLength(1)
    // The guard never disturbs what exportSpans receives.
    expect(exp.calls[0].spans).toEqual([JSON.parse(oneSpanJsonl)])
    expect(exp.calls[0].opts).toEqual({
      runId: undefined,
      gitSha: undefined,
      saltPasses: undefined,
    })
    // No provenance in env → the written sidecar records nulls.
    expect(writes).toEqual([JSON.stringify({ runId: null, gitSha: null })])
    expect(logs.some(l => l.includes('WARNING'))).toBe(false)
  })

  it('a sidecar with DIFFERENT provenance warns loudly, still ships the same spans/opts, and updates the sidecar to CURRENT provenance', async () => {
    const writes: string[] = []
    const { code, logs, exp } = await run(
      guardFs({
        sidecarExists: true,
        sidecarContent: JSON.stringify({ runId: '20260101T000000Z', gitSha: 'oldsha1' }),
        writes,
      }),
      { ...LF_ENV, QA_RUN_ID: '20260717T151639Z' }
    )
    expect(code).toBe(0)
    expect(exp.calls).toHaveLength(1)
    expect(exp.calls[0].spans).toEqual([JSON.parse(oneSpanJsonl)])
    expect(exp.calls[0].opts).toEqual({
      runId: '20260717T151639Z',
      gitSha: undefined,
      saltPasses: undefined,
    })
    expect(logs.some(l => l.includes('reused across rounds'))).toBe(true)
    // Sidecar rewritten to THIS export's provenance (the exact bytes).
    expect(writes).toEqual([JSON.stringify({ runId: '20260717T151639Z', gitSha: null })])
  })

  it('a shape-invalid sidecar ({}, array, or wrong-typed field) is treated as MALFORMED, not silently coerced', async () => {
    for (const bad of ['{}', '[]', JSON.stringify({ runId: 42, gitSha: null })]) {
      const { code, logs, exp } = await run(guardFs({ sidecarExists: true, sidecarContent: bad }))
      expect(code).toBe(0)
      expect(exp.calls).toHaveLength(1)
      expect(logs.some(l => l.includes('unreadable/malformed'))).toBe(true)
    }
  })

  it('a MATCHING sidecar ships silently (idempotent re-export, no warning)', async () => {
    const { code, logs } = await run(
      guardFs({
        sidecarExists: true,
        sidecarContent: JSON.stringify({ runId: null, gitSha: null }),
      })
    )
    expect(code).toBe(0)
    expect(logs.some(l => l.includes('WARNING'))).toBe(false)
  })

  // A guard failure must leave the export call EXACTLY as it would have been (same
  // spans + opts) and the exit code 0 — the sidecar is a belt, never load-bearing.
  const expectExportUnchanged = (exp: ReturnType<typeof makeExport>): void => {
    expect(exp.calls).toHaveLength(1)
    expect(exp.calls[0].spans).toEqual([JSON.parse(oneSpanJsonl)])
    expect(exp.calls[0].opts).toEqual({
      runId: undefined,
      gitSha: undefined,
      saltPasses: undefined,
    })
  }

  it('CORRUPT sidecar JSON → warns (guard degraded), still ships unchanged, no throw', async () => {
    const { code, logs, exp } = await run(
      guardFs({ sidecarExists: true, sidecarContent: '{ not json' })
    )
    expect(code).toBe(0)
    expectExportUnchanged(exp)
    expect(logs.some(l => l.includes('unreadable/malformed'))).toBe(true)
  })

  it('sidecar readFileSync throws (permission/read error) → warns, still ships unchanged, no throw', async () => {
    const { code, logs, exp } = await run(guardFs({ sidecarExists: true, readThrows: true }))
    expect(code).toBe(0)
    expectExportUnchanged(exp)
    expect(logs.some(l => l.includes('WARNING'))).toBe(true)
  })

  it('sidecar writeFileSync throws (permission/write error) → warns, still ships unchanged, no throw', async () => {
    const { code, logs, exp } = await run(guardFs({ sidecarExists: false, writeThrows: true }))
    expect(code).toBe(0)
    expectExportUnchanged(exp)
    expect(logs.some(l => l.includes('protection degraded'))).toBe(true)
  })
})
