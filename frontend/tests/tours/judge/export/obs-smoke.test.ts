// The live smoke's own harness, driven against a STUBBED transport that models the
// deployed server's ASYNCHRONOUS ingestion. The live run itself is the human's Mac
// gate (this sandbox has read-only access to the instance); what is proved here is
// what a live run cannot prove on demand — that an ingestion delay is waited out
// rather than reported as a failure, that a genuine mismatch fails FAST instead of
// burning the bound under the wrong diagnosis, that the re-export's non-duplication
// check can actually fail, and that a stalled read cannot hang the command.
//
// The double models BOTH endpoints SEPARATELY, including the case where they
// DISAGREE — trace detail reporting `observations: []` while the observations
// endpoint returns the priced row. That is not a hypothetical: it is what the
// deployed instance returns, and it is what hung the first live run. A double whose
// two views always agree agrees with itself, not with the server.

import { describe, expect, it } from 'vitest'
import { ApiError } from './langfuse'
import {
  IngestionTimeoutError,
  MARKER_GAP_MS,
  MARKER_TRUNCATION_MS,
  RE_EXPORT_SETTLE_MS,
  SPAN_START_LAG_MS,
  SMOKE_EXPORT_OPTIONS,
  reExportDwellMs,
  listReadErrorDisposition,
  observationCost,
  parseObservationPage,
  readErrorDisposition,
  runSmoke,
  spanTimes,
  waitForPresence,
  type SmokeDeps,
} from './obs-smoke'

const SPAN_ID = 'abcdef0123456789'
const CARRIER = `judge-SMOKE-OBS-${SPAN_ID}-item0`
const SIBLING = `judge-SMOKE-OBS-${SPAN_ID}-item2`
const OBS_ID = `obs-judge-SMOKE-OBS-${SPAN_ID}-gen`
// The fake's virtual clock starts here, so every instant the smoke derives is
// reproducible without pinning a literal date into the production module.
const FAKE_NOW_MS = Date.UTC(2026, 6, 22, 18, 5, 34)
const SPAN_START_ISO = new Date(spanTimes(FAKE_NOW_MS).startMs).toISOString()
const GOOD_COST = 4_000 * 0.75e-6 + 16_000 * 0.075e-6 + 1_000 * 4.5e-6
const GOOD_USAGE = { input: 4_000, input_cached_tokens: 16_000, output: 1_000 }
const GOOD_COST_DETAILS = {
  input: 0.003,
  input_cached_tokens: 0.0012,
  output: 0.0045,
  total: GOOD_COST,
}

type Trace = Record<string, unknown>

interface FakeOpts {
  // Reads that return "not visible yet" before the state appears, per trace id.
  hideCarrierReads?: number
  hideSiblingReads?: number
  // Observations-endpoint reads that return no rows before the observation lands.
  hideObsReads?: number
  // What the SECOND export does to the existing row:
  //   'createOnly' — accepted and ignored; the row keeps the FIRST export's endTime.
  //                  THE MEASURED LIVE BEHAVIOR, and the default.
  //   'duplicates' — a SECOND row appears. The defect that would double-count cost.
  //   'updates'    — the row's endTime becomes the re-export's (upsert semantics).
  //   'reissues'   — still ONE row, but under a DIFFERENT id: the first export's
  //                  observation was replaced rather than left alone, so any score or
  //                  link pointing at the original id now dangles.
  reExport?: 'createOnly' | 'duplicates' | 'updates' | 'reissues'
  // Post-re-export reads before the duplicate shows up, so a dwell is required to see
  // it and a single immediate sample would miss it.
  duplicateAfterReads?: number
  // ...and the read after which it goes away again. A duplicate that existed at any
  // point double-counted the cost while it existed, so it must not be excused by
  // having been compacted away before the last sample.
  duplicateVanishesAfterReads?: number
  // A violation the row shows only BRIEFLY after the re-export, reverting afterwards:
  // a reissue under another id, or a different price. Reading only the final sample
  // would call both clean.
  transientReExport?: 'reissues' | 'reprices'
  transientUntilReads?: number
  // Bound on the re-export dwell window.
  settleMs?: number
  // The endpoint's `totalItems` never moves after the re-export — what a stalled
  // ingestion worker looks like, since a create-only re-export leaves no other trace.
  totalItemsFrozen?: boolean
  // The endpoint reports no `meta.totalItems` at all.
  omitTotalItems?: boolean
  // Per-read deadline used wherever a read is raced.
  readTimeoutMs?: number
  // The re-export call YIELDS to the event loop while in flight, as a real HTTP call
  // does. Lets a test observe whether anything samples during that window.
  yieldDuringShip?: boolean
  // How long that yield lasts, in REAL milliseconds.
  shipYieldMs?: number
  // Wall-clock the EXPORT call itself consumes. `exportSpans` keeps working after
  // posting the generation, so the row can land while the call is still returning.
  shipDurationMs?: number
  // What the TRACE DETAIL endpoint says about this trace, independently of what the
  // observations endpoint says:
  //   'agrees'      — same row, same cost.
  //   'silent'      — `observations: []` + `totalCost: 0` while the observations
  //                   endpoint returns the priced row. THE MEASURED LIVE BEHAVIOR.
  //   'contradicts' — a row IS visible there, at a different cost.
  detailView?: 'agrees' | 'silent' | 'contradicts'
  detailCost?: number
  // The observation's own price, as the observations endpoint reports it.
  obsCost?: number
  // Report the price ONLY as `calculatedTotalCost` (no `costDetails` object).
  omitCostDetails?: boolean
  // Report NO price at all — distinct from a price of zero.
  omitAllCost?: boolean
  usage?: Record<string, number>
  // Reads where the sibling row exists but its final body (metadata) has not landed.
  siblingInitOnlyReads?: number
  // Lines the exporter reports back with its count.
  shipLog?: string[]
  // What the exporter reports for traces / ship failures, per export.
  tracesPerShip?: number[]
  failedPerShip?: number[]
  // What the exporter itself reports (0 = the generation was rejected at POST time).
  observationsPerShip?: number[]
  timeoutMs?: number
  realTimers?: boolean
}

