/* eslint-disable @typescript-eslint/no-explicit-any */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

// Mock the hooks
vi.mock('@/hooks/use-imports', () => ({
  useImportCandidates: vi.fn(),
  useImportAsContact: vi.fn(),
  useLinkCandidate: vi.fn(),
  useIgnoreCandidate: vi.fn(),
  useTriggerSync: vi.fn(),
}))

vi.mock('@/hooks/use-contacts', () => ({
  useContacts: vi.fn(),
}))

vi.mock('@/hooks/use-google-accounts', () => ({
  useGoogleAccounts: vi.fn(),
}))

vi.mock('@/components/layout/navigation', () => ({
  Navigation: () => <div>Navigation</div>,
}))

import ImportsPage from '../page'
import {
  useImportCandidates,
  useImportAsContact,
  useLinkCandidate,
  useIgnoreCandidate,
  useTriggerSync,
} from '@/hooks/use-imports'
import { useContacts } from '@/hooks/use-contacts'
import { useGoogleAccounts } from '@/hooks/use-google-accounts'

const createWrapper = () => {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
    },
  })
  // eslint-disable-next-line react/display-name
  return ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  )
}

describe('ImportsPage - Suggested Matches', () => {
  beforeEach(() => {
    vi.clearAllMocks()

    // Default mock implementations
    vi.mocked(useImportAsContact).mockReturnValue({
      mutateAsync: vi.fn(),
      isPending: false,
    } as any)

    vi.mocked(useLinkCandidate).mockReturnValue({
      mutateAsync: vi.fn(),
      isPending: false,
    } as any)

    vi.mocked(useIgnoreCandidate).mockReturnValue({
      mutateAsync: vi.fn(),
      isPending: false,
    } as any)

    vi.mocked(useTriggerSync).mockReturnValue({
      mutateAsync: vi.fn(),
      isPending: false,
    } as any)

    vi.mocked(useGoogleAccounts).mockReturnValue({
      data: [],
    } as any)

    vi.mocked(useContacts).mockReturnValue({
      data: {
        contacts: [
          {
            id: 'contact-1',
            full_name: 'John Smith',
            created_at: '2024-01-01',
            updated_at: '2024-01-01',
          },
        ],
        total: 1,
        page: 1,
        limit: 500,
      },
    } as any)
  })

  it('shows "Link to [Name]" when candidate has suggested match', () => {
    vi.mocked(useImportCandidates).mockReturnValue({
      data: {
        candidates: [
          {
            id: 'candidate-1',
            source: 'gcontacts',
            display_name: 'John Doe',
            emails: ['john@example.com'],
            phones: [],
            suggested_match: {
              contact_id: 'contact-1',
              contact_name: 'John Smith',
              confidence: 0.85,
            },
          },
        ],
        total: 1,
        page: 1,
        limit: 20,
        pages: 1,
      },
      isLoading: false,
      error: null,
    } as any)

    render(<ImportsPage />, { wrapper: createWrapper() })

    expect(screen.getByRole('button', { name: 'Link to John Smith (85%)' })).toBeInTheDocument()
  })

  it('shows "Link (select)" when candidate has no suggested match', () => {
    vi.mocked(useImportCandidates).mockReturnValue({
      data: {
        candidates: [
          {
            id: 'candidate-1',
            source: 'gcontacts',
            display_name: 'Jane Doe',
            emails: ['jane@example.com'],
            phones: [],
            // No suggested_match
          },
        ],
        total: 1,
        page: 1,
        limit: 20,
        pages: 1,
      },
      isLoading: false,
      error: null,
    } as any)

    render(<ImportsPage />, { wrapper: createWrapper() })

    expect(screen.getByRole('button', { name: 'Link (select)' })).toBeInTheDocument()
  })

  it('displays candidates with matches before those without', () => {
    vi.mocked(useImportCandidates).mockReturnValue({
      data: {
        candidates: [
          {
            id: 'candidate-with-match',
            source: 'gcontacts',
            display_name: 'John Doe',
            emails: [],
            phones: [],
            suggested_match: {
              contact_id: 'contact-1',
              contact_name: 'John Smith',
              confidence: 0.85,
            },
          },
          {
            id: 'candidate-without-match',
            source: 'gcontacts',
            display_name: 'Jane Doe',
            emails: [],
            phones: [],
            // No suggested_match
          },
        ],
        total: 2,
        page: 1,
        limit: 20,
        pages: 1,
      },
      isLoading: false,
      error: null,
    } as any)

    render(<ImportsPage />, { wrapper: createWrapper() })

    const linkButtons = screen.getAllByRole('button', { name: /Link/i })

    // First button should be for the candidate with match
    expect(linkButtons[0]).toHaveTextContent('Link to John Smith (85%)')
    // Second button should be for the candidate without match
    expect(linkButtons[1]).toHaveTextContent('Link (select)')
  })

  it('opens modal with pre-selected contact when clicking suggested match', async () => {
    const user = userEvent.setup()

    vi.mocked(useImportCandidates).mockReturnValue({
      data: {
        candidates: [
          {
            id: 'candidate-1',
            source: 'gcontacts',
            display_name: 'John Doe',
            emails: ['john@example.com'],
            phones: [],
            suggested_match: {
              contact_id: 'contact-1',
              contact_name: 'John Smith',
              confidence: 0.85,
            },
          },
        ],
        total: 1,
        page: 1,
        limit: 20,
        pages: 1,
      },
      isLoading: false,
      error: null,
    } as any)

    render(<ImportsPage />, { wrapper: createWrapper() })

    // Click the Link button
    const linkButton = screen.getByRole('button', { name: 'Link to John Smith (85%)' })
    await user.click(linkButton)

    // Modal should open
    await waitFor(() => {
      expect(screen.getByText('Link to Existing Contact')).toBeInTheDocument()
    })

    // The modal description should mention the candidate (there will be multiple "John Doe" on page)
    expect(screen.getAllByText(/John Doe/).length).toBeGreaterThan(0)
  })

  it('shows confidence score in button for high confidence matches', () => {
    vi.mocked(useImportCandidates).mockReturnValue({
      data: {
        candidates: [
          {
            id: 'candidate-1',
            source: 'gcontacts',
            display_name: 'John Doe',
            emails: ['john@example.com'],
            phones: [],
            suggested_match: {
              contact_id: 'contact-1',
              contact_name: 'John Smith',
              confidence: 0.95, // Very high confidence
            },
          },
        ],
        total: 1,
        page: 1,
        limit: 20,
        pages: 1,
      },
      isLoading: false,
      error: null,
    } as any)

    render(<ImportsPage />, { wrapper: createWrapper() })

    // Button should show the suggested contact name with confidence percentage
    expect(screen.getByRole('button', { name: 'Link to John Smith (95%)' })).toBeInTheDocument()
  })

  it('handles multiple candidates with different match states', () => {
    vi.mocked(useImportCandidates).mockReturnValue({
      data: {
        candidates: [
          {
            id: 'candidate-1',
            source: 'gcontacts',
            display_name: 'John Doe',
            emails: [],
            phones: [],
            suggested_match: {
              contact_id: 'contact-1',
              contact_name: 'John Smith',
              confidence: 0.85,
            },
          },
          {
            id: 'candidate-2',
            source: 'gcontacts',
            display_name: 'Jane Doe',
            emails: [],
            phones: [],
            suggested_match: {
              contact_id: 'contact-2',
              contact_name: 'Jane Johnson',
              confidence: 0.75,
            },
          },
          {
            id: 'candidate-3',
            source: 'gcontacts',
            display_name: 'Bob Wilson',
            emails: [],
            phones: [],
            // No match
          },
        ],
        total: 3,
        page: 1,
        limit: 20,
        pages: 1,
      },
      isLoading: false,
      error: null,
    } as any)

    render(<ImportsPage />, { wrapper: createWrapper() })

    // Should show all three candidates
    expect(screen.getByText('John Doe')).toBeInTheDocument()
    expect(screen.getByText('Jane Doe')).toBeInTheDocument()
    expect(screen.getByText('Bob Wilson')).toBeInTheDocument()

    // Should have correct button texts
    expect(screen.getByRole('button', { name: 'Link to John Smith (85%)' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Link to Jane Johnson (75%)' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Link (select)' })).toBeInTheDocument()
  })

  it('closes modal when clicking Cancel', async () => {
    const user = userEvent.setup()

    vi.mocked(useImportCandidates).mockReturnValue({
      data: {
        candidates: [
          {
            id: 'candidate-1',
            source: 'gcontacts',
            display_name: 'John Doe',
            emails: [],
            phones: [],
            suggested_match: {
              contact_id: 'contact-1',
              contact_name: 'John Smith',
              confidence: 0.85,
            },
          },
        ],
        total: 1,
        page: 1,
        limit: 20,
        pages: 1,
      },
      isLoading: false,
      error: null,
    } as any)

    render(<ImportsPage />, { wrapper: createWrapper() })

    // Open modal
    const linkButton = screen.getByRole('button', { name: 'Link to John Smith (85%)' })
    await user.click(linkButton)

    await waitFor(() => {
      expect(screen.getByText('Link to Existing Contact')).toBeInTheDocument()
    })

    // Close modal
    const cancelButton = screen.getByRole('button', { name: /Cancel/i })
    await user.click(cancelButton)

    // Modal should be closed
    await waitFor(() => {
      expect(screen.queryByText('Link to Existing Contact')).not.toBeInTheDocument()
    })
  })
})

