// One cost assertion inside the nightly round: reads GENERATION observations
// since a given ISO8601 timestamp and asserts every one of them carries
// non-zero cost — warn-only, fail-open (the round wrapper decides how a
// non-zero exit here is surfaced; this file only computes ok/not-ok and a
// human-readable reason).
//
// The `/api/public/observations` endpoint is the authoritative view of a
// generation row, and an observation's own cost is `costDetails.total`,
// falling back to the flattened `calculatedTotalCost`.

import { apiGetAllPages, type FetchFn, type LangfuseConfig } from './http'

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

function numOf(v: unknown): number | undefined {
  return typeof v === 'number' ? v : undefined
}

function observationCost(obs: Record<string, unknown>): number | undefined {
  const details = obs.costDetails
  const total = details !== null && typeof details === 'object' ? numOf((details as Record<string, unknown>).total) : undefined
  return total ?? numOf(obs.calculatedTotalCost)
}

export async function assertCost(
  fromIso: string,
  cfg: LangfuseConfig,
  opts: AssertCostOpts = {}
): Promise<AssertCostResult> {
  const retries = opts.retries ?? DEFAULT_RETRIES
  const sleep = opts.sleep ?? ((ms: number) => new Promise<void>(r => setTimeout(r, ms)))
  const path = `/api/public/observations?type=GENERATION&fromStartTime=${encodeURIComponent(fromIso)}`

  let rows = await apiGetAllPages(cfg, path, opts.fetchFn)
  for (let attempt = 0; rows.length === 0 && attempt < retries; attempt++) {
    await sleep(opts.retryDelayMs ?? DEFAULT_RETRY_DELAY_MS)
    rows = await apiGetAllPages(cfg, path, opts.fetchFn)
  }

  if (rows.length === 0) {
    return { ok: false, message: `assert-cost: nothing to assert — no GENERATION observations found since ${fromIso}` }
  }

  for (const obs of rows) {
    const cost = observationCost(obs)
    if (cost === undefined || cost === 0) {
      const model = typeof obs.model === 'string' ? obs.model : 'unknown'
      return {
        ok: false,
        message: `assert-cost: observation for model "${model}" has zero/missing cost since ${fromIso}`,
      }
    }
  }

  return { ok: true, message: `assert-cost: ${rows.length} GENERATION observation(s) since ${fromIso} all priced` }
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
    const result = await assertCost(fromIso, cfg)
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
