// The live smoke's own harness, driven against a STUBBED transport that models the
// deployed server's ASYNCHRONOUS ingestion. The live run itself is the human's Mac
// gate (this sandbox has read-only access to the instance); what is proved here is
// what a live run cannot prove on demand — that an ingestion delay is waited out
// rather than reported as a failure, that a genuine mismatch fails FAST instead of
// burning the bound under the wrong diagnosis, that the upsert check can actually
// fail, and that a stalled read cannot hang the command.

import { describe, expect, it } from 'vitest'
import { ApiError } from './langfuse'
import {
  FIRST_END_MS,
  IngestionTimeoutError,
  MARKER_GAP_MS,
  MARKER_TRUNCATION_MS,
  RE_EXPORT_END_MS,
  SMOKE_EXPORT_OPTIONS,
  readErrorDisposition,
  runSmoke,
  waitForPresence,
  type SmokeDeps,
} from './obs-smoke'

const SPAN_ID = 'abcdef0123456789'
const CARRIER = `judge-SMOKE-OBS-${SPAN_ID}-item0`
const SIBLING = `judge-SMOKE-OBS-${SPAN_ID}-item2`
const OBS_ID = `obs-judge-SMOKE-OBS-${SPAN_ID}-gen`
const START_MS = Date.UTC(2026, 0, 15, 9, 0, 0)
const SPAN_START_ISO = new Date(START_MS).toISOString()
const GOOD_COST = 4_000 * 0.75e-6 + 16_000 * 0.075e-6 + 1_000 * 4.5e-6
const GOOD_USAGE = { input: 4_000, input_cached_tokens: 16_000, output: 1_000 }

type Trace = Record<string, unknown>

interface FakeOpts {
  // Reads that return "not visible yet" before the state appears, per trace id.
  hideCarrierReads?: number
  hideSiblingReads?: number
  // Reads AFTER the re-export before its marker becomes visible.
  hideUpsertReads?: number
  totalCost?: number
  usage?: Record<string, number>
  // The re-export silently does nothing — the upsert never lands.
  reExportIsNoOp?: boolean
  // Reads where the sibling row exists but its final body (metadata) has not landed.
  siblingInitOnlyReads?: number
  // Lines the exporter reports back with its count.
  shipLog?: string[]
  // What the exporter itself reports (0 = the generation was rejected at POST time).
  observationsPerShip?: number[]
  timeoutMs?: number
  realTimers?: boolean
}

// A fake Langfuse: `shipSpans` records the endTime the observation WOULD carry (read
// off the span, exactly as the real body does), and `getTrace` serves it back after a
// configurable delay. This is what makes the upsert marker meaningful — the stub can
// only show the second endTime if the second export actually happened.
function makeFake(opts: FakeOpts = {}): {
  deps: SmokeDeps
  logs: string[]
  counts: { reads: number; sleeps: number; ships: number }
} {
  const logs: string[] = []
  const counts = { reads: 0, sleeps: 0, ships: 0 }
  let clock = 0
  let obsEndIso: string | undefined
  let carrierReads = 0
  let siblingReads = 0
  let readsAfterSecondShip = 0
  const shippedEndTimes: string[] = []
  const shipCounts = opts.observationsPerShip ?? [1, 1]

  const deps: SmokeDeps = {
    ids: { traceId: 'f'.repeat(32), spanId: SPAN_ID },
    log: m => logs.push(m),
    timeoutMs: opts.timeoutMs ?? 120_000,
    intervalMs: opts.realTimers === true ? 20 : 2_000,
    ...(opts.realTimers === true
      ? {}
      : {
          now: () => clock,
          sleep: async ms => {
            counts.sleeps++
            clock += ms
          },
        }),
    getTrace: async id => {
      counts.reads++
      if (id === SIBLING) {
        siblingReads++
        if (siblingReads <= (opts.hideSiblingReads ?? 0)) return undefined
        // The init trace-create lands FIRST: the row is readable with no metadata at
        // all, which is where a naive read would assert `usage_attributed === false`
        // against `undefined` and report a false mismatch.
        if (siblingReads <= (opts.hideSiblingReads ?? 0) + (opts.siblingInitOnlyReads ?? 0)) {
          return { id: SIBLING, metadata: {} }
        }
        return { id: SIBLING, metadata: { usage_attributed: false } }
      }
      carrierReads++
      if (carrierReads <= (opts.hideCarrierReads ?? 0)) return undefined
      if (counts.ships >= 2) {
        readsAfterSecondShip++
        if (readsAfterSecondShip <= (opts.hideUpsertReads ?? 0)) {
          // Visible, but still carrying the PRE-upsert state.
          return carrierTrace(preUpsertEndIso, opts)
        }
      }
      return carrierTrace(obsEndIso, opts)
    },
    shipSpans: async spans => {
      counts.ships++
      const n = shipCounts[counts.ships - 1] ?? 1
      if (counts.ships === 2 && opts.reExportIsNoOp === true) {
        return { observations: n, log: opts.shipLog ?? [] }
      }
      preUpsertEndIso = obsEndIso
      obsEndIso = new Date(spans[0].end_time_unix_nano / 1e6).toISOString()
      shippedEndTimes.push(obsEndIso)
      return { observations: n, log: opts.shipLog ?? [] }
    },
  }
  let preUpsertEndIso: string | undefined
  return { deps, logs, counts }
}

