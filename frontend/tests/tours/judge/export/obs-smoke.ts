// CLI: qa-obs-smoke — live-verify the usage generation observation end-to-end.
//
//   LANGFUSE_HOST=... LANGFUSE_PUBLIC_KEY=... LANGFUSE_SECRET_KEY=... \
//     bun run tests/tours/judge/export/obs-smoke.ts
//
// Unit tests can only prove we SEND the documented payload. This proves the
// deployed server ACCEPTS and PRICES it: it ships one synthetic span through the
// real `exportSpans` path, reads the generation observation back, and asserts the
// buckets echoed exactly, a non-zero cost matching a hand-computed figure, a
// span-derived startTime, and — across a re-export — exactly one observation, at the
// same id and the same cost.
//
// A RE-EXPORT DOES NOT UPDATE. Measured against the deployed instance: shipping the
// same span twice has both `generation-create` events ACCEPTED, but the second does
// not modify the existing row (its endTime stays the first export's). The guarantee
// is therefore NON-DUPLICATION, not upsert — which is the property that actually
// protects the cost figure, and the only one this script asserts. Forcing real update
// semantics would mean emitting a different event type, and is not this script's job.
//
// The span ids are minted ONCE PER INVOCATION rather than left random or pinned
// globally. Stable WITHIN the run so the second export lands on the same id;
// unique ACROSS runs so a previous run's residue can never satisfy an assertion —
// a globally-fixed id would let a completely broken generation path pass by
// reading back the trace some earlier run created.
//
// INGESTION IS ASYNCHRONOUS: /api/public/ingestion only ENQUEUES events for a
// worker, so an immediate read can 404 or return a trace whose observation has not
// landed yet. EVERY read therefore polls for PRESENCE under a finite bound, and
// only then asserts VALUES — once. Polling on the values instead would burn the
// whole timeout on a genuine mismatch and report the wrong diagnosis: "never
// landed" and "landed wrong" are different bugs, and this script names which.
//
// TWO READ ENDPOINTS, DELIBERATELY. `/api/public/observations?traceId=…` is the
// AUTHORITATIVE view of the generation row: it returns `usageDetails`, `costDetails`
// and `calculatedTotalCost` directly, so the cost oracle is asserted against the
// observation's OWN price rather than a trace-level aggregate. The trace detail
// endpoint `/api/public/traces/{id}` is measured to omit observations for some rows
// while the observations endpoint returns them, so it is used ONLY for what it does
// report faithfully: the trace body's own `metadata` (`usage_attributed`). The two
// views are cross-checked and a disagreement is named as its OWN outcome — a run
// that trusted one view silently is how a healthy instance read as a hang.
//
// FAILURE TAXONOMY (kept distinguishable, never folded together):
//   never shipped  — the exporter rejected/skipped the generation at POST time.
//   never landed   — shipped, but the row never became visible within the bound.
//   landed wrong   — visible, but a bucket / cost / id assertion mismatched.
//   read stalled   — a single read never came back; the overall bound fired.
//   views disagree — the two endpoints describe the same trace differently.
//   duplicated     — the re-export added a row instead of leaving the first alone.
//
// Requires WRITE access to the Langfuse instance. Exits non-zero on any failed
// assertion.

import * as crypto from 'crypto'
import * as fs from 'fs'
import * as os from 'os'
import * as path from 'path'
import { buildGenAiSpan, type GenAiSpan } from '../adapter/span'
import type { Scenario } from '../label-trace'
import {
  ApiError,
  api,
  isAbortError,
  configFromEnv,
  exportSpans,
  parseSpanFile,
  usageTraceId,
  type ExportOptions,
} from './langfuse'

const BEHAVIOR_ID = 'SMOKE-OBS'
const MODEL = 'gpt-5.4-mini'

// Fixed counts: every bucket non-zero, and the arithmetic checkable by hand.
const INPUT_TOKENS = 20_000
const CACHED_INPUT_TOKENS = 16_000
const OUTPUT_TOKENS = 1_000
const REASONING_OUTPUT_TOKENS = 800
const CACHE_WRITE_INPUT_TOKENS = 500

const EXPECT_INPUT = INPUT_TOKENS - CACHED_INPUT_TOKENS
// A PINNED constant from the verified price table, not a figure re-derived from the
// instance: deriving the applicable price requires the model-matching rules, which
// are not this script's subject. A mismatch is reported informatively — it may be a
// price move rather than a code defect.
const PRICE_INPUT_PER_TOKEN = 0.75e-6
const PRICE_CACHED_PER_TOKEN = 0.075e-6
const PRICE_OUTPUT_PER_TOKEN = 4.5e-6
const EXPECTED_COST =
  EXPECT_INPUT * PRICE_INPUT_PER_TOKEN +
  CACHED_INPUT_TOKENS * PRICE_CACHED_PER_TOKEN +
  OUTPUT_TOKENS * PRICE_OUTPUT_PER_TOKEN
const COST_TOLERANCE = 1e-6

// Generous enough for a real worker queue under load, finite so a wedged instance
// fails loudly rather than hanging.
const INGEST_TIMEOUT_MS = 120_000
const INGEST_POLL_MS = 2_000
// Per-READ bound. Well under the overall ingestion bound, so a stalled single read
// costs one poll interval rather than the whole budget.
const TRACE_READ_TIMEOUT_MS = 30_000

// The smoke's traces are synthetic and its verdicts are all `pass`, so the ONLY way
// they could reach the standing triage queue is the pass-salt sample. Zero salt keeps
// SMOKE-OBS junk out of the queue a human uses for real labeling. Scoped to THIS
// script — a real round passes its own options, so the enqueue path is untouched
// (a mutation/fail/unsure trace still enqueues there).
export const SMOKE_EXPORT_OPTIONS: ExportOptions = { saltPasses: 0 }

