// CLI: qa-obs-smoke — live-verify the usage generation observation end-to-end.
//
//   LANGFUSE_HOST=... LANGFUSE_PUBLIC_KEY=... LANGFUSE_SECRET_KEY=... \
//     bun run tests/tours/judge/export/obs-smoke.ts
//
// Unit tests can only prove we SEND the documented payload. This proves the
// deployed server ACCEPTS and PRICES it: it ships one synthetic span through the
// real `exportSpans` path, reads the carrier trace back, and asserts the buckets
// echoed exactly, a non-zero cost matching a hand-computed figure, a span-derived
// startTime, and a stable observation id across a re-export.
//
// The span ids are minted ONCE PER INVOCATION rather than left random or pinned
// globally. Stable WITHIN the run so the second export demonstrates the upsert;
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

// A fixed instant well in the past, so a startTime taken from the export clock is
// unmistakable rather than plausible.
const START_MS = Date.UTC(2026, 0, 15, 9, 0, 0)
const END_MS = START_MS + 3_200
// The re-export ships the same observation id with a different (still fixed, still
// past) end instant — the marker that makes "the upsert was processed" observable.
// The comparison truncates to WHOLE SECONDS to tolerate server-side millisecond
// formatting, so the gap MUST exceed one second or the marker silently stops
// discriminating and the upsert check becomes unfalsifiable again. Derived from that
// granularity rather than hand-picked, and asserted by MARKER_GAP_MS's own test.
export const MARKER_TRUNCATION_MS = 1_000
export const MARKER_GAP_MS = MARKER_TRUNCATION_MS * 4
export const RE_EXPORT_END_MS = END_MS + MARKER_GAP_MS
export const FIRST_END_MS = END_MS

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
  getTrace: (traceId: string) => Promise<Trace | undefined>
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

const hasKey = (v: unknown, key: string): boolean =>
  v !== null && typeof v === 'object' && key in (v as Record<string, unknown>)

const observationsOf = (trace: Trace | undefined): Array<Record<string, unknown>> =>
  Array.isArray(trace?.observations) ? (trace.observations as Array<Record<string, unknown>>) : []

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

