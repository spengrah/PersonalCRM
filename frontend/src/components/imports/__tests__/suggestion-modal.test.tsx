/* eslint-disable @typescript-eslint/no-explicit-any */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

vi.mock('@/hooks/use-contacts', () => ({
  useContacts: vi.fn(),
  useContact: vi.fn(),
}))

vi.mock('@/hooks/use-imports', () => ({
  useImportAsContact: vi.fn(),
  useLinkCandidate: vi.fn(),
  useIgnoreCandidate: vi.fn(),
  useImportCandidates: vi.fn(),
}))

vi.mock('@/hooks/use-suggestions', () => ({
  useResolveMethodSuggestions: vi.fn(),
  useDismissMethodSuggestions: vi.fn(),
}))

import { SuggestionModal } from '../suggestion-modal'
import { useContacts, useContact } from '@/hooks/use-contacts'
import {
  useImportAsContact,
  useLinkCandidate,
  useIgnoreCandidate,
  useImportCandidates,
} from '@/hooks/use-imports'
import type { ImportCandidate } from '@/types/import'

const cand = (id: string, name: string): ImportCandidate => ({
  id,
  source: 'gcontacts',
  display_name: name,
  emails: [`${id}@example.test`],
  phones: [],
})

const A = cand('cand-a', 'Candidate A')
const B = cand('cand-b', 'Candidate B')
const C = cand('cand-c', 'Candidate C')
const X = cand('cand-x', 'Candidate X')
const Y = cand('cand-y', 'Candidate Y')

let refetchMock: ReturnType<typeof vi.fn>
let importAsync: ReturnType<typeof vi.fn>

/**
 * Mock the modal's 1000-limit query. `age: 'preOpen'` models a cached
 * response that predates the modal opening (dataUpdatedAt in the past);
 * the default `'postOpen'` models a fetch that completed after mount.
 */
function mockCandidatesQuery(list: ImportCandidate[], age: 'preOpen' | 'postOpen' = 'postOpen') {
  mockCandidatesQueryRaw({
    data: { candidates: list },
    dataUpdatedAt: age === 'preOpen' ? 1 : Date.now() + 60_000,
  })
}

/** Full control over the query result shape (error states, absent data, exact timestamps). */
function mockCandidatesQueryRaw(result: {
  data?: { candidates: ImportCandidate[] }
  isError?: boolean
  dataUpdatedAt: number
}) {
  vi.mocked(useImportCandidates).mockReturnValue({
    data: result.data,
    isError: result.isError ?? false,
    dataUpdatedAt: result.dataUpdatedAt,
    refetch: refetchMock,
  } as any)
}

beforeEach(() => {
  vi.clearAllMocks()
  // The mount effect chains .finally() on the refetch promise.
  refetchMock = vi.fn().mockResolvedValue(undefined)
  importAsync = vi.fn().mockResolvedValue({})
  vi.mocked(useContacts).mockReturnValue({ data: { contacts: [] } } as any)
  vi.mocked(useContact).mockReturnValue({ data: undefined } as any)
  const inertMutation = { mutateAsync: vi.fn().mockResolvedValue({}), isPending: false } as any
  vi.mocked(useImportAsContact).mockReturnValue({
    mutateAsync: importAsync,
    isPending: false,
  } as any)
  vi.mocked(useLinkCandidate).mockReturnValue(inertMutation)
  vi.mocked(useIgnoreCandidate).mockReturnValue(inertMutation)
})

function renderModal(
  candidates: ImportCandidate[],
  initialCandidateId: string,
  opts?: { initialMode?: 'import' | 'link' }
) {
  const item = { kind: 'contact' as const, candidates, initialCandidateId, ...opts }
  const props = { onClose: vi.fn(), onSuccess: vi.fn(), onError: vi.fn() }
  const view = render(<SuggestionModal item={item} {...props} />)
  return {
    ...view,
    props,
    rerenderModal: () => view.rerender(<SuggestionModal item={item} {...props} />),
  }
}

const heading = () => screen.getByRole('heading', { level: 3 })