// The span instant is NEAR-PRESENT, derived from the injected clock rather than a
// literal date: a real judge span is timestamped ~now, and an instant months in the
// past is not the shape production ships — a time-windowed read can serve the row on
// one endpoint and omit it on another, which is exactly the divergence that made a
// healthy instance look like a wedged one. Lagged by a FIXED offset so the startTime
// assertion still discriminates: an observation stamped with the export clock is
// minutes away from the span's own start and fails, while both sit in the same
// present-day window. Determinism is preserved because the clock is a seam
// (`deps.now`) — tests pin it, live runs pass real time.
export const SPAN_START_LAG_MS = 5 * 60_000
export const SPAN_DURATION_MS = 3_200
// The re-export ships the same observation id with a DIFFERENT end instant. It was
// once a marker for "the upsert was processed"; a live run measured that a second
// `generation-create` on an existing id does NOT update the row, so there is no
// update to wait for and that use is gone. The differing instant is KEPT, repurposed:
// it makes the re-export's row DISTINGUISHABLE from the first export's, so a
// duplicate is unmistakable (two rows, two different endTimes) rather than
// ambiguous, and a future platform that starts honouring updates announces itself
// instead of passing silently. The comparison truncates to WHOLE SECONDS to tolerate
// server-side millisecond formatting, so the gap MUST exceed one second or the two
// exports stop being tellable apart — derived from that granularity rather than
// hand-picked, and asserted by MARKER_GAP_MS's own test.
export const MARKER_TRUNCATION_MS = 1_000
export const MARKER_GAP_MS = MARKER_TRUNCATION_MS * 4
// How long the re-export's effect is DWELLED on, at MINIMUM. The property under test
// is a NEGATIVE — no second row appears — and a negative cannot be polled for: there
// is no state change to wait on, so a single read taken immediately after the ship
// would "prove" non-duplication by racing the ingestion worker.
export const RE_EXPORT_SETTLE_MS = 20_000
// A FIXED floor is not enough on its own: under queue load the second event may not
// be processed until well past it, and a dwell that ended first would report PASS
// over a duplicate that had not landed yet. The window therefore SCALES with the
// latency this instance just demonstrated — how long the FIRST export took to become
// visible, which is the same queue doing the same work — and is capped by the
// ingestion bound so it stays finite on a wedged instance.
export const DWELL_LATENCY_MULTIPLE = 3

export function reExportDwellMs(firstVisibleMs: number, ingestBoundMs: number): number {
  const scaled = Math.max(0, firstVisibleMs) * DWELL_LATENCY_MULTIPLE
  return Math.min(ingestBoundMs, Math.max(RE_EXPORT_SETTLE_MS, scaled))
}

export interface SpanTimes {
  startMs: number
  endMs: number
  reExportEndMs: number
}

// Every instant the smoke ships, from ONE clock reading. Exported so the marker-gap
// guard test asserts the relationship itself rather than a snapshot of it.
export function spanTimes(nowMs: number): SpanTimes {
  const startMs = nowMs - SPAN_START_LAG_MS
  const endMs = startMs + SPAN_DURATION_MS
  return { startMs, endMs, reExportEndMs: endMs + MARKER_GAP_MS }
}

const scenario: Scenario = {
  kind: 'behavior',
  behaviorId: BEHAVIOR_ID,
  behaviorTitle: 'synthetic usage smoke',
  given: 'a synthetic span',
  when: 'the exporter ships it',
  // Two items with a GAP, so the carrier is provably the lowest index rather than
  // "the first one" — and the sibling proves the usage_attributed marking live.
  items: [
    { itemIndex: 0, thenText: 'the observation lands on this trace' },
    { itemIndex: 2, thenText: 'this sibling carries no usage' },
  ],
  allThen: ['the observation lands on this trace', 'this sibling carries no usage'],
}

interface Assertion {
  label: string
  ok: boolean
  detail: string
}

// Neither an assertion the smoke can stand behind nor one it can honestly fail: a
// state the operator must SEE, reported under its own name so it is never quietly
// absorbed into a pass or misfiled as one of the four failure classes.
interface Anomaly {
  label: string
  detail: string
}

// One page of the observations endpoint. `totalItems` is carried alongside the rows
// but is NEVER used as a row count: it was measured to over-report (2 for a single
// row — it appears to count accepted ingestion EVENTS, not surviving rows). That makes
// it useless as a count and useful as something else entirely: evidence that the
// second event was PROCESSED, which is otherwise unobservable now that a re-export
// leaves no trace on the row it targets.
export interface ObservationPage {
  rows: Array<Record<string, unknown>>
  totalItems?: number
}

type Trace = Record<string, unknown>

// The expected state never became visible within the bound. Deliberately distinct
// from a value mismatch: an operator seeing this knows the write or the ingestion
// worker is the suspect, not the payload.
export class IngestionTimeoutError extends Error {
  constructor(what: string, waitedMs: number) {
    super(`${what} never became visible within ${Math.round(waitedMs / 1000)}s`)
    this.name = 'IngestionTimeoutError'
  }
}

export interface ShipResult {
  observations: number
  traces: number
  failed: number
  log: string[]
}

// The smoke ships ONE span with a two-item scenario, so a complete export is exactly
// two traces, one observation, and zero failures. Anything else is a defect the smoke
// must report rather than poll past.
const EXPECT_TRACES_PER_EXPORT = 2

export interface SmokeDeps {
  // Read a trace back. `undefined` = not visible YET (a 404 while the worker is
  // still draining the queue), which is a poll-again signal, not a failure.
  // Reports the trace BODY faithfully (metadata); its `observations` array is NOT
  // relied upon — see `getObservations`.
  getTrace: (traceId: string) => Promise<Trace | undefined>
  // Read the observation rows for a trace from the observations endpoint — the
  // authoritative view, carrying usageDetails / costDetails / calculatedTotalCost.
  // `undefined` = the read could not be served yet (poll again); an EMPTY `rows` is a
  // real answer ("no rows for this trace"), distinct from "not visible yet".
  getObservations: (traceId: string) => Promise<ObservationPage | undefined>
  // Ships the spans and returns the WHOLE outcome — counts AND the exporter's own
  // log lines. Every discarded field is a failure the smoke can print PASS through:
  // a shipped observation says nothing about whether its sibling trace landed, and
  // on a one-shot live gate the rejection reason is the most valuable line there is.
  shipSpans: (spans: GenAiSpan[]) => Promise<ShipResult>
  log: (msg: string) => void
  sleep?: (ms: number) => Promise<void>
  now?: () => number
  timeoutMs?: number
  intervalMs?: number
  // Bound on the re-export dwell window (see RE_EXPORT_SETTLE_MS).
  settleMs?: number
  // Per-READ bound, applied uniformly wherever a read is raced against a deadline.
  readTimeoutMs?: number
  // Pinned by tests; a live run mints them per invocation.
  ids?: { traceId: string; spanId: string }
}

