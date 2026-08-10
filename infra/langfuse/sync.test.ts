import { describe, expect, test } from 'bun:test'
import { syncRows, type CatalogFetchers } from './sync'
import type { ModelRow } from './schema'

function veniceCatalog(entries: Array<Record<string, unknown>>) {
  return { data: entries }
}
function openRouterCatalog(entries: Array<Record<string, unknown>>) {
  return { data: entries }
}

function fetchers(venice: unknown, openrouter: unknown): CatalogFetchers {
  return {
    fetchVenice: async () => venice,
    fetchOpenRouter: async () => openrouter,
  }
}

describe('venice unit conversion', () => {
  test('divides by 1e6, USD/M -> USD/token', async () => {
    const rows: ModelRow[] = [
      {
        modelName: 'my-venice-model',
        source: 'venice',
        sourceId: 'my-venice-model',
        prices: { input: 0, output: 0 },
      },
    ]
    const catalog = veniceCatalog([
      {
        id: 'my-venice-model',
        type: 'text',
        model_spec: { pricing: { input: { usd: 0.75 }, output: { usd: 3 } } },
      },
    ])
    const out = await syncRows(rows, fetchers(catalog, openRouterCatalog([])))
    expect(out[0]?.prices.input).toBe(7.5e-7)
    expect(out[0]?.prices.output).toBe(3e-6)
    // Magnitude guard: fails if the ÷1e6 conversion is ever dropped.
    expect(out[0]!.prices.input).toBeLessThan(1e-3)
    expect(out[0]!.prices.output).toBeLessThan(1e-3)
  })
})

describe('open-router unit conversion', () => {
  test('parses decimal-string per-token prices with no division', async () => {
    const rows: ModelRow[] = [
      {
        modelName: 'gpt-5.5',
        source: 'open-router',
        sourceId: 'openai/gpt-5.5',
        prices: { input: 0, output: 0 },
      },
    ]
    const catalog = openRouterCatalog([
      { id: 'openai/gpt-5.5', pricing: { prompt: '0.0000001', completion: '0.0000004' } },
    ])
    const out = await syncRows(rows, fetchers(veniceCatalog([]), catalog))
    expect(out[0]?.prices.input).toBe(1e-7)
    expect(out[0]?.prices.output).toBe(4e-7)
  })
})

describe('sync never adds or removes rows', () => {
  test('extra catalog entries are ignored; row count and identity unchanged', async () => {
    const rows: ModelRow[] = [
      { modelName: 'gpt-5.5', source: 'open-router', sourceId: 'openai/gpt-5.5', prices: { input: 0, output: 0 } },
    ]
    const extras = Array.from({ length: 40 }, (_, i) => ({
      id: `openai/extra-${i}`,
      pricing: { prompt: '0.000001', completion: '0.000002' },
    }))
    const catalog = openRouterCatalog([
      { id: 'openai/gpt-5.5', pricing: { prompt: '0.000005', completion: '0.00003' } },
      ...extras,
    ])
    const out = await syncRows(rows, fetchers(veniceCatalog([]), catalog))
    expect(out.length).toBe(1)
    expect(out[0]?.modelName).toBe('gpt-5.5')
    expect(out[0]?.sourceId).toBe('openai/gpt-5.5')
  })
})

describe('loud failure — declared row absent from catalog', () => {
  test('sourceId not found -> throws', async () => {
    const rows: ModelRow[] = [
      { modelName: 'gpt-9', source: 'open-router', sourceId: 'openai/gpt-9', prices: { input: 0, output: 0 } },
    ]
    await expect(syncRows(rows, fetchers(veniceCatalog([]), openRouterCatalog([])))).rejects.toThrow(
      /openai\/gpt-9/
    )
  })
})

describe('venice non-text model', () => {
  test('declared row maps to a non-text catalog entry -> throws', async () => {
    const rows: ModelRow[] = [
      { modelName: 'stable-diffusion', source: 'venice', sourceId: 'stable-diffusion', prices: { input: 0, output: 0 } },
    ]
    const catalog = veniceCatalog([
      {
        id: 'stable-diffusion',
        type: 'image',
        model_spec: { pricing: { input: { usd: 1 }, output: { usd: 1 } } },
      },
    ])
    await expect(syncRows(rows, fetchers(catalog, openRouterCatalog([])))).rejects.toThrow(/text model/)
  })
})

describe('cachedInput population', () => {
  test('open-router: populated from input_cache_read when present', async () => {
    const rows: ModelRow[] = [
      { modelName: 'gpt-5.5', source: 'open-router', sourceId: 'openai/gpt-5.5', prices: { input: 0, output: 0 } },
    ]
    const catalog = openRouterCatalog([
      {
        id: 'openai/gpt-5.5',
        pricing: { prompt: '0.000005', completion: '0.00003', input_cache_read: '0.0000005' },
      },
    ])
    const out = await syncRows(rows, fetchers(veniceCatalog([]), catalog))
    expect(out[0]?.prices.cachedInput).toBe(5e-7)
  })

  test('open-router: absent when the catalog entry has no input_cache_read', async () => {
    const rows: ModelRow[] = [
      { modelName: 'gpt-5.5', source: 'open-router', sourceId: 'openai/gpt-5.5', prices: { input: 0, output: 0 } },
    ]
    const catalog = openRouterCatalog([
      { id: 'openai/gpt-5.5', pricing: { prompt: '0.000005', completion: '0.00003' } },
    ])
    const out = await syncRows(rows, fetchers(veniceCatalog([]), catalog))
    expect(out[0]?.prices.cachedInput).toBeUndefined()
  })

  test('venice: populated from cache_input.usd/1e6 when present', async () => {
    const rows: ModelRow[] = [
      { modelName: 'v-model', source: 'venice', sourceId: 'v-model', prices: { input: 0, output: 0 } },
    ]
    const catalog = veniceCatalog([
      {
        id: 'v-model',
        type: 'text',
        model_spec: { pricing: { input: { usd: 1 }, output: { usd: 2 }, cache_input: { usd: 0.75 } } },
      },
    ])
    const out = await syncRows(rows, fetchers(catalog, openRouterCatalog([])))
    expect(out[0]?.prices.cachedInput).toBe(7.5e-7)
  })

  test('venice: absent when the catalog entry has no cache_input', async () => {
    const rows: ModelRow[] = [
      { modelName: 'v-model', source: 'venice', sourceId: 'v-model', prices: { input: 0, output: 0 } },
    ]
    const catalog = veniceCatalog([
      { id: 'v-model', type: 'text', model_spec: { pricing: { input: { usd: 1 }, output: { usd: 2 } } } },
    ])
    const out = await syncRows(rows, fetchers(catalog, openRouterCatalog([])))
    expect(out[0]?.prices.cachedInput).toBeUndefined()
  })
})

describe('byte-identical no-drift', () => {
  test('canonical file + unchanged fake quotes -> output bytes equal input bytes', async () => {
    const rows: ModelRow[] = [
      {
        modelName: 'gpt-5.5',
        source: 'open-router',
        sourceId: 'openai/gpt-5.5',
        prices: { input: 5e-6, output: 3e-5, cachedInput: 5e-7 },
      },
    ]
    const catalog = openRouterCatalog([
      {
        id: 'openai/gpt-5.5',
        pricing: { prompt: '0.000005', completion: '0.00003', input_cache_read: '0.0000005' },
      },
    ])
    const out = await syncRows(rows, fetchers(veniceCatalog([]), catalog))
    expect(out[0]?.prices).toEqual(rows[0]!.prices)
  })
})
