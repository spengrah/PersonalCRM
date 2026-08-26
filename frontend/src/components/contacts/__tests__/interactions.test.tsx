import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type {
  InteractionContentResponse,
  InteractionListItemResponse,
} from '@/types/generated/contact'
import type { CalendarEvent } from '@/types/calendar'

const mocks = vi.hoisted(() => ({
  list: vi.fn(),
  getContent: vi.fn(),
  currentTime: new Date('2031-05-10T12:00:00.000Z'),
  events: [] as CalendarEvent[],
  advancingClock: false,
}))

vi.mock('@/lib/interactions-api', () => ({
  interactionsApi: { list: mocks.list, getContent: mocks.getContent },
}))
vi.mock('@/hooks/use-calendar', () => ({
  useUpcomingEventsForContact: () => ({ data: mocks.events, isLoading: false, error: null }),
}))
vi.mock('@/hooks/use-accelerated-time', () => ({
  useAcceleratedTime: () => ({
    currentTime: mocks.advancingClock
      ? new Date(mocks.currentTime.getTime() + Math.random() * 1000)
      : mocks.currentTime,
  }),
}))

import { Interactions } from '../interactions'

const item: InteractionListItemResponse = {
  id: 'interaction-1',
  contact_id: 'contact-1',
  source: 'gchat',
  occurred_at: '2031-05-09T12:00:00Z',
  created_at: '2031-05-09T12:00:00Z',
  direction: 'mutual',
  label: 'Thread',
  content_kind: 'messages',
  message_count: 2,
  is_group: true,
  venue_tags: [
    { key: 'venue-a', label: 'Alpha', kind: 'group_chat', is_group: true },
    { key: 'venue-b', label: 'Beta', kind: 'group_chat', is_group: true },
  ],
}

const content: InteractionContentResponse = {
  interaction_id: item.id,
  kind: 'messages',
  meeting_notes: [],
  messages: [
    {
      id: 'message-a',
      sender: 'Sender A',
      is_outgoing: false,
      sent_at: item.occurred_at,
      body: 'A',
      venue_key: 'venue-a',
    },
    {
      id: 'message-b',
      sender: 'Sender B',
      is_outgoing: false,
      sent_at: item.occurred_at,
      body: 'B',
      venue_key: 'venue-b',
    },
  ],
}

const response = {
  data: { items: [item], venue_options: item.venue_tags },
  meta: { pagination: { page: 1, pages: 1 } },
}

function renderInteractions() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <Interactions contactId="contact-1" />
    </QueryClientProvider>
  )
}

beforeEach(() => {
  mocks.list.mockReset().mockResolvedValue(response)
  mocks.getContent.mockReset().mockResolvedValue(content)
  mocks.events = []
  mocks.currentTime = new Date('2031-05-10T12:00:00.000Z')
  mocks.advancingClock = false
})

afterEach(() => {
  vi.useRealTimers()
})

describe('Interactions filter wiring', () => {
  it('forwards the selected venue into the expanded content path', async () => {
    const user = (await import('@testing-library/user-event')).default.setup()
    renderInteractions()
    await screen.findByText('Thread', { exact: true })
    await user.click(screen.getByRole('button', { name: 'Expand content' }))
    await waitFor(() => expect(document.querySelectorAll('[data-message-id]')).toHaveLength(2))
    expect(document.querySelectorAll('[data-message-id]')).toHaveLength(2)
    await user.click(screen.getByRole('button', { name: 'Collapse content' }))
    await user.selectOptions(screen.getByRole('combobox', { name: 'Venue' }), 'venue-a')
    await user.click(screen.getByRole('button', { name: 'Expand content' }))
    await waitFor(() => expect(document.querySelectorAll('[data-message-id]')).toHaveLength(1))
    expect(document.querySelector('[data-message-id="message-a"]')).toBeInTheDocument()
    expect(document.querySelector('[data-message-id="message-b"]')).not.toBeInTheDocument()
  })

  it('materializes preset dates from the accelerated clock', async () => {
    const user = (await import('@testing-library/user-event')).default.setup()
    renderInteractions()
    await screen.findByText('Thread', { exact: true })
    await user.click(screen.getByRole('button', { name: '30 days' }))
    const expected = new Date(mocks.currentTime.getTime() - 30 * 86400000).toISOString()
    await waitFor(() =>
      expect(mocks.list.mock.calls.some(([, params]) => params.from === expected)).toBe(true)
    )
  })

  it('uses the accelerated clock for custom upcoming visibility', async () => {
    vi.useFakeTimers({ now: new Date('2026-09-01T12:00:00.000Z'), toFake: ['Date'] })
    const user = (await import('@testing-library/user-event')).default.setup({
      advanceTimers: vi.advanceTimersByTime,
    })
    mocks.events = [
      {
        id: 'event-1',
        title: 'Event',
        start_time: '2027-06-15T00:00:00Z',
        end_time: '2027-06-15T01:00:00Z',
        status: 'confirmed',
        attendee_count: 1,
      },
    ]
    renderInteractions()
    await screen.findByText('Thread', { exact: true })
    await user.click(screen.getByRole('button', { name: 'Custom' }))
    fireEvent.change(screen.getByLabelText('Start date'), { target: { value: '2027-06-01' } })
    fireEvent.change(screen.getByLabelText('End date'), { target: { value: '2027-06-30' } })
    await waitFor(() =>
      expect(screen.queryByRole('list', { name: 'Upcoming events' })).not.toBeInTheDocument()
    )
    expect(document.querySelector('[data-event-id="event-1"]')).not.toBeInTheDocument()
    mocks.events = [{ ...mocks.events[0], id: 'event-2', start_time: '2031-08-01T00:00:00Z' }]
    fireEvent.change(screen.getByLabelText('Start date'), { target: { value: '2031-06-01' } })
    fireEvent.change(screen.getByLabelText('End date'), { target: { value: '2031-12-31' } })
    await waitFor(() =>
      expect(screen.getByRole('list', { name: 'Upcoming events' })).toBeInTheDocument()
    )
    expect(document.querySelector('[data-event-id="event-2"]')).toBeInTheDocument()
  })

  it('materializes a preset once per action despite later re-renders', async () => {
    const user = (await import('@testing-library/user-event')).default.setup()
    mocks.advancingClock = true
    renderInteractions()
    await screen.findByText('Thread', { exact: true })
    await user.click(screen.getByRole('button', { name: '30 days' }))
    await waitFor(() => expect(mocks.list.mock.calls.some(([, params]) => params.from)).toBe(true))
    await user.click(screen.getByRole('button', { name: 'Expand content' }))
    await user.click(screen.getByRole('button', { name: 'Collapse content' }))
    const froms = mocks.list.mock.calls.map(([, params]) => params.from).filter(Boolean)
    expect(new Set(froms).size).toBe(1)
  })
})