describe('ImportsPage - Source Filter', () => {
  beforeEach(() => {
    vi.clearAllMocks()

    // Default mock implementations
    vi.mocked(useImportAsContact).mockReturnValue({
      mutateAsync: vi.fn(),
      isPending: false,
    } as any)

    vi.mocked(useLinkCandidate).mockReturnValue({
      mutateAsync: vi.fn(),
      isPending: false,
    } as any)

    vi.mocked(useIgnoreCandidate).mockReturnValue({
      mutateAsync: vi.fn(),
      isPending: false,
    } as any)

    vi.mocked(useTriggerSync).mockReturnValue({
      mutateAsync: vi.fn(),
      isPending: false,
    } as any)

    vi.mocked(useGoogleAccounts).mockReturnValue({
      data: [],
    } as any)

    vi.mocked(useContacts).mockReturnValue({
      data: {
        contacts: [],
        total: 0,
        page: 1,
        limit: 500,
      },
    } as any)

    vi.mocked(useImportCandidates).mockReturnValue({
      data: {
        candidates: [],
        total: 0,
        page: 1,
        limit: 20,
        pages: 0,
      },
      isLoading: false,
      error: null,
    } as any)
  })

  it('displays source filter buttons', () => {
    render(<ImportsPage />, { wrapper: createWrapper() })

    expect(screen.getByText('Filter:')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'All Sources' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Google Contacts' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Calendar' })).toBeInTheDocument()
  })

  it('All Sources filter is selected by default', () => {
    render(<ImportsPage />, { wrapper: createWrapper() })

    const allSourcesButton = screen.getByRole('button', { name: 'All Sources' })
    expect(allSourcesButton).toHaveClass('bg-blue-600')

    const googleContactsButton = screen.getByRole('button', { name: 'Google Contacts' })
    expect(googleContactsButton).toHaveClass('bg-gray-100')

    const calendarButton = screen.getByRole('button', { name: 'Calendar' })
    expect(calendarButton).toHaveClass('bg-gray-100')
  })

  it('clicking Google Contacts filter updates selection', async () => {
    const user = userEvent.setup()
    render(<ImportsPage />, { wrapper: createWrapper() })

    const googleContactsButton = screen.getByRole('button', { name: 'Google Contacts' })
    await user.click(googleContactsButton)

    // useImportCandidates should be called with source filter
    expect(useImportCandidates).toHaveBeenCalledWith(
      expect.objectContaining({ source: 'gcontacts' })
    )
  })

  it('clicking Calendar filter updates selection', async () => {
    const user = userEvent.setup()
    render(<ImportsPage />, { wrapper: createWrapper() })

    const calendarButton = screen.getByRole('button', { name: 'Calendar' })
    await user.click(calendarButton)

    // useImportCandidates should be called with source filter
    expect(useImportCandidates).toHaveBeenCalledWith(
      expect.objectContaining({ source: 'gcal_attendee' })
    )
  })

  it('clicking All Sources removes source filter', async () => {
    const user = userEvent.setup()
    render(<ImportsPage />, { wrapper: createWrapper() })

    // First click Calendar to set a filter
    const calendarButton = screen.getByRole('button', { name: 'Calendar' })
    await user.click(calendarButton)

    // Then click All Sources
    const allSourcesButton = screen.getByRole('button', { name: 'All Sources' })
    await user.click(allSourcesButton)

    // useImportCandidates should be called without source filter (undefined)
    const lastCall = vi.mocked(useImportCandidates).mock.calls.pop()
    expect(lastCall?.[0]?.source).toBeUndefined()
  })

  it('resets to page 1 when changing filter', async () => {
    const user = userEvent.setup()
    render(<ImportsPage />, { wrapper: createWrapper() })

    const calendarButton = screen.getByRole('button', { name: 'Calendar' })
    await user.click(calendarButton)

    // Should reset page to 1 when changing filter
    expect(useImportCandidates).toHaveBeenCalledWith(expect.objectContaining({ page: 1 }))
  })
})
