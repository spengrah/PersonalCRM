/* eslint-disable @typescript-eslint/no-explicit-any */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import {
  useSuggestions,
  useResolveMethodSuggestions,
  useDismissMethodSuggestions,
} from '../use-suggestions'

vi.mock('@/lib/imports-api', () => ({
  importsApi: {
    getSuggestions: vi.fn(),
    resolveMethodSuggestions: vi.fn(),
    dismissMethodSuggestions: vi.fn(),
  },
}))

vi.mock('@/lib/query-invalidation', () => ({
  invalidateFor: vi.fn(),
  importKeys: {
    all: ['imports'] as const,
    suggestionsLists: () => ['imports', 'suggestions'] as const,
    suggestions: (params: any) => ['imports', 'suggestions', params] as const,
  },
}))

const mockRegisterJob = vi.fn()
vi.mock('@/components/providers/rematch-jobs-provider', () => ({
  useRegisterRematchJob: () => mockRegisterJob,
}))

import { importsApi } from '@/lib/imports-api'
import { invalidateFor } from '@/lib/query-invalidation'

const mockedApi = importsApi as unknown as {
  getSuggestions: ReturnType<typeof vi.fn>
  resolveMethodSuggestions: ReturnType<typeof vi.fn>
  dismissMethodSuggestions: ReturnType<typeof vi.fn>
}
const mockedInvalidateFor = invalidateFor as ReturnType<typeof vi.fn>

function createTestQueryClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } })
}

function wrapper(client: QueryClient) {
  // eslint-disable-next-line react/display-name
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  )
}

beforeEach(() => {
  vi.clearAllMocks()
})

describe('useSuggestions', () => {
  it('fetches the suggestions list with the given params', async () => {
    mockedApi.getSuggestions.mockResolvedValue({
      items: [],
      total: 0,
      page: 1,
      limit: 20,
      pages: 0,
    })
    const client = createTestQueryClient()
    const { result } = renderHook(() => useSuggestions({ source: 'gcontacts', page: 1 }), {
      wrapper: wrapper(client),
    })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(mockedApi.getSuggestions).toHaveBeenCalledWith({ source: 'gcontacts', page: 1 })
  })
})

describe('useResolveMethodSuggestions', () => {
  it('invalidates and registers a rematch job on success', async () => {
    mockedApi.resolveMethodSuggestions.mockResolvedValue({
      external_contact_id: 'ext-1',
      contact_id: 'contact-1',
      resolved_count: 1,
      rematch_job_id: 'job-1',
    })
    const client = createTestQueryClient()
    const { result } = renderHook(() => useResolveMethodSuggestions(), {
      wrapper: wrapper(client),
    })
    await result.current.mutateAsync({
      id: 'ext-1',
      request: { methods: [{ type: 'email', value: 'a@example.test' }] },
    })

    expect(mockedApi.resolveMethodSuggestions).toHaveBeenCalledWith('ext-1', {
      methods: [{ type: 'email', value: 'a@example.test' }],
    })
    expect(mockedInvalidateFor).toHaveBeenCalledWith('method-suggestion:resolved', 'contact-1')
    expect(mockRegisterJob).toHaveBeenCalledWith({
      jobId: 'job-1',
      contactId: 'contact-1',
      invalidateImports: true,
    })
  })

  it('does not register a rematch job when none is returned', async () => {
    mockedApi.resolveMethodSuggestions.mockResolvedValue({
      external_contact_id: 'ext-1',
      contact_id: 'contact-1',
      resolved_count: 0,
      rematch_job_id: null,
    })
    const client = createTestQueryClient()
    const { result } = renderHook(() => useResolveMethodSuggestions(), {
      wrapper: wrapper(client),
    })
    await result.current.mutateAsync({ id: 'ext-1', request: { methods: [] } })

    expect(mockedInvalidateFor).toHaveBeenCalledWith('method-suggestion:resolved', 'contact-1')
    expect(mockRegisterJob).not.toHaveBeenCalled()
  })
})

describe('useDismissMethodSuggestions', () => {
  it('invalidates the suggestions surface on success (no rematch)', async () => {
    mockedApi.dismissMethodSuggestions.mockResolvedValue({
      external_contact_id: 'ext-1',
      dismissed_count: 1,
    })
    const client = createTestQueryClient()
    const { result } = renderHook(() => useDismissMethodSuggestions(), {
      wrapper: wrapper(client),
    })
    await result.current.mutateAsync({ id: 'ext-1', request: {} })

    expect(mockedApi.dismissMethodSuggestions).toHaveBeenCalledWith('ext-1', {})
    expect(mockedInvalidateFor).toHaveBeenCalledWith('method-suggestion:dismissed')
    expect(mockRegisterJob).not.toHaveBeenCalled()
  })
})
