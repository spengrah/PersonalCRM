import { describe, expect, it, vi, beforeEach } from 'vitest'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

import type {
  InteractionDirection,
  TimelineMemberResponse,
  InteractionContentKind,
  CallService,
  TimelineContentResponse,
} from '@/types/generated/contact'
import { TimelineDrillDown, TimelineRow } from '@/components/timeline'
import { formatDateTime } from '@/components/timeline/format'
import { InteractionDrillDown } from '../interaction-content'

const mocks = vi.hoisted(() => ({ getTimelineContent: vi.fn(), getTimelineAssertions: vi.fn() }))
vi.mock('@/lib/interactions-api', () => ({
  interactionsApi: {
    getTimelineContent: mocks.getTimelineContent,
    getTimelineAssertions: mocks.getTimelineAssertions,
  },
}))

beforeEach(() => {
  mocks.getTimelineContent.mockReset().mockReturnValue(new Promise(() => {}))
  mocks.getTimelineAssertions.mockReset().mockResolvedValue({ items: [] })
})

type RowOverrides = Partial<TimelineMemberResponse> & {
  id?: string
  direction?: InteractionDirection
}

const baseItem = ({ id = 'ixn-test', direction = 'inbound', ...member }: RowOverrides = {}) => ({
  identity: { kind: 'interaction' as const, id },
  cadence_inert: false,
  occurred_at: '2026-01-02T03:04:00Z',
  direction,
  members: [
    {
      source: 'manual',
      label: 'Synthetic interaction',
      content_kind: 'none' as const,
      message_count: 0,
      is_group: false,
      venue_tags: [],
      ...member,
    },
  ],
})

function renderItem(overrides: RowOverrides = {}) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <TimelineRow item={baseItem(overrides)} context={{ contactId: 'contact-test' }} />
    </QueryClientProvider>
  )
}

