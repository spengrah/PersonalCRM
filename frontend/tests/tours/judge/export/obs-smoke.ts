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
// Requires WRITE access to the Langfuse instance. Exits non-zero on any failed
// assertion.

import * as crypto from 'crypto'
import * as fs from 'fs'
import * as os from 'os'
import * as path from 'path'
import { buildGenAiSpan } from '../adapter/span'
import type { Scenario } from '../label-trace'
import { api, configFromEnv, exportSpans, parseSpanFile, usageTraceId } from './langfuse'

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

const num = (v: unknown): number | undefined => (typeof v === 'number' ? v : undefined)

async function main(): Promise<number> {
  const cfg = configFromEnv()
  if (!cfg) {
    console.error(
      'qa-obs-smoke: LANGFUSE_HOST / LANGFUSE_PUBLIC_KEY / LANGFUSE_SECRET_KEY must be set ' +
        '(and the keys must have WRITE access).'
    )
    return 2
  }

  // One id pair for this invocation: stable across both exports below, unique across runs.
  const suffix = `${new Date().toISOString().replace(/[-:.TZ]/g, '')}${crypto.randomBytes(3).toString('hex')}`
  const runTraceId = crypto.randomBytes(16).toString('hex')
  const runSpanId = suffix.slice(-16)

  const makeSpan = (): ReturnType<typeof buildGenAiSpan> => ({
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
  const obsId = `obs-judge-${BEHAVIOR_ID}-${runSpanId}-gen`

  console.log(`qa-obs-smoke: shipping synthetic span ${BEHAVIOR_ID} (model ${MODEL})`)
  console.log(`  trace  ${carrierId}   (sibling: ${siblingId})`)
  console.log(`  obs    ${obsId}`)
  console.log('')

  // Ship through the REAL path, JSONL parse included.
  const file = path.join(os.tmpdir(), `qa-obs-smoke-${runSpanId}.jsonl`)
  fs.writeFileSync(file, `${JSON.stringify(span)}\n`, 'utf8')
  let first
  try {
    first = await exportSpans(cfg, parseSpanFile(fs.readFileSync(file, 'utf8')), () => {})
  } finally {
    fs.rmSync(file, { force: true })
  }

  const trace = await api(cfg, 'GET', `/api/public/traces/${carrierId}`)
  const observations = Array.isArray(trace.observations)
    ? (trace.observations as Array<Record<string, unknown>>)
    : []
  const obs = observations[0] ?? {}
  const usage = (obs.usageDetails ?? {}) as Record<string, unknown>
  const meta = (obs.metadata ?? {}) as Record<string, unknown>
  const totalCost = num(trace.totalCost) ?? 0
  const startTime = typeof obs.startTime === 'string' ? obs.startTime : ''
  const spanStartIso = new Date(span.start_time_unix_nano / 1e6).toISOString()

  const results: Assertion[] = [
    {
      label: 'observations',
      ok: observations.length === 1 && first.observations === 1,
      detail: `${observations.length} on the trace, exporter counted ${first.observations}`,
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

  // Re-export: the id must be stable (an upsert), never a duplicate.
  const second = await exportSpans(cfg, [makeSpan()], () => {})
  const after = await api(cfg, 'GET', `/api/public/traces/${carrierId}`)
  const afterObs = Array.isArray(after.observations)
    ? (after.observations as Array<Record<string, unknown>>)
    : []
  results.push({
    label: 're-export',
    ok: afterObs.length === 1 && second.observations === 1 && afterObs[0]?.id === obs.id,
    detail: `${afterObs.length} observation(s), id ${String(afterObs[0]?.id)} (was ${String(obs.id)})`,
  })

  // The sibling trace must say, in metadata, that it carries none of the usage.
  const sibling = await api(cfg, 'GET', `/api/public/traces/${siblingId}`)
  const carrierMeta = (trace.metadata ?? {}) as Record<string, unknown>
  const siblingMeta = (sibling.metadata ?? {}) as Record<string, unknown>
  results.push({
    label: 'usage_attributed',
    ok: carrierMeta.usage_attributed === true && siblingMeta.usage_attributed === false,
    detail: `item0=${String(carrierMeta.usage_attributed)} item2=${String(siblingMeta.usage_attributed)}`,
  })

  for (const r of results) {
    console.log(`  ${r.label.padEnd(22)} ${r.ok ? '[OK]' : '[FAIL]'} ${r.detail}`)
  }
  const failed = results.filter(r => !r.ok)
  console.log('')
  if (failed.length === 0) {
    console.log('PASS')
    return 0
  }
  console.log(`FAIL — ${failed.length} assertion(s): ${failed.map(f => f.label).join(', ')}`)
  return 1
}

// Import-guarded: importing this module runs NO side effects; main() executes only
// when the file is the process entry point.
if (typeof import.meta !== 'undefined' && (import.meta as ImportMeta).main) {
  void main().then(code => {
    process.exitCode = code
  })
}
