// One cost assertion inside the nightly round: reads GENERATION observations
// since a given ISO8601 timestamp and asserts every one of them carries
// non-zero cost — warn-only, fail-open (the round wrapper decides how a
// non-zero exit here is surfaced; this file only computes ok/not-ok and a
// human-readable reason).
//
// Generations are read through `GET /api/public/v2/observations` with the
// `usage` field group, which carries each generation's `totalCost`. The v2 list
// is cursor-paginated (`meta.cursor`, no limit); a page without a cursor is the
// last. Rows are unique by `(traceId, id)`: a re-exported generation can come
// back more than once and counts once. This is the same read the Langfuse v4
// `events_only` write mode serves — the legacy `/api/public/observations`
// endpoint stops seeing new generations there.

import { apiGetAllCursorPages, type FetchFn, type LangfuseConfig } from './http'

export interface AssertCostResult {
  ok: boolean
  message: string
}

export interface AssertCostOpts {
  fetchFn?: FetchFn
  // Langfuse materializes exported observations asynchronously, so an immediate
  // read can see none yet. Zero observations is retried on a bounded schedule
  // before being reported; a zero-COST observation is never retried (it is the
  // signal this tool exists to surface). Two bounds are ACCEPTED, not oversights:
  // scoping is by timestamp only, and a non-empty first batch ends polling even
  // if later worker batches are still materializing. Closing either would
  // require the export's run identity or observation count — coupling this
  // self-contained tool to the judge tree, which the design forbids. The check
  // is an advisory fail-open signal on a single-writer instance; the next
  // night's round re-covers anything a partial batch missed.
  retries?: number
  retryDelayMs?: number
  sleep?: (ms: number) => Promise<void>
}

const DEFAULT_RETRIES = 3
const DEFAULT_RETRY_DELAY_MS = 20_000

// Rows keyed by `(traceId, id)`, first seen wins. A row missing either id would
// collapse with every other such row, so it fails the read instead.
function uniqueGenerations(rows: Array<Record<string, unknown>>): Array<Record<string, unknown>> {
  const byKey = new Map<string, Record<string, unknown>>()
  for (const row of rows) {
    if (typeof row.traceId !== 'string' || typeof row.id !== 'string') {
      throw new Error('GENERATION row has no string traceId and id')
    }
    const key = JSON.stringify([row.traceId, row.id])
    if (!byKey.has(key)) byKey.set(key, row)
  }
  return [...byKey.values()]
}

export async function assertCost(
  fromIso: string,
  cfg: LangfuseConfig,
  opts: AssertCostOpts = {}
): Promise<AssertCostResult> {
  const retries = opts.retries ?? DEFAULT_RETRIES
  const sleep = opts.sleep ?? ((ms: number) => new Promise<void>(r => setTimeout(r, ms)))
  const path = `/api/public/v2/observations?type=GENERATION&fromStartTime=${encodeURIComponent(fromIso)}&fields=core,model,usage`
  const read = async () => uniqueGenerations(await apiGetAllCursorPages(cfg, path, opts.fetchFn))

  let rows = await read()
  for (let attempt = 0; rows.length === 0 && attempt < retries; attempt++) {
    await sleep(opts.retryDelayMs ?? DEFAULT_RETRY_DELAY_MS)
    rows = await read()
  }

  if (rows.length === 0) {
    return { ok: false, message: `assert-cost: nothing to assert — no GENERATION observations found since ${fromIso}` }
  }

  const unpriced = rows
    .filter(obs => obs.totalCost === undefined || obs.totalCost === null || obs.totalCost === 0)
    .map(obs => {
      const model = typeof obs.model === 'string' ? obs.model : 'unknown'
      return `assert-cost: observation for model "${model}" (trace ${String(obs.traceId)}, id ${String(obs.id)}) has zero/missing cost since ${fromIso}`
    })
  if (unpriced.length > 0) return { ok: false, message: unpriced.join('\n') }

  return { ok: true, message: `assert-cost: ${rows.length} GENERATION observation(s) since ${fromIso} all priced` }
}

// Test seam: the retry delay is 20s per attempt, so a command-level test of the
// "nothing found" path sets this to 0. Unset or non-numeric keeps the default.
function retryDelayFromEnv(env: Record<string, string | undefined> = process.env): number | undefined {
  const ms = Number(env.QA_COST_ASSERT_RETRY_DELAY_MS)
  return env.QA_COST_ASSERT_RETRY_DELAY_MS !== undefined && Number.isInteger(ms) && ms >= 0 ? ms : undefined
}

async function main(): Promise<number> {
  const { configFromEnv } = await import('./http')

  const cfg = configFromEnv()
  if (cfg === undefined) {
    console.error('qa-cost-assert: LANGFUSE_HOST/LANGFUSE_PUBLIC_KEY/LANGFUSE_SECRET_KEY must be set.')
    return 2
  }

  const fromIso = process.argv[2]
  if (!fromIso) {
    console.error('qa-cost-assert: usage: bun run assert-cost.ts <FROM ISO8601>')
    return 2
  }

  try {
    const result = await assertCost(fromIso, cfg, { retryDelayMs: retryDelayFromEnv() })
    console.log(result.message)
    return result.ok ? 0 : 1
  } catch (err) {
    console.error(`qa-cost-assert: ${err instanceof Error ? err.message : String(err)}`)
    return 1
  }
}

if (typeof import.meta !== 'undefined' && (import.meta as unknown as { main?: boolean }).main) {
  void main().then(code => {
    process.exitCode = code
  })
}