// A fake Langfuse modelling BOTH endpoints the smoke reads, independently — the
// observations endpoint (authoritative: usageDetails / costDetails /
// calculatedTotalCost) and the trace detail endpoint (the trace body, whose
// `observations` array the live instance was measured to leave EMPTY for a row it
// nonetheless prices). `shipSpans` records the endTime the observation WOULD carry
// (read off the span, exactly as the real body does) and the reads serve it back
// after a configurable delay.
//
// The re-export models the MEASURED platform semantics by default: the second
// `generation-create` is accepted and then does NOT modify the row. `reExport` opts
// into the two alternatives — a genuine duplicate (the defect the assertion exists to
// catch) and a real update (a platform change the smoke must announce).
function makeFake(opts: FakeOpts = {}): {
  deps: SmokeDeps
  logs: string[]
  counts: { reads: number; sleeps: number; ships: number; readsDuringSecondShip: number }
} {
  const logs: string[] = []
  const counts = { reads: 0, sleeps: 0, ships: 0, readsDuringSecondShip: 0 }
  let inSecondShip = false
  let clock = FAKE_NOW_MS
  // The endTime the FIRST export's row carries — never overwritten, because the
  // platform does not overwrite it.
  let firstEndIso: string | undefined
  let reExportEndIso: string | undefined
  let carrierReads = 0
  let siblingReads = 0
  let obsReads = 0
  let obsReadsAfterSecondShip = 0
  const shipCounts = opts.observationsPerShip ?? [1, 1]
  const reExport = opts.reExport ?? 'createOnly'

  // What the observations endpoint would return RIGHT NOW.
  const currentRows = (): Array<Record<string, unknown>> => {
    if (obsReads <= (opts.hideObsReads ?? 0)) return []
    const first = observationRow(firstEndIso, opts)
    if (counts.ships < 2) return [first]
    obsReadsAfterSecondShip++
    if (
      opts.transientReExport !== undefined &&
      obsReadsAfterSecondShip <= (opts.transientUntilReads ?? 2)
    ) {
      const row = observationRow(firstEndIso, opts)
      return [
        opts.transientReExport === 'reissues'
          ? { ...row, id: `${OBS_ID}-transient` }
          : { ...row, costDetails: { total: 0.0195 }, calculatedTotalCost: 0.0195 },
      ]
    }
    if (reExport === 'updates') return [observationRow(reExportEndIso, opts)]
    if (reExport === 'reissues') {
      return [{ ...observationRow(reExportEndIso, opts), id: `${OBS_ID}-reissued` }]
    }
    const duplicateVisible =
      obsReadsAfterSecondShip > (opts.duplicateAfterReads ?? 0) &&
      obsReadsAfterSecondShip <= (opts.duplicateVanishesAfterReads ?? Number.MAX_SAFE_INTEGER)
    if (reExport === 'duplicates' && duplicateVisible) {
      // A second ROW, distinguishable by the re-export's endTime — which is what the
      // differing instant is for now that it no longer marks an update.
      return [first, { ...observationRow(reExportEndIso, opts), id: `${OBS_ID}-dup` }]
    }
    return [first]
  }

  const deps: SmokeDeps = {
    ids: { traceId: 'f'.repeat(32), spanId: SPAN_ID },
    // Left UNSET by default so the dwell window is the one runSmoke computes from the
    // observed ingestion latency; tests that need a single sample pin it explicitly.
    ...(opts.settleMs === undefined ? {} : { settleMs: opts.settleMs }),
    ...(opts.readTimeoutMs === undefined ? {} : { readTimeoutMs: opts.readTimeoutMs }),
    log: m => logs.push(m),
    timeoutMs: opts.timeoutMs ?? 120_000,
    intervalMs: opts.realTimers === true ? 20 : 2_000,
    // Real timers deliberately leave BOTH seams unpinned: the failure they exclude is
    // a probe that never settles, which a virtual clock cannot express.
    ...(opts.realTimers === true
      ? {}
      : {
          now: () => clock,
          sleep: async ms => {
            counts.sleeps++
            clock += ms
          },
        }),
    getObservations: async id => {
      counts.reads++
      if (inSecondShip) counts.readsDuringSecondShip++
      if (id !== CARRIER) return { rows: [] }
      obsReads++
      const rows = currentRows()
      if (opts.omitTotalItems === true) return { rows }
      // One per ACCEPTED ingestion event, which is what the live endpoint was measured
      // to report — deliberately NOT the number of surviving rows.
      const events = opts.totalItemsFrozen === true ? 1 : counts.ships
      return { rows, totalItems: events }
    },
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
      return carrierTrace(firstEndIso, opts)
    },
    shipSpans: async spans => {
      counts.ships++
      const n = shipCounts[counts.ships - 1] ?? 1
      // The endTime the shipped body WOULD carry, read off the span exactly as the
      // real body does — so the fake can only show the re-export's instant if the
      // re-export actually happened.
      clock += opts.shipDurationMs ?? 0
      if (counts.ships === 2 && opts.yieldDuringShip === true) {
        inSecondShip = true
        await new Promise(r => setTimeout(r, opts.shipYieldMs ?? 5))
        inSecondShip = false
      }
      const shippedEndIso = new Date(spans[0].end_time_unix_nano / 1e6).toISOString()
      if (counts.ships === 1) firstEndIso = shippedEndIso
      else reExportEndIso = shippedEndIso
      return {
        observations: n,
        traces: opts.tracesPerShip?.[counts.ships - 1] ?? 2,
        failed: opts.failedPerShip?.[counts.ships - 1] ?? 0,
        log: opts.shipLog ?? [],
      }
    },
  }
  return { deps, logs, counts }
}

