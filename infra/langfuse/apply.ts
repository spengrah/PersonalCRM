// Converges Langfuse's project-scoped model definitions to model-prices.json.
//
// Hard constraint (spec): apply never reads upstream and never decides anything
// beyond "does the existing project-scoped definition match the file". It never
// consults Venice, OpenRouter, or Langfuse's managed rows — any logic that did
// would be the deleted reconciler growing back. Every request this file issues
// goes through http.ts's `api`/`apiGetAllPages`, which always build the request
// URL as `${cfg.host}${path}` — there is no code path here that can reach any
// host but the configured Langfuse instance.

import { api, apiGetAllPages, type FetchFn, type LangfuseConfig } from './http'
import { emitMatchPattern, type ModelRow } from './schema'

interface ExistingTier {
  isDefault: boolean
  priority: number
  conditions: unknown[]
  prices: Record<string, number>
}

interface ExistingModel {
  id: string
  modelName: string
  matchPattern: string
  isLangfuseManaged: boolean
  pricingTiers: ExistingTier[]
}

export interface ApplyResult {
  created: number
  deleted: number
  unchanged: number
  warnings: string[]
}

export interface ApplyOpts {
  fetchFn?: FetchFn
  log?: (msg: string) => void
}

// The desired default-tier price map: input/output always, input_cached_tokens
// present iff the row carries cachedInput. This IS the identity a row is compared
// by, alongside matchPattern (see pricesEqual below).
function buildPriceMap(row: ModelRow): Record<string, number> {
  return {
    input: row.prices.input,
    output: row.prices.output,
    ...(row.prices.cachedInput !== undefined ? { input_cached_tokens: row.prices.cachedInput } : {}),
  }
}

function pricesEqual(a: Record<string, number>, b: Record<string, number>): boolean {
  const ak = Object.keys(a).sort()
  const bk = Object.keys(b).sort()
  if (ak.length !== bk.length) return false
  for (let i = 0; i < ak.length; i++) {
    if (ak[i] !== bk[i] || a[ak[i]!] !== b[bk[i]!]) return false
  }
  return true
}

async function createDefinition(
  cfg: LangfuseConfig,
  row: ModelRow,
  matchPattern: string,
  prices: Record<string, number>,
  fetchFn: FetchFn | undefined
): Promise<void> {
  await api(
    cfg,
    'POST',
    '/api/public/models',
    {
      modelName: row.modelName,
      matchPattern,
      unit: 'TOKENS',
      pricingTiers: [{ isDefault: true, priority: 0, conditions: [], prices }],
    },
    fetchFn
  )
}

export async function applyPrices(
  rows: ModelRow[],
  cfg: LangfuseConfig,
  opts: ApplyOpts = {}
): Promise<ApplyResult> {
  const log = opts.log ?? (() => {})
  const existingAll = (await apiGetAllPages(
    cfg,
    '/api/public/models',
    opts.fetchFn
  )) as unknown as ExistingModel[]
  const projectScoped = existingAll.filter(m => m.isLangfuseManaged === false)
  const byName = new Map(projectScoped.map(m => [m.modelName, m]))

  let created = 0
  let deleted = 0
  let unchanged = 0
  const warnings: string[] = []

  for (const row of rows) {
    const desiredPattern = emitMatchPattern(row)
    const desiredPrices = buildPriceMap(row)
    const existing = byName.get(row.modelName)
    byName.delete(row.modelName)

    if (existing === undefined) {
      await createDefinition(cfg, row, desiredPattern, desiredPrices, opts.fetchFn)
      created++
      log(`created ${row.modelName}`)
      continue
    }

    const defaultTier = existing.pricingTiers.find(t => t.isDefault === true)
    const existingPrices = defaultTier?.prices ?? {}
    // Identity requires the WHOLE tier structure, not just the default tier's
    // prices: a definition that also retains a conditional tier (e.g. one the
    // retired reconciler mirrored from upstream) has not converged to the flat
    // one-tier shape, even when its default prices already match.
    const identical =
      existing.matchPattern === desiredPattern &&
      existing.pricingTiers.length === 1 &&
      pricesEqual(existingPrices, desiredPrices)

    if (identical) {
      unchanged++
      continue
    }

    await api(cfg, 'DELETE', `/api/public/models/${encodeURIComponent(existing.id)}`, undefined, opts.fetchFn)
    deleted++
    await createDefinition(cfg, row, desiredPattern, desiredPrices, opts.fetchFn)
    created++
    log(`replaced ${row.modelName}`)
  }

  for (const stray of byName.values()) {
    const msg = `stray project-scoped model definition not in model-prices.json: ${stray.modelName}`
    warnings.push(msg)
    log(`WARNING ${msg}`)
  }

  return { created, deleted, unchanged, warnings }
}

async function main(): Promise<number> {
  const fs = await import('fs')
  const path = await import('path')
  const { parseFile } = await import('./schema')
  const { configFromEnv } = await import('./http')

  const cfg = configFromEnv()
  if (cfg === undefined) {
    console.error(
      'model-prices-apply: LANGFUSE_HOST/LANGFUSE_PUBLIC_KEY/LANGFUSE_SECRET_KEY must be set (write access required).'
    )
    return 2
  }

  const filePath = path.join(import.meta.dir, 'model-prices.json')
  const file = parseFile(JSON.parse(fs.readFileSync(filePath, 'utf8')))

  try {
    const result = await applyPrices(file.models, cfg, {
      log: msg => console.log(`model-prices-apply: ${msg}`),
    })
    console.log(
      `model-prices-apply: created=${result.created} deleted=${result.deleted} unchanged=${result.unchanged}`
    )
    return 0
  } catch (err) {
    console.error(`model-prices-apply: ${err instanceof Error ? err.message : String(err)}`)
    return 1
  }
}

if (typeof import.meta !== 'undefined' && (import.meta as unknown as { main?: boolean }).main) {
  void main().then(code => {
    process.exitCode = code
  })
}