const num = (v: unknown): number | undefined => (typeof v === 'number' ? v : undefined)

// Is `key` PRESENT on the object (regardless of value)? Presence of a key the final
// body supplies is the honest "this row has settled" signal; comparing its VALUE here
// would turn a poll into a value-wait, which is exactly what must not happen.
// How a failed trace READ is treated. `retry` means "indistinguishable from not yet
// visible", so the poller waits and the OVERALL bound produces the one
// human-readable diagnosis: a 404 (the row has not appeared) and an abort (this read
// did not come back in time) are both that. Anything else is a real transport
// failure and must surface rather than be waited out.
export function readErrorDisposition(err: unknown): 'retry' | 'fatal' {
  if (err instanceof ApiError && err.status === 404) return 'retry'
  if (isAbortError(err)) return 'retry'
  return 'fatal'
}

// How a failed COLLECTION read is treated — deliberately NOT the same rule. A list
// query for a trace with no rows answers 200 with an empty `data`, so a 404 here
// cannot mean "the row has not appeared": it means the ROUTE is not there (an older
// instance, a proxy that does not forward it). Retrying that spends the entire bound
// and then blames the ingestion worker for an API incompatibility — the wrong
// diagnosis, loudly stated. An abort is still "this read did not come back in time".
export function listReadErrorDisposition(err: unknown): 'retry' | 'fatal' {
  if (isAbortError(err)) return 'retry'
  return 'fatal'
}

const hasKey = (v: unknown, key: string): boolean =>
  v !== null && typeof v === 'object' && key in (v as Record<string, unknown>)

const observationsOf = (trace: Trace | undefined): Array<Record<string, unknown>> =>
  Array.isArray(trace?.observations) ? (trace.observations as Array<Record<string, unknown>>) : []

// The observation's OWN cost, as the observations endpoint reports it: the per-bucket
// sum it priced (`costDetails.total`), falling back to the flattened
// `calculatedTotalCost`. Asserting this rather than the trace-level aggregate is what
// proves per-bucket pricing — the aggregate can only say the trace cost SOMETHING.
export function observationCost(obs: Record<string, unknown>): number | undefined {
  const details = obs.costDetails
  const total =
    details !== null && typeof details === 'object'
      ? num((details as Record<string, unknown>).total)
      : undefined
  return total ?? num(obs.calculatedTotalCost)
}

// The observations endpoint returns a paged envelope `{ data: [...], meta: {...} }`.
// An envelope this does not recognise is raised, never coerced to "no rows": a
// silently-empty read is indistinguishable from a genuine absence and would be waited
// out under the "never landed" diagnosis, blaming the ingestion worker for a shape
// change in the API.
export function parseObservationPage(body: unknown, traceId: string): ObservationPage {
  const envelope =
    body !== null && typeof body === 'object' ? (body as Record<string, unknown>) : {}
  const data = envelope.data
  if (!Array.isArray(data)) {
    throw new Error(
      `observations read for ${traceId}: unrecognised envelope (no \`data\` array) — ` +
        'the observations endpoint changed shape; this is neither a payload nor a worker fault'
    )
  }
  const meta = envelope.meta
  const totalItems =
    meta !== null && typeof meta === 'object'
      ? num((meta as Record<string, unknown>).totalItems)
      : undefined
  return {
    rows: data.filter((o): o is Record<string, unknown> => o !== null && typeof o === 'object'),
    totalItems,
  }
}

// Resolves to STALLED if `p` has not settled within `ms`. The probe itself must be
// raced against the deadline: a read that never returns (rather than erroring) would
// otherwise park on `await` forever and the advertised bound would never be reached.
const STALLED = Symbol('stalled')
function withDeadline<T>(p: Promise<T>, ms: number): Promise<T | typeof STALLED> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => resolve(STALLED), ms)
    p.then(
      v => {
        clearTimeout(timer)
        resolve(v)
      },
      e => {
        clearTimeout(timer)
        reject(e instanceof Error ? e : new Error(String(e)))
      }
    )
  })
}

// Poll `probe` until it returns a value, under a finite bound. Polls for PRESENCE
// only — never for a value — so a genuine mismatch fails immediately at the
// assertions instead of spinning out the whole timeout under the wrong diagnosis.
export async function waitForPresence<T>(
  what: string,
  probe: () => Promise<T | undefined>,
  deps: Pick<SmokeDeps, 'log' | 'sleep' | 'now' | 'timeoutMs' | 'intervalMs'>
): Promise<T> {
  const now = deps.now ?? Date.now
  const sleep = deps.sleep ?? ((ms: number): Promise<void> => new Promise(r => setTimeout(r, ms)))
  const timeoutMs = deps.timeoutMs ?? INGEST_TIMEOUT_MS
  const intervalMs = deps.intervalMs ?? INGEST_POLL_MS
  const started = now()
  let attempts = 0
  for (;;) {
    attempts++
    const remaining = Math.max(1, timeoutMs - (now() - started))
    const raced = await withDeadline(probe(), remaining)
    if (raced === STALLED) throw new IngestionTimeoutError(`${what} (read stalled)`, timeoutMs)
    const got = raced
    if (got !== undefined) {
      if (attempts > 1) deps.log(`  ${what}: visible after ${attempts} attempt(s)`)
      return got
    }
    const waited = now() - started
    if (waited + intervalMs > timeoutMs) throw new IngestionTimeoutError(what, waited)
    // Progress, so a slow-but-working instance does not read as a hang.
    deps.log(`  waiting for ${what}… (${Math.round(waited / 1000)}s elapsed)`)
    await sleep(intervalMs)
  }
}

