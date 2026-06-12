import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import { SyncStalenessBanner } from '../sync-staleness-banner'
import type { StalenessBreach } from '@/types/sync'

vi.mock('@/hooks/use-sync-staleness', () => ({
  useSyncStaleness: vi.fn(),
}))

import { useSyncStaleness } from '@/hooks/use-sync-staleness'

const mockedUseSyncStaleness = useSyncStaleness as unknown as ReturnType<typeof vi.fn>

function createBreach(overrides: Partial<StalenessBreach> = {}): StalenessBreach {
  return {
    id: 'breach-1',
    source: 'messages',
    account_id: 'host-1',
    breach_type: 'push_stale',
    stale_since: '2026-06-01T00:00:00Z',
    threshold_seconds: 172800,
    details: 'no push for 3d (threshold 48h)',
    detected_at: '2026-06-04T00:00:00Z',
    last_observed_at: '2026-06-04T00:00:00Z',
    ...overrides,
  }
}

beforeEach(() => {
  vi.clearAllMocks()
})

describe('SyncStalenessBanner', () => {
  it('renders nothing while loading', () => {
    mockedUseSyncStaleness.mockReturnValue({ data: undefined, isLoading: true, isError: false })
    const { container } = render(<SyncStalenessBanner />)
    expect(container).toBeEmptyDOMElement()
  })

  it('renders nothing on fetch error (fail-quiet)', () => {
    mockedUseSyncStaleness.mockReturnValue({ data: undefined, isLoading: false, isError: true })
    const { container } = render(<SyncStalenessBanner />)
    expect(container).toBeEmptyDOMElement()
  })

  it('renders nothing when there are no breaches', () => {
    mockedUseSyncStaleness.mockReturnValue({ data: [], isLoading: false, isError: false })
    const { container } = render(<SyncStalenessBanner />)
    expect(container).toBeEmptyDOMElement()
  })

  it('renders nothing when all breaches are sync_error (settings already shows them)', () => {
    mockedUseSyncStaleness.mockReturnValue({
      data: [
        createBreach({ id: 'b1', source: 'gcal', breach_type: 'sync_error' }),
        createBreach({ id: 'b2', source: 'email', breach_type: 'sync_error' }),
      ],
      isLoading: false,
      isError: false,
    })
    const { container } = render(<SyncStalenessBanner />)
    expect(container).toBeEmptyDOMElement()
  })

  it('excludes sync_error breaches from the list and the count', () => {
    mockedUseSyncStaleness.mockReturnValue({
      data: [
        createBreach({ id: 'b1', source: 'messages' }),
        createBreach({ id: 'b2', source: 'gcal', breach_type: 'sync_error' }),
      ],
      isLoading: false,
      isError: false,
    })
    render(<SyncStalenessBanner />)

    expect(screen.getByText('1 sync source may be stalled')).toBeInTheDocument()
    expect(screen.getByText(/Messages/)).toBeInTheDocument()
    expect(screen.queryByText(/Google Calendar/)).not.toBeInTheDocument()
    expect(screen.getAllByRole('listitem')).toHaveLength(1)
  })

  it('renders a singular heading and one line for a single breach', () => {
    mockedUseSyncStaleness.mockReturnValue({
      data: [createBreach()],
      isLoading: false,
      isError: false,
    })
    render(<SyncStalenessBanner />)

    expect(screen.getByRole('status')).toBeInTheDocument()
    expect(screen.getByText('1 sync source may be stalled')).toBeInTheDocument()
    // Human source label + the watchdog's details string appear on the line.
    expect(screen.getByText(/Messages/)).toBeInTheDocument()
    expect(screen.getByText(/no push for 3d/)).toBeInTheDocument()
  })

  it('renders a plural heading and one line per breach', () => {
    mockedUseSyncStaleness.mockReturnValue({
      data: [
        createBreach({ id: 'b1', source: 'messages' }),
        createBreach({
          id: 'b2',
          source: 'gcal',
          breach_type: 'sync_stale',
          details: 'no successful sync for 26h (threshold 24h)',
        }),
        createBreach({ id: 'b3', source: 'mac_host', breach_type: 'heartbeat', details: '' }),
      ],
      isLoading: false,
      isError: false,
    })
    render(<SyncStalenessBanner />)

    expect(screen.getByText('3 sync sources may be stalled')).toBeInTheDocument()
    expect(screen.getByText(/Messages/)).toBeInTheDocument()
    expect(screen.getByText(/Google Calendar/)).toBeInTheDocument()
    expect(screen.getByText(/Mac daemon/)).toBeInTheDocument()
    expect(screen.getAllByRole('listitem')).toHaveLength(3)
  })

  it('falls back to the raw source name for unknown sources', () => {
    mockedUseSyncStaleness.mockReturnValue({
      data: [createBreach({ source: 'some_new_source', details: '' })],
      isLoading: false,
      isError: false,
    })
    render(<SyncStalenessBanner />)
    expect(screen.getByText(/some_new_source/)).toBeInTheDocument()
  })
})