describe('TimelineRow on the contact page', () => {
  it.each([
    [45, '45s'],
    [372, '6m 12s'],
  ])('formats %s seconds as %s', (duration, expected) => {
    renderItem({
      source: 'phone_calls',
      content_kind: 'call',
      call: {
        service: 'voice',
        direction: 'inbound',
        answered: true,
        has_voicemail: false,
        duration_seconds: duration,
      },
    })
    expect(screen.getByText(expected)).toBeVisible()
  })

  it.each([
    ['facetime_video', 'FaceTime video'],
    ['facetime_audio', 'FaceTime audio'],
  ] as [CallService, string][])('renders the %s service label', (service, expected) => {
    renderItem({
      source: 'phone_calls',
      content_kind: 'call',
      call: {
        service,
        direction: 'inbound',
        answered: true,
        has_voicemail: false,
        duration_seconds: 60,
      },
    })
    expect(screen.getByText(expected)).toBeVisible()
  })

  it('renders missed and voicemail call states without a content indicator or expand affordance', () => {
    renderItem({
      source: 'phone_calls',
      content_kind: 'call',
      call: {
        service: 'voice',
        direction: 'inbound',
        answered: false,
        has_voicemail: true,
        duration_seconds: 0,
      },
    })
    expect(screen.getByText('Missed')).toBeVisible()
    expect(screen.getByText('Voicemail')).toBeVisible()
    expect(screen.queryByText('No content')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /expand/i })).not.toBeInTheDocument()
  })

  it.each([
    ['messages', 1, '1 message'],
    ['messages', 3, '3 messages'],
    ['meeting_note', 0, 'Meeting note'],
    ['none', 0, 'No content'],
  ] as [InteractionContentKind, number, string][])(
    'renders %s as %s',
    (content_kind, message_count, expected) => {
      renderItem({ content_kind, message_count })
      expect(screen.getByText(expected)).toBeVisible()
    }
  )

  it('renders venue and group chips and preserves row identity attributes', () => {
    renderItem({
      id: 'ixn-with-tags',
      source: 'gchat',
      is_group: true,
      venue_tags: [
        { key: 'venue-key', label: 'Synthetic venue', kind: 'group_chat', is_group: true },
      ],
    })
    const row = screen.getByRole('listitem')
    expect(row).toHaveAttribute('data-interaction-id', 'ixn-with-tags')
    expect(row).toHaveAttribute('data-source', 'gchat')
    expect(screen.getByText('Google Chat')).toHaveAttribute('data-badge')
    expect(screen.getByText('Synthetic venue')).toHaveAttribute('data-venue-key', 'venue-key')
    expect(screen.getByText('Group')).toBeVisible()
  })

  it('does not render a Group chip when the interaction is not a group', () => {
    renderItem({ is_group: false })
    expect(screen.queryByText('Group', { exact: true })).not.toBeInTheDocument()
  })

  it.each(['inbound', 'outbound', 'mutual'] as const)(
    'labels the %s direction glyph',
    direction => {
      renderItem({ direction })
      expect(screen.getByLabelText(direction)).toBeVisible()
    }
  )

  it('omits the attendee line for one attendee and links event titles externally when linked', () => {
    renderItem({
      source: 'gcal',
      content_kind: 'none',
      event: {
        title: 'Synthetic event',
        start_time: '2026-01-02T03:04:00Z',
        end_time: '2026-01-02T04:04:00Z',
        attendee_count: 1,
        html_link: 'https://example.test/event',
      },
    })
    expect(screen.getByRole('link', { name: 'Synthetic event' })).toHaveAttribute(
      'target',
      '_blank'
    )
    expect(screen.getByRole('link', { name: 'Synthetic event' })).toHaveAttribute(
      'rel',
      'noopener noreferrer'
    )
    expect(screen.queryByText(/attendees/)).not.toBeInTheDocument()
  })

  it('renders two attendees and plain event titles without a link', () => {
    renderItem({
      source: 'gcal',
      event: {
        title: 'Synthetic plain event',
        start_time: '2026-01-02T03:04:00Z',
        end_time: '2026-01-02T04:04:00Z',
        attendee_count: 2,
      },
    })
    const row = screen.getByRole('listitem')
    expect(within(row).getByText('2 attendees')).toBeVisible()
    expect(
      within(row).queryByRole('link', { name: 'Synthetic plain event' })
    ).not.toBeInTheDocument()
    expect(within(row).getByText('Synthetic plain event')).toBeVisible()
  })

  it.each(['messages', 'meeting_note'] as const)(
    'renders an expand affordance for %s content',
    content_kind => {
      renderItem({ content_kind })
      const button = screen.getByRole('button', { name: 'Expand content' })
      expect(button).toHaveAttribute('aria-expanded', 'false')
    }
  )

  it('offers the expand affordance as an icon-only caret in the row action cluster', () => {
    renderItem({ content_kind: 'messages' })
    const button = screen.getByRole('button', { name: 'Expand content' })
    // Named by aria-label only: a visible text label would put a button in the
    // metadata chip row, which is the affordance the design canvas rejected.
    expect(button.textContent).toBe('')
    const actions = button.closest('[data-row-actions]')
    expect(actions).not.toBeNull()
    expect(
      within(actions as HTMLElement).getByRole('button', { name: 'More actions' })
    ).toBeVisible()
  })

  it('toggles the content region in place and unmounts it on collapse', () => {
    renderItem({ content_kind: 'messages' })
    const button = screen.getByRole('button', { name: 'Expand content' })
    fireEvent.click(button)
    expect(screen.getByRole('button', { name: 'Collapse content' })).toHaveAttribute(
      'aria-expanded',
      'true'
    )
    expect(document.querySelector('[data-content-region]')).toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Collapse content' }))
    expect(document.querySelector('[data-content-region]')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Expand content' })).toHaveAttribute(
      'aria-expanded',
      'false'
    )
  })

  it.each(['call', 'none'] as const)(
    'does not render an expand affordance for %s content',
    content_kind => {
      renderItem({ content_kind })
      expect(screen.queryByRole('button', { name: /expand/i })).not.toBeInTheDocument()
    }
  )
})

const message = (
  id: string,
  sender: string,
  sent_at: string,
  body: string,
  container = 'venue-a'
) => ({
  id,
  sender,
  is_outgoing: false,
  sent_at,
  body,
  venue_key: container,
  source: 'gchat',
  channel: 'group_chat' as const,
  container,
  content_ref: `ref-${id}`,
})

const identity = { kind: 'interaction' as const, id: 'ixn-content' }
const context = { contactId: 'contact-test' }

const messagesContent = (messages: ReturnType<typeof message>[]): TimelineContentResponse => ({
  identity,
  members: [{ source: 'gchat', kind: 'messages', messages, meeting_notes: [] }],
})

