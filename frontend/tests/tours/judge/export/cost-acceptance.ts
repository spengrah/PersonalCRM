// CLI: qa-cost-acceptance — the run-once live proof that Langfuse prices a judge
// generation correctly in every usage bucket, not only in total (P3-8).
//
//   LANGFUSE_HOST=... LANGFUSE_PUBLIC_KEY=... LANGFUSE_SECRET_KEY=... QA_TEST_TAG=<slug> \
//     bun run tests/tours/judge/export/cost-acceptance.ts
//
// It runs once for the Langfuse v4 migration, through `make qa-cost-acceptance` only.
// It is in no test lane and no nightly step, so it never gates anything; the file name
// deliberately does not end in `.test.ts`.
//
// What it does, in order:
//   1. Reads the instance's own price for the fixture model — the project-scoped
//      definition `make model-prices-apply` writes — before writing anything.
//   2. Exports ONE fresh synthetic span through the exporter (`exportSpans`), tagged
//      `test:<slug>`, so the generation takes the same build-and-send path a nightly
//      generation takes. The span has no verdict, so no score is sent and nothing is
//      enqueued: the run writes one trace and its generation.
//   3. Reads the generation back through observations v2 with the `usage` field group
//      and compares, bucket by bucket, the usage and cost Langfuse recorded with the
//      fixture's known count and the instance's price for that bucket, and the total
//      with their sum. Buckets are read by key; the server adds a `total` key, so the
//      readback is never required to hold exactly three keys.
//
// The expected side is fixed HERE and never derived from the exporter. Renaming a
// usage key in the exporter (the falsification run renames `input_cached_tokens` to
// `input_cached`) leaves this expectation keyed by the price keys, so the renamed
// bucket reads back absent, which is priced at zero, and the run fails.

import { buildGenAiSpan, type GenAiSpan } from '../adapter/span'
import {
  apiGetAllPages,
  configFromEnv,
  exportSpans,
  generationSpanId,
  hexTraceId,
  usageTraceId,
  type LangfuseConfig,
} from './langfuse'
import { TEST_TAG_RE } from './run'

// A model `infra/langfuse/model-prices.json` declares with all three prices.
export const FIXTURE_MODEL = 'gpt-5.4-mini'

// The fixture's usage, keyed by the price keys `model-prices-apply` writes. Every
// count is non-zero, so a bucket priced at zero always shows as a cost mismatch.
export const FIXTURE_USAGE = { input: 1200, input_cached_tokens: 800, output: 300 } as const
type Bucket = keyof typeof FIXTURE_USAGE
const BUCKETS = Object.keys(FIXTURE_USAGE) as Bucket[]
export type BucketPrices = Record<Bucket, number>

export function fixtureSpan(nowMs: number): GenAiSpan {
  return buildGenAiSpan({
    impl: 'cost-acceptance',
    behaviorId: 'cost-acceptance',
    model: FIXTURE_MODEL,
    startMs: nowMs - 2_000,
    endMs: nowMs - 1_000,
    // The judge reports cached input inclusively; the exporter nets it out of `input`.
    inputTokens: FIXTURE_USAGE.input + FIXTURE_USAGE.input_cached_tokens,
    cachedInputTokens: FIXTURE_USAGE.input_cached_tokens,
    outputTokens: FIXTURE_USAGE.output,
  })
}

// A number, or a numeric string as a decimal column may serialize; else undefined.
function numeric(v: unknown): number | undefined {
  if (typeof v === 'number' && Number.isFinite(v)) return v
  if (typeof v === 'string' && v.trim() !== '' && Number.isFinite(Number(v))) return Number(v)
  return undefined
}

function asRecord(v: unknown): Record<string, unknown> {
  return v !== null && typeof v === 'object' && !Array.isArray(v)
    ? (v as Record<string, unknown>)
    : {}
}

