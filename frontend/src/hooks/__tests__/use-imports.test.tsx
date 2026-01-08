/* eslint-disable @typescript-eslint/no-explicit-any */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import {
  useImportCandidates,
  useImportCandidate,
  useImportAsContact,
  useLinkCandidate,
  useIgnoreCandidate,
  useTriggerSync,
} from '../use-imports'

// Mock the imports API
vi.mock('@/lib/imports-api', () => ({
  importsApi: {
    getCandidates: vi.fn(),
    getCandidate: vi.fn(),
    importCandidate: vi.fn(),
    linkCandidate: vi.fn(),
    ignoreCandidate: vi.fn(),
    triggerSync: vi.fn(),
  },
}))

// Mock the query invalidation
vi.mock('@/lib/query-invalidation', () => ({
  invalidateFor: vi.fn(),
  contactKeys: {
    all: ['contacts'] as const,
    lists: () => ['contacts', 'list'] as const,
    list: (params: any) => ['contacts', 'list', params] as const,
    details: () => ['contacts', 'detail'] as const,
    detail: (id: string) => ['contacts', 'detail', id] as const,
  },
  importKeys: {
    all: ['imports'] as const,
    lists: () => ['imports', 'list'] as const,
    list: (params: any) => ['imports', 'list', params] as const,
    details: () => ['imports', 'detail'] as const,
    detail: (id: string) => ['imports', 'detail', id] as const,
  },
}))

// Import mocked modules
import { importsApi } from '@/lib/imports-api'
import { invalidateFor } from '@/lib/query-invalidation'

const mockedImportsApi = importsApi as {
  getCandidates: ReturnType<typeof vi.fn>
  getCandidate: ReturnType<typeof vi.fn>
  importCandidate: ReturnType<typeof vi.fn>
  linkCandidate: ReturnType<typeof vi.fn>
  ignoreCandidate: ReturnType<typeof vi.fn>
  triggerSync: ReturnType<typeof vi.fn>
}

const mockedInvalidateFor = invalidateFor as ReturnType<typeof vi.fn>

// Create a fresh QueryClient for each test
function createTestQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: {
        retry: false,
        gcTime: 0,
      },
      mutations: {
        retry: false,
      },
    },
  })
}

// Wrapper component for rendering hooks with QueryClient
function createWrapper(queryClient: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  }
}

