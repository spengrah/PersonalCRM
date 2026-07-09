import { describe, it, expect } from 'vitest'
import { assertNonProductionTarget, PROD_OR_UNKNOWN } from './prod-guard'

// A minimal Response-shaped stub for the injected fetch.
function fakeFetch(opts: {
  ok?: boolean
  status?: number
  environment?: string | undefined
  throwErr?: boolean
  onUrl?: (url: string) => void
}): typeof fetch {
  return (async (input: string | URL | Request) => {
    if (opts.onUrl) opts.onUrl(String(input))
    if (opts.throwErr) throw new Error('network down')
    return {
      ok: opts.ok ?? true,
      status: opts.status ?? 200,
      json: async () => ({ data: { environment: opts.environment } }),
    }
  }) as unknown as typeof fetch
}

describe('assertNonProductionTarget', () => {
  it('passes for a verified non-production environment (testing)', async () => {
    await expect(
      assertNonProductionTarget({
        apiBase: 'http://localhost:8080',
        apiKey: 'k',
        fetchImpl: fakeFetch({ environment: 'testing' }),
      })
    ).resolves.toBeUndefined()
  })

  it('refuses each production / empty / unknown alias', async () => {
    for (const env of ['production', 'prod', '', 'unknown']) {
      await expect(
        assertNonProductionTarget({
          apiBase: 'http://x',
          apiKey: 'k',
          fetchImpl: fakeFetch({ environment: env }),
        }),
        `environment='${env}' must refuse`
      ).rejects.toThrow(/REFUSING/)
    }
  })

  it('refuses a missing environment field (fail-closed)', async () => {
    await expect(
      assertNonProductionTarget({
        apiBase: 'http://x',
        apiKey: 'k',
        fetchImpl: fakeFetch({ environment: undefined }),
      })
    ).rejects.toThrow(/REFUSING/)
  })

  it('refuses when the probe fetch throws (fail-closed)', async () => {
    await expect(
      assertNonProductionTarget({
        apiBase: 'http://x',
        apiKey: 'k',
        fetchImpl: fakeFetch({ throwErr: true }),
      })
    ).rejects.toThrow(/could not verify/)
  })

  it('refuses on a non-OK response (fail-closed)', async () => {
    await expect(
      assertNonProductionTarget({
        apiBase: 'http://x',
        apiKey: 'k',
        fetchImpl: fakeFetch({ ok: false, status: 503 }),
      })
    ).rejects.toThrow(/could not verify/)
  })

  it('probes the API base (split-origin: API URL differs from the frontend base)', async () => {
    // The guard must check where CRM_ENV is truthful — the API host — not the
    // frontend origin. We assert the probed URL is the API base, not some other.
    let probed = ''
    await assertNonProductionTarget({
      apiBase: 'http://api-host:8080',
      apiKey: 'k',
      fetchImpl: fakeFetch({ environment: 'testing', onUrl: u => (probed = u) }),
    })
    expect(probed).toBe('http://api-host:8080/api/v1/system/time')
  })

  it('deny-set covers the backend production aliases', () => {
    // Keep in lockstep with config.IsProductionCRMEnv on the backend.
    expect(PROD_OR_UNKNOWN.has('production')).toBe(true)
    expect(PROD_OR_UNKNOWN.has('prod')).toBe(true)
    expect(PROD_OR_UNKNOWN.has('')).toBe(true)
    expect(PROD_OR_UNKNOWN.has('unknown')).toBe(true)
    expect(PROD_OR_UNKNOWN.has('testing')).toBe(false)
  })
})
