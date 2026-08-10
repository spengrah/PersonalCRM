import { describe, expect, test } from 'bun:test'
import { applyPrices } from './apply'
import type { ModelRow } from './schema'
import type { LangfuseConfig } from './http'

const cfg: LangfuseConfig = { host: 'https://fake-langfuse.test', publicKey: 'pk', secretKey: 'sk' }

interface Call {
  method: string
  path: string
  body: unknown
}

// A fake transport recording every call in order, backed by an in-memory list of
// "existing" model definitions it serves through the paginated GET endpoint (one
// or two pages, per test) and mutates on DELETE/POST like the real API would.
function makeTransport(existing: Array<Record<string, unknown>>, opts: { pageSize?: number } = {}) {
  const calls: Call[] = []
  const store = [...existing]
  let nextId = 1000
  const pageSize = opts.pageSize ?? 100

  const fetchFn: typeof fetch = async (input, init) => {
    const url = new URL(String(input))
    // Negative contract: apply must never call anything but the configured
    // Langfuse host.
    if (`${url.protocol}//${url.host}` !== cfg.host) {
      throw new Error(`apply issued a request to a non-Langfuse host: ${url.href}`)
    }
    const method = init?.method ?? 'GET'
    const path = url.pathname + url.search

    if (method === 'GET' && url.pathname === '/api/public/models') {
      const page = Number(url.searchParams.get('page') ?? '1')
      calls.push({ method, path, body: undefined })
      const start = (page - 1) * pageSize
      const pageItems = store.slice(start, start + pageSize)
      const totalPages = Math.max(1, Math.ceil(store.length / pageSize))
      return new Response(
        JSON.stringify({ data: pageItems, meta: { page, limit: pageSize, totalItems: store.length, totalPages } }),
        { status: 200 }
      )
    }

    if (method === 'DELETE' && url.pathname.startsWith('/api/public/models/')) {
      const id = decodeURIComponent(url.pathname.slice('/api/public/models/'.length))
      calls.push({ method, path, body: undefined })
      const idx = store.findIndex(m => m.id === id)
      if (idx >= 0) store.splice(idx, 1)
      return new Response('{}', { status: 200 })
    }

    if (method === 'POST' && url.pathname === '/api/public/models') {
      const body = init?.body ? JSON.parse(String(init.body)) : {}
      calls.push({ method, path, body })
      const created = { id: `new-${nextId++}`, isLangfuseManaged: false, ...body }
      store.push(created)
      return new Response(JSON.stringify(created), { status: 200 })
    }

    throw new Error(`fake transport: unexpected ${method} ${path}`)
  }

  return { fetchFn, calls, store }
}

function row(over: Partial<ModelRow> = {}): ModelRow {
  return {
    modelName: 'gpt-5.5',
    source: 'open-router',
    sourceId: 'openai/gpt-5.5',
    prices: { input: 5e-6, output: 3e-5 },
    ...over,
  }
}

function converged(r: ModelRow, extra: Record<string, unknown> = {}) {
  return {
    id: `existing-${r.modelName}`,
    modelName: r.modelName,
    matchPattern: `(?i)^${r.modelName.replace(/[.*+?^${}()|[\]\\/-]/g, '\\$&')}$`,
    isLangfuseManaged: false,
    unit: 'TOKENS',
    createdAt: '2026-01-01T00:00:00.000Z',
    updatedAt: '2026-01-01T00:00:00.000Z',
    prices: { input: r.prices.input, output: r.prices.output }, // deprecated top-level view — must be ignored
    pricingTiers: [
      {
        id: `${r.modelName}-tier`,
        isDefault: true,
        priority: 0,
        conditions: [],
        prices: {
          input: r.prices.input,
          output: r.prices.output,
          ...(r.prices.cachedInput !== undefined ? { input_cached_tokens: r.prices.cachedInput } : {}),
        },
      },
    ],
    ...extra,
  }
}

describe('idempotent', () => {
  test('an already-converged instance performs zero DELETE/POST calls', async () => {
    const r = row()
    const { fetchFn, calls } = makeTransport([converged(r)])
    const result = await applyPrices([r], cfg, { fetchFn })
    expect(calls.filter(c => c.method !== 'GET')).toHaveLength(0)
    expect(result.unchanged).toBe(1)
    expect(result.created).toBe(0)
    expect(result.deleted).toBe(0)
  })
})

describe('one edit', () => {
  test('exactly one DELETE then one POST, both touching only the edited row', async () => {
    const r = row()
    const stale = converged(row({ prices: { input: 1e-6, output: 1e-6 } }))
    const { fetchFn, calls } = makeTransport([stale])
    const result = await applyPrices([r], cfg, { fetchFn })
    const writes = calls.filter(c => c.method !== 'GET')
    expect(writes.map(c => c.method)).toEqual(['DELETE', 'POST'])
    expect(writes[0]?.path).toBe(`/api/public/models/${stale.id}`)
    expect((writes[1]?.body as { modelName: string }).modelName).toBe(r.modelName)
    expect(result.deleted).toBe(1)
    expect(result.created).toBe(1)
  })
})

describe('row missing from instance', () => {
  test('one POST, no DELETE', async () => {
    const r = row()
    const { fetchFn, calls } = makeTransport([])
    const result = await applyPrices([r], cfg, { fetchFn })
    const writes = calls.filter(c => c.method !== 'GET')
    expect(writes.map(c => c.method)).toEqual(['POST'])
    expect(result.created).toBe(1)
    expect(result.deleted).toBe(0)
  })
})