function renderWithContent(
  content: TimelineContentResponse,
  ui: (client: QueryClient) => React.ReactElement
) {
  mocks.getTimelineContent.mockReset().mockResolvedValue(content)
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const view = render(<QueryClientProvider client={client}>{ui(client)}</QueryClientProvider>)
  return {
    ...view,
    rerenderUi: (next: React.ReactElement) =>
      view.rerender(<QueryClientProvider client={client}>{next}</QueryClientProvider>),
  }
}

describe('TimelineDrillDown on the contact page', () => {
  it('renders messages in API order with exact child text, including empty bodies', async () => {
    const content = messagesContent([
      message('message-3', 'Sender C', '2026-01-02T03:06:00Z', ''),
      message('message-1', 'Sender A', '2026-01-02T03:04:00Z', 'First body'),
      message('message-2', 'Sender B', '2026-01-02T03:05:00Z', 'Second body'),
    ])
    renderWithContent(content, () => <TimelineDrillDown identity={identity} context={context} />)
    await waitFor(() => expect(document.querySelectorAll('[data-message-id]')).toHaveLength(3))
    const nodes = Array.from(document.querySelectorAll('[data-message-id]'))
    expect(nodes.map(node => node.getAttribute('data-message-id'))).toEqual([
      'message-3',
      'message-1',
      'message-2',
    ])
    const expected = content.members[0].messages
    expected.forEach((expectedMessage, index) => {
      const node = nodes[index]
      expect(node.querySelector('[data-message-sender]')?.textContent).toBe(expectedMessage.sender)
      expect(node.querySelector('[data-message-timestamp]')?.textContent).toBe(
        formatDateTime(expectedMessage.sent_at)
      )
      expect(node.querySelector('[data-message-body]')?.textContent).toBe(expectedMessage.body)
    })
    expect(nodes[0].querySelector('[data-message-body]')?.textContent).toBe('')
  })

  it('renders HTML-looking message bodies as literal text without elements', async () => {
    const content = messagesContent([
      message('message-html', 'Sender', '2026-01-02T03:04:00Z', '<b>not bold</b>'),
    ])
    renderWithContent(content, () => <TimelineDrillDown identity={identity} context={context} />)
    const region = (await screen.findByText('<b>not bold</b>')).closest(
      '[data-content-region]'
    ) as HTMLElement
    expect(region.querySelector('[data-message-body]')).toHaveTextContent('<b>not bold</b>')
    expect(region.querySelector('b')).not.toBeInTheDocument()
  })

  it('filters messages by venue and renders all messages when the filter is absent', async () => {
    const content = messagesContent([
      message('message-a', 'Sender A', '2026-01-02T03:04:00Z', 'A', 'venue-a'),
      message('message-b', 'Sender B', '2026-01-02T03:05:00Z', 'B', 'venue-b'),
    ])
    const view = renderWithContent(content, () => (
      <TimelineDrillDown identity={identity} context={context} venueFilter="venue-b" />
    ))
    await waitFor(() => expect(document.querySelectorAll('[data-message-id]')).toHaveLength(1))
    expect(document.querySelector('[data-message-id]')).toHaveAttribute(
      'data-message-id',
      'message-b'
    )
    view.rerenderUi(<TimelineDrillDown identity={identity} context={context} />)
    expect(document.querySelectorAll('[data-message-id]')).toHaveLength(2)
  })

  it('renders notes in API order, preserves null fields, and shows provenance once', async () => {
    const note = (title: string | undefined, summary: string, memo: string, ref: string) => ({
      title,
      summary,
      memo,
      source: 'anarlog_sessions',
      channel: 'meeting' as const,
      container: 'anarlog_sessions:session-1',
      content_ref: ref,
    })
    const content: TimelineContentResponse = {
      identity,
      members: [
        {
          source: 'gcal',
          kind: 'meeting_note',
          messages: [],
          meeting_notes: [
            note(undefined, 'Summary A', '<i>memo A</i>', 'ref-a'),
            note('Title B', 'Summary B', 'Memo B', 'ref-b'),
          ],
        },
      ],
    }
    renderWithContent(content, () => <TimelineDrillDown identity={identity} context={context} />)
    await screen.findByText('Summary A')
    const notes = Array.from(document.querySelectorAll('[data-meeting-note]'))
    expect(notes).toHaveLength(2)
    expect(notes[0].querySelector('[data-note-title]')).not.toBeInTheDocument()
    const expectedNotes = [
      { summary: 'Summary A', memo: '<i>memo A</i>' },
      { title: 'Title B', summary: 'Summary B', memo: 'Memo B' },
    ]
    expectedNotes.forEach((expectedNote, index) => {
      const node = notes[index]
      if (expectedNote.title !== undefined) {
        expect(node.querySelector('[data-note-title]')?.textContent).toBe(expectedNote.title)
      }
      expect(node.querySelector('[data-note-summary]')?.textContent).toBe(expectedNote.summary)
      expect(node.querySelector('[data-note-memo]')?.textContent).toBe(expectedNote.memo)
    })
    expect(screen.getByText('Meeting notes are processed on-device.')).toBeInTheDocument()
    expect(screen.getAllByText('Meeting notes are processed on-device.')).toHaveLength(1)
    expect(document.querySelector('[data-content-region] i')).not.toBeInTheDocument()
  })

  it('renders loading and error states with exact copy', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const { unmount } = render(
      <QueryClientProvider client={client}>
        <TimelineDrillDown identity={identity} context={context} />
      </QueryClientProvider>
    )
    expect(document.querySelector('[data-content-region]')).toHaveTextContent('Loading content…')
    unmount()
    mocks.getTimelineContent.mockReset().mockRejectedValue(new Error('boom'))
    render(
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <TimelineDrillDown identity={identity} context={context} />
      </QueryClientProvider>
    )
    await waitFor(() =>
      expect(document.querySelector('[data-content-region]')).toHaveTextContent(
        'Failed to load content.'
      )
    )
  })
})

