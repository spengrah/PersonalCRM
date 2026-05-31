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
  useContact: vi.fn(),
}))

vi.mock('@/hooks/use-google-accounts', () => ({
  useGoogleAccounts: vi.fn(),
}))

vi.mock('@/hooks/use-interactions-queue', () => ({
  useInteractionsQueue: vi.fn(),
  useResolveLink: vi.fn(),
  useAnarlogTitleDiscovery: vi.fn(),
  useResolveDiscoveryToken: vi.fn(),
}))

vi.mock('@/components/layout/navigation', () => ({
  Navigation: () => <div>Navigation</div>,
}))

// App Router navigation: the page derives tab/session state from the URL.
const mockReplace = vi.fn()
let mockSearchParams = new URLSearchParams()
vi.mock('next/navigation', () => ({
  useRouter: () => ({ replace: mockReplace, push: vi.fn() }),
  useSearchParams: () => mockSearchParams,
}))

import ImportsPage from '../page'
import {
  useImportCandidates,
  useImportAsContact,
  useLinkCandidate,
  useIgnoreCandidate,
  useTriggerSync,
} from '@/hooks/use-imports'
import { useContacts, useContact } from '@/hooks/use-contacts'
import { useGoogleAccounts } from '@/hooks/use-google-accounts'
import {
  useInteractionsQueue,
  useResolveLink,
  useAnarlogTitleDiscovery,
  useResolveDiscoveryToken,
} from '@/hooks/use-interactions-queue'

/** Reset the interactions-queue hooks to a quiet default (no items, no
 * discovery, idle mutations) so existing People-tab tests are unaffected. */
function mockInteractionsQueueDefaults() {
  vi.mocked(useInteractionsQueue).mockReturnValue({
    data: [],
    isLoading: false,
  } as any)
  vi.mocked(useAnarlogTitleDiscovery).mockReturnValue({
    data: [],
    isLoading: false,
  } as any)
  vi.mocked(useResolveLink).mockReturnValue({
    mutateAsync: vi.fn(),
    isPending: false,
  } as any)
  vi.mocked(useResolveDiscoveryToken).mockReturnValue({
    mutateAsync: vi.fn(),
    isPending: false,
  } as any)
}

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
    mockSearchParams = new URLSearchParams()
    mockReplace.mockClear()
    mockInteractionsQueueDefaults()

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

    vi.mocked(useContact).mockReturnValue({
      data: {
        id: 'contact-1',
        full_name: 'John Smith',
        methods: [],
        created_at: '2024-01-01',
        updated_at: '2024-01-01',
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

    // Modal should open - new modal has "Import as New" and "Link to Existing" toggle buttons
    await waitFor(() => {
      expect(screen.getByRole('button', { name: /Import as New/i })).toBeInTheDocument()
      expect(screen.getByRole('button', { name: /Link to Existing/i })).toBeInTheDocument()
    })

    // The modal should show the candidate name in the header
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
      expect(screen.getByRole('button', { name: /Import as New/i })).toBeInTheDocument()
    })

    // Close modal
    const cancelButton = screen.getByRole('button', { name: /Cancel/i })
    await user.click(cancelButton)

    // Modal should be closed - no Import as New button visible
    await waitFor(() => {
      expect(screen.queryByRole('button', { name: /Import as New/i })).not.toBeInTheDocument()
    })
  })
})

describe('ImportsPage - Source Filter', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockSearchParams = new URLSearchParams()
    mockReplace.mockClear()
    mockInteractionsQueueDefaults()

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

    vi.mocked(useContact).mockReturnValue({
      data: undefined,
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
    expect(screen.getByRole('button', { name: 'Telegram' })).toBeInTheDocument()
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

  it('clicking Telegram filter updates selection', async () => {
    const user = userEvent.setup()
    render(<ImportsPage />, { wrapper: createWrapper() })

    const telegramButton = screen.getByRole('button', { name: 'Telegram' })
    await user.click(telegramButton)

    // useImportCandidates should be called with source filter
    expect(useImportCandidates).toHaveBeenCalledWith(
      expect.objectContaining({ source: 'telegram' })
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

describe('ImportsPage - Telegram @username', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mockSearchParams = new URLSearchParams()
    mockReplace.mockClear()
    mockInteractionsQueueDefaults()

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
    vi.mocked(useGoogleAccounts).mockReturnValue({ data: [] } as any)
    vi.mocked(useContacts).mockReturnValue({
      data: { contacts: [], total: 0, page: 1, limit: 500 },
    } as any)
    vi.mocked(useContact).mockReturnValue({ data: null } as any)
  })

  it('renders @username chip for Telegram candidate with a handle', () => {
    vi.mocked(useImportCandidates).mockReturnValue({
      data: {
        candidates: [
          {
            id: 'tg-candidate-1',
            source: 'telegram',
            display_name: 'Dale Dobeck',
            emails: [],
            phones: [],
            metadata: { username: '@daledobeck' },
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

    // Heading shows the display name
    expect(screen.getByRole('heading', { name: 'Dale Dobeck' })).toBeInTheDocument()
    // Chip is a link to t.me/<handle> (no '@' in the URL path)
    const chip = screen.getByRole('link', { name: '@daledobeck' })
    expect(chip).toHaveAttribute('href', 'https://t.me/daledobeck')
    expect(chip).toHaveAttribute('target', '_blank')
    expect(chip).toHaveAttribute('rel', expect.stringContaining('noopener'))
  })

  it('falls back to @username heading and hides the chip when no names are set', () => {
    vi.mocked(useImportCandidates).mockReturnValue({
      data: {
        candidates: [
          {
            id: 'tg-candidate-2',
            source: 'telegram',
            emails: [],
            phones: [],
            metadata: { username: '@daledobeck' },
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

    // @username becomes the primary heading
    expect(screen.getByRole('heading', { name: '@daledobeck' })).toBeInTheDocument()
    // No "Unknown" rendered
    expect(screen.queryByText('Unknown')).not.toBeInTheDocument()
    // Chip is suppressed — no duplicate link rendered for the same handle
    expect(screen.queryByRole('link', { name: '@daledobeck' })).not.toBeInTheDocument()
  })
})