export interface DwellResult {
  // The last successfully-read set of rows.
  rows: Array<Record<string, unknown>>
  // `totalItems` from the last successful read, if the endpoint reported one.
  totalItems?: number
  samples: number
  // The MOST rows any single sample saw. A duplicate that appears and is later
  // compacted away still failed the property while it existed, so the worst sample —
  // not the last one — is what the assertion reads.
  worstCount: number
  // The FIRST violation any sample showed, if any. Same reasoning as `worstCount`,
  // generalised: a row that was briefly reissued under another id, or briefly
  // repriced, broke the invariant while it was broken — reading only the final sample
  // would call that clean because the state happened to settle back.
  violation?: string
}

// Sample `traceId`'s observation rows repeatedly across a bounded window. Used where
// the property under test is a NEGATIVE (no duplicate row appears): there is no state
// transition to poll for, so the only honest evidence is repeated observation over
// time. A read that cannot be served is skipped rather than failing the dwell — the
// dwell asserts about rows that DID appear, and absence is the expected state.
export async function dwellOnObservations(
  traceId: string,
  deps: Pick<
    SmokeDeps,
    'getObservations' | 'log' | 'sleep' | 'now' | 'intervalMs' | 'settleMs' | 'readTimeoutMs'
  > & {
    // Ends the dwell early. Used for the phase that runs ALONGSIDE the re-export
    // request: that phase exists only to cover the in-flight window, and holding the
    // full settle time there would delay the window that must follow the write.
    stopWhen?: () => boolean
    // Judges a single sample. Returns a short description when the sample violates
    // the property under test, or undefined when it is fine.
    violates?: (rows: Array<Record<string, unknown>>) => string | undefined
    // Raise if NO read was served. Set for the authoritative post-export window, whose
    // emptiness would otherwise be reported as a value mismatch; left off for the
    // opportunistic in-flight phase, where unserved reads are not a failure.
    requireServed?: boolean
  }
): Promise<DwellResult> {
  const now = deps.now ?? Date.now
  const sleep = deps.sleep ?? ((ms: number): Promise<void> => new Promise(r => setTimeout(r, ms)))
  const settleMs = deps.settleMs ?? RE_EXPORT_SETTLE_MS
  const intervalMs = deps.intervalMs ?? INGEST_POLL_MS
  const started = now()
  let rows: Array<Record<string, unknown>> = []
  let totalItems: number | undefined
  let worstCount = 0
  let violation: string | undefined
  let samples = 0
  // Reads that actually returned a page. A dwell whose reads were ALL unserved has
  // observed nothing at all — which is not the same as having observed no duplicate.
  let served = 0
  // Whether the MOST RECENT read was served. Coverage has to reach the boundary: one
  // stalled read can consume the rest of the window (the per-read allowance exceeds
  // the default one), so a dwell that ends blind saw nothing of its own tail — the
  // exact stretch a late duplicate would land in.
  let lastServed = false
  for (;;) {
    samples++
    // Each read gets the SAME bound every other read in this script gets. Shrinking it
    // to whatever is left of the dwell would report a healthy-but-slow read at the
    // final sample as a stalled ingestion — a read's allowance has nothing to do with
    // how much of the observation window remains.
    const raced = await withDeadline(
      deps.getObservations(traceId),
      deps.readTimeoutMs ?? TRACE_READ_TIMEOUT_MS
    )
    if (raced === STALLED) {
      throw new IngestionTimeoutError(`the re-export dwell on ${traceId} (read stalled)`, settleMs)
    }
    lastServed = raced !== undefined
    if (raced !== undefined) {
      served++
      rows = raced.rows
      totalItems = raced.totalItems ?? totalItems
      worstCount = Math.max(worstCount, raced.rows.length)
      violation = violation ?? deps.violates?.(raced.rows)
    }
    // Sample AT the boundary, not one interval short of it. Reads take real time, so
    // stopping as soon as a whole interval no longer fits leaves the tail of the
    // advertised window unobserved — and a duplicate landing in that tail would be
    // reported clean by a check whose entire purpose is to catch it.
    const remaining = settleMs - (now() - started)
    if (remaining <= 0 || deps.stopWhen?.() === true) {
      // NOT a value mismatch. If nothing was ever served, the honest report is that the
      // reads did not come back — returning the empty default here would surface as
      // "0 observation(s)" under the "ingestion landed; the values are wrong"
      // diagnosis, blaming the payload for what is a read failure. A single stalled
      // read is enough to produce this: the per-read allowance deliberately exceeds
      // the default window.
      if (deps.requireServed === true && (served === 0 || !lastServed)) {
        throw new IngestionTimeoutError(
          `the re-export dwell on ${traceId} (` +
            (served === 0 ? 'no read was served' : 'the last read was not served') +
            ')',
          settleMs
        )
      }
      return { rows, totalItems, samples, worstCount, violation }
    }
    deps.log(
      `  watching ${traceId} for a duplicate… (${Math.round((settleMs - remaining) / 1000)}s of ` +
        `${Math.round(settleMs / 1000)}s)`
    )
    // Never overshoot the window: the last wait is only as long as the window has left.
    await sleep(Math.min(intervalMs, remaining))
  }
}

