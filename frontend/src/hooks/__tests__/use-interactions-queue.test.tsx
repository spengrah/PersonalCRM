import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import {
  useInteractionsQueue,
  useResolveLink,
  useAnarlogTitleCandidates,
  useResolveNameCandidate,
} from '../use-interactions-queue'

vi.mock('@/lib/imports-api', () => ({
  importsApi: {
    getNeedsAttention: vi.fn(),
    resolveLink: vi.fn(),
    getAnarlogTitleGroups: vi.fn(),
    resolveNameCandidate: vi.fn(),
  },
}))

vi.mock('@/lib/query-invalidation', () => ({
  invalidateFor: vi.fn(),
  importKeys: {
    all: ['imports'] as const,
    needsAttention: () => ['imports', 'needs-attention'] as const,
    anarlogTitle: () => ['imports', 'anarlog-title'] as const,
  },
}))

import { importsApi } from '@/lib/imports-api'
import { invalidateFor } from '@/lib/query-invalidation'

const mockedApi = importsApi as unknown as {
  getNeedsAttention: ReturnType<typeof vi.fn>
  resolveLink: ReturnType<typeof vi.fn>
  getAnarlogTitleGroups: ReturnType<typeof vi.fn>
  resolveNameCandidate: ReturnType<typeof vi.fn>
}
const mockedInvalidateFor = invalidateFor as ReturnType<typeof vi.fn>

function createTestQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: 0 },
      mutations: { retry: false },
    },
  })
}

function createWrapper(queryClient: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  }
}

describe('use-interactions-queue hooks', () => {
  let queryClient: QueryClient

  beforeEach(() => {
    queryClient = createTestQueryClient()
    vi.clearAllMocks()
  })

  afterEach(() => {
    queryClient.clear()
  })

  it('useInteractionsQueue fetches the queue', async () => {
    const items = [{ id: 'mn-1', candidates: [] }]
    mockedApi.getNeedsAttention.mockResolvedValueOnce(items)

    const { result } = renderHook(() => useInteractionsQueue(), {
      wrapper: createWrapper(queryClient),
    })

    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toEqual(items)
  })

  it('useAnarlogTitleCandidates fetches grouped tokens', async () => {
    const groups = [
      { normalized_token: 'lena', token_display: 'Lena', evidence_count: 2, session_titles: [] },
    ]
    mockedApi.getAnarlogTitleGroups.mockResolvedValueOnce(groups)

    const { result } = renderHook(() => useAnarlogTitleCandidates(), {
      wrapper: createWrapper(queryClient),
    })

    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(result.current.data).toEqual(groups)
  })

  it('useResolveLink always invalidates static keys and once per affected contact', async () => {
    mockedApi.resolveLink.mockResolvedValueOnce({
      meeting_note: { id: 'mn-1' },
      interactions_created: [
        { id: 'ix-1', contact_id: 'c-1' },
        { id: 'ix-2', contact_id: 'c-1' }, // duplicate contact → dedup
        { id: 'ix-3', contact_id: 'c-2' },
      ],
    })

    const { result } = renderHook(() => useResolveLink(), {
      wrapper: createWrapper(queryClient),
    })
    result.current.mutate({ id: 'mn-1', request: { action: 'none_of_these' } })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))

    // Once unconditional (static keys) + once per DISTINCT contact (2).
    expect(mockedInvalidateFor).toHaveBeenCalledWith('meeting-note:resolved')
    expect(mockedInvalidateFor).toHaveBeenCalledWith('meeting-note:resolved', 'c-1')
    expect(mockedInvalidateFor).toHaveBeenCalledWith('meeting-note:resolved', 'c-2')
    expect(mockedInvalidateFor).toHaveBeenCalledTimes(3)
  })

  it('useResolveLink invalidates static keys even with zero interactions', async () => {
    mockedApi.resolveLink.mockResolvedValueOnce({
      meeting_note: { id: 'mn-1' },
      interactions_created: [],
    })

    const { result } = renderHook(() => useResolveLink(), {
      wrapper: createWrapper(queryClient),
    })
    result.current.mutate({
      id: 'mn-1',
      request: { action: 'link', kind: 'event', id: 'evt-1' },
    })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))

    expect(mockedInvalidateFor).toHaveBeenCalledWith('meeting-note:resolved')
    expect(mockedInvalidateFor).toHaveBeenCalledTimes(1)
  })

  it('useResolveNameCandidate passes the response contact_id for import/link', async () => {
    mockedApi.resolveNameCandidate.mockResolvedValueOnce({ action: 'import', contact_id: 'c-9' })

    const { result } = renderHook(() => useResolveNameCandidate(), {
      wrapper: createWrapper(queryClient),
    })
    result.current.mutate({ normalized_token: 'lena', action: 'import' })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))

    expect(mockedInvalidateFor).toHaveBeenCalledWith('name-candidate:resolved', 'c-9')
  })

  it('useResolveNameCandidate passes undefined contact_id for ignore', async () => {
    mockedApi.resolveNameCandidate.mockResolvedValueOnce({ action: 'ignore' })

    const { result } = renderHook(() => useResolveNameCandidate(), {
      wrapper: createWrapper(queryClient),
    })
    result.current.mutate({ normalized_token: 'lena', action: 'ignore' })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))

    expect(mockedInvalidateFor).toHaveBeenCalledWith('name-candidate:resolved', undefined)
  })
})
