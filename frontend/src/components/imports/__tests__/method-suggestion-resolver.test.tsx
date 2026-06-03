/* eslint-disable @typescript-eslint/no-explicit-any */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

vi.mock('@/hooks/use-contacts', () => ({
  useContact: vi.fn(),
}))

vi.mock('@/hooks/use-suggestions', () => ({
  useResolveMethodSuggestions: vi.fn(),
  useDismissMethodSuggestions: vi.fn(),
}))

import { MethodSuggestionResolver } from '../method-suggestion-resolver'
import { useContact } from '@/hooks/use-contacts'
import { useResolveMethodSuggestions, useDismissMethodSuggestions } from '@/hooks/use-suggestions'
import type { MethodSuggestion } from '@/types/import'

const suggestion: MethodSuggestion = {
  external_contact_id: 'ext-1',
  contact_id: 'contact-1',
  contact_name: 'Contact A',
  source: 'gcontacts',
  methods: [
    { type: 'email', value: 'a@example.test' },
    { type: 'phone', value: '+15551230000' },
  ],
}

let resolveAsync: ReturnType<typeof vi.fn>
let dismissAsync: ReturnType<typeof vi.fn>

beforeEach(() => {
  vi.clearAllMocks()
  resolveAsync = vi.fn().mockResolvedValue({})
  dismissAsync = vi.fn().mockResolvedValue({})
  vi.mocked(useContact).mockReturnValue({ data: { id: 'contact-1', methods: [] } } as any)
  vi.mocked(useResolveMethodSuggestions).mockReturnValue({
    mutateAsync: resolveAsync,
    isPending: false,
  } as any)
  vi.mocked(useDismissMethodSuggestions).mockReturnValue({
    mutateAsync: dismissAsync,
    isPending: false,
  } as any)
})

function renderResolver() {
  return render(
    <MethodSuggestionResolver
      suggestion={suggestion}
      onClose={vi.fn()}
      onSuccess={vi.fn()}
      onError={vi.fn()}
    />
  )
}

describe('MethodSuggestionResolver', () => {
  it('renders the fixed-contact header (no ContactSelector, no Import)', () => {
    renderResolver()
    expect(screen.getByText('Adding to Contact A')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Import/i })).not.toBeInTheDocument()
    expect(screen.queryByPlaceholderText(/Search for a contact/i)).not.toBeInTheDocument()
  })

  it('does not render an email type dropdown (type is locked)', () => {
    renderResolver()
    expect(screen.queryByLabelText('Email type')).not.toBeInTheDocument()
  })

  it('submits the explicit selected (type,value) list on Confirm', async () => {
    const user = userEvent.setup()
    renderResolver()
    // Both methods pre-selected → Confirm enabled.
    const confirm = screen.getByRole('button', { name: /Confirm/i })
    expect(confirm).toBeEnabled()
    await user.click(confirm)
    expect(resolveAsync).toHaveBeenCalledWith({
      id: 'ext-1',
      request: {
        methods: [
          { type: 'email', value: 'a@example.test' },
          { type: 'phone', value: '+15551230000' },
        ],
      },
    })
  })

  it('disables Confirm when zero methods are selected', async () => {
    const user = userEvent.setup()
    renderResolver()
    // Deselect both methods.
    const toggles = screen.getAllByRole('button', { name: /method/i })
    for (const t of toggles) {
      await user.click(t)
    }
    expect(screen.getByRole('button', { name: /Confirm/i })).toBeDisabled()
  })

  it('dismisses the whole card with an empty methods request', async () => {
    const user = userEvent.setup()
    renderResolver()
    await user.click(screen.getByRole('button', { name: /Dismiss/i }))
    expect(dismissAsync).toHaveBeenCalledWith({ id: 'ext-1', request: {} })
  })
})