function carrierTrace(endTime: string | undefined, opts: FakeOpts): Trace {
  return {
    id: CARRIER,
    totalCost: opts.totalCost ?? GOOD_COST,
    metadata: { usage_attributed: true },
    observations: [
      {
        id: OBS_ID,
        startTime: SPAN_START_ISO,
        endTime,
        usageDetails: opts.usage ?? GOOD_USAGE,
        metadata: { reasoning_output_tokens: 800, cache_write_input_tokens: 500 },
      },
    ],
  }
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

  it('bounds the PROBE itself: a read that never settles still hits the deadline', async () => {
    // Real timers: the failure being excluded is a probe that parks on `await`
    // forever, which a virtual clock cannot express.
    const started = Date.now()
    await expect(
      waitForPresence('the thing', () => new Promise<string>(() => {}), {
        log: () => {},
        timeoutMs: 150,
        intervalMs: 20,
      })
    ).rejects.toBeInstanceOf(IngestionTimeoutError)
    expect(Date.now() - started).toBeLessThan(3_000)
  })
})

describe('runSmoke — asynchronous ingestion', () => {
  it('PASSES when the trace is 404 and then observation-less before it lands', async () => {
    const { deps, logs, counts } = makeFake({ hideCarrierReads: 2, hideSiblingReads: 1 })
    const code = await runSmoke(deps)
    expect(code).toBe(0)
    expect(logs).toContain('PASS')
    expect(counts.sleeps).toBeGreaterThan(0) // it really waited
    expect(logs.some(l => l.includes('waiting for the observation + final body'))).toBe(true)
    expect(logs.every(l => !l.includes('[FAIL]'))).toBe(true)
  })

  it('FAILS FAST on a wrong cost — the value is asserted once, never polled for', async () => {
    const { deps, logs, counts } = makeFake({ totalCost: 0.0195 })
    expect(await runSmoke(deps)).toBe(1)
    // Visible immediately → zero waiting. A value-polling implementation would have
    // slept out the entire 120s bound before reporting the same mismatch.
    expect(counts.sleeps).toBe(0)
    expect(logs.some(l => l.includes('expected') && l.includes('[FAIL]'))).toBe(true)
    expect(logs.some(l => l.includes('ingestion landed but'))).toBe(true)
  })

  it('FAILS FAST on a wrong usage bucket too', async () => {
    const { deps, logs, counts } = makeFake({
      usage: { input: 20_000, input_cached_tokens: 16_000, output: 1_000 },
    })
    expect(await runSmoke(deps)).toBe(1)
    expect(counts.sleeps).toBe(0)
    expect(logs.some(l => l.includes('usageDetails') && l.includes('[FAIL]'))).toBe(true)
  })

  it('raises the timeout error — NOT a value mismatch — when ingestion never lands', async () => {
    const { deps, counts } = makeFake({ hideCarrierReads: 10_000, timeoutMs: 20_000 })
    await expect(runSmoke(deps)).rejects.toBeInstanceOf(IngestionTimeoutError)
    expect(counts.reads).toBeLessThan(20) // bounded, not forever
  })

  it('fails IMMEDIATELY when the exporter itself shipped no observation', async () => {
    // A generation rejected at POST time means no observation is COMING. Polling for
    // one would burn the full bound and then blame the ingestion worker for what is
    // an export-side rejection — the wrong diagnosis, loudly stated.
    const { deps, logs, counts } = makeFake({ observationsPerShip: [0] })
    expect(await runSmoke(deps)).toBe(1)
    expect(counts.reads).toBe(0) // never even read back
    expect(counts.sleeps).toBe(0)
    expect(logs.some(l => l.includes('shipped 0 observation(s)'))).toBe(true)
    expect(logs.some(l => l.includes('not a worker race'))).toBe(true)
  })

  it('waits for the RE-EXPORT to be processed before asserting the upsert', async () => {
    const { deps, logs } = makeFake({ hideUpsertReads: 2 })
    expect(await runSmoke(deps)).toBe(0)
    // It waited on the marker specifically, not merely on "an observation exists".
    expect(logs.some(l => l.includes('waiting for the re-export to be processed'))).toBe(true)
  })

  it('FAILS when the re-export never processes (the upsert check can actually fail)', async () => {
    // The first export's observation is already present, so a presence-only probe
    // would return instantly and "prove" an upsert that never happened.
    const { deps } = makeFake({ reExportIsNoOp: true, timeoutMs: 20_000 })
    await expect(runSmoke(deps)).rejects.toBeInstanceOf(IngestionTimeoutError)
  })

  it('waits for the SIBLING final body instead of asserting against the init-only row', async () => {
    // exportSpans sends a minimal init trace-create first, so the sibling is readable
    // before `usage_attributed` exists. Reading it then yields `undefined !== false`
    // — a FALSE mismatch on a healthy instance.
    const { deps, logs } = makeFake({ siblingInitOnlyReads: 2 })
    expect(await runSmoke(deps)).toBe(0)
    expect(logs.some(l => l.includes('waiting for the sibling trace'))).toBe(true)
    expect(logs.some(l => l.includes('usage_attributed') && l.includes('[OK]'))).toBe(true)
  })

  it('short-circuits on a RE-EXPORT that shipped no observation, without polling', async () => {
    const { deps, logs, counts } = makeFake({ observationsPerShip: [1, 0] })
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('re-export shipped 0 observation(s)'))).toBe(true)
    expect(counts.sleeps).toBe(0) // never waited on a marker that cannot appear
  })

  it("prints the exporter's own log when it shipped nothing (the rejection reason)", async () => {
    const { deps, logs } = makeFake({
      observationsPerShip: [0],
      shipLog: ['  generation-create failed for judge-X: ingestion rejected: invalid usageDetails'],
    })
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('exporter log:'))).toBe(true)
    expect(logs.some(l => l.includes('invalid usageDetails'))).toBe(true)
  })

  it('does not hang when a trace read never settles', async () => {
    const started = Date.now()
    const { deps } = makeFake({ timeoutMs: 150, realTimers: true })
    deps.getTrace = () => new Promise(() => {})
    await expect(runSmoke(deps)).rejects.toBeInstanceOf(IngestionTimeoutError)
    expect(Date.now() - started).toBeLessThan(3_000)
  })
})