function observationRow(endTime: string | undefined, opts: FakeOpts): Record<string, unknown> {
  const cost = opts.obsCost ?? GOOD_COST
  return {
    id: OBS_ID,
    traceId: CARRIER,
    type: 'GENERATION',
    startTime: SPAN_START_ISO,
    endTime,
    usageDetails: opts.usage ?? GOOD_USAGE,
    ...(opts.omitCostDetails === true || opts.omitAllCost === true
      ? {}
      : { costDetails: { ...GOOD_COST_DETAILS, total: cost } }),
    ...(opts.omitAllCost === true ? {} : { calculatedTotalCost: cost }),
    metadata: { reasoning_output_tokens: 800, cache_write_input_tokens: 500 },
  }
}

function carrierTrace(endTime: string | undefined, opts: FakeOpts): Trace {
  const view = opts.detailView ?? 'agrees'
  const base: Trace = { id: CARRIER, metadata: { usage_attributed: true } }
  // The live shape: the trace body is there, its observations array is not.
  if (view === 'silent') return { ...base, observations: [], totalCost: 0 }
  return {
    ...base,
    totalCost: view === 'contradicts' ? (opts.detailCost ?? 0.0195) : (opts.obsCost ?? GOOD_COST),
    observations: [observationRow(endTime, opts)],
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
    const { deps, logs, counts } = makeFake({
      hideObsReads: 2,
      hideCarrierReads: 1,
      hideSiblingReads: 1,
    })
    const code = await runSmoke(deps)
    expect(code).toBe(0)
    expect(logs).toContain('PASS')
    expect(counts.sleeps).toBeGreaterThan(0) // it really waited
    expect(logs.some(l => l.includes('waiting for the observation on'))).toBe(true)
    expect(logs.some(l => l.includes('waiting for the final body on'))).toBe(true)
    expect(logs.every(l => !l.includes('[FAIL]'))).toBe(true)
  })

  it('FAILS FAST on a wrong cost — the value is asserted once, never polled for', async () => {
    // The observation's OWN price is wrong; the trace aggregate is left agreeing with
    // it, so what fails is the pinned oracle rather than a cross-view mismatch.
    const { deps, logs, counts } = makeFake({ obsCost: 0.0195, settleMs: 0 })
    expect(await runSmoke(deps)).toBe(1)
    // Visible immediately → zero waiting. A value-polling implementation would have
    // slept out the entire 120s bound before reporting the same mismatch.
    expect(counts.sleeps).toBe(0)
    expect(logs.some(l => l.includes('expected') && l.includes('[FAIL]'))).toBe(true)
    expect(logs.some(l => l.includes('ingestion landed; the values are wrong'))).toBe(true)
  })

  it('FAILS FAST on a wrong usage bucket too', async () => {
    const { deps, logs, counts } = makeFake({
      usage: { input: 20_000, input_cached_tokens: 16_000, output: 1_000 },
      settleMs: 0,
    })
    expect(await runSmoke(deps)).toBe(1)
    expect(counts.sleeps).toBe(0)
    expect(logs.some(l => l.includes('usageDetails') && l.includes('[FAIL]'))).toBe(true)
  })

  it('raises the timeout error — NOT a value mismatch — when ingestion never lands', async () => {
    const { deps, counts } = makeFake({ hideObsReads: 10_000, timeoutMs: 20_000 })
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
    const { deps, logs, counts } = makeFake({ observationsPerShip: [1, 0], settleMs: 0 })
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

  it('does not hang when an OBSERVATIONS read never settles', async () => {
    // The second read path gets the same bound as the first; a per-call-site timeout
    // that was only ever threaded through `getTrace` would hang here forever.
    const started = Date.now()
    const { deps } = makeFake({ timeoutMs: 150, realTimers: true })
    deps.getObservations = () => new Promise(() => {})
    await expect(runSmoke(deps)).rejects.toBeInstanceOf(IngestionTimeoutError)
    expect(Date.now() - started).toBeLessThan(3_000)
  })
})