describe('use-imports hooks', () => {
  let queryClient: QueryClient

  beforeEach(() => {
    queryClient = createTestQueryClient()
    vi.clearAllMocks()
  })

  afterEach(() => {
    queryClient.clear()
  })

  describe('useImportCandidates', () => {
    it('fetches import candidates with default params', async () => {
      const mockData = {
        candidates: [{ id: 'ext-1', display_name: 'John Doe', emails: [], phones: [] }],
        total: 1,
        page: 1,
        limit: 20,
        pages: 1,
      }
      mockedImportsApi.getCandidates.mockResolvedValueOnce(mockData)

      const { result } = renderHook(() => useImportCandidates(), {
        wrapper: createWrapper(queryClient),
      })

      await waitFor(() => expect(result.current.isSuccess).toBe(true))

      expect(mockedImportsApi.getCandidates).toHaveBeenCalledWith({})
      expect(result.current.data).toEqual(mockData)
    })

    it('fetches import candidates with custom params', async () => {
      const mockData = {
        candidates: [],
        total: 0,
        page: 2,
        limit: 10,
        pages: 0,
      }
      mockedImportsApi.getCandidates.mockResolvedValueOnce(mockData)

      const { result } = renderHook(() => useImportCandidates({ page: 2, limit: 10 }), {
        wrapper: createWrapper(queryClient),
      })

      await waitFor(() => expect(result.current.isSuccess).toBe(true))

      expect(mockedImportsApi.getCandidates).toHaveBeenCalledWith({ page: 2, limit: 10 })
    })

    it('fetches import candidates filtered by source', async () => {
      const mockData = {
        candidates: [
          {
            id: 'ext-1',
            source: 'gcontacts',
            display_name: 'Google Contact',
            emails: [],
            phones: [],
          },
        ],
        total: 1,
        page: 1,
        limit: 20,
        pages: 1,
      }
      mockedImportsApi.getCandidates.mockResolvedValueOnce(mockData)

      const { result } = renderHook(() => useImportCandidates({ source: 'gcontacts' }), {
        wrapper: createWrapper(queryClient),
      })

      await waitFor(() => expect(result.current.isSuccess).toBe(true))

      expect(mockedImportsApi.getCandidates).toHaveBeenCalledWith({ source: 'gcontacts' })
      expect(result.current.data?.candidates[0].source).toBe('gcontacts')
    })

    it('fetches calendar attendees when filtered by gcal_attendee source', async () => {
      const mockData = {
        candidates: [
          {
            id: 'ext-2',
            source: 'gcal_attendee',
            display_name: 'Calendar Attendee',
            emails: ['attendee@example.com'],
            phones: [],
            metadata: {
              meeting_title: 'Team Meeting',
              meeting_date: '2026-01-08',
              meeting_link: 'https://calendar.google.com/event/123',
            },
          },
        ],
        total: 1,
        page: 1,
        limit: 20,
        pages: 1,
      }
      mockedImportsApi.getCandidates.mockResolvedValueOnce(mockData)

      const { result } = renderHook(() => useImportCandidates({ source: 'gcal_attendee' }), {
        wrapper: createWrapper(queryClient),
      })

      await waitFor(() => expect(result.current.isSuccess).toBe(true))

      expect(mockedImportsApi.getCandidates).toHaveBeenCalledWith({ source: 'gcal_attendee' })
      expect(result.current.data?.candidates[0].source).toBe('gcal_attendee')
      expect(result.current.data?.candidates[0].metadata?.meeting_title).toBe('Team Meeting')
    })

    it('correctly parses candidates with suggested_match field', async () => {
      const mockData = {
        candidates: [
          {
            id: 'ext-1',
            display_name: 'John Doe',
            emails: ['john@example.com'],
            phones: [],
            suggested_match: {
              contact_id: 'crm-123',
              contact_name: 'John Smith',
              confidence: 0.85,
            },
          },
          {
            id: 'ext-2',
            display_name: 'Jane Doe',
            emails: [],
            phones: [],
            // No suggested match
          },
        ],
        total: 2,
        page: 1,
        limit: 20,
        pages: 1,
      }
      mockedImportsApi.getCandidates.mockResolvedValueOnce(mockData)

      const { result } = renderHook(() => useImportCandidates(), {
        wrapper: createWrapper(queryClient),
      })

      await waitFor(() => expect(result.current.isSuccess).toBe(true))

      expect(result.current.data).toEqual(mockData)
      expect(result.current.data?.candidates[0].suggested_match).toBeDefined()
      expect(result.current.data?.candidates[0].suggested_match?.contact_id).toBe('crm-123')
      expect(result.current.data?.candidates[0].suggested_match?.contact_name).toBe('John Smith')
      expect(result.current.data?.candidates[0].suggested_match?.confidence).toBe(0.85)
      expect(result.current.data?.candidates[1].suggested_match).toBeUndefined()
    })

    it('handles suggested_match with different confidence levels', async () => {
      const mockData = {
        candidates: [
          {
            id: 'ext-1',
            display_name: 'High Confidence Match',
            emails: [],
            phones: [],
            suggested_match: {
              contact_id: 'crm-1',
              contact_name: 'Contact 1',
              confidence: 0.95,
            },
          },
          {
            id: 'ext-2',
            display_name: 'Low Confidence Match',
            emails: [],
            phones: [],
            suggested_match: {
              contact_id: 'crm-2',
              contact_name: 'Contact 2',
              confidence: 0.51,
            },
          },
        ],
        total: 2,
        page: 1,
        limit: 20,
        pages: 1,
      }
      mockedImportsApi.getCandidates.mockResolvedValueOnce(mockData)

      const { result } = renderHook(() => useImportCandidates(), {
        wrapper: createWrapper(queryClient),
      })

      await waitFor(() => expect(result.current.isSuccess).toBe(true))

      const candidates = result.current.data?.candidates
      expect(candidates?.[0].suggested_match?.confidence).toBeGreaterThan(0.9)
      expect(candidates?.[1].suggested_match?.confidence).toBeGreaterThan(0.5)
      expect(candidates?.[1].suggested_match?.confidence).toBeLessThan(0.6)
    })
  })

  describe('useImportCandidate', () => {
    it('fetches single import candidate by id', async () => {
      const mockCandidate = {
        id: 'ext-123',
        display_name: 'Jane Smith',
        emails: ['jane@example.com'],
        phones: [],
      }
      mockedImportsApi.getCandidate.mockResolvedValueOnce(mockCandidate)

      const { result } = renderHook(() => useImportCandidate('ext-123'), {
        wrapper: createWrapper(queryClient),
      })

      await waitFor(() => expect(result.current.isSuccess).toBe(true))

      expect(mockedImportsApi.getCandidate).toHaveBeenCalledWith('ext-123')
      expect(result.current.data).toEqual(mockCandidate)
    })

    it('does not fetch when id is empty', () => {
      renderHook(() => useImportCandidate(''), {
        wrapper: createWrapper(queryClient),
      })

      expect(mockedImportsApi.getCandidate).not.toHaveBeenCalled()
    })
  })

  describe('useImportAsContact', () => {
    it('imports candidate and invalidates queries on success', async () => {
      const mockContact = { id: 'crm-456', full_name: 'John Doe' }
      mockedImportsApi.importCandidate.mockResolvedValueOnce(mockContact)

      const { result } = renderHook(() => useImportAsContact(), {
        wrapper: createWrapper(queryClient),
      })

      await result.current.mutateAsync('ext-123')

      expect(mockedImportsApi.importCandidate).toHaveBeenCalledWith('ext-123')
      expect(mockedInvalidateFor).toHaveBeenCalledWith('import:imported')
    })

    it('populates contact detail cache on success', async () => {
      const mockContact = { id: 'crm-456', full_name: 'John Doe' }
      mockedImportsApi.importCandidate.mockResolvedValueOnce(mockContact)

      const { result } = renderHook(() => useImportAsContact(), {
        wrapper: createWrapper(queryClient),
      })

      await result.current.mutateAsync('ext-123')

      // Check that the contact was cached
      const cachedContact = queryClient.getQueryData(['contacts', 'detail', 'crm-456'])
      expect(cachedContact).toEqual(mockContact)
    })
  })

  describe('useLinkCandidate', () => {
    it('links candidate and invalidates queries on success', async () => {
      mockedImportsApi.linkCandidate.mockResolvedValueOnce(undefined)

      const { result } = renderHook(() => useLinkCandidate(), {
        wrapper: createWrapper(queryClient),
      })

      await result.current.mutateAsync({ id: 'ext-123', crmContactId: 'crm-456' })

      expect(mockedImportsApi.linkCandidate).toHaveBeenCalledWith('ext-123', 'crm-456')
      expect(mockedInvalidateFor).toHaveBeenCalledWith('import:linked')
    })
  })

  describe('useIgnoreCandidate', () => {
    it('ignores candidate and invalidates queries on success', async () => {
      mockedImportsApi.ignoreCandidate.mockResolvedValueOnce(undefined)

      const { result } = renderHook(() => useIgnoreCandidate(), {
        wrapper: createWrapper(queryClient),
      })

      await result.current.mutateAsync('ext-123')

      expect(mockedImportsApi.ignoreCandidate).toHaveBeenCalledWith('ext-123')
      expect(mockedInvalidateFor).toHaveBeenCalledWith('import:ignored')
    })
  })

  describe('useTriggerSync', () => {
    it('triggers sync with default source and invalidates on success', async () => {
      mockedImportsApi.triggerSync.mockResolvedValueOnce(undefined)

      const { result } = renderHook(() => useTriggerSync(), {
        wrapper: createWrapper(queryClient),
      })

      await result.current.mutateAsync({ source: 'gcontacts' })

      expect(mockedImportsApi.triggerSync).toHaveBeenCalledWith('gcontacts', undefined)
      expect(mockedInvalidateFor).toHaveBeenCalledWith('import:synced')
    })

    it('triggers sync with custom source', async () => {
      mockedImportsApi.triggerSync.mockResolvedValueOnce(undefined)

      const { result } = renderHook(() => useTriggerSync(), {
        wrapper: createWrapper(queryClient),
      })

      await result.current.mutateAsync({ source: 'icloud' })

      expect(mockedImportsApi.triggerSync).toHaveBeenCalledWith('icloud', undefined)
    })

    it('triggers sync with accountId', async () => {
      mockedImportsApi.triggerSync.mockResolvedValueOnce(undefined)

      const { result } = renderHook(() => useTriggerSync(), {
        wrapper: createWrapper(queryClient),
      })

      await result.current.mutateAsync({ source: 'gcontacts', accountId: 'account-123' })

      expect(mockedImportsApi.triggerSync).toHaveBeenCalledWith('gcontacts', 'account-123')
    })
  })
})