describe('ContactCandidateResolver candidate tracking', () => {
  it('opens on the clicked candidate even when the fetched list is ordered differently', () => {
    // The page passes its (paginated) view of the list; the modal's own
    // 1000-limit fetch comes back in a different order.
    mockCandidatesQuery([C, A, B])
    renderModal([A, B, C], B.id)

    expect(heading()).toHaveTextContent('Candidate B')
    expect(screen.getByText('3 of 3')).toBeInTheDocument()
  })

  it('keeps following the candidate when the list reorders under the open modal', () => {
    mockCandidatesQuery([A, B, C])
    const { rerenderModal } = renderModal([A, B, C], B.id)
    expect(heading()).toHaveTextContent('Candidate B')
    expect(screen.getByText('2 of 3')).toBeInTheDocument()

    mockCandidatesQuery([B, C, A])
    rerenderModal()

    expect(heading()).toHaveTextContent('Candidate B')
    expect(screen.getByText('1 of 3')).toBeInTheDocument()
  })

  it('advances in place when a post-open fetch drops the tracked candidate', () => {
    mockCandidatesQuery([A, B, C])
    const { rerenderModal } = renderModal([A, B, C], B.id)
    expect(heading()).toHaveTextContent('Candidate B')

    // B was resolved elsewhere: the modal adopts the candidate now at B's
    // old position.
    mockCandidatesQuery([A, C])
    rerenderModal()

    expect(heading()).toHaveTextContent('Candidate C')
    expect(screen.getByText('2 of 2')).toBeInTheDocument()
  })

  it('clamps to the last candidate when the removed candidate was at the end', () => {
    mockCandidatesQuery([A, B, C])
    const { rerenderModal } = renderModal([A, B, C], C.id)
    expect(heading()).toHaveTextContent('Candidate C')

    mockCandidatesQuery([A, B])
    rerenderModal()

    expect(heading()).toHaveTextContent('Candidate B')
    expect(screen.getByText('2 of 2')).toBeInTheDocument()
  })

  it('runs on the page list when a pre-open cache lacks the clicked candidate, and refetches', () => {
    // The cached 1000-limit list predates the click and does not contain X.
    // The whole modal (display AND counter/nav) must run on the page's
    // list — never adopt from the pre-open cache — and force a refetch.
    mockCandidatesQuery([A, B], 'preOpen')
    renderModal([X, Y], X.id)

    expect(heading()).toHaveTextContent('Candidate X')
    expect(screen.getByText('1 of 2')).toBeInTheDocument()
    expect(refetchMock).toHaveBeenCalled()
  })

  it('hands over to the post-open list once it arrives with the clicked candidate', () => {
    mockCandidatesQuery([A, B], 'preOpen')
    const { rerenderModal } = renderModal([X, Y], X.id)
    expect(heading()).toHaveTextContent('Candidate X')

    mockCandidatesQuery([A, B, X])
    rerenderModal()

    expect(heading()).toHaveTextContent('Candidate X')
    expect(screen.getByText('3 of 3')).toBeInTheDocument()
  })

  it('advances in place when the post-open list confirms the clicked candidate is gone', () => {
    mockCandidatesQuery([A, B], 'preOpen')
    const { rerenderModal } = renderModal([X, Y], X.id)
    expect(heading()).toHaveTextContent('Candidate X')

    mockCandidatesQuery([A, B])
    rerenderModal()

    expect(heading()).toHaveTextContent('Candidate A')
    expect(screen.getByText('1 of 2')).toBeInTheDocument()
  })

  it('targets the clicked candidate’s real neighbor when Next is pressed during the fallback window', async () => {
    const user = userEvent.setup()
    // Pre-open cache lacks X: the modal runs on the page list [X, Y], so
    // Next must go to Y — not to a candidate from the stale cache.
    mockCandidatesQuery([A, B], 'preOpen')
    renderModal([X, Y], X.id)
    expect(heading()).toHaveTextContent('Candidate X')

    await user.click(screen.getByRole('button', { name: 'Next candidate' }))

    expect(
      await screen.findByRole('heading', { level: 3, name: /Candidate Y/ }, { timeout: 5000 })
    ).toBeInTheDocument()
    expect(screen.getByText('2 of 2')).toBeInTheDocument()
  })

  it('advances after a resolve even when no fresh list ever arrives', async () => {
    const user = userEvent.setup()
    // The confirming refetch fails/never lands: the query keeps returning
    // the same PRE-OPEN list including X. The modal resolved X itself, so
    // it must still advance off it.
    mockCandidatesQuery([X, Y, A], 'preOpen')
    renderModal([X, Y, A], X.id)
    expect(heading()).toHaveTextContent('Candidate X')

    await user.click(screen.getByRole('button', { name: 'Import as New Contact' }))

    expect(importAsync).toHaveBeenCalledWith(expect.objectContaining({ id: X.id }))
    expect(
      await screen.findByRole('heading', { level: 3, name: /Candidate Y/ }, { timeout: 5000 })
    ).toBeInTheDocument()
    expect(screen.getByText('1 of 2')).toBeInTheDocument()
  })

  it('lands Next on the adjacent remaining candidate when the neighbor vanishes mid-transition', async () => {
    const user = userEvent.setup()
    mockCandidatesQuery([A, B, C])
    const { rerenderModal } = renderModal([A, B, C], A.id)
    expect(heading()).toHaveTextContent('Candidate A')

    // Click Next (targets B), then B is resolved elsewhere before the 150ms
    // transition commits. Next must land on the candidate now adjacent (C),
    // not visibly no-op back onto A.
    await user.click(screen.getByRole('button', { name: 'Next candidate' }))
    mockCandidatesQuery([A, C])
    rerenderModal()

    // Generous timeout: the 150ms pager transition runs on a real timer and
    // can be starved on a loaded machine.
    expect(
      await screen.findByRole('heading', { level: 3, name: /Candidate C/ }, { timeout: 5000 })
    ).toBeInTheDocument()
    expect(screen.getByText('2 of 2')).toBeInTheDocument()
  })

  it('navigates to the adjacent candidate with the pager', async () => {
    const user = userEvent.setup()
    mockCandidatesQuery([A, B, C])
    renderModal([A, B, C], A.id)
    expect(heading()).toHaveTextContent('Candidate A')

    await user.click(screen.getByRole('button', { name: 'Next candidate' }))

    // The 150ms transition delays the switch (generous timeout for loaded
    // machines — the transition runs on a real timer).
    expect(await screen.findByText('2 of 3', {}, { timeout: 5000 })).toBeInTheDocument()
    expect(
      await screen.findByRole('heading', { level: 3, name: /Candidate B/ }, { timeout: 5000 })
    ).toBeInTheDocument()
  })

  it('closes when an authoritative fetch returns an empty queue', () => {
    // A post-open fetch is the truth: an empty queue means there is nothing
    // left to act on, even though the page snapshot still has candidates.
    mockCandidatesQuery([])
    const { props } = renderModal([X, Y], X.id)

    expect(props.onClose).toHaveBeenCalled()
  })

  it('unwinds without crashing when the last candidate is resolved in link mode', () => {
    // Resolving the FINAL candidate empties the queue, so `candidate` becomes
    // undefined. In link mode with a CRM contact selected, methodComparisons
    // dereferenced the missing candidate (candidate.emails) BEFORE the modal's
    // `if (!candidate) return null` guard could run, dropping the app into the
    // React error boundary. The modal must unwind cleanly instead.
    vi.mocked(useContact).mockReturnValue({
      data: { id: 'contact-1', full_name: 'Real Person', methods: [], cadence: '' },
    } as any)

    mockCandidatesQuery([X])
    const { props, rerenderModal } = renderModal([X], X.id, { initialMode: 'link' })
    expect(screen.getByRole('dialog')).toBeInTheDocument()

    // The last candidate is resolved: an authoritative fetch returns empty.
    mockCandidatesQuery([])
    rerenderModal()

    expect(props.onClose).toHaveBeenCalled()
  })

  it('keeps running on retained data after a background refetch error', () => {
    // React Query keeps the previous data when a background refetch errors.
    // That retained pre-open list still contains the tracked candidate, so
    // the modal must keep using it — not discard it and fall elsewhere.
    mockCandidatesQueryRaw({
      data: { candidates: [A, B, X] },
      isError: true,
      dataUpdatedAt: 1,
    })
    const { props } = renderModal([X], X.id)

    expect(heading()).toHaveTextContent('Candidate X')
    expect(screen.getByText('3 of 3')).toBeInTheDocument()
    expect(props.onClose).not.toHaveBeenCalled()
  })

  it('waits without adopting when neither list contains the clicked candidate', () => {
    // Pre-open cache [A, B] predates X; the page list is empty (e.g. its
    // last render dropped X). Displaying or adopting A here is exactly the
    // wrong-candidate bug — the modal must render nothing and wait for the
    // forced refetch.
    mockCandidatesQuery([A, B], 'preOpen')
    const { props, rerenderModal } = renderModal([], X.id)

    expect(screen.queryByRole('heading', { level: 3 })).not.toBeInTheDocument()
    expect(props.onClose).not.toHaveBeenCalled()
    expect(refetchMock).toHaveBeenCalledTimes(1)

    // The post-open list arrives with X: the modal shows the clicked
    // candidate, never having displayed a wrong one.
    mockCandidatesQuery([A, X])
    rerenderModal()
    expect(heading()).toHaveTextContent('Candidate X')
  })

  it('waits out a retained pre-open error while the forced refetch is in flight', async () => {
    // React Query can retain BOTH stale data and an error state from before
    // the open. That error must not close the modal — the mount-forced
    // refetch is still the thing being waited on, and it can land with the
    // clicked candidate.
    let settleRefetch!: () => void
    refetchMock.mockReturnValue(new Promise<void>(resolve => (settleRefetch = resolve)))
    mockCandidatesQueryRaw({
      data: { candidates: [A, B] },
      isError: true,
      dataUpdatedAt: 1,
    })
    const { props, rerenderModal } = renderModal([], X.id)

    expect(screen.queryByRole('heading', { level: 3 })).not.toBeInTheDocument()
    expect(props.onClose).not.toHaveBeenCalled()

    // The refetch succeeds with the clicked candidate: seamless display.
    mockCandidatesQuery([A, X])
    settleRefetch()
    rerenderModal()
    expect(heading()).toHaveTextContent('Candidate X')
    expect(props.onClose).not.toHaveBeenCalled()
  })

  it('closes instead of hanging when the POST-open refetch settles in error', async () => {
    mockCandidatesQueryRaw({
      data: { candidates: [A, B] },
      isError: true,
      dataUpdatedAt: 1,
    })
    const { props } = renderModal([], X.id)
    expect(screen.queryByRole('heading', { level: 3 })).not.toBeInTheDocument()

    // The forced refetch (mocked as already-resolved) settles, the query is
    // still errored, and no list contains the clicked candidate: close.
    await vi.waitFor(() => expect(props.onClose).toHaveBeenCalled())
  })

  it('does not close from a queue snapshot captured before the action settled', async () => {
    const user = userEvent.setup()
    // Authoritative queue is just [X] when Import is clicked, but a refetch
    // lands [X, Y] while the mutation is pending. The close decision must
    // see the fresh list and advance onto Y, not close from the stale one.
    let settleImport!: (v: unknown) => void
    importAsync.mockReturnValue(new Promise(resolve => (settleImport = resolve)))
    mockCandidatesQuery([X])
    const { props, rerenderModal } = renderModal([X], X.id)
    expect(heading()).toHaveTextContent('Candidate X')

    await user.click(screen.getByRole('button', { name: 'Import as New Contact' }))
    mockCandidatesQuery([X, Y])
    rerenderModal()
    settleImport({})

    expect(
      await screen.findByRole('heading', { level: 3, name: /Candidate Y/ }, { timeout: 5000 })
    ).toBeInTheDocument()
    expect(props.onClose).not.toHaveBeenCalled()
  })

  it('treats a fetch stamped at the mount instant as pre-open', () => {
    // Authority requires strictly-after-mount: a timestamp equal to the
    // mount instant cannot have observed the click.
    vi.useFakeTimers({ toFake: ['Date'] })
    try {
      mockCandidatesQueryRaw({
        data: { candidates: [A, B] },
        dataUpdatedAt: Date.now(),
      })
      const { props } = renderModal([X, Y], X.id)

      // Pre-open semantics: run on the page list, never adopt from the cache.
      expect(heading()).toHaveTextContent('Candidate X')
      expect(screen.getByText('1 of 2')).toBeInTheDocument()
      expect(refetchMock).toHaveBeenCalledTimes(1)
      expect(props.onClose).not.toHaveBeenCalled()
    } finally {
      vi.useRealTimers()
    }
  })

  it('forces exactly one refetch when the cache lacks the clicked candidate', () => {
    mockCandidatesQuery([A, B], 'preOpen')
    const { rerenderModal } = renderModal([X, Y], X.id)
    rerenderModal()
    rerenderModal()

    expect(refetchMock).toHaveBeenCalledTimes(1)
  })

  it('advances onto the fetched view when the resolve exhausts the page list', async () => {
    const user = userEvent.setup()
    // Page list is just [X]; the pre-open cache still knows [A, B]. After
    // resolving X (with the confirming refetch never landing), the modal
    // must advance onto A — not close while candidates remain.
    mockCandidatesQuery([A, B], 'preOpen')
    const { props } = renderModal([X], X.id)
    expect(heading()).toHaveTextContent('Candidate X')

    await user.click(screen.getByRole('button', { name: 'Import as New Contact' }))

    expect(importAsync).toHaveBeenCalledWith(expect.objectContaining({ id: X.id }))
    expect(
      await screen.findByRole('heading', { level: 3, name: /Candidate A/ }, { timeout: 5000 })
    ).toBeInTheDocument()
    expect(props.onClose).not.toHaveBeenCalled()
  })
})
