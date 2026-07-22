// The live smoke's own harness, driven against a STUBBED transport. The live run
// itself is the human's Mac gate (this sandbox has read-only access to the
// instance); what is proved here is the part a live run cannot prove on demand:
// that an asynchronous ingestion delay is waited out rather than reported as a
// failure, and — the companion property — that a genuine value mismatch fails
// FAST instead of burning the whole timeout under the wrong diagnosis.

import { describe, expect, it } from 'vitest'
import { IngestionTimeoutError, runSmoke, waitForPresence, type SmokeDeps } from './obs-smoke'

const SPAN_ID = 'abcdef0123456789'
const CARRIER = `judge-SMOKE-OBS-${SPAN_ID}-item0`
const SIBLING = `judge-SMOKE-OBS-${SPAN_ID}-gen`.replace('-gen', '-item2')
const OBS_ID = `obs-judge-SMOKE-OBS-${SPAN_ID}-gen`
const SPAN_START_ISO = new Date(Date.UTC(2026, 0, 15, 9, 0, 0)).toISOString()
const GOOD_COST = 4_000 * 0.75e-6 + 16_000 * 0.075e-6 + 1_000 * 4.5e-6

type Trace = Record<string, unknown>

function carrierTrace(over: { totalCost?: number; usage?: Record<string, number> } = {}): Trace {
  return {
    id: CARRIER,
    totalCost: over.totalCost ?? GOOD_COST,
    metadata: { usage_attributed: true },
    observations: [
      {
        id: OBS_ID,
        startTime: SPAN_START_ISO,
        usageDetails: over.usage ?? { input: 4_000, input_cached_tokens: 16_000, output: 1_000 },
        metadata: { reasoning_output_tokens: 800, cache_write_input_tokens: 500 },
      },
    ],
  }
}

const siblingTrace = (): Trace => ({ id: SIBLING, metadata: { usage_attributed: false } })

// A deps factory with a virtual clock: `sleep` advances `now`, so a 120s bound is
// exercised without the test waiting for it.
function makeDeps(
  getTrace: (id: string, call: number) => Trace | undefined,
  opts: { timeoutMs?: number } = {}
): {
  deps: SmokeDeps
  logs: string[]
  counts: { reads: number; sleeps: number; ships: number }
} {
  const logs: string[] = []
  const counts = { reads: 0, sleeps: 0, ships: 0 }
  let clock = 0
  const deps: SmokeDeps = {
    ids: { traceId: 'f'.repeat(32), spanId: SPAN_ID },
    log: m => logs.push(m),
    now: () => clock,
    sleep: async ms => {
      counts.sleeps++
      clock += ms
    },
    timeoutMs: opts.timeoutMs ?? 120_000,
    intervalMs: 2_000,
    getTrace: async id => {
      counts.reads++
      return getTrace(id, counts.reads)
    },
    shipSpans: async () => {
      counts.ships++
      return { observations: 1 }
    },
  }
  return { deps, logs, counts }
}

describe('waitForPresence', () => {
  it('polls past a not-yet-visible read and returns the value once it appears', async () => {
    let n = 0
    const logs: string[] = []
    let clock = 0
    const got = await waitForPresence('the thing', async () => (++n < 4 ? undefined : 'here'), {
      log: m => logs.push(m),
      now: () => clock,
      sleep: async ms => {
        clock += ms
      },
      timeoutMs: 60_000,
      intervalMs: 1_000,
    })
    expect(got).toBe('here')
    expect(n).toBe(4)
    // Progress is printed while waiting, so a slow instance never reads as a hang.
    expect(logs.filter(l => l.includes('waiting for the thing'))).toHaveLength(3)
    expect(logs.some(l => l.includes('visible after 4 attempt(s)'))).toBe(true)
  })

  it('throws a DISTINGUISHABLE timeout error when the state never appears', async () => {
    let clock = 0
    await expect(
      waitForPresence('the thing', async () => undefined, {
        log: () => {},
        now: () => clock,
        sleep: async ms => {
          clock += ms
        },
        timeoutMs: 10_000,
        intervalMs: 1_000,
      })
    ).rejects.toBeInstanceOf(IngestionTimeoutError)
  })
})

describe('runSmoke — asynchronous ingestion', () => {
  it('PASSES when the trace is 404 and then observation-less before it lands', async () => {
    // The exact race: ingestion only ENQUEUES, so the first reads legitimately show
    // nothing. Without polling this is a spurious FAIL on a working instance.
    const { deps, logs, counts } = makeDeps((id, call) => {
      if (id === CARRIER) {
        if (call <= 2) return undefined // 404 — not visible yet
        if (call === 3) return { id: CARRIER, metadata: {}, observations: [] } // trace, no obs yet
        return carrierTrace()
      }
      return siblingTrace()
    })
    const code = await runSmoke(deps)
    expect(code).toBe(0)
    expect(logs).toContain('PASS')
    expect(counts.sleeps).toBeGreaterThan(0) // it really waited
    expect(logs.some(l => l.includes('waiting for the observation on'))).toBe(true)
    expect(logs.every(l => !l.includes('[FAIL]'))).toBe(true)
  })

  it('FAILS FAST on a wrong cost — the value is asserted once, never polled for', async () => {
    const { deps, logs, counts } = makeDeps(id =>
      id === CARRIER ? carrierTrace({ totalCost: 0.0195 }) : siblingTrace()
    )
    const code = await runSmoke(deps)
    expect(code).toBe(1)
    // Visible immediately → zero waiting. A value-polling implementation would have
    // slept out the entire 120s bound before reporting the same mismatch.
    expect(counts.sleeps).toBe(0)
    expect(logs.some(l => l.includes('expected') && l.includes('[FAIL]'))).toBe(true)
    // The diagnosis names the RIGHT bug: landed, but wrong.
    expect(logs.some(l => l.includes('ingestion landed but'))).toBe(true)
  })

  it('FAILS FAST on a wrong usage bucket too', async () => {
    const { deps, logs, counts } = makeDeps(id =>
      id === CARRIER
        ? carrierTrace({ usage: { input: 20_000, input_cached_tokens: 16_000, output: 1_000 } })
        : siblingTrace()
    )
    expect(await runSmoke(deps)).toBe(1)
    expect(counts.sleeps).toBe(0)
    expect(logs.some(l => l.includes('usageDetails') && l.includes('[FAIL]'))).toBe(true)
  })

  it('raises the timeout error — NOT a value mismatch — when ingestion never lands', async () => {
    const { deps, counts } = makeDeps(() => undefined, { timeoutMs: 20_000 })
    await expect(runSmoke(deps)).rejects.toBeInstanceOf(IngestionTimeoutError)
    // Bounded: it gave up rather than polling forever.
    expect(counts.reads).toBeLessThan(20)
  })

  it('polls the SIBLING read and the RE-EXPORT read too, not only the first', async () => {
    const seen: string[] = []
    let carrierReads = 0
    let siblingReads = 0
    const { deps } = makeDeps(id => {
      seen.push(id)
      if (id === SIBLING) {
        siblingReads++
        return siblingReads < 3 ? undefined : siblingTrace()
      }
      carrierReads++
      // Visible for the first assertion; the re-export read is briefly stale again.
      if (carrierReads === 1) return carrierTrace()
      return carrierReads < 4 ? undefined : carrierTrace()
    })
    expect(await runSmoke(deps)).toBe(0)
    expect(siblingReads).toBe(3)
    expect(carrierReads).toBe(4)
  })
})