describe('InteractionDrillDown evidence scope', () => {
  const rowItem = (messageCount: number) => ({
    ...baseItem({ content_kind: 'messages', message_count: messageCount }),
    identity,
  })
  const drillDown = (messageCount: number, venueFilter?: string) => (
    <InteractionDrillDown
      item={rowItem(messageCount)}
      identity={identity}
      context={context}
      venueFilter={venueFilter}
    />
  )

  it('discloses the evidence scope only when it differs from the recorded count', async () => {
    const content = messagesContent([
      message('message-a', 'Sender A', '2026-01-02T03:04:00Z', 'A', 'venue-a'),
      message('message-b', 'Sender B', '2026-01-02T03:05:00Z', 'B', 'venue-b'),
      message('message-c', 'Sender C', '2026-01-02T03:06:00Z', 'C', 'venue-b'),
    ])
    const scope = () => document.querySelector('[data-evidence-scope]')

    // Window wider than the interaction: the common case behind "header says 6,
    // thread shows 10".
    const view = renderWithContent(content, () => drillDown(2))
    await waitFor(() => expect(scope()).toBeInTheDocument())
    expect(scope()).toHaveTextContent('Showing 3 messages from this conversation')
    expect(scope()).toHaveTextContent('2 recorded for this interaction')

    // A venue filter narrows BELOW the recorded count — also a mismatch, and
    // also disclosed, so the row's count never over-promises either way.
    view.rerenderUi(drillDown(2, 'venue-a'))
    expect(scope()).toHaveTextContent('Showing 1 message from this conversation')

    // Agreement is silent: no note when the counts already match.
    view.rerenderUi(drillDown(3))
    expect(scope()).not.toBeInTheDocument()
  })
})

describe('InteractionDrillDown assertions read', () => {
  const drillDown = (
    <InteractionDrillDown
      item={{ ...baseItem({ content_kind: 'messages', message_count: 1 }), identity }}
      identity={identity}
      context={context}
    />
  )
  const content = messagesContent([
    message('message-a', 'Sender A', '2026-01-02T03:04:00Z', 'A', 'venue-a'),
  ])
  const failure = () => document.querySelector('[data-assertions-error]')

  // A failed read of the assertions beside the content (IXN-023) must not
  // look like content no assertion cites.
  it('says a failed assertions read failed, and stays silent on an empty one', async () => {
    mocks.getTimelineAssertions.mockRejectedValue(new Error('unavailable'))
    const view = renderWithContent(content, () => drillDown)
    await waitFor(() => expect(failure()).toHaveTextContent('Failed to load assertions.'))
    expect(document.querySelector('[data-message-id]')).toHaveAttribute(
      'data-message-id',
      'message-a'
    )
    view.unmount()

    mocks.getTimelineAssertions.mockReset().mockResolvedValue({ items: [] })
    renderWithContent(content, () => drillDown)
    await waitFor(() => expect(document.querySelector('[data-message-id]')).toBeInTheDocument())
    await waitFor(() => expect(mocks.getTimelineAssertions).toHaveBeenCalled())
    expect(failure()).not.toBeInTheDocument()
  })
})