describe('runSmoke — the re-export must not DUPLICATE (it does not update)', () => {
  it('PASSES on create-only semantics: one row, same id, same cost, endTime unchanged', async () => {
    // The measured live behavior. The second generation-create is accepted and then
    // does nothing. Asserting an UPDATE here would fail a healthy instance forever —
    // which is what a live run did before this was measured.
    const { deps, logs } = makeFake({ reExport: 'createOnly' })
    expect(await runSmoke(deps)).toBe(0)
    expect(logs.some(l => l.includes('re-export') && l.includes('[OK]'))).toBe(true)
    expect(logs.some(l => l.includes(`id ${OBS_ID} (was ${OBS_ID})`))).toBe(true)
    // No update happened, so nothing is announced about one.
    expect(logs.some(l => l.includes('reExportUpdated'))).toBe(false)
  })

  it('FAILS when the re-export DUPLICATES the observation', async () => {
    // The defect the assertion exists to catch: two rows means the trace's cost is
    // counted twice, which is the entire reason the check is here.
    const { deps, logs } = makeFake({ reExport: 'duplicates' })
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('re-export') && l.includes('[FAIL]'))).toBe(true)
    expect(logs.some(l => l.includes('2 observation(s) across'))).toBe(true)
    // ...and everything already computed is still printed, including the cost oracle.
    expect(logs.some(l => l.includes('expected') && l.includes('[OK]'))).toBe(true)
    expect(logs.some(l => l.includes('usageDetails') && l.includes('[OK]'))).toBe(true)
  })

  it('CATCHES a duplicate that appears LATE — a single sample would have missed it', async () => {
    // Non-duplication is a NEGATIVE: there is no state change to wait on, so one read
    // taken straight after the ship proves nothing except that the worker was slow.
    const { deps, logs, counts } = makeFake({ reExport: 'duplicates', duplicateAfterReads: 2 })
    expect(await runSmoke(deps)).toBe(1)
    expect(counts.sleeps).toBeGreaterThan(0) // it really dwelled
    expect(logs.some(l => l.includes('watching') && l.includes('for a duplicate'))).toBe(true)
    expect(logs.some(l => l.includes('re-export') && l.includes('[FAIL]'))).toBe(true)
  })

  it('FAILS when the re-export REISSUES the row under a different id', async () => {
    // One row is not enough on its own. The id is the handle everything else uses to
    // reach this observation, so a silent reissue is a defect even though the count
    // and the price are both perfectly correct.
    const { deps, logs } = makeFake({ reExport: 'reissues' })
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('re-export') && l.includes('[FAIL]'))).toBe(true)
    expect(logs.some(l => l.includes(`${OBS_ID}-reissued (was ${OBS_ID})`))).toBe(true)
  })

  it('FAILS on a duplicate that appeared and then VANISHED before the last sample', async () => {
    // This is what makes every sample of the dwell load-bearing rather than
    // decoration: an assertion that reads only the final read would call this clean.
    const { deps, logs } = makeFake({
      reExport: 'duplicates',
      duplicateAfterReads: 1,
      duplicateVanishesAfterReads: 2,
    })
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('re-export') && l.includes('[FAIL]'))).toBe(true)
    expect(logs.some(l => l.includes('2 observation(s) across'))).toBe(true)
  })

  it('SCALES the dwell to the ingestion latency this instance just demonstrated', async () => {
    // A fixed window is a false pass waiting to happen: under queue load the second
    // event can be processed long after it closes, and the dwell would report clean
    // over a duplicate that had not landed yet. The first export's own latency is the
    // honest calibration — same queue, same work.
    const slow = makeFake({ hideObsReads: 20, reExport: 'duplicates', duplicateAfterReads: 12 })
    expect(await runSmoke(slow.deps)).toBe(1)
    expect(slow.logs.some(l => l.includes('re-export') && l.includes('[FAIL]'))).toBe(true)
    // The same duplicate, arriving at the same read index, is out of reach of the
    // floor-sized window a fast instance would have used.
    const fast = makeFake({
      reExport: 'duplicates',
      duplicateAfterReads: 12,
      settleMs: RE_EXPORT_SETTLE_MS,
    })
    expect(await runSmoke(fast.deps)).toBe(0)
  })

  it('measures ingestion latency from BEFORE the export call, not after it returns', async () => {
    // `exportSpans` keeps working after posting the generation, so the row can land
    // while the call is still returning. Timing from afterwards reads near-zero
    // latency on a slow instance and sizes the dwell off the floor — and a duplicate
    // arriving on that same slow queue lands after the window has closed.
    const { deps, logs } = makeFake({
      shipDurationMs: 30_000,
      reExport: 'duplicates',
      duplicateAfterReads: 20,
    })
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('re-export') && l.includes('[FAIL]'))).toBe(true)
    // The floor-sized window would have stopped ~11 samples in and missed it.
    const { deps: floored } = makeFake({
      shipDurationMs: 30_000,
      reExport: 'duplicates',
      duplicateAfterReads: 20,
      settleMs: RE_EXPORT_SETTLE_MS,
    })
    expect(await runSmoke(floored)).toBe(0)
  })

  it('starts sampling BEFORE the re-export call returns', async () => {
    // exportSpans posts the generation and then keeps working, so ingestion can expose
    // a duplicate while that call is still in flight. A dwell that only began
    // afterwards has a blind spot exactly where the write it watches for happens.
    const { deps, counts } = makeFake({ yieldDuringShip: true })
    expect(await runSmoke(deps)).toBe(0)
    // Reads landed while the re-export call was still in flight — which is only
    // possible if the dwell was already running when it was issued.
    expect(counts.readsDuringSecondShip).toBeGreaterThan(0)
  })

  it('gives each dwell read the normal per-read bound, not the leftover window', async () => {
    // A healthy-but-slow read near the boundary is not a stalled ingestion. Bounding
    // it by whatever is left of the dwell turns ordinary latency into a false failure.
    // Virtual clock for the window, REAL time for the read: the read takes 60ms of
    // wall clock while the window has 30 "ms" left, which is precisely the case a
    // window-shaped read bound misreports as a stalled ingestion.
    const { deps } = makeFake({ settleMs: 30, readTimeoutMs: 2_000 })
    const inner = deps.getObservations
    deps.getObservations = async id => {
      await new Promise(r => setTimeout(r, 60))
      return inner(id)
    }
    expect(await runSmoke(deps)).toBe(0)
  })

  it('FAILS on a duplicate visible ONLY while the export call was in flight', async () => {
    // The in-flight phase is evidence, not decoration: a duplicate that existed only
    // during the export still double-counted the cost while it existed, and the
    // post-export window alone would never see it.
    const { deps, logs } = makeFake({
      yieldDuringShip: true,
      reExport: 'duplicates',
      duplicateAfterReads: 1,
      duplicateVanishesAfterReads: 3,
    })
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('re-export') && l.includes('[FAIL]'))).toBe(true)
    expect(logs.some(l => l.includes('2 observation(s) across'))).toBe(true)
  })

  it('names an ANOMALY when nothing proves the second event was ever processed', async () => {
    // The export proves the POST was accepted, nothing more. A stalled worker looks
    // exactly like a correct create-only re-export — every sample sees the original
    // row — so non-duplication would "pass" without the write having been handled.
    const { deps, logs } = makeFake({ totalItemsFrozen: true })
    expect(await runSmoke(deps)).toBe(0)
    expect(logs.some(l => l.includes('reExportUnconfirmed') && l.includes('[ANOMALY]'))).toBe(true)
    expect(logs.some(l => l.includes('totalItems did not move'))).toBe(true)
  })

  it('stays silent about processing when the count DID move', async () => {
    const { deps, logs } = makeFake()
    expect(await runSmoke(deps)).toBe(0)
    expect(logs.some(l => l.includes('reExportUnconfirmed'))).toBe(false)
    expect(logs).toContain('PASS')
  })

  it('does not invent an anomaly when the endpoint reports no count at all', async () => {
    // Absence of the signal is not evidence of a stalled worker.
    const { deps, logs } = makeFake({ omitTotalItems: true })
    expect(await runSmoke(deps)).toBe(0)
    expect(logs.some(l => l.includes('reExportUnconfirmed'))).toBe(false)
  })

  it('watches for the WHOLE export, even one that outlives the settle window', async () => {
    // The in-flight phase is bounded by the export, not by `settleMs`. Otherwise a
    // long export goes unwatched for its remainder, and the post-export window cannot
    // see what came and went in that gap.
    const { deps, logs } = makeFake({
      yieldDuringShip: true,
      settleMs: 4_000, // far shorter than the in-flight sampling that follows
      reExport: 'duplicates',
      duplicateAfterReads: 6,
      duplicateVanishesAfterReads: 8,
    })
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('re-export') && l.includes('[FAIL]'))).toBe(true)
  })

  it('JOINS the in-flight watcher when the export throws, leaving nothing running', async () => {
    // Otherwise the exception skips the join and the watcher keeps issuing reads —
    // and holding a timer — after the run has already failed, delaying exit.
    // REAL timers: the failure being excluded is a watcher that is still asleep when
    // the export throws and wakes up afterwards, which a virtual clock (whose sleeps
    // resolve instantly) cannot express.
    const { deps, counts } = makeFake({ realTimers: true, yieldDuringShip: true })
    const shipInner = deps.shipSpans
    deps.shipSpans = async spans => {
      const res = await shipInner(spans)
      if (counts.ships === 2) throw new Error('export blew up')
      return res
    }
    await expect(runSmoke(deps)).rejects.toThrow('export blew up')
    const readsAtFailure = counts.reads
    await new Promise(r => setTimeout(r, 30))
    expect(counts.reads).toBe(readsAtFailure) // nothing kept polling
  })

  it('FAILS on a TRANSIENT reissue that reverted before the final sample', async () => {
    // Same reasoning as the transient duplicate: the invariant was broken while it was
    // broken. A check that read only the last sample would call this clean.
    const { deps, logs } = makeFake({ transientReExport: 'reissues' })
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('re-export') && l.includes('[FAIL]'))).toBe(true)
    expect(logs.some(l => l.includes('worst sample') && l.includes('-transient'))).toBe(true)
  })

  it('FAILS on a TRANSIENT repricing that reverted before the final sample', async () => {
    const { deps, logs } = makeFake({ transientReExport: 'reprices' })
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('worst sample') && l.includes('cost 0.019500'))).toBe(true)
  })

  it('FAILS when the window ENDS BLIND — served at first, unserved at the boundary', async () => {
    // One stalled read can consume the rest of the window, so a dwell that ends blind
    // never observed its own tail — the exact stretch a late duplicate lands in.
    const { deps, logs, counts } = makeFake()
    const inner = deps.getObservations
    let postReads = 0
    deps.getObservations = async id => {
      if (counts.ships < 2) return inner(id)
      postReads++
      return postReads === 1 ? inner(id) : undefined
    }
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('the last read was not served'))).toBe(true)
    expect(logs.some(l => l.includes('the values are wrong'))).toBe(false)
  })

  it('reports an ALL-UNSERVED post-export window as a read failure, not a value mismatch', async () => {
    // Returning the empty default would surface as "0 observation(s)" under the
    // "ingestion landed; the values are wrong" diagnosis — blaming the payload for
    // what is a read failure. One stalled read is enough: the per-read allowance
    // deliberately exceeds the default window.
    const { deps, logs, counts } = makeFake()
    const inner = deps.getObservations
    deps.getObservations = async id => (counts.ships >= 2 ? undefined : inner(id))
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('re-export') && l.includes('no read was served'))).toBe(true)
    expect(logs.some(l => l.includes('the values are wrong'))).toBe(false)
    expect(logs.some(l => l.includes('0 observation(s)'))).toBe(false)
  })

  it('keeps a FULL window AFTER the export returns, not one shared across it', async () => {
    // The generation is posted late in the export, so a slow export can consume the
    // whole settle time before the write under test is even sent. A single window
    // spanning both is over before there is anything to observe.
    const { deps, logs } = makeFake({
      settleMs: 5_000,
      shipDurationMs: 5_000, // the export alone exhausts a shared window
      reExport: 'duplicates',
      duplicateAfterReads: 3,
    })
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('re-export') && l.includes('[FAIL]'))).toBe(true)
  })

  it('never leaves the in-flight watcher as an unhandled rejection when the export throws', async () => {
    // The watcher is started before the export is awaited. If its read fails while the
    // export is still in flight — and the export then throws, so the later handler is
    // never reached — an unattached rejection fails the process outright, with no
    // diagnosis, instead of the export's own error being reported.
    const unhandled: unknown[] = []
    const onUnhandled = (err: unknown): void => {
      unhandled.push(err)
    }
    process.on('unhandledRejection', onUnhandled)
    try {
      const { deps } = makeFake({ yieldDuringShip: true })
      let reads = 0
      const inner = deps.getObservations
      deps.getObservations = async id => {
        reads++
        if (reads > 3) throw new Error('read blew up mid-export')
        return inner(id)
      }
      const shipInner = deps.shipSpans
      deps.shipSpans = async spans => {
        const res = await shipInner(spans)
        if (res.observations === 1 && reads > 0) throw new Error('export blew up')
        return res
      }
      await expect(runSmoke(deps)).rejects.toThrow(/blew up/)
      // Let any stray rejection surface before asserting there was none.
      await new Promise(r => setTimeout(r, 20))
      expect(unhandled).toEqual([])
    } finally {
      process.off('unhandledRejection', onUnhandled)
    }
  })

  it('samples AT the window boundary, not one interval short of it', async () => {
    // 5s window, 2s interval: samples at 0/2/4 leave the final second unobserved. A
    // duplicate landing there is exactly what the check exists to catch.
    const { deps, logs } = makeFake({
      settleMs: 5_000,
      reExport: 'duplicates',
      // Visible ONLY on the boundary sample: post-ship reads land at 2s, 4s and 5s, so
      // a dwell that stopped as soon as a whole interval no longer fit sees 1 and 2.
      duplicateAfterReads: 2,
    })
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('re-export') && l.includes('[FAIL]'))).toBe(true)
  })

  it('reports an ANOMALY — not a pass, not a failure — if the platform starts UPDATING', async () => {
    // Better than assumed, but still a change to a property the backfill notes depend
    // on. It must be said out loud rather than passing silently.
    const { deps, logs } = makeFake({ reExport: 'updates' })
    expect(await runSmoke(deps)).toBe(0)
    expect(logs.some(l => l.includes('reExportUpdated') && l.includes('[ANOMALY]'))).toBe(true)
    expect(logs.some(l => l.includes('re-export') && l.includes('[OK]'))).toBe(true)
  })

  it('FAILS when the surviving row is REPRICED by the re-export', async () => {
    // One row and a stable id are not enough on their own: a re-export that changed
    // the price would still satisfy both while corrupting the spend figure.
    const { deps, logs } = makeFake({ reExport: 'createOnly' })
    const inner = deps.getObservations
    let ships = 0
    deps.shipSpans = (
      (orig: SmokeDeps['shipSpans']) => async (spans: Parameters<SmokeDeps['shipSpans']>[0]) => {
        ships++
        return orig(spans)
      }
    )(deps.shipSpans)
    deps.getObservations = async id => {
      const page = (await inner(id)) ?? { rows: [] }
      if (ships < 2) return page
      return { ...page, rows: page.rows.map(r => ({ ...r, costDetails: { total: 0.0195 } })) }
    }
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('re-export') && l.includes('[FAIL]'))).toBe(true)
    expect(logs.some(l => l.includes('cost 0.019500 (was 0.008700)'))).toBe(true)
  })
})