export async function runSmoke(deps: SmokeDeps): Promise<number> {
  const log = deps.log
  // One id pair for this invocation: stable across both exports below, unique across runs.
  const runTraceId = deps.ids?.traceId ?? crypto.randomBytes(16).toString('hex')
  const runSpanId = deps.ids?.spanId ?? crypto.randomBytes(8).toString('hex')
  // ONE clock reading for every instant this run ships, so the two exports agree on
  // the span's start and differ only in the marker.
  const times = spanTimes((deps.now ?? Date.now)())

  // `endMs` is what makes the two exports tellable apart: the second ships the same
  // observation id with a DIFFERENT endTime, so a duplicate row is unmistakable and a
  // row that was actually rewritten announces itself. Shipping the identical body
  // twice would leave both indistinguishable from a single ingestion.
  const makeSpan = (endMs: number): GenAiSpan => ({
    ...buildGenAiSpan({
      impl: 'obs-smoke',
      behaviorId: BEHAVIOR_ID,
      model: MODEL,
      startMs: times.startMs,
      endMs,
      inputTokens: INPUT_TOKENS,
      cachedInputTokens: CACHED_INPUT_TOKENS,
      cacheWriteInputTokens: CACHE_WRITE_INPUT_TOKENS,
      outputTokens: OUTPUT_TOKENS,
      reasoningOutputTokens: REASONING_OUTPUT_TOKENS,
      prompt: 'synthetic usage smoke — no real content',
      response: '[]',
      scenario,
      itemVerdicts: [
        { itemIndex: 0, verdict: 'pass', citation: 'synthetic', critique: 'synthetic' },
        { itemIndex: 2, verdict: 'pass', citation: 'synthetic', critique: 'synthetic' },
      ],
    }),
    trace_id: runTraceId,
    span_id: runSpanId,
  })

  const span = makeSpan(times.endMs)
  const carrierId = usageTraceId(span)
  const siblingId = carrierId.replace(/-item0$/, '-item2')
  const spanStartIso = new Date(span.start_time_unix_nano / 1e6).toISOString()

  log(`qa-obs-smoke: shipping synthetic span ${BEHAVIOR_ID} (model ${MODEL})`)
  log(`  trace  ${carrierId}   (sibling: ${siblingId})`)
  log(`  obs    obs-judge-${BEHAVIOR_ID}-${runSpanId}-gen`)
  log('')

  // The exporter's own count is checked after EVERY export, not just the first: a
  // generation rejected at POST time means no observation is COMING, so polling for
  // one would burn the whole bound and then blame the ingestion worker for an
  // export-side rejection. The exporter's log lines are printed with it — that
  // rejection reason is the most valuable thing on a one-shot live gate, and there
  // is nothing else the operator could "re-run with" to obtain it.
  // Every field of the export result is checked, not just the observation count: a
  // failed sibling trace alongside an accepted observation would otherwise satisfy
  // every later probe and print PASS over a failure the exporter had reported.
  const exportProblem = (result: ShipResult, which: string): string | undefined => {
    if (result.failed > 0) return `the ${which} reported ${result.failed} trace ship failure(s)`
    if (result.traces !== EXPECT_TRACES_PER_EXPORT) {
      return `the ${which} shipped ${result.traces} trace(s), expected ${EXPECT_TRACES_PER_EXPORT}`
    }
    if (result.observations !== 1) {
      return (
        `the ${which} shipped ${result.observations} observation(s), expected 1 — the ` +
        'generation was rejected or skipped BEFORE ingestion; this is not a worker race'
      )
    }
    return undefined
  }

  const reportExportProblem = (problem: string, result: ShipResult): void => {
    log('')
    log(`FAIL — ${problem}.`)
    if (result.log.length > 0) {
      log('  exporter log:')
      for (const line of result.log) log(`    ${line.trim()}`)
    }
  }

  // Timed from BEFORE the ship. `exportSpans` posts the generation and then keeps
  // working (sibling trace, queue steps), so the observation can become visible while
  // the export call is still returning — starting the clock afterwards would measure
  // near-zero latency on an instance that is in fact slow, and size the re-export's
  // dwell window off that. Starting early can only OVERSTATE the latency, which
  // widens the window; understating it is what would let a duplicate through.
  const shippedAt = (deps.now ?? Date.now)()
  const first = await deps.shipSpans([span])
  const firstProblem = exportProblem(first, 'export')
  if (firstProblem !== undefined) {
    reportExportProblem(firstProblem, first)
    return 1
  }

  // The observation is waited for on the AUTHORITATIVE endpoint. Waiting for it on
  // the trace detail endpoint is what hung the first live run: the row was present,
  // priced, and aggregated into the trace list, yet the detail endpoint reported
  // `observations: []` indefinitely, so a presence probe there can never settle.
  const obsPage = await waitForPresence(
    `the observation on ${carrierId}`,
    async () => {
      const page = await deps.getObservations(carrierId)
      return page !== undefined && page.rows.length > 0 ? page : undefined
    },
    deps
  )
  // What this instance's queue just demonstrated, used below to size the re-export
  // dwell. Measured rather than assumed: a loaded worker gets a longer window.
  const firstVisibleMs = (deps.now ?? Date.now)() - shippedAt

  // Presence must include the FINAL body's marks, not merely the row: `exportSpans`
  // sends a minimal init trace-create first, so a trace can be readable before its
  // metadata exists. Asserting `usage_attributed` off that intermediate state is a
  // FALSE MISMATCH during ordinary worker lag — the mirror of a false pass, and it
  // burns the operator's one live run just as effectively.
  const carrier = await waitForPresence(
    `the final body on ${carrierId}`,
    async () => {
      const t = await deps.getTrace(carrierId)
      return hasKey(t?.metadata, 'usage_attributed') ? t : undefined
    },
    deps
  )

  const obsRows = obsPage.rows
  // Captured BEFORE the re-export, so the count's movement afterwards is evidence the
  // second event was processed.
  const totalItemsBefore = obsPage.totalItems
  const obs = obsRows[0]
  const usage = (obs.usageDetails ?? {}) as Record<string, unknown>
  const meta = (obs.metadata ?? {}) as Record<string, unknown>
  // Kept distinct from a reported zero: "the row carries no price at all" and "the
  // row is priced at nothing" are different defects, and collapsing them costs the
  // operator the one line that says which.
  const reportedCost = observationCost(obs)
  const obsCost = reportedCost ?? 0
  const startTime = typeof obs.startTime === 'string' ? obs.startTime : ''

  const results: Assertion[] = [
    {
      label: 'observations',
      ok: obsRows.length === 1 && first.observations === 1,
      detail: `${obsRows.length} for the trace, exporter counted ${first.observations}`,
    },
    {
      label: 'usageDetails',
      ok:
        num(usage.input) === EXPECT_INPUT &&
        num(usage.input_cached_tokens) === CACHED_INPUT_TOKENS &&
        num(usage.output) === OUTPUT_TOKENS,
      detail: `input=${String(usage.input)} input_cached_tokens=${String(usage.input_cached_tokens)} output=${String(usage.output)}`,
    },
    {
      label: 'metadata',
      ok:
        num(meta.reasoning_output_tokens) === REASONING_OUTPUT_TOKENS &&
        num(meta.cache_write_input_tokens) === CACHE_WRITE_INPUT_TOKENS,
      detail: `reasoning_output_tokens=${String(meta.reasoning_output_tokens)} cache_write_input_tokens=${String(meta.cache_write_input_tokens)}`,
    },
    {
      label: 'observationCost',
      ok: reportedCost !== undefined && reportedCost > 0,
      detail:
        reportedCost === undefined
          ? 'NOT REPORTED — the row carries neither costDetails.total nor calculatedTotalCost'
          : `${obsCost.toFixed(6)} (> 0)`,
    },
    {
      label: 'expected',
      ok: Math.abs(obsCost - EXPECTED_COST) < COST_TOLERANCE,
      detail:
        `${EXPECTED_COST.toFixed(6)} vs observed ${obsCost.toFixed(6)} ` +
        `(|delta| < ${COST_TOLERANCE})\n` +
        `                         ${EXPECT_INPUT}*0.75/M + ${CACHED_INPUT_TOKENS}*0.075/M + ${OUTPUT_TOKENS}*4.50/M`,
    },
    {
      label: 'startTime',
      ok: startTime.startsWith(spanStartIso.slice(0, 19)),
      detail: `${startTime} (span start ${spanStartIso}, not export time)`,
    },
  ]

  // CROSS-VIEW RECONCILIATION. Both endpoints describe the same trace, so their
  // disagreement is information — and it is NOT one of the four failure classes
  // above, which is why it is reported under its own name instead of being folded
  // into "landed wrong". Two shapes, treated differently and deliberately:
  //
  //   the detail view is SILENT (no observations, zero aggregate) while the
  //   authoritative view has the row — the measured behavior of the live instance.
  //   Reported as an anomaly, not a failure: the observation demonstrably exists and
  //   is priced, so failing here would fail a healthy instance.
  //
  //   the detail view is POPULATED and DISAGREES — a real contradiction about a row
  //   both endpoints can see, and an assertion the smoke must fail on.
  const anomalies: Anomaly[] = []
  const traceViewRows = observationsOf(carrier).length
  const traceViewCost = num(carrier.totalCost) ?? 0
  if (traceViewRows === 0 && traceViewCost === 0) {
    anomalies.push({
      label: 'viewsDisagree',
      detail:
        `the trace detail endpoint reports no observations and zero cost for ${carrierId}, ` +
        `while the observations endpoint returns ${obsRows.length} row(s) costing ` +
        `${obsCost.toFixed(6)} — the observations endpoint is authoritative and the ` +
        'assertions below are taken from it',
    })
  } else {
    results.push({
      label: 'viewsAgree',
      ok: traceViewRows === obsRows.length && Math.abs(traceViewCost - obsCost) < COST_TOLERANCE,
      detail:
        `trace detail: ${traceViewRows} observation(s) costing ${traceViewCost.toFixed(6)}; ` +
        `observations endpoint: ${obsRows.length} costing ${obsCost.toFixed(6)}`,
    })
  }

  // The sibling trace must say, in metadata, that it carries none of the usage. Same
  // export, but a SEPARATE row with its own ingestion visibility.
  const sibling = await waitForPresence(
    `the sibling trace ${siblingId} (final body)`,
    async () => {
      const t = await deps.getTrace(siblingId)
      return hasKey(t?.metadata, 'usage_attributed') ? t : undefined
    },
    deps
  )
  const carrierMeta = (carrier.metadata ?? {}) as Record<string, unknown>
  const siblingMeta = (sibling.metadata ?? {}) as Record<string, unknown>
  results.push({
    label: 'usage_attributed',
    ok: carrierMeta.usage_attributed === true && siblingMeta.usage_attributed === false,
    detail: `item0=${String(carrierMeta.usage_attributed)} item2=${String(siblingMeta.usage_attributed)}`,
  })

  // RE-EXPORT: the property is NON-DUPLICATION, not update. Measured against the live
  // instance: a second `generation-create` carrying an existing observation id is
  // accepted and then does NOT modify the row — the endTime stays the first export's.
  // So the assertion is the one that is both TRUE and load-bearing: after shipping the
  // same span twice, the carrier still has exactly ONE observation, with the SAME id,
  // at the SAME cost. That is what protects the spend figure from double-counting,
  // which is the whole reason the check exists. Asserting an update instead would fail
  // a healthy instance forever, which is exactly what a live run did.
  //
  // A duplicate cannot be waited FOR (it is a negative), so its absence is DWELLED on
  // across a bounded window — in TWO phases, because neither alone is sufficient.
  //
  //   DURING the export call. `exportSpans` posts the generation and then keeps
  //   working, so ingestion can expose — and, if it ever compacts, withdraw — a
  //   duplicate while that call is still returning. A dwell that only began afterwards
  //   would be blind exactly where the write it watches for happens. This phase ends
  //   when the call does; it is coverage, not the window.
  //
  //   AFTER it returns, a FULL window. The generation is posted LATE in the export, so
  //   a slow export could consume the entire settle time before the write under test
  //   was even sent — and a single window spanning both would then be over before
  //   there was anything to observe.
  const secondEndIso = new Date(times.reExportEndMs).toISOString()
  let timedOutOnReExport = false
  const settleMs =
    deps.settleMs ?? reExportDwellMs(firstVisibleMs, deps.timeoutMs ?? INGEST_TIMEOUT_MS)
  let shipSettled = false
  let watchFailure: unknown
  // THE property, evaluated on every sample of both phases rather than on the final
  // read alone: one row, the same id, the same price. A state that briefly broke and
  // settled back still broke, and a check that only looked at the end would call the
  // run clean because the instance happened to recover before the last read.
  const reExportViolation = (rows: Array<Record<string, unknown>>): string | undefined => {
    if (rows.length !== 1) return `${rows.length} observation(s)`
    const id = rows[0]?.id
    if (id !== obs.id) return `id ${String(id)} (was ${String(obs.id)})`
    const cost = observationCost(rows[0] ?? {})
    if (cost === undefined) return 'cost NOT REPORTED'
    if (Math.abs(cost - obsCost) >= COST_TOLERANCE) {
      return `cost ${cost.toFixed(6)} (was ${obsCost.toFixed(6)})`
    }
    return undefined
  }
  // The rejection handler is attached HERE, at creation. Deferring it to the await
  // below leaves a window in which a failing read is an unhandled rejection — which
  // fails the process outright, with no diagnosis, even though the code downstream
  // would have reported it properly.
  const preWatch = dwellOnObservations(carrierId, {
    ...deps,
    // Bounded by the INGESTION bound rather than the settle window: this phase covers
    // the export call, which easily outlasts a 20s window. It is deliberately not
    // unbounded — a gate must not contain a loop whose only exit is another promise —
    // so an export outlasting even the ingestion bound leaves its remainder unwatched.
    // Accepted, and stated: such an export is already pathological, and the run's own
    // ingestion bound is the coarser problem at that point.
    settleMs: deps.timeoutMs ?? INGEST_TIMEOUT_MS,
    stopWhen: () => shipSettled,
    violates: reExportViolation,
  }).catch((err: unknown): DwellResult => {
    watchFailure = err
    return { rows: [], samples: 0, worstCount: 0 }
  })
  let second: ShipResult
  let pre: DwellResult
  try {
    second = await deps.shipSpans([makeSpan(times.reExportEndMs)])
  } finally {
    // Releases the in-flight watcher AND joins it — even when the export throws, in
    // which case the exception below would otherwise skip the join and leave the
    // watcher issuing reads (and holding a timer) after the run has already failed.
    // `preWatch` never rejects, so awaiting here cannot mask the export's own error.
    shipSettled = true
    pre = await preWatch
  }
  const secondProblem = exportProblem(second, 're-export')
  if (secondProblem !== undefined) {
    // Record it as a failed assertion instead of returning: seven assertions —
    // including the pinned cost oracle — are already computed, and on a run the
    // operator cannot cheaply repeat, discarding them is pure loss.
    reportExportProblem(secondProblem, second)
    results.push({ label: 're-export', ok: false, detail: secondProblem })
  } else {
    try {
      // Read on the authoritative endpoint, for the same reason every other read is.
      // NOTE: the ROWS are counted, never `meta.totalItems` — that count was measured
      // to over-report (2 for a single row, one per accepted ingestion event). An
      // assertion "simplified" to read the count would fail on a healthy instance. It
      // is used ONLY as processing evidence, below.
      // A read that failed during the in-flight phase is surfaced here rather than
      // swallowed: it is classified by the same handler as any other dwell failure.
      if (watchFailure !== undefined) throw watchFailure
      const post = await dwellOnObservations(carrierId, {
        ...deps,
        settleMs,
        requireServed: true,
        violates: reExportViolation,
      })
      // Evidence from BOTH phases decides — a violation seen only while the export was
      // in flight is still a violation.
      const dwell: DwellResult = {
        rows: post.rows,
        totalItems: post.totalItems ?? pre.totalItems,
        samples: pre.samples + post.samples,
        worstCount: Math.max(pre.worstCount, post.worstCount),
        violation: pre.violation ?? post.violation,
      }
      const afterObs = dwell.rows
      const afterCost = observationCost(afterObs[0] ?? {})
      results.push({
        label: 're-export',
        ok: dwell.violation === undefined,
        detail:
          `${dwell.worstCount} observation(s) across ${dwell.samples} sample(s), id ` +
          `${String(afterObs[0]?.id)} (was ${String(obs.id)}), cost ` +
          `${afterCost === undefined ? 'NOT REPORTED' : afterCost.toFixed(6)} (was ${obsCost.toFixed(6)})` +
          (dwell.violation === undefined ? '' : ` — worst sample: ${dwell.violation}`),
      })
      // NON-DUPLICATION IS ONLY MEANINGFUL IF THE SECOND EVENT WAS PROCESSED. The
      // export proves the POST was accepted (enqueued), nothing more; and since a
      // re-export leaves no mark on the row it targets, a stalled worker looks exactly
      // like a correct create-only outcome — every sample sees the original row and the
      // assertion above passes without the write ever having been handled. The one
      // signal that does move is `totalItems`, which counts accepted EVENTS rather than
      // surviving rows: useless as a row count, and the only available evidence of
      // processing. Reported as an ANOMALY, never a failure — the count's semantics
      // rest on a single live measurement, so a healthy instance must not be failed
      // over them; but a run that could not confirm processing must not pass in silence
      // claiming it proved something it did not.
      if (
        totalItemsBefore !== undefined &&
        dwell.totalItems !== undefined &&
        dwell.totalItems <= totalItemsBefore
      ) {
        anomalies.push({
          label: 'reExportUnconfirmed',
          detail:
            `the observations endpoint's totalItems did not move (${totalItemsBefore} → ` +
            `${dwell.totalItems}) after the re-export, so the second event may never have ` +
            'been processed — no duplicate appeared, but that is weak evidence if nothing ' +
            'was written at all; suspect the ingestion worker',
        })
      }
      // The re-export's row carries a different endTime, so if the platform ever DOES
      // start applying updates the smoke says so instead of passing silently on an
      // assumption we have measured to be false today.
      const endTime = afterObs[0]?.endTime
      if (typeof endTime === 'string' && endTime.startsWith(secondEndIso.slice(0, 19))) {
        anomalies.push({
          label: 'reExportUpdated',
          detail:
            `the re-export UPDATED the existing observation (endTime is now ${secondEndIso}). ` +
            'A second generation-create was measured NOT to modify an existing row; if that ' +
            'has changed, the backfill and re-export notes that depend on it need revisiting',
        })
      }
    } catch (err) {
      // A timeout HERE must not discard the assertions already computed above — the
      // re-export is the LAST check, and letting it throw would throw away the cost
      // oracle and the bucket echoes on a run the operator cannot cheaply repeat.
      // Everything earlier is still valid evidence about the first export.
      if (!(err instanceof IngestionTimeoutError)) throw err
      timedOutOnReExport = true
      results.push({ label: 're-export', ok: false, detail: err.message })
    }
  }

  for (const r of results) {
    log(`  ${r.label.padEnd(22)} ${r.ok ? '[OK]' : '[FAIL]'} ${r.detail}`)
  }
  // Named separately from both OK and FAIL: an anomaly is neither an assertion the
  // smoke can stand behind nor one it can honestly fail, and burying it in either
  // column is how the disagreement stayed invisible for a whole live run.
  for (const a of anomalies) log(`  ${a.label.padEnd(22)} [ANOMALY] ${a.detail}`)
  const failed = results.filter(r => !r.ok)
  log('')
  if (failed.length === 0) {
    // Named by LABEL, never described generically: an operator who reads "view
    // disagreement" over a `reExportUpdated` dismisses it as the known endpoint quirk
    // and misses a platform behavior change.
    log(
      anomalies.length === 0
        ? 'PASS'
        : `PASS (with ${anomalies.length} anomaly/anomalies: ${anomalies.map(a => a.label).join(', ')})`
    )
    return 0
  }
  // Everything that could be computed HAS been printed above; this line names which
  // assertions failed. "landed but wrong" stays the diagnosis whenever the export
  // itself was clean — a stalled ingestion raises IngestionTimeoutError instead.
  log(
    `FAIL — ${failed.length} assertion(s) failed: ${failed.map(f => f.label).join(', ')}` +
      (secondProblem === undefined && !timedOutOnReExport
        ? ' (ingestion landed; the values are wrong)'
        : '')
  )
  return 1
}

