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
import { ApiError, api, configFromEnv, exportSpans, parseSpanFile, usageTraceId } from './langfuse'

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

// A fixed instant well in the past, so a startTime taken from the export clock is
// unmistakable rather than plausible.
const START_MS = Date.UTC(2026, 0, 15, 9, 0, 0)
const END_MS = START_MS + 3_200

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

export interface SmokeDeps {
  // Read a trace back. `undefined` = not visible YET (a 404 while the worker is
  // still draining the queue), which is a poll-again signal, not a failure.
  getTrace: (traceId: string) => Promise<Trace | undefined>
  shipSpans: (spans: GenAiSpan[]) => Promise<{ observations: number }>
  log: (msg: string) => void
  sleep?: (ms: number) => Promise<void>
  now?: () => number
  timeoutMs?: number
  intervalMs?: number
  // Pinned by tests; a live run mints them per invocation.
  ids?: { traceId: string; spanId: string }
}

const num = (v: unknown): number | undefined => (typeof v === 'number' ? v : undefined)

const observationsOf = (trace: Trace | undefined): Array<Record<string, unknown>> =>
  Array.isArray(trace?.observations) ? (trace.observations as Array<Record<string, unknown>>) : []

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
    const got = await probe()
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

  const makeSpan = (): GenAiSpan => ({
    ...buildGenAiSpan({
      impl: 'obs-smoke',
      behaviorId: BEHAVIOR_ID,
      model: MODEL,
      startMs: START_MS,
      endMs: END_MS,
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

  const span = makeSpan()
  const carrierId = usageTraceId(span)
  const siblingId = carrierId.replace(/-item0$/, '-item2')
  const spanStartIso = new Date(span.start_time_unix_nano / 1e6).toISOString()

  log(`qa-obs-smoke: shipping synthetic span ${BEHAVIOR_ID} (model ${MODEL})`)
  log(`  trace  ${carrierId}   (sibling: ${siblingId})`)
  log(`  obs    obs-judge-${BEHAVIOR_ID}-${runSpanId}-gen`)
  log('')

  const first = await deps.shipSpans([span])

  const carrier = await waitForPresence(
    `the observation on ${carrierId}`,
    async () => {
      const t = await deps.getTrace(carrierId)
      return observationsOf(t).length > 0 ? t : undefined
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
    `the sibling trace ${siblingId}`,
    () => deps.getTrace(siblingId),
    deps
  )
  const carrierMeta = (carrier.metadata ?? {}) as Record<string, unknown>
  const siblingMeta = (sibling.metadata ?? {}) as Record<string, unknown>
  results.push({
    label: 'usage_attributed',
    ok: carrierMeta.usage_attributed === true && siblingMeta.usage_attributed === false,
    detail: `item0=${String(carrierMeta.usage_attributed)} item2=${String(siblingMeta.usage_attributed)}`,
  })

  // Re-export: the id must be stable (an upsert), never a duplicate. The upsert is
  // processed asynchronously too — but a presence probe cannot tell "upserted" from
  // "not yet processed", which is exactly why the duplicate/id check below is an
  // ASSERTION over the polled state rather than a poll condition.
  const second = await deps.shipSpans([makeSpan()])
  const after = await waitForPresence(
    `the re-exported observation on ${carrierId}`,
    async () => {
      const t = await deps.getTrace(carrierId)
      return observationsOf(t).length > 0 ? t : undefined
    },
    deps
  )
  const afterObs = observationsOf(after)
  results.push({
    label: 're-export',
    ok: afterObs.length === 1 && second.observations === 1 && afterObs[0]?.id === obs.id,
    detail: `${afterObs.length} observation(s), id ${String(afterObs[0]?.id)} (was ${String(obs.id)})`,
  })

  for (const r of results) {
    log(`  ${r.label.padEnd(22)} ${r.ok ? '[OK]' : '[FAIL]'} ${r.detail}`)
  }
  const failed = results.filter(r => !r.ok)
  log('')
  if (failed.length === 0) {
    log('PASS')
    return 0
  }
  // Ingestion LANDED and the values are wrong — a payload/pricing bug, not a race.
  log(
    `FAIL — ingestion landed but ${failed.length} assertion(s) mismatched: ` +
      failed.map(f => f.label).join(', ')
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
      // A 404 means "not visible yet" while the worker drains the queue; every other
      // transport error is a real failure and propagates.
      getTrace: async id => {
        try {
          return await api(cfg, 'GET', `/api/public/traces/${id}`)
        } catch (err) {
          if (err instanceof ApiError && err.status === 404) return undefined
          throw err
        }
      },
      shipSpans: async spans => {
        // Through the REAL path, JSONL parse included.
        const file = path.join(os.tmpdir(), `qa-obs-smoke-${crypto.randomUUID()}.jsonl`)
        fs.writeFileSync(file, spans.map(s => `${JSON.stringify(s)}\n`).join(''), 'utf8')
        try {
          return await exportSpans(cfg, parseSpanFile(fs.readFileSync(file, 'utf8')), () => {})
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
    throw err
  }
}

// Import-guarded: importing this module (as obs-smoke.test.ts does) runs NO side
// effects; main() executes only when the file is the process entry point.
if (typeof import.meta !== 'undefined' && (import.meta as ImportMeta).main) {
  void main().then(code => {
    process.exitCode = code
  })
}