describe("the smoke's own configuration", () => {
  it('keeps SMOKE-OBS out of the standing triage queue (zero pass-salt)', () => {
    // The smoke's verdicts are all `pass` and non-mutation, so the salt sample is the
    // only path into the queue a human uses for real labeling.
    expect(SMOKE_EXPORT_OPTIONS.saltPasses).toBe(0)
  })

  it('keeps the re-export marker above the endTime comparison granularity', () => {
    // The marker comparison truncates to whole seconds; a gap below that granularity
    // makes the upsert check unfalsifiable again, silently.
    expect(MARKER_GAP_MS).toBeGreaterThan(MARKER_TRUNCATION_MS)
    expect(new Date(RE_EXPORT_END_MS).toISOString().slice(0, 19)).not.toBe(
      new Date(FIRST_END_MS).toISOString().slice(0, 19)
    )
  })
})

describe('readErrorDisposition — how a failed trace read is classified', () => {
  it('retries a 404 (the row has not appeared yet)', () => {
    expect(readErrorDisposition(new ApiError(404, 'not found', 'GET', '/t'))).toBe('retry')
  })

  it('retries an ABORT — the per-read timeout must not kill the run with a raw stack', () => {
    // The measured shape: an aborted fetch rejects with an AbortError, NOT an
    // ApiError, so a 404-only rescue lets it escape every handler.
    const abort = new Error('The operation was aborted')
    abort.name = 'AbortError'
    expect(readErrorDisposition(abort)).toBe('retry')
    const timeout = new Error('timed out')
    timeout.name = 'TimeoutError'
    expect(readErrorDisposition(timeout)).toBe('retry')
  })

  it('does NOT swallow a real transport failure', () => {
    expect(readErrorDisposition(new ApiError(500, 'boom', 'GET', '/t'))).toBe('fatal')
    expect(readErrorDisposition(new Error('ECONNREFUSED'))).toBe('fatal')
  })
})