describe('runSmoke — the two endpoints disagree (the measured live behavior)', () => {
  it('PASSES on the LIVE shape: trace detail empty, observations endpoint priced', async () => {
    // Exactly what the deployed instance returns: `/traces/{id}` reports
    // `observations: []` and `totalCost: 0` indefinitely, while
    // `/observations?traceId=…` returns the priced GENERATION row. A smoke that read
    // presence or cost off the trace detail endpoint polls until its bound fires and
    // blames the ingestion worker for a healthy instance.
    const { deps, logs, counts } = makeFake({ detailView: 'silent', settleMs: 0 })
    expect(await runSmoke(deps)).toBe(0)
    expect(counts.sleeps).toBe(0) // never waited on something it could not see
    expect(logs.some(l => l.includes('expected') && l.includes('[OK]'))).toBe(true)
    expect(logs.some(l => l.includes('re-export') && l.includes('[OK]'))).toBe(true)
    // ...and the divergence is NAMED rather than silently trusted away.
    expect(logs.some(l => l.includes('[ANOMALY]') && l.includes('viewsDisagree'))).toBe(true)
    expect(logs.some(l => l.includes('observations endpoint is authoritative'))).toBe(true)
    expect(logs.some(l => l === 'PASS (with 1 anomaly/anomalies: viewsDisagree)')).toBe(true)
  })

  it('FAILS when the detail view is POPULATED and contradicts the observation', async () => {
    // Distinct from the silent case: both endpoints can see the row and describe it
    // differently, which no reading of "the detail endpoint omits some rows" excuses.
    const { deps, logs } = makeFake({ detailView: 'contradicts', detailCost: 0.0195 })
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('viewsAgree') && l.includes('[FAIL]'))).toBe(true)
    // The oracle still passed — the observation itself was priced correctly.
    expect(logs.some(l => l.includes('expected') && l.includes('[OK]'))).toBe(true)
  })

  it('reports agreement as a plain assertion when both views match', async () => {
    const { deps, logs } = makeFake({ detailView: 'agrees' })
    expect(await runSmoke(deps)).toBe(0)
    expect(logs.some(l => l.includes('viewsAgree') && l.includes('[OK]'))).toBe(true)
    expect(logs.some(l => l.includes('[ANOMALY]'))).toBe(false)
  })

  it('prices from calculatedTotalCost when the row carries no costDetails object', async () => {
    const { deps, logs } = makeFake({ omitCostDetails: true })
    expect(await runSmoke(deps)).toBe(0)
    expect(logs.some(l => l.includes('expected') && l.includes('[OK]'))).toBe(true)
  })

  it('says NOT REPORTED — not "priced at zero" — when the row carries no price', async () => {
    // An unpriced row and a row priced at nothing are different defects; the output
    // must not collapse them into the same "0.000000" line.
    const { deps, logs } = makeFake({ omitAllCost: true, detailView: 'silent' })
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('observationCost') && l.includes('NOT REPORTED'))).toBe(true)
    expect(logs.some(l => l.includes('observationCost') && l.includes('0.000000'))).toBe(false)
  })
})

