/* eslint-disable @typescript-eslint/no-explicit-any */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'

const registerJob = vi.fn()

vi.mock('@/components/providers/rematch-jobs-provider', () => ({
  useRegisterRematchJob: () => registerJob,
}))
vi.mock('@/lib/contacts-api', () => ({
  contactsApi: {
    updateContact: vi.fn(),
    applyMethodOperations: vi.fn(),
  },
}))
vi.mock('@/lib/query-invalidation', async () => {
  const actual = await vi.importActual<any>('@/lib/query-invalidation')
  return { ...actual, invalidateFor: vi.fn() }
})

import { useUpdateContact, useApplyMethodOperations } from '@/hooks/use-contacts'
import { contactsApi } from '@/lib/contacts-api'

const CONTACT_ID = '11111111-0000-4000-8000-000000000000'
const JOB_ID = '99999999-0000-4000-8000-000000000009'

let queryClient: QueryClient

function wrapper({ children }: { children: ReactNode }) {
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
}

beforeEach(() => {
  vi.clearAllMocks()
  queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
})

// Once the PUT's method branch is gone, a rematch can only be triggered by the
// operations endpoint: a rematch fires on newly-present method VALUES, and the
// scalar PUT no longer writes any. A registration left on the PUT would be a
// contract that can never fire.
describe('rematch registration follows the methods', () => {
  it('registers the job from the methods response', async () => {
    ;(contactsApi.applyMethodOperations as any).mockResolvedValue({
      methods: [],
      results: [],
      rematch_job_id: JOB_ID,
    })

    const { result } = renderHook(() => useApplyMethodOperations(), { wrapper })
    await result.current.mutateAsync({
      id: CONTACT_ID,
      operations: [{ op: 'add', type: 'email', value: 'new@example.test' }],
    })

    await waitFor(() =>
      expect(registerJob).toHaveBeenCalledWith({ jobId: JOB_ID, contactId: CONTACT_ID })
    )
  })

  it('does not register a job from the scalar PUT', async () => {
    // Seeded with a rematch_job_id the update path must ignore: if the hook
    // still read the field, this would register and the assertion would fail.
    ;(contactsApi.updateContact as any).mockResolvedValue({
      id: CONTACT_ID,
      full_name: 'Test Person',
      rematch_job_id: JOB_ID,
    })

    const { result } = renderHook(() => useUpdateContact(), { wrapper })
    await result.current.mutateAsync({ id: CONTACT_ID, data: { full_name: 'Test Person' } })

    expect(registerJob).not.toHaveBeenCalled()
  })

  it('leaves the methods request off entirely when it has no operations to send', async () => {
    // Guards the API surface rather than the caller: an operations array is the
    // only thing this client may send, and it must reach the endpoint verbatim.
    ;(contactsApi.applyMethodOperations as any).mockResolvedValue({ methods: [], results: [] })

    const { result } = renderHook(() => useApplyMethodOperations(), { wrapper })
    const operations = [{ op: 'remove' as const, method_id: 'x' }]
    await result.current.mutateAsync({ id: CONTACT_ID, operations })

    expect(contactsApi.applyMethodOperations).toHaveBeenCalledWith(CONTACT_ID, operations)
    expect(registerJob).not.toHaveBeenCalled()
  })
})

describe('the detail cache', () => {
  it('is still written from the scalar PUT response', async () => {
    // Retiring the methods branch must not take the detail-cache refresh with
    // it: the detail view reads scalars from this cache, and the edit form
    // closing onto a stale name would look like the save had not happened.
    const updated = { id: CONTACT_ID, full_name: 'Renamed Person' }
    ;(contactsApi.updateContact as any).mockResolvedValue(updated)

    const { result } = renderHook(() => useUpdateContact(), { wrapper })
    await result.current.mutateAsync({ id: CONTACT_ID, data: { full_name: 'Renamed Person' } })

    await waitFor(() =>
      expect(queryClient.getQueryData(['contacts', 'detail', CONTACT_ID])).toEqual(updated)
    )
  })

  it('takes its methods from the operations response', async () => {
    const methods = [{ id: 'm1', type: 'email', value: 'a@example.test', is_primary: false }]
    queryClient.setQueryData(['contacts', 'detail', CONTACT_ID], {
      id: CONTACT_ID,
      full_name: 'Test Person',
      methods: [],
    })
    ;(contactsApi.applyMethodOperations as any).mockResolvedValue({ methods, results: [] })

    const { result } = renderHook(() => useApplyMethodOperations(), { wrapper })
    await result.current.mutateAsync({
      id: CONTACT_ID,
      operations: [{ op: 'add', type: 'email', value: 'a@example.test' }],
    })

    await waitFor(() =>
      expect((queryClient.getQueryData(['contacts', 'detail', CONTACT_ID]) as any).methods).toEqual(
        methods
      )
    )
  })
})