// The instance's default-tier price per bucket for `model`. A missing or zero price
// fails the run before anything is written: a bucket priced at zero cannot tell a
// correctly keyed bucket from a misnamed one, so the comparison would prove nothing.
export async function instancePrices(cfg: LangfuseConfig, model: string): Promise<BucketPrices> {
  const defs = (await apiGetAllPages(cfg, '/api/public/models', 'page')).filter(
    m => m.modelName === model && m.isLangfuseManaged === false
  )
  if (defs.length !== 1) {
    throw new Error(
      `expected exactly 1 project-scoped model definition named ${model}, found ${defs.length} ` +
        '(run make model-prices-apply)'
    )
  }
  const tiers = defs[0].pricingTiers
  const tier = Array.isArray(tiers)
    ? tiers.map(asRecord).find(t => t.isDefault === true)
    : undefined
  if (tier === undefined) throw new Error(`model definition ${model} has no default pricing tier`)
  const prices = asRecord(tier.prices)
  const out = {} as BucketPrices
  for (const b of BUCKETS) {
    const p = numeric(prices[b])
    if (p === undefined || p <= 0) {
      throw new Error(`model definition ${model} has no positive ${b} price`)
    }
    out[b] = p
  }
  return out
}

// Equal within the rounding a decimal cost column introduces; far tighter than the
// smallest bucket cost the fixture produces.
function close(got: number | undefined, want: number): boolean {
  return got !== undefined && Math.abs(got - want) <= Math.max(1e-12, 1e-9 * Math.abs(want))
}

const costText = (v: number | undefined): string => (v === undefined ? '0 (absent)' : String(v))
const usageText = (v: number | undefined): string => (v === undefined ? 'absent' : String(v))

export interface GenerationCheck {
  ok: boolean
  lines: string[]
}

// PURE: compare one v2 generation row with the fixture's counts at `prices`. Every
// bucket and the total get a line, so a capture records each observed value.
export function checkGeneration(
  row: Record<string, unknown>,
  prices: BucketPrices
): GenerationCheck {
  const usage = asRecord(row.usageDetails)
  const cost = asRecord(row.costDetails)
  const lines: string[] = []
  let ok = true
  let wantTotal = 0
  for (const b of BUCKETS) {
    const count = FIXTURE_USAGE[b]
    const want = prices[b] * count
    wantTotal += want
    const gotUsage = numeric(usage[b])
    const gotCost = numeric(cost[b])
    const good = gotUsage === count && close(gotCost, want)
    ok = ok && good
    lines.push(
      `${good ? 'ok' : 'FAIL'} ${b}: usage ${usageText(gotUsage)} (want ${count}), ` +
        `cost ${costText(gotCost)} (want ${want} = ${prices[b]} x ${count})`
    )
  }
  const totalCost = numeric(row.totalCost)
  const detailsTotal = numeric(cost.total)
  const totalGood = close(totalCost, wantTotal) && close(detailsTotal, wantTotal)
  ok = ok && totalGood
  lines.push(
    `${totalGood ? 'ok' : 'FAIL'} total: totalCost ${costText(totalCost)}, ` +
      `costDetails.total ${costText(detailsTotal)} (want ${wantTotal})`
  )
  // A key outside the price keys is a bucket Langfuse could not price; a renamed
  // bucket shows up here. Reported, never required absent (the server adds `total`).
  const extra = [...new Set([...Object.keys(usage), ...Object.keys(cost)])].filter(
    k => k !== 'total' && !(BUCKETS as string[]).includes(k)
  )
  for (const k of extra) {
    lines.push(
      `note ${k}: not a price key; usage ${usageText(numeric(usage[k]))}, ` +
        `cost ${costText(numeric(cost[k]))}`
    )
  }
  return { ok, lines }
}

export interface RunOptions {
  nowMs?: number
  // Langfuse materializes an exported span asynchronously, so the readback is
  // retried while the generation is absent. A row that is present is judged at once:
  // a wrong cost is the finding, not a reason to wait.
  readAttempts?: number
  readDelayMs?: number
  sleep?: (ms: number) => Promise<void>
}