describe("the smoke's own configuration", () => {
  it('keeps SMOKE-OBS out of the standing triage queue (zero pass-salt)', () => {
    // The smoke's verdicts are all `pass` and non-mutation, so the salt sample is the
    // only path into the queue a human uses for real labeling.
    expect(SMOKE_EXPORT_OPTIONS.saltPasses).toBe(0)
  })

  it('keeps the two exports tellable apart at the endTime comparison granularity', () => {
    // The re-export ships a DIFFERENT end instant so its row is distinguishable from
    // the first export's — that is what makes a duplicate unmistakable and what lets
    // an actual update announce itself. The comparison truncates to whole seconds, so
    // a gap below that granularity silently collapses both. Asserted against the
    // clock-derived instants, at several clock readings, so the relationship is what
    // is guarded rather than one snapshot of it.
    expect(MARKER_GAP_MS).toBeGreaterThan(MARKER_TRUNCATION_MS)
    for (const nowMs of [FAKE_NOW_MS, Date.UTC(2026, 0, 1, 0, 0, 0, 999), Date.now()]) {
      const t = spanTimes(nowMs)
      expect(t.reExportEndMs).toBe(t.endMs + MARKER_GAP_MS)
      expect(new Date(t.reExportEndMs).toISOString().slice(0, 19)).not.toBe(
        new Date(t.endMs).toISOString().slice(0, 19)
      )
    }
  })

  it('stamps the span NEAR-PRESENT, so the fixture matches what a real round ships', () => {
    // The January-instant fixture is what made the smoke unrepresentative: a row six
    // months from `now` can be served by one endpoint and omitted by another.
    const t = spanTimes(FAKE_NOW_MS)
    expect(FAKE_NOW_MS - t.startMs).toBe(SPAN_START_LAG_MS)
    expect(SPAN_START_LAG_MS).toBeLessThan(60 * 60_000)
    // ...and still far enough from the export clock that a startTime taken from THAT
    // fails the startTime assertion rather than passing by coincidence.
    expect(new Date(t.startMs).toISOString().slice(0, 19)).not.toBe(
      new Date(FAKE_NOW_MS).toISOString().slice(0, 19)
    )
    expect(t.reExportEndMs).toBeLessThanOrEqual(FAKE_NOW_MS)
  })

  it('FAILS the startTime assertion when the observation is stamped with the export clock', () => {
    // The guard the near-present fixture must not weaken.
    const t = spanTimes(FAKE_NOW_MS)
    expect(
      new Date(FAKE_NOW_MS).toISOString().startsWith(new Date(t.startMs).toISOString().slice(0, 19))
    ).toBe(false)
  })
})