async function main(): Promise<number> {
  const cfg = configFromEnv()
  if (!cfg) {
    console.error(
      'qa-obs-smoke: LANGFUSE_HOST / LANGFUSE_PUBLIC_KEY / LANGFUSE_SECRET_KEY must be set ' +
        '(and the keys must have WRITE access).'
    )
    return 2
  }
  try {
    return await runSmoke({
      log: msg => console.log(msg),
      getTrace: async id => {
        try {
          // Bounded per read. An abort is NOT fatal: it means this read did not come
          // back in time, which is indistinguishable from "not visible yet" — so it
          // is retried like any other empty read, and the OVERALL bound (which the
          // poller owns) is what finally fails, with the same human-readable
          // diagnosis as every other stalled path.
          return await api(cfg, 'GET', `/api/public/traces/${id}`, undefined, TRACE_READ_TIMEOUT_MS)
        } catch (err) {
          if (readErrorDisposition(err) === 'retry') return undefined
          throw err
        }
      },
      getObservations: async id => {
        try {
          // Bounded exactly as the trace read is, but classified by the COLLECTION
          // rule: an empty result is a 200 here, so a 404 is a missing route rather
          // than a missing row. `limit` is well above the one row the smoke ships,
          // so a paged walk would only add a failure mode without adding coverage.
          const body = await api(
            cfg,
            'GET',
            `/api/public/observations?traceId=${encodeURIComponent(id)}&limit=50`,
            undefined,
            TRACE_READ_TIMEOUT_MS
          )
          return parseObservationPage(body, id)
        } catch (err) {
          if (listReadErrorDisposition(err) === 'retry') return undefined
          throw err
        }
      },
      shipSpans: async spans => {
        // Through the REAL path, JSONL parse included. The exporter's log is captured
        // rather than dropped, so a per-event rejection reaches the operator.
        const file = path.join(os.tmpdir(), `qa-obs-smoke-${crypto.randomUUID()}.jsonl`)
        fs.writeFileSync(file, spans.map(s => `${JSON.stringify(s)}\n`).join(''), 'utf8')
        const captured: string[] = []
        try {
          const res = await exportSpans(
            cfg,
            parseSpanFile(fs.readFileSync(file, 'utf8')),
            m => captured.push(m),
            SMOKE_EXPORT_OPTIONS
          )
          return {
            observations: res.observations,
            traces: res.traces,
            failed: res.failed,
            log: captured,
          }
        } finally {
          fs.rmSync(file, { force: true })
        }
      },
    })
  } catch (err) {
    if (err instanceof IngestionTimeoutError) {
      console.error('')
      console.error(`FAIL — ingestion never landed: ${err.message}`)
      console.error(
        '  The export itself reported success, so suspect the ingestion worker / queue on ' +
          'the instance, not the payload. This is NOT the same failure as a wrong bucket or cost.'
      )
      return 1
    }
    // Anything else still fails as a DIAGNOSIS, not as a raw stack: the operator gets
    // one line naming the failure class plus the message.
    console.error('')
    console.error(
      `FAIL — the smoke could not complete: ${err instanceof Error ? `${err.name}: ${err.message}` : String(err)}`
    )
    console.error('  Neither the payload nor the ingestion worker is implicated; this is a')
    console.error('  transport/configuration failure against ' + cfg.host + '.')
    return 1
  }
}

// Import-guarded: importing this module runs NO side effects; main() executes
// only when the file is the process entry point.
if (typeof import.meta !== 'undefined' && (import.meta as ImportMeta).main) {
  void main().then(code => {
    process.exitCode = code
  })
}
