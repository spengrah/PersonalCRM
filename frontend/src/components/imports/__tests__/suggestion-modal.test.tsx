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

function mockCandidatesQuery(
  list: ImportCandidate[],
  opts: { isFetching?: boolean; isStale?: boolean } = {}
) {
  vi.mocked(useImportCandidates).mockReturnValue({
    data: { candidates: list },
    isSuccess: true,
    isFetching: opts.isFetching ?? false,
    isStale: opts.isStale ?? false,
  } as any)
}

beforeEach(() => {
  vi.clearAllMocks()
  vi.mocked(useContacts).mockReturnValue({ data: { contacts: [] } } as any)
  vi.mocked(useContact).mockReturnValue({ data: undefined } as any)
  const mutation = { mutateAsync: vi.fn().mockResolvedValue({}), isPending: false } as any
  vi.mocked(useImportAsContact).mockReturnValue(mutation)
  vi.mocked(useLinkCandidate).mockReturnValue(mutation)
  vi.mocked(useIgnoreCandidate).mockReturnValue(mutation)
})

function renderModal(candidates: ImportCandidate[], initialCandidateId: string) {
  const item = { kind: 'contact' as const, candidates, initialCandidateId }
  const props = { onClose: vi.fn(), onSuccess: vi.fn(), onError: vi.fn() }
  const view = render(<SuggestionModal item={item} {...props} />)
  return {
    ...view,
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

  it('advances in place when the tracked candidate leaves the list', () => {
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

  it('does not latch onto a stale cached list that predates the clicked candidate', () => {
    // The modal mounts against a cached 1000-limit list that predates the
    // clicked candidate X (a refetch is in flight). X must stay displayed —
    // adopting from the stale list would latch the modal onto the wrong
    // candidate with no self-correction once the fresh list lands.
    const X = cand('cand-x', 'Candidate X')
    mockCandidatesQuery([A, B], { isFetching: true, isStale: true })
    const { rerenderModal } = renderModal([X], X.id)

    expect(heading()).toHaveTextContent('Candidate X')

    // The fresh list arrives, containing X.
    mockCandidatesQuery([A, B, X])
    rerenderModal()

    expect(heading()).toHaveTextContent('Candidate X')
    expect(screen.getByText('3 of 3')).toBeInTheDocument()
  })

  it('advances to the candidate the fresh list places at its position when the tracked one is truly gone', () => {
    // Same stale-cache mount, but the fresh settled list confirms the
    // tracked candidate is gone (resolved elsewhere): now advance in place.
    const X = cand('cand-x', 'Candidate X')
    mockCandidatesQuery([A, B], { isFetching: true, isStale: true })
    const { rerenderModal } = renderModal([X], X.id)
    expect(heading()).toHaveTextContent('Candidate X')

    mockCandidatesQuery([A, B])
    rerenderModal()

    expect(heading()).toHaveTextContent('Candidate A')
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

    expect(
      await screen.findByRole('heading', { level: 3, name: /Candidate C/ })
    ).toBeInTheDocument()
    expect(screen.getByText('2 of 2')).toBeInTheDocument()
  })

  it('navigates to the adjacent candidate with the pager', async () => {
    const user = userEvent.setup()
    mockCandidatesQuery([A, B, C])
    renderModal([A, B, C], A.id)
    expect(heading()).toHaveTextContent('Candidate A')

    await user.click(screen.getByRole('button', { name: 'Next candidate' }))

    // The 150ms transition delays the switch.
    expect(await screen.findByText('2 of 3')).toBeInTheDocument()
    expect(
      await screen.findByRole('heading', { level: 3, name: /Candidate B/ })
    ).toBeInTheDocument()
  })
})