describe('reExportDwellMs — the dwell window is measured, not assumed', () => {
  it('never drops below the floor, however fast the instance was', () => {
    expect(reExportDwellMs(0, 120_000)).toBe(RE_EXPORT_SETTLE_MS)
    expect(reExportDwellMs(100, 120_000)).toBe(RE_EXPORT_SETTLE_MS)
    expect(reExportDwellMs(-5, 120_000)).toBe(RE_EXPORT_SETTLE_MS)
  })

  it('scales past the floor when the first export was slow to land', () => {
    expect(reExportDwellMs(30_000, 120_000)).toBe(90_000)
  })

  it('stays finite: capped by the ingestion bound', () => {
    expect(reExportDwellMs(600_000, 120_000)).toBe(120_000)
  })
})

describe('observationCost — the price comes from the observation itself', () => {
  it('prefers costDetails.total', () => {
    expect(observationCost({ costDetails: { total: 0.0087 }, calculatedTotalCost: 0.9 })).toBe(
      0.0087
    )
  })

  it('falls back to calculatedTotalCost when costDetails carries no total', () => {
    expect(observationCost({ costDetails: { input: 0.003 }, calculatedTotalCost: 0.0087 })).toBe(
      0.0087
    )
    expect(observationCost({ calculatedTotalCost: 0.0087 })).toBe(0.0087)
  })

  it('reports UNKNOWN rather than zero when neither is present', () => {
    // Zero would read as "priced at nothing" and fail the oracle under the wrong
    // diagnosis; undefined lets the caller say the price was not reported at all.
    expect(observationCost({ id: 'x' })).toBeUndefined()
    expect(observationCost({ costDetails: null })).toBeUndefined()
  })
})