const DEFAULT_READ_ATTEMPTS = 12
const DEFAULT_READ_DELAY_MS = 15_000

async function readGeneration(
  cfg: LangfuseConfig,
  traceId: string,
  generationId: string,
  fromIso: string,
  opts: RunOptions
): Promise<Record<string, unknown> | undefined> {
  const attempts = opts.readAttempts ?? DEFAULT_READ_ATTEMPTS
  const delay = opts.readDelayMs ?? DEFAULT_READ_DELAY_MS
  const sleep = opts.sleep ?? ((ms: number) => new Promise<void>(r => setTimeout(r, ms)))
  const q = new URLSearchParams({
    type: 'GENERATION',
    traceId,
    fromStartTime: fromIso,
    fields: 'core,model,usage',
  })
  for (let attempt = 1; ; attempt++) {
    const rows = await apiGetAllPages(
      cfg,
      `/api/public/v2/observations?${q.toString()}`,
      'observations-v2'
    )
    const row = rows.find(r => r.id === generationId)
    if (row !== undefined || attempt >= attempts) return row
    await sleep(delay)
  }
}

export async function main(
  env: Record<string, string | undefined> = process.env,
  log: (msg: string) => void = console.log,
  errlog: (msg: string) => void = console.error,
  opts: RunOptions = {}
): Promise<number> {
  // Unlike qa-export, an unconfigured run fails: a test that passes without running
  // would read as a proof.
  const cfg = configFromEnv(env)
  if (!cfg) {
    errlog(
      'qa-cost-acceptance: LANGFUSE_HOST/LANGFUSE_PUBLIC_KEY/LANGFUSE_SECRET_KEY must be set ' +
        '(write access required).'
    )
    return 2
  }
  const testTag = env.QA_TEST_TAG
  if (testTag === undefined || !TEST_TAG_RE.test(testTag)) {
    errlog(
      `qa-cost-acceptance: QA_TEST_TAG must be set to a slug matching ${String(TEST_TAG_RE)} ` +
        '— nothing sent.'
    )
    return 2
  }

  try {
    const prices = await instancePrices(cfg, FIXTURE_MODEL)
    const span = fixtureSpan(opts.nowMs ?? Date.now())
    const traceId = hexTraceId(usageTraceId(span))
    const generationId = generationSpanId(span)

    const result = await exportSpans(cfg, [span], msg => log(msg), { testTag, saltPasses: 0 })
    if (result.traces !== 1 || result.observations !== 1) {
      errlog(
        `qa-cost-acceptance: export did not land (${result.traces} trace(s), ` +
          `${result.observations} generation(s), ${result.observationsFailed} generation(s) failed)`
      )
      return 1
    }
    log(
      `qa-cost-acceptance: exported ${FIXTURE_MODEL} generation ${generationId} ` +
        `on trace ${traceId} (test:${testTag})`
    )

    const fromIso = new Date(span.start_time_unix_nano / 1e6 - 60_000).toISOString()
    const row = await readGeneration(cfg, traceId, generationId, fromIso, opts)
    if (row === undefined) {
      errlog(
        `qa-cost-acceptance: generation ${generationId} on trace ${traceId} not readable ` +
          'through observations v2'
      )
      return 1
    }
    const check = checkGeneration(row, prices)
    for (const line of check.lines) log(`qa-cost-acceptance: ${line}`)
    log(
      check.ok
        ? 'qa-cost-acceptance: PASS — every bucket and the total match the instance prices'
        : 'qa-cost-acceptance: FAIL — the readback does not match the instance prices'
    )
    return check.ok ? 0 : 1
  } catch (err) {
    errlog(`qa-cost-acceptance: ${err instanceof Error ? err.message : String(err)}`)
    return 1
  }
}

// Import-guarded: main() runs only when the file is the process entry point.
if (typeof import.meta !== 'undefined' && (import.meta as ImportMeta).main) {
  void main().then(code => {
    process.exitCode = code
  })
}
