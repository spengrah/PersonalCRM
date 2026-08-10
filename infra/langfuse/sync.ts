// Refreshes every declared row's prices from its declared source. Sync never
// imports a catalog and never adds or removes rows — declaring a model is a hand
// edit (see schema.ts / model-prices.json); this file only refreshes the prices
// of rows that already exist.
//
// Both source APIs are anonymous, probed live on 2026-08-09:
//   - Venice: GET https://api.venice.ai/api/v1/models — an entry's per-token
//     prices live at `model_spec.pricing.{input,output,cache_input}.usd`, quoted
//     in USD per MILLION tokens (÷1e6 to get per-token), and its top-level `type`
//     field is "text" for text models (anything else fails loudly rather than
//     being priced wrong).
//   - OpenRouter: GET https://openrouter.ai/api/v1/models — an entry's prices
//     live at `pricing.{prompt,completion,input_cache_read}`, already per-token
//     but quoted as decimal STRINGS (parsed, never divided). OpenRouter's large-
//     context `pricing.overrides` tiers are deliberately ignored — see the spec's
//     Non-Goals.

import type { ModelRow } from './schema'

const VENICE_MODELS_URL = 'https://api.venice.ai/api/v1/models'
const OPENROUTER_MODELS_URL = 'https://openrouter.ai/api/v1/models'

export interface CatalogFetchers {
  fetchVenice: () => Promise<unknown>
  fetchOpenRouter: () => Promise<unknown>
}

export function liveFetchers(): CatalogFetchers {
  return {
    fetchVenice: async () => {
      const res = await fetch(VENICE_MODELS_URL, { headers: { Accept: 'application/json' } })
      if (!res.ok) throw new Error(`venice models fetch failed: ${res.status} ${await res.text()}`)
      return res.json()
    },
    fetchOpenRouter: async () => {
      const res = await fetch(OPENROUTER_MODELS_URL, { headers: { Accept: 'application/json' } })
      if (!res.ok) throw new Error(`open-router models fetch failed: ${res.status} ${await res.text()}`)
      return res.json()
    },
  }
}

interface VeniceEntry {
  isText: boolean
  input: number
  output: number
  cachedInput?: number
}

interface OpenRouterEntry {
  input: number
  output: number
  cachedInput?: number
}

function numAt(obj: Record<string, unknown> | undefined, ...path: string[]): number | undefined {
  let cur: unknown = obj
  for (const key of path) {
    if (cur === null || typeof cur !== 'object') return undefined
    cur = (cur as Record<string, unknown>)[key]
  }
  return typeof cur === 'number' ? cur : undefined
}

function parseVeniceCatalog(raw: unknown): Map<string, VeniceEntry> {
  const body = raw as { data?: unknown[] }
  const map = new Map<string, VeniceEntry>()
  for (const entry of Array.isArray(body.data) ? body.data : []) {
    const e = entry as Record<string, unknown>
    if (typeof e.id !== 'string') continue
    const modelSpec = e.model_spec as Record<string, unknown> | undefined
    const pricing = modelSpec?.pricing as Record<string, unknown> | undefined
    const inputUsd = numAt(pricing, 'input', 'usd')
    const outputUsd = numAt(pricing, 'output', 'usd')
    if (inputUsd === undefined || outputUsd === undefined) continue
    const cacheUsd = numAt(pricing, 'cache_input', 'usd')
    map.set(e.id, {
      isText: e.type === 'text',
      input: inputUsd / 1_000_000,
      output: outputUsd / 1_000_000,
      ...(cacheUsd !== undefined ? { cachedInput: cacheUsd / 1_000_000 } : {}),
    })
  }
  return map
}

function strNum(v: unknown): number | undefined {
  if (typeof v !== 'string') return undefined
  const n = Number(v)
  return Number.isFinite(n) ? n : undefined
}

function parseOpenRouterCatalog(raw: unknown): Map<string, OpenRouterEntry> {
  const body = raw as { data?: unknown[] }
  const map = new Map<string, OpenRouterEntry>()
  for (const entry of Array.isArray(body.data) ? body.data : []) {
    const e = entry as Record<string, unknown>
    if (typeof e.id !== 'string') continue
    const pricing = e.pricing as Record<string, unknown> | undefined
    const prompt = strNum(pricing?.prompt)
    const completion = strNum(pricing?.completion)
    if (prompt === undefined || completion === undefined) continue
    const cacheRead = strNum(pricing?.input_cache_read)
    map.set(e.id, {
      input: prompt,
      output: completion,
      ...(cacheRead !== undefined ? { cachedInput: cacheRead } : {}),
    })
  }
  return map
}

// Compute-all-then-return: any row that cannot be refreshed throws before this
// function returns anything, so the caller (main(), below) never gets a partial
// result to write. Row count and order are always preserved.
export async function syncRows(rows: ModelRow[], fetchers: CatalogFetchers): Promise<ModelRow[]> {
  const needsVenice = rows.some(r => r.source === 'venice')
  const needsOpenRouter = rows.some(r => r.source === 'open-router')
  const venice = needsVenice ? parseVeniceCatalog(await fetchers.fetchVenice()) : new Map<string, VeniceEntry>()
  const openrouter = needsOpenRouter
    ? parseOpenRouterCatalog(await fetchers.fetchOpenRouter())
    : new Map<string, OpenRouterEntry>()

  return rows.map(row => {
    if (row.source === 'venice') {
      const entry = venice.get(row.sourceId)
      if (entry === undefined) {
        throw new Error(
          `model-prices sync: venice catalog has no entry for sourceId "${row.sourceId}" (row ${row.modelName})`
        )
      }
      if (!entry.isText) {
        throw new Error(
          `model-prices sync: venice model "${row.sourceId}" (row ${row.modelName}) is not a text model`
        )
      }
      return {
        ...row,
        prices: {
          input: entry.input,
          output: entry.output,
          ...(entry.cachedInput !== undefined ? { cachedInput: entry.cachedInput } : {}),
        },
      }
    }
    const entry = openrouter.get(row.sourceId)
    if (entry === undefined) {
      throw new Error(
        `model-prices sync: open-router catalog has no entry for sourceId "${row.sourceId}" (row ${row.modelName})`
      )
    }
    return {
      ...row,
      prices: {
        input: entry.input,
        output: entry.output,
        ...(entry.cachedInput !== undefined ? { cachedInput: entry.cachedInput } : {}),
      },
    }
  })
}

async function main(): Promise<number> {
  const fs = await import('fs')
  const path = await import('path')
  const { parseFile, serialize } = await import('./schema')

  const filePath = path.join(import.meta.dir, 'model-prices.json')
  const file = parseFile(JSON.parse(fs.readFileSync(filePath, 'utf8')))

  let updated: ModelRow[]
  try {
    updated = await syncRows(file.models, liveFetchers())
  } catch (err) {
    console.error(`model-prices-sync: ${err instanceof Error ? err.message : String(err)}`)
    return 1
  }

  const out = serialize({ models: updated })
  fs.writeFileSync(filePath, out, 'utf8')
  console.log(`model-prices-sync: wrote ${updated.length} row(s) to ${filePath}`)
  return 0
}

if (typeof import.meta !== 'undefined' && (import.meta as unknown as { main?: boolean }).main) {
  void main().then(code => {
    process.exitCode = code
  })
}