describe('POST body contract', () => {
  test('unit TOKENS, matchPattern, exactly one default tier; input_cached_tokens iff cachedInput set', async () => {
    const withCache = row({ modelName: 'gpt-5.4-mini', sourceId: 'openai/gpt-5.4-mini', prices: { input: 7.5e-7, output: 4.5e-6, cachedInput: 7.5e-8 } })
    const noCache = row({ modelName: 'gpt-5.6-terra', sourceId: 'openai/gpt-5.6-terra' })
    const { fetchFn, calls } = makeTransport([])
    await applyPrices([withCache, noCache], cfg, { fetchFn })
    const posts = calls.filter(c => c.method === 'POST').map(c => c.body as Record<string, unknown>)

    const cacheBody = posts.find(b => b.modelName === 'gpt-5.4-mini')!
    expect(cacheBody.unit).toBe('TOKENS')
    expect(cacheBody.matchPattern).toBe('(?i)^gpt\\-5\\.4\\-mini$')
    expect(cacheBody.pricingTiers).toEqual([
      { isDefault: true, priority: 0, conditions: [], prices: { input: 7.5e-7, output: 4.5e-6, input_cached_tokens: 7.5e-8 } },
    ])

    const noCacheBody = posts.find(b => b.modelName === 'gpt-5.6-terra')!
    const tiers = noCacheBody.pricingTiers as Array<{ prices: Record<string, number> }>
    expect(Object.keys(tiers[0]!.prices).sort()).toEqual(['input', 'output'])
  })
})

describe('editing only cachedInput', () => {
  test('participates in the identity comparison: DELETE then POST', async () => {
    const withoutCache = row()
    const withCache = row({ prices: { ...withoutCache.prices, cachedInput: 1e-7 } })
    const stale = converged(withoutCache)
    const { fetchFn, calls } = makeTransport([stale])
    const result = await applyPrices([withCache], cfg, { fetchFn })
    const writes = calls.filter(c => c.method !== 'GET')
    expect(writes.map(c => c.method)).toEqual(['DELETE', 'POST'])
    expect(result.deleted).toBe(1)
    expect(result.created).toBe(1)
  })
})

describe('comparison ignores server-side noise', () => {
  test('differing id/timestamps/unit-echo/deprecated top-level prices still counts identical', async () => {
    const r = row()
    const noisy = converged(r, {
      id: 'totally-different-id',
      createdAt: '2020-01-01T00:00:00.000Z',
      updatedAt: '2020-01-01T00:00:00.000Z',
      unit: 'CHARACTERS', // echoed differently server-side; not part of the comparison
      prices: { input: 999, output: 999 }, // deprecated flattened view, deliberately wrong
    })
    const { fetchFn, calls } = makeTransport([noisy])
    const result = await applyPrices([r], cfg, { fetchFn })
    expect(calls.filter(c => c.method !== 'GET')).toHaveLength(0)
    expect(result.unchanged).toBe(1)
  })
})

describe('managed rows', () => {
  test('only project-scoped rows considered; a managed row matching the name is never DELETEd', async () => {
    const r = row()
    const managed = converged(r, { id: 'managed-1', isLangfuseManaged: true })
    const { fetchFn, calls } = makeTransport([managed])
    const result = await applyPrices([r], cfg, { fetchFn })
    const writes = calls.filter(c => c.method !== 'GET')
    // No override exists (the only row is managed), so apply creates one; it must
    // never DELETE the managed row.
    expect(writes.some(c => c.method === 'DELETE')).toBe(false)
    expect(result.created).toBe(1)
  })
})

describe('pagination', () => {
  test('a two-page fake GET is walked to completion', async () => {
    const r1 = row({ modelName: 'gpt-5.5', sourceId: 'openai/gpt-5.5' })
    const r2 = row({ modelName: 'gpt-5.6-luna', sourceId: 'openai/gpt-5.6-luna' })
    const existing = [converged(r1), converged(r2)]
    const { fetchFn, calls } = makeTransport(existing, { pageSize: 1 })
    const result = await applyPrices([r1, r2], cfg, { fetchFn })
    expect(calls.filter(c => c.method === 'GET').length).toBeGreaterThanOrEqual(2)
    expect(result.unchanged).toBe(2)
  })
})

describe('stray rows', () => {
  test('a project-scoped row not in the file is warned, never deleted', async () => {
    const r = row()
    const stray = converged(row({ modelName: 'orphaned-model', sourceId: 'openai/orphaned-model' }))
    const { fetchFn, calls } = makeTransport([converged(r), stray])
    const result = await applyPrices([r], cfg, { fetchFn })
    expect(calls.some(c => c.method === 'DELETE')).toBe(false)
    expect(result.warnings.some(w => w.includes('orphaned-model'))).toBe(true)
  })
})

describe('HTTP failure', () => {
  test('any transport failure rejects', async () => {
    const r = row()
    const fetchFn: typeof fetch = async () => new Response('boom', { status: 500 })
    await expect(applyPrices([r], cfg, { fetchFn })).rejects.toThrow()
  })
})

describe('negative contract: apply never reads upstream', () => {
  test('a fake transport that only serves the Langfuse host completes with no throw', async () => {
    const r = row()
    const { fetchFn } = makeTransport([converged(r)])
    // makeTransport itself throws if a non-Langfuse URL is requested (see above);
    // applyPrices completing without throwing IS the negative-contract proof.
    await expect(applyPrices([r], cfg, { fetchFn })).resolves.toBeDefined()
  })
})