describe('parseObservationPage — the envelope is never coerced to "no rows"', () => {
  it('returns the data array', () => {
    expect(parseObservationPage({ data: [{ id: 'a' }], meta: {} }, 'T').rows).toEqual([{ id: 'a' }])
    expect(parseObservationPage({ data: [], meta: {} }, 'T').rows).toEqual([])
  })

  it('carries meta.totalItems through, and tolerates its absence', () => {
    // Never a row count — it is the only evidence available that an event was
    // PROCESSED, and the assertions read rows.
    expect(
      parseObservationPage({ data: [{ id: 'a' }], meta: { totalItems: 2 } }, 'T').totalItems
    ).toBe(2)
    expect(parseObservationPage({ data: [], meta: {} }, 'T').totalItems).toBeUndefined()
    expect(parseObservationPage({ data: [] }, 'T').totalItems).toBeUndefined()
  })

  it('RAISES on an envelope it does not recognise, instead of reporting emptiness', () => {
    // An empty read is waited out as "never landed" and blames the ingestion worker;
    // a raise names the real fault (the API changed shape).
    expect(() => parseObservationPage({ observations: [] }, 'T')).toThrow(/unrecognised envelope/)
    expect(() => parseObservationPage([], 'T')).toThrow(/unrecognised envelope/)
    expect(() => parseObservationPage(null, 'T')).toThrow(/unrecognised envelope/)
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

describe('listReadErrorDisposition — a COLLECTION read is classified differently', () => {
  it('treats a 404 as FATAL: an absent row is a 200 with empty data, so this is the route', () => {
    // Retrying it burns the whole bound and then blames the ingestion worker for an
    // instance that simply does not serve the endpoint.
    expect(
      listReadErrorDisposition(new ApiError(404, 'not found', 'GET', '/api/public/observations'))
    ).toBe('fatal')
  })

  it('still retries an abort — a bounded read that did not come back in time', () => {
    const abort = new Error('The operation was aborted')
    abort.name = 'AbortError'
    expect(listReadErrorDisposition(abort)).toBe('retry')
  })

  it('does NOT swallow a real transport failure', () => {
    expect(listReadErrorDisposition(new ApiError(500, 'boom', 'GET', '/o'))).toBe('fatal')
    expect(listReadErrorDisposition(new Error('ECONNREFUSED'))).toBe('fatal')
  })
})

describe("runSmoke — the exporter's own failure counts are never discarded", () => {
  it('FAILS when the FIRST export reported a trace ship failure', async () => {
    const { deps, logs } = makeFake({ failedPerShip: [1] })
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('reported 1 trace ship failure(s)'))).toBe(true)
  })

  it('FAILS when the RE-EXPORT reported a trace ship failure, even though the observation landed', async () => {
    // The masking case: the sibling row and the carrier marker from the first export
    // satisfy every later probe, so only the discarded `failed` count exposes this.
    const { deps, logs } = makeFake({ failedPerShip: [0, 1] })
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('re-export reported 1 trace ship failure(s)'))).toBe(true)
    // ...and everything already computed is still printed, including the cost oracle.
    expect(logs.some(l => l.includes('expected') && l.includes('[OK]'))).toBe(true)
    expect(logs.some(l => l.includes('usageDetails'))).toBe(true)
    expect(logs.some(l => l.includes('re-export') && l.includes('[FAIL]'))).toBe(true)
  })

  it('FAILS when an export shipped the wrong number of traces', async () => {
    const { deps, logs } = makeFake({ tracesPerShip: [1] })
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('shipped 1 trace(s), expected 2'))).toBe(true)
  })
})