export async function runSmoke(deps: SmokeDeps): Promise<number> {
  const log = deps.log
  // One id pair for this invocation: stable across both exports below, unique across runs.
  const runTraceId = deps.ids?.traceId ?? crypto.randomBytes(16).toString('hex')
  const runSpanId = deps.ids?.spanId ?? crypto.randomBytes(8).toString('hex')

  // `endMs` is the re-export's observable MARKER: the second export ships the same
  // observation id with a DIFFERENT endTime, so "the upsert was processed" becomes a
  // state a probe can actually wait for. Without it the first export's observation
  // already satisfies every presence check and the upsert assertion cannot fail.
  const makeSpan = (endMs: number): GenAiSpan => ({
    ...buildGenAiSpan({
      impl: 'obs-smoke',
      behaviorId: BEHAVIOR_ID,
      model: MODEL,
      startMs: START_MS,
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

  const span = makeSpan(END_MS)
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

  const first = await deps.shipSpans([span])
  const firstProblem = exportProblem(first, 'export')
  if (firstProblem !== undefined) {
    reportExportProblem(firstProblem, first)
    return 1
  }

  // Presence must include the FINAL body's marks, not merely the row: `exportSpans`
  // sends a minimal init trace-create first, so a trace can be readable before its
  // metadata exists. Asserting `usage_attributed` off that intermediate state is a
  // FALSE MISMATCH during ordinary worker lag — the mirror of a false pass, and it
  // burns the operator's one live run just as effectively.
  const carrier = await waitForPresence(
    `the observation + final body on ${carrierId}`,
    async () => {
      const t = await deps.getTrace(carrierId)
      const settled = observationsOf(t).length > 0 && hasKey(t?.metadata, 'usage_attributed')
      return settled ? t : undefined
    },
    deps
  )

  const obs = observationsOf(carrier)[0]
  const usage = (obs.usageDetails ?? {}) as Record<string, unknown>
  const meta = (obs.metadata ?? {}) as Record<string, unknown>
  const totalCost = num(carrier.totalCost) ?? 0
  const startTime = typeof obs.startTime === 'string' ? obs.startTime : ''

  const results: Assertion[] = [
    {
      label: 'observations',
      ok: observationsOf(carrier).length === 1 && first.observations === 1,
      detail: `${observationsOf(carrier).length} on the trace, exporter counted ${first.observations}`,
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
    { label: 'totalCost', ok: totalCost > 0, detail: `${totalCost.toFixed(6)} (> 0)` },
    {
      label: 'expected',
      ok: Math.abs(totalCost - EXPECTED_COST) < COST_TOLERANCE,
      detail:
        `${EXPECTED_COST.toFixed(6)} vs observed ${totalCost.toFixed(6)} ` +
        `(|delta| < ${COST_TOLERANCE})\n` +
        `                         ${EXPECT_INPUT}*0.75/M + ${CACHED_INPUT_TOKENS}*0.075/M + ${OUTPUT_TOKENS}*4.50/M`,
    },
    {
      label: 'startTime',
      ok: startTime.startsWith(spanStartIso.slice(0, 19)),
      detail: `${startTime} (span start ${spanStartIso}, not export time)`,
    },
  ]

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

  // Re-export: the id must be stable (an upsert), never a duplicate. Waiting for
  // "an observation exists" would be satisfied by the FIRST export's row, so the
  // check could not fail — wait for the MARKER (the changed endTime) to prove the
  // second write was actually processed, and only then assert count + id.
  const secondEndIso = new Date(RE_EXPORT_END_MS).toISOString()
  const second = await deps.shipSpans([makeSpan(RE_EXPORT_END_MS)])
  const secondProblem = exportProblem(second, 're-export')
  if (secondProblem !== undefined) {
    // Record it as a failed assertion instead of returning: seven assertions —
    // including the pinned cost oracle — are already computed, and on a run the
    // operator cannot cheaply repeat, discarding them is pure loss.
    reportExportProblem(secondProblem, second)
    results.push({ label: 're-export', ok: false, detail: secondProblem })
  } else {
    const after = await waitForPresence(
      `the re-export to be processed on ${carrierId} (endTime ${secondEndIso})`,
      async () => {
        const t = await deps.getTrace(carrierId)
        const rows = observationsOf(t)
        const marked = rows.some(
          o => typeof o.endTime === 'string' && o.endTime.startsWith(secondEndIso.slice(0, 19))
        )
        return marked ? t : undefined
      },
      deps
    )
    const afterObs = observationsOf(after)
    results.push({
      label: 're-export',
      ok: afterObs.length === 1 && afterObs[0]?.id === obs.id,
      detail: `${afterObs.length} observation(s), id ${String(afterObs[0]?.id)} (was ${String(obs.id)})`,
    })
  }

  for (const r of results) {
    log(`  ${r.label.padEnd(22)} ${r.ok ? '[OK]' : '[FAIL]'} ${r.detail}`)
  }
  const failed = results.filter(r => !r.ok)
  log('')
  if (failed.length === 0) {
    log('PASS')
    return 0
  }
  // Everything that could be computed HAS been printed above; this line names which
  // assertions failed. "landed but wrong" stays the diagnosis whenever the export
  // itself was clean — a stalled ingestion raises IngestionTimeoutError instead.
  log(
    `FAIL — ${failed.length} assertion(s) failed: ${failed.map(f => f.label).join(', ')}` +
      (secondProblem === undefined ? ' (ingestion landed; the values are wrong)' : '')
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

// Import-guarded: importing this module (as obs-smoke.test.ts does) runs NO side
// effects; main() executes only when the file is the process entry point.
if (typeof import.meta !== 'undefined' && (import.meta as ImportMeta).main) {
  void main().then(code => {
    process.exitCode = code
  })
}
