// The live smoke's own harness, driven against a STUBBED transport that models the
// deployed server's ASYNCHRONOUS ingestion. The live run itself is the human's Mac
// gate (this sandbox has read-only access to the instance); what is proved here is
// what a live run cannot prove on demand — that an ingestion delay is waited out
// rather than reported as a failure, that a genuine mismatch fails FAST instead of
// burning the bound under the wrong diagnosis, that the upsert check can actually
// fail, and that a stalled read cannot hang the command.
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
  SPAN_START_LAG_MS,
  SMOKE_EXPORT_OPTIONS,
  listReadErrorDisposition,
  observationCost,
  parseObservationList,
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
  // Reads AFTER the re-export before its marker becomes visible.
  hideUpsertReads?: number
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
  // The re-export silently does nothing — the upsert never lands.
  reExportIsNoOp?: boolean
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
// after a configurable delay, so the upsert marker is meaningful: the stub can only
// show the second endTime if the second export actually happened.
function makeFake(opts: FakeOpts = {}): {
  deps: SmokeDeps
  logs: string[]
  counts: { reads: number; sleeps: number; ships: number }
} {
  const logs: string[] = []
  const counts = { reads: 0, sleeps: 0, ships: 0 }
  let clock = FAKE_NOW_MS
  let obsEndIso: string | undefined
  let preUpsertEndIso: string | undefined
  let carrierReads = 0
  let siblingReads = 0
  let obsReads = 0
  let obsReadsAfterSecondShip = 0
  const shipCounts = opts.observationsPerShip ?? [1, 1]

  // What the observations endpoint would return RIGHT NOW.
  const currentRows = (): Array<Record<string, unknown>> => {
    if (obsReads <= (opts.hideObsReads ?? 0)) return []
    if (counts.ships >= 2) {
      obsReadsAfterSecondShip++
      if (obsReadsAfterSecondShip <= (opts.hideUpsertReads ?? 0)) {
        // Visible, but still carrying the PRE-upsert state.
        return [observationRow(preUpsertEndIso, opts)]
      }
    }
    return [observationRow(obsEndIso, opts)]
  }

  const deps: SmokeDeps = {
    ids: { traceId: 'f'.repeat(32), spanId: SPAN_ID },
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
      if (id !== CARRIER) return []
      obsReads++
      return currentRows()
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
      return carrierTrace(obsEndIso, opts)
    },
    shipSpans: async spans => {
      counts.ships++
      const n = shipCounts[counts.ships - 1] ?? 1
      if (counts.ships === 2 && opts.reExportIsNoOp === true) {
        return { observations: n, traces: 2, failed: 0, log: opts.shipLog ?? [] }
      }
      preUpsertEndIso = obsEndIso
      obsEndIso = new Date(spans[0].end_time_unix_nano / 1e6).toISOString()
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
    const { deps, logs, counts } = makeFake({ obsCost: 0.0195 })
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

  it('waits for the RE-EXPORT to be processed before asserting the upsert', async () => {
    const { deps, logs } = makeFake({ hideUpsertReads: 2 })
    expect(await runSmoke(deps)).toBe(0)
    // It waited on the marker specifically, not merely on "an observation exists".
    expect(logs.some(l => l.includes('waiting for the re-export to be processed'))).toBe(true)
  })

  it('FAILS when the re-export never processes, KEEPING the assertions already computed', async () => {
    // The first export's observation is already present, so a presence-only probe
    // would return instantly and "prove" an upsert that never happened. And since
    // the re-export is the LAST check, throwing here would discard the cost oracle
    // and the bucket echoes on a run that cannot cheaply be repeated.
    const { deps, logs } = makeFake({ reExportIsNoOp: true, timeoutMs: 20_000 })
    expect(await runSmoke(deps)).toBe(1)
    expect(logs.some(l => l.includes('re-export') && l.includes('[FAIL]'))).toBe(true)
    expect(logs.some(l => l.includes('expected') && l.includes('[OK]'))).toBe(true)
    expect(logs.some(l => l.includes('usageDetails') && l.includes('[OK]'))).toBe(true)
    // Not misreported as a value mismatch — the values were fine.
    expect(logs.some(l => l.includes('the values are wrong'))).toBe(false)
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

describe('runSmoke — the two endpoints disagree (the measured live behavior)', () => {
  it('PASSES on the LIVE shape: trace detail empty, observations endpoint priced', async () => {
    // Exactly what the deployed instance returns: `/traces/{id}` reports
    // `observations: []` and `totalCost: 0` indefinitely, while
    // `/observations?traceId=…` returns the priced GENERATION row. A smoke that read
    // presence or cost off the trace detail endpoint polls until its bound fires and
    // blames the ingestion worker for a healthy instance.
    const { deps, logs, counts } = makeFake({ detailView: 'silent' })
    expect(await runSmoke(deps)).toBe(0)
    expect(counts.sleeps).toBe(0) // never waited on something it could not see
    expect(logs.some(l => l.includes('expected') && l.includes('[OK]'))).toBe(true)
    expect(logs.some(l => l.includes('re-export') && l.includes('[OK]'))).toBe(true)
    // ...and the divergence is NAMED rather than silently trusted away.
    expect(logs.some(l => l.includes('[ANOMALY]') && l.includes('viewsDisagree'))).toBe(true)
    expect(logs.some(l => l.includes('observations endpoint is authoritative'))).toBe(true)
    expect(logs.some(l => l.startsWith('PASS (with 1 view disagreement'))).toBe(true)
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

  it('keeps the re-export marker above the endTime comparison granularity', () => {
    // The marker comparison truncates to whole seconds; a gap below that granularity
    // makes the upsert check unfalsifiable again, silently. Asserted against the
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

describe('parseObservationList — the envelope is never coerced to "no rows"', () => {
  it('returns the data array', () => {
    expect(parseObservationList({ data: [{ id: 'a' }], meta: {} }, 'T')).toEqual([{ id: 'a' }])
    expect(parseObservationList({ data: [], meta: {} }, 'T')).toEqual([])
  })

  it('RAISES on an envelope it does not recognise, instead of reporting emptiness', () => {
    // An empty read is waited out as "never landed" and blames the ingestion worker;
    // a raise names the real fault (the API changed shape).
    expect(() => parseObservationList({ observations: [] }, 'T')).toThrow(/unrecognised envelope/)
    expect(() => parseObservationList([], 'T')).toThrow(/unrecognised envelope/)
    expect(() => parseObservationList(null, 'T')).toThrow(/unrecognised envelope/)
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
