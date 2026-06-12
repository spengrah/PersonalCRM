import { describe, it, expect, vi, beforeEach } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { useSyncStaleness } from '../use-sync-staleness'
import type { StalenessBreach } from '@/types/sync'

vi.mock('@/lib/sync-api', () => ({
  syncApi: {
    getStalenessBreaches: vi.fn(),
  },
}))

import { syncApi } from '@/lib/sync-api'

const mockedApi = syncApi as unknown as {
  getStalenessBreaches: ReturnType<typeof vi.fn>
}

function createTestQueryClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } })
}

function wrapper(client: QueryClient) {
  // eslint-disable-next-line react/display-name
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  )
}

function createBreach(overrides: Partial<StalenessBreach> = {}): StalenessBreach {
  return {
    id: 'breach-1',
    source: 'messages',
    account_id: 'host-1',
    breach_type: 'push_stale',
    stale_since: '2026-06-01T00:00:00Z',
    threshold_seconds: 172800,
    details: 'no push for 3d2h (threshold 48h)',
    detected_at: '2026-06-04T00:00:00Z',
    last_observed_at: '2026-06-04T00:00:00Z',
    ...overrides,
  }
}

beforeEach(() => {
  vi.clearAllMocks()
})

describe('useSyncStaleness', () => {
  it('exposes the breaches returned by the api', async () => {
    const breaches = [createBreach()]
    mockedApi.getStalenessBreaches.mockResolvedValue(breaches)
    const client = createTestQueryClient()
    const { result } = renderHook(() => useSyncStaleness(), { wrapper: wrapper(client) })

    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toEqual(breaches)
    expect(mockedApi.getStalenessBreaches).toHaveBeenCalledOnce()
  })

  it('exposes an empty list when there are no breaches', async () => {
    mockedApi.getStalenessBreaches.mockResolvedValue([])
    const client = createTestQueryClient()
    const { result } = renderHook(() => useSyncStaleness(), { wrapper: wrapper(client) })

    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toEqual([])
  })

  it('surfaces the error state when the api rejects', async () => {
    mockedApi.getStalenessBreaches.mockRejectedValue(new Error('boom'))
    const client = createTestQueryClient()
    const { result } = renderHook(() => useSyncStaleness(), { wrapper: wrapper(client) })

    await waitFor(() => expect(result.current.isError).toBe(true))
    expect(result.current.data).toBeUndefined()
  })
})
