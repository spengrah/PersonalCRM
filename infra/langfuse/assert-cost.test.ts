import { describe, expect, test } from 'bun:test'
import { assertCost } from './assert-cost'
import type { LangfuseConfig } from './http'

const cfg: LangfuseConfig = { host: 'https://fake-langfuse.test', publicKey: 'pk', secretKey: 'sk' }
const FROM = '2026-08-09T00:00:00.000Z'

function transportFor(rows: Array<Record<string, unknown>>): typeof fetch {
  return async (input, init) => {
    const url = new URL(String(input))
    if (`${url.protocol}//${url.host}` !== cfg.host) {
      throw new Error(`unexpected host: ${url.href}`)
    }
    expect(init?.method ?? 'GET').toBe('GET')
    expect(url.pathname).toBe('/api/public/observations')
    expect(url.searchParams.get('type')).toBe('GENERATION')
    expect(url.searchParams.get('fromStartTime')).toBe(FROM)
    return new Response(
      JSON.stringify({ data: rows, meta: { page: 1, limit: 100, totalItems: rows.length, totalPages: 1 } }),
      { status: 200 }
    )
  }
}

describe('assertCost', () => {
  test('one or more GENERATION observations, all non-zero cost -> ok', async () => {
    const fetchFn = transportFor([
      { model: 'gpt-5.5', costDetails: { total: 0.0042 } },
      { model: 'gpt-5.6-luna', calculatedTotalCost: 0.0001 },
    ])
    const result = await assertCost(FROM, cfg, { fetchFn })
    expect(result.ok).toBe(true)
  })

  test('an observation with zero/null cost -> not ok, names the model string', async () => {
    const fetchFn = transportFor([
      { model: 'gpt-5.5', costDetails: { total: 0.0042 } },
      { model: 'gpt-5.6-terra', costDetails: { total: 0 } },
    ])
    const result = await assertCost(FROM, cfg, { fetchFn })
    expect(result.ok).toBe(false)
    expect(result.message).toContain('gpt-5.6-terra')
  })

  test('an observation with null cost -> not ok, names the model string', async () => {
    const fetchFn = transportFor([{ model: 'gpt-5.4-mini', costDetails: null, calculatedTotalCost: null }])
    const result = await assertCost(FROM, cfg, { fetchFn })
    expect(result.ok).toBe(false)
    expect(result.message).toContain('gpt-5.4-mini')
  })

  test('zero observations found -> not ok, distinct "nothing to assert" message', async () => {
    const fetchFn = transportFor([])
    const result = await assertCost(FROM, cfg, { fetchFn, retries: 0 })
    expect(result.ok).toBe(false)
    expect(result.message).toMatch(/no .*observations|nothing to assert/i)
  })
})

// Langfuse materializes exported observations asynchronously; only the
// zero-OBSERVATIONS case retries — a zero-COST observation is the signal itself.
describe('ingestion-lag retry', () => {
  function sequencedTransport(pages: Array<Array<Record<string, unknown>>>) {
    let call = 0
    const fetchFn: typeof fetch = async () => {
      const rows = pages[Math.min(call++, pages.length - 1)]
      return new Response(
        JSON.stringify({ data: rows, meta: { page: 1, limit: 100, totalItems: rows.length, totalPages: 1 } }),
        { status: 200 }
      )
    }
    return { fetchFn, callCount: () => call }
  }

  test('zero observations retries on the bounded schedule, then succeeds', async () => {
    const { fetchFn, callCount } = sequencedTransport([[], [], [{ model: 'gpt-5.5', costDetails: { total: 0.01 } }]])
    const sleeps: number[] = []
    const result = await assertCost(FROM, cfg, {
      fetchFn,
      retries: 3,
      retryDelayMs: 5,
      sleep: async ms => void sleeps.push(ms),
    })
    expect(result.ok).toBe(true)
    expect(callCount()).toBe(3)
    expect(sleeps).toEqual([5, 5])
  })

  test('still zero after all retries -> nothing to assert', async () => {
    const { fetchFn, callCount } = sequencedTransport([[]])
    const sleeps: number[] = []
    const result = await assertCost(FROM, cfg, { fetchFn, retries: 2, retryDelayMs: 1, sleep: async ms => void sleeps.push(ms) })
    expect(result.ok).toBe(false)
    expect(result.message).toMatch(/nothing to assert/i)
    expect(callCount()).toBe(3)
    expect(sleeps).toHaveLength(2)
  })

  test('a zero-cost observation is reported immediately, never retried', async () => {
    const { fetchFn, callCount } = sequencedTransport([[{ model: 'gpt-5.5', costDetails: { total: 0 } }]])
    const result = await assertCost(FROM, cfg, { fetchFn, retries: 3, retryDelayMs: 1, sleep: async () => {} })
    expect(result.ok).toBe(false)
    expect(callCount()).toBe(1)
  })
})
