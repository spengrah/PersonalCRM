import { describe, it, expect, vi, beforeEach } from 'vitest'
import { act, renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'

vi.mock('@/lib/bench-api', () => ({
  benchApi: {},
  threadsApi: { labelFlag: vi.fn(), labelClearing: vi.fn(), close: vi.fn() },
}))

import { saveItem, useCloseThread, useLabelFlag, usePendingSaves } from '@/hooks/use-bench'
import { threadsApi } from '@/lib/bench-api'
import type { ThreadReviewResponse } from '@/types/generated/contact'

const THREAD = '11111111-0000-4000-8000-000000000001'
const RUN = '22222222-0000-4000-8000-000000000002'
const recorded = {} as ThreadReviewResponse

function wrapper({ children }: { children: ReactNode }) {
  return (
    <QueryClientProvider
      client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
    >
      {children}
    </QueryClientProvider>
  )
}

beforeEach(() => {
  vi.clearAllMocks()
})

// A close takes effect after every save made before it is recorded (SC9): the
// thread page relies on the queue to order its saves while its controls stay
// live.
describe('Threads saves', () => {
  it('are sent in the order made, a close after the saves before it, each pending save holding only its own controls', async () => {
    const sent: string[] = []
    const releases: (() => void)[] = []
    vi.mocked(threadsApi.labelFlag).mockImplementation(
      (_thread, _run, label) =>
        new Promise(resolve => {
          sent.push(`flag:${label.message_key}`)
          releases.push(() => resolve(recorded))
        })
    )
    vi.mocked(threadsApi.close).mockImplementation(async () => {
      sent.push('close')
      return recorded
    })
    const { result } = renderHook(
      () => ({ label: useLabelFlag(), close: useCloseThread(), pending: usePendingSaves() }),
      { wrapper }
    )
    const target = { threadId: THREAD, runId: RUN }

    act(() => {
      result.current.label.mutate({ ...target, messageKey: 'a', kind: 'claim', holds: true })
      result.current.label.mutate({ ...target, messageKey: 'b', kind: 'claim', holds: false })
      result.current.close.mutate(target)
    })

    await waitFor(() => expect(sent).toEqual(['flag:a']))
    expect(result.current.pending(saveItem.flag('a', 'claim'))).toBe(true)
    expect(result.current.pending(saveItem.flag('b', 'claim'))).toBe(true)
    expect(result.current.pending(saveItem.flag('c', 'claim'))).toBe(false)
    expect(result.current.pending(saveItem.flag('a', 'commitment'))).toBe(false)

    act(() => releases[0]())
    await waitFor(() => expect(sent).toEqual(['flag:a', 'flag:b']))
    await waitFor(() => expect(result.current.pending(saveItem.flag('a', 'claim'))).toBe(false))
    expect(result.current.pending(saveItem.flag('b', 'claim'))).toBe(true)

    act(() => releases[1]())
    await waitFor(() => expect(sent).toEqual(['flag:a', 'flag:b', 'close']))
  })
})
