/* eslint-disable @typescript-eslint/no-explicit-any */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

vi.mock('@/hooks/use-interactions-queue', () => ({
  useResolveNameCandidate: vi.fn(),
}))

vi.mock('@/hooks/use-contacts', () => ({
  useContacts: vi.fn(),
}))

import { NameCandidateModal } from '../NameCandidateModal'
import { useResolveNameCandidate } from '@/hooks/use-interactions-queue'
import { useContacts } from '@/hooks/use-contacts'
import type { NameCandidateGroup } from '@/types/import'

const groups: NameCandidateGroup[] = [
  {
    normalized_token: 'lena',
    token_display: 'Lena',
    evidence_count: 2,
    session_titles: ['1:1 with Lena'],
  },
  {
    normalized_token: 'marco',
    token_display: 'Marco',
    evidence_count: 1,
    session_titles: [],
  },
]

let mutateAsync: ReturnType<typeof vi.fn>

beforeEach(() => {
  vi.clearAllMocks()
  mutateAsync = vi.fn().mockResolvedValue({ action: 'import', contact_id: 'c-1' })
  vi.mocked(useResolveNameCandidate).mockReturnValue({
    mutateAsync,
    isPending: false,
  } as any)
  vi.mocked(useContacts).mockReturnValue({
    data: {
      contacts: [{ id: 'contact-1', full_name: 'Existing Person', methods: [] }],
    },
  } as any)
})

describe('NameCandidateModal', () => {
  it('opens at the given index with the token pre-filled as the name', () => {
    render(
      <NameCandidateModal
        groups={groups}
        initialIndex={0}
        onClose={() => {}}
        onSuccess={() => {}}
        onError={() => {}}
      />
    )
    expect(screen.getByText('1 of 2')).toBeInTheDocument()
    expect(screen.getByLabelText('Name')).toHaveValue('Lena')
  })

  it('shows the no-methods info note and session-title evidence (never a MethodSelector)', () => {
    render(
      <NameCandidateModal
        groups={groups}
        initialIndex={0}
        onClose={() => {}}
        onSuccess={() => {}}
        onError={() => {}}
      />
    )
    expect(screen.getByText(/No contact methods/)).toBeInTheDocument()
    expect(screen.getByText('Evidence · session titles')).toBeInTheDocument()
    expect(screen.getByText('1:1 with Lena')).toBeInTheDocument()
    // No contact-methods apparatus in the name-candidate branch: the modal exposes
    // only name + cadence + (link mode) contact selector — never the method
    // import/conflict UI used by the candidate modal.
    expect(screen.queryByText(/Import Methods/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/Resolve Conflicts/i)).not.toBeInTheDocument()
  })

  it('imports the whole token group with name + cadence and reports success', async () => {
    const user = userEvent.setup()
    const onSuccess = vi.fn()
    render(
      <NameCandidateModal
        groups={groups}
        initialIndex={0}
        onClose={() => {}}
        onSuccess={onSuccess}
        onError={() => {}}
      />
    )

    await user.selectOptions(screen.getByLabelText('Cadence'), 'monthly')
    await user.click(screen.getByRole('button', { name: /Create contact/ }))

    await waitFor(() => {
      expect(mutateAsync).toHaveBeenCalledWith({
        normalized_token: 'lena',
        action: 'import',
        name: 'Lena',
        cadence: 'monthly',
        crm_contact_id: undefined,
      })
    })
    expect(onSuccess).toHaveBeenCalled()
  })

  it('links to a selected contact in link mode', async () => {
    mutateAsync.mockResolvedValue({ action: 'link', contact_id: 'contact-1' })
    const user = userEvent.setup()
    render(
      <NameCandidateModal
        groups={groups}
        initialIndex={0}
        onClose={() => {}}
        onSuccess={() => {}}
        onError={() => {}}
      />
    )

    // Toggle to link mode.
    await user.click(screen.getByRole('button', { name: /Link to existing/ }))
    // Open the ContactSelector (placeholder is a span until the field opens),
    // then pick the seeded contact.
    await user.click(screen.getByText('Search contacts...'))
    await user.click(await screen.findByText('Existing Person'))

    await user.click(screen.getByRole('button', { name: /Link contact/ }))

    await waitFor(() => {
      expect(mutateAsync).toHaveBeenCalledWith(
        expect.objectContaining({
          normalized_token: 'lena',
          action: 'link',
          crm_contact_id: 'contact-1',
        })
      )
    })
  })

  it('blocks the link action until a contact is picked', () => {
    render(
      <NameCandidateModal
        groups={groups}
        initialIndex={0}
        onClose={() => {}}
        onSuccess={() => {}}
        onError={() => {}}
      />
    )
    fireEvent.click(screen.getByRole('button', { name: /Link to existing/ }))
    expect(screen.getByRole('button', { name: /Link contact/ })).toBeDisabled()
  })

  it('ignores the token group via "Not a person"', async () => {
    mutateAsync.mockResolvedValue({ action: 'ignore' })
    const user = userEvent.setup()
    render(
      <NameCandidateModal
        groups={groups}
        initialIndex={0}
        onClose={() => {}}
        onSuccess={() => {}}
        onError={() => {}}
      />
    )
    await user.click(screen.getByRole('button', { name: /Not a person/ }))
    await waitFor(() => {
      expect(mutateAsync).toHaveBeenCalledWith(
        expect.objectContaining({ normalized_token: 'lena', action: 'ignore' })
      )
    })
  })

  it('pages forward to the next token group', async () => {
    const user = userEvent.setup()
    render(
      <NameCandidateModal
        groups={groups}
        initialIndex={0}
        onClose={() => {}}
        onSuccess={() => {}}
        onError={() => {}}
      />
    )
    await user.click(screen.getByRole('button', { name: 'Next' }))
    expect(screen.getByText('2 of 2')).toBeInTheDocument()
    expect(screen.getByLabelText('Name')).toHaveValue('Marco')
  })
})
